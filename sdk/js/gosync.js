// GoSync browser client.
//
//   import { createClient } from '@harshalpatel2868/gosync-client';
//   const db = await createClient({ url: 'wss://sync.example.com/sync', getToken });
//   await db.set('todos', id, { title: 'Buy milk', done: false });
//   db.watch('todos', todos => render(todos));
//
// The sync engine is Go compiled to WebAssembly (gosync.wasm). This file
// provides what Go cannot do well from WASM: IndexedDB transactions and
// coordination between browser tabs. One tab per database (the leader,
// elected with the Web Locks API) holds the server connection; other tabs
// read and write the shared IndexedDB and learn about changes over a
// BroadcastChannel.

const DEFAULT_WASM_URL = new URL('./gosync.wasm', import.meta.url);
const WASM_EXEC_URL = new URL('./wasm_exec.js', import.meta.url);

let runtimePromise;

// Go's wasm_exec.js is a classic script that defines globalThis.Go. In a
// browser it is loaded with a <script> tag rather than import(): bundlers
// (Next.js with Turbopack or webpack) try to resolve dynamic imports
// themselves and fail, whereas new URL(..., import.meta.url) above is the
// standard way to make every bundler emit the file as an asset.
function loadWasmExec() {
  if (globalThis.Go) return Promise.resolve();
  if (typeof document === 'undefined') {
    return import(/* webpackIgnore: true */ /* @vite-ignore */ WASM_EXEC_URL.href); // Node, workers
  }
  return new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = WASM_EXEC_URL.href;
    script.async = true;
    script.onload = () => resolve();
    script.onerror = () => reject(new Error(`gosync: could not load ${WASM_EXEC_URL.href}`));
    document.head.appendChild(script);
  });
}

function loadRuntime(wasmUrl) {
  runtimePromise ??= (async () => {
    await loadWasmExec();
    const go = new globalThis.Go();
    const ready = new Promise((resolve) => { globalThis.__gosyncReady = resolve; });
    let instance;
    try {
      ({ instance } = await WebAssembly.instantiateStreaming(fetch(wasmUrl), go.importObject));
    } catch {
      // Servers that do not send Content-Type: application/wasm.
      const bytes = await (await fetch(wasmUrl)).arrayBuffer();
      ({ instance } = await WebAssembly.instantiate(bytes, go.importObject));
    }
    go.run(instance);
    await ready;
    return globalThis.__gosyncCreate;
  })();
  runtimePromise.catch(() => { runtimePromise = undefined; });
  return runtimePromise;
}

// ---------------------------------------------------------------------------
// IndexedDB bridge. Each method is one transaction. Documents are stored as
// {c, d, f: {field: {v: <raw JSON string>, t: <HLC>}}}.

function request(r) {
  return new Promise((resolve, reject) => {
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}

function completion(tx) {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error ?? new DOMException('Transaction aborted', 'AbortError'));
  });
}

function openDatabase(name) {
  const r = indexedDB.open(name, 1);
  r.onupgradeneeded = () => {
    const db = r.result;
    db.createObjectStore('docs', { keyPath: ['c', 'd'] }).createIndex('c', 'c');
    db.createObjectStore('outbox', { keyPath: 'id', autoIncrement: true });
    db.createObjectStore('meta');
  };
  return request(r);
}

// Last-writer-wins per field; must match client.MergeFields in Go.
function merge(fields, incoming) {
  for (const [name, value] of Object.entries(incoming)) {
    const current = fields[name];
    if (!current || current.t < value.t) fields[name] = value;
  }
}

// Server change: a purge (tombstone compaction) drops every field at or
// below its HLC, then fields merge. Must match client.ApplyChange in Go.
function applyChange(fields, change) {
  if (change.purged) {
    for (const [name, value] of Object.entries(fields)) {
      if (value.t <= change.purged) delete fields[name];
    }
  }
  merge(fields, change.f);
}

function randomHex(bytes) {
  const b = crypto.getRandomValues(new Uint8Array(bytes));
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

function createBridge(db) {
  const tx = (stores, mode = 'readonly') => db.transaction(stores, mode);
  const bumpMaxHlc = async (meta, hlc) => {
    const current = (await request(meta.get('maxHlc'))) ?? '';
    if (hlc > current) meta.put(hlc, 'maxHlc');
  };

  return {
    async clientId() {
      const t = tx('meta', 'readwrite');
      const meta = t.objectStore('meta');
      let id = await request(meta.get('clientId'));
      if (!id) {
        id = randomHex(12);
        meta.put(id, 'clientId');
      }
      await completion(t);
      return id;
    },
    async cursor(scope) {
      return (await request(tx('meta').objectStore('meta').get('cursor:' + scope))) ?? 0;
    },
    async maxHlc() {
      return (await request(tx('meta').objectStore('meta').get('maxHlc'))) ?? '';
    },
    async write(c, d, fieldsJSON, hlc) {
      const raw = JSON.parse(fieldsJSON); // {field: rawJSONString}
      const t = tx(['docs', 'outbox', 'meta'], 'readwrite');
      const docs = t.objectStore('docs');
      const doc = (await request(docs.get([c, d]))) ?? { c, d, f: {} };
      const incoming = {};
      for (const [name, v] of Object.entries(raw)) incoming[name] = { v, t: hlc };
      merge(doc.f, incoming);
      docs.put(doc);
      const id = await request(t.objectStore('outbox').add({ c, d, t: hlc, f: raw }));
      await bumpMaxHlc(t.objectStore('meta'), hlc);
      await completion(t);
      return id;
    },
    async applyRemote(changesJSON, scope, cursor) {
      const changes = JSON.parse(changesJSON);
      const t = tx(['docs', 'meta'], 'readwrite');
      const docs = t.objectStore('docs');
      let max = '';
      for (const ch of changes) {
        const doc = (await request(docs.get([ch.c, ch.d]))) ?? { c: ch.c, d: ch.d, f: {} };
        applyChange(doc.f, ch);
        for (const v of Object.values(ch.f)) if (v.t > max) max = v.t;
        if (ch.purged && ch.purged > max) max = ch.purged;
        if (Object.keys(doc.f).length === 0) docs.delete([ch.c, ch.d]);
        else docs.put(doc);
      }
      const meta = t.objectStore('meta');
      meta.put(cursor, 'cursor:' + scope);
      if (max) await bumpMaxHlc(meta, max);
      await completion(t);
    },
    async revert(c, d, hlc, currentJSON) {
      const current = JSON.parse(currentJSON);
      const t = tx('docs', 'readwrite');
      const docs = t.objectStore('docs');
      const doc = await request(docs.get([c, d]));
      if (doc) {
        // Same as client.RevertFields in Go.
        for (const [name, value] of Object.entries(doc.f)) if (value.t === hlc) delete doc.f[name];
      }
      const next = doc ?? { c, d, f: {} };
      merge(next.f, current);
      if (Object.keys(next.f).length === 0) docs.delete([c, d]);
      else docs.put(next);
      await completion(t);
    },
    async pending(limit) {
      return JSON.stringify(await request(tx('outbox').objectStore('outbox').getAll(null, limit)));
    },
    async ack(upTo) {
      const t = tx('outbox', 'readwrite');
      t.objectStore('outbox').delete(IDBKeyRange.upperBound(upTo));
      await completion(t);
    },
    async get(c, d) {
      const doc = await request(tx('docs').objectStore('docs').get([c, d]));
      return doc ? JSON.stringify(doc) : null;
    },
    async list(c) {
      return JSON.stringify(await request(tx('docs').objectStore('docs').index('c').getAll(c)));
    },
  };
}

// ---------------------------------------------------------------------------

/**
 * Opens a GoSync database in this browser and starts syncing it.
 * See index.d.ts for the full API.
 */
export async function createClient(options = {}) {
  const { url, getToken, token, dbName = 'gosync', wasmUrl = DEFAULT_WASM_URL, collections, debug = false } = options;
  if (!url) throw new TypeError('createClient: `url` is required, e.g. "wss://sync.example.com/sync"');

  const [create, db] = await Promise.all([loadRuntime(wasmUrl), openDatabase(dbName)]);
  const bridge = createBridge(db);
  const engine = await create({
    url: String(url),
    getToken: getToken ?? (() => token ?? ''),
    bridge,
    nodeSuffix: randomHex(4),
    collections: collections ? Array.from(collections, String) : undefined,
    debug,
  });

  const channel = 'BroadcastChannel' in globalThis ? new BroadcastChannel(`gosync:${dbName}`) : null;
  const changeListeners = new Set();
  const statusListeners = new Set();
  const errorListeners = new Set();
  let status = 'offline';
  let isLeader = false;
  let closed = false;
  let stopSync = null;
  let releaseLock = null;
  const lockAbort = new AbortController();

  const emitChange = (collection, ids) => {
    for (const l of changeListeners) {
      if (l.collection === null || l.collection === collection) {
        try { l.callback({ collection, ids }); } catch (e) { console.error('[gosync] change listener threw', e); }
      }
    }
  };
  const setStatus = (s) => {
    if (s === status) return;
    status = s;
    for (const cb of statusListeners) cb(s);
  };

  const unsubscribeEngine = engine.subscribe((ev) => {
    if (ev.kind === 'change') {
      const ids = Array.from(ev.ids);
      emitChange(ev.collection, ids);
      channel?.postMessage({ type: 'change', collection: ev.collection, ids });
    } else if (ev.kind === 'status') {
      if (isLeader) {
        setStatus(ev.status);
        channel?.postMessage({ type: 'status', status: ev.status });
      }
    } else if (ev.kind === 'error') {
      const err = new Error(ev.error);
      if (errorListeners.size === 0) console.warn('[gosync]', err.message);
      for (const cb of errorListeners) cb(err);
    }
  });

  if (channel) {
    channel.onmessage = async ({ data }) => {
      switch (data?.type) {
        case 'change':
          // Keep this tab's clock ahead of writes made in other tabs.
          engine.observeHlc(await bridge.maxHlc());
          emitChange(data.collection, data.ids);
          break;
        case 'outbox':
          if (isLeader) engine.notifyOutbox();
          break;
        case 'status':
          if (!isLeader) setStatus(data.status);
          break;
        case 'status?':
          if (isLeader) channel.postMessage({ type: 'status', status });
          break;
      }
    };
  }

  const becomeLeader = () => {
    isLeader = true;
    stopSync = engine.run();
  };
  if (globalThis.navigator?.locks) {
    navigator.locks
      .request(`gosync:${dbName}`, { signal: lockAbort.signal }, () => {
        if (closed) return undefined;
        becomeLeader();
        return new Promise((resolve) => { releaseLock = resolve; });
      })
      .catch(() => {}); // aborted on close
    channel?.postMessage({ type: 'status?' });
  } else {
    becomeLeader();
  }

  const onOnline = () => engine.reconnect();
  globalThis.addEventListener?.('online', onOnline);

  const toDoc = ({ id, fields }) => ({ ...fields, id });
  const ensureOpen = () => { if (closed) throw new Error('gosync: client is closed'); };
  const afterWrite = () => channel?.postMessage({ type: 'outbox' });

  const api = {
    get clientId() { return engine.clientId(); },
    get status() { return status; },
    get isLeader() { return isLeader; },

    async set(collection, id, fields) {
      ensureOpen();
      if (fields === null || typeof fields !== 'object' || Array.isArray(fields)) {
        throw new TypeError('gosync: fields must be a plain object');
      }
      const { id: _ignored, ...rest } = fields;
      await engine.set(String(collection), String(id), JSON.stringify(rest));
      afterWrite();
    },

    async delete(collection, id) {
      ensureOpen();
      await engine.delete(String(collection), String(id));
      afterWrite();
    },

    async get(collection, id) {
      ensureOpen();
      const json = await engine.get(String(collection), String(id));
      return json ? toDoc(JSON.parse(json)) : null;
    },

    async list(collection) {
      ensureOpen();
      return JSON.parse(await engine.list(String(collection))).map(toDoc);
    },

    subscribe(collectionOrCallback, maybeCallback) {
      const listener = typeof collectionOrCallback === 'function'
        ? { collection: null, callback: collectionOrCallback }
        : { collection: String(collectionOrCallback), callback: maybeCallback };
      changeListeners.add(listener);
      return () => changeListeners.delete(listener);
    },

    watch(collection, callback) {
      let active = true;
      let queued = false;
      const refresh = () => {
        if (queued) return;
        queued = true;
        queueMicrotask(async () => {
          queued = false;
          if (!active || closed) return;
          try { callback(await api.list(collection)); } catch (e) { console.error('[gosync] watch failed', e); }
        });
      };
      const unsubscribe = api.subscribe(collection, refresh);
      refresh();
      return () => { active = false; unsubscribe(); };
    },

    onStatus(callback) {
      statusListeners.add(callback);
      callback(status);
      return () => statusListeners.delete(callback);
    },

    onError(callback) {
      errorListeners.add(callback);
      return () => errorListeners.delete(callback);
    },

    close() {
      if (closed) return;
      closed = true;
      stopSync?.();
      releaseLock?.();
      lockAbort.abort();
      unsubscribeEngine();
      globalThis.removeEventListener?.('online', onOnline);
      channel?.close();
      changeListeners.clear();
      statusListeners.clear();
      errorListeners.clear();
      db.close();
    },
  };
  return api;
}

// Exposed for the SDK's own tests; not part of the public API.
export const _internals = { createBridge, openDatabase };
