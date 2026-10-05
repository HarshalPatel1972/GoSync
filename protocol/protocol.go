// Package protocol defines the GoSync wire protocol (version 2).
//
// Every frame is a JSON envelope {"type": "...", "data": {...}} sent over a
// WebSocket. The flow is:
//
//	client -> hello{token, clientId, wall}     server -> welcome{serverTime, lastMutationId}
//	client -> push{mutations}                  server -> pushResult{lastMutationId}
//	client -> pull{cursor}                     server -> pullResult{cursor, changes, more}
//	                                           server -> poke{}   (new data, please pull)
//	                                           server -> error{code, message, fatal}
//
// Data is organised as collections of documents. Each document field is an
// independent last-writer-wins register ordered by a hybrid logical clock, so
// concurrent edits to different fields merge and every replica converges.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/HarshalPatel1972/GoSync/hlc"
)

// Version is the protocol version sent in hello.
const Version = 2

// Message types.
const (
	TypeHello      = "hello"
	TypeWelcome    = "welcome"
	TypePush       = "push"
	TypePushResult = "pushResult"
	TypePull       = "pull"
	TypePullResult = "pullResult"
	TypePoke       = "poke"
	TypeError      = "error"
)

// FieldDeleted is the reserved field that marks a document as deleted.
// Deletion is itself a LWW register, so a later write can revive a document.
const FieldDeleted = "_deleted"

// Limits shared by client and server. The server enforces them; the client
// checks them up front so applications get errors at write time.
const (
	MaxMessageBytes = 1 << 20  // largest frame a client may send
	MaxValueBytes   = 64 << 10 // largest single field value
	// MaxMutationBytes bounds a whole mutation (names + values) so any single
	// mutation fits in a push frame with room to spare.
	MaxMutationBytes = 256 << 10
	// MaxPushBytes is the size clients aim to keep push frames under.
	MaxPushBytes        = 768 << 10
	MaxMutationsPerPush = 100
	MaxFieldsPerDoc     = 100
	MaxCollectionLen    = 64
	MaxDocIDLen         = 128
	MaxFieldNameLen     = 64
	MaxClientIDLen      = 64
	MaxTokenLen         = 16 << 10
	MaxCollections      = 64 // per hello
)

// Envelope is the outer frame.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Hello authenticates and identifies a client. It must be the first frame.
type Hello struct {
	Version  int    `json:"version"`
	Token    string `json:"token"`
	ClientID string `json:"clientId"`
	// Collections limits pulls to these collections; empty means all. A
	// cursor is only meaningful for the collection set it was pulled with.
	Collections []string `json:"collections,omitempty"`
}

// Welcome confirms authentication. ServerTime (unix ms) lets clients correct
// clock skew; LastMutationID tells the client which outbox entries the server
// already applied, in case an earlier acknowledgement was lost.
type Welcome struct {
	ServerTime     int64 `json:"serverTime"`
	LastMutationID int64 `json:"lastMutationId"`
}

// Mutation is one local write: a set of field values for one document, all
// stamped with the same HLC. IDs are assigned by the client, strictly
// increasing per client, and make pushes idempotent.
type Mutation struct {
	ID         int64                      `json:"id"`
	Collection string                     `json:"c"`
	Doc        string                     `json:"d"`
	HLC        string                     `json:"t"`
	Fields     map[string]json.RawMessage `json:"f"`
}

// Push sends pending mutations in ascending ID order.
type Push struct {
	Mutations []Mutation `json:"mutations"`
}

// PushResult acknowledges every mutation up to LastMutationID.
type PushResult struct {
	LastMutationID int64       `json:"lastMutationId"`
	Rejected       []Rejection `json:"rejected,omitempty"`
}

// Rejection reports a mutation the server refused. It is still acknowledged
// (it will never apply), so clients must drop it from their outbox.
type Rejection struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// Pull asks for every change after Cursor.
type Pull struct {
	Cursor int64 `json:"cursor"`
}

// FieldValue is one LWW register: a JSON value and its HLC.
type FieldValue struct {
	Value json.RawMessage `json:"v"`
	HLC   string          `json:"t"`
}

// DocChange carries the latest value of changed fields of one document.
//
// Purged, when set, reports that the server compacted the document after a
// deletion with that HLC: replicas drop every field whose HLC is at or below
// it (fields set later, e.g. by an offline revival, stay).
type DocChange struct {
	Collection string                `json:"c"`
	Doc        string                `json:"d"`
	Fields     map[string]FieldValue `json:"f"`
	Purged     string                `json:"purged,omitempty"`
}

// PullResult returns changes up to Cursor. When More is set the client should
// pull again immediately.
type PullResult struct {
	Cursor  int64       `json:"cursor"`
	More    bool        `json:"more,omitempty"`
	Changes []DocChange `json:"changes"`
}

// Error codes.
const (
	ErrUnauthorized = "unauthorized"
	ErrBadRequest   = "bad_request"
	ErrRateLimited  = "rate_limited"
	ErrInternal     = "internal"
	ErrVersion      = "unsupported_version"
)

// Error is sent before the server closes a connection (Fatal) or when a single
// request fails.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal,omitempty"`
}

// Encode wraps a payload in an envelope.
func Encode(typ string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{Type: typ, Data: raw})
}

// ValidateClientID checks a client ID.
func ValidateClientID(id string) error {
	return validateName("client id", id, MaxClientIDLen)
}

// ValidateCollection checks a collection name: 1-64 chars of [A-Za-z0-9_.-].
func ValidateCollection(c string) error {
	return validateName("collection", c, MaxCollectionLen)
}

func validateName(what, s string, max int) error {
	if s == "" || len(s) > max {
		return fmt.Errorf("%s must be 1-%d characters", what, max)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return fmt.Errorf("%s may only contain letters, digits, '_', '-' and '.'", what)
		}
	}
	return nil
}

// ValidateHello checks a hello's client-supplied fields (not the token).
func ValidateHello(h Hello) error {
	if err := ValidateClientID(h.ClientID); err != nil {
		return err
	}
	if len(h.Collections) > MaxCollections {
		return fmt.Errorf("at most %d collections", MaxCollections)
	}
	for _, c := range h.Collections {
		if err := ValidateCollection(c); err != nil {
			return err
		}
	}
	return nil
}

// ValidateDocID checks a document ID: 1-128 bytes of valid UTF-8 without NUL.
func ValidateDocID(id string) error {
	if id == "" || len(id) > MaxDocIDLen {
		return fmt.Errorf("document id must be 1-%d bytes", MaxDocIDLen)
	}
	if !utf8.ValidString(id) {
		return errors.New("document id must be valid UTF-8")
	}
	for i := 0; i < len(id); i++ {
		if id[i] == 0 {
			return errors.New("document id must not contain NUL")
		}
	}
	return nil
}

// ValidateFieldName checks a field name. Names beginning with '_' are reserved
// (except FieldDeleted) and "id" is reserved for the document ID.
func ValidateFieldName(f string) error {
	if f == FieldDeleted {
		return nil
	}
	if f == "id" || (len(f) > 0 && f[0] == '_') {
		return fmt.Errorf("field name %q is reserved", f)
	}
	if f == "" || len(f) > MaxFieldNameLen || !utf8.ValidString(f) {
		return fmt.Errorf("field name must be 1-%d bytes of UTF-8", MaxFieldNameLen)
	}
	return nil
}

// ValidateMutation checks everything about a mutation that does not depend on
// server state.
func ValidateMutation(m Mutation) error {
	if m.ID <= 0 {
		return errors.New("mutation id must be positive")
	}
	if err := ValidateCollection(m.Collection); err != nil {
		return err
	}
	if err := ValidateDocID(m.Doc); err != nil {
		return err
	}
	if _, err := hlc.Parse(m.HLC); err != nil {
		return err
	}
	if len(m.Fields) == 0 || len(m.Fields) > MaxFieldsPerDoc {
		return fmt.Errorf("a mutation must set 1-%d fields", MaxFieldsPerDoc)
	}
	total := 0
	for name, v := range m.Fields {
		if err := ValidateFieldName(name); err != nil {
			return err
		}
		total += len(name) + len(v)
		if total > MaxMutationBytes {
			return fmt.Errorf("a mutation may hold at most %d bytes", MaxMutationBytes)
		}
		if len(v) > MaxValueBytes {
			return fmt.Errorf("field %q exceeds %d bytes", name, MaxValueBytes)
		}
		if !json.Valid(v) {
			return fmt.Errorf("field %q is not valid JSON", name)
		}
		if name == FieldDeleted && string(v) != "true" && string(v) != "false" {
			return fmt.Errorf("%s must be a boolean", FieldDeleted)
		}
	}
	return nil
}
