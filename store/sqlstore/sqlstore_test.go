package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/HarshalPatel1972/GoSync/hlc"
	"github.com/HarshalPatel1972/GoSync/protocol"
)

// stores returns a fresh SQLite store, plus a PostgreSQL one when
// GOSYNC_TEST_POSTGRES holds a connection URL.
func stores(t *testing.T) map[string]*Store {
	t.Helper()
	ctx := context.Background()
	out := map[string]*Store{}

	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	out["sqlite"] = s

	if dsn := os.Getenv("GOSYNC_TEST_POSTGRES"); dsn != "" {
		p, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, tbl := range []string{"gosync_fields", "gosync_clients", "gosync_spaces"} {
			if _, err := p.db.Exec("DELETE FROM " + tbl); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { p.Close() })
		out["postgres"] = p
	}
	return out
}

func ts(wall int64, node string) string { return hlc.Timestamp{Wall: wall, Node: node}.String() }

func mut(id int64, doc, field, value, t string) protocol.Mutation {
	return protocol.Mutation{ID: id, Collection: "todos", Doc: doc, HLC: t,
		Fields: map[string]json.RawMessage{field: json.RawMessage(value)}}
}

func pullAll(t *testing.T, s *Store, ns string, cursor int64) (map[string]map[string]string, int64) {
	t.Helper()
	docs := map[string]map[string]string{}
	for {
		res, err := s.Pull(context.Background(), ns, cursor, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range res.Changes {
			if docs[ch.Doc] == nil {
				docs[ch.Doc] = map[string]string{}
			}
			for f, v := range ch.Fields {
				docs[ch.Doc][f] = string(v.Value)
			}
		}
		cursor = res.Cursor
		if !res.More {
			return docs, cursor
		}
	}
}

func TestLastWriterWinsPerField(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			// Client B writes later (higher HLC) but its push arrives first.
			if _, err := s.Push(ctx, "u1", "B", []protocol.Mutation{mut(1, "a", "title", `"from B"`, ts(200, "B"))}, 0); err != nil {
				t.Fatal(err)
			}
			out, err := s.Push(ctx, "u1", "A", []protocol.Mutation{
				mut(1, "a", "title", `"from A"`, ts(100, "A")), // older: must lose
				mut(2, "a", "done", `true`, ts(100, "A")),      // other field: must merge
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if out.LastMutationID != 2 || !out.Changed {
				t.Fatalf("unexpected outcome %+v", out)
			}
			docs, _ := pullAll(t, s, "u1", 0)
			if docs["a"]["title"] != `"from B"` || docs["a"]["done"] != "true" {
				t.Fatalf("bad merge: %v", docs)
			}
		})
	}
}

func TestPushIsIdempotent(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := []protocol.Mutation{mut(1, "a", "n", `1`, ts(100, "A"))}
			first, _ := s.Push(ctx, "u1", "A", m, 0)
			_, v1 := pullAll(t, s, "u1", 0)
			second, err := s.Push(ctx, "u1", "A", m, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, v2 := pullAll(t, s, "u1", 0)
			if first.LastMutationID != 1 || second.LastMutationID != 1 || second.Changed || v1 != v2 {
				t.Fatalf("replay changed state: %+v %+v v1=%d v2=%d", first, second, v1, v2)
			}
			// ackUpTo acknowledges rejected mutations without applying them.
			out, _ := s.Push(ctx, "u1", "A", nil, 5)
			if last, _ := s.LastMutationID(ctx, "u1", "A"); out.LastMutationID != 5 || last != 5 {
				t.Fatalf("ackUpTo not recorded: %+v last=%d", out, last)
			}
		})
	}
}

func TestNamespacesAreIsolated(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.Push(ctx, "alice", "A", []protocol.Mutation{mut(1, "secret", "v", `"alice"`, ts(1, "A"))}, 0)
			docs, cursor := pullAll(t, s, "bob", 0)
			if len(docs) != 0 || cursor != 0 {
				t.Fatalf("bob sees alice's data: %v", docs)
			}
		})
	}
}

func TestIncrementalPullAndPagination(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for i := 1; i <= 50; i++ {
				m := mut(int64(i), fmt.Sprintf("doc%02d", i), "n", fmt.Sprint(i), ts(int64(i), "A"))
				if _, err := s.Push(ctx, "u1", "A", []protocol.Mutation{m}, 0); err != nil {
					t.Fatal(err)
				}
			}
			// Tiny budget forces one version per page; nothing may be lost.
			seen := map[string]bool{}
			cursor, pages := int64(0), 0
			for {
				res, err := s.Pull(ctx, "u1", cursor, 1)
				if err != nil {
					t.Fatal(err)
				}
				pages++
				for _, ch := range res.Changes {
					seen[ch.Doc] = true
				}
				if res.Cursor <= cursor && res.More {
					t.Fatal("cursor did not advance")
				}
				cursor = res.Cursor
				if !res.More {
					break
				}
			}
			if len(seen) != 50 || pages < 50 {
				t.Fatalf("saw %d docs in %d pages", len(seen), pages)
			}
			// Only the newer write shows up after the cursor.
			s.Push(ctx, "u1", "A", []protocol.Mutation{mut(51, "doc07", "n", `700`, ts(1000, "A"))}, 0)
			docs, _ := pullAll(t, s, "u1", cursor)
			if len(docs) != 1 || docs["doc07"]["n"] != "700" {
				t.Fatalf("incremental pull returned %v", docs)
			}
		})
	}
}

func TestConcurrentPushesConverge(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var wg sync.WaitGroup
			for c := 0; c < 8; c++ {
				wg.Add(1)
				go func(c int) {
					defer wg.Done()
					client := fmt.Sprintf("c%d", c)
					for i := 1; i <= 20; i++ {
						m := mut(int64(i), "shared", "v", fmt.Sprintf(`"%s-%d"`, client, i), ts(int64(i*10+c), client))
						if _, err := s.Push(ctx, "u1", client, []protocol.Mutation{m}, 0); err != nil {
							t.Error(err)
							return
						}
					}
				}(c)
			}
			wg.Wait()
			docs, _ := pullAll(t, s, "u1", 0)
			// Highest HLC is wall 20*10+7 from c7.
			if docs["shared"]["v"] != `"c7-20"` {
				t.Fatalf("expected c7-20 to win, got %v", docs["shared"])
			}
		})
	}
}
