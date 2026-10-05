# GoSync

![Build Status](https://github.com/HarshalPatel1972/GoSync/actions/workflows/ci.yml/badge.svg)

**Offline-first, real-time sync for web apps. Self-hosted, written in Go.**

[🌐 Website](https://gosync-zero.vercel.app) · [📖 Protocol](./PROTOCOL.md) · [🛠 Operations](./docs/OPERATIONS.md) · [📦 NPM Package](https://www.npmjs.com/package/@harshalpatel2868/gosync-client) · [🎨 Website Repo](https://github.com/HarshalPatel1972/gosync-zero)

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
  Measured: 1,000+ writes/s across 2,000 devices with a p99 under 100 ms ([details](./docs/OPERATIONS.md#capacity-measured)).
- **Partial sync.** A client can pull only the collections it needs (`collections: ['todos']`).
- **Stays small.** Deleted documents are compacted after a retention period, and devices that
  were offline the whole time still converge.
- **Observable.** Prometheus metrics, pprof, structured logs and health checks.
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
| `GOSYNC_METRICS_ADDR` | Private listener for Prometheus `/metrics` and `/debug/pprof` (off by default) |
| `GOSYNC_TOMBSTONE_RETENTION` | Compact deleted documents after this long (default `720h`) |
| `GOSYNC_DB_MAX_CONNS` | PostgreSQL pool size per instance (default 25) |

Endpoints: `GET /sync` (WebSocket), `GET /healthz` (liveness), `GET /readyz` (database reachable).

Before launch, work through the [operations guide](./docs/OPERATIONS.md): deployment checklist,
scaling, monitoring, backups and upgrades. Commands: `gosync-server migrate` applies schema
migrations, and `gosync-server compact` runs tombstone compaction once.

## Data model

- Data lives in **collections** of **documents** (`db.set(collection, id, fields)`).
- Field values are any JSON. `set` merges the given fields and leaves the others alone; set a field to `null` to clear it.
- `delete` is a tombstone and is itself last-writer-wins, so a later `set` revives the document.
- Each user (JWT namespace) has a separate dataset. Use one `dbName` per signed-in user in the browser.
- Limits: 64 KiB per field value and 256 KiB per write.

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
| `cmd/gosync-loadtest` | Load generator measuring end-to-end propagation latency |
| `examples/todo` | Demo app |
| `docs/OPERATIONS.md` | Running GoSync in production |

## Development

```bash
make test                     # Go tests, race detector (set GOSYNC_TEST_POSTGRES to include PostgreSQL)
make sdk                      # build gosync.wasm (+ .br/.gz) and copy the SDK into the demo
cd sdk/js && npm install && npx playwright install
npm run test:e2e              # SDK end-to-end (WASM + IndexedDB) against a demo server on :8090
npm run test:browser          # multi-tab, failover and persistence in Chromium, Firefox and WebKit
```

CI runs all of these, plus a load test with two server instances on PostgreSQL.

## Known limitations

- Last-writer-wins is per field. Concurrent edits to the *same* text field keep one version;
  collaborative text editing needs a sequence CRDT, which GoSync doesn't provide yet.
- The browser engine is Go compiled to WebAssembly: about 0.8 MB with brotli, mostly the Go
  runtime. Load it after first paint (`createClient` is async) and cache it.
- A device whose clock is more than 5 minutes fast has its writes rejected (reported through
  `onError`) rather than letting them win every conflict.
- Server-side checks are structural (sizes, names, JSON) unless you embed the server and set
  `server.Config.ValidateMutation` for permissions and business rules. A rejected write is
  undone on the device that made it and reported through `onError`.

## License

MIT License © 2025 Harshal Patel.
