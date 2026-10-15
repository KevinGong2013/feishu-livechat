package feishu

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

func TestRootCardMentionsAllAndEscapesUserInput(t *testing.T) {
	u := livechat.User{
		ID:     "u1",
		Name:   `<at id=all></at> **evil**`,
		Fields: []livechat.Field{{Label: "邮箱", Value: "a@example.com"}, {Label: "", Value: ""}},
		Links: []livechat.Link{
			{Text: "打开后台", URL: "https://admin.example.com/users/u1"},
			{Text: "bad", URL: "javascript:alert(1)"},
		},
	}
	raw, err := rootCard(u, DefaultCardHint)
	if err != nil {
		t.Fatal(err)
	}
	var card struct {
		Header struct {
			Title map[string]string `json:"title"`
		} `json:"header"`
		Elements []map[string]any `json:"elements"`
	}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	if card.Header.Title["tag"] != "plain_text" || card.Header.Title["content"] != u.Name {
		t.Fatalf("title must be the raw name as plain_text, got %v", card.Header.Title)
	}
	// Exactly one lark_md node, and it is the fixed @所有人 line.
	if n := strings.Count(string(raw), `"lark_md"`); n != 1 {
		t.Fatalf("want exactly one lark_md node, got %d: %s", n, raw)
	}
	first := card.Elements[0]["text"].(map[string]any)
	if first["tag"] != "lark_md" || !strings.HasPrefix(first["content"].(string), "<at id=all></at>") {
		t.Fatalf("first element should @所有人, got %v", first)
	}
	if !strings.Contains(string(raw), "邮箱：a@example.com") {
		t.Fatalf("field missing: %s", raw)
	}
	if strings.Contains(string(raw), "javascript:") {
		t.Fatalf("non-web link must be dropped: %s", raw)
	}
	if !strings.Contains(string(raw), "https://admin.example.com/users/u1") {
		t.Fatalf("admin link missing: %s", raw)
	}
}

func TestPostContentIsLiteralLines(t *testing.T) {
	raw, err := postContent("第一行\n<at user_id=\"all\"></at>")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ZhCn struct {
			Content [][]map[string]string `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	c := got.ZhCn.Content
	if len(c) != 2 || c[0][0]["tag"] != "text" || c[1][0]["text"] != `<at user_id="all"></at>` {
		t.Fatalf("unexpected post content: %s", raw)
	}
}

func strp(s string) *string { return &s }

func receiveEvent(chatID, senderType string, mentionOpenIDs ...string) *larkim.P2MessageReceiveV1 {
	var mentions []*larkim.MentionEvent
	for i, id := range mentionOpenIDs {
		mentions = append(mentions, &larkim.MentionEvent{
			Key: strp("@_user_" + string(rune('1'+i))),
			Id:  &larkim.UserId{OpenId: strp(id)},
		})
	}
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderType: strp(senderType), SenderId: &larkim.UserId{OpenId: strp("ou_agent")}},
		Message: &larkim.EventMessage{
			MessageId:   strp("om_reply"),
			RootId:      strp("om_root"),
			ThreadId:    strp("omt_1"),
			ChatId:      strp(chatID),
			MessageType: strp("text"),
			Content:     strp(`{"text":"@_user_1 好的"}`),
			Mentions:    mentions,
		},
	}}
}

func TestAgentMessageFilter(t *testing.T) {
	c := &Client{cfg: Config{ChatID: "oc_group"}, botOpenID: "ou_bot"}

	am, ok := c.agentMessage(receiveEvent("oc_group", "user", "ou_bot"))
	if !ok {
		t.Fatal("agent reply that @-s the bot must be forwarded")
	}
	want := livechat.AgentMessage{RemoteID: "om_reply", ThreadID: "omt_1", RootMessageID: "om_root", AgentID: "ou_agent", Parts: []livechat.Part{{Text: "好的"}}}
	if am.RemoteID != want.RemoteID || am.ThreadID != want.ThreadID || am.RootMessageID != want.RootMessageID || am.AgentID != want.AgentID || len(am.Parts) != 1 || am.Parts[0] != want.Parts[0] {
		t.Fatalf("got %#v", am)
	}

	for name, ev := range map[string]*larkim.P2MessageReceiveV1{
		"no mention":            receiveEvent("oc_group", "user"),
		"mentions someone else": receiveEvent("oc_group", "user", "ou_colleague"),
		"other chat":            receiveEvent("oc_other", "user", "ou_bot"),
		"sent by an app":        receiveEvent("oc_group", "app", "ou_bot"),
		"nil":                   nil,
	} {
		if _, ok := c.agentMessage(ev); ok {
			t.Errorf("%s: must be ignored", name)
		}
	}
}

func TestSerialKeepsOrderPerKey(t *testing.T) {
	var s serial
	var mu sync.Mutex
	var got []int
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		i := i
		s.run("topic", func() {
			defer wg.Done()
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
	}
	wg.Wait()
	for i, v := range got {
		if v != i {
			t.Fatalf("out of order at %d: %v", i, got)
		}
	}
}
