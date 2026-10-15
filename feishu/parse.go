package feishu

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// Parse reduces an agent's text or post (富文本) message to what gets
// forwarded: runs of text and images, in order. Every @mention — the bot,
// colleagues, 所有人 — is removed, since the user has no use for agent
// names. Links keep their text and address; anything else (emoji, video,
// files, dividers) is dropped. Other message types yield nothing.
//
// mentionKeys are the placeholders 飞书 puts in text messages for each
// mention ("@_user_1"), from the event's mentions list.
func Parse(msgType, content string, mentionKeys []string) []livechat.Part {
	switch msgType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(content), &c) != nil {
			return nil
		}
		return textParts(stripMentionKeys(c.Text, mentionKeys))
	case "post":
		body, ok := postBodyOf(content)
		if !ok {
			return nil
		}
		return body.parts()
	}
	return nil
}

// mentionPlaceholder matches the placeholders 飞书 uses for mentions in
// text messages, as a fallback for keys missing from the mentions list.
var mentionPlaceholder = regexp.MustCompile(`@_(?:user_\d+|all)[ \t]*`)

func stripMentionKeys(text string, keys []string) string {
	// Longest first, so "@_user_1" never eats the front of "@_user_12".
	keys = append([]string(nil), keys...)
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if k == "" {
			continue
		}
		text = regexp.MustCompile(regexp.QuoteMeta(k)+`[ \t]*`).ReplaceAllString(text, "")
	}
	return mentionPlaceholder.ReplaceAllString(text, "")
}

func textParts(text string) []livechat.Part {
	if t := strings.TrimSpace(text); t != "" {
		return []livechat.Part{{Text: t}}
	}
	return nil
}

type postBody struct {
	Title     string          `json:"title"`
	Content   [][]postElement `json:"content"`
	ContentV2 json.RawMessage `json:"content_v2"`
}

type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	Href     string `json:"href"`
	ImageKey string `json:"image_key"`
}

// postLocales is the order a locale-wrapped post is read in.
var postLocales = []string{"zh_cn", "en_us", "ja_jp"}

// postBodyOf accepts both shapes a post arrives in: the body itself
// ({"title", "content"}), as in message events, or wrapped per locale
// ({"zh_cn": {...}}), as when sent.
func postBodyOf(content string) (postBody, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &top) != nil {
		return postBody{}, false
	}
	_, hasContent := top["content"]
	_, hasV2 := top["content_v2"]
	_, hasTitle := top["title"]
	if hasContent || hasV2 || hasTitle {
		var b postBody
		if json.Unmarshal([]byte(content), &b) != nil {
			return postBody{}, false
		}
		return b, true
	}
	keys := append([]string(nil), postLocales...)
	var rest []string
	for k := range top {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range append(keys, rest...) {
		raw, ok := top[k]
		if !ok {
			continue
		}
		var b postBody
		if json.Unmarshal(raw, &b) == nil && (len(b.Content) > 0 || len(b.ContentV2) > 0 || b.Title != "") {
			return b, true
		}
	}
	return postBody{}, false
}

// paragraphs returns the structured content, falling back to content_v2
// (paragraphs of markdown "md" elements) when that is all there is.
func (b postBody) paragraphs() [][]postElement {
	if len(b.Content) > 0 {
		return b.Content
	}
	var v2 [][]postElement
	if len(b.ContentV2) > 0 && json.Unmarshal(b.ContentV2, &v2) == nil {
		return v2
	}
	return nil
}

type partsBuilder struct {
	parts []livechat.Part
	buf   strings.Builder
	// afterMention trims the space that followed a removed @mention.
	afterMention bool
}

func (pb *partsBuilder) text(s string) {
	if pb.afterMention {
		s = strings.TrimLeft(s, " \t")
	}
	pb.afterMention = false
	pb.buf.WriteString(s)
}

func (pb *partsBuilder) image(key string) {
	pb.flush()
	if key != "" {
		pb.parts = append(pb.parts, livechat.Part{ImageKey: key})
	}
}

func (pb *partsBuilder) flush() {
	if t := strings.TrimSpace(pb.buf.String()); t != "" {
		pb.parts = append(pb.parts, livechat.Part{Text: t})
	}
	pb.buf.Reset()
	pb.afterMention = false
}

func (b postBody) parts() []livechat.Part {
	var pb partsBuilder
	if t := strings.TrimSpace(b.Title); t != "" {
		pb.text(t + "\n")
	}
	for i, para := range b.paragraphs() {
		if i > 0 {
			pb.buf.WriteString("\n")
			pb.afterMention = false
		}
		for _, el := range para {
			switch el.Tag {
			case "text", "code_block":
				pb.text(el.Text)
			case "a":
				pb.text(linkText(el))
			case "md":
				pb.markdown(el.Text)
			case "img":
				pb.image(el.ImageKey)
			case "at":
				pb.afterMention = true
			}
		}
	}
	pb.flush()
	return pb.parts
}

func linkText(el postElement) string {
	switch {
	case el.Href == "":
		return el.Text
	case el.Text == "" || el.Text == el.Href:
		return el.Href
	default:
		return el.Text + " (" + el.Href + ")"
	}
}

var (
	mdMention = regexp.MustCompile(`<at\b[^>]*>[^<]*</at>[ \t]*`)
	mdImage   = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)\)`)
)

// markdown handles content_v2 "md" elements: mentions are <at> tags and
// images are ![](image_key); the rest is kept as text.
func (pb *partsBuilder) markdown(s string) {
	s = mdMention.ReplaceAllString(s, "")
	last := 0
	for _, m := range mdImage.FindAllStringSubmatchIndex(s, -1) {
		pb.text(s[last:m[0]])
		pb.image(s[m[2]:m[3]])
		last = m[1]
	}
	pb.text(s[last:])
}
