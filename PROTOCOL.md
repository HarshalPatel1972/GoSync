# GoSync Protocol Specification

**Version:** 2  
**Status:** Stable  
**Last Updated:** October 2026

Version 2 replaces the v1 snapshot/hash protocol, which exchanged the full dataset on every
mismatch and overwrote newer data with older data. The two versions are not compatible.

## 1. Overview

GoSync replicates a set of JSON documents between a server and many clients. Clients write to
a local store first and sync opportunistically. The design follows the push/pull/poke model
used by modern sync engines:

- **Push**: a client uploads its queued local writes (*mutations*).
- **Pull**: a client downloads everything that changed since its *cursor*.
- **Poke**: the server tells connected clients that new data exists, so they pull.

Conflicts are resolved by a per-field last-writer-wins (LWW) rule over hybrid logical clocks.
That rule is commutative, associative and idempotent, so all replicas converge regardless of
delivery order or duplication.

## 2. Data model

| Concept | Description |
|---|---|
| Namespace | Isolated dataset, chosen by the server from the authenticated token (default: JWT `sub`). Clients never name it. |
| Collection | `[A-Za-z0-9_.-]{1,64}` |
| Document ID | 1-128 bytes of UTF-8, no NUL |
| Field | Name of 1-64 bytes. `id` and names starting with `_` are reserved, except `_deleted`. |
| Value | Any JSON value, at most 64 KiB |
| Register | `(value, hlc)` per field. The value with the greater HLC wins. |

A document is deleted when its `_deleted` register holds `true`. Writes through the client API
set `_deleted: false`, so a write made after a delete revives the document.

### 2.1 Hybrid logical clock

An HLC timestamp is `(wall_ms, counter, node)`, encoded as

```
%015d-%06d-%s      e.g. 001759683000123-000002-3f9a1c.ab12
```

The fixed-width encoding makes byte order equal timestamp order, so stores compare strings.
`node` is the client ID (plus a per-tab suffix in browsers), 1-64 printable ASCII characters,
and breaks ties. A clock never goes backwards, and after observing a remote timestamp it
always issues later ones. Clients estimate the server clock offset during the handshake to
limit skew, and the server rejects mutations more than 5 minutes in its future.

## 3. Transport

A WebSocket at `GET /sync`, carrying UTF-8 JSON text frames:

```json
{ "type": "<message type>", "data": { ... } }
```

Clients may send frames of at most 1 MiB and should keep at most one push and one pull in
flight. The server processes a connection's frames in order. It sends WebSocket pings every
25 s, and a peer silent for about 60 s is disconnected. Browsers must connect from an allowed
`Origin`.

## 4. Messages

### 4.1 `hello` (client → server, first frame, within 10 s)

```json
{ "version": 2, "token": "<JWT>", "clientId": "3f9a1c0d22e4b5a6c7d8e9f0", "collections": ["todos"] }
```

`clientId` identifies the replica's outbox: `[A-Za-z0-9_.-]{1,64}`, stable for the lifetime of
the local store. `collections` (optional, at most 64) limits pulls to those collections; writes
to any collection are still accepted. A cursor is only valid for the collection set it was
pulled with, so clients keep one cursor per set.

### 4.2 `welcome` (server → client)

```json
{ "serverTime": 1759683000123, "lastMutationId": 41 }
```

`lastMutationId` is the highest mutation ID the server has applied for this client. The client
drops those entries from its outbox, which recovers from a lost `pushResult`.

### 4.3 `push` (client → server)

```json
{ "mutations": [
  { "id": 42, "c": "todos", "d": "t1", "t": "001759683000123-000000-3f9a1c.ab12",
    "f": { "title": "Buy milk", "done": false, "_deleted": false } }
] }
```

At most 100 mutations, with IDs strictly increasing. A mutation holds at most 256 KiB of field
names and values, and clients keep push frames under 768 KiB, so a batch always fits the
1 MiB frame limit. The server, in one transaction per push:

1. Skips mutations with `id <= lastMutationId` (idempotency).
2. Skips mutations on a compacted document whose HLC is at or below the purge HLC (see 4.5).
3. For each field, stores `(value, t)` only if `t` is greater than the stored HLC.
4. Tags changed fields with the namespace's new version and records the new `lastMutationId`.

### 4.4 `pushResult` (server → client)

```json
{ "lastMutationId": 42, "rejected": [ { "id": 40, "reason": "timestamp is too far in the future" } ] }
```

Every mutation up to `lastMutationId` is acknowledged and the client removes it from the outbox.
Rejected mutations are acknowledged too; they will never apply.

### 4.5 `pull` (client → server) and `pullResult` (server → client)

```json
{ "cursor": 17 }
```

```json
{ "cursor": 19, "more": false, "changes": [
  { "c": "todos", "d": "t1", "f": { "done": { "v": true, "t": "001759683000456-000000-77aa01" } } }
] }
```

Returns the current register of every field whose version is greater than `cursor` (limited to
the hello's `collections`, if any). Responses
are cut only at version boundaries (about 2 MiB each); when `more` is true the client pulls
again. The client merges changes with the same LWW rule and stores `cursor` in the same
transaction. If a cursor is ahead of the server (for example after a database restore), the
server answers as if the cursor were 0.

A change may carry `"purged": "<hlc>"` instead of (or as well as) fields. The server compacted the
document after a deletion with that HLC. Replicas drop every field whose HLC is at or below it,
then merge any fields in the change, and remove the document if no fields remain. Purge records
are sent only to clients with a non-zero cursor, since fresh clients never held the document.

### 4.6 `poke` (server → client)

No payload. Something changed in the namespace and the client should pull. Pokes coalesce,
and the client that caused a change is not poked.

### 4.7 `error` (server → client)

```json
{ "code": "unauthorized", "message": "invalid or expired token", "fatal": true }
```

| Code | Meaning |
|---|---|
| `unauthorized` | Bad or expired token. Also sent when a token expires mid-session; reconnect with a fresh one. |
| `bad_request` | Protocol violation; the connection closes. |
| `rate_limited` | Too many frames (default 50/s, burst 100). |
| `unsupported_version` | Wrong `hello.version`. |
| `internal` | Server error; the client reconnects with backoff. |

## 5. Server versioning and correctness

Each namespace has a monotonically increasing version. A push increments it while holding the
namespace's row lock, so versions commit in order and a cursor can never skip a concurrent
transaction. A version is `max(previous + 1, current time in µs)`, so versions keep increasing
even after the database is restored from a backup, and cursors issued before a restore never
skip writes made after it. Clients must treat cursors as opaque integers below 2^53. Each field row stores the version that last changed it. A pull reads the
namespace version and the changed rows in one snapshot.

## 6. Client behaviour

- **Writes:** stamp a new HLC, merge into the local document and append to the outbox in one
  local transaction, then signal the sync loop.
- **Sync loop:** connect → `hello` → `welcome` → ack the outbox up to `lastMutationId` → push
  pending mutations in batches and pull from the stored cursor → pull again on every `poke`.
- **Reconnect:** exponential backoff with jitter (0.5 s to 30 s), reset only after a session
  stays up for 10 s, and skipped when the browser reports it is back online.
- **Browsers:** one tab per database holds the connection (Web Locks API). Other tabs write
  to the shared IndexedDB outbox and are notified over BroadcastChannel.

## 7. Security

- Authentication is by bearer token in `hello` (never in the URL, which ends up in logs).
- All data access is scoped to the namespace derived from the verified token.
- Browser origins are checked; frame, value, batch and rate limits bound resource use.
- Run behind TLS (`wss://`).
