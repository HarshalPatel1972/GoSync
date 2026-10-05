package client

import (
	"context"
	"encoding/json"

	"github.com/HarshalPatel1972/GoSync/protocol"
)

// StoredDoc is a document as kept by a LocalStore: every field with its HLC.
type StoredDoc struct {
	Collection string
	ID         string
	Fields     map[string]protocol.FieldValue
}

// Deleted reports whether the document's latest state is deleted.
func (d StoredDoc) Deleted() bool {
	f, ok := d.Fields[protocol.FieldDeleted]
	return ok && string(f.Value) == "true"
}

// LocalStore is the client's durable state: documents, the outbox of
// unacknowledged mutations, and sync metadata. Each method must be atomic,
// because the app can be closed at any moment.
type LocalStore interface {
	// ClientID returns this replica's stable ID, creating it on first use.
	ClientID(ctx context.Context) (string, error)
	// Cursor returns the last server version applied.
	Cursor(ctx context.Context) (int64, error)
	// MaxHLC returns the greatest HLC ever stored ("" if none), so the clock
	// stays monotonic across restarts even if the device clock moves back.
	MaxHLC(ctx context.Context) (string, error)

	// Write merges fields stamped with hlc into a document (LWW per field)
	// and appends the mutation to the outbox, returning its ID. IDs must be
	// strictly increasing.
	Write(ctx context.Context, collection, id string, fields map[string]json.RawMessage, hlc string) (int64, error)
	// ApplyRemote merges server changes (LWW per field) and saves cursor.
	ApplyRemote(ctx context.Context, changes []protocol.DocChange, cursor int64) error
	// Pending returns up to limit outbox mutations in ascending ID order.
	Pending(ctx context.Context, limit int) ([]protocol.Mutation, error)
	// Ack removes outbox mutations with ID <= upTo.
	Ack(ctx context.Context, upTo int64) error

	Get(ctx context.Context, collection, id string) (StoredDoc, bool, error)
	List(ctx context.Context, collection string) ([]StoredDoc, error)
}

// MergeFields applies last-writer-wins: each incoming field replaces the
// existing one only if its HLC is greater. HLC strings sort in timestamp
// order. It reports whether anything changed.
func MergeFields(dst map[string]protocol.FieldValue, incoming map[string]protocol.FieldValue) bool {
	changed := false
	for name, in := range incoming {
		if cur, ok := dst[name]; ok && cur.HLC >= in.HLC {
			continue
		}
		dst[name] = in
		changed = true
	}
	return changed
}
