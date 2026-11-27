// Package hub fans out live updates to subscribers.
package hub

import "sync"

// Hub fans out live updates (logs, events, deliveries) to SSE subscribers.
// Slow subscribers miss messages rather than blocking publishers.
type Hub struct {
	mu      sync.Mutex
	subs    map[string]map[chan any]struct{}
	closing chan struct{}
	once    sync.Once
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan any]struct{}{}, closing: make(chan struct{})}
}

// Close ends every live stream (on shutdown). Streams never go idle, so
// without this a graceful http.Server.Shutdown waits for its full timeout.
func (h *Hub) Close() { h.once.Do(func() { close(h.closing) }) }

// Done is closed when the hub shuts down.
func (h *Hub) Done() <-chan struct{} { return h.closing }

func (h *Hub) Subscribe(topic string) (<-chan any, func()) {
	ch := make(chan any, 256)
	h.mu.Lock()
	if h.subs[topic] == nil {
		h.subs[topic] = map[chan any]struct{}{}
	}
	h.subs[topic][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[topic], ch)
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(topic string, v any) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[topic] {
		select {
		case ch <- v:
		default:
		}
	}
}

func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.subs {
		n += len(m)
	}
	return n
}
