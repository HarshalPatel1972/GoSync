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

const (
	pokeChannel = "gosync_poke"
	// flushInterval bounds how long a change notification waits to be
	// batched. PostgreSQL serialises the commit of every transaction that
	// issued NOTIFY behind one database-wide lock, so one NOTIFY per push
	// caps throughput; batching makes the cost independent of write rate.
	flushInterval = 10 * time.Millisecond
	// maxPayload stays under PostgreSQL's 8000-byte NOTIFY payload limit.
	maxPayload = 7000
)

// PostgresBroker fans out change notifications between server instances with
// PostgreSQL LISTEN/NOTIFY. It satisfies server.Broker.
type PostgresBroker struct {
	dsn    string
	db     *sql.DB
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	handler func(ns, origin string)
	pending map[string]string // ns -> origin ("" once several origins changed it)
	kick    chan struct{}
}

type poke struct {
	NS     string `json:"n"`
	Origin string `json:"o,omitempty"`
}

// NewPostgresBroker publishes through s and listens on a dedicated connection
// to dsn.
func NewPostgresBroker(s *Store, dsn string, log *slog.Logger) *PostgresBroker {
	ctx, cancel := context.WithCancel(context.Background())
	b := &PostgresBroker{
		dsn: dsn, db: s.db, log: log, ctx: ctx, cancel: cancel,
		pending: map[string]string{},
		kick:    make(chan struct{}, 1),
	}
	b.wg.Add(1)
	go b.flushLoop()
	return b
}

// Publish queues a notification; it is sent within flushInterval.
func (b *PostgresBroker) Publish(_ context.Context, ns, origin string) error {
	b.mu.Lock()
	if prev, ok := b.pending[ns]; ok && prev != origin {
		origin = "" // changed by several connections: poke all of them
	}
	b.pending[ns] = origin
	b.mu.Unlock()
	select {
	case b.kick <- struct{}{}:
	default:
	}
	return nil
}

func (b *PostgresBroker) flushLoop() {
	defer b.wg.Done()
	for {
		select {
		case <-b.ctx.Done():
			b.flush(context.Background()) // deliver what is queued
			return
		case <-b.kick:
		}
		select { // let more notifications accumulate
		case <-time.After(flushInterval):
		case <-b.ctx.Done():
		}
		b.flush(b.ctx)
	}
}

// flush sends every queued notification in a single transaction, split into
// payloads under the size limit.
func (b *PostgresBroker) flush(ctx context.Context) {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.pending
	b.pending = map[string]string{}
	b.mu.Unlock()

	var payloads []string
	var cur []poke
	size := 0
	for ns, origin := range batch {
		p := poke{NS: ns, Origin: origin}
		n := len(ns) + len(origin) + 16
		if size+n > maxPayload && len(cur) > 0 {
			raw, _ := json.Marshal(cur)
			payloads = append(payloads, string(raw))
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += n
	}
	raw, _ := json.Marshal(cur)
	payloads = append(payloads, string(raw))

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := b.db.BeginTx(ctx, nil)
	if err == nil {
		for _, p := range payloads {
			if _, err = tx.ExecContext(ctx, `SELECT pg_notify($1, $2)`, pokeChannel, p); err != nil {
				break
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	if err != nil {
		// Clients still converge on their next pull; deliver locally so
		// this instance's own clients are not delayed.
		b.log.Warn("postgres notify failed", "err", err, "namespaces", len(batch))
		for ns, origin := range batch {
			b.deliver(ns, origin)
		}
	}
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
	b.mu.Lock()
	h := b.handler
	b.mu.Unlock()
	if h != nil {
		h(ns, origin)
	}
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
		var pokes []poke
		if json.Unmarshal([]byte(n.Payload), &pokes) != nil {
			continue
		}
		for _, p := range pokes {
			if p.NS != "" {
				b.deliver(p.NS, p.Origin)
			}
		}
	}
}

// Close flushes queued notifications and stops listening.
func (b *PostgresBroker) Close() error {
	b.cancel()
	b.wg.Wait()
	return nil
}
