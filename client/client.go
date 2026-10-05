// Package client is the GoSync client engine. It is platform independent: the
// browser build (cmd/gosync-wasm) plugs in IndexedDB and the browser
// WebSocket, while native Go programs use MemoryStore or their own LocalStore
// with the gorilla-based dialer.
//
// Writes always go to the local store first and succeed offline. A background
// sync loop (Run) pushes the outbox and pulls server changes whenever a
// connection is available.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshalPatel1972/GoSync/hlc"
	"github.com/HarshalPatel1972/GoSync/protocol"
)

// Conn is a message-oriented connection to the server.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, msg []byte) error
	Close() error
}

// Dialer opens connections.
type Dialer interface {
	Dial(ctx context.Context, url string) (Conn, error)
}

// Status is the sync connection state.
type Status string

const (
	StatusOffline    Status = "offline"
	StatusConnecting Status = "connecting"
	StatusOnline     Status = "online"
)

// Event is delivered to subscribers.
type Event struct {
	// Kind is "change", "status" or "error".
	Kind string
	// For "change": the collection and IDs of documents that changed.
	Collection string
	IDs        []string
	// For "status".
	Status Status
	// For "error".
	Err error
}

// ServerError is an error frame sent by the server.
type ServerError struct {
	Code, Message string
	Fatal         bool
}

func (e *ServerError) Error() string { return fmt.Sprintf("gosync server: %s: %s", e.Code, e.Message) }

// Options configures a Client.
type Options struct {
	// URL of the server's sync endpoint, e.g. "wss://sync.example.com/sync".
	URL string
	// Token returns the auth token for each connection attempt. It is called
	// again on every reconnect, so it can refresh expiring tokens.
	Token  func(ctx context.Context) (string, error)
	Store  LocalStore
	Dialer Dialer
	// Collections limits which collections are pulled from the server;
	// empty syncs everything. Writes to any collection are always pushed.
	Collections []string
	// NodeSuffix distinguishes concurrent writers sharing one LocalStore
	// (e.g. browser tabs). Optional.
	NodeSuffix string
	// Logger receives debug output. *slog.Logger satisfies it. Optional.
	Logger Logger
}

// Logger is the logging the engine needs. It is an interface rather than
// *slog.Logger so the browser build does not link log/slog.
type Logger interface {
	Debug(msg string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}

// Document is a live (not deleted) document.
type Document struct {
	ID     string
	Fields map[string]json.RawMessage
}

// Client is a GoSync replica. All methods are safe for concurrent use.
type Client struct {
	opts     Options
	log      Logger
	clientID string
	clock    *hlc.Clock
	offsetMS atomic.Int64 // server time - local time

	kick chan struct{} // outbox has new mutations
	wake chan struct{} // retry connecting now

	mu     sync.Mutex
	subs   map[int]func(Event)
	nextID int
	status Status
}

const (
	pushBatch      = protocol.MaxMutationsPerPush
	connectTimeout = 15 * time.Second
	minBackoff     = 500 * time.Millisecond
	maxBackoff     = 30 * time.Second
)

// New opens a client on opts.Store. Call Run to start syncing.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Store == nil {
		return nil, errors.New("client: Store is required")
	}
	if opts.Logger == nil {
		opts.Logger = nopLogger{}
	}
	id, err := opts.Store.ClientID(ctx)
	if err != nil {
		return nil, fmt.Errorf("client: load client id: %w", err)
	}
	c := &Client{
		opts:     opts,
		log:      opts.Logger,
		clientID: id,
		kick:     make(chan struct{}, 1),
		wake:     make(chan struct{}, 1),
		subs:     map[int]func(Event){},
		status:   StatusOffline,
	}
	node := id
	if opts.NodeSuffix != "" {
		node += "." + opts.NodeSuffix
	}
	c.clock, err = hlc.NewClock(node, func() int64 { return time.Now().UnixMilli() + c.offsetMS.Load() })
	if err != nil {
		return nil, err
	}
	if maxTS, err := opts.Store.MaxHLC(ctx); err != nil {
		return nil, err
	} else if maxTS != "" {
		if ts, err := hlc.Parse(maxTS); err == nil {
			c.clock.Observe(ts)
		}
	}
	return c, nil
}

// ClientID returns this replica's ID.
func (c *Client) ClientID() string { return c.clientID }

// Status returns the current connection state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Subscribe registers fn for events and returns a function that removes it.
// fn runs on the goroutine that caused the event and must not block.
func (c *Client) Subscribe(fn func(Event)) func() {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.subs[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

func (c *Client) emit(e Event) {
	c.mu.Lock()
	fns := make([]func(Event), 0, len(c.subs))
	for _, fn := range c.subs {
		fns = append(fns, fn)
	}
	c.mu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

func (c *Client) setStatus(s Status) {
	c.mu.Lock()
	changed := c.status != s
	c.status = s
	c.mu.Unlock()
	if changed {
		c.emit(Event{Kind: "status", Status: s})
	}
}

// Set writes fields into a document, creating (or reviving) it. Fields not
// mentioned keep their values; set a field to null to clear it. Each value
// must be JSON.
func (c *Client) Set(ctx context.Context, collection, id string, fields map[string]json.RawMessage) error {
	all := make(map[string]json.RawMessage, len(fields)+1)
	for k, v := range fields {
		if k == protocol.FieldDeleted {
			return fmt.Errorf("field name %q is reserved", k)
		}
		all[k] = v
	}
	all[protocol.FieldDeleted] = json.RawMessage("false")
	return c.write(ctx, collection, id, all)
}

// Delete marks a document deleted. A later Set revives it.
func (c *Client) Delete(ctx context.Context, collection, id string) error {
	return c.write(ctx, collection, id, map[string]json.RawMessage{protocol.FieldDeleted: json.RawMessage("true")})
}

func (c *Client) write(ctx context.Context, collection, id string, fields map[string]json.RawMessage) error {
	ts := c.clock.Now().String()
	if err := protocol.ValidateMutation(protocol.Mutation{ID: 1, Collection: collection, Doc: id, HLC: ts, Fields: fields}); err != nil {
		return err
	}
	if _, err := c.opts.Store.Write(ctx, collection, id, fields, ts); err != nil {
		return err
	}
	c.emit(Event{Kind: "change", Collection: collection, IDs: []string{id}})
	c.NotifyOutbox()
	return nil
}

// Get returns a document, or ok=false if it does not exist or is deleted.
func (c *Client) Get(ctx context.Context, collection, id string) (doc Document, ok bool, err error) {
	d, ok, err := c.opts.Store.Get(ctx, collection, id)
	if err != nil || !ok || d.Deleted() {
		return Document{}, false, err
	}
	return toDocument(d), true, nil
}

// List returns every live document in a collection.
func (c *Client) List(ctx context.Context, collection string) ([]Document, error) {
	docs, err := c.opts.Store.List(ctx, collection)
	if err != nil {
		return nil, err
	}
	out := make([]Document, 0, len(docs))
	for _, d := range docs {
		if !d.Deleted() {
			out = append(out, toDocument(d))
		}
	}
	return out, nil
}

func toDocument(d StoredDoc) Document {
	f := make(map[string]json.RawMessage, len(d.Fields))
	for k, v := range d.Fields {
		if k != protocol.FieldDeleted {
			f[k] = v.Value
		}
	}
	return Document{ID: d.ID, Fields: f}
}

// NotifyOutbox tells the sync loop the outbox changed. Call it when another
// writer (e.g. another browser tab) added mutations to the shared store.
func (c *Client) NotifyOutbox() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Reconnect skips the current backoff delay, e.g. when the device comes
// back online.
func (c *Client) Reconnect() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ObserveHLC advances the local clock past a timestamp written elsewhere.
func (c *Client) ObserveHLC(ts string) {
	if t, err := hlc.Parse(ts); err == nil {
		c.clock.Observe(t)
	}
}

// Run syncs until ctx is cancelled, reconnecting with exponential backoff
// and jitter. It returns ctx.Err().
func (c *Client) Run(ctx context.Context) error {
	if c.opts.Dialer == nil || c.opts.URL == "" {
		return errors.New("client: URL and Dialer are required to sync")
	}
	defer c.setStatus(StatusOffline)
	backoff := minBackoff
	for {
		c.setStatus(StatusConnecting)
		established, err := c.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.setStatus(StatusOffline)
		if err != nil {
			c.log.Debug("sync session ended", "err", err)
			var se *ServerError
			if errors.As(err, &se) {
				c.emit(Event{Kind: "error", Err: err})
			}
		}
		if established {
			backoff = minBackoff
		}
		delay := backoff/2 + rand.N(backoff/2+1)
		select {
		case <-time.After(delay):
		case <-c.wake:
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session runs one connection. established reports whether the handshake
// completed, which resets the reconnect backoff.
func (c *Client) session(ctx context.Context) (established bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(ctx, connectTimeout)
	defer dialCancel()
	conn, err := c.opts.Dialer.Dial(dialCtx, c.opts.URL)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	token := ""
	if c.opts.Token != nil {
		if token, err = c.opts.Token(dialCtx); err != nil {
			return false, fmt.Errorf("get token: %w", err)
		}
	}
	if err := c.send(dialCtx, conn, protocol.TypeHello, protocol.Hello{Version: protocol.Version, Token: token, ClientID: c.clientID, Collections: c.opts.Collections}); err != nil {
		return false, err
	}
	sent := time.Now()
	env, err := readEnvelope(dialCtx, conn)
	if err != nil {
		return false, err
	}
	if env.Type != protocol.TypeWelcome {
		return false, unexpected(env)
	}
	var welcome protocol.Welcome
	if err := json.Unmarshal(env.Data, &welcome); err != nil {
		return false, err
	}
	// Estimate the server clock at the midpoint of the round trip.
	rtt := time.Since(sent)
	c.offsetMS.Store(welcome.ServerTime - sent.Add(rtt/2).UnixMilli())
	if err := c.opts.Store.Ack(ctx, welcome.LastMutationID); err != nil {
		return true, err
	}
	scope := ScopeKey(c.opts.Collections)
	cursor, err := c.opts.Store.Cursor(ctx, scope)
	if err != nil {
		return true, err
	}
	c.setStatus(StatusOnline)

	frames := make(chan protocol.Envelope)
	readErr := make(chan error, 1)
	go func() {
		for {
			env, err := readEnvelope(ctx, conn)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- env:
			case <-ctx.Done():
				return
			}
		}
	}()

	needPush, needPull := true, true
	pushing, pulling := false, false
	for {
		if needPush && !pushing {
			needPush = false
			muts, err := c.opts.Store.Pending(ctx, pushBatch)
			if err != nil {
				return true, err
			}
			if len(muts) > 0 {
				if err := c.send(ctx, conn, protocol.TypePush, protocol.Push{Mutations: muts}); err != nil {
					return true, err
				}
				pushing = true
			}
		}
		if needPull && !pulling {
			needPull = false
			if err := c.send(ctx, conn, protocol.TypePull, protocol.Pull{Cursor: cursor}); err != nil {
				return true, err
			}
			pulling = true
		}

		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case err := <-readErr:
			return true, err
		case <-c.kick:
			needPush = true
		case env := <-frames:
			switch env.Type {
			case protocol.TypePushResult:
				var res protocol.PushResult
				if err := json.Unmarshal(env.Data, &res); err != nil {
					return true, err
				}
				if err := c.opts.Store.Ack(ctx, res.LastMutationID); err != nil {
					return true, err
				}
				for _, r := range res.Rejected {
					c.emit(Event{Kind: "error", Err: fmt.Errorf("gosync: server rejected mutation %d: %s", r.ID, r.Reason)})
				}
				pushing, needPush = false, true
			case protocol.TypePullResult:
				var res protocol.PullResult
				if err := json.Unmarshal(env.Data, &res); err != nil {
					return true, err
				}
				if err := c.applyRemote(ctx, res, scope); err != nil {
					return true, err
				}
				cursor = res.Cursor
				pulling = false
				needPull = needPull || res.More
			case protocol.TypePoke:
				needPull = true
			default:
				return true, unexpected(env)
			}
		}
	}
}

func (c *Client) applyRemote(ctx context.Context, res protocol.PullResult, scope string) error {
	for _, ch := range res.Changes {
		for _, f := range ch.Fields {
			c.ObserveHLC(f.HLC)
		}
		if ch.Purged != "" {
			c.ObserveHLC(ch.Purged)
		}
	}
	if err := c.opts.Store.ApplyRemote(ctx, res.Changes, scope, res.Cursor); err != nil {
		return err
	}
	byColl := map[string][]string{}
	var order []string
	for _, ch := range res.Changes {
		if _, ok := byColl[ch.Collection]; !ok {
			order = append(order, ch.Collection)
		}
		byColl[ch.Collection] = append(byColl[ch.Collection], ch.Doc)
	}
	for _, coll := range order {
		c.emit(Event{Kind: "change", Collection: coll, IDs: byColl[coll]})
	}
	return nil
}

func (c *Client) send(ctx context.Context, conn Conn, typ string, payload any) error {
	b, err := protocol.Encode(typ, payload)
	if err != nil {
		return err
	}
	return conn.Write(ctx, b)
}

func readEnvelope(ctx context.Context, conn Conn) (protocol.Envelope, error) {
	b, err := conn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return env, fmt.Errorf("malformed frame: %w", err)
	}
	if env.Type == protocol.TypeError {
		var e protocol.Error
		json.Unmarshal(env.Data, &e)
		return env, &ServerError{Code: e.Code, Message: e.Message, Fatal: e.Fatal}
	}
	return env, nil
}

func unexpected(env protocol.Envelope) error {
	return fmt.Errorf("unexpected %q frame", env.Type)
}
