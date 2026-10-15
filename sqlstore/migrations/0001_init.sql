-- feishu-livechat schema, version 1. Portable between SQLite and Postgres:
-- times are unix milliseconds, booleans are 0/1 integers. Every table is
-- prefixed livechat_ so it can live in a host application's database.

CREATE TABLE IF NOT EXISTS livechat_conversations (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL UNIQUE,
    user_name       TEXT NOT NULL DEFAULT '',
    -- The topic: root (card) message id and thread id in the 飞书 group.
    -- Empty until the user's first message opens the topic.
    root_message_id TEXT NOT NULL DEFAULT '',
    thread_id       TEXT NOT NULL DEFAULT '',
    last_message_at BIGINT NOT NULL DEFAULT 0,
    last_sender     TEXT NOT NULL DEFAULT '',
    -- Newest message id the user has seen.
    user_read_id    TEXT NOT NULL DEFAULT '',
    created_at      BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS livechat_conversations_root_idx ON livechat_conversations (root_message_id);
CREATE INDEX IF NOT EXISTS livechat_conversations_thread_idx ON livechat_conversations (thread_id);

CREATE TABLE IF NOT EXISTS livechat_messages (
    -- UUIDv7: ordering by id is ordering by time.
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES livechat_conversations (id) ON DELETE CASCADE,
    sender          TEXT NOT NULL,
    -- JSON array of blocks (text runs and images).
    blocks          TEXT NOT NULL,
    status          TEXT NOT NULL,
    recalled        INTEGER NOT NULL DEFAULT 0,
    agent_id        TEXT NOT NULL DEFAULT '',
    -- 飞书 message id; unique so a redelivered event is stored once.
    remote_id       TEXT UNIQUE,
    created_at      BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS livechat_messages_conversation_idx ON livechat_messages (conversation_id, id);
