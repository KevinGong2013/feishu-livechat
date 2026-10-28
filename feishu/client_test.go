package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

func TestReadEmojiDefault(t *testing.T) {
	c, err := New(Config{AppID: "cli", AppSecret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ReadEmoji() != "Get" {
		t.Fatalf("ReadEmoji = %q, want Get", c.ReadEmoji())
	}
	custom, err := New(Config{AppID: "cli", AppSecret: "sec", ReadEmoji: "OK"})
	if err != nil {
		t.Fatal(err)
	}
	if custom.ReadEmoji() != "OK" {
		t.Fatalf("ReadEmoji = %q, want OK", custom.ReadEmoji())
	}
}

func TestRuntimeReadEmojiFollowsTheConnectedApp(t *testing.T) {
	r, err := NewRuntime(RuntimeConfig{Settings: KVSettings(&memKV{}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.ReadEmoji(); got != "" {
		t.Fatalf("unconfigured ReadEmoji = %q", got)
	}
	c, err := New(Config{AppID: "cli", AppSecret: "sec", ReadEmoji: "OK"})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.client = c
	r.mu.Unlock()
	if got := r.ReadEmoji(); got != "OK" {
		t.Fatalf("ReadEmoji = %q", got)
	}
}

type reactionHit struct {
	messageID string
	emoji     string
}

func TestReactAndAck(t *testing.T) {
	var mu sync.Mutex
	var hits []reactionHit
	status := http.StatusOK
	body := `{"code":0,"msg":"ok","data":{"reaction_id":"r1"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/v3/tenant_access_token/internal"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t-1","expire":7200}`)
		case strings.Contains(r.URL.Path, "/reactions") && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				ReactionType struct {
					EmojiType string `json:"emoji_type"`
				} `json:"reaction_type"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Errorf("reaction body: %v %s", err, raw)
			}
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			messageID := ""
			for i, p := range parts {
				if p == "messages" && i+1 < len(parts) {
					messageID = parts[i+1]
				}
			}
			mu.Lock()
			hits = append(hits, reactionHit{messageID, req.ReactionType.EmojiType})
			code, payload := status, body
			mu.Unlock()
			w.WriteHeader(code)
			_, _ = io.WriteString(w, payload)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{AppID: "cli", AppSecret: "sec", OpenBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Ack(ctx, "om_ok", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Ack(ctx, "om_bad", false); err != nil {
		t.Fatal(err)
	}
	if err := c.React(ctx, "om_read", c.ReadEmoji()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	status, body = http.StatusBadRequest, `{"code":231003,"msg":"The message is not found, maybe not exist or deleted."}`
	mu.Unlock()
	err = c.React(ctx, "om_recalled", "Get")
	if !errors.Is(err, livechat.ErrMessageGone) {
		t.Fatalf("231003: %v", err)
	}

	mu.Lock()
	body = `{"code":230110,"msg":"Action unavailable as the message has been deleted."}`
	mu.Unlock()
	err = c.React(ctx, "om_deleted", "Get")
	if !errors.Is(err, livechat.ErrMessageGone) {
		t.Fatalf("230110: %v", err)
	}

	mu.Lock()
	body = `{"code":231015,"msg":"repeated request is processing"}`
	mu.Unlock()
	err = c.React(ctx, "om_busy", "Get")
	if err == nil || errors.Is(err, livechat.ErrMessageGone) {
		t.Fatalf("temporary feishu error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []reactionHit{
		{"om_ok", "DONE"},
		{"om_bad", "CrossMark"},
		{"om_read", "Get"},
		{"om_recalled", "Get"},
		{"om_deleted", "Get"},
		{"om_busy", "Get"},
	}
	if len(hits) != len(want) {
		t.Fatalf("reactions = %+v", hits)
	}
	for i, hit := range hits {
		if hit != want[i] {
			t.Fatalf("reaction %d = %+v, want %+v", i, hit, want[i])
		}
	}
}
