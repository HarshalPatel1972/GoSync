// Package sqlstore implements store.Store on SQLite or PostgreSQL.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	db      *sql.DB
	dialect dialect
}

var _ store.Store = (*Store)(nil)

// IsPostgres reports whether dsn names a PostgreSQL database.
func IsPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// Open connects to dsn and creates the schema if needed. A postgres:// or
// postgresql:// URL selects PostgreSQL; anything else is a SQLite file path
// (or ":memory:").
func Open(ctx context.Context, dsn string) (*Store, error) {
	s := &Store{}
	var err error
	if IsPostgres(dsn) {
		s.dialect = postgres
		s.db, err = sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		s.db.SetMaxOpenConns(20)
		s.db.SetConnMaxIdleTime(5 * time.Minute)
	} else {
		s.dialect = sqlite
		s.db, err = sql.Open("sqlite", sqliteDSN(dsn))
		if err != nil {
			return nil, err
		}
		// SQLite allows one writer at a time; a single connection serialises
		// transactions without SQLITE_BUSY upgrade failures and keeps
		// ":memory:" databases coherent.
		s.db.SetMaxOpenConns(1)
	}
	if err := s.db.PingContext(ctx); err != nil {
		s.db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		s.db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
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

func (s *Store) migrate(ctx context.Context) error {
	// HLC strings must compare bytewise; PostgreSQL's default collation may not.
	hlcType := "TEXT"
	if s.dialect == postgres {
		hlcType = `TEXT COLLATE "C"`
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS gosync_spaces (
			ns      TEXT PRIMARY KEY,
			version BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS gosync_clients (
			ns               TEXT NOT NULL,
			client_id        TEXT NOT NULL,
			last_mutation_id BIGINT NOT NULL,
			updated_at       BIGINT NOT NULL,
			PRIMARY KEY (ns, client_id)
		)`,
		`CREATE TABLE IF NOT EXISTS gosync_fields (
			ns      TEXT NOT NULL,
			coll    TEXT NOT NULL,
			doc     TEXT NOT NULL,
			field   TEXT NOT NULL,
			value   TEXT NOT NULL,
			hlc     ` + hlcType + ` NOT NULL,
			version BIGINT NOT NULL,
			PRIMARY KEY (ns, coll, doc, field)
		)`,
		`CREATE INDEX IF NOT EXISTS gosync_fields_ns_version ON gosync_fields (ns, version)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// DB exposes the underlying handle (used by tests and health checks).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

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
	err := s.db.QueryRowContext(ctx, s.q(`SELECT last_mutation_id FROM gosync_clients WHERE ns = ? AND client_id = ?`), ns, clientID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (s *Store) Push(ctx context.Context, ns, clientID string, muts []protocol.Mutation, ackUpTo int64) (out store.PushOutcome, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Bumping the namespace version first takes a row lock that serialises
	// pushes within the namespace, so versions commit in order and a cursor
	// can never skip over a slower concurrent transaction.
	var version int64
	err = tx.QueryRowContext(ctx, s.q(`
		INSERT INTO gosync_spaces (ns, version) VALUES (?, 1)
		ON CONFLICT (ns) DO UPDATE SET version = gosync_spaces.version + 1
		RETURNING version`), ns).Scan(&version)
	if err != nil {
		return out, fmt.Errorf("bump version: %w", err)
	}

	var last int64
	err = tx.QueryRowContext(ctx, s.q(`SELECT last_mutation_id FROM gosync_clients WHERE ns = ? AND client_id = ?`), ns, clientID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	err = nil

	upsert := s.q(`
		INSERT INTO gosync_fields (ns, coll, doc, field, value, hlc, version)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (ns, coll, doc, field) DO UPDATE
		SET value = excluded.value, hlc = excluded.hlc, version = excluded.version
		WHERE gosync_fields.hlc < excluded.hlc`)

	newLast := last
	for _, m := range muts {
		if m.ID <= last {
			continue // already applied: pushes are idempotent
		}
		for field, value := range m.Fields {
			res, err := tx.ExecContext(ctx, upsert, ns, m.Collection, m.Doc, field, string(value), m.HLC, version)
			if err != nil {
				return out, fmt.Errorf("apply mutation %d: %w", m.ID, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				out.Changed = true
			}
		}
		newLast = max(newLast, m.ID)
	}
	newLast = max(newLast, ackUpTo)

	if newLast == last {
		// Nothing new: undo the version bump.
		tx.Rollback()
		return store.PushOutcome{LastMutationID: last}, nil
	}

	_, err = tx.ExecContext(ctx, s.q(`
		INSERT INTO gosync_clients (ns, client_id, last_mutation_id, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (ns, client_id) DO UPDATE SET last_mutation_id = excluded.last_mutation_id, updated_at = excluded.updated_at`),
		ns, clientID, newLast, time.Now().UnixMilli())
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out.LastMutationID = newLast
	return out, nil
}

func (s *Store) Pull(ctx context.Context, ns string, cursor int64, budgetBytes int) (res protocol.PullResult, err error) {
	opts := &sql.TxOptions{}
	if s.dialect == postgres {
		// One snapshot for both queries.
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	var version int64
	err = tx.QueryRowContext(ctx, s.q(`SELECT version FROM gosync_spaces WHERE ns = ?`), ns).Scan(&version)
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

	rows, err := tx.QueryContext(ctx, s.q(`
		SELECT coll, doc, field, value, hlc, version FROM gosync_fields
		WHERE ns = ? AND version > ? AND version <= ?
		ORDER BY version, coll, doc`), ns, cursor, version)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	type key struct{ c, d string }
	index := map[key]int{}
	size := 0
	var prevVersion int64 = cursor
	for rows.Next() {
		var c, d, f, v, t string
		var ver int64
		if err := rows.Scan(&c, &d, &f, &v, &t, &ver); err != nil {
			return res, err
		}
		if ver != prevVersion && size >= budgetBytes {
			// Stop at a version boundary so the cursor stays exact.
			res.Cursor = prevVersion
			res.More = true
			break
		}
		prevVersion = ver
		k := key{c, d}
		i, ok := index[k]
		if !ok {
			i = len(res.Changes)
			index[k] = i
			res.Changes = append(res.Changes, protocol.DocChange{Collection: c, Doc: d, Fields: map[string]protocol.FieldValue{}})
		}
		res.Changes[i].Fields[f] = protocol.FieldValue{Value: json.RawMessage(v), HLC: t}
		size += len(c) + len(d) + len(f) + len(v) + len(t) + 16
	}
	return res, rows.Err()
}

// Ping checks database connectivity (used by the server's /readyz).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
