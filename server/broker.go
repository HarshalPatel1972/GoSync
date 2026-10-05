package server

import (
	"context"
	"sync"
)

// Broker fans out "namespace changed" notifications. With one server
// instance LocalBroker is enough; when several instances share a database
// use a broker that crosses processes, such as sqlstore.PostgresBroker.
type Broker interface {
	// Publish announces that ns changed. origin identifies the connection
	// that caused the change so it is not poked about its own write.
	Publish(ctx context.Context, ns, origin string) error
	// Subscribe registers the single handler that receives notifications,
	// including this instance's own. It is called once, before Publish.
	Subscribe(handler func(ns, origin string))
	Close() error
}

// LocalBroker delivers notifications within this process.
type LocalBroker struct {
	mu      sync.RWMutex
	handler func(ns, origin string)
}

func NewLocalBroker() *LocalBroker { return &LocalBroker{} }

func (b *LocalBroker) Publish(_ context.Context, ns, origin string) error {
	b.mu.RLock()
	h := b.handler
	b.mu.RUnlock()
	if h != nil {
		h(ns, origin)
	}
	return nil
}

func (b *LocalBroker) Subscribe(handler func(ns, origin string)) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

func (b *LocalBroker) Close() error { return nil }

// hub tracks live connections by namespace.
type hub struct {
	mu    sync.RWMutex
	byNS  map[string]map[*conn]struct{}
	total int
}

func newHub() *hub { return &hub{byNS: map[string]map[*conn]struct{}{}} }

func (h *hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byNS[c.principal.Namespace]
	if set == nil {
		set = map[*conn]struct{}{}
		h.byNS[c.principal.Namespace] = set
	}
	set[c] = struct{}{}
	h.total++
}

func (h *hub) remove(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byNS[c.principal.Namespace]
	if _, ok := set[c]; !ok {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.byNS, c.principal.Namespace)
	}
	h.total--
}

func (h *hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.total
}

// poke tells every connection in ns except origin to pull; an empty ns pokes
// every connection (used after a broker misses notifications). Pokes
// coalesce: a connection with a poke already pending is not queued twice.
func (h *hub) poke(ns, origin string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	notify := func(set map[*conn]struct{}) {
		for c := range set {
			if c.id == origin {
				continue
			}
			select {
			case c.pokes <- struct{}{}:
			default:
			}
		}
	}
	if ns == "" {
		for _, set := range h.byNS {
			notify(set)
		}
		return
	}
	notify(h.byNS[ns])
}

func (h *hub) all() []*conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []*conn
	for _, set := range h.byNS {
		for c := range set {
			out = append(out, c)
		}
	}
	return out
}
