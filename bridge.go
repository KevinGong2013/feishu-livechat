package livechat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	// ErrEmptyMessage: the text was empty after trimming.
	ErrEmptyMessage = errors.New("livechat: empty message")
	// ErrTooLong: the text exceeds Config.MaxTextRunes.
	ErrTooLong = errors.New("livechat: message too long")
	// ErrBadImage: not a PNG, JPEG, GIF or WebP image.
	ErrBadImage = errors.New("livechat: unsupported image")
	// ErrTooLarge: the image exceeds Config.MaxImageBytes.
	ErrTooLarge = errors.New("livechat: image too large")
	// ErrRateLimited: the user sent more than Config.MaxMessagesPerMinute.
	ErrRateLimited = errors.New("livechat: too many messages")
	// ErrUnavailable: the backend has no app connected yet.
	ErrUnavailable = errors.New("livechat: support chat is not set up")
	// ErrUnknownTopic: an agent replied in a topic that belongs to no
	// conversation (a topic someone started by hand, or one replaced after
	// its root card was deleted). The reply is ignored.
	ErrUnknownTopic = errors.New("livechat: topic belongs to no conversation")
)

const (
	defaultMaxTextRunes  = 4000
	defaultMaxImageBytes = 10 << 20 // 飞书's limit for message images
	defaultMaxPerMinute  = 20
	defaultPageSize      = 30
	maxPageSize          = 100
)

// Config wires a Bridge.
type Config struct {
	Store   Store
	Blobs   BlobStore
	Backend Backend

	// OnAgentMessage runs after an agent's reply was stored and pushed to
	// the user's open pages. online reports whether the user has a page
	// open; use it to reach users who are away (Web Push, email, an in-app
	// inbox). Optional.
	OnAgentMessage func(ctx context.Context, userID string, m *Message, online bool)

	Logger        *slog.Logger
	MaxTextRunes  int
	MaxImageBytes int
	// MaxMessagesPerMinute caps what one user can post (default 20; -1
	// for no cap). All users share the bot's sending quota in the group, so
	// one user flooding it would delay everyone else.
	MaxMessagesPerMinute int
}

// Bridge moves messages between users and the topic group.
type Bridge struct {
	cfg   Config
	log   *slog.Logger
	hub   *hub
	locks keyedMutex
	rate  rateLimiter
}

// New returns a Bridge. Store, Blobs and Backend are required.
func New(cfg Config) (*Bridge, error) {
	if cfg.Store == nil || cfg.Blobs == nil || cfg.Backend == nil {
		return nil, errors.New("livechat: Store, Blobs and Backend are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxTextRunes <= 0 {
		cfg.MaxTextRunes = defaultMaxTextRunes
	}
	if cfg.MaxImageBytes <= 0 {
		cfg.MaxImageBytes = defaultMaxImageBytes
	}
	if cfg.MaxMessagesPerMinute == 0 {
		cfg.MaxMessagesPerMinute = defaultMaxPerMinute
	}
	return &Bridge{cfg: cfg, log: cfg.Logger, hub: newHub(), rate: rateLimiter{limit: cfg.MaxMessagesPerMinute, window: time.Minute}}, nil
}

// SendText posts a user's text message into their topic, opening the topic
// first if this is their first message. The returned message carries the
// delivery outcome (StatusSent or StatusFailed); err is only for messages
// that were never accepted (validation, storage).
func (b *Bridge) SendText(ctx context.Context, u User, text string) (*Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrEmptyMessage
	}
	if utf8.RuneCountInString(text) > b.cfg.MaxTextRunes {
		return nil, ErrTooLong
	}
	if !b.Available() {
		return nil, ErrUnavailable
	}
	if !b.rate.allow(u.ID) {
		return nil, ErrRateLimited
	}
	conv, err := b.cfg.Store.EnsureConversation(ctx, u.ID, u.Name)
	if err != nil {
		return nil, err
	}
	m := newMessage(conv.ID, SenderUser, StatusSending)
	m.Blocks = []Block{{Type: BlockText, Text: text}}
	if err := b.accept(ctx, u.ID, m); err != nil {
		return nil, err
	}
	b.deliver(ctx, u, conv, m, func(ctx context.Context, t Topic) (string, error) {
		return b.cfg.Backend.PostText(ctx, t, text, m.ID)
	})
	return m, nil
}

// SendImage posts a user's image into their topic. The format is sniffed
// from the bytes; PNG, JPEG, GIF and WebP are accepted.
func (b *Bridge) SendImage(ctx context.Context, u User, data []byte) (*Message, error) {
	if len(data) > b.cfg.MaxImageBytes {
		return nil, ErrTooLarge
	}
	img, ext, err := inspectImage(data)
	if err != nil {
		return nil, err
	}
	if !b.Available() {
		return nil, ErrUnavailable
	}
	if !b.rate.allow(u.ID) {
		return nil, ErrRateLimited
	}
	conv, err := b.cfg.Store.EnsureConversation(ctx, u.ID, u.Name)
	if err != nil {
		return nil, err
	}
	m := newMessage(conv.ID, SenderUser, StatusSending)
	img.Key = imageKey(conv.ID, m.ID, 0, ext)
	if err := b.cfg.Blobs.Put(ctx, img.Key, img.ContentType, data); err != nil {
		return nil, fmt.Errorf("store image: %w", err)
	}
	m.Blocks = []Block{{Type: BlockImage, Image: &img}}
	if err := b.accept(ctx, u.ID, m); err != nil {
		return nil, err
	}
	b.deliver(ctx, u, conv, m, func(ctx context.Context, t Topic) (string, error) {
		return b.cfg.Backend.PostImage(ctx, t, data, m.ID)
	})
	return m, nil
}

// accept stores a new user message and shows it on the user's other open
// pages right away, before it reaches the group.
func (b *Bridge) accept(ctx context.Context, userID string, m *Message) error {
	if _, err := b.cfg.Store.InsertMessage(ctx, m); err != nil {
		return err
	}
	b.hub.publish(userID, m)
	return nil
}

// deliverTimeout bounds posting one message into the group, card creation
// and one retry included.
const deliverTimeout = 30 * time.Second

func (b *Bridge) deliver(ctx context.Context, u User, conv *Conversation, m *Message, post func(context.Context, Topic) (string, error)) {
	// A user closing the page mid-send must not abort a post that may
	// already have reached the group and leave it recorded as failed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
	defer cancel()
	remoteID, err := b.postToTopic(ctx, u, conv, m.ID, post)
	m.Status, m.RemoteID = StatusSent, remoteID
	if err != nil {
		b.log.Error("livechat: post user message", "conversation", conv.ID, "message", m.ID, "err", err)
		m.Status, m.RemoteID = StatusFailed, ""
	}
	if err := b.cfg.Store.UpdateDelivery(ctx, m.ID, m.Status, m.RemoteID); err != nil {
		b.log.Error("livechat: record delivery", "message", m.ID, "err", err)
	}
	b.hub.publish(u.ID, m)
}

// postToTopic posts into the user's topic. If the post fails because the
// topic's root card was deleted or recalled in the group, it opens a new
// topic and posts there instead — the only way a user ends up with a second
// card.
func (b *Bridge) postToTopic(ctx context.Context, u User, conv *Conversation, msgID string, post func(context.Context, Topic) (string, error)) (string, error) {
	topic, err := b.ensureTopic(ctx, u, conv.ID, "", "r-"+msgID)
	if err != nil {
		return "", err
	}
	remoteID, err := post(ctx, topic)
	if err == nil {
		return remoteID, nil
	}
	gone, gerr := b.cfg.Backend.TopicGone(ctx, topic)
	if gerr != nil || !gone {
		return "", err
	}
	b.log.Warn("livechat: topic root card is gone, opening a new topic", "conversation", conv.ID, "old_root", topic.RootMessageID)
	topic, err = b.ensureTopic(ctx, u, conv.ID, topic.RootMessageID, "g-"+msgID)
	if err != nil {
		return "", err
	}
	return post(ctx, topic)
}

// ensureTopic returns the conversation's topic, creating it when there is
// none yet, when it lives in a group the backend no longer posts to, or
// when its root is replaceRoot. It is serialized per conversation
// in-process; across processes Store.SetTopic's compare-and-set keeps a
// single winner.
func (b *Bridge) ensureTopic(ctx context.Context, u User, convID, replaceRoot, dedupeKey string) (Topic, error) {
	unlock := b.locks.lock(convID)
	defer unlock()

	cur, err := b.cfg.Store.ConversationByID(ctx, convID)
	if err != nil {
		return Topic{}, err
	}
	if cur.RootMessageID != "" && cur.RootMessageID != replaceRoot && cur.TopicGroup == b.cfg.Backend.Group() {
		return cur.Topic(), nil
	}
	topic, err := b.cfg.Backend.CreateTopic(ctx, u, dedupeKey)
	if err != nil {
		return Topic{}, fmt.Errorf("create topic: %w", err)
	}
	ok, err := b.cfg.Store.SetTopic(ctx, convID, cur.RootMessageID, topic)
	if err != nil {
		return Topic{}, err
	}
	if !ok {
		// Another instance opened the topic first; ours is a stray card.
		b.log.Warn("livechat: lost topic race, a duplicate card was posted", "conversation", convID, "root", topic.RootMessageID)
		if cur, err = b.cfg.Store.ConversationByID(ctx, convID); err != nil {
			return Topic{}, err
		}
		return cur.Topic(), nil
	}
	return topic, nil
}

// HandleAgentMessage forwards an agent's reply to the user. The backend
// calls it only for replies that @-ed the bot. Replies in topics that
// belong to no conversation return ErrUnknownTopic and are not
// acknowledged; replies with nothing to forward, or whose images could not
// be fetched, get a failure ack.
func (b *Bridge) HandleAgentMessage(ctx context.Context, am AgentMessage) error {
	conv, err := b.cfg.Store.ConversationByTopic(ctx, am.ThreadID, am.RootMessageID)
	if errors.Is(err, ErrNotFound) {
		return ErrUnknownTopic
	}
	if err != nil {
		return err
	}
	if conv.ThreadID == "" && am.ThreadID != "" && conv.RootMessageID == am.RootMessageID {
		// 飞书 did not return the thread id when the card was posted; learn
		// it from the first reply.
		t := conv.Topic()
		t.ThreadID = am.ThreadID
		_, _ = b.cfg.Store.SetTopic(ctx, conv.ID, conv.RootMessageID, t)
	}

	m := newMessage(conv.ID, SenderAgent, StatusSent)
	m.AgentID, m.RemoteID = am.AgentID, am.RemoteID
	m.Blocks, err = b.agentBlocks(ctx, conv.ID, m.ID, am)
	if err == nil && len(m.Blocks) == 0 {
		err = errors.New("nothing to forward (only text and images are supported)")
	}
	if err != nil {
		b.ack(ctx, am.RemoteID, false)
		return err
	}
	inserted, err := b.cfg.Store.InsertMessage(ctx, m)
	if err != nil {
		b.ack(ctx, am.RemoteID, false)
		return err
	}
	if !inserted {
		return nil // redelivered event, already forwarded
	}
	online := b.hub.online(conv.UserID)
	b.hub.publish(conv.UserID, m)
	if b.cfg.OnAgentMessage != nil {
		b.cfg.OnAgentMessage(ctx, conv.UserID, m, online)
	}
	b.ack(ctx, am.RemoteID, true)
	return nil
}

func (b *Bridge) agentBlocks(ctx context.Context, convID, msgID string, am AgentMessage) ([]Block, error) {
	var blocks []Block
	for i, p := range am.Parts {
		if p.ImageKey == "" {
			if t := strings.TrimSpace(p.Text); t != "" {
				blocks = append(blocks, Block{Type: BlockText, Text: t})
			}
			continue
		}
		data, err := b.cfg.Backend.FetchImage(ctx, am.RemoteID, p.ImageKey)
		if err != nil {
			return nil, fmt.Errorf("fetch image: %w", err)
		}
		img, ext, err := inspectImage(data)
		if err != nil {
			return nil, err
		}
		img.Key = imageKey(convID, msgID, i, ext)
		if err := b.cfg.Blobs.Put(ctx, img.Key, img.ContentType, data); err != nil {
			return nil, fmt.Errorf("store image: %w", err)
		}
		blocks = append(blocks, Block{Type: BlockImage, Image: &img})
	}
	return blocks, nil
}

func (b *Bridge) ack(ctx context.Context, remoteID string, ok bool) {
	if err := b.cfg.Backend.Ack(ctx, remoteID, ok); err != nil {
		b.log.Warn("livechat: ack agent message", "remote_id", remoteID, "ok", ok, "err", err)
	}
}

// HandleRecall hides a message that was recalled in the group from the
// user.
func (b *Bridge) HandleRecall(ctx context.Context, remoteID string) error {
	m, err := b.cfg.Store.RecallMessage(ctx, remoteID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	conv, err := b.cfg.Store.ConversationByID(ctx, m.ConversationID)
	if err != nil {
		return err
	}
	b.hub.publish(conv.UserID, m)
	return nil
}

// History pages through the user's messages; see Store.ListMessages.
func (b *Bridge) History(ctx context.Context, userID string, q Query) ([]*Message, bool, error) {
	if q.Limit <= 0 {
		q.Limit = defaultPageSize
	}
	if q.Limit > maxPageSize {
		q.Limit = maxPageSize
	}
	conv, err := b.cfg.Store.ConversationByUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b.cfg.Store.ListMessages(ctx, conv.ID, q)
}

// MarkRead records that the user has seen everything up to messageID.
func (b *Bridge) MarkRead(ctx context.Context, userID, messageID string) error {
	conv, err := b.cfg.Store.ConversationByUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return b.cfg.Store.MarkRead(ctx, conv.ID, messageID)
}

// Unread counts the agent replies the user has not seen.
func (b *Bridge) Unread(ctx context.Context, userID string) (int, error) {
	conv, err := b.cfg.Store.ConversationByUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return b.cfg.Store.CountUnread(ctx, conv.ID)
}

// Subscribe streams the user's new and changed messages (sent, failed,
// recalled, agent replies) until cancel is called. The channel is closed if
// the reader falls behind; re-sync with History and subscribe again.
func (b *Bridge) Subscribe(userID string) (<-chan *Message, func()) {
	return b.hub.subscribe(userID)
}

// Available reports whether user messages can be delivered: false while the
// backend has no app connected (see Availability).
func (b *Bridge) Available() bool {
	if a, ok := b.cfg.Backend.(Availability); ok {
		return a.Available()
	}
	return true
}

// Online reports whether the user has a page subscribed in this process.
func (b *Bridge) Online(userID string) bool {
	return b.hub.online(userID)
}

func newMessage(convID string, s Sender, st Status) *Message {
	return &Message{
		ID:             uuid.Must(uuid.NewV7()).String(),
		ConversationID: convID,
		Sender:         s,
		Status:         st,
		CreatedAt:      time.Now().UTC().Truncate(time.Millisecond),
	}
}

func imageKey(convID, msgID string, n int, ext string) string {
	return fmt.Sprintf("%s/%s-%d.%s", convID, msgID, n, ext)
}
