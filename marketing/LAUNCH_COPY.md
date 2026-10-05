# GoSync v2 Launch Copy

Ready-to-post copy for the v2.0 launch. Every number here is measured (see
`docs/OPERATIONS.md#capacity-measured`) and every claim matches the code; keep it that way
when editing.

Links used below:

- Website and docs: https://gosync-zero.vercel.app
- Repo: https://github.com/HarshalPatel1972/GoSync
- npm: https://www.npmjs.com/package/@harshalpatel2868/gosync-client
- Article: publish `DEVTO_ARTICLE_V2.md` from the website repo first, then link it.

**Suggested order:** Dev.to article → Show HN → r/golang → r/webdev → X/LinkedIn. Space them
a few hours apart so you can answer comments.

---

## 1. Hacker News (Show HN)

**Title** (80 characters max):

> Show HN: GoSync – Self-hosted offline-first sync for web apps, written in Go

**URL:** https://github.com/HarshalPatel1972/GoSync

**Text:**

Hi HN,

GoSync is an open-source sync engine that makes a web app work offline and stay in sync across a user's devices and tabs in real time. You run the server yourself: one Go binary on SQLite or Postgres.

Your app reads and writes a local IndexedDB database through a small API (`db.set`, `db.watch`), so every interaction is instant. Changes go to a local outbox, sync to the server when there's a connection, and are pushed to the user's other devices immediately.

How it works:

- Push/pull/poke: devices upload only their pending writes (idempotent, so retries are safe) and download only what changed since their cursor; the server pokes other devices when data changes.
- Conflicts: every field is a last-writer-wins register ordered by a hybrid logical clock, so concurrent edits to different fields both survive and every replica converges.
- Auth: JWT, or JWKS from Auth0/Clerk/Supabase/Firebase, with per-user data isolation.
- Multi-tab: one tab holds the connection (Web Locks); others update over BroadcastChannel.
- Scale-out: several server instances on Postgres coordinate via LISTEN/NOTIFY.

The browser engine is the same Go code compiled to WebAssembly (about 0.8 MB with brotli; that's the main trade-off).

It's tested in Chromium, Firefox and WebKit, and load-tested: 2,000 simulated devices at ~1,000 writes/s on SQLite with p99 propagation under 100 ms, 100% delivery. The load tests found two good bugs: a 4.5 s SQLite tail latency (fixed with group commit and prepared statements), and Postgres serialising every NOTIFY-ing commit behind one lock (fixed by batching).

It's not for collaborative editing of the same text (use Yjs/Automerge) or for syncing huge shared datasets to every client.

v1 was a prototype with real problems (no auth, old data could overwrite new data); v2 is a ground-up rebuild. I'd love feedback from anyone who has built sync before.

---

## 2. Reddit: r/golang

**Title:**

> I rebuilt my offline-first sync engine in Go: HLC conflict resolution, Postgres LISTEN/NOTIFY fan-out, and a Go→WASM browser client

**Text:**

Hey Gophers,

A while ago I posted GoSync, a sync engine that runs Go on the server and (via WASM) in the browser. v1 was a fun prototype but not something you'd trust with data, so I rebuilt it. v2.0 is out.

**What it is:** offline-first, real-time sync for web apps. The browser writes to IndexedDB and a local outbox; the server is one Go binary (`net/http` handler) on SQLite or Postgres.

**The Go parts you might find interesting:**

- **Hybrid logical clocks** (`hlc` package) for per-field last-writer-wins, encoded as fixed-width strings so SQL can compare them bytewise (`COLLATE "C"` on Postgres).
- **Namespace versioning:** each push bumps a per-namespace version under a row lock, so cursors can never skip a slower concurrent transaction. Versions are `max(prev+1, now µs)` so they survive a restore from backup.
- **SQLite group commit:** one writer goroutine batches concurrent pushes into one transaction with a savepoint per push. That took p99 from 4.5 s to under 100 ms at 1k writes/s.
- **Postgres fan-out:** LISTEN/NOTIFY, batched every 10 ms, because Postgres serialises the commit of every transaction that issued a NOTIFY.
- **WASM client:** the sync engine is plain Go behind `Dialer` and `LocalStore` interfaces, so the same code runs natively in tests (with `-race`) and in the browser.

It's tested with `go test -race` on SQLite and Postgres, in real browsers with Playwright, and with a load generator in CI.

Repo: https://github.com/HarshalPatel1972/GoSync

I'd welcome reviews, especially of `store/sqlstore` and `server/conn.go`.

---

## 3. Reddit: r/webdev (or r/javascript)

**Title:**

> I built a self-hosted alternative to Firebase-style offline sync: IndexedDB + real-time + automatic conflict resolution

**Text:**

Making a web app work offline sounds easy until you hit two tabs, two devices, flaky networks and edits that collide.

GoSync handles that for you:

```js
import { createClient } from '@harshalpatel2868/gosync-client';

const db = await createClient({ url: 'wss://sync.example.com/sync', getToken: () => auth.getIdToken() });

await db.set('todos', id, { title: 'Buy milk', done: false }); // instant, works offline
db.watch('todos', render); // updates from other tabs and devices
```

- Works offline, survives reloads, syncs when the connection returns
- Real-time across every device and tab
- Concurrent edits to different fields both survive; every device converges
- Works with your existing auth (Auth0, Clerk, Supabase, Firebase)
- Self-hosted, MIT licensed, no per-seat pricing

Docs: https://gosync-zero.vercel.app/docs/quick-start
Repo: https://github.com/HarshalPatel1972/GoSync

---

## 4. X / Twitter

**Post 1:**

> GoSync v2 is out 🚀
>
> Offline-first, real-time sync for web apps, self-hosted and written in Go.
>
> ✅ Works offline (IndexedDB)
> ✅ Syncs every device & tab live
> ✅ Automatic conflict resolution
> ✅ Your auth, your server, your data
>
> https://gosync-zero.vercel.app

**Post 2 (reply, for the thread):**

> Under the hood: an outbox + cursor sync protocol, per-field last-writer-wins on hybrid logical clocks, and Postgres LISTEN/NOTIFY to scale out.
>
> Load-tested: ~1,000 writes/s across 2,000 devices, p99 < 100 ms, 0 lost writes.
>
> Code: https://github.com/HarshalPatel1972/GoSync

---

## 5. LinkedIn

> I just released **GoSync v2.0**, an open-source, offline-first sync engine for web apps.
>
> Making apps work offline sounds simple, but the hard parts (queued writes, retries, multiple tabs and devices, clock skew, conflicting edits) are where data gets lost. GoSync handles those so developers don't have to:
>
> • Apps keep working offline and sync automatically when back online
> • Changes appear on every device and browser tab in real time
> • Conflicting edits merge deterministically, so every device ends up with the same data
> • Self-hosted on your own infrastructure (Go server, SQLite or PostgreSQL), with your existing auth
>
> v2 is a ground-up rebuild of my first prototype. It's tested in Chromium, Firefox and WebKit and load-tested at ~1,000 writes per second with 99th-percentile sync latency under 100 ms.
>
> Docs: https://gosync-zero.vercel.app
> Code (MIT): https://github.com/HarshalPatel1972/GoSync
>
> #opensource #golang #webdevelopment #offlinefirst
