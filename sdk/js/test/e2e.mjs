// End-to-end test of the real browser SDK (gosync.js + gosync.wasm) against a
// running server, with fake-indexeddb standing in for the browser's IndexedDB.
import 'fake-indexeddb/auto';
import { createClient } from '../gosync.js';

// Expects a server started with GOSYNC_INSECURE_DEV_AUTH=true and
// GOSYNC_STATIC_DIR serving the built SDK at /gosync/ (see CI).
const BASE = process.env.GOSYNC_TEST_URL ?? 'http://127.0.0.1:8090';
const WASM = BASE + '/gosync/gosync.wasm';
const URL_OK = BASE.replace(/^http/, 'ws') + '/sync';
const URL_DOWN = 'ws://127.0.0.1:1/sync'; // nothing listens: simulates offline

const open = (dbName, user, url = URL_OK) => createClient({ url, token: user, dbName, wasmUrl: WASM });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(what, fn, ms = 10000) {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (await fn()) return; await sleep(50); }
  throw new Error('timed out: ' + what);
}
let passed = 0;
const ok = (msg) => { passed++; console.log('  ✓', msg); };

const run = Date.now().toString(36); // fresh data each run
const A = await open('A-' + run, 'alice-' + run);
const B = await open('B-' + run, 'alice-' + run);
const bob = await open('C-' + run, 'bob-' + run);
await until('all online', () => [A, B, bob].every((c) => c.status === 'online'));
ok('three clients online');

const changes = [];
B.subscribe('todos', (e) => changes.push(e));
await A.set('todos', 't1', { title: 'Buy milk <script>', done: false, meta: { tags: ['a', 'b'], n: 1.5 } });
await until('B receives A\'s write', async () => (await B.get('todos', 't1'))?.title === 'Buy milk <script>');
const t1 = await B.get('todos', 't1');
if (JSON.stringify(t1.meta) !== '{"tags":["a","b"],"n":1.5}') throw new Error('nested value mangled: ' + JSON.stringify(t1));
if (!changes.some((e) => e.ids.includes('t1'))) throw new Error('no change event on B');
ok('real-time sync A → B with nested JSON intact and change event fired');

await B.set('todos', 't1', { done: true });
await until('A sees B\'s field update', async () => (await A.get('todos', 't1'))?.done === true);
if ((await A.get('todos', 't1')).title !== 'Buy milk <script>') throw new Error('partial update clobbered title');
ok('partial update merges (title kept, done updated)');

await B.delete('todos', 't1');
await until('delete reaches A', async () => (await A.get('todos', 't1')) === null);
ok('delete propagates');

await sleep(300);
if ((await bob.list('todos')).length !== 0) throw new Error('bob sees alice\'s data');
ok('another user sees nothing');

// Offline: write with no server, close the "app", reopen online.
let off = await open('E-' + run, 'alice-' + run, URL_DOWN);
for (let i = 0; i < 120; i++) await off.set('notes', 'n' + i, { i });
if ((await off.list('notes')).length !== 120) throw new Error('offline writes not readable locally');
if (off.status === 'online') throw new Error('should be offline');
off.close();
ok('120 writes succeed offline and are readable locally');

off = await open('E-' + run, 'alice-' + run); // same IndexedDB, server reachable now
if ((await off.list('notes')).length !== 120) throw new Error('data lost across restart');
ok('data survives app restart (IndexedDB)');
await until('outbox reaches other device', async () => (await A.list('notes')).length === 120);
ok('offline outbox syncs to the other device after reconnect');

// Concurrent edits on the same doc while one side is offline converge.
await A.set('docs', 'x', { title: 'v0', body: 'v0' });
await until('x everywhere', async () => (await off.get('docs', 'x'))?.title === 'v0');
off.close();
let offline = await open('E-' + run, 'alice-' + run, URL_DOWN);
await offline.set('docs', 'x', { title: 'from offline device' });
await sleep(10);
await A.set('docs', 'x', { body: 'from A' });
offline.close();
offline = await open('E-' + run, 'alice-' + run);
const want = JSON.stringify({ body: 'from A', title: 'from offline device', id: 'x' });
const norm = (d) => JSON.stringify({ body: d?.body, title: d?.title, id: d?.id });
await until('concurrent edits converge', async () => norm(await offline.get('docs', 'x')) === want && norm(await A.get('docs', 'x')) === want);
ok('concurrent offline edits to different fields merge on both replicas');

for (const c of [A, B, bob, offline]) c.close();
console.log(`\n${passed} checks passed`);
process.exit(0);
