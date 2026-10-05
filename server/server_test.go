package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"

	"github.com/HarshalPatel1972/GoSync/client"
	"github.com/HarshalPatel1972/GoSync/protocol"
	"github.com/HarshalPatel1972/GoSync/server"
	"github.com/HarshalPatel1972/GoSync/store/sqlstore"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	t     *testing.T
	store *sqlstore.Store
	http  *httptest.Server
	srv   atomic.Pointer[server.Server]
	auth  server.Authenticator
	url   string
}

func newEnv(t *testing.T, auth server.Authenticator) *env {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil {
		auth = server.InsecureDevAuthenticator()
	}
	e := &env{t: t, store: st, auth: auth}
	e.restart()
	e.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.srv.Load().Handler().ServeHTTP(w, r)
	}))
	e.url = "ws" + strings.TrimPrefix(e.http.URL, "http") + "/sync"
	t.Cleanup(func() {
		e.srv.Load().Shutdown(context.Background())
		e.http.Close()
		st.Close()
	})
	return e
}

// restart swaps in a fresh Server on the same store, dropping all sessions.
func (e *env) restart() {
	srv, err := server.New(server.Config{Store: e.store, Auth: e.auth, Logger: quiet, PullBudgetBytes: 4096})
	if err != nil {
		e.t.Fatal(err)
	}
	if old := e.srv.Swap(srv); old != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		old.Shutdown(ctx)
	}
}

type testClient struct {
	*client.Client
	store  *client.MemoryStore
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	errs   []error
}

func (e *env) client(token string) *testClient {
	e.t.Helper()
	st := client.NewMemoryStore()
	c, err := client.New(context.Background(), client.Options{
		URL:    e.url,
		Token:  func(context.Context) (string, error) { return token, nil },
		Store:  st,
		Dialer: client.WebSocketDialer{},
		Logger: quiet,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	tc := &testClient{Client: c, store: st}
	c.Subscribe(func(ev client.Event) {
		if ev.Kind == "error" {
			tc.mu.Lock()
			tc.errs = append(tc.errs, ev.Err)
			tc.mu.Unlock()
		}
	})
	e.t.Cleanup(tc.stop)
	return tc
}

func (tc *testClient) start() {
	ctx, cancel := context.WithCancel(context.Background())
	tc.cancel, tc.done = cancel, make(chan struct{})
	go func() { tc.Run(ctx); close(tc.done) }()
}

func (tc *testClient) stop() {
	if tc.cancel != nil {
		tc.cancel()
		<-tc.done
		tc.cancel = nil
	}
}

func (tc *testClient) errors() []error {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return append([]error(nil), tc.errs...)
}

func (tc *testClient) set(t *testing.T, coll, id string, fields map[string]any) {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for k, v := range fields {
		b, _ := json.Marshal(v)
		raw[k] = b
	}
	if err := tc.Set(context.Background(), coll, id, raw); err != nil {
		t.Fatal(err)
	}
}

// snapshot renders a collection as a comparable string.
func (tc *testClient) snapshot(coll string) string {
	docs, _ := tc.List(context.Background(), coll)
	var b strings.Builder
	for _, d := range docs {
		f, _ := json.Marshal(d.Fields) // map keys are sorted
		fmt.Fprintf(&b, "%s=%s;", d.ID, f)
	}
	return b.String()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRealtimeSyncBetweenDevices(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.client("alice"), e.client("alice")
	a.start()
	b.start()
	eventually(t, "both online", func() bool {
		return a.Status() == client.StatusOnline && b.Status() == client.StatusOnline
	})

	changed := make(chan string, 10)
	b.Subscribe(func(ev client.Event) {
		if ev.Kind == "change" {
			changed <- ev.Collection
		}
	})
	a.set(t, "todos", "t1", map[string]any{"title": "Buy milk", "done": false})
	select {
	case c := <-changed:
		if c != "todos" {
			t.Fatalf("change event for %q", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B was not notified of A's write")
	}
	eventually(t, "B has the todo", func() bool { return b.snapshot("todos") == a.snapshot("todos") })

	if err := b.Delete(context.Background(), "todos", "t1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delete propagates", func() bool { return a.snapshot("todos") == "" })
}

func TestOfflineWritesSyncLater(t *testing.T) {
	e := newEnv(t, nil)
	a := e.client("alice")
	for i := range 250 { // more than one push batch
		a.set(t, "notes", fmt.Sprintf("n%03d", i), map[string]any{"i": i})
	}
	a.start()

	b := e.client("alice")
	b.start()
	eventually(t, "B receives all offline writes", func() bool {
		docs, _ := b.List(context.Background(), "notes")
		return len(docs) == 250
	})
	eventually(t, "A's outbox drains", func() bool {
		p, _ := a.store.Pending(context.Background(), 1)
		return len(p) == 0
	})
}

func TestConcurrentOfflineEditsConverge(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.client("alice"), e.client("alice")
	a.set(t, "doc", "x", map[string]any{"title": "v0", "body": "v0"})
	a.start()
	b.start()
	eventually(t, "B gets x", func() bool { return b.snapshot("doc") == a.snapshot("doc") })
	a.stop()
	b.stop()

	// Both edit offline: different fields merge, the same field converges on
	// the later write (B writes after A on the hybrid clock).
	a.set(t, "doc", "x", map[string]any{"title": "from A", "shared": "A"})
	time.Sleep(5 * time.Millisecond)
	b.set(t, "doc", "x", map[string]any{"body": "from B", "shared": "B"})

	a.start()
	b.start()
	want := `x={"body":"from B","shared":"B","title":"from A"};`
	eventually(t, "replicas converge", func() bool {
		return a.snapshot("doc") == want && b.snapshot("doc") == want
	})
}

func TestUsersAreIsolated(t *testing.T) {
	e := newEnv(t, nil)
	alice, bob := e.client("alice"), e.client("bob")
	alice.set(t, "todos", "secret", map[string]any{"v": 1})
	alice.start()
	bob.start()
	eventually(t, "alice pushed", func() bool {
		p, _ := alice.store.Pending(context.Background(), 1)
		return len(p) == 0
	})
	bob.set(t, "todos", "mine", map[string]any{"v": 2})
	eventually(t, "bob pushed", func() bool {
		p, _ := bob.store.Pending(context.Background(), 1)
		return len(p) == 0
	})
	time.Sleep(200 * time.Millisecond)
	if got := bob.snapshot("todos"); got != `mine={"v":2};` {
		t.Fatalf("bob sees %s", got)
	}
	if got := alice.snapshot("todos"); got != `secret={"v":1};` {
		t.Fatalf("alice sees %s", got)
	}
}

func TestReconnectAfterServerRestart(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.client("alice"), e.client("alice")
	a.start()
	b.start()
	eventually(t, "online", func() bool { return a.Status() == client.StatusOnline && b.Status() == client.StatusOnline })

	e.restart() // drops every session
	a.set(t, "todos", "after-restart", map[string]any{"ok": true})
	eventually(t, "B sees write made across restart", func() bool {
		return strings.Contains(b.snapshot("todos"), "after-restart")
	})
}

func TestJWTAuth(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	auth, err := server.NewJWTAuthenticator(context.Background(), server.JWTConfig{Secret: secret, Issuer: "test"})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, auth)
	sign := func(sub string, exp time.Time, key []byte) string {
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": sub, "iss": "test", "exp": exp.Unix()}).SignedString(key)
		return tok
	}

	good := e.client(sign("alice", time.Now().Add(time.Hour), secret))
	good.start()
	eventually(t, "valid token connects", func() bool { return good.Status() == client.StatusOnline })

	for name, tok := range map[string]string{
		"forged":  sign("alice", time.Now().Add(time.Hour), []byte(strings.Repeat("x", 32))),
		"expired": sign("alice", time.Now().Add(-time.Hour), secret),
		"garbage": "not-a-jwt",
	} {
		bad := e.client(tok)
		bad.start()
		eventually(t, name+" token rejected", func() bool {
			for _, err := range bad.errors() {
				var se *client.ServerError
				if errors.As(err, &se) && se.Code == protocol.ErrUnauthorized {
					return true
				}
			}
			return false
		})
		if bad.Status() == client.StatusOnline {
			t.Fatalf("%s token went online", name)
		}
		bad.stop()
	}
}

func TestRejectsDisallowedOrigin(t *testing.T) {
	e := newEnv(t, nil)
	h := http.Header{"Origin": {"https://evil.example"}}
	_, resp, err := websocket.DefaultDialer.Dial(e.url, h)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for foreign origin, got err=%v resp=%v", err, resp)
	}
}

func TestRejectsFutureTimestamps(t *testing.T) {
	e := newEnv(t, nil)
	ws, _, err := websocket.DefaultDialer.Dial(e.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	send := func(typ string, v any) {
		b, _ := protocol.Encode(typ, v)
		ws.WriteMessage(websocket.TextMessage, b)
	}
	read := func() protocol.Envelope {
		var env protocol.Envelope
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatal(err)
		}
		return env
	}
	send(protocol.TypeHello, protocol.Hello{Version: protocol.Version, Token: "alice", ClientID: "raw"})
	if env := read(); env.Type != protocol.TypeWelcome {
		t.Fatalf("got %s", env.Type)
	}
	future := fmt.Sprintf("%015d-000000-raw", time.Now().Add(24*time.Hour).UnixMilli())
	send(protocol.TypePush, protocol.Push{Mutations: []protocol.Mutation{
		{ID: 1, Collection: "c", Doc: "d", HLC: future, Fields: map[string]json.RawMessage{"v": json.RawMessage(`1`)}},
	}})
	env := read()
	var res protocol.PushResult
	json.Unmarshal(env.Data, &res)
	if env.Type != protocol.TypePushResult || res.LastMutationID != 1 || len(res.Rejected) != 1 {
		t.Fatalf("expected rejection, got %s %s", env.Type, env.Data)
	}
}

func TestHelloRequired(t *testing.T) {
	e := newEnv(t, nil)
	ws, _, err := websocket.DefaultDialer.Dial(e.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	b, _ := protocol.Encode(protocol.TypePull, protocol.Pull{})
	ws.WriteMessage(websocket.TextMessage, b)
	var env protocol.Envelope
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ws.ReadJSON(&env); err != nil || env.Type != protocol.TypeError {
		t.Fatalf("expected error frame, got %v %v", env.Type, err)
	}
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("connection should be closed")
	}
}
