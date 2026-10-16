// Package sqlstore implements livechat.Store on database/sql, for SQLite
// (modernc.org/sqlite, pure Go) and Postgres (e.g. pgx's stdlib driver).
//
// The schema lives in migrations/*.sql and is portable between the two.
// Standalone deployments call Migrate; an application that manages its own
// migrations can copy those files into its migration tool instead.
package sqlstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// Dialect selects the SQL placeholder style.
type Dialect int

const (
	SQLite   Dialect = iota // ? placeholders
	Postgres                // $1 placeholders
)

// Store is a livechat.Store backed by a SQL database.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

var _ livechat.Store = (*Store)(nil)

// New wraps db. It does not touch the schema; see Migrate.
func New(db *sql.DB, d Dialect) *Store {
	return &Store{db: db, dialect: d}
}

// OpenSQLite opens (creating if needed) a SQLite database file with WAL,
// a busy timeout and foreign keys on. It keeps a single connection: chat
// traffic is light, and it rules out "database is locked" errors. A driver
// registered as "sqlite" must be linked in, e.g. import _ "modernc.org/sqlite"
// (this package does not import one, so Postgres users don't carry it).
func OpenSQLite(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies the migrations that have not run yet, recording them in
// livechat_schema_migrations.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS livechat_schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at BIGINT NOT NULL
	)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad version prefix", base)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM livechat_schema_migrations WHERE version = ?`), version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if err := s.apply(ctx, version, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, version int, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range splitStatements(body) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO livechat_schema_migrations (version, applied_at) VALUES (?, ?)`), version, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// splitStatements splits a migration on semicolons after dropping comment
// lines (comments may contain semicolons; statements may not contain
// string literals with them).
func splitStatements(body string) []string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	var out []string
	for _, stmt := range strings.Split(b.String(), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// q rewrites ? placeholders for the dialect.
func (s *Store) q(query string) string {
	if s.dialect != Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

const convColumns = `id, user_id, user_name, topic_group, root_message_id, thread_id, last_message_at, last_sender, user_read_id, created_at`

func scanConversation(row interface{ Scan(...any) error }) (*livechat.Conversation, error) {
	var c livechat.Conversation
	var lastAt, createdAt int64
	var lastSender string
	err := row.Scan(&c.ID, &c.UserID, &c.UserName, &c.TopicGroup, &c.RootMessageID, &c.ThreadID, &lastAt, &lastSender, &c.UserReadID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, livechat.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if lastAt > 0 {
		c.LastMessageAt = time.UnixMilli(lastAt).UTC()
	}
	c.LastSender = livechat.Sender(lastSender)
	c.CreatedAt = time.UnixMilli(createdAt).UTC()
	return &c, nil
}

func (s *Store) EnsureConversation(ctx context.Context, userID, userName string) (*livechat.Conversation, error) {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO livechat_conversations (id, user_id, user_name, created_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (user_id) DO NOTHING`),
		uuid.Must(uuid.NewV7()).String(), userID, userName, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	return s.ConversationByUser(ctx, userID)
}

func (s *Store) ConversationByID(ctx context.Context, id string) (*livechat.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, s.q(`SELECT `+convColumns+` FROM livechat_conversations WHERE id = ?`), id))
}

func (s *Store) ConversationByUser(ctx context.Context, userID string) (*livechat.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, s.q(`SELECT `+convColumns+` FROM livechat_conversations WHERE user_id = ?`), userID))
}

func (s *Store) ConversationByTopic(ctx context.Context, threadID, rootMessageID string) (*livechat.Conversation, error) {
	if threadID != "" {
		c, err := scanConversation(s.db.QueryRowContext(ctx, s.q(`SELECT `+convColumns+` FROM livechat_conversations WHERE thread_id = ?`), threadID))
		if !errors.Is(err, livechat.ErrNotFound) {
			return c, err
		}
	}
	if rootMessageID != "" {
		return scanConversation(s.db.QueryRowContext(ctx, s.q(`SELECT `+convColumns+` FROM livechat_conversations WHERE root_message_id = ?`), rootMessageID))
	}
	return nil, livechat.ErrNotFound
}

func (s *Store) SetTopic(ctx context.Context, conversationID, oldRoot string, t livechat.Topic) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE livechat_conversations SET topic_group = ?, root_message_id = ?, thread_id = ?
		WHERE id = ? AND root_message_id = ?`), t.Group, t.RootMessageID, t.ThreadID, conversationID, oldRoot)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

const msgColumns = `id, conversation_id, sender, blocks, status, recalled, agent_id, remote_id, created_at`

func scanMessage(row interface{ Scan(...any) error }) (*livechat.Message, error) {
	var m livechat.Message
	var sender, status, blocks string
	var recalled int
	var remoteID sql.NullString
	var createdAt int64
	err := row.Scan(&m.ID, &m.ConversationID, &sender, &blocks, &status, &recalled, &m.AgentID, &remoteID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, livechat.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(blocks), &m.Blocks); err != nil {
		return nil, fmt.Errorf("message %s: decode blocks: %w", m.ID, err)
	}
	m.Sender, m.Status = livechat.Sender(sender), livechat.Status(status)
	m.Recalled = recalled != 0
	m.RemoteID = remoteID.String
	m.CreatedAt = time.UnixMilli(createdAt).UTC()
	return &m, nil
}

func nullable(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func (s *Store) InsertMessage(ctx context.Context, m *livechat.Message) (bool, error) {
	blocks, err := json.Marshal(m.Blocks)
	if err != nil {
		return false, err
	}
	if m.Blocks == nil {
		blocks = []byte("[]")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, s.q(`INSERT INTO livechat_messages (`+msgColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`),
		m.ID, m.ConversationID, string(m.Sender), string(blocks), string(m.Status), boolInt(m.Recalled),
		m.AgentID, nullable(m.RemoteID), m.CreatedAt.UnixMilli())
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, s.q(`UPDATE livechat_conversations SET last_message_at = ?, last_sender = ? WHERE id = ?`),
		m.CreatedAt.UnixMilli(), string(m.Sender), m.ConversationID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) UpdateDelivery(ctx context.Context, id string, status livechat.Status, remoteID string) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE livechat_messages SET status = ?, remote_id = ? WHERE id = ?`),
		string(status), nullable(remoteID), id)
	return err
}

func (s *Store) RecallMessage(ctx context.Context, remoteID string) (*livechat.Message, error) {
	if remoteID == "" {
		return nil, livechat.ErrNotFound
	}
	if _, err := s.db.ExecContext(ctx, s.q(`UPDATE livechat_messages SET recalled = 1 WHERE remote_id = ?`), remoteID); err != nil {
		return nil, err
	}
	return scanMessage(s.db.QueryRowContext(ctx, s.q(`SELECT `+msgColumns+` FROM livechat_messages WHERE remote_id = ?`), remoteID))
}

func (s *Store) ListMessages(ctx context.Context, conversationID string, q livechat.Query) ([]*livechat.Message, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 30
	}
	var query string
	args := []any{conversationID}
	asc := false
	switch {
	case q.After != "":
		query = `SELECT ` + msgColumns + ` FROM livechat_messages WHERE conversation_id = ? AND id > ? ORDER BY id ASC LIMIT ?`
		args = append(args, q.After)
		asc = true
	case q.Before != "":
		query = `SELECT ` + msgColumns + ` FROM livechat_messages WHERE conversation_id = ? AND id < ? ORDER BY id DESC LIMIT ?`
		args = append(args, q.Before)
	default:
		query = `SELECT ` + msgColumns + ` FROM livechat_messages WHERE conversation_id = ? ORDER BY id DESC LIMIT ?`
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []*livechat.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	if !asc {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, more, nil
}

func (s *Store) MarkRead(ctx context.Context, conversationID, messageID string) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE livechat_conversations SET user_read_id = ?
		WHERE id = ? AND user_read_id < ?
		AND EXISTS (SELECT 1 FROM livechat_messages WHERE id = ? AND conversation_id = ?)`),
		messageID, conversationID, messageID, messageID, conversationID)
	return err
}

func (s *Store) CountUnread(ctx context.Context, conversationID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM livechat_messages m
		JOIN livechat_conversations c ON c.id = m.conversation_id
		WHERE m.conversation_id = ? AND m.sender = 'agent' AND m.recalled = 0 AND m.id > c.user_read_id`),
		conversationID).Scan(&n)
	return n, err
}

// AgentNumber is the agent's place, from 1, in the order agents first
// replied to anyone, or livechat.ErrNotFound if they never have. Replies
// are never deleted, so an agent keeps their number.
func (s *Store) AgentNumber(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM (
		SELECT MIN(id) AS first FROM livechat_messages WHERE sender = 'agent' AND agent_id <> '' GROUP BY agent_id
	) f WHERE f.first <= (SELECT MIN(id) FROM livechat_messages WHERE sender = 'agent' AND agent_id = ?)`),
		agentID).Scan(&n)
	if err == nil && n == 0 {
		err = livechat.ErrNotFound
	}
	return n, err
}

// AgentNames returns a livechat.HandlerOptions.AgentName that shows each
// agent by number, formatted with format (e.g. "客服 %02d"), so users can
// tell agents apart without learning who they are. Numbers are cached.
func (s *Store) AgentNames(format string) func(ctx context.Context, agentID string) string {
	var mu sync.Mutex
	cache := map[string]string{}
	return func(ctx context.Context, agentID string) string {
		mu.Lock()
		name, ok := cache[agentID]
		mu.Unlock()
		if ok {
			return name
		}
		n, err := s.AgentNumber(ctx, agentID)
		if err != nil {
			return ""
		}
		name = fmt.Sprintf(format, n)
		mu.Lock()
		cache[agentID] = name
		mu.Unlock()
		return name
	}
}

// GetSetting returns a value stored with PutSetting, or
// livechat.ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT value FROM livechat_settings WHERE key = ?`), key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", livechat.ErrNotFound
	}
	return v, err
}

// PutSetting stores value under key, replacing any previous value.
func (s *Store) PutSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO livechat_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`),
		key, value, time.Now().UnixMilli())
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
