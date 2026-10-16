package sqlstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	livechat "github.com/KevinGong2013/feishu-livechat"
	"github.com/KevinGong2013/feishu-livechat/sqlstore"
)

// stores returns a fresh SQLite store, plus a Postgres one when
// LIVECHAT_TEST_POSTGRES_URL is set (its livechat_ tables are dropped
// first).
func stores(t *testing.T) map[string]*sqlstore.Store {
	t.Helper()
	ctx := context.Background()
	out := map[string]*sqlstore.Store{}

	db, err := sqlstore.OpenSQLite(filepath.Join(t.TempDir(), "livechat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	out["sqlite"] = sqlstore.New(db, sqlstore.SQLite)

	if dsn := os.Getenv("LIVECHAT_TEST_POSTGRES_URL"); dsn != "" {
		pg, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pg.Close() })
		for _, tbl := range []string{"livechat_messages", "livechat_conversations", "livechat_settings", "livechat_schema_migrations"} {
			if _, err := pg.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
				t.Fatal(err)
			}
		}
		out["postgres"] = sqlstore.New(pg, sqlstore.Postgres)
	}
	for name, s := range out {
		// Twice: migrations must be idempotent.
		for i := 0; i < 2; i++ {
			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("%s: migrate: %v", name, err)
			}
		}
	}
	return out
}

func msg(convID string, s livechat.Sender, remoteID string) *livechat.Message {
	return &livechat.Message{
		ID:             uuid.Must(uuid.NewV7()).String(),
		ConversationID: convID,
		Sender:         s,
		Blocks:         []livechat.Block{{Type: livechat.BlockText, Text: "hi"}},
		Status:         livechat.StatusSent,
		RemoteID:       remoteID,
		CreatedAt:      time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestStore(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("conversations", func(t *testing.T) { testConversations(t, s) })
			t.Run("messages", func(t *testing.T) { testMessages(t, s) })
			t.Run("unread", func(t *testing.T) { testUnread(t, s) })
		})
	}
}

func testConversations(t *testing.T, s *sqlstore.Store) {
	ctx := context.Background()
	c1, err := s.EnsureConversation(ctx, "user-a", "A")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.EnsureConversation(ctx, "user-a", "A again")
	if err != nil {
		t.Fatal(err)
	}
	if c1.ID != c2.ID || c2.UserName != "A" || c1.RootMessageID != "" {
		t.Fatalf("EnsureConversation must be idempotent: %+v %+v", c1, c2)
	}
	if _, err := s.ConversationByTopic(ctx, "", "om_x"); !errors.Is(err, livechat.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	ok, err := s.SetTopic(ctx, c1.ID, "", livechat.Topic{Group: "oc_g", RootMessageID: "om_root"})
	if err != nil || !ok {
		t.Fatalf("SetTopic: %v %v", ok, err)
	}
	if ok, _ := s.SetTopic(ctx, c1.ID, "", livechat.Topic{RootMessageID: "om_other"}); ok {
		t.Fatal("SetTopic must fail when the root changed underneath")
	}
	if ok, _ := s.SetTopic(ctx, c1.ID, "om_root", livechat.Topic{Group: "oc_g", RootMessageID: "om_root", ThreadID: "omt_1"}); !ok {
		t.Fatal("SetTopic with the current root must succeed")
	}
	if c, _ := s.ConversationByID(ctx, c1.ID); c.Topic() != (livechat.Topic{Group: "oc_g", RootMessageID: "om_root", ThreadID: "omt_1"}) {
		t.Fatalf("topic not stored: %+v", c.Topic())
	}
	for _, q := range [][2]string{{"omt_1", ""}, {"", "om_root"}, {"omt_unknown", "om_root"}} {
		c, err := s.ConversationByTopic(ctx, q[0], q[1])
		if err != nil || c.ID != c1.ID {
			t.Fatalf("ConversationByTopic(%q, %q) = %v, %v", q[0], q[1], c, err)
		}
	}
}

func testMessages(t *testing.T, s *sqlstore.Store) {
	ctx := context.Background()
	c, _ := s.EnsureConversation(ctx, "user-m", "M")

	var ids []string
	for i := 0; i < 5; i++ {
		m := msg(c.ID, livechat.SenderUser, "")
		if ok, err := s.InsertMessage(ctx, m); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
		ids = append(ids, m.ID)
	}

	agent := msg(c.ID, livechat.SenderAgent, "om_agent_1")
	if ok, _ := s.InsertMessage(ctx, agent); !ok {
		t.Fatal("agent message not inserted")
	}
	dup := msg(c.ID, livechat.SenderAgent, "om_agent_1")
	if ok, err := s.InsertMessage(ctx, dup); err != nil || ok {
		t.Fatalf("duplicate remote id must be skipped: %v %v", ok, err)
	}
	ids = append(ids, agent.ID)

	conv, _ := s.ConversationByID(ctx, c.ID)
	if conv.LastSender != livechat.SenderAgent || conv.LastMessageAt.IsZero() {
		t.Fatalf("last message fields not bumped: %+v", conv)
	}

	page, more, err := s.ListMessages(ctx, c.ID, livechat.Query{Limit: 2})
	if err != nil || !more || len(page) != 2 || page[0].ID != ids[4] || page[1].ID != ids[5] {
		t.Fatalf("newest page wrong: more=%v err=%v %v", more, err, idsOf(page))
	}
	page, more, _ = s.ListMessages(ctx, c.ID, livechat.Query{Before: ids[4], Limit: 3})
	if !more || len(page) != 3 || page[0].ID != ids[1] || page[2].ID != ids[3] {
		t.Fatalf("before page wrong: more=%v %v", more, idsOf(page))
	}
	page, more, _ = s.ListMessages(ctx, c.ID, livechat.Query{Before: ids[1], Limit: 3})
	if more || len(page) != 1 || page[0].ID != ids[0] {
		t.Fatalf("oldest page wrong: more=%v %v", more, idsOf(page))
	}
	page, more, _ = s.ListMessages(ctx, c.ID, livechat.Query{After: ids[2], Limit: 2})
	if !more || len(page) != 2 || page[0].ID != ids[3] {
		t.Fatalf("after page wrong: more=%v %v", more, idsOf(page))
	}

	if err := s.UpdateDelivery(ctx, ids[0], livechat.StatusFailed, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDelivery(ctx, ids[1], livechat.StatusSent, "om_user_1"); err != nil {
		t.Fatal(err)
	}
	page, _, _ = s.ListMessages(ctx, c.ID, livechat.Query{Limit: 100})
	if page[0].Status != livechat.StatusFailed || page[1].RemoteID != "om_user_1" || page[0].Blocks[0].Text != "hi" {
		t.Fatalf("delivery not recorded: %+v %+v", page[0], page[1])
	}

	r, err := s.RecallMessage(ctx, "om_agent_1")
	if err != nil || !r.Recalled || r.ID != agent.ID {
		t.Fatalf("recall: %+v %v", r, err)
	}
	if _, err := s.RecallMessage(ctx, "om_missing"); !errors.Is(err, livechat.ErrNotFound) {
		t.Fatalf("recall of unknown message: %v", err)
	}
}

func testUnread(t *testing.T, s *sqlstore.Store) {
	ctx := context.Background()
	c, _ := s.EnsureConversation(ctx, "user-u", "U")
	other, _ := s.EnsureConversation(ctx, "user-other", "O")

	a1 := msg(c.ID, livechat.SenderAgent, "om_u1")
	u1 := msg(c.ID, livechat.SenderUser, "")
	a2 := msg(c.ID, livechat.SenderAgent, "om_u2")
	a3 := msg(c.ID, livechat.SenderAgent, "om_u3")
	foreign := msg(other.ID, livechat.SenderAgent, "om_f")
	for _, m := range []*livechat.Message{a1, u1, a2, a3, foreign} {
		if _, err := s.InsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		n, err := s.CountUnread(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 3 {
		t.Fatalf("unread = %d, want 3", n)
	}
	if _, err := s.RecallMessage(ctx, "om_u3"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("recalled replies are not unread: got %d", n)
	}
	_ = s.MarkRead(ctx, c.ID, u1.ID)
	if n := count(); n != 1 {
		t.Fatalf("unread after reading up to u1 = %d, want 1", n)
	}
	_ = s.MarkRead(ctx, c.ID, a1.ID) // backwards: ignored
	if n := count(); n != 1 {
		t.Fatalf("read pointer moved backwards: %d", n)
	}
	_ = s.MarkRead(ctx, c.ID, foreign.ID) // another conversation's message: ignored
	if n := count(); n != 1 {
		t.Fatalf("read pointer accepted a foreign message: %d", n)
	}
	_ = s.MarkRead(ctx, c.ID, a3.ID)
	if n := count(); n != 0 {
		t.Fatalf("unread after reading all = %d", n)
	}
}

func idsOf(ms []*livechat.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := s.GetSetting(ctx, "feishu"); !errors.Is(err, livechat.ErrNotFound) {
				t.Fatalf("missing setting: %v", err)
			}
			for _, v := range []string{"v1", "v2"} {
				if err := s.PutSetting(ctx, "feishu", v); err != nil {
					t.Fatal(err)
				}
				if got, err := s.GetSetting(ctx, "feishu"); err != nil || got != v {
					t.Fatalf("GetSetting = %q, %v; want %q", got, err, v)
				}
			}
		})
	}
}

func TestAgentNumber(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			c1, _ := s.EnsureConversation(ctx, "user-1", "1")
			c2, _ := s.EnsureConversation(ctx, "user-2", "2")
			// ou_b replies first (to user-1), then ou_a (to user-2), then ou_b again.
			for i, r := range []struct{ conv, agent string }{{c1.ID, "ou_b"}, {c2.ID, "ou_a"}, {c2.ID, "ou_b"}} {
				m := msg(r.conv, livechat.SenderAgent, fmt.Sprintf("om_%d", i))
				m.AgentID = r.agent
				if _, err := s.InsertMessage(ctx, m); err != nil {
					t.Fatal(err)
				}
				if _, err := s.InsertMessage(ctx, msg(r.conv, livechat.SenderUser, "")); err != nil {
					t.Fatal(err)
				}
			}
			for agent, want := range map[string]int{"ou_b": 1, "ou_a": 2} {
				if n, err := s.AgentNumber(ctx, agent); err != nil || n != want {
					t.Fatalf("AgentNumber(%s) = %d, %v; want %d", agent, n, err, want)
				}
			}
			if _, err := s.AgentNumber(ctx, "ou_never"); !errors.Is(err, livechat.ErrNotFound) {
				t.Fatalf("unknown agent: %v", err)
			}
			names := s.AgentNames("客服 %02d")
			if got := names(ctx, "ou_a"); got != "客服 02" {
				t.Fatalf("AgentNames = %q", got)
			}
			if got := names(ctx, "ou_never"); got != "" {
				t.Fatalf("unknown agent name = %q", got)
			}
		})
	}
}
