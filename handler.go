package livechat

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HandlerOptions configures the HTTP API the chat widget talks to.
type HandlerOptions struct {
	// Authenticate identifies the user making the request. Return an error
	// to answer 401. Embedded in an app, read the user your own auth
	// middleware already resolved; standalone, cmd/feishu-livechat verifies
	// a JWT signed by the host site.
	Authenticate func(*http.Request) (User, error)

	// BasePath is where the handler is mounted, e.g. "/api/v1/support". The
	// handler strips it from incoming paths itself and uses it to build
	// image URLs, so mount it there without another StripPrefix.
	BasePath string

	// SigningKey signs image URLs when the BlobStore cannot sign its own
	// (see URLSigner). Use the same key on every instance; if empty a
	// random key is generated, and URLs stop working after a restart.
	SigningKey []byte

	// ImageURLTTL is how long image URLs stay valid. Default 1h.
	ImageURLTTL time.Duration
}

// Handler serves the chat API:
//
//	GET  /status                           {"available"}: false until an app is connected; hide the chat entry then
//	GET  /messages?before=&after=&limit=  history, ascending; {"messages", "has_more"}
//	POST /messages        {"text"}         send text
//	POST /images          image bytes, or multipart field "file"
//	POST /read            {"message_id"}   mark read up to a message
//	GET  /unread                           {"count"}
//	GET  /events                           server-sent events, one "message" event per new or changed message
//	GET  /files/{key}?exp=&sig=            images, authorized by the signature in the URL
//
// Messages are upserted by id on the client: a message first arrives as
// "sending", then again as "sent" or "failed", and agent messages may come
// back "recalled". After a reconnect, fetch ?after=<newest id> to fill the
// gap.
func (b *Bridge) Handler(opts HandlerOptions) http.Handler {
	if opts.Authenticate == nil {
		panic("livechat: HandlerOptions.Authenticate is required")
	}
	if len(opts.SigningKey) == 0 {
		opts.SigningKey = make([]byte, 32)
		_, _ = rand.Read(opts.SigningKey)
	}
	if opts.ImageURLTTL <= 0 {
		opts.ImageURLTTL = time.Hour
	}
	opts.BasePath = strings.TrimSuffix(opts.BasePath, "/")
	h := &handler{b: b, opts: opts}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", h.authed(h.status))
	mux.HandleFunc("GET /messages", h.authed(h.listMessages))
	mux.HandleFunc("POST /messages", h.authed(h.sendText))
	mux.HandleFunc("POST /images", h.authed(h.sendImage))
	mux.HandleFunc("POST /read", h.authed(h.markRead))
	mux.HandleFunc("GET /unread", h.authed(h.unread))
	mux.HandleFunc("GET /events", h.authed(h.events))
	mux.HandleFunc("GET /files/{key...}", h.file)
	if opts.BasePath == "" {
		return mux
	}
	return http.StripPrefix(opts.BasePath, mux)
}

type handler struct {
	b    *Bridge
	opts HandlerOptions
}

func (h *handler) authed(fn func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := h.opts.Authenticate(r)
		if err != nil || u.ID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		fn(w, r, u)
	}
}

// MessageView is a message as the API returns it.
type MessageView struct {
	ID        string      `json:"id"`
	Sender    Sender      `json:"sender"`
	Status    Status      `json:"status"`
	Recalled  bool        `json:"recalled,omitempty"`
	Blocks    []BlockView `json:"blocks"`
	CreatedAt int64       `json:"created_at"` // unix milliseconds
}

// BlockView is a text run or an image with a short-lived URL.
type BlockView struct {
	Type   BlockType `json:"type"`
	Text   string    `json:"text,omitempty"`
	URL    string    `json:"url,omitempty"`
	Width  int       `json:"width,omitempty"`
	Height int       `json:"height,omitempty"`
}

func (h *handler) view(ctx context.Context, m *Message) MessageView {
	v := MessageView{
		ID:        m.ID,
		Sender:    m.Sender,
		Status:    m.Status,
		Recalled:  m.Recalled,
		Blocks:    []BlockView{},
		CreatedAt: m.CreatedAt.UnixMilli(),
	}
	if m.Recalled {
		return v // a recalled reply's content is not handed out anymore
	}
	for _, bl := range m.Blocks {
		bv := BlockView{Type: bl.Type, Text: bl.Text}
		if bl.Image != nil {
			bv.URL = h.imageURL(ctx, bl.Image.Key)
			bv.Width, bv.Height = bl.Image.Width, bl.Image.Height
		}
		v.Blocks = append(v.Blocks, bv)
	}
	return v
}

func (h *handler) imageURL(ctx context.Context, key string) string {
	if s, ok := h.b.cfg.Blobs.(URLSigner); ok {
		u, err := s.SignedURL(ctx, key, h.opts.ImageURLTTL)
		if err == nil {
			return u
		}
		h.b.log.Warn("livechat: sign image url", "key", key, "err", err)
		return ""
	}
	exp := strconv.FormatInt(time.Now().Add(h.opts.ImageURLTTL).Unix(), 10)
	q := url.Values{"exp": {exp}, "sig": {h.sign(key, exp)}}
	return h.opts.BasePath + "/files/" + key + "?" + q.Encode()
}

func (h *handler) sign(key, exp string) string {
	mac := hmac.New(sha256.New, h.opts.SigningKey)
	mac.Write([]byte(key + "\n" + exp))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *handler) status(w http.ResponseWriter, _ *http.Request, _ User) {
	writeJSON(w, http.StatusOK, map[string]bool{"available": h.b.Available()})
}

func (h *handler) listMessages(w http.ResponseWriter, r *http.Request, u User) {
	q := Query{Before: r.URL.Query().Get("before"), After: r.URL.Query().Get("after")}
	if !validID(q.Before) || !validID(q.After) || (q.Before != "" && q.After != "") {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		q.Limit = n
	}
	msgs, more, err := h.b.History(r.Context(), u.ID, q)
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		views = append(views, h.view(r.Context(), m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": views, "has_more": more})
}

func (h *handler) sendText(w http.ResponseWriter, r *http.Request, u User) {
	var body struct {
		Text string `json:"text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	m, err := h.b.SendText(r.Context(), u, body.Text)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.view(r.Context(), m))
}

func (h *handler) sendImage(w http.ResponseWriter, r *http.Request, u User) {
	limit := int64(h.b.cfg.MaxImageBytes)
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	var src io.Reader = r.Body
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		f, _, err := r.FormFile("file")
		if err != nil {
			h.fail(w, err)
			return
		}
		defer f.Close()
		src = f
	}
	data, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err != nil {
		h.fail(w, err)
		return
	}
	m, err := h.b.SendImage(r.Context(), u, data)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.view(r.Context(), m))
}

func (h *handler) markRead(w http.ResponseWriter, r *http.Request, u User) {
	var body struct {
		MessageID string `json:"message_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MessageID == "" || !validID(body.MessageID) {
		writeError(w, http.StatusBadRequest, "invalid message_id")
		return
	}
	if err := h.b.MarkRead(r.Context(), u.ID, body.MessageID); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) unread(w http.ResponseWriter, r *http.Request, u User) {
	n, err := h.b.Unread(r.Context(), u.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}

// sseHeartbeat keeps proxies from closing an idle stream.
const sseHeartbeat = 25 * time.Second

func (h *handler) events(w http.ResponseWriter, r *http.Request, u User) {
	rc := http.NewResponseController(w)
	// The stream outlives any server WriteTimeout.
	_ = rc.SetWriteDeadline(time.Time{})

	ch, cancel := h.b.Subscribe(u.ID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "event: ready\ndata: {}\n\n"); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}

	tick := time.NewTicker(sseHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
		case m, ok := <-ch:
			if !ok {
				return // fell behind; the client reconnects and re-syncs
			}
			data, _ := json.Marshal(h.view(r.Context(), m))
			if _, err := io.WriteString(w, "event: message\ndata: "+string(data)+"\n\n"); err != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
	}
}

func (h *handler) file(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	exp, sig := r.URL.Query().Get("exp"), r.URL.Query().Get("sig")
	expAt, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > expAt || !hmac.Equal([]byte(sig), []byte(h.sign(key, exp))) {
		writeError(w, http.StatusForbidden, "invalid or expired link")
		return
	}
	rc, err := h.b.cfg.Blobs.Get(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer rc.Close()
	ct := mime.TypeByExtension(path.Ext(key))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, rc)
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, ErrEmptyMessage):
		writeError(w, http.StatusBadRequest, "message is empty")
	case errors.Is(err, ErrTooLong):
		writeError(w, http.StatusBadRequest, "message is too long")
	case errors.Is(err, ErrBadImage):
		writeError(w, http.StatusBadRequest, "only PNG, JPEG, GIF and WebP images are supported")
	case errors.Is(err, ErrTooLarge), errors.As(err, &maxErr):
		writeError(w, http.StatusRequestEntityTooLarge, "image is too large")
	case errors.Is(err, ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "support chat is not available")
	case errors.Is(err, ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "too many messages, try again in a minute")
	case errors.Is(err, http.ErrMissingFile):
		writeError(w, http.StatusBadRequest, "missing file")
	default:
		h.b.log.Error("livechat: request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func validID(s string) bool {
	if s == "" {
		return true
	}
	_, err := uuid.Parse(s)
	return err == nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
