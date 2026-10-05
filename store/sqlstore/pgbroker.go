package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const pokeChannel = "gosync_poke"

// PostgresBroker fans out change notifications between server instances with
// PostgreSQL LISTEN/NOTIFY. It satisfies server.Broker.
type PostgresBroker struct {
	dsn    string
	db     *sql.DB
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.RWMutex
	handler func(ns, origin string)
}

type pokePayload struct {
	NS     string `json:"ns"`
	Origin string `json:"o"`
}

// NewPostgresBroker publishes through s and listens on a dedicated connection
// to dsn.
func NewPostgresBroker(s *Store, dsn string, log *slog.Logger) *PostgresBroker {
	ctx, cancel := context.WithCancel(context.Background())
	return &PostgresBroker{dsn: dsn, db: s.db, log: log, ctx: ctx, cancel: cancel}
}

func (b *PostgresBroker) Publish(ctx context.Context, ns, origin string) error {
	payload, _ := json.Marshal(pokePayload{NS: ns, Origin: origin})
	_, err := b.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, pokeChannel, string(payload))
	return err
}

func (b *PostgresBroker) Subscribe(handler func(ns, origin string)) {
	b.mu.Lock()
	first := b.handler == nil
	b.handler = handler
	b.mu.Unlock()
	if first {
		b.wg.Add(1)
		go b.listen()
	}
}

func (b *PostgresBroker) deliver(ns, origin string) {
	b.mu.RLock()
	h := b.handler
	b.mu.RUnlock()
	h(ns, origin)
}

func (b *PostgresBroker) listen() {
	defer b.wg.Done()
	backoff := time.Second
	reconnected := false
	for b.ctx.Err() == nil {
		err := b.listenOnce(reconnected)
		if b.ctx.Err() != nil {
			return
		}
		b.log.Warn("postgres listener disconnected; retrying", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-b.ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
		reconnected = true
	}
}

func (b *PostgresBroker) listenOnce(reconnected bool) error {
	conn, err := pgx.Connect(b.ctx, b.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(b.ctx, "LISTEN "+pokeChannel); err != nil {
		return err
	}
	if reconnected {
		// Notifications sent while we were disconnected are lost; poke
		// everyone so no client misses a change.
		b.deliver("", "")
	}
	for {
		n, err := conn.WaitForNotification(b.ctx)
		if err != nil {
			return err
		}
		var p pokePayload
		if json.Unmarshal([]byte(n.Payload), &p) == nil && p.NS != "" {
			b.deliver(p.NS, p.Origin)
		}
	}
}

// Close stops listening.
func (b *PostgresBroker) Close() error {
	b.cancel()
	b.wg.Wait()
	return nil
}
