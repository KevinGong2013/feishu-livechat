package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// fakeFeishu serves the scan-to-create endpoint (the user confirms on the
// third poll) and the Open API calls a Runtime makes.
func fakeFeishu(t *testing.T) (accounts, open *httptest.Server) {
	t.Helper()
	var polls atomic.Int32
	accounts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("action") {
		case "begin":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "dev-1",
				"verification_uri_complete": "https://accounts.example/verify?user_code=ABCD",
				"interval":                  1,
				"expire_in":                 600,
			})
		case "poll":
			if polls.Add(1) < 3 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"client_id":     "cli_new",
				"client_secret": "secret_new",
				"user_info":     map[string]any{"open_id": "ou_admin", "tenant_brand": "feishu"},
			})
		}
	}))
	t.Cleanup(accounts.Close)

	open = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/v3/tenant_access_token/internal"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t-1","expire":7200}`)
		case r.URL.Path == "/open-apis/bot/v3/info":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","bot":{"open_id":"ou_bot","app_name":"在线客服"}}`)
		case r.URL.Path == "/open-apis/im/v1/chats" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"ou_admin"`) {
				t.Errorf("the admin who scanned is not added to the group: %s", b)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"chat_id":"oc_new"}}`)
		case r.URL.Path == "/open-apis/im/v1/chats/oc_new/link":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"share_link":"https://applink.example/join"}}`)
		case r.URL.Path == "/open-apis/im/v1/messages" && r.Method == http.MethodPost:
			if r.URL.Query().Get("receive_id_type") != "chat_id" {
				t.Errorf("root card not sent to a chat: %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"message_id":"om_root","thread_id":"omt_1"}}`)
		default:
			t.Errorf("unexpected Open API call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(open.Close)
	return accounts, open
}

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) GetSetting(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	if !ok {
		return "", livechat.ErrNotFound
	}
	return v, nil
}

func (k *memKV) PutSetting(_ context.Context, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[key] = value
	return nil
}

// xorSealer stands in for the host's encryption.
type xorSealer struct{}

func (xorSealer) Seal(p []byte) ([]byte, error) { return xor(p), nil }
func (xorSealer) Open(p []byte) ([]byte, error) { return xor(p), nil }
func xor(p []byte) []byte {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ 0x5a
	}
	return out
}

type nopSink struct{}

func (nopSink) HandleAgentMessage(context.Context, livechat.AgentMessage) error { return nil }
func (nopSink) HandleRecall(context.Context, string) error                      { return nil }

func newTestRuntime(t *testing.T, kv *memKV, accounts, open string, listens *atomic.Int32) *Runtime {
	t.Helper()
	r, err := NewRuntime(RuntimeConfig{
		Settings:    KVSettings(kv, xorSealer{}),
		Base:        Config{OpenBaseURL: open},
		AccountsURL: accounts,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.botWait = time.Second
	r.listen = func(ctx context.Context, c *Client, _ Sink) error {
		listens.Add(1)
		c.onState("connected", nil)
		<-ctx.Done()
		return ctx.Err()
	}
	return r
}

func getJSON(t *testing.T, h http.Handler, method, path string, body string, out any) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if out != nil && w.Body.Len() > 0 && strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, w.Body.String())
		}
	}
	return w.Code
}

func TestRuntimeSetupFromOperatorPage(t *testing.T) {
	accounts, open := fakeFeishu(t)
	kv := &memKV{}
	var listens atomic.Int32
	r := newTestRuntime(t, kv, accounts.URL, open.URL, &listens)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, nopSink{})

	// Nothing connected yet.
	if r.Available() {
		t.Fatal("unconfigured runtime must not be available")
	}
	if _, err := r.CreateTopic(ctx, livechat.User{ID: "u"}, "k"); !errors.Is(err, livechat.ErrUnavailable) {
		t.Fatalf("CreateTopic before setup: %v", err)
	}
	admin := r.AdminHandler("/admin/support")
	var st Status
	getJSON(t, admin, "GET", "/admin/support/status", "", &st)
	if st.Configured || st.Setup != nil {
		t.Fatalf("initial status: %+v", st)
	}

	// The operator starts the scan flow and gets a link and a QR code.
	var setup SetupStatus
	if code := getJSON(t, admin, "POST", "/admin/support/setup", `{"group_name":"客服群"}`, &setup); code != http.StatusOK {
		t.Fatalf("start setup: %d %+v", code, setup)
	}
	if setup.State != "waiting" || !strings.HasPrefix(setup.URL, "https://accounts.example/verify") || setup.ExpiresAt.IsZero() {
		t.Fatalf("setup: %+v", setup)
	}
	var again SetupStatus
	getJSON(t, admin, "POST", "/admin/support/setup", "", &again)
	if again.URL != setup.URL {
		t.Fatal("reloading the page must not replace the code being scanned")
	}
	req := httptest.NewRequest("GET", "/admin/support/setup/qr.png", nil)
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("qr: %d %q", w.Code, w.Header().Get("Content-Type"))
	}

	// The admin scans and confirms; the runtime creates the group and switches over.
	deadline := time.Now().Add(15 * time.Second)
	for {
		getJSON(t, admin, "GET", "/admin/support/status", "", &st)
		if st.Setup != nil && (st.Setup.State == "done" || st.Setup.State == "failed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup did not finish: %+v", st.Setup)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st.Setup.State != "done" || !st.Configured || st.AppID != "cli_new" || st.ChatID != "oc_new" || st.GroupName != "客服群" || st.BotName != "在线客服" {
		t.Fatalf("status after setup: %+v %+v", st, st.Setup)
	}
	waitFor(t, func() bool { return r.Status().Listener == "connected" })

	// Stored sealed: the secret is not readable in the database.
	if raw := kv.m["feishu"]; raw == "" || strings.Contains(raw, "secret_new") {
		t.Fatalf("settings not stored sealed: %q", raw)
	}

	// Backend calls now go to the new app and group.
	topic, err := r.CreateTopic(ctx, livechat.User{ID: "u1", Name: "用户"}, "r-1")
	if err != nil || topic != (livechat.Topic{Group: "oc_new", RootMessageID: "om_root", ThreadID: "omt_1"}) || r.Group() != "oc_new" {
		t.Fatalf("CreateTopic after setup: %+v %v", topic, err)
	}
	var link map[string]string
	if code := getJSON(t, admin, "POST", "/admin/support/join-link", `{"validity":"week"}`, &link); code != http.StatusOK || link["url"] != "https://applink.example/join" {
		t.Fatalf("join link: %d %v", code, link)
	}

	// A restart picks the app up from the store.
	cancel()
	r2 := newTestRuntime(t, kv, accounts.URL, open.URL, &listens)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go r2.Run(ctx2, nopSink{})
	waitFor(t, func() bool { return r2.Available() && r2.Status().Listener == "connected" })
	if s := r2.Status(); s.AppID != "cli_new" || s.ChatID != "oc_new" {
		t.Fatalf("status after restart: %+v", s)
	}
	if n := listens.Load(); n != 2 {
		t.Fatalf("long connection started %d times, want 2", n)
	}
}

func TestKVSettingsWithoutSealer(t *testing.T) {
	kv := &memKV{}
	s := KVSettings(kv, nil)
	got, err := s.Load(context.Background())
	if err != nil || got != nil {
		t.Fatalf("empty store: %+v %v", got, err)
	}
	want := &Settings{AppID: "cli", AppSecret: "sec", ChatID: "oc", ConnectedAt: time.Unix(1, 0).UTC()}
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(context.Background())
	if err != nil || *got != *want {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
