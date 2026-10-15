package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/KevinGong2013/feishu-livechat/feishu"
)

type initConfig struct {
	appName   string
	groupName string
	envFile   string
	link      string
	// Test hooks: where the registration and Open API calls go.
	accountsURL string
	openBaseURL string
	botWait     time.Duration
}

// initCmd sets everything up from nothing: create the app by scanning a QR
// code in 飞书, create the 话题群 with the scanner in it, and write the
// configuration serve reads.
func initCmd(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("init", flag.ExitOnError)
	c := initConfig{botWait: time.Minute}
	fl.StringVar(&c.appName, "name", "在线客服", "app (bot) name, editable on the creation page")
	fl.StringVar(&c.groupName, "group", "在线客服", "name of the 话题群 to create")
	fl.StringVar(&c.envFile, "env", defaultEnvFile, "where to write the configuration")
	fl.StringVar(&c.link, "link", "week", "join link validity: week, year or permanently")
	_ = fl.Parse(args)
	return runInit(ctx, c, os.Stdout)
}

func runInit(ctx context.Context, c initConfig, out io.Writer) error {
	if _, err := os.Stat(c.envFile); err == nil {
		return fmt.Errorf("%s already exists; delete it or pass -env to write elsewhere", c.envFile)
	}

	fmt.Fprintln(out, "① 用飞书扫描下面的二维码（或把链接发到飞书里点开），确认创建应用：")
	reg, err := feishu.RegisterApp(ctx, feishu.RegisterOptions{
		Name:        c.appName,
		AccountsURL: c.accountsURL,
		OnURL: func(url string, expiresIn int) {
			if code, err := renderQR(url); err == nil {
				fmt.Fprint(out, code)
			}
			fmt.Fprintf(out, "\n%s\n\n等待确认（%d 分钟内有效）", url, expiresIn/60)
		},
		OnWaiting: func() { fmt.Fprint(out, ".") },
	})
	fmt.Fprintln(out)
	if err != nil {
		return fmt.Errorf("create app: %w", err)
	}
	fmt.Fprintf(out, "② 应用已创建：%s\n", reg.AppID)

	jwtSecret := make([]byte, 32)
	if _, err := rand.Read(jwtSecret); err != nil {
		return err
	}
	cfg := []envVar{
		{"FEISHU_APP_ID", reg.AppID},
		{"FEISHU_APP_SECRET", reg.AppSecret},
		{"FEISHU_CHAT_ID", ""},
		{"LIVECHAT_JWT_SECRET", hex.EncodeToString(jwtSecret)},
	}
	if reg.Lark {
		cfg = append(cfg, envVar{"FEISHU_LARK", "1"})
	}
	// Save the credentials before anything else can fail.
	if err := writeEnvFile(c.envFile, cfg); err != nil {
		return err
	}

	client, err := feishu.New(feishu.Config{AppID: reg.AppID, AppSecret: reg.AppSecret, Lark: reg.Lark, OpenBaseURL: c.openBaseURL})
	if err != nil {
		return err
	}
	bot, err := client.WaitReady(ctx, c.botWait)
	if err != nil {
		return fmt.Errorf("%w\n应用的机器人能力还不可用：在 %s 打开「机器人」能力并发布版本，然后运行 feishu-livechat setup -name %s",
			err, consoleURL(reg), c.groupName)
	}

	var members []string
	if reg.CreatorOpenID != "" {
		members = append(members, reg.CreatorOpenID)
	}
	chatID, err := client.CreateTopicGroup(ctx, c.groupName, "在线客服：每位用户一个话题，@机器人 的回复会发给用户。", members...)
	if err != nil {
		return fmt.Errorf("create group: %w", err)
	}
	cfg[2].value = chatID
	if err := writeEnvFile(c.envFile, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "③ 已创建话题群「%s」，机器人「%s」是群主", c.groupName, bot.AppName)
	if len(members) > 0 {
		fmt.Fprint(out, "，你已在群里")
	}
	fmt.Fprintln(out)

	if share, err := client.ShareLink(ctx, chatID, c.link); err == nil {
		fmt.Fprintf(out, "④ 其他客服用这个链接入群（有效期 %s）：%s\n", c.link, share)
	} else {
		fmt.Fprintf(out, "④ 入群链接生成失败（%v），稍后可用 feishu-livechat setup -chat %s 重试\n", err, chatID)
	}

	fmt.Fprintf(out, "⑤ 配置已写入 %s。启动服务：\n\n   feishu-livechat serve\n\n", c.envFile)
	fmt.Fprintln(out, "   本地试用：LIVECHAT_DEMO=1 feishu-livechat serve，再用 feishu-livechat token -user u1 -name 测试用户 生成 token，打开 http://localhost:8080/")
	return nil
}

func consoleURL(r feishu.Registration) string {
	host := "https://open.feishu.cn"
	if r.Lark {
		host = "https://open.larksuite.com"
	}
	return host + "/app/" + r.AppID
}

// renderQR draws a QR code with half-block characters, two modules rows
// per text line. Light modules are drawn, so it reads correctly on the
// usual dark terminal background.
func renderQR(text string) (string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return "", err
	}
	const quiet = 2
	n := code.Size + 2*quiet
	light := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return true
		}
		return !code.Black(x, y)
	}
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			top, bottom := light(x, y), y+1 < n && light(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}
