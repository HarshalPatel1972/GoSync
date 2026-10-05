package client

import (
	"encoding/json"
	"testing"

	"github.com/HarshalPatel1972/GoSync/protocol"
)

func fv(v, t string) protocol.FieldValue {
	return protocol.FieldValue{Value: json.RawMessage(v), HLC: t}
}

func TestMergeFieldsIsOrderIndependent(t *testing.T) {
	a := map[string]protocol.FieldValue{"x": fv(`1`, "001"), "y": fv(`"a"`, "005")}
	b := map[string]protocol.FieldValue{"x": fv(`2`, "003"), "y": fv(`"b"`, "002"), "z": fv(`true`, "004")}

	ab := map[string]protocol.FieldValue{}
	MergeFields(ab, a)
	MergeFields(ab, b)
	ba := map[string]protocol.FieldValue{}
	MergeFields(ba, b)
	MergeFields(ba, a)
	MergeFields(ba, a) // idempotent

	want := map[string]string{"x": "2", "y": `"a"`, "z": "true"}
	for name, w := range want {
		if string(ab[name].Value) != w || string(ba[name].Value) != w {
			t.Fatalf("field %s: ab=%s ba=%s want %s", name, ab[name].Value, ba[name].Value, w)
		}
	}
}

func TestApplyChangePurge(t *testing.T) {
	doc := map[string]protocol.FieldValue{
		"old":      fv(`1`, "100"),
		"_deleted": fv(`true`, "110"),
		"revival":  fv(`"local"`, "200"), // written offline after the delete
	}
	ApplyChange(doc, protocol.DocChange{Purged: "110", Fields: map[string]protocol.FieldValue{}})
	if len(doc) != 1 || string(doc["revival"].Value) != `"local"` {
		t.Fatalf("purge should keep only later fields, got %v", doc)
	}
}

func TestScopeKey(t *testing.T) {
	if ScopeKey([]string{"b", "a", "b"}) != "a,b" || ScopeKey(nil) != "" {
		t.Fatal("scope key must be sorted, deduplicated, and empty for all")
	}
}
