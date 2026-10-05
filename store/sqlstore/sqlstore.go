// Package sqlstore implements store.Store on SQLite or PostgreSQL.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	_ "modernc.org/sqlite"             // registers the "sqlite" driver

	"github.com/HarshalPatel1972/GoSync/protocol"
	"github.com/HarshalPatel1972/GoSync/store"
)

type dialect int

const (
	sqlite dialect = iota
	postgres
)

// Store is a SQL-backed store.Store.
type Store struct {
	db      *sql.DB // writes (and reads on PostgreSQL)
	rdb     *sql.DB // reads; a separate pool on SQLite so pulls never queue behind writes
	dialect dialect

	// SQLite group commit (nil on PostgreSQL).
	writes  chan *pushReq
	stop    chan struct{}
	stmts   sync.Map // stmtKey -> *sql.Stmt
	upserts sync.Map // field count -> upsert statement text

	writerDone       chan struct{}
	checkpointerDone chan struct{}
	closeOnce        sync.Once
}

var _ store.Store = (*Store)(nil)

// IsPostgres reports whether dsn names a PostgreSQL database.
func IsPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// Open connects to dsn and migrates the schema to the latest version. A
// postgres:// or postgresql:// URL selects PostgreSQL; anything else is a
// SQLite file path (or ":memory:").
func Open(ctx context.Context, dsn string) (*Store, error) {
	return OpenWith(ctx, dsn, Options{})
}

// Options tunes a Store.
type Options struct {
	// MaxConns is the PostgreSQL connection pool size per server instance
	// (default 25). Keep instances × MaxConns below max_connections.
	MaxConns int
}

// OpenWith is Open with options.
func OpenWith(ctx context.Context, dsn string, opts Options) (*Store, error) {
	if opts.MaxConns <= 0 {
		opts.MaxConns = 25
	}
	s := &Store{}
	var err error
	if IsPostgres(dsn) {
		s.dialect = postgres
		s.db, err = sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		s.db.SetMaxOpenConns(opts.MaxConns)
		s.db.SetMaxIdleConns(opts.MaxConns)
		s.db.SetConnMaxIdleTime(5 * time.Minute)
		s.rdb = s.db
	} else {
		s.dialect = sqlite
		// Checkpoints run in the background (checkpointLoop) instead of
		// inside whichever commit crosses the WAL threshold, which would
		// stall that commit and everything queued behind it.
		s.db, err = sql.Open("sqlite", sqliteDSN(dsn)+"&_pragma=wal_autocheckpoint(0)")
		if err != nil {
			return nil, err
		}
		// SQLite allows one writer at a time; a single connection serialises
		// transactions without SQLITE_BUSY upgrade failures.
		s.db.SetMaxOpenConns(1)
		s.rdb = s.db
		if dsn != ":memory:" { // each ":memory:" connection is its own database
			// WAL mode lets readers run concurrently with the writer, each
			// transaction on a consistent snapshot.
			s.rdb, err = sql.Open("sqlite", sqliteDSN(dsn)+"&_pragma=query_only(1)")
			if err != nil {
				s.db.Close()
				return nil, err
			}
			s.rdb.SetMaxOpenConns(8)
		}
	}
	if err := s.db.PingContext(ctx); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if s.dialect == sqlite {
		s.writes = make(chan *pushReq, maxGroupCommit)
		s.stop = make(chan struct{})
		s.writerDone = make(chan struct{})
		go s.groupCommitLoop()
		if s.rdb != s.db {
			s.checkpointerDone = make(chan struct{})
			go s.checkpointLoop(dsn)
		}
	}
	return s, nil
}

func sqliteDSN(path string) string {
	pragmas := "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	if path == ":memory:" {
		return "file::memory:?" + pragmas
	}
	if strings.Contains(path, "?") {
		return path + "&" + pragmas
	}
	return "file:" + path + "?" + pragmas
}

// migrations are applied in order, each exactly once. Never edit a released
// migration; append a new one.
func (s *Store) migrations() []string {
	// HLC strings must compare bytewise; PostgreSQL's default collation may not.
	hlcType := "TEXT"
	if s.dialect == postgres {
		hlcType = `TEXT COLLATE "C"`
	}
	return []string{
		// 1: core tables.
		`CREATE TABLE IF NOT EXISTS gosync_spaces (
			ns      TEXT PRIMARY KEY,
			version BIGINT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS gosync_clients (
			ns               TEXT NOT NULL,
			client_id        TEXT NOT NULL,
			last_mutation_id BIGINT NOT NULL,
			updated_at       BIGINT NOT NULL,
			PRIMARY KEY (ns, client_id)
		);
		CREATE TABLE IF NOT EXISTS gosync_fields (
			ns      TEXT NOT NULL,
			coll    TEXT NOT NULL,
			doc     TEXT NOT NULL,
			field   TEXT NOT NULL,
			value   TEXT NOT NULL,
			hlc     ` + hlcType + ` NOT NULL,
			version BIGINT NOT NULL,
			PRIMARY KEY (ns, coll, doc, field)
		);
		CREATE INDEX IF NOT EXISTS gosync_fields_ns_version ON gosync_fields (ns, version)`,

		// 2: tombstone compaction. A purged document keeps only this row; it
		// syncs to clients like any other change and fences off writes
		// older than the deletion.
		`CREATE TABLE gosync_purged (
			ns      TEXT NOT NULL,
			coll    TEXT NOT NULL,
			doc     TEXT NOT NULL,
			hlc     ` + hlcType + ` NOT NULL,
			version BIGINT NOT NULL,
			PRIMARY KEY (ns, coll, doc)
		);
		CREATE INDEX gosync_purged_ns_version ON gosync_purged (ns, version);
		CREATE INDEX gosync_fields_tombstones ON gosync_fields (hlc)
			WHERE field = '_deleted' AND value = 'true'`,
	}
}

// migrate applies pending migrations in one transaction. On PostgreSQL an
// advisory lock stops concurrently starting instances from racing.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.dialect == postgres {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7471001)`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS gosync_schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at BIGINT NOT NULL
	)`); err != nil {
		return err
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM gosync_schema_migrations`).Scan(&current); err != nil {
		return err
	}
	all := s.migrations()
	if current > len(all) {
		return fmt.Errorf("database schema version %d is newer than this server (%d); upgrade the server", current, len(all))
	}
	for v := current + 1; v <= len(all); v++ {
		for _, stmt := range strings.Split(all[v-1], ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %d: %w", v, err)
			}
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO gosync_schema_migrations (version, applied_at) VALUES (?, ?)`), v, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SchemaVersion returns the applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM gosync_schema_migrations`).Scan(&v)
	return v, err
}

type stmtKey struct {
	db    *sql.DB
	query string
}

// cachedStmt returns the prepared statement for query on db, or nil if it
// is not prepared yet. Preparation happens in the background: preparing
// needs a free connection, and SQLite's single writer connection is held by
// the very transaction asking. Re-parsing SQL on every execution was the
// largest CPU cost under load.
func (s *Store) cachedStmt(db *sql.DB, query string) *sql.Stmt {
	key := stmtKey{db, query}
	if v, ok := s.stmts.Load(key); ok {
		st, _ := v.(*sql.Stmt)
		return st // nil while preparing
	}
	if _, loaded := s.stmts.LoadOrStore(key, struct{}{}); !loaded {
		go func() {
			st, err := db.PrepareContext(context.Background(), query)
			if err != nil {
				s.stmts.Delete(key) // retry on next use
				return
			}
			s.stmts.Store(key, st)
		}()
	}
	return nil
}

// txExec, txQueryRow and txQuery run query inside tx (which belongs to db),
// as a prepared statement once one is available.
func (s *Store) txExec(ctx context.Context, tx *sql.Tx, db *sql.DB, query string, args ...any) (sql.Result, error) {
	if st := s.cachedStmt(db, query); st != nil {
		return tx.StmtContext(ctx, st).ExecContext(ctx, args...)
	}
	return tx.ExecContext(ctx, query, args...)
}

func (s *Store) txQueryRow(ctx context.Context, tx *sql.Tx, db *sql.DB, query string, args ...any) *sql.Row {
	if st := s.cachedStmt(db, query); st != nil {
		return tx.StmtContext(ctx, st).QueryRowContext(ctx, args...)
	}
	return tx.QueryRowContext(ctx, query, args...)
}

func (s *Store) txQuery(ctx context.Context, tx *sql.Tx, db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	if st := s.cachedStmt(db, query); st != nil {
		return tx.StmtContext(ctx, st).QueryContext(ctx, args...)
	}
	return tx.QueryContext(ctx, query, args...)
}

// DB exposes the underlying handle (used by tests and health checks).
func (s *Store) DB() *sql.DB { return s.db }

// Close stops the group-commit writer and closes the database.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
			<-s.writerDone
			if s.checkpointerDone != nil {
				<-s.checkpointerDone
			}
		}
	})
	s.stmts.Range(func(_, v any) bool {
		if st, ok := v.(*sql.Stmt); ok {
			st.Close()
		}
		return true
	})
	if s.rdb != s.db {
		s.rdb.Close()
	}
	return s.db.Close()
}

// Ping checks database connectivity (used by the server's /readyz).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// q rewrites '?' placeholders to $n for PostgreSQL.
func (s *Store) q(query string) string {
	if s.dialect != postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

func (s *Store) LastMutationID(ctx context.Context, ns, clientID string) (int64, error) {
	var id int64
	err := s.rdb.QueryRowContext(ctx, s.q(`SELECT last_mutation_id FROM gosync_clients WHERE ns = ? AND client_id = ?`), ns, clientID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// bumpVersion increments a namespace's version. The row lock it takes
// serialises writers within the namespace, so versions commit in order and a
// cursor can never skip over a slower concurrent transaction.
//
// Versions are at least the current time in microseconds, so they keep
// increasing even after the database is restored from a backup: otherwise a
// client whose cursor came from the lost timeline would skip the versions
// re-issued after the restore. (2^53 µs, the largest exact cursor in
// JavaScript, is in the year 2255.)
func (s *Store) bumpVersion(ctx context.Context, tx *sql.Tx, ns string) (int64, error) {
	greatest := "MAX" // SQLite's scalar max
	if s.dialect == postgres {
		greatest = "GREATEST"
	}
	var version int64
	err := s.txQueryRow(ctx, tx, s.db, s.q(`
		INSERT INTO gosync_spaces (ns, version) VALUES (?, ?)
		ON CONFLICT (ns) DO UPDATE SET version = `+greatest+`(gosync_spaces.version + 1, excluded.version)
		RETURNING version`), ns, time.Now().UnixMicro()).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("bump version: %w", err)
	}
	return version, nil
}

func (s *Store) Push(ctx context.Context, ns, clientID string, muts []protocol.Mutation, ackUpTo int64) (store.PushOutcome, error) {
	if s.writes != nil {
		return s.enqueuePush(ctx, &pushReq{ctx: ctx, ns: ns, clientID: clientID, muts: muts, ackUpTo: ackUpTo})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.PushOutcome{}, err
	}
	defer tx.Rollback()
	out, keep, err := s.applyPush(ctx, tx, ns, clientID, muts, ackUpTo)
	if err != nil || !keep {
		return out, err
	}
	return out, tx.Commit()
}

// Group commit (SQLite). SQLite has a single writer, so committing each push
// separately caps throughput at a few hundred pushes per second. Instead one
// goroutine collects the pushes waiting at that moment and commits them in
// one transaction, each inside its own savepoint so one failure or no-op
// does not affect the others.

const maxGroupCommit = 128

type pushReq struct {
	ctx      context.Context
	ns       string
	clientID string
	muts     []protocol.Mutation
	ackUpTo  int64
	done     chan pushResp
}

type pushResp struct {
	out store.PushOutcome
	err error
}

var errClosed = errors.New("sqlstore: closed")

func (s *Store) enqueuePush(ctx context.Context, r *pushReq) (store.PushOutcome, error) {
	r.done = make(chan pushResp, 1)
	select {
	case s.writes <- r:
	case <-s.stop:
		return store.PushOutcome{}, errClosed
	case <-ctx.Done():
		return store.PushOutcome{}, ctx.Err()
	}
	select {
	case res := <-r.done:
		return res.out, res.err
	case <-ctx.Done():
		// The push may still commit; the client will learn its outcome from
		// lastMutationId on reconnect.
		return store.PushOutcome{}, ctx.Err()
	}
}

func (s *Store) groupCommitLoop() {
	defer close(s.writerDone)
	for {
		var first *pushReq
		select {
		case first = <-s.writes:
		case <-s.stop:
			return
		}
		batch := []*pushReq{first}
	drain:
		for len(batch) < maxGroupCommit {
			select {
			case r := <-s.writes:
				batch = append(batch, r)
			default:
				break drain
			}
		}
		s.commitBatch(batch)
	}
}

// checkpointLoop copies the WAL into the database file every second on its
// own connection. PASSIVE never blocks readers or the writer.
func (s *Store) checkpointLoop(dsn string) {
	defer close(s.checkpointerDone)
	db, err := sql.Open("sqlite", sqliteDSN(dsn))
	if err != nil {
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			db.Exec("PRAGMA wal_checkpoint(TRUNCATE)") // leave a small WAL behind
			return
		case <-t.C:
			db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
		}
	}
}

func (s *Store) commitBatch(batch []*pushReq) {
	results := make([]pushResp, len(batch))
	fail := func(err error) {
		for _, r := range batch {
			r.done <- pushResp{err: err}
		}
	}
	// The batch runs on its own context: one caller giving up must not
	// abort everyone else's writes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		fail(err)
		return
	}
	defer tx.Rollback()
	for i, r := range batch {
		if err := r.ctx.Err(); err != nil {
			results[i].err = err
			continue
		}
		if _, err := s.txExec(ctx, tx, s.db, "SAVEPOINT push"); err != nil {
			fail(err)
			return
		}
		out, keep, err := s.applyPush(ctx, tx, r.ns, r.clientID, r.muts, r.ackUpTo)
		results[i] = pushResp{out: out, err: err}
		if err != nil || !keep {
			if _, err := s.txExec(ctx, tx, s.db, "ROLLBACK TO push"); err != nil {
				fail(err)
				return
			}
		}
		if _, err := s.txExec(ctx, tx, s.db, "RELEASE push"); err != nil {
			fail(err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(err)
		return
	}
	for i, r := range batch {
		r.done <- results[i]
	}
}

// applyPush applies one push inside tx. keep is false when nothing was new,
// in which case the caller must roll back (undoing the version bump).
func (s *Store) applyPush(ctx context.Context, tx *sql.Tx, ns, clientID string, muts []protocol.Mutation, ackUpTo int64) (out store.PushOutcome, keep bool, err error) {
	version, err := s.bumpVersion(ctx, tx, ns)
	if err != nil {
		return out, false, err
	}

	var last int64
	err = s.txQueryRow(ctx, tx, s.db, s.q(`SELECT last_mutation_id FROM gosync_clients WHERE ns = ? AND client_id = ?`), ns, clientID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, false, err
	}
	err = nil

	// One query finds every compacted document this push touches.
	purged, err := s.purgedHLCs(ctx, tx, ns, muts, last)
	if err != nil {
		return out, false, err
	}

	newLast := last
	for _, m := range muts {
		if m.ID <= last {
			continue // already applied: pushes are idempotent
		}
		newLast = max(newLast, m.ID)

		// Writes older than a compacted deletion lost to it; ignore them so
		// the purged document cannot be partially resurrected.
		if at, ok := purged[docKey{m.Collection, m.Doc}]; ok && m.HLC <= at {
			continue
		}

		// All of a mutation's fields in one statement.
		names := make([]string, 0, len(m.Fields))
		for name := range m.Fields {
			names = append(names, name)
		}
		sort.Strings(names) // stable statement text for the prepared-statement cache
		args := make([]any, 0, len(names)*7)
		for _, name := range names {
			args = append(args, ns, m.Collection, m.Doc, name, string(m.Fields[name]), m.HLC, version)
		}
		res, err := s.txExec(ctx, tx, s.db, s.upsertFields(len(names)), args...)
		if err != nil {
			return out, false, fmt.Errorf("apply mutation %d: %w", m.ID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out.Changed = true
		}
	}
	newLast = max(newLast, ackUpTo)

	if newLast == last {
		return store.PushOutcome{LastMutationID: last}, false, nil
	}

	_, err = s.txExec(ctx, tx, s.db, s.q(`
		INSERT INTO gosync_clients (ns, client_id, last_mutation_id, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (ns, client_id) DO UPDATE SET last_mutation_id = excluded.last_mutation_id, updated_at = excluded.updated_at`),
		ns, clientID, newLast, time.Now().UnixMilli())
	if err != nil {
		return out, false, err
	}
	out.LastMutationID = newLast
	return out, true, nil
}

type docKey struct{ coll, doc string }

// upsertFields returns a statement writing n field rows, each kept only if
// its HLC is newer than the stored one.
func (s *Store) upsertFields(n int) string {
	if q, ok := s.upserts.Load(n); ok {
		return q.(string)
	}
	q := s.q(`INSERT INTO gosync_fields (ns, coll, doc, field, value, hlc, version) VALUES (?, ?, ?, ?, ?, ?, ?)` +
		strings.Repeat(`, (?, ?, ?, ?, ?, ?, ?)`, n-1) + `
		ON CONFLICT (ns, coll, doc, field) DO UPDATE
		SET value = excluded.value, hlc = excluded.hlc, version = excluded.version
		WHERE gosync_fields.hlc < excluded.hlc`)
	s.upserts.Store(n, q)
	return q
}

// purgedHLCs returns the purge HLC of every compacted document among the
// new mutations.
func (s *Store) purgedHLCs(ctx context.Context, tx *sql.Tx, ns string, muts []protocol.Mutation, last int64) (map[docKey]string, error) {
	seen := map[docKey]bool{}
	args := []any{ns}
	for _, m := range muts {
		k := docKey{m.Collection, m.Doc}
		if m.ID > last && !seen[k] {
			seen[k] = true
			args = append(args, m.Collection, m.Doc)
		}
	}
	out := map[docKey]string{}
	if len(seen) == 0 {
		return out, nil
	}
	q := s.q(`SELECT coll, doc, hlc FROM gosync_purged WHERE ns = ? AND (coll, doc) IN ((?, ?)` +
		strings.Repeat(`, (?, ?)`, len(seen)-1) + `)`)
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k docKey
		var at string
		if err := rows.Scan(&k.coll, &k.doc, &at); err != nil {
			return nil, err
		}
		out[k] = at
	}
	return out, rows.Err()
}

func (s *Store) Doc(ctx context.Context, ns, collection, id string) (protocol.DocChange, error) {
	ch := protocol.DocChange{Collection: collection, Doc: id, Fields: map[string]protocol.FieldValue{}}
	rows, err := s.rdb.QueryContext(ctx, s.q(`SELECT field, value, hlc FROM gosync_fields WHERE ns = ? AND coll = ? AND doc = ?`), ns, collection, id)
	if err != nil {
		return ch, err
	}
	defer rows.Close()
	for rows.Next() {
		var f, v, t string
		if err := rows.Scan(&f, &v, &t); err != nil {
			return ch, err
		}
		ch.Fields[f] = protocol.FieldValue{Value: json.RawMessage(v), HLC: t}
	}
	return ch, rows.Err()
}

// collFilter returns " AND coll IN (?, ...)" and its arguments.
func collFilter(collections []string) (string, []any) {
	if len(collections) == 0 {
		return "", nil
	}
	args := make([]any, len(collections))
	for i, c := range collections {
		args[i] = c
	}
	return " AND coll IN (?" + strings.Repeat(", ?", len(collections)-1) + ")", args
}

func (s *Store) Pull(ctx context.Context, ns string, cursor int64, collections []string, budgetBytes int) (res protocol.PullResult, err error) {
	opts := &sql.TxOptions{}
	if s.dialect == postgres {
		// One snapshot for all queries.
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	tx, err := s.rdb.BeginTx(ctx, opts)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	var version int64
	err = s.txQueryRow(ctx, tx, s.rdb, s.q(`SELECT version FROM gosync_spaces WHERE ns = ?`), ns).Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	if cursor > version || cursor < 0 {
		// The client has seen versions this server never issued (e.g. the
		// database was restored); resend everything.
		cursor = 0
	}
	res.Cursor = version
	res.Changes = []protocol.DocChange{}
	if cursor == version {
		return res, nil
	}

	filter, filterArgs := collFilter(collections)
	args := append([]any{ns, cursor, version}, filterArgs...)
	rows, err := s.txQuery(ctx, tx, s.rdb, s.q(`
		SELECT coll, doc, field, value, hlc, version FROM gosync_fields
		WHERE ns = ? AND version > ? AND version <= ?`+filter+`
		ORDER BY version, coll, doc`), args...)
	if err != nil {
		return res, err
	}

	type key struct{ c, d string }
	index := map[key]int{}
	change := func(c, d string) *protocol.DocChange {
		k := key{c, d}
		i, ok := index[k]
		if !ok {
			i = len(res.Changes)
			index[k] = i
			res.Changes = append(res.Changes, protocol.DocChange{Collection: c, Doc: d, Fields: map[string]protocol.FieldValue{}})
		}
		return &res.Changes[i]
	}
	size := 0
	prevVersion := cursor
	for rows.Next() {
		var c, d, f, v, t string
		var ver int64
		if err := rows.Scan(&c, &d, &f, &v, &t, &ver); err != nil {
			rows.Close()
			return res, err
		}
		if ver != prevVersion && size >= budgetBytes {
			// Stop at a version boundary so the cursor stays exact.
			res.Cursor = prevVersion
			res.More = true
			break
		}
		prevVersion = ver
		change(c, d).Fields[f] = protocol.FieldValue{Value: json.RawMessage(v), HLC: t}
		size += len(c) + len(d) + len(f) + len(v) + len(t) + 16
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	// Compactions in the same version range. Fresh clients (cursor 0)
	// never held the purged documents, so they skip these.
	if cursor > 0 {
		args := append([]any{ns, cursor, res.Cursor}, filterArgs...)
		rows, err := s.txQuery(ctx, tx, s.rdb, s.q(`
			SELECT coll, doc, hlc FROM gosync_purged
			WHERE ns = ? AND version > ? AND version <= ?`+filter), args...)
		if err != nil {
			return res, err
		}
		defer rows.Close()
		for rows.Next() {
			var c, d, t string
			if err := rows.Scan(&c, &d, &t); err != nil {
				return res, err
			}
			change(c, d).Purged = t
		}
		if err := rows.Err(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Compact purges documents deleted before cutoff (an HLC string): their
// field rows are removed and replaced by a small purge record. It processes
// at most limit documents and returns the namespaces it changed.
func (s *Store) Compact(ctx context.Context, cutoff string, limit int) (purged int, namespaces []string, err error) {
	// Documents with a field newer than their deletion are never purged
	// (see compactNamespace); excluding them here keeps them from filling
	// every batch and starving the rest.
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT f.ns, f.coll, f.doc, f.hlc FROM gosync_fields f
		WHERE f.field = '_deleted' AND f.value = 'true' AND f.hlc < ?
		  AND NOT EXISTS (
			SELECT 1 FROM gosync_fields g
			WHERE g.ns = f.ns AND g.coll = f.coll AND g.doc = f.doc AND g.hlc > f.hlc)
		LIMIT ?`), cutoff, limit)
	if err != nil {
		return 0, nil, err
	}
	byNS := map[string][]tomb{}
	var order []string
	for rows.Next() {
		var ns string
		var t tomb
		if err := rows.Scan(&ns, &t.coll, &t.doc, &t.hlc); err != nil {
			rows.Close()
			return 0, nil, err
		}
		if _, ok := byNS[ns]; !ok {
			order = append(order, ns)
		}
		byNS[ns] = append(byNS[ns], t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	for _, ns := range order {
		n, err := s.compactNamespace(ctx, ns, byNS[ns])
		if err != nil {
			return purged, namespaces, err
		}
		if n > 0 {
			purged += n
			namespaces = append(namespaces, ns)
		}
	}
	return purged, namespaces, nil
}

type tomb struct{ coll, doc, hlc string }

func (s *Store) compactNamespace(ctx context.Context, ns string, tombs []tomb) (n int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	version, err := s.bumpVersion(ctx, tx, ns)
	if err != nil {
		return 0, err
	}
	for _, t := range tombs {
		// Re-check under the namespace lock: the document may have been
		// revived (or deleted again later) since we scanned.
		var cur string
		err := tx.QueryRowContext(ctx, s.q(`SELECT hlc FROM gosync_fields
			WHERE ns = ? AND coll = ? AND doc = ? AND field = '_deleted' AND value = 'true'`), ns, t.coll, t.doc).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && cur != t.hlc) {
			continue
		}
		if err != nil {
			return 0, err
		}
		// A field written after the deletion (possible from raw protocol
		// clients) must survive, so such documents are left alone.
		var newer int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM gosync_fields
			WHERE ns = ? AND coll = ? AND doc = ? AND hlc > ?`), ns, t.coll, t.doc, t.hlc).Scan(&newer); err != nil {
			return 0, err
		}
		if newer > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM gosync_fields WHERE ns = ? AND coll = ? AND doc = ?`), ns, t.coll, t.doc); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO gosync_purged (ns, coll, doc, hlc, version) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (ns, coll, doc) DO UPDATE SET hlc = excluded.hlc, version = excluded.version`),
			ns, t.coll, t.doc, t.hlc, version); err != nil {
			return 0, err
		}
		n++
	}
	if n == 0 {
		return 0, nil // rollback undoes the version bump
	}
	return n, tx.Commit()
}
