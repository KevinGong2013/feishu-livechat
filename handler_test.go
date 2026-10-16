package livechat_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

func newServer(t *testing.T) (*env, *httptest.Server) {
	t.Helper()
	e := newEnv(t)
	h := e.bridge.Handler(livechat.HandlerOptions{
		BasePath:   "/api/support",
		SigningKey: []byte("test-key"),
		Authenticate: func(r *http.Request) (livechat.User, error) {
			id := r.Header.Get("X-Test-User")
			if id == "" {
				return livechat.User{}, errors.New("no user")
			}
			return livechat.User{ID: id, Name: id}, nil
		},
		AgentName: func(_ context.Context, agentID string) string { return "客服 " + agentID },
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return e, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, user, contentType string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func TestHandlerFlow(t *testing.T) {
	e, srv := newServer(t)

	if resp, _ := call(t, srv, "GET", "/api/support/messages", "", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	if _, body := call(t, srv, "GET", "/api/support/status", "bob", "", nil); strings.TrimSpace(string(body)) != `{"available":true}` {
		t.Fatalf("status: %s", body)
	}
	// No conversation yet: empty history, not an error, and no topic opened.
	resp, body := call(t, srv, "GET", "/api/support/messages", "bob", "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"messages":[]`) {
		t.Fatalf("empty history: %d %s", resp.StatusCode, body)
	}

	resp, body = call(t, srv, "POST", "/api/support/messages", "bob", "application/json", []byte(`{"text":"你好"}`))
	var sent livechat.MessageView
	_ = json.Unmarshal(body, &sent)
	if resp.StatusCode != http.StatusCreated || sent.Status != livechat.StatusSent || sent.Blocks[0].Text != "你好" {
		t.Fatalf("send text: %d %s", resp.StatusCode, body)
	}
	if resp, _ := call(t, srv, "POST", "/api/support/messages", "bob", "application/json", []byte(`{"text":""}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty text: %d", resp.StatusCode)
	}

	// Image as multipart.
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, _ := mw.CreateFormFile("file", "screenshot.png")
	_, _ = fw.Write(pngBytes(t, 5, 5))
	_ = mw.Close()
	resp, body = call(t, srv, "POST", "/api/support/images", "bob", mw.FormDataContentType(), form.Bytes())
	var img livechat.MessageView
	_ = json.Unmarshal(body, &img)
	if resp.StatusCode != http.StatusCreated || len(img.Blocks) != 1 || img.Blocks[0].Width != 5 {
		t.Fatalf("send image: %d %s", resp.StatusCode, body)
	}
	url := img.Blocks[0].URL
	if !strings.HasPrefix(url, "/api/support/files/") {
		t.Fatalf("image url: %q", url)
	}
	// The signed URL works without auth headers (it goes in an <img>)…
	resp, body = call(t, srv, "GET", url, "", "", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(body, []byte("\x89PNG")) {
		t.Fatalf("fetch image: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// …but not once tampered with.
	if resp, _ := call(t, srv, "GET", strings.Replace(url, "sig=", "sig=x", 1), "", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered url: %d", resp.StatusCode)
	}
	// Raw body works too; a non-image is refused.
	if resp, _ := call(t, srv, "POST", "/api/support/images", "bob", "image/png", []byte("not an image")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad image: %d", resp.StatusCode)
	}

	// An agent replies; it shows up in history and as unread.
	root := e.conv(t, "bob").RootMessageID
	if err := e.bridge.HandleAgentMessage(context.Background(), livechat.AgentMessage{RemoteID: "om_a", RootMessageID: root, Parts: []livechat.Part{{Text: "收到"}}}); err != nil {
		t.Fatal(err)
	}
	_, body = call(t, srv, "GET", "/api/support/unread", "bob", "", nil)
	if strings.TrimSpace(string(body)) != `{"count":1}` {
		t.Fatalf("unread: %s", body)
	}
	_, body = call(t, srv, "GET", "/api/support/messages?after="+img.ID, "bob", "", nil)
	var page struct {
		Messages []livechat.MessageView `json:"messages"`
		HasMore  bool                   `json:"has_more"`
	}
	_ = json.Unmarshal(body, &page)
	if len(page.Messages) != 1 || page.Messages[0].Sender != livechat.SenderAgent || page.Messages[0].Blocks[0].Text != "收到" {
		t.Fatalf("after page: %s", body)
	}
	if resp, _ := call(t, srv, "POST", "/api/support/read", "bob", "application/json", []byte(`{"message_id":"`+page.Messages[0].ID+`"}`)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mark read: %d", resp.StatusCode)
	}
	_, body = call(t, srv, "GET", "/api/support/unread", "bob", "", nil)
	if strings.TrimSpace(string(body)) != `{"count":0}` {
		t.Fatalf("unread after read: %s", body)
	}
	if resp, _ := call(t, srv, "GET", "/api/support/messages?before=not-an-id", "bob", "", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d", resp.StatusCode)
	}

	// Another user sees none of it.
	_, body = call(t, srv, "GET", "/api/support/messages", "eve", "", nil)
	if !strings.Contains(string(body), `"messages":[]`) {
		t.Fatalf("eve sees bob's messages: %s", body)
	}
}

func TestHandlerEvents(t *testing.T) {
	e, srv := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Open the conversation first so the agent reply has a topic.
	call(t, srv, "POST", "/api/support/messages", "carol", "application/json", []byte(`{"text":"hi"}`))

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/support/events", nil)
	req.Header.Set("X-Test-User", "carol")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	rd := bufio.NewReader(resp.Body)
	readEvent := func() (string, string) {
		var name, data string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v", err)
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "" && name != "":
				return name, data
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}
	if name, _ := readEvent(); name != "ready" {
		t.Fatalf("first event %q", name)
	}

	root := e.conv(t, "carol").RootMessageID
	go func() {
		_ = e.bridge.HandleAgentMessage(context.Background(), livechat.AgentMessage{RemoteID: "om_live", AgentID: "ou_7", RootMessageID: root, Parts: []livechat.Part{{Text: "实时回复"}}})
	}()
	name, data := readEvent()
	var v livechat.MessageView
	_ = json.Unmarshal([]byte(data), &v)
	if name != "message" || v.Sender != livechat.SenderAgent || v.Blocks[0].Text != "实时回复" || v.Agent != "客服 ou_7" {
		t.Fatalf("event %q %s", name, data)
	}
}
