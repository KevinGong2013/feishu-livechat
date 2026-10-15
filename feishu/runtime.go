package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	"rsc.io/qr"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// Runtime is the Backend for hosts that connect 飞书 from an operator page
// rather than from the environment. It starts unconfigured (the bridge
// then reports itself unavailable), connects an app through the
// scan-to-create flow (StartSetup), stores the result in a SettingsStore,
// and switches to it without a restart — reconnecting later switches
// apps and groups the same way.
type Runtime struct {
	store       SettingsStore
	base        Config
	log         *slog.Logger
	accountsURL string
	botWait     time.Duration
	// listen runs one long-connection session; replaced in tests.
	listen func(ctx context.Context, c *Client, sink Sink) error

	mu         sync.RWMutex
	client     *Client
	settings   *Settings
	runCtx     context.Context
	sink       Sink
	stopListen context.CancelFunc
	state      string
	lastErr    string

	setupMu sync.Mutex
	setup   *setupSession
}

var _ livechat.Backend = (*Runtime)(nil)
var _ livechat.Availability = (*Runtime)(nil)

// RuntimeConfig configures a Runtime.
type RuntimeConfig struct {
	Settings SettingsStore
	// Base supplies everything except the app and the group, which come
	// from Settings: CardHint, the ack emojis, MaxImageBytes, Logger.
	Base Config
	// AccountsURL and Base.OpenBaseURL redirect the 飞书 endpoints (tests).
	AccountsURL string
}

// NewRuntime returns an unconfigured Runtime; Run loads stored settings.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if cfg.Settings == nil {
		return nil, errors.New("feishu: RuntimeConfig.Settings is required")
	}
	log := cfg.Base.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{
		store:       cfg.Settings,
		base:        cfg.Base,
		log:         log,
		accountsURL: cfg.AccountsURL,
		botWait:     time.Minute,
		listen:      func(ctx context.Context, c *Client, sink Sink) error { return c.Listen(ctx, sink) },
		state:       "stopped",
	}, nil
}

// Run connects the stored app, if any, and keeps its long connection up
// until ctx is done. Run it in exactly the process(es) that should handle
// agent replies. A stored app that cannot be loaded is logged and shown in
// Status; it does not stop Run, so the host keeps running without support
// chat.
func (r *Runtime) Run(ctx context.Context, sink Sink) error {
	r.mu.Lock()
	r.runCtx, r.sink = ctx, sink
	r.mu.Unlock()

	s, err := r.store.Load(ctx)
	switch {
	case err != nil:
		r.log.Error("feishu: load settings", "err", err)
		r.mu.Lock()
		r.state, r.lastErr = "error", "load settings: "+err.Error()
		r.mu.Unlock()
	case s != nil:
		if err := r.use(s); err != nil {
			r.log.Error("feishu: connect stored app", "app", s.AppID, "err", err)
		}
	}

	<-ctx.Done()
	r.mu.Lock()
	if r.stopListen != nil {
		r.stopListen()
		r.stopListen = nil
	}
	r.state = "stopped"
	r.mu.Unlock()
	return nil
}

func (r *Runtime) clientConfig(s *Settings) Config {
	c := r.base
	c.AppID, c.AppSecret, c.ChatID, c.Lark = s.AppID, s.AppSecret, s.ChatID, s.Lark
	c.Logger = r.log
	return c
}

// use makes s the connected app: later backend calls go to it, and if Run
// is active its long connection replaces the previous one.
func (r *Runtime) use(s *Settings) error {
	c, err := New(r.clientConfig(s))
	if err != nil {
		return err
	}
	r.mu.Lock()
	old := r.stopListen
	r.client, r.settings, r.stopListen, r.lastErr = c, s, nil, ""
	if r.runCtx != nil && r.runCtx.Err() == nil {
		lctx, cancel := context.WithCancel(r.runCtx)
		r.stopListen = cancel
		r.state = "connecting"
		c.onState = func(state string, err error) { r.setState(c, state, err) }
		go r.keepListening(lctx, c, r.sink)
	}
	r.mu.Unlock()
	if old != nil {
		old()
	}
	return nil
}

func (r *Runtime) keepListening(ctx context.Context, c *Client, sink Sink) {
	backoff := 5 * time.Second
	for {
		err := r.listen(ctx, c, sink)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("long connection closed")
		}
		r.setState(c, "error", err)
		r.log.Error("feishu: long connection", "app", c.cfg.AppID, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
		r.setState(c, "connecting", nil)
	}
}

func (r *Runtime) setState(c *Client, state string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != c {
		return // a replaced app's connection winding down
	}
	if state != "" {
		r.state = state
	}
	switch {
	case err != nil:
		r.lastErr = err.Error()
	case state == "connected":
		r.lastErr = ""
	}
}

func (r *Runtime) current() (*Client, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.client == nil {
		return nil, livechat.ErrUnavailable
	}
	return r.client, nil
}

// Available reports whether an app is connected.
func (r *Runtime) Available() bool {
	_, err := r.current()
	return err == nil
}

func (r *Runtime) Group() string {
	if c, err := r.current(); err == nil {
		return c.Group()
	}
	return ""
}

func (r *Runtime) CreateTopic(ctx context.Context, u livechat.User, dedupeKey string) (livechat.Topic, error) {
	c, err := r.current()
	if err != nil {
		return livechat.Topic{}, err
	}
	return c.CreateTopic(ctx, u, dedupeKey)
}

func (r *Runtime) PostText(ctx context.Context, t livechat.Topic, text, dedupeKey string) (string, error) {
	c, err := r.current()
	if err != nil {
		return "", err
	}
	return c.PostText(ctx, t, text, dedupeKey)
}

func (r *Runtime) PostImage(ctx context.Context, t livechat.Topic, data []byte, dedupeKey string) (string, error) {
	c, err := r.current()
	if err != nil {
		return "", err
	}
	return c.PostImage(ctx, t, data, dedupeKey)
}

func (r *Runtime) TopicGone(ctx context.Context, t livechat.Topic) (bool, error) {
	c, err := r.current()
	if err != nil {
		return false, err
	}
	return c.TopicGone(ctx, t)
}

func (r *Runtime) FetchImage(ctx context.Context, remoteID, imageKey string) ([]byte, error) {
	c, err := r.current()
	if err != nil {
		return nil, err
	}
	return c.FetchImage(ctx, remoteID, imageKey)
}

func (r *Runtime) Ack(ctx context.Context, remoteID string, ok bool) error {
	c, err := r.current()
	if err != nil {
		return err
	}
	return c.Ack(ctx, remoteID, ok)
}

// ShareLink returns a new join link for the connected group.
func (r *Runtime) ShareLink(ctx context.Context, validity string) (string, error) {
	c, err := r.current()
	if err != nil {
		return "", err
	}
	return c.ShareLink(ctx, c.Group(), validity)
}

// Status is what the operator page shows.
type Status struct {
	Configured  bool       `json:"configured"`
	AppID       string     `json:"app_id,omitempty"`
	BotName     string     `json:"bot_name,omitempty"`
	ChatID      string     `json:"chat_id,omitempty"`
	GroupName   string     `json:"group_name,omitempty"`
	Lark        bool       `json:"lark,omitempty"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	// Listener is the long connection: stopped, connecting, connected,
	// disconnected or error (see Error).
	Listener string `json:"listener"`
	Error    string `json:"error,omitempty"`
	// Setup is the scan flow in progress or just finished, if any.
	Setup *SetupStatus `json:"setup,omitempty"`
}

// Status reports the connected app and the long connection.
func (r *Runtime) Status() Status {
	r.mu.RLock()
	st := Status{Listener: r.state, Error: r.lastErr}
	if s := r.settings; s != nil {
		st.Configured = true
		st.AppID, st.BotName, st.ChatID, st.GroupName, st.Lark = s.AppID, s.BotName, s.ChatID, s.GroupName, s.Lark
		at := s.ConnectedAt
		st.ConnectedAt = &at
	}
	r.mu.RUnlock()
	st.Setup = r.setupStatus()
	return st
}

// SetupOptions names what StartSetup creates.
type SetupOptions struct {
	AppName   string // pre-filled on the creation page; default 在线客服
	GroupName string // the new 话题群; default 在线客服
}

// SetupStatus is the scan flow's progress: waiting (for the scan and
// confirmation), creating (the group), done, failed, declined or expired.
type SetupStatus struct {
	State     string    `json:"state"`
	URL       string    `json:"url,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type setupSession struct {
	SetupStatus
	png    []byte
	cancel context.CancelFunc
}

func (s *setupSession) active() bool {
	return (s.State == "waiting" && time.Now().Before(s.ExpiresAt)) || s.State == "starting" || s.State == "creating"
}

// StartSetup starts the scan-to-create flow and returns once its link is
// ready: open it in 飞书 (QRCodePNG renders it), confirm the pre-filled app,
// and the Runtime creates the 话题群 with the person who scanned in it,
// saves the settings and switches to the new app. A flow already waiting
// for its scan is returned as is, so reloading the page does not
// invalidate the code being scanned. Progress shows in Status().Setup.
func (r *Runtime) StartSetup(ctx context.Context, o SetupOptions) (SetupStatus, error) {
	if o.AppName == "" {
		o.AppName = "在线客服"
	}
	if o.GroupName == "" {
		o.GroupName = "在线客服"
	}
	r.setupMu.Lock()
	if r.setup != nil && r.setup.active() {
		st := r.setup.SetupStatus
		r.setupMu.Unlock()
		return st, nil
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	sess := &setupSession{SetupStatus: SetupStatus{State: "starting"}, cancel: cancel}
	r.setup = sess
	r.setupMu.Unlock()

	ready := make(chan struct{})
	go r.runSetup(sctx, sess, o, ready)
	select {
	case <-ready:
	case <-ctx.Done():
		return SetupStatus{}, ctx.Err()
	}
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if sess.State == "failed" {
		return sess.SetupStatus, errors.New(sess.Error)
	}
	return sess.SetupStatus, nil
}

// CancelSetup abandons the scan flow in progress, if any.
func (r *Runtime) CancelSetup() {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if r.setup != nil && r.setup.active() {
		r.setup.cancel()
		r.setup.State, r.setup.Error = "failed", "cancelled"
	}
}

// SetupQRCode returns the current setup link as a PNG QR code.
func (r *Runtime) SetupQRCode() ([]byte, bool) {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if r.setup == nil || r.setup.State != "waiting" || r.setup.png == nil {
		return nil, false
	}
	return r.setup.png, true
}

func (r *Runtime) setupStatus() *SetupStatus {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if r.setup == nil {
		return nil
	}
	st := r.setup.SetupStatus
	if st.State == "waiting" && time.Now().After(st.ExpiresAt) {
		st.State = "expired"
	}
	return &st
}

func (r *Runtime) updateSetup(sess *setupSession, fn func(*setupSession)) {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if sess.State == "failed" && sess.Error == "cancelled" {
		return
	}
	fn(sess)
}

func (r *Runtime) runSetup(ctx context.Context, sess *setupSession, o SetupOptions, ready chan struct{}) {
	defer sess.cancel()
	var once sync.Once
	signal := func() { once.Do(func() { close(ready) }) }
	defer signal()

	fail := func(state string, err error) {
		r.log.Warn("feishu: setup", "state", state, "err", err)
		r.updateSetup(sess, func(s *setupSession) { s.State, s.Error, s.png = state, err.Error(), nil })
	}

	reg, err := RegisterApp(ctx, RegisterOptions{
		Name:        o.AppName,
		AccountsURL: r.accountsURL,
		OnURL: func(url string, expiresIn int) {
			png, _ := QRCodePNG(url)
			r.updateSetup(sess, func(s *setupSession) {
				s.State, s.URL, s.ExpiresAt, s.png = "waiting", url, time.Now().Add(time.Duration(expiresIn)*time.Second), png
			})
			signal()
		},
	})
	var expired *registration.ExpiredError
	var denied *registration.AccessDeniedError
	switch {
	case errors.As(err, &expired):
		fail("expired", err)
		return
	case errors.As(err, &denied):
		fail("declined", err)
		return
	case err != nil:
		fail("failed", err)
		return
	}
	r.updateSetup(sess, func(s *setupSession) { s.State, s.png = "creating", nil })

	s := &Settings{AppID: reg.AppID, AppSecret: reg.AppSecret, Lark: reg.Lark, GroupName: o.GroupName}
	c, err := New(r.clientConfig(s))
	if err != nil {
		fail("failed", err)
		return
	}
	bot, err := c.WaitReady(ctx, r.botWait)
	if err != nil {
		fail("failed", fmt.Errorf("应用 %s 已创建，但机器人能力还不可用（%v）；在开放平台为它开启机器人能力并发布版本后重新扫码", reg.AppID, err))
		return
	}
	var members []string
	if reg.CreatorOpenID != "" {
		members = append(members, reg.CreatorOpenID)
	}
	s.ChatID, err = c.CreateTopicGroup(ctx, o.GroupName, "在线客服：每位用户一个话题，@机器人 的回复会发给用户。", members...)
	if err != nil {
		fail("failed", fmt.Errorf("create group: %w", err))
		return
	}
	s.BotName, s.ConnectedAt = bot.AppName, time.Now().UTC()
	if err := r.store.Save(ctx, s); err != nil {
		fail("failed", fmt.Errorf("save settings: %w", err))
		return
	}
	if err := r.use(s); err != nil {
		fail("failed", err)
		return
	}
	r.updateSetup(sess, func(s *setupSession) { s.State = "done" })
}

// WaitReady polls the bot info until the app's bot is live (right after
// the app is created it can take a moment) or max has passed.
func (c *Client) WaitReady(ctx context.Context, max time.Duration) (BotInfo, error) {
	deadline := time.Now().Add(max)
	for {
		bot, err := c.Bot(ctx)
		if err == nil {
			if c.botOpenID == "" {
				c.botOpenID = bot.OpenID
			}
			return bot, nil
		}
		if time.Now().After(deadline) {
			return BotInfo{}, err
		}
		select {
		case <-ctx.Done():
			return BotInfo{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// QRCodePNG renders a link as a QR code image.
func QRCodePNG(link string) ([]byte, error) {
	code, err := qr.Encode(link, qr.L)
	if err != nil {
		return nil, err
	}
	code.Scale = 6
	return code.PNG(), nil
}
