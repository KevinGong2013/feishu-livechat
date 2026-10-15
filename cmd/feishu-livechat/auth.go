package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// userClaims is the token the host site signs (HS256, shared secret) for a
// logged-in user and hands to the chat widget. sub is the user's stable id;
// name, fields and links fill the root card when the user's first message
// opens their topic.
type userClaims struct {
	jwt.RegisteredClaims
	Name   string           `json:"name,omitempty"`
	Fields []livechat.Field `json:"fields,omitempty"`
	Links  []livechat.Link  `json:"links,omitempty"`
}

func jwtAuthenticator(secret []byte) func(*http.Request) (livechat.User, error) {
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	return func(r *http.Request) (livechat.User, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			return livechat.User{}, errors.New("missing bearer token")
		}
		var c userClaims
		if _, err := parser.ParseWithClaims(tok, &c, func(*jwt.Token) (any, error) { return secret, nil }); err != nil {
			return livechat.User{}, err
		}
		if c.Subject == "" {
			return livechat.User{}, errors.New("token has no sub")
		}
		return livechat.User{ID: c.Subject, Name: c.Name, Fields: c.Fields, Links: c.Links}, nil
	}
}

type pairs []string

func (p *pairs) String() string     { return strings.Join(*p, ",") }
func (p *pairs) Set(v string) error { *p = append(*p, v); return nil }

// token mints a user token with LIVECHAT_JWT_SECRET — for trying the demo
// page; in production the host site signs these itself.
func token(args []string) error {
	fl := flag.NewFlagSet("token", flag.ExitOnError)
	user := fl.String("user", "", "user id (sub)")
	name := fl.String("name", "", "display name")
	ttl := fl.Duration("ttl", 24*time.Hour, "lifetime")
	var fields, links pairs
	fl.Var(&fields, "field", "card field 标签=值 (repeatable)")
	fl.Var(&links, "link", "card button 文字=URL (repeatable)")
	_ = fl.Parse(args)
	if *user == "" {
		return errors.New("-user is required")
	}
	if err := mustEnv("LIVECHAT_JWT_SECRET"); err != nil {
		return err
	}
	c := userClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   *user,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(*ttl)),
		},
		Name: *name,
	}
	for _, f := range fields {
		k, v, _ := strings.Cut(f, "=")
		c.Fields = append(c.Fields, livechat.Field{Label: k, Value: v})
	}
	for _, l := range links {
		k, v, _ := strings.Cut(l, "=")
		c.Links = append(c.Links, livechat.Link{Text: k, URL: v})
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(env("LIVECHAT_JWT_SECRET", "")))
	if err != nil {
		return err
	}
	fmt.Println(s)
	return nil
}
