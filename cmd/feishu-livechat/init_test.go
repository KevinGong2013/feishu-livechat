package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KevinGong2013/feishu-livechat/feishu"
)

// fakeFeishu serves the scan-to-create registration endpoint and the Open
// API calls init makes.
func fakeFeishu(t *testing.T) (accounts, open *httptest.Server, chatBody *atomic.Value) {
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
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"client_id":     "cli_new",
				"client_secret": "secret_new",
				"user_info":     map[string]any{"open_id": "ou_creator", "tenant_brand": "feishu"},
			})
		}
	}))
	t.Cleanup(accounts.Close)

	chatBody = &atomic.Value{}
	open = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/v3/tenant_access_token/internal"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t-1","expire":7200}`)
		case r.URL.Path == "/open-apis/bot/v3/info":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","bot":{"open_id":"ou_bot","app_name":"在线客服"}}`)
		case r.URL.Path == "/open-apis/im/v1/chats" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			chatBody.Store(r.URL.RawQuery + " " + string(b))
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"chat_id":"oc_new"}}`)
		case r.URL.Path == "/open-apis/im/v1/chats/oc_new/link":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"share_link":"https://applink.example/join"}}`)
		default:
			t.Errorf("unexpected Open API call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(open.Close)
	return accounts, open, chatBody
}

func TestInit(t *testing.T) {
	accounts, open, chatBody := fakeFeishu(t)
	envPath := filepath.Join(t.TempDir(), "feishu-livechat.env")
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runInit(ctx, initConfig{
		appName: "在线客服", groupName: "客服群", envFile: envPath, link: "week",
		accountsURL: accounts.URL, openBaseURL: open.URL, botWait: time.Second,
	}, &out)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}

	// The creation link pre-fills the name, our scopes and our events.
	var link string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "https://accounts.example/verify") {
			link = line
		}
	}
	u, err := url.Parse(link)
	if err != nil || link == "" {
		t.Fatalf("no creation link in output:\n%s", out.String())
	}
	if u.Query().Get("name") != "在线客服" || u.Query().Get("createOnly") != "true" {
		t.Fatalf("link query: %v", u.Query())
	}
	addons := decodeAddons(t, u.Query().Get("addons"))
	for _, s := range feishu.Scopes {
		if !strings.Contains(addons, `"`+s+`"`) {
			t.Errorf("scope %s not requested: %s", s, addons)
		}
	}
	for _, e := range feishu.Events {
		if !strings.Contains(addons, `"`+e+`"`) {
			t.Errorf("event %s not requested: %s", e, addons)
		}
	}

	// The group is a 话题 group with the creator in it.
	body, _ := chatBody.Load().(string)
	for _, want := range []string{"user_id_type=open_id", `"group_message_type":"thread"`, `"ou_creator"`, `"name":"客服群"`} {
		if !strings.Contains(body, want) {
			t.Errorf("create chat request lacks %s: %s", want, body)
		}
	}

	// The configuration is written, owner-readable only, and loads back.
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("env file mode %v, want 0600", info.Mode().Perm())
	}
	for _, k := range []string{"FEISHU_APP_ID", "FEISHU_APP_SECRET", "FEISHU_CHAT_ID", "LIVECHAT_JWT_SECRET", "FEISHU_LARK"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	t.Setenv("FEISHU_APP_ID", "from-environment") // the environment wins
	if err := loadEnvFile(envPath); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FEISHU_APP_ID") != "from-environment" || os.Getenv("FEISHU_APP_SECRET") != "secret_new" ||
		os.Getenv("FEISHU_CHAT_ID") != "oc_new" || len(os.Getenv("LIVECHAT_JWT_SECRET")) != 64 {
		t.Fatalf("loaded env: id=%q secret=%q chat=%q jwt=%d", os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET"), os.Getenv("FEISHU_CHAT_ID"), len(os.Getenv("LIVECHAT_JWT_SECRET")))
	}
	if !strings.Contains(out.String(), "https://applink.example/join") {
		t.Errorf("join link not shown:\n%s", out.String())
	}

	// Running it again must not overwrite the credentials.
	if err := runInit(ctx, initConfig{envFile: envPath}, io.Discard); err == nil {
		t.Fatal("init over an existing env file must fail")
	}
}

func decodeAddons(t *testing.T, s string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("addons not base64url: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("addons not gzip: %v", err)
	}
	b, _ := io.ReadAll(zr)
	return string(b)
}

func TestRenderQR(t *testing.T) {
	s, err := renderQR("https://accounts.feishu.cn/verify?user_code=ABCD")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	width := len([]rune(lines[0]))
	if width < 25 || len(lines) != (width+1)/2 {
		t.Fatalf("unexpected QR shape: %d lines of %d", len(lines), width)
	}
	for _, l := range lines {
		if len([]rune(l)) != width {
			t.Fatalf("ragged QR line: %q", l)
		}
	}
}
