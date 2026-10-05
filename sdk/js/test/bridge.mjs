// Unit tests for the IndexedDB bridge's merge rules. They must match the Go
// implementations in client/store.go exactly (MergeFields, ApplyChange,
// RevertFields), since browsers use these and native clients use Go.
import 'fake-indexeddb/auto';
import assert from 'node:assert/strict';
import { _internals } from '../gosync.js';

const db = await _internals.openDatabase('bridge-test-' + Date.now());
const b = _internals.createBridge(db);
const doc = async (c, d) => { const j = await b.get(c, d); return j ? JSON.parse(j).f : undefined; };
const values = (f) => Object.fromEntries(Object.entries(f).map(([k, v]) => [k, v.v]));

// Local write, then an older remote value loses and a newer one wins.
await b.write('c', 'x', JSON.stringify({ a: '1', b: '"x"' }), 't05');
await b.applyRemote(JSON.stringify([{ c: 'c', d: 'x', f: { a: { v: '0', t: 't01' }, b: { v: '"y"', t: 't09' } } }]), '', 7);
assert.deepEqual(values(await doc('c', 'x')), { a: '1', b: '"y"' });
assert.equal(await b.cursor(''), 7);
assert.equal(await b.maxHlc(), 't09');

// Raw JSON survives untouched (no re-encoding of big integers).
await b.write('c', 'big', JSON.stringify({ n: '9007199254740993' }), 't10');
assert.equal((await doc('c', 'big')).n.v, '9007199254740993');

// Purge drops fields at or below the purge HLC, keeps later ones, and
// removes the document when nothing is left.
await b.write('c', 'p', JSON.stringify({ old: '1' }), 't02');
await b.write('c', 'p', JSON.stringify({ newer: '2' }), 't20');
await b.applyRemote(JSON.stringify([{ c: 'c', d: 'p', f: {}, purged: 't10' }]), '', 8);
assert.deepEqual(values(await doc('c', 'p')), { newer: '2' });
await b.applyRemote(JSON.stringify([{ c: 'c', d: 'p', f: {}, purged: 't30' }]), '', 9);
assert.equal(await doc('c', 'p'), undefined);

// Revert removes only the rejected write's fields and restores server state.
await b.applyRemote(JSON.stringify([{ c: 'c', d: 'r', f: { text: { v: '"official"', t: 't03' } } }]), '', 10);
await b.write('c', 'r', JSON.stringify({ text: '"hacked"', extra: 'true' }), 't40');
await b.revert('c', 'r', 't40', JSON.stringify({ text: { v: '"official"', t: 't03' } }));
assert.deepEqual(values(await doc('c', 'r')), { text: '"official"' });
// Reverting a write to a document the server never had removes it.
await b.write('c', 'ghost', JSON.stringify({ a: '1' }), 't41');
await b.revert('c', 'ghost', 't41', '{}');
assert.equal(await doc('c', 'ghost'), undefined);

// Per-scope cursors are independent.
await b.applyRemote('[]', 'todos', 3);
assert.equal(await b.cursor('todos'), 3);
assert.equal(await b.cursor(''), 10);

// Outbox: ids increase, ack removes up to an id.
const pending = JSON.parse(await b.pending(100));
assert.ok(pending.length >= 5 && pending.every((m, i) => i === 0 || m.id > pending[i - 1].id));
await b.ack(pending[2].id);
assert.equal(JSON.parse(await b.pending(100)).length, pending.length - 3);

console.log('bridge: all merge/purge/revert/outbox checks passed');
