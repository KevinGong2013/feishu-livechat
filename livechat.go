// Package livechat bridges a website's live chat to a 飞书 (Feishu/Lark)
// 话题群: every end user gets one topic in the group, opened by a card that
// @所有人, and everything after that is appended to the same topic.
//
//   - User → 飞书: what the user types (text or an image) is posted into their
//     topic by the bot.
//   - 飞书 → user: only replies that @ the bot are forwarded; everything else in
//     the topic is the agents talking among themselves. Forwarded replies get a
//     ✅ reaction, ones that could not be forwarded get ❌.
//
// The package itself only knows conversations and messages. Where they are
// stored (Store), where images live (BlobStore) and how the topic group is
// reached (Backend, implemented by package feishu) are interfaces, so the
// bridge can be embedded into an existing Go service or run standalone
// (cmd/feishu-livechat).
package livechat

import (
	"errors"
	"time"
)

// ErrNotFound is returned by a Store when the conversation or message does
// not exist.
var ErrNotFound = errors.New("livechat: not found")

// User is the end user chatting from the website, as the host application
// knows them. Fields and Links are shown on the topic's root card; the card
// is posted once and never updated, so put stable facts there (id, email,
// signup date) and link to a page with live data for the rest.
type User struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Fields []Field `json:"fields,omitempty"`
	Links  []Link  `json:"links,omitempty"`
}

// Field is one label/value line on the root card.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Link is a button on the root card that opens URL.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Topic identifies a user's topic in the group: the root (card) message and,
// once known, the thread id 飞书 assigned to it.
type Topic struct {
	RootMessageID string
	ThreadID      string
}

// Conversation is the one-per-user chat. RootMessageID is empty until the
// user's first message creates the topic.
type Conversation struct {
	ID            string
	UserID        string
	UserName      string
	RootMessageID string
	ThreadID      string
	LastMessageAt time.Time
	LastSender    Sender
	// UserReadID is the newest message id the user has seen; agent messages
	// after it are unread.
	UserReadID string
	CreatedAt  time.Time
}

// Topic returns the conversation's topic.
func (c *Conversation) Topic() Topic {
	return Topic{RootMessageID: c.RootMessageID, ThreadID: c.ThreadID}
}

// Sender says who wrote a message.
type Sender string

const (
	SenderUser  Sender = "user"
	SenderAgent Sender = "agent"
)

// Status is the delivery state of a user message into the topic. Agent
// messages are always StatusSent.
type Status string

const (
	StatusSending Status = "sending"
	StatusSent    Status = "sent"
	StatusFailed  Status = "failed"
)

// Message is one chat message. IDs are UUIDv7 strings, so ordering by ID is
// ordering by creation time.
type Message struct {
	ID             string
	ConversationID string
	Sender         Sender
	Blocks         []Block
	Status         Status
	Recalled       bool
	// AgentID is the agent's 飞书 open_id (agent messages only).
	AgentID string
	// RemoteID is the 飞书 message id: the posted message for user messages
	// (empty until sent), the agent's reply for agent messages.
	RemoteID  string
	CreatedAt time.Time
}

// BlockType is the kind of a Block.
type BlockType string

const (
	BlockText  BlockType = "text"
	BlockImage BlockType = "image"
)

// Block is one piece of a message: a run of text or an image. Messages keep
// the order the parts were written in, so a reply like "看这里 [截图] 再点保存"
// is three blocks.
type Block struct {
	Type  BlockType `json:"type"`
	Text  string    `json:"text,omitempty"`
	Image *Image    `json:"image,omitempty"`
}

// Image is an image stored in the BlobStore under Key.
type Image struct {
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

// Query pages through a conversation's messages. With neither Before nor
// After set it returns the newest Limit messages.
type Query struct {
	Before string
	After  string
	Limit  int
}
