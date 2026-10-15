package feishu

import (
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// DefaultCardHint is the footnote on every root card.
const DefaultCardHint = "在本话题里 @机器人 的回复才会发给用户，其他消息仅客服可见。"

// rootCard renders the card that opens a user's topic. It @所有人 so every
// agent follows the topic from the start, and it is never updated
// afterwards (飞书 only allows updating cards for 14 days), so it carries
// only stable facts and links.
//
// Only the @所有人 line is lark_md. Everything that comes from the host —
// name, fields, link texts — is plain_text, so a user who names themselves
// "<at id=all></at>" pings nobody.
func rootCard(u livechat.User, hint string) ([]byte, error) {
	title := u.Name
	if strings.TrimSpace(title) == "" {
		title = u.ID
	}
	elements := []any{
		map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": "<at id=all></at> 新的用户会话"},
		},
	}
	var fields []any
	for _, f := range u.Fields {
		if strings.TrimSpace(f.Label) == "" && strings.TrimSpace(f.Value) == "" {
			continue
		}
		fields = append(fields, map[string]any{
			"is_short": true,
			"text":     plainText(truncate(f.Label, 40) + "：" + truncate(f.Value, 200)),
		})
	}
	if len(fields) > 0 {
		elements = append(elements, map[string]any{"tag": "div", "fields": fields})
	}
	var buttons []any
	for _, l := range u.Links {
		if !webURL(l.URL) || strings.TrimSpace(l.Text) == "" {
			continue
		}
		buttons = append(buttons, map[string]any{
			"tag":  "button",
			"text": plainText(truncate(l.Text, 40)),
			"type": "default",
			"url":  l.URL,
		})
	}
	if len(buttons) > 0 {
		elements = append(elements, map[string]any{"tag": "action", "actions": buttons})
	}
	if hint != "" {
		elements = append(elements, map[string]any{
			"tag":      "note",
			"elements": []any{plainText(hint)},
		})
	}
	return json.Marshal(map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": "blue",
			"title":    plainText(truncate(title, 100)),
		},
		"elements": elements,
	})
}

func plainText(s string) map[string]any {
	return map[string]any{"tag": "plain_text", "content": s}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func webURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// postContent renders user text as a post with one text element per line.
// Text elements are literal — "<at user_id=all>" typed by a user stays
// text — which is why user messages are posts rather than text messages.
func postContent(text string) ([]byte, error) {
	var paras [][]map[string]string
	for _, line := range strings.Split(text, "\n") {
		paras = append(paras, []map[string]string{{"tag": "text", "text": line}})
	}
	return json.Marshal(map[string]any{"zh_cn": map[string]any{"content": paras}})
}
