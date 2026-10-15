package feishu

import (
	"context"
	"errors"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// Sink receives what agents do in the group. *livechat.Bridge implements
// it.
type Sink interface {
	HandleAgentMessage(ctx context.Context, m livechat.AgentMessage) error
	HandleRecall(ctx context.Context, remoteID string) error
}

// handleTimeout bounds handling one event (image downloads included).
const handleTimeout = 2 * time.Minute

// Listen receives events over 飞书's long connection and feeds sink until
// ctx is done. Run it in exactly the processes that should handle agent
// replies; with several, 飞书 delivers each event to only one of them.
//
// Events are acknowledged at once and handled in the background (飞书
// redelivers events not acknowledged within 3 seconds), one at a time per
// topic so a user sees replies in the order agents sent them.
func (c *Client) Listen(ctx context.Context, sink Sink) error {
	if c.cfg.ChatID == "" {
		return errors.New("feishu: ChatID is not configured")
	}
	if c.botOpenID == "" {
		if err := c.Init(ctx); err != nil {
			return err
		}
	}
	d := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			am, ok := c.agentMessage(ev)
			if !ok {
				return nil
			}
			c.serial.run(topicKey(am), func() {
				ctx, cancel := context.WithTimeout(context.Background(), handleTimeout)
				defer cancel()
				err := sink.HandleAgentMessage(ctx, am)
				switch {
				case errors.Is(err, livechat.ErrUnknownTopic):
					c.log.Debug("feishu: reply in a topic that is not a user conversation", "message", am.RemoteID)
				case err != nil:
					c.log.Error("feishu: forward agent reply", "message", am.RemoteID, "err", err)
				}
			})
			return nil
		}).
		OnP2MessageRecalledV1(func(_ context.Context, ev *larkim.P2MessageRecalledV1) error {
			if ev == nil || ev.Event == nil || deref(ev.Event.ChatId) != c.cfg.ChatID {
				return nil
			}
			id := deref(ev.Event.MessageId)
			c.serial.run("recall", func() {
				ctx, cancel := context.WithTimeout(context.Background(), handleTimeout)
				defer cancel()
				if err := sink.HandleRecall(ctx, id); err != nil {
					c.log.Error("feishu: recall", "message", id, "err", err)
				}
			})
			return nil
		})

	opts := []larkws.ClientOption{
		larkws.WithEventHandler(d),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	}
	switch {
	case c.cfg.OpenBaseURL != "":
		opts = append(opts, larkws.WithDomain(c.cfg.OpenBaseURL))
	case c.cfg.Lark:
		opts = append(opts, larkws.WithDomain("https://open.larksuite.com"))
	}
	ws := larkws.NewClient(c.cfg.AppID, c.cfg.AppSecret, opts...)
	if c.onState != nil {
		ws.SetOnReady(func() { c.onState("connected", nil) })
		ws.SetOnReconnecting(func() { c.onState("connecting", nil) })
		ws.SetOnReconnected(func() { c.onState("connected", nil) })
		ws.SetOnDisconnected(func() { c.onState("disconnected", nil) })
		ws.SetOnError(func(err error) { c.onState("", err) })
	}
	return ws.Start(ctx)
}

// agentMessage turns a message event into an AgentMessage when it is an
// agent's message in our group that @-s the bot. Messages without the
// mention are the agents talking among themselves and are ignored, as is
// everything the bot posts itself.
func (c *Client) agentMessage(ev *larkim.P2MessageReceiveV1) (livechat.AgentMessage, bool) {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil || ev.Event.Sender == nil {
		return livechat.AgentMessage{}, false
	}
	msg, sender := ev.Event.Message, ev.Event.Sender
	if deref(msg.ChatId) != c.cfg.ChatID || deref(sender.SenderType) != "user" {
		return livechat.AgentMessage{}, false
	}
	mentioned := false
	var keys []string
	for _, m := range msg.Mentions {
		if m == nil {
			continue
		}
		keys = append(keys, deref(m.Key))
		if m.Id != nil && c.botOpenID != "" && deref(m.Id.OpenId) == c.botOpenID {
			mentioned = true
		}
	}
	if !mentioned {
		return livechat.AgentMessage{}, false
	}
	am := livechat.AgentMessage{
		RemoteID:      deref(msg.MessageId),
		ThreadID:      deref(msg.ThreadId),
		RootMessageID: deref(msg.RootId),
		Parts:         Parse(deref(msg.MessageType), deref(msg.Content), keys),
	}
	if sender.SenderId != nil {
		am.AgentID = deref(sender.SenderId.OpenId)
	}
	return am, true
}

func topicKey(am livechat.AgentMessage) string {
	switch {
	case am.RootMessageID != "":
		return am.RootMessageID
	case am.ThreadID != "":
		return am.ThreadID
	}
	return am.RemoteID
}

// serial runs functions one at a time per key, in submission order, each
// key on its own goroutine that exits when the key's queue drains.
type serial struct {
	mu     sync.Mutex
	queues map[string][]func()
}

func (s *serial) run(key string, fn func()) {
	s.mu.Lock()
	if s.queues == nil {
		s.queues = make(map[string][]func())
	}
	q, running := s.queues[key]
	s.queues[key] = append(q, fn)
	s.mu.Unlock()
	if running {
		return
	}
	go func() {
		for {
			s.mu.Lock()
			q := s.queues[key]
			if len(q) == 0 {
				delete(s.queues, key)
				s.mu.Unlock()
				return
			}
			next := q[0]
			s.queues[key] = q[1:]
			s.mu.Unlock()
			next()
		}
	}()
}
