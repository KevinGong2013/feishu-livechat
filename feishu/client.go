// Package feishu connects livechat to a 飞书 (or Lark) 话题群 through a
// self-built app (企业自建应用): Client is the livechat.Backend, Listen
// receives agent replies over the long connection (no public callback URL
// needed), and CreateTopicGroup / ShareLink set the group up.
//
// The app needs these permissions: 获取与发送单聊、群组消息 (im:message),
// 以应用的身份发消息 (im:message:send_as_bot), 接收群聊中@机器人消息事件
// (im:message.group_at_msg:readonly), 获取与上传图片或文件资源 (im:resource),
// and for setup 获取与更新群组信息 (im:chat). Subscribe to the events
// 接收消息 (im.message.receive_v1) and 消息被撤回 (im.message.recalled_v1)
// with 使用长连接接收事件.
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// Config configures a Client.
type Config struct {
	AppID     string
	AppSecret string
	// ChatID is the 话题群 (oc_...). Messages from other chats are ignored.
	ChatID string
	// Lark selects open.larksuite.com instead of open.feishu.cn.
	Lark bool
	// OpenBaseURL overrides the Open API host entirely (tests, private
	// deployments).
	OpenBaseURL string
	// CardHint is the root card's footnote; DefaultCardHint if empty.
	CardHint string
	// DeliveredEmoji and FailedEmoji are the 飞书 emoji_type reactions put
	// on an agent's reply once it was forwarded, or could not be. Defaults
	// "DONE" and "CrossMark".
	DeliveredEmoji string
	FailedEmoji    string
	// ReadEmoji is the extra reaction put on an agent's reply once the web
	// user has read it. Default "Get". Empty means Get; it does not disable
	// the reaction. DeliveredEmoji is unchanged.
	ReadEmoji string
	// MaxImageBytes caps images downloaded from agents. Default 20 MiB.
	MaxImageBytes int64
	Logger        *slog.Logger
}

// Client talks to the 飞书 Open API as the bot.
type Client struct {
	cfg       Config
	api       *lark.Client
	log       *slog.Logger
	botOpenID string
	serial    serial
	// onState, when set, hears the long connection's state changes
	// ("connecting", "connected", "disconnected") and errors.
	onState func(state string, err error)
}

var _ livechat.Backend = (*Client)(nil)
var _ livechat.Reactor = (*Client)(nil)

// New returns a Client. Call Init (or Listen, which calls it) before
// handling events.
func New(cfg Config) (*Client, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, errors.New("feishu: AppID and AppSecret are required")
	}
	if cfg.CardHint == "" {
		cfg.CardHint = DefaultCardHint
	}
	if cfg.DeliveredEmoji == "" {
		cfg.DeliveredEmoji = "DONE"
	}
	if cfg.FailedEmoji == "" {
		cfg.FailedEmoji = "CrossMark"
	}
	if cfg.ReadEmoji == "" {
		cfg.ReadEmoji = "Get"
	}
	if cfg.MaxImageBytes <= 0 {
		cfg.MaxImageBytes = 20 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	opts := []lark.ClientOptionFunc{lark.WithLogLevel(larkcore.LogLevelWarn)}
	switch {
	case cfg.OpenBaseURL != "":
		opts = append(opts, lark.WithOpenBaseUrl(cfg.OpenBaseURL))
	case cfg.Lark:
		opts = append(opts, lark.WithOpenBaseUrl(lark.LarkBaseUrl))
	}
	return &Client{
		cfg: cfg,
		api: lark.NewClient(cfg.AppID, cfg.AppSecret, opts...),
		log: cfg.Logger,
	}, nil
}

// BotInfo is the app's bot as 飞书 reports it.
type BotInfo struct {
	OpenID  string `json:"open_id"`
	AppName string `json:"app_name"`
}

// Bot fetches the bot's identity. It also proves the credentials work and
// the bot capability (机器人能力) is enabled.
func (c *Client) Bot(ctx context.Context) (BotInfo, error) {
	resp, err := c.api.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return BotInfo{}, fmt.Errorf("feishu: bot info: %w", err)
	}
	var r struct {
		Code int     `json:"code"`
		Msg  string  `json:"msg"`
		Bot  BotInfo `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &r); err != nil {
		return BotInfo{}, fmt.Errorf("feishu: bot info: %w", err)
	}
	if r.Code != 0 || r.Bot.OpenID == "" {
		return BotInfo{}, fmt.Errorf("feishu: bot info: %s (code %d); is 机器人能力 enabled for the app?", r.Msg, r.Code)
	}
	return r.Bot, nil
}

// Init learns the bot's open_id, which is how replies that @ the bot are
// recognized.
func (c *Client) Init(ctx context.Context) error {
	bot, err := c.Bot(ctx)
	if err != nil {
		return err
	}
	c.botOpenID = bot.OpenID
	return nil
}

// Group is the 话题群 topics are created in.
func (c *Client) Group() string { return c.cfg.ChatID }

func apiErr(op string, ce larkcore.CodeError) error {
	return fmt.Errorf("feishu: %s: %s (code %d)", op, ce.Msg, ce.Code)
}

func (c *Client) CreateTopic(ctx context.Context, u livechat.User, dedupeKey string) (livechat.Topic, error) {
	if c.cfg.ChatID == "" {
		return livechat.Topic{}, errors.New("feishu: ChatID is not configured")
	}
	card, err := rootCard(u, c.cfg.CardHint)
	if err != nil {
		return livechat.Topic{}, err
	}
	resp, err := c.api.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(c.cfg.ChatID).
			MsgType(larkim.MsgTypeInteractive).
			Content(string(card)).
			Uuid(dedupeKey).
			Build()).
		Build())
	if err != nil {
		return livechat.Topic{}, err
	}
	if !resp.Success() {
		return livechat.Topic{}, apiErr("post root card", resp.CodeError)
	}
	return livechat.Topic{Group: c.cfg.ChatID, RootMessageID: deref(resp.Data.MessageId), ThreadID: deref(resp.Data.ThreadId)}, nil
}

func (c *Client) PostText(ctx context.Context, t livechat.Topic, text, dedupeKey string) (string, error) {
	content, err := postContent(text)
	if err != nil {
		return "", err
	}
	return c.reply(ctx, t, larkim.MsgTypePost, string(content), dedupeKey)
}

func (c *Client) PostImage(ctx context.Context, t livechat.Topic, data []byte, dedupeKey string) (string, error) {
	up, err := c.api.Im.Image.Create(ctx, larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType(larkim.CreateImageImageTypeMessage).
			Image(bytes.NewReader(data)).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !up.Success() {
		return "", apiErr("upload image", up.CodeError)
	}
	content, err := json.Marshal(map[string]string{"image_key": deref(up.Data.ImageKey)})
	if err != nil {
		return "", err
	}
	return c.reply(ctx, t, larkim.MsgTypeImage, string(content), dedupeKey)
}

func (c *Client) reply(ctx context.Context, t livechat.Topic, msgType, content, dedupeKey string) (string, error) {
	resp, err := c.api.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(t.RootMessageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(content).
			ReplyInThread(true).
			Uuid(dedupeKey).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("reply in topic", resp.CodeError)
	}
	return deref(resp.Data.MessageId), nil
}

func (c *Client) TopicGone(ctx context.Context, t livechat.Topic) (bool, error) {
	resp, err := c.api.Im.Message.Get(ctx, larkim.NewGetMessageReqBuilder().MessageId(t.RootMessageID).Build())
	if err != nil {
		return false, err
	}
	if !resp.Success() {
		return false, apiErr("get root card", resp.CodeError)
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 {
		return true, nil
	}
	item := resp.Data.Items[0]
	return item.Deleted != nil && *item.Deleted, nil
}

func (c *Client) FetchImage(ctx context.Context, remoteID, imageKey string) ([]byte, error) {
	resp, err := c.api.Im.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(remoteID).
		FileKey(imageKey).
		Type("image").
		Build())
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, apiErr("download image", resp.CodeError)
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, c.cfg.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > c.cfg.MaxImageBytes {
		return nil, livechat.ErrTooLarge
	}
	return data, nil
}

// ReadEmoji is the reaction added when the user has read an agent's reply.
func (c *Client) ReadEmoji() string { return c.cfg.ReadEmoji }

// React adds one emoji reaction to a message. Ack uses it for the delivery
// receipt; read receipts use it with ReadEmoji.
func (c *Client) React(ctx context.Context, remoteID, emoji string) error {
	resp, err := c.api.Im.MessageReaction.Create(ctx, larkim.NewCreateMessageReactionReqBuilder().
		MessageId(remoteID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		// 231003: message id is wrong, or the message was recalled or deleted.
		// 230110: the message was deleted. Neither can succeed later.
		if resp.CodeError.Code == 231003 || resp.CodeError.Code == 230110 {
			return fmt.Errorf("feishu: add reaction %s: %s (code %d): %w", emoji, resp.CodeError.Msg, resp.CodeError.Code, livechat.ErrMessageGone)
		}
		return apiErr("add reaction "+emoji, resp.CodeError)
	}
	return nil
}

func (c *Client) Ack(ctx context.Context, remoteID string, ok bool) error {
	emoji := c.cfg.DeliveredEmoji
	if !ok {
		emoji = c.cfg.FailedEmoji
	}
	return c.React(ctx, remoteID, emoji)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
