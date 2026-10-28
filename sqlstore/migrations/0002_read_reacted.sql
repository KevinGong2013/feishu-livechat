-- Claim flag for the "user has read" reaction.
-- 0: not claimed. 1: claimed (reaction sent, or the remote message is gone
-- and will not be retried). A failed attempt is put back to 0.
-- IF NOT EXISTS so a host that already added this column (apkgo-cloud
-- migration 062, same name and type) can still apply this migration.
-- Postgres runs the statement as written. SQLite has no ADD COLUMN
-- IF NOT EXISTS; sqlstore.Migrate rewrites it and ignores a duplicate column.

ALTER TABLE livechat_messages ADD COLUMN IF NOT EXISTS read_reacted SMALLINT NOT NULL DEFAULT 0;
