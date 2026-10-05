// Package server implements the GoSync sync server as an http.Handler.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/HarshalPatel1972/GoSync/hlc"
	"github.com/HarshalPatel1972/GoSync/protocol"
	"github.com/HarshalPatel1972/GoSync/store"
)

// Config configures a Server. Store and Auth are required.
type Config struct {
	Store store.Store
	Auth  Authenticator
	// Broker fans out change notifications. Default: NewLocalBroker().
	Broker Broker
	// AllowedOrigins lists browser origins (e.g. "https://app.example.com")
	// allowed to connect. "*" allows any origin. Requests without an Origin
	// header (native clients) are always allowed; they still need a token.
	AllowedOrigins []string
	Logger         *slog.Logger
	// Metrics records Prometheus metrics when non-nil (see NewMetrics).
	Metrics *Metrics
	// ValidateMutation, when set, runs for every structurally valid
	// mutation before it is stored. Returning an error rejects it (the
	// client receives the message), which is how to enforce permissions
	// (e.g. read-only collections) and business rules. It must be fast
	// and must not block: it runs inline on the connection.
	ValidateMutation func(ctx context.Context, p Principal, m protocol.Mutation) error

	// Tuning; zero values use the defaults below.
	MaxConnections  int           // default 10000
	HelloTimeout    time.Duration // default 10s
	PingInterval    time.Duration // default 25s
	MaxClockSkew    time.Duration // default 5m; later client HLCs are rejected
	PullBudgetBytes int           // default 2 MiB per pull response
	MessagesPerSec  float64       // per-connection rate limit; default 50
	MessageBurst    int           // default 100
}

// Server serves the sync WebSocket endpoint plus health checks.
type Server struct {
	cfg      Config
	log      *slog.Logger
	hub      *hub
	upgrader websocket.Upgrader
	instance string

	live atomic.Int64 // open sockets, authenticated or not

	mu       sync.Mutex
	closed   bool
	sessions sync.WaitGroup
}

// New validates cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Auth == nil {
		return nil, errors.New("server: Store and Auth are required")
	}
	if cfg.Broker == nil {
		cfg.Broker = NewLocalBroker()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	setDefault(&cfg.MaxConnections, 10000)
	setDefault(&cfg.HelloTimeout, 10*time.Second)
	setDefault(&cfg.PingInterval, 25*time.Second)
	setDefault(&cfg.MaxClockSkew, 5*time.Minute)
	setDefault(&cfg.PullBudgetBytes, 2<<20)
	setDefault(&cfg.MessagesPerSec, 50)
	setDefault(&cfg.MessageBurst, 100)

	s := &Server{cfg: cfg, log: cfg.Logger, hub: newHub(), instance: randomID(6)}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		EnableCompression: true,
		CheckOrigin:       s.checkOrigin,
	}
	cfg.Broker.Subscribe(s.hub.poke)
	return s, nil
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for _, allowed := range s.cfg.AllowedOrigins {
		if allowed == "*" || strings.EqualFold(strings.TrimSuffix(allowed, "/"), u.Scheme+"://"+u.Host) {
			return true
		}
	}
	// Same-origin is always fine.
	return strings.EqualFold(u.Host, r.Host)
}

// Handler returns routes for /sync (WebSocket), /healthz and /readyz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sync", s.ServeSync)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if p, ok := s.cfg.Store.(interface{ Ping(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := p.Ping(ctx); err != nil {
				http.Error(w, "store unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.Write([]byte("ok\n"))
	})
	return mux
}

// ServeSync upgrades the request to a sync WebSocket session.
func (s *Server) ServeSync(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.sessions.Add(1)
	}
	s.mu.Unlock()
	if closed {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.sessions.Done()

	// Count every socket, including ones still in the handshake, so clients
	// that connect and never say hello cannot exhaust the server.
	if s.live.Add(1) > int64(s.cfg.MaxConnections) {
		s.live.Add(-1)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer s.live.Add(-1)
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote an HTTP error
	}
	c := newConn(s, ws, r.RemoteAddr)
	c.serve(r.Context())
}

// RunCompaction purges documents deleted more than retention ago, every
// interval, until ctx is done. It does nothing if the store cannot compact.
// Running it on several instances at once is safe.
func (s *Server) RunCompaction(ctx context.Context, retention, interval time.Duration) {
	c, ok := s.cfg.Store.(store.Compactor)
	if !ok || retention <= 0 {
		return
	}
	const batch = 500
	for {
		cutoff := hlc.Timestamp{Wall: time.Now().Add(-retention).UnixMilli()}.String()
		for ctx.Err() == nil {
			n, namespaces, err := c.Compact(ctx, cutoff, batch)
			if err != nil {
				s.log.Error("tombstone compaction failed", "err", err)
				break
			}
			s.cfg.Metrics.compactedDocs(n)
			for _, ns := range namespaces {
				s.cfg.Broker.Publish(ctx, ns, "")
			}
			if n > 0 {
				s.log.Info("compacted tombstones", "documents", n, "namespaces", len(namespaces))
			}
			if n < batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Shutdown stops accepting sessions, asks every client to reconnect
// elsewhere, and waits for sessions to end or ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	for _, c := range s.hub.all() {
		c.closeWith(websocket.CloseGoingAway, "server shutting down")
	}
	done := make(chan struct{})
	go func() { s.sessions.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
