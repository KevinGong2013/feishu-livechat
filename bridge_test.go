package livechat_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	livechat "github.com/KevinGong2013/feishu-livechat"
	"github.com/KevinGong2013/feishu-livechat/sqlstore"
)

// fakeBackend is an in-memory topic group.
type fakeBackend struct {
	mu         sync.Mutex
	group      string
	down       bool // no app connected
	n          int
	created    []livechat.User
	posts      map[string][]string // root → posted texts / "[image]"
	gone       map[string]bool     // roots deleted in the group
	noThreadID bool
	images     map[string][]byte // agent image key → bytes
	acks       map[string]bool   // remote id → ok
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{group: "oc_group", posts: map[string][]string{}, gone: map[string]bool{}, images: map[string][]byte{}, acks: map[string]bool{}}
}

func (f *fakeBackend) Group() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.group
}

func (f *fakeBackend) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.down
}

func (f *fakeBackend) CreateTopic(_ context.Context, u livechat.User, _ string) (livechat.Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.created = append(f.created, u)
	t := livechat.Topic{Group: f.group, RootMessageID: fmt.Sprintf("om_root_%d", f.n)}
	if !f.noThreadID {
		t.ThreadID = fmt.Sprintf("omt_%d", f.n)
	}
	time.Sleep(5 * time.Millisecond) // widen the window for racing first messages
	return t, nil
}

func (f *fakeBackend) post(t livechat.Topic, s string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[t.RootMessageID] {
		return "", errors.New("root message was recalled")
	}
	f.n++
	f.posts[t.RootMessageID] = append(f.posts[t.RootMessageID], s)
	return fmt.Sprintf("om_post_%d", f.n), nil
}

func (f *fakeBackend) PostText(_ context.Context, t livechat.Topic, text, _ string) (string, error) {
	return f.post(t, text)
}

func (f *fakeBackend) PostImage(_ context.Context, t livechat.Topic, _ []byte, _ string) (string, error) {
	return f.post(t, "[image]")
}

func (f *fakeBackend) TopicGone(_ context.Context, t livechat.Topic) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gone[t.RootMessageID], nil
}

func (f *fakeBackend) FetchImage(_ context.Context, _, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.images[key]; ok {
		return b, nil
	}
	return nil, errors.New("no such image")
}

func (f *fakeBackend) Ack(_ context.Context, remoteID string, ok bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks[remoteID] = ok
	return nil
}

func (f *fakeBackend) topics() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

func (f *fakeBackend) postsIn(root string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts[root]...)
}

func (f *fakeBackend) ack(remoteID string) (ok, found bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok, found = f.acks[remoteID]
	return
}

// reactingBackend adds the optional read-reaction capability on top of a
// fakeBackend. fakeBackend itself does not implement it, so existing tests
// do not start background reactions.
type readCall struct {
	remoteID string
	emoji    string
	err      error
}

type reactingBackend struct {
	*fakeBackend
	emoji string
	fn    func(ctx context.Context, remoteID, emoji string) error
	mu    sync.Mutex
	calls []readCall
}

func (r *reactingBackend) ReadEmoji() string { return r.emoji }

func (r *reactingBackend) React(ctx context.Context, remoteID, emoji string) error {
	var err error
	if r.fn != nil {
		err = r.fn(ctx, remoteID, emoji)
	}
	r.mu.Lock()
	r.calls = append(r.calls, readCall{remoteID, emoji, err})
	r.mu.Unlock()
	return err
}

func (r *reactingBackend) snapshot() []readCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]readCall(nil), r.calls...)
}

func (r *reactingBackend) count(remoteID string) int {
	n := 0
	for _, c := range r.snapshot() {
		if c.remoteID == remoteID {
			n++
		}
	}
	return n
}

func (e *env) enableReadReactions(t *testing.T, emoji string, fn func(context.Context, string, string) error) *reactingBackend {
	t.Helper()
	rb := &reactingBackend{fakeBackend: e.backend, emoji: emoji, fn: fn}
	var err error
	e.bridge, err = livechat.New(livechat.Config{
		Store:   e.store,
		Blobs:   e.blobs,
		Backend: rb,
		OnAgentMessage: func(_ context.Context, userID string, m *livechat.Message, online bool) {
			e.hookMu.Lock()
			defer e.hookMu.Unlock()
			e.hooks = append(e.hooks, hookCall{userID, m, online})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rb
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func (e *env) agent(t *testing.T, remote, text string) {
	t.Helper()
	err := e.bridge.HandleAgentMessage(context.Background(), livechat.AgentMessage{
		RemoteID:      remote,
		RootMessageID: "om_root_1",
		ThreadID:      "omt_1",
		AgentID:       "ou_agent",
		Parts:         []livechat.Part{{Text: text}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func messageIDByRemote(t *testing.T, e *env, remote string) string {
	t.Helper()
	msgs, _, err := e.bridge.History(context.Background(), "alice", livechat.Query{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.RemoteID == remote {
			return m.ID
		}
	}
	t.Fatalf("no message with remote id %s", remote)
	return ""
}

type memBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (b *memBlobs) Put(_ context.Context, key, _ string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string][]byte{}
	}
	b.m[key] = append([]byte(nil), data...)
	return nil
}

func (b *memBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.m[key]
	if !ok {
		return nil, livechat.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

type hookCall struct {
	userID string
	msg    *livechat.Message
	online bool
}

type env struct {
	bridge  *livechat.Bridge
	store   *sqlstore.Store
	backend *fakeBackend
	blobs   *memBlobs
	hookMu  sync.Mutex
	hooks   []hookCall
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlstore.OpenSQLite(filepath.Join(t.TempDir(), "livechat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := sqlstore.New(db, sqlstore.SQLite)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &env{store: st, backend: newFakeBackend(), blobs: &memBlobs{}}
	e.bridge, err = livechat.New(livechat.Config{
		Store:   st,
		Blobs:   e.blobs,
		Backend: e.backend,
		OnAgentMessage: func(_ context.Context, userID string, m *livechat.Message, online bool) {
			e.hookMu.Lock()
			defer e.hookMu.Unlock()
			e.hooks = append(e.hooks, hookCall{userID, m, online})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var alice = livechat.User{ID: "alice", Name: "Alice"}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e *env) conv(t *testing.T, userID string) *livechat.Conversation {
	t.Helper()
	c, err := e.store.ConversationByUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFirstMessagesOpenExactlyOneTopic(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := e.bridge.SendText(ctx, alice, fmt.Sprintf("hello %d", i))
			if err != nil || m.Status != livechat.StatusSent || m.RemoteID == "" {
				t.Errorf("send %d: %+v %v", i, m, err)
			}
		}(i)
	}
	wg.Wait()
	if n := e.backend.topics(); n != 1 {
		t.Fatalf("created %d topics, want 1", n)
	}
	c := e.conv(t, "alice")
	if c.RootMessageID != "om_root_1" || c.ThreadID != "omt_1" {
		t.Fatalf("topic not recorded: %+v", c)
	}
	if posts := e.backend.postsIn(c.RootMessageID); len(posts) != 5 {
		t.Fatalf("posts in topic: %v", posts)
	}
	// Later messages reuse the topic.
	if _, err := e.bridge.SendText(ctx, alice, "again"); err != nil {
		t.Fatal(err)
	}
	if n := e.backend.topics(); n != 1 {
		t.Fatalf("second message opened another topic")
	}
}

func TestSendValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "   "); !errors.Is(err, livechat.ErrEmptyMessage) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := e.bridge.SendImage(ctx, alice, []byte("<svg></svg>")); !errors.Is(err, livechat.ErrBadImage) {
		t.Fatalf("svg: %v", err)
	}
	if n := e.backend.topics(); n != 0 {
		t.Fatal("rejected messages must not open a topic")
	}
}

func TestSendImage(t *testing.T) {
	e := newEnv(t)
	m, err := e.bridge.SendImage(context.Background(), alice, pngBytes(t, 4, 3))
	if err != nil {
		t.Fatal(err)
	}
	img := m.Blocks[0].Image
	if m.Status != livechat.StatusSent || img == nil || img.Width != 4 || img.Height != 3 || img.ContentType != "image/png" {
		t.Fatalf("unexpected message: %+v %+v", m, img)
	}
	if _, ok := e.blobs.m[img.Key]; !ok {
		t.Fatalf("image not stored under %s", img.Key)
	}
	if posts := e.backend.postsIn("om_root_1"); len(posts) != 1 || posts[0] != "[image]" {
		t.Fatalf("posts: %v", posts)
	}
}

func TestDeletedRootCardOpensNewTopic(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "first"); err != nil {
		t.Fatal(err)
	}
	e.backend.mu.Lock()
	e.backend.gone["om_root_1"] = true
	e.backend.mu.Unlock()

	m, err := e.bridge.SendText(ctx, alice, "second")
	if err != nil || m.Status != livechat.StatusSent {
		t.Fatalf("%+v %v", m, err)
	}
	c := e.conv(t, "alice")
	if c.RootMessageID == "om_root_1" || e.backend.topics() != 2 {
		t.Fatalf("no new topic: %+v", c)
	}
	if posts := e.backend.postsIn(c.RootMessageID); len(posts) != 1 || posts[0] != "second" {
		t.Fatalf("posts in new topic: %v", posts)
	}
}

func TestAgentReplyIsForwardedOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "help"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.bridge.Subscribe("alice")
	defer cancel()

	e.backend.images["img_1"] = pngBytes(t, 8, 6)
	am := livechat.AgentMessage{
		RemoteID:      "om_reply_1",
		RootMessageID: "om_root_1",
		ThreadID:      "omt_1",
		AgentID:       "ou_agent",
		Parts:         []livechat.Part{{Text: "看这里"}, {ImageKey: "img_1"}, {Text: "  "}},
	}
	if err := e.bridge.HandleAgentMessage(ctx, am); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-ch:
		if m.Sender != livechat.SenderAgent || len(m.Blocks) != 2 || m.Blocks[0].Text != "看这里" || m.Blocks[1].Image.Width != 8 {
			t.Fatalf("pushed message: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("reply was not pushed to the open page")
	}
	if ok, found := e.backend.ack("om_reply_1"); !found || !ok {
		t.Fatal("forwarded reply must be acked ok")
	}
	if len(e.hooks) != 1 || e.hooks[0].userID != "alice" || !e.hooks[0].online {
		t.Fatalf("hook calls: %+v", e.hooks)
	}

	// 飞书 redelivers the event: nothing new happens.
	if err := e.bridge.HandleAgentMessage(ctx, am); err != nil {
		t.Fatal(err)
	}
	msgs, _, _ := e.bridge.History(ctx, "alice", livechat.Query{})
	if len(msgs) != 2 || len(e.hooks) != 1 {
		t.Fatalf("duplicate event stored or hooked again: %d messages, %d hooks", len(msgs), len(e.hooks))
	}
	if n, _ := e.bridge.Unread(ctx, "alice"); n != 1 {
		t.Fatalf("unread = %d", n)
	}
	if err := e.bridge.MarkRead(ctx, "alice", msgs[1].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.bridge.Unread(ctx, "alice"); n != 0 {
		t.Fatalf("unread after read = %d", n)
	}
}

func TestAgentReplyEdgeCases(t *testing.T) {
	e := newEnv(t)
	e.backend.noThreadID = true
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "help"); err != nil {
		t.Fatal(err)
	}

	err := e.bridge.HandleAgentMessage(ctx, livechat.AgentMessage{RemoteID: "om_x", RootMessageID: "om_unknown", Parts: []livechat.Part{{Text: "hi"}}})
	if !errors.Is(err, livechat.ErrUnknownTopic) {
		t.Fatalf("unknown topic: %v", err)
	}
	if _, found := e.backend.ack("om_x"); found {
		t.Fatal("replies in unknown topics must not be acked")
	}

	err = e.bridge.HandleAgentMessage(ctx, livechat.AgentMessage{RemoteID: "om_empty", RootMessageID: "om_root_1", ThreadID: "omt_learned"})
	if err == nil {
		t.Fatal("a reply with nothing to forward must fail")
	}
	if ok, found := e.backend.ack("om_empty"); !found || ok {
		t.Fatal("a reply with nothing to forward must be acked as failed")
	}
	if c := e.conv(t, "alice"); c.ThreadID != "omt_learned" {
		t.Fatalf("thread id not learned from the reply: %+v", c)
	}

	err = e.bridge.HandleAgentMessage(ctx, livechat.AgentMessage{RemoteID: "om_img", ThreadID: "omt_learned", Parts: []livechat.Part{{ImageKey: "missing"}}})
	if err == nil {
		t.Fatal("a reply whose image cannot be fetched must fail")
	}
	if ok, found := e.backend.ack("om_img"); !found || ok {
		t.Fatal("failed image reply must be acked as failed")
	}
}

func TestRecall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _ = e.bridge.SendText(ctx, alice, "help")
	_ = e.bridge.HandleAgentMessage(ctx, livechat.AgentMessage{RemoteID: "om_r", RootMessageID: "om_root_1", Parts: []livechat.Part{{Text: "oops"}}})

	ch, cancel := e.bridge.Subscribe("alice")
	defer cancel()
	if err := e.bridge.HandleRecall(ctx, "om_r"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-ch:
		if !m.Recalled {
			t.Fatalf("pushed message not recalled: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("recall not pushed")
	}
	if err := e.bridge.HandleRecall(ctx, "om_unrelated"); err != nil {
		t.Fatalf("recall of an unrelated message: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if _, err := e.bridge.SendText(ctx, alice, "spam"); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if _, err := e.bridge.SendText(ctx, alice, "spam"); !errors.Is(err, livechat.ErrRateLimited) {
		t.Fatalf("21st message in a minute: %v", err)
	}
	if _, err := e.bridge.SendText(ctx, livechat.User{ID: "bob"}, "hi"); err != nil {
		t.Fatalf("other users are not limited: %v", err)
	}
}

func TestReconnectedGroupGetsNewTopics(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "first"); err != nil {
		t.Fatal(err)
	}
	// The operator connects another app and group.
	e.backend.mu.Lock()
	e.backend.group = "oc_new_group"
	e.backend.mu.Unlock()

	if _, err := e.bridge.SendText(ctx, alice, "after reconnect"); err != nil {
		t.Fatal(err)
	}
	c := e.conv(t, "alice")
	if c.TopicGroup != "oc_new_group" || c.RootMessageID == "om_root_1" || e.backend.topics() != 2 {
		t.Fatalf("no new topic in the new group: %+v", c)
	}
	if posts := e.backend.postsIn(c.RootMessageID); len(posts) != 1 || posts[0] != "after reconnect" {
		t.Fatalf("posts in new topic: %v", posts)
	}
}

func TestUnavailableBackendRefusesMessages(t *testing.T) {
	e := newEnv(t)
	e.backend.down = true
	if e.bridge.Available() {
		t.Fatal("bridge must report the backend unavailable")
	}
	if _, err := e.bridge.SendText(context.Background(), alice, "hello?"); !errors.Is(err, livechat.ErrUnavailable) {
		t.Fatalf("send while unavailable: %v", err)
	}
	if msgs, _, _ := e.bridge.History(context.Background(), "alice", livechat.Query{}); len(msgs) != 0 {
		t.Fatal("a refused message must not be stored")
	}
}

func TestReadReactionOnNewlyReadAgentMessages(t *testing.T) {
	e := newEnv(t)
	rb := e.enableReadReactions(t, "Get", nil)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_a", "a")
	e.agent(t, "om_b", "b")
	if err := e.bridge.HandleRecall(ctx, "om_b"); err != nil {
		t.Fatal(err)
	}
	c := e.conv(t, "alice")
	noRemote := &livechat.Message{
		ID:             uuid.Must(uuid.NewV7()).String(),
		ConversationID: c.ID,
		Sender:         livechat.SenderAgent,
		Blocks:         []livechat.Block{{Type: livechat.BlockText, Text: "local"}},
		Status:         livechat.StatusSent,
		CreatedAt:      time.Now().UTC(),
	}
	if _, err := e.store.InsertMessage(ctx, noRemote); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_c", "c")
	e.agent(t, "om_d", "d")

	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_c")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return rb.count("om_a") == 1 && rb.count("om_c") == 1 })
	if rb.count("om_b")+rb.count("om_d")+rb.count("") != 0 || len(rb.snapshot()) != 2 {
		t.Fatalf("reactions = %+v", rb.snapshot())
	}
	for _, call := range rb.snapshot() {
		if call.emoji != "Get" || call.err != nil {
			t.Fatalf("reaction call: %+v", call)
		}
	}
	if ok, found := e.backend.ack("om_a"); !found || !ok {
		t.Fatal("delivery ack DONE is independent of the read reaction")
	}

	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_d")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return rb.count("om_d") == 1 })
	if rb.count("om_a") != 1 || rb.count("om_c") != 1 || len(rb.snapshot()) != 3 {
		t.Fatalf("already-read messages were reacted again: %+v", rb.snapshot())
	}
}

func TestReadReactionDoesNotBackfill(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_old", "old")
	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_old")); err != nil {
		t.Fatal(err)
	}
	rb := e.enableReadReactions(t, "OK", nil)
	e.agent(t, "om_new", "new")
	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_new")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return rb.count("om_new") == 1 })
	if rb.count("om_old") != 0 || rb.snapshot()[0].emoji != "OK" {
		t.Fatalf("reactions = %+v", rb.snapshot())
	}
}

func TestEmptyReadEmojiSkipsReaction(t *testing.T) {
	e := newEnv(t)
	rb := e.enableReadReactions(t, "", nil)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_a", "a")
	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_a")); err != nil {
		t.Fatal(err)
	}
	if len(rb.snapshot()) != 0 {
		t.Fatalf("empty ReadEmoji still reacted: %+v", rb.snapshot())
	}
}

func TestReadReactionRetriesTemporaryFailure(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	fails := map[string]int{}
	rb := e.enableReadReactions(t, "Get", func(_ context.Context, remoteID, _ string) error {
		mu.Lock()
		fails[remoteID]++
		n := fails[remoteID]
		mu.Unlock()
		if remoteID == "om_a" && n == 1 {
			return errors.New("feishu timeout")
		}
		return nil
	})
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_a", "a")
	e.agent(t, "om_b", "b")
	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_b")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return rb.count("om_a") == 1 && rb.count("om_b") == 1 })
	// The failed row is put back to 0 after React returns. Keep reading newer
	// messages until that release is visible and om_a is claimed again.
	deadline := time.Now().Add(5 * time.Second)
	for rb.count("om_a") < 2 && time.Now().Before(deadline) {
		remote := fmt.Sprintf("om_more_%d", time.Now().UnixNano())
		e.agent(t, remote, remote)
		if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, remote)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rb.count("om_a") != 2 || rb.count("om_b") != 1 {
		t.Fatalf("reactions = %+v", rb.snapshot())
	}
	var goneErr bool
	for _, c := range rb.snapshot() {
		if c.remoteID == "om_a" && c.err != nil {
			goneErr = true
		}
	}
	if !goneErr {
		t.Fatal("the failed attempt was not recorded")
	}
}

func TestReadReactionDoesNotRetryGoneMessage(t *testing.T) {
	e := newEnv(t)
	rb := e.enableReadReactions(t, "Get", func(_ context.Context, remoteID, _ string) error {
		if remoteID == "om_gone" {
			return fmt.Errorf("recalled: %w", livechat.ErrMessageGone)
		}
		return nil
	})
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_gone", "gone")
	e.agent(t, "om_ok", "ok")
	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_ok")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return rb.count("om_gone") == 1 && rb.count("om_ok") == 1 })
	e.agent(t, "om_next", "next")
	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_next")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return rb.count("om_next") == 1 })
	if rb.count("om_gone") != 1 || rb.count("om_ok") != 1 {
		t.Fatalf("gone message was retried: %+v", rb.snapshot())
	}
}

func TestReadReactionRace(t *testing.T) {
	e := newEnv(t)
	rb := e.enableReadReactions(t, "Get", nil)
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	var remotes []string
	for i := 0; i < 5; i++ {
		remote := fmt.Sprintf("om_race_%d", i)
		remotes = append(remotes, remote)
		e.agent(t, remote, remote)
	}
	last := messageIDByRemote(t, e, remotes[len(remotes)-1])
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.bridge.MarkRead(ctx, "alice", last); err != nil {
				t.Errorf("MarkRead: %v", err)
			}
		}()
	}
	wg.Wait()
	var lastN int
	var stableSince time.Time
	waitUntil(t, func() bool {
		n := len(rb.snapshot())
		if n != lastN {
			lastN = n
			stableSince = time.Now()
			return false
		}
		return n == len(remotes) && time.Since(stableSince) > 50*time.Millisecond
	})
	if len(rb.snapshot()) != len(remotes) {
		t.Fatalf("reactions = %+v", rb.snapshot())
	}
	for _, remote := range remotes {
		if rb.count(remote) != 1 {
			t.Fatalf("remote %s claimed %d times: %+v", remote, rb.count(remote), rb.snapshot())
		}
	}
}

func TestMarkReadDoesNotBlockOnReaction(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	rb := e.enableReadReactions(t, "Get", func(ctx context.Context, _, _ string) error {
		<-release
		return ctx.Err()
	})
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_a", "a")
	id := messageIDByRemote(t, e, "om_a")

	done := make(chan error, 1)
	go func() {
		done <- e.bridge.MarkRead(ctx, "alice", id)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("MarkRead blocked on the reaction")
	}
	once.Do(func() { close(release) })
	waitUntil(t, func() bool {
		calls := rb.snapshot()
		return len(calls) == 1 && calls[0].err == nil
	})
}

func TestHTTPReadDoesNotWaitForReaction(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var once sync.Once
	rb := e.enableReadReactions(t, "Get", func(ctx context.Context, _, _ string) error {
		<-release
		return ctx.Err()
	})
	ctx := context.Background()
	if _, err := e.bridge.SendText(ctx, alice, "hi"); err != nil {
		t.Fatal(err)
	}
	e.agent(t, "om_a", "a")
	id := messageIDByRemote(t, e, "om_a")

	h := e.bridge.Handler(livechat.HandlerOptions{
		Authenticate: func(r *http.Request) (livechat.User, error) {
			return alice, nil
		},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/read", strings.NewReader(`{"message_id":"`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /read blocked or failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	once.Do(func() { close(release) })
	waitUntil(t, func() bool {
		calls := rb.snapshot()
		return len(calls) == 1 && calls[0].err == nil && calls[0].remoteID == "om_a"
	})

	// The request context was canceled when the handler returned. The reaction
	// still stuck, so a later read does not send it again.
	e.agent(t, "om_b", "b")
	if err := e.bridge.MarkRead(ctx, "alice", messageIDByRemote(t, e, "om_b")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return rb.count("om_b") == 1 })
	if rb.count("om_a") != 1 {
		t.Fatalf("canceled request context dropped the reaction: %+v", rb.snapshot())
	}
}
