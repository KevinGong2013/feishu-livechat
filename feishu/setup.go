package feishu

import (
	"context"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// CreateTopicGroup creates a private group in 话题 mode with the bot as its
// owner — which is what lets the bot @所有人 on every root card — adds the
// given members (open_ids) and returns its chat id. Other agents join
// through ShareLink.
func (c *Client) CreateTopicGroup(ctx context.Context, name, description string, memberOpenIDs ...string) (string, error) {
	body := larkim.NewCreateChatReqBodyBuilder().
		Name(name).
		Description(description).
		ChatMode("group").
		ChatType("private").
		GroupMessageType(larkim.CreateChatGroupMessageTypeThread)
	if len(memberOpenIDs) > 0 {
		body.UserIdList(memberOpenIDs)
	}
	resp, err := c.api.Im.Chat.Create(ctx, larkim.NewCreateChatReqBuilder().
		UserIdType("open_id").
		Body(body.Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("create group", resp.CodeError)
	}
	return deref(resp.Data.ChatId), nil
}

// ShareLink returns a link agents open to join the group. validity is
// "week", "year" or "permanently".
func (c *Client) ShareLink(ctx context.Context, chatID, validity string) (string, error) {
	resp, err := c.api.Im.Chat.Link(ctx, larkim.NewLinkChatReqBuilder().
		ChatId(chatID).
		Body(larkim.NewLinkChatReqBodyBuilder().ValidityPeriod(validity).Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("share link", resp.CodeError)
	}
	return deref(resp.Data.ShareLink), nil
}
