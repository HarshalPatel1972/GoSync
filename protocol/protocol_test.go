package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HarshalPatel1972/GoSync/hlc"
)

func validMutation() Mutation {
	return Mutation{
		ID:         1,
		Collection: "todos",
		Doc:        "a",
		HLC:        hlc.Timestamp{Wall: 1, Node: "n"}.String(),
		Fields:     map[string]json.RawMessage{"title": json.RawMessage(`"x"`)},
	}
}

func TestValidateMutation(t *testing.T) {
	if err := ValidateMutation(validMutation()); err != nil {
		t.Fatalf("valid mutation rejected: %v", err)
	}
	cases := map[string]func(*Mutation){
		"zero id":          func(m *Mutation) { m.ID = 0 },
		"bad collection":   func(m *Mutation) { m.Collection = "a b" },
		"empty doc":        func(m *Mutation) { m.Doc = "" },
		"bad hlc":          func(m *Mutation) { m.HLC = "nope" },
		"no fields":        func(m *Mutation) { m.Fields = nil },
		"reserved id":      func(m *Mutation) { m.Fields = map[string]json.RawMessage{"id": json.RawMessage(`1`)} },
		"reserved _x":      func(m *Mutation) { m.Fields = map[string]json.RawMessage{"_x": json.RawMessage(`1`)} },
		"invalid json":     func(m *Mutation) { m.Fields = map[string]json.RawMessage{"a": json.RawMessage(`{`)} },
		"deleted not bool": func(m *Mutation) { m.Fields = map[string]json.RawMessage{FieldDeleted: json.RawMessage(`1`)} },
		"value too big": func(m *Mutation) {
			m.Fields = map[string]json.RawMessage{"a": json.RawMessage(`"` + strings.Repeat("x", MaxValueBytes) + `"`)}
		},
	}
	for name, mutate := range cases {
		m := validMutation()
		mutate(&m)
		if err := ValidateMutation(m); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestEncode(t *testing.T) {
	b, err := Encode(TypePull, Pull{Cursor: 7})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"pull","data":{"cursor":7}}` {
		t.Fatalf("unexpected encoding %s", b)
	}
}
