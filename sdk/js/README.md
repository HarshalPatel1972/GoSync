# @harshalpatel2868/gosync-client

Offline-first, real-time sync for web apps, backed by a self-hosted [GoSync](https://github.com/HarshalPatel1972/GoSync) server.

Your app reads and writes a local IndexedDB database, so it is instant and works offline. Changes
sync to your server and to the user's other devices and tabs in real time, and conflicts resolve
automatically (per-field last-writer-wins over hybrid logical clocks).

```bash
npm install @harshalpatel2868/gosync-client
```

```js
import { createClient } from '@harshalpatel2868/gosync-client';

const db = await createClient({
  url: 'wss://sync.example.com/sync',
  getToken: () => auth.getIdToken(), // JWT from your auth provider
  dbName: `app-${user.id}`,          // one local database per signed-in user
});

await db.set('todos', crypto.randomUUID(), { title: 'Buy milk', done: false }); // works offline
db.watch('todos', (todos) => render(todos)); // local, other-tab and remote changes
db.onStatus((s) => console.log(s));          // 'offline' | 'connecting' | 'online'
db.onError((e) => console.warn(e.message));  // e.g. a write the server rejected
```

## API

| Method | Description |
|---|---|
| `set(collection, id, fields)` | Create or update a document. Only the given fields change; `null` clears one. |
| `delete(collection, id)` | Delete a document (a later `set` revives it). |
| `get(collection, id)` | One document as `{ id, ...fields }`, or `null`. |
| `list(collection)` | All documents in a collection. |
| `watch(collection, cb)` | Calls `cb(docs)` now and after every change. Returns an unsubscribe function. |
| `subscribe([collection,] cb)` | Change events `{ collection, ids }`. |
| `onStatus(cb)`, `onError(cb)` | Connection status and server errors. |
| `close()` | Stop syncing and release the database. |

Options: `url`, `getToken` or `token`, `dbName`, `collections` (pull only these), `wasmUrl`, `debug`.
TypeScript types are included.

## Serving the engine

The sync engine is Go compiled to WebAssembly (`gosync.wasm`, loaded from next to `gosync.js`
by default). Serve the precompressed `gosync.wasm.br` / `.gz` files that ship with the package
(about 0.8 MB instead of 4 MB) with `Content-Type: application/wasm`, or pass `wasmUrl` to load it
from your CDN.

## Server

Run the GoSync server (one Go binary, SQLite or PostgreSQL, JWT/JWKS auth). See the
[main README](https://github.com/HarshalPatel1972/GoSync) and the
[operations guide](https://github.com/HarshalPatel1972/GoSync/blob/main/docs/OPERATIONS.md).

MIT License.
