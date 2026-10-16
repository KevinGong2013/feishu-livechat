// Command feishu-livechat runs the live chat bridge as a standalone
// service, for sites that are not written in Go (Go services can embed the
// livechat package instead).
//
//	feishu-livechat init                     scan a QR code in 飞书: creates the app and the 话题群, writes feishu-livechat.env
//	feishu-livechat serve                    run the HTTP API and the 飞书 listener
//	feishu-livechat setup -name 在线客服    for an existing app: create the 话题群, print its chat id and a join link
//	feishu-livechat token -user u1 -name 张三  mint a user token for testing
//
// Configuration is read from the environment, falling back to
// feishu-livechat.env (or the file LIVECHAT_ENV_FILE names); see README.md.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	livechat "github.com/KevinGong2013/feishu-livechat"
	"github.com/KevinGong2013/feishu-livechat/feishu"
	"github.com/KevinGong2013/feishu-livechat/localblob"
	"github.com/KevinGong2013/feishu-livechat/sqlstore"
)

//go:embed demo
var demoFS embed.FS

const usage = `usage:
  feishu-livechat init [-name <应用名>] [-group <群名称>] [-env feishu-livechat.env]
  feishu-livechat setup -name <群名称> [-link week|year|permanently]
  feishu-livechat setup -chat <chat id> [-link ...]     print a new join link for an existing group
  feishu-livechat serve
  feishu-livechat token -user <id> [-name <名字>] [-field 标签=值]... [-link 文字=URL]... [-ttl 24h]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if os.Args[1] != "init" {
		if err := loadEnvFile(env("LIVECHAT_ENV_FILE", defaultEnvFile)); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	var err error
	switch os.Args[1] {
	case "init":
		err = initCmd(ctx, os.Args[2:])
	case "serve":
		err = serve(ctx)
	case "setup":
		err = setup(ctx, os.Args[2:])
	case "token":
		err = token(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func mustEnv(keys ...string) error {
	var missing []string
	for _, k := range keys {
		if env(k, "") == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
	}
	return nil
}

func feishuClient(chatID string, log *slog.Logger) (*feishu.Client, error) {
	if err := mustEnv("FEISHU_APP_ID", "FEISHU_APP_SECRET"); err != nil {
		return nil, err
	}
	return feishu.New(feishu.Config{
		AppID:     env("FEISHU_APP_ID", ""),
		AppSecret: env("FEISHU_APP_SECRET", ""),
		ChatID:    chatID,
		Lark:      env("FEISHU_LARK", "") == "1",
		CardHint:  env("FEISHU_CARD_HINT", ""),
		Logger:    log,
	})
}

func setup(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("setup", flag.ExitOnError)
	name := fl.String("name", "", "group name for a new 话题群")
	chat := fl.String("chat", "", "existing group's chat id (only print a join link)")
	link := fl.String("link", "week", "join link validity: week, year or permanently")
	_ = fl.Parse(args)
	if (*name == "") == (*chat == "") {
		return errors.New("pass exactly one of -name or -chat")
	}
	c, err := feishuClient(*chat, slog.Default())
	if err != nil {
		return err
	}
	bot, err := c.Bot(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("bot:        %s (%s)\n", bot.AppName, bot.OpenID)

	chatID := *chat
	if chatID == "" {
		if chatID, err = c.CreateTopicGroup(ctx, *name, "在线客服：每位用户一个话题，@机器人 的回复会发给用户。"); err != nil {
			return err
		}
		fmt.Printf("group:      %s (话题模式, owner: the bot)\n", *name)
	}
	fmt.Printf("chat id:    %s   → set FEISHU_CHAT_ID to this\n", chatID)
	share, err := c.ShareLink(ctx, chatID, *link)
	if err != nil {
		return err
	}
	fmt.Printf("join link:  %s   (valid: %s; send it to your agents)\n", share, *link)
	return nil
}

func serve(ctx context.Context) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := mustEnv("FEISHU_APP_ID", "FEISHU_APP_SECRET", "FEISHU_CHAT_ID", "LIVECHAT_JWT_SECRET"); err != nil {
		return err
	}
	secret := []byte(env("LIVECHAT_JWT_SECRET", ""))
	if len(secret) < 32 {
		return errors.New("LIVECHAT_JWT_SECRET must be at least 32 bytes")
	}
	dataDir := env("LIVECHAT_DATA_DIR", "./data")

	db, dialect, err := openDB(dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlstore.New(db, dialect)
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	blobs, err := localblob.New(filepath.Join(dataDir, "images"))
	if err != nil {
		return err
	}
	fc, err := feishuClient(env("FEISHU_CHAT_ID", ""), log)
	if err != nil {
		return err
	}
	bridge, err := livechat.New(livechat.Config{Store: store, Blobs: blobs, Backend: fc, Logger: log})
	if err != nil {
		return err
	}

	// Derive the image URL key from the JWT secret, so links survive
	// restarts and work across instances without another setting.
	signing := sha256.Sum256(append([]byte("feishu-livechat image urls\n"), secret...))
	api := bridge.Handler(livechat.HandlerOptions{
		BasePath:     "/api",
		SigningKey:   signing[:],
		Authenticate: jwtAuthenticator(secret),
		AgentName:    store.AgentNames("客服 %02d"),
	})
	mux := http.NewServeMux()
	mux.Handle("/api/", cors(api, env("LIVECHAT_ALLOWED_ORIGINS", "")))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	if env("LIVECHAT_DEMO", "") == "1" {
		sub, _ := fs.Sub(demoFS, "demo")
		mux.Handle("GET /", http.FileServerFS(sub))
	}

	srv := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 2)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()
	go func() {
		log.Info("connecting to 飞书 (long connection)")
		errc <- fc.Listen(ctx, bridge)
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// openDB uses DATABASE_URL (postgres://…) when set, else a SQLite file in
// the data directory.
func openDB(dataDir string) (*sql.DB, sqlstore.Dialect, error) {
	if dsn := env("DATABASE_URL", ""); dsn != "" {
		if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
			return nil, 0, errors.New("DATABASE_URL must be a postgres:// URL (leave it empty for SQLite)")
		}
		db, err := sql.Open("pgx", dsn)
		return db, sqlstore.Postgres, err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, 0, err
	}
	db, err := sqlstore.OpenSQLite(filepath.Join(dataDir, "livechat.db"))
	return db, sqlstore.SQLite, err
}

// cors lets pages on the listed origins ("*" for any) call the API. The API
// authenticates with a bearer token, never cookies, so credentials are not
// allowed.
func cors(next http.Handler, origins string) http.Handler {
	allowed := map[string]bool{}
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[strings.TrimSuffix(o, "/")] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowed["*"] || allowed[origin]) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
