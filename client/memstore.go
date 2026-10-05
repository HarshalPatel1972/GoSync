package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"

	"github.com/HarshalPatel1972/GoSync/protocol"
)

// MemoryStore is an in-memory LocalStore, for tests and short-lived native
// clients. State is lost when the process exits.
type MemoryStore struct {
	mu       sync.Mutex
	clientID string
	cursors  map[string]int64
	maxHLC   string
	nextID   int64
	docs     map[string]map[string]*StoredDoc // collection -> id -> doc
	outbox   []protocol.Mutation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{docs: map[string]map[string]*StoredDoc{}, cursors: map[string]int64{}}
}

func (s *MemoryStore) ClientID(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientID == "" {
		b := make([]byte, 12)
		rand.Read(b)
		s.clientID = hex.EncodeToString(b)
	}
	return s.clientID, nil
}

func (s *MemoryStore) Cursor(_ context.Context, scope string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[scope], nil
}

func (s *MemoryStore) MaxHLC(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxHLC, nil
}

func (s *MemoryStore) doc(collection, id string) *StoredDoc {
	coll := s.docs[collection]
	if coll == nil {
		coll = map[string]*StoredDoc{}
		s.docs[collection] = coll
	}
	d := coll[id]
	if d == nil {
		d = &StoredDoc{Collection: collection, ID: id, Fields: map[string]protocol.FieldValue{}}
		coll[id] = d
	}
	return d
}

func (s *MemoryStore) Write(_ context.Context, collection, id string, fields map[string]json.RawMessage, hlc string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	incoming := make(map[string]protocol.FieldValue, len(fields))
	for k, v := range fields {
		incoming[k] = protocol.FieldValue{Value: v, HLC: hlc}
	}
	MergeFields(s.doc(collection, id).Fields, incoming)
	s.maxHLC = max(s.maxHLC, hlc)
	s.nextID++
	s.outbox = append(s.outbox, protocol.Mutation{ID: s.nextID, Collection: collection, Doc: id, HLC: hlc, Fields: fields})
	return s.nextID, nil
}

func (s *MemoryStore) ApplyRemote(_ context.Context, changes []protocol.DocChange, scope string, cursor int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range changes {
		d := s.doc(ch.Collection, ch.Doc)
		ApplyChange(d.Fields, ch)
		if len(d.Fields) == 0 {
			delete(s.docs[ch.Collection], ch.Doc)
		}
		for _, f := range ch.Fields {
			s.maxHLC = max(s.maxHLC, f.HLC)
		}
	}
	s.cursors[scope] = cursor
	return nil
}

func (s *MemoryStore) Pending(_ context.Context, limit int) ([]protocol.Mutation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := min(limit, len(s.outbox))
	return append([]protocol.Mutation(nil), s.outbox[:n]...), nil
}

func (s *MemoryStore) Ack(_ context.Context, upTo int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := 0
	for i < len(s.outbox) && s.outbox[i].ID <= upTo {
		i++
	}
	s.outbox = s.outbox[i:]
	return nil
}

func (s *MemoryStore) Get(_ context.Context, collection, id string) (StoredDoc, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[collection][id]
	if !ok {
		return StoredDoc{}, false, nil
	}
	return cloneDoc(d), true, nil
}

func (s *MemoryStore) List(_ context.Context, collection string) ([]StoredDoc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredDoc, 0, len(s.docs[collection]))
	for _, d := range s.docs[collection] {
		out = append(out, cloneDoc(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func cloneDoc(d *StoredDoc) StoredDoc {
	f := make(map[string]protocol.FieldValue, len(d.Fields))
	for k, v := range d.Fields {
		f[k] = v
	}
	return StoredDoc{Collection: d.Collection, ID: d.ID, Fields: f}
}
