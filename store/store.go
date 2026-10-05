// Package store defines the server-side persistence contract.
//
// Data is partitioned by namespace (one per user or tenant, decided by the
// server's Authenticator). Within a namespace every field of every document is
// a last-writer-wins register. Each successful push bumps the namespace's
// version, and every field it changes is tagged with that version, so a client
// that remembers the last version it saw (its cursor) can fetch exactly what
// changed since.
package store

import (
	"context"

	"github.com/HarshalPatel1972/GoSync/protocol"
)

// PushOutcome reports the effect of a push.
type PushOutcome struct {
	// LastMutationID is the highest mutation ID now acknowledged for the client.
	LastMutationID int64
	// Changed is true when at least one field value changed, i.e. other
	// clients in the namespace have something new to pull.
	Changed bool
}

// Store persists namespaces. Implementations must be safe for concurrent use.
type Store interface {
	// LastMutationID returns the highest mutation ID applied for a client.
	LastMutationID(ctx context.Context, ns, clientID string) (int64, error)

	// Push applies, in one transaction, every mutation whose ID is greater
	// than the client's last applied ID, keeping for each field the value
	// with the greatest HLC. The client's last ID then becomes
	// max(previous, ackUpTo); ackUpTo lets the server acknowledge mutations
	// it rejected without applying them. Mutations must be pre-validated and
	// sorted by ID.
	Push(ctx context.Context, ns, clientID string, muts []protocol.Mutation, ackUpTo int64) (PushOutcome, error)

	// Pull returns changes with version greater than cursor, limited to
	// collections when non-empty. A response is cut only at version
	// boundaries once it exceeds roughly budgetBytes, in which case More is
	// set. Purge records are included only when cursor > 0.
	Pull(ctx context.Context, ns string, cursor int64, collections []string, budgetBytes int) (protocol.PullResult, error)

	Close() error
}

// Compactor is implemented by stores that can purge old tombstones.
type Compactor interface {
	// Compact purges up to limit documents whose deletion HLC is below
	// cutoff, replacing them with purge records (see protocol.DocChange),
	// and returns the namespaces it changed.
	Compact(ctx context.Context, cutoff string, limit int) (purged int, namespaces []string, err error)
}
