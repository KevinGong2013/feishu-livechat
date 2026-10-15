package livechat

import (
	"sync"
	"time"
)

// hub fans new and changed messages out to the user's open pages (the SSE
// streams). It is per process: with several instances behind a load
// balancer a page only hears about messages handled by its own instance
// until it re-syncs, which the client does on every reconnect.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan *Message]struct{}
}

// subBuffer is how many messages a slow page may lag behind before it is
// dropped. A dropped page sees its stream end, reconnects and catches up
// with GET /messages?after=, so nothing is lost silently.
const subBuffer = 32

func newHub() *hub {
	return &hub{subs: make(map[string]map[chan *Message]struct{})}
}

func (h *hub) subscribe(userID string) (<-chan *Message, func()) {
	ch := make(chan *Message, subBuffer)
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = make(map[chan *Message]struct{})
	}
	h.subs[userID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() { h.remove(userID, ch) }
}

func (h *hub) remove(userID string, ch chan *Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[userID][ch]; !ok {
		return
	}
	delete(h.subs[userID], ch)
	if len(h.subs[userID]) == 0 {
		delete(h.subs, userID)
	}
	close(ch)
}

func (h *hub) publish(userID string, m *Message) {
	cp := *m
	h.mu.Lock()
	var slow []chan *Message
	for ch := range h.subs[userID] {
		select {
		case ch <- &cp:
		default:
			slow = append(slow, ch)
		}
	}
	h.mu.Unlock()
	for _, ch := range slow {
		h.remove(userID, ch)
	}
}

func (h *hub) online(userID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[userID]) > 0
}

// keyedMutex serializes work per key (per conversation) without keeping a
// mutex around for every conversation ever seen.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*keyedEntry)
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// rateLimiter allows limit events per key per fixed window. limit < 0
// disables it.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu    sync.Mutex
	seen  map[string]*rateWindow
	swept time.Time
}

type rateWindow struct {
	start time.Time
	n     int
}

func (r *rateLimiter) allow(key string) bool {
	if r.limit < 0 {
		return true
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]*rateWindow)
	}
	if now.Sub(r.swept) > r.window {
		for k, w := range r.seen {
			if now.Sub(w.start) > r.window {
				delete(r.seen, k)
			}
		}
		r.swept = now
	}
	w := r.seen[key]
	if w == nil || now.Sub(w.start) > r.window {
		w = &rateWindow{start: now}
		r.seen[key] = w
	}
	if w.n >= r.limit {
		return false
	}
	w.n++
	return true
}
