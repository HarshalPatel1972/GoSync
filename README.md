# GoSync

![Build Status](https://github.com/HarshalPatel1972/GoSync/actions/workflows/ci.yml/badge.svg)

**Offline-first, real-time sync for web apps. Self-hosted, written in Go.**

[🌐 Website](https://gosync-zero.vercel.app) · [📖 Protocol](./PROTOCOL.md) · [📦 NPM Package](https://www.npmjs.com/package/@harshalpatel2868/gosync-client) · [🎨 Website Repo](https://github.com/HarshalPatel1972/gosync-zero)

Your app reads and writes a local database in the browser, so it is instant and works
offline. GoSync syncs those changes to your server and to the user's other devices and
tabs in real time, and resolves conflicts automatically. You own the server and the
data: one Go binary on SQLite or PostgreSQL.

```js
import { createClient } from '@harshalpatel2868/gosync-client';

const db = await createClient({
  url: 'wss://sync.example.com/sync',
  getToken: () => auth.getIdToken(),   // JWT from your auth provider
  dbName: `app-${user.id}`,
});

await db.set('todos', crypto.randomUUID(), { title: 'Buy milk', done: false }); // works offline
db.watch('todos', (todos) => render(todos));     // fires on local, other-tab and remote changes
db.onStatus((s) => console.log(s));              // 'offline' | 'connecting' | 'online'
```

## Features

- **Local-first.** Every write lands in IndexedDB first and succeeds offline. Data survives reloads.
- **Real-time.** The server pokes connected devices the moment data changes.
- **Automatic conflict resolution.** Each field is a last-writer-wins register ordered by a
  [hybrid logical clock](https://cse.buffalo.edu/tech-reports/2014-04.pdf). Concurrent edits to
  different fields merge, and every replica converges to the same state.
- **Efficient.** Clients pull only what changed since their cursor. Pushes are batched and idempotent.
- **Secure by default.** JWT auth (HS256 secret, or JWKS for Auth0, Clerk, Supabase, Firebase,
  Cognito…), per-user data isolation, origin checks, rate and size limits.
- **Multi-tab aware.** One tab holds the connection (Web Locks); the others share the same
  IndexedDB and update instantly over BroadcastChannel.
- **Scales out.** Run several server instances on PostgreSQL; they coordinate with LISTEN/NOTIFY.
- **Same Go code on both ends.** The client engine is Go compiled to WebAssembly and shares the
  protocol and clock packages with the server.

## Try the demo

Requires Go 1.26+ and Node 18+.

```bash
make demo        # or see the Makefile for the two underlying commands
```

Open <http://localhost:8080> in two tabs or browsers. Add todos, stop the server, keep editing,
restart it, and watch everything converge. Add `?user=bob` to the URL to see per-user isolation.

> The demo runs with `GOSYNC_INSECURE_DEV_AUTH=true`, which trusts any user name as a token.
> Never enable it in production.

## Running in production

```bash
docker build -t gosync .
docker run -p 8080:8080 \
  -e GOSYNC_DATABASE_URL=postgres://user:pass@db:5432/gosync \
  -e GOSYNC_JWKS_URL=https://YOUR_TENANT.auth0.com/.well-known/jwks.json \
  -e GOSYNC_JWT_AUDIENCE=https://api.example.com \
  -e GOSYNC_ALLOWED_ORIGINS=https://app.example.com \
  gosync
```

Or `GOSYNC_JWT_SECRET=$(openssl rand -hex 32) docker compose up --build` for a server plus PostgreSQL.

| Variable | Purpose |
|---|---|
| `GOSYNC_ADDR` | Listen address (default `:8080`) |
| `GOSYNC_DATABASE_URL` | `postgres://…` URL or SQLite file path (default `gosync.db`) |
| `GOSYNC_JWT_SECRET` | HS256 secret, at least 32 bytes, **or** |
| `GOSYNC_JWKS_URL` | Your identity provider's JWKS URL |
| `GOSYNC_JWT_ISSUER` / `GOSYNC_JWT_AUDIENCE` | Required `iss` / `aud` claims (recommended) |
| `GOSYNC_NAMESPACE_CLAIM` | Claim that selects whose data a connection sees (default `sub`). Use an org or workspace claim to share data between users. |
| `GOSYNC_ALLOWED_ORIGINS` | Comma-separated browser origins allowed to connect |
| `GOSYNC_TLS_CERT` / `GOSYNC_TLS_KEY` | Serve `wss://` directly (or terminate TLS at your proxy) |
| `GOSYNC_STATIC_DIR` | Optionally serve your web app from the same origin |
| `GOSYNC_LOG_FORMAT` / `GOSYNC_LOG_LEVEL` | `json`/`text`; `debug`…`error` |

Endpoints: `GET /sync` (WebSocket), `GET /healthz` (liveness), `GET /readyz` (database reachable).

**Checklist:** serve over HTTPS/WSS, set `GOSYNC_ALLOWED_ORIGINS`, use JWKS or a strong secret with
issuer and audience checks, back up the database, and serve `gosync.wasm` with brotli or gzip
(about 4 MB raw, roughly 1 MB compressed) and long-lived caching.

## Data model

- Data lives in **collections** of **documents** (`db.set(collection, id, fields)`).
- Field values are any JSON. `set` merges the given fields and leaves the others alone; set a field to `null` to clear it.
- `delete` is a tombstone and is itself last-writer-wins, so a later `set` revives the document.
- Each user (JWT namespace) has a separate dataset. Use one `dbName` per signed-in user in the browser.

## Using GoSync from Go

The engine in [`client`](./client) runs natively too, which is useful for CLIs, services and tests:

```go
c, _ := client.New(ctx, client.Options{
    URL:    "wss://sync.example.com/sync",
    Token:  func(ctx context.Context) (string, error) { return token, nil },
    Store:  client.NewMemoryStore(), // or your own LocalStore
    Dialer: client.WebSocketDialer{},
})
go c.Run(ctx)
c.Set(ctx, "todos", "t1", map[string]json.RawMessage{"title": json.RawMessage(`"Buy milk"`)})
```

The server is an `http.Handler` (`server.New(server.Config{...}).Handler()`), so you can mount it in
an existing Go service and plug in your own `Authenticator` or `store.Store`.

## Project layout

| Path | What |
|---|---|
| `cmd/gosync-server` | Server binary |
| `cmd/gosync-wasm` | Browser engine (Go → WebAssembly) |
| `sdk/js` | npm package: JS API, IndexedDB bridge, multi-tab coordination |
| `client` | Platform-independent sync engine |
| `server` | WebSocket server, auth, change fan-out |
| `store/sqlstore` | SQLite and PostgreSQL storage, Postgres broker |
| `protocol`, `hlc` | Wire protocol and hybrid logical clock, shared by both ends |
| `examples/todo` | Demo app |

## Development

```bash
make test                       # Go tests, race detector (set GOSYNC_TEST_POSTGRES to include PostgreSQL)
make sdk                        # build gosync.wasm + copy the SDK into the demo
node sdk/js/test/e2e.mjs        # browser SDK end-to-end, against a running demo server on :8090
```

## Known limitations

- Last-writer-wins is per field. Concurrent edits to the *same* text field keep one version;
  collaborative text editing needs a sequence CRDT.
- Deleted documents keep a small tombstone forever (no compaction yet).
- Every user's dataset is synced in full to their devices; there are no partial or query-based subscriptions yet.
- Server-side validation is structural (sizes, names, JSON). Add business rules in a custom server wrapper.

## License

MIT License © 2025 Harshal Patel.
