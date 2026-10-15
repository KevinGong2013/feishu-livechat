# feishu-livechat

把网站的在线客服接到**飞书话题群**：每位用户在群里有一个话题，客服直接在飞书（手机或电脑）里回复，支持文字和图片。

*Live chat for your website, answered from a Feishu/Lark topic group: one topic per user, agents reply by @-ing the bot.*

```
网站聊天窗 ──HTTP──► feishu-livechat ──飞书 Open API──► 客服话题群
    ▲                 │  第一次发消息：机器人发一张用户卡片（@所有人），开出这个用户的话题
    │                 │  之后的消息：都追加在同一个话题里
    └──SSE 实时推送───┘◄── 长连接事件：话题里 @机器人 的回复转发给用户
```

## 客服怎么用

- 每位用户一个话题，话题第一条是用户卡片（名字、邮箱等，以及「打开后台」之类的按钮）。卡片 @所有人，所以群里每个客服都会收到这个话题的后续消息。卡片发出后不再更新。
- **只有 @机器人 的回复才会发给用户**；没有 @机器人 的消息只有客服能看到，可以放心在话题里讨论。
- 转发成功的消息会被机器人贴上 ✅（`DONE`），没能转发的贴 ❌（`CrossMark`）。没有表情的消息就是只有客服看得到的。
- 能转发的是文字和图片：要发截图，就在输入框里先 @机器人 再粘贴图片，一起发出去。@ 会被去掉，链接保留文字和地址，表情、视频、文件等其他内容不会转发。
- 在飞书里撤回已转发的消息，用户那边也会撤回。
- 如果有人删掉了话题的根卡片，用户下次发消息时会自动开一个新话题。

## 快速开始：独立部署

适合不是 Go 写的网站。Go 服务可以把它当库嵌进去，见[作为 Go 库嵌入](#作为-go-库嵌入)。

### 1. 在飞书开放平台创建企业自建应用

1. 在[飞书开放平台](https://open.feishu.cn/app)创建**企业自建应用**，开启**机器人**能力。
2. 在「权限管理」开通：
   - 获取与发送单聊、群组消息 `im:message`
   - 以应用的身份发消息 `im:message:send_as_bot`
   - 接收群聊中 @机器人消息事件 `im:message.group_at_msg:readonly`
   - 获取与上传图片或文件资源 `im:resource`
   - 获取与更新群组信息 `im:chat`（只在 `setup` 建群时用到）

   不需要「获取群组中所有消息」这类敏感权限：机器人只接收 @它 的消息。如果接口报缺少权限，错误信息里会写明是哪一项，以飞书的提示为准。
3. 在「事件与回调」里选择**使用长连接接收事件**，添加事件「接收消息」`im.message.receive_v1` 和「消息被撤回」`im.message.recalled_v1`。用长连接就不需要公网回调地址。
4. 创建版本并发布，权限和事件才会生效。

### 2. 建客服话题群

```bash
export FEISHU_APP_ID=cli_xxx FEISHU_APP_SECRET=xxx
feishu-livechat setup -name 在线客服
# bot:        客服机器人 (ou_xxx)
# group:      在线客服 (话题模式, owner: the bot)
# chat id:    oc_xxx   → set FEISHU_CHAT_ID to this
# join link:  https://applink.feishu.cn/...   (valid: week; send it to your agents)
```

群由机器人创建，机器人是群主，所以它能在卡片里 @所有人。把入群链接发给客服。链接过期后可以用 `setup -chat oc_xxx` 重新生成。

也可以用已有的话题群：把机器人拉进去，并确认群设置允许机器人 @所有人。

### 3. 启动

```bash
docker run -d -p 8080:8080 -v livechat:/data \
  -e FEISHU_APP_ID=cli_xxx -e FEISHU_APP_SECRET=xxx -e FEISHU_CHAT_ID=oc_xxx \
  -e LIVECHAT_JWT_SECRET="$(openssl rand -hex 32)" \
  -e LIVECHAT_ALLOWED_ORIGINS=https://www.example.com \
  feishu-livechat
```

| 环境变量 | 说明 |
|---|---|
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | 自建应用凭证（必填） |
| `FEISHU_CHAT_ID` | 客服话题群的 chat id（必填） |
| `FEISHU_LARK` | `1` 表示使用 Lark 国际版（open.larksuite.com） |
| `FEISHU_CARD_HINT` | 用户卡片底部的提示文字 |
| `LIVECHAT_JWT_SECRET` | 网站后端签发用户 token 的密钥，至少 32 字节（必填） |
| `LIVECHAT_DATA_DIR` | 数据目录，SQLite 数据库和图片都放在这里，默认 `./data`（镜像里是 `/data`） |
| `DATABASE_URL` | 设置为 `postgres://…` 时改用 Postgres；留空用 SQLite |
| `LISTEN_ADDR` | 默认 `:8080` |
| `LIVECHAT_ALLOWED_ORIGINS` | 允许跨域调用的网站，逗号分隔；`*` 表示任意 |
| `LIVECHAT_DEMO` | `1` 时在 `/` 提供一个演示聊天页 |

### 4. 网站后端给登录用户签 token

用 HS256 和 `LIVECHAT_JWT_SECRET` 签 JWT，交给前端聊天窗，放在 `Authorization: Bearer <token>` 里。

- `sub`：用户的稳定 ID（必填）
- `exp`：过期时间（必填）
- `name`、`fields`、`links`：开话题时用来生成用户卡片

```js
// Node.js（jsonwebtoken）
const token = jwt.sign({
  sub: user.id,
  name: user.name,
  fields: [{ label: "邮箱", value: user.email }, { label: "注册时间", value: "2026-03-01" }],
  links: [{ text: "打开后台", url: `https://admin.example.com/users/${user.id}` }],
}, process.env.LIVECHAT_JWT_SECRET, { algorithm: "HS256", expiresIn: "12h" });
```

```python
# Python（PyJWT）
token = jwt.encode({"sub": user.id, "name": user.name,
                    "fields": [{"label": "邮箱", "value": user.email}],
                    "exp": int(time.time()) + 12 * 3600}, SECRET, algorithm="HS256")
```

本地试用：设置 `LIVECHAT_DEMO=1`，用 `feishu-livechat token -user u1 -name 张三 -field 邮箱=z@example.com` 生成一个 token，打开 `http://localhost:8080/` 粘贴进去。

卡片只在开话题时生成一次，以后不会更新。所以卡片上放不会变的信息（ID、邮箱、注册时间），会变的数据用 `links` 链接到你的后台去看。

## HTTP API

所有接口都需要 `Authorization`，只有 `/files` 例外（它靠 URL 里的签名鉴权）。独立部署时路径前缀是 `/api`。

| 接口 | 说明 |
|---|---|
| `GET /messages?before=&after=&limit=` | 消息历史，升序返回；`{"messages": [...], "has_more": bool}`。不带参数时返回最新一页 |
| `POST /messages` `{"text"}` | 发文字，最多 4000 字 |
| `POST /images` | 发图片：请求体直接是图片，或 multipart 的 `file` 字段。支持 PNG / JPEG / GIF / WebP，最大 10 MB |
| `POST /read` `{"message_id"}` | 标记已读到某条消息 |
| `GET /unread` | 未读的客服回复数，`{"count": n}` |
| `GET /events` | SSE：先发一个 `ready`，之后每条新消息或有变化的消息（发送成功/失败、撤回）都是一个 `message` 事件 |
| `GET /files/{key}?exp=&sig=` | 图片，短期有效的签名链接 |

每个用户每分钟最多发 20 条（`livechat.Config.MaxMessagesPerMinute`），超出返回 429：所有用户共用机器人在群里的发送频率额度，一个人刷屏会拖慢其他人。

消息格式：

```json
{"id": "0192…", "sender": "user|agent", "status": "sending|sent|failed", "recalled": false,
 "blocks": [{"type": "text", "text": "…"}, {"type": "image", "url": "/api/files/…", "width": 800, "height": 600}],
 "created_at": 1760000000000}
```

- 前端按 `id` 合并消息：同一条消息会先以 `sending` 出现，再以 `sent` 或 `failed` 出现一次。
- `id` 是 UUIDv7，可以直接按字符串排序。
- SSE 断线重连后，请求 `GET /messages?after=<最新 id>` 补齐断线期间的消息。
- `EventSource` 不能带请求头，所以要用 `fetch` 读取 SSE。写法见 `cmd/feishu-livechat/demo/index.html`。

## 作为 Go 库嵌入

```go
store := sqlstore.New(db, sqlstore.Postgres) // 建表 SQL 在 sqlstore/migrations/，也可以直接调用 store.Migrate(ctx)
fc, _ := feishu.New(feishu.Config{AppID: id, AppSecret: secret, ChatID: chatID})
bridge, _ := livechat.New(livechat.Config{
	Store:   store,
	Blobs:   myBlobStore, // 实现 livechat.BlobStore；如果也实现了 URLSigner，图片会直接用对象存储的签名链接
	Backend: fc,
	OnAgentMessage: func(ctx context.Context, userID string, m *livechat.Message, online bool) {
		if !online {
			// 用户没开着页面：发 Web Push / 站内信 / 邮件
		}
	},
})
go fc.Listen(ctx, bridge) // 只在处理客服回复的进程里运行

mux.Handle("/api/v1/support/", bridge.Handler(livechat.HandlerOptions{
	BasePath:   "/api/v1/support",
	SigningKey: imageURLKey,
	Authenticate: func(r *http.Request) (livechat.User, error) {
		u := currentUser(r) // 你自己的登录态
		return livechat.User{ID: u.ID, Name: u.Name, Fields: …, Links: …}, nil
	},
}))
```

## 限制和注意事项

- **只转发文字和图片。** 手机飞书从相册直接发的图片是单独一条消息，带不了 @，所以不会转发。要和 @机器人 写在同一条消息里发。
- **卡片的 @所有人** 需要机器人有 @所有人 的权限；`setup` 建的群里机器人是群主，没问题。第一次部署后，请确认客服确实收到了新话题的提醒。
- **多实例部署：** 飞书的长连接事件每条只投递给其中一个实例；SSE 实时推送也只在本进程内。用户的页面如果连在另一个实例上，要等它重连补齐才能看到。单实例部署不受影响。跨实例推送（Redis）在路线图里。
- 表情回执用的 `emoji_type` 可以通过 `feishu.Config` 修改。
- 话题的根卡片永远不更新，所以不受飞书「卡片只能在 14 天内更新」的限制。

## 开发

```bash
go test -race ./...
# 同时测 Postgres：
LIVECHAT_TEST_POSTGRES_URL=postgres://postgres:postgres@localhost:5432/livechat_test?sslmode=disable go test ./sqlstore/
```

代码结构：

- `livechat`（根目录）：会话、消息、桥接逻辑、HTTP 接口
- `feishu`：飞书后端、消息解析、长连接、建群
- `sqlstore`：SQLite / Postgres 存储
- `localblob`：本地磁盘图片存储
- `cmd/feishu-livechat`：独立服务

## 路线图

- 可直接嵌入的聊天窗组件和 JS 客户端（npm）
- Redis 跨实例推送
- 未登录访客
- 其他话题型 IM 后端（Telegram 论坛话题、Slack）

## License

MIT
