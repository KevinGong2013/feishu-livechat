package feishu

import (
	"context"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
)

// Scopes and Events are what the bridge needs from the app. None of them
// is a sensitive scope: the bot only receives messages that @ it, so it
// never needs to read the whole group.
var (
	Scopes = []string{
		"im:message",                       // 获取与发送单聊、群组消息
		"im:message:send_as_bot",           // 以应用的身份发消息
		"im:message.group_at_msg:readonly", // 接收群聊中 @机器人消息事件
		"im:resource",                      // 获取与上传图片或文件资源
		"im:chat",                          // 获取与更新群组信息（建群、入群链接）
	}
	Events = []string{
		"im.message.receive_v1",  // 接收消息
		"im.message.recalled_v1", // 消息被撤回
	}
)

// RegisterOptions configures RegisterApp.
type RegisterOptions struct {
	// Name and Description pre-fill the app creation page; the user can
	// still change them there.
	Name        string
	Description string
	// OnURL receives the link to open in 飞书 (scan it as a QR code, or tap
	// it inside 飞书) and how many seconds it stays valid.
	OnURL func(url string, expiresIn int)
	// OnWaiting is called on every poll while the user has not confirmed
	// yet. Optional.
	OnWaiting func()
	// AccountsURL overrides https://accounts.feishu.cn (tests).
	AccountsURL string
}

// Registration is a newly created app.
type Registration struct {
	AppID     string
	AppSecret string
	// CreatorOpenID is the open_id (under this app) of the person who
	// scanned and confirmed, so they can be put into the group right away.
	CreatorOpenID string
	// Lark is true when the creator's tenant is on Lark (larksuite.com)
	// rather than 飞书.
	Lark bool
}

// RegisterApp creates a self-built app by 飞书's scan-to-create flow (the
// OAuth device flow lark-cli uses): the user opens the link in 飞书, sees a
// creation page with the name, the scopes in Scopes and the events in
// Events already filled in, and confirms. No developer console visit is
// needed. It blocks until the user confirms, declines, or the link expires.
func RegisterApp(ctx context.Context, o RegisterOptions) (Registration, error) {
	res, err := registration.RegisterApp(ctx, &registration.Options{
		Source:     "feishu-livechat",
		Domain:     o.AccountsURL,
		CreateOnly: true,
		AppPreset:  &registration.AppPreset{Name: o.Name, Desc: o.Description},
		Addons: &registration.AppAddons{
			Scopes: registration.AppAddonsScopes{Tenant: Scopes},
			Events: registration.AppAddonsEvents{Items: registration.AppAddonsEventItems{Tenant: Events}},
		},
		OnQRCode: func(info *registration.QRCodeInfo) {
			if o.OnURL != nil {
				o.OnURL(info.URL, info.ExpireIn)
			}
		},
		OnStatusChange: func(info *registration.StatusChangeInfo) {
			if info.Status == registration.StatusPolling && o.OnWaiting != nil {
				o.OnWaiting()
			}
		},
	})
	if err != nil {
		return Registration{}, err
	}
	r := Registration{AppID: res.ClientID, AppSecret: res.ClientSecret}
	if res.UserInfo != nil {
		r.CreatorOpenID = res.UserInfo.OpenID
		r.Lark = res.UserInfo.TenantBrand == "lark"
	}
	return r, nil
}
