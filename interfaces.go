package livechat

import (
	"context"
	"io"
	"time"
)

// Store persists conversations and messages. sqlstore implements it for
// SQLite and Postgres.
type Store interface {
	// EnsureConversation returns the user's conversation, creating an empty
	// one (no topic yet) if there is none.
	EnsureConversation(ctx context.Context, userID, userName string) (*Conversation, error)
	ConversationByID(ctx context.Context, id string) (*Conversation, error)
	ConversationByUser(ctx context.Context, userID string) (*Conversation, error)
	// ConversationByTopic finds the conversation whose topic has threadID or
	// rootMessageID (either may be empty).
	ConversationByTopic(ctx context.Context, threadID, rootMessageID string) (*Conversation, error)
	// SetTopic replaces the conversation's topic only if its root message is
	// still oldRoot, and reports whether it did. This keeps two concurrent
	// first messages from both creating a topic.
	SetTopic(ctx context.Context, conversationID, oldRoot string, topic Topic) (bool, error)

	// InsertMessage stores m and bumps the conversation's last-message
	// fields. It returns false, without error, if a message with the same
	// non-empty RemoteID already exists (飞书 redelivered the event).
	InsertMessage(ctx context.Context, m *Message) (bool, error)
	// UpdateDelivery records the outcome of posting a user message.
	UpdateDelivery(ctx context.Context, id string, status Status, remoteID string) error
	// RecallMessage marks the message with remoteID as recalled and returns
	// it.
	RecallMessage(ctx context.Context, remoteID string) (*Message, error)
	// ListMessages returns messages in ascending ID order, plus whether more
	// exist beyond the page in the direction being paged (older for Before
	// and for the default newest page, newer for After).
	ListMessages(ctx context.Context, conversationID string, q Query) ([]*Message, bool, error)
	// MarkRead moves the user's read pointer forward to messageID (never
	// back).
	MarkRead(ctx context.Context, conversationID, messageID string) error
	// CountUnread counts agent messages after the read pointer that were not
	// recalled.
	CountUnread(ctx context.Context, conversationID string) (int, error)
}

// BlobStore keeps the images exchanged in chats. Keys look like
// "<conversation id>/<message id>-<n>.<ext>".
type BlobStore interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// URLSigner is an optional BlobStore capability: when the store can hand
// out short-lived public URLs (an object storage signed link), the HTTP
// handler gives those to the browser instead of serving the bytes itself.
type URLSigner interface {
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Backend is the topic group. Package feishu implements it.
type Backend interface {
	// Group is the group new topics are created in. A conversation whose
	// topic is in another group gets a new topic.
	Group() string
	// CreateTopic posts the user's root card into the group and returns the
	// new topic. dedupeKey makes retries of the same attempt idempotent.
	CreateTopic(ctx context.Context, user User, dedupeKey string) (Topic, error)
	// PostText and PostImage post a user message into the topic and return
	// the posted message's id.
	PostText(ctx context.Context, topic Topic, text, dedupeKey string) (string, error)
	PostImage(ctx context.Context, topic Topic, data []byte, dedupeKey string) (string, error)
	// TopicGone reports whether the topic's root message was deleted or
	// recalled, in which case the bridge opens a new topic for the user.
	TopicGone(ctx context.Context, topic Topic) (bool, error)
	// FetchImage downloads an image an agent sent in message remoteID.
	FetchImage(ctx context.Context, remoteID, imageKey string) ([]byte, error)
	// Ack marks an agent's message as forwarded (ok) or not (!ok).
	Ack(ctx context.Context, remoteID string, ok bool) error
}

// Availability is an optional Backend capability: a backend that can be
// unconfigured (no app connected yet) reports it, and the bridge then
// refuses user messages with ErrUnavailable instead of accepting messages
// it cannot deliver.
type Availability interface {
	Available() bool
}

// AgentMessage is an agent's reply that @-ed the bot, already reduced to
// the parts that get forwarded.
type AgentMessage struct {
	RemoteID      string
	ThreadID      string
	RootMessageID string
	AgentID       string
	Parts         []Part
}

// Part is a run of text or an image (by the backend's image key), in the
// order the agent wrote them.
type Part struct {
	Text     string
	ImageKey string
}
