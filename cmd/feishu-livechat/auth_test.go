package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func sign(t *testing.T, method jwt.SigningMethod, key any, c userClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, c).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claimsFor(sub string, exp time.Duration) userClaims {
	return userClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(exp))},
		Name:             "张三",
		Fields:           []livechat.Field{{Label: "邮箱", Value: "z@example.com"}},
		Links:            []livechat.Link{{Text: "后台", URL: "https://admin.example.com/u/1"}},
	}
}

func TestJWTAuthenticator(t *testing.T) {
	auth := jwtAuthenticator(testSecret)
	req := func(tok string) *http.Request {
		r := httptest.NewRequest("GET", "/api/messages", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return r
	}

	u, err := auth(req(sign(t, jwt.SigningMethodHS256, testSecret, claimsFor("u1", time.Hour))))
	if err != nil || u.ID != "u1" || u.Name != "张三" || len(u.Fields) != 1 || len(u.Links) != 1 {
		t.Fatalf("valid token: %+v %v", u, err)
	}
	bad := map[string]string{
		"missing":      "",
		"expired":      sign(t, jwt.SigningMethodHS256, testSecret, claimsFor("u1", -time.Minute)),
		"wrong secret": sign(t, jwt.SigningMethodHS256, []byte("another-secret-another-secret-xx"), claimsFor("u1", time.Hour)),
		"alg none":     sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claimsFor("u1", time.Hour)),
		"no sub":       sign(t, jwt.SigningMethodHS256, testSecret, claimsFor("", time.Hour)),
	}
	noExp := claimsFor("u1", time.Hour)
	noExp.ExpiresAt = nil
	bad["no exp"] = sign(t, jwt.SigningMethodHS256, testSecret, noExp)
	for name, tok := range bad {
		if _, err := auth(req(tok)); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}

func TestCORS(t *testing.T) {
	h := cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }), "https://www.example.com, https://app.example.com/")
	do := func(method, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/messages", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("OPTIONS", "https://app.example.com"); w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("preflight from allowed origin: %d %v", w.Code, w.Header())
	}
	if w := do("GET", "https://evil.example.com"); w.Header().Get("Access-Control-Allow-Origin") != "" || w.Code != http.StatusTeapot {
		t.Fatalf("other origin got CORS headers: %v", w.Header())
	}
}
