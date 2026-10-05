//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"syscall/js"

	"github.com/HarshalPatel1972/GoSync/client"
	"github.com/HarshalPatel1972/GoSync/protocol"
)

// idbStore implements client.LocalStore on the IndexedDB bridge in gosync.js.
// The bridge performs each operation in a single IndexedDB transaction
// (IndexedDB transactions cannot span Go/JS round trips), applying the same
// per-field last-writer-wins rule as client.MergeFields. Field values cross
// the boundary and are stored as raw JSON strings, so JavaScript never
// re-encodes them (which would, for example, corrupt integers above 2^53).
type idbStore struct {
	bridge js.Value
}

var _ client.LocalStore = idbStore{}

func (s idbStore) call(ctx context.Context, method string, args ...any) (js.Value, error) {
	return await(ctx, s.bridge.Call(method, args...))
}

func (s idbStore) ClientID(ctx context.Context) (string, error) {
	v, err := s.call(ctx, "clientId")
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

func (s idbStore) Cursor(ctx context.Context, scope string) (int64, error) {
	v, err := s.call(ctx, "cursor", scope)
	if err != nil {
		return 0, err
	}
	return int64(v.Float()), nil
}

func (s idbStore) MaxHLC(ctx context.Context) (string, error) {
	v, err := s.call(ctx, "maxHlc")
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

func (s idbStore) Write(ctx context.Context, collection, id string, fields map[string]json.RawMessage, hlc string) (int64, error) {
	raw := make(map[string]string, len(fields))
	for k, v := range fields {
		raw[k] = string(v)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return 0, err
	}
	v, err := s.call(ctx, "write", collection, id, string(b), hlc)
	if err != nil {
		return 0, err
	}
	return int64(v.Float()), nil
}

func (s idbStore) ApplyRemote(ctx context.Context, changes []protocol.DocChange, scope string, cursor int64) error {
	docs := make([]bridgeDoc, len(changes))
	for i, ch := range changes {
		f := make(map[string]bridgeField, len(ch.Fields))
		for k, v := range ch.Fields {
			f[k] = bridgeField{V: string(v.Value), T: v.HLC}
		}
		docs[i] = bridgeDoc{C: ch.Collection, D: ch.Doc, F: f, Purged: ch.Purged}
	}
	b, err := json.Marshal(docs)
	if err != nil {
		return err
	}
	_, err = s.call(ctx, "applyRemote", string(b), scope, float64(cursor))
	return err
}

func (s idbStore) Pending(ctx context.Context, limit int) ([]protocol.Mutation, error) {
	v, err := s.call(ctx, "pending", limit)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		ID int64             `json:"id"`
		C  string            `json:"c"`
		D  string            `json:"d"`
		T  string            `json:"t"`
		F  map[string]string `json:"f"`
	}
	if err := json.Unmarshal([]byte(v.String()), &entries); err != nil {
		return nil, err
	}
	muts := make([]protocol.Mutation, len(entries))
	for i, e := range entries {
		f := make(map[string]json.RawMessage, len(e.F))
		for k, v := range e.F {
			f[k] = json.RawMessage(v)
		}
		muts[i] = protocol.Mutation{ID: e.ID, Collection: e.C, Doc: e.D, HLC: e.T, Fields: f}
	}
	return muts, nil
}

func (s idbStore) Ack(ctx context.Context, upTo int64) error {
	_, err := s.call(ctx, "ack", float64(upTo))
	return err
}

// bridgeDoc is the bridge's representation of a document.
type bridgeDoc struct {
	C string                 `json:"c"`
	D string                 `json:"d"`
	F map[string]bridgeField `json:"f"`
	// Purged is only set on changes sent to applyRemote.
	Purged string `json:"purged,omitempty"`
}

type bridgeField struct {
	V string `json:"v"` // raw JSON
	T string `json:"t"` // HLC
}

func (d bridgeDoc) toClient() client.StoredDoc {
	f := make(map[string]protocol.FieldValue, len(d.F))
	for k, v := range d.F {
		f[k] = protocol.FieldValue{Value: json.RawMessage(v.V), HLC: v.T}
	}
	return client.StoredDoc{Collection: d.C, ID: d.D, Fields: f}
}

func (s idbStore) Get(ctx context.Context, collection, id string) (client.StoredDoc, bool, error) {
	v, err := s.call(ctx, "get", collection, id)
	if err != nil || v.IsNull() || v.IsUndefined() {
		return client.StoredDoc{}, false, err
	}
	var d bridgeDoc
	if err := json.Unmarshal([]byte(v.String()), &d); err != nil {
		return client.StoredDoc{}, false, err
	}
	return d.toClient(), true, nil
}

func (s idbStore) List(ctx context.Context, collection string) ([]client.StoredDoc, error) {
	v, err := s.call(ctx, "list", collection)
	if err != nil {
		return nil, err
	}
	var docs []bridgeDoc
	if err := json.Unmarshal([]byte(v.String()), &docs); err != nil {
		return nil, err
	}
	out := make([]client.StoredDoc, len(docs))
	for i, d := range docs {
		out[i] = d.toClient()
	}
	return out, nil
}
