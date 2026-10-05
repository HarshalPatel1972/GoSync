// Real-browser test (Chromium, Firefox, WebKit via Playwright) of the parts
// that only exist in browsers: Web Locks leader election, BroadcastChannel
// fan-out between tabs, IndexedDB shared by tabs, and failover when the
// leader tab closes.
//
// Expects a server started with GOSYNC_INSECURE_DEV_AUTH=true serving the
// built SDK at /gosync/ (see CI). Usage: node test/browser.mjs [chromium firefox webkit]
import { chromium, firefox, webkit } from 'playwright';

const BASE = process.env.GOSYNC_TEST_URL ?? 'http://127.0.0.1:8090';
const engines = { chromium, firefox, webkit };
const wanted = process.argv.slice(2).length ? process.argv.slice(2) : Object.keys(engines);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(what, fn, ms = 15000) {
  const end = Date.now() + ms;
  let last;
  while (Date.now() < end) {
    try { if (await fn()) return; } catch (e) { last = e; }
    await sleep(100);
  }
  throw new Error('timed out: ' + what + (last ? ` (${last.message})` : ''));
}

// Opens a tab with a GoSync client exposed as window.db.
async function openTab(context, user, dbName) {
  const page = await context.newPage();
  page.on('pageerror', (e) => console.error(`    [page error] ${e.message}`));
  await page.goto(BASE + '/gosync/'); // any same-origin page will do
  await page.evaluate(async ({ user, dbName }) => {
    const { createClient } = await import('/gosync/gosync.js');
    const url = new URL('/sync', location.href);
    url.protocol = url.protocol.replace('http', 'ws');
    window.db = await createClient({ url, token: user, dbName, wasmUrl: '/gosync/gosync.wasm' });
  }, { user, dbName });
  return page;
}

const state = (page) => page.evaluate(() => ({ leader: window.db.isLeader, status: window.db.status }));
const get = (page, id) => page.evaluate((id) => window.db.get('todos', id), id);
const set = (page, id, fields) => page.evaluate(({ id, fields }) => window.db.set('todos', id, fields), { id, fields });

let failures = 0;
for (const name of wanted) {
  console.log(`\n${name}`);
  const browser = await engines[name].launch();
  try {
    const run = name + '-' + Date.now().toString(36);
    const user = 'mt-' + run;
    const device1 = await browser.newContext();
    const device2 = await browser.newContext();
    const tab1 = await openTab(device1, user, 'db-' + run);
    const tab2 = await openTab(device1, user, 'db-' + run);
    const other = await openTab(device2, user, 'db-' + run);
    const ok = (msg) => console.log('  ✓', msg);

    await until('one leader per device, all online', async () => {
      const [a, b] = [await state(tab1), await state(tab2)];
      return a.leader !== b.leader && a.status === 'online' && b.status === 'online';
    });
    let [leader, follower] = (await state(tab1)).leader ? [tab1, tab2] : [tab2, tab1];
    ok('exactly one tab holds the connection; the follower mirrors its online status');

    await set(follower, 'f1', { title: 'from follower' });
    await until('leader tab sees follower write', async () => (await get(leader, 'f1'))?.title === 'from follower');
    ok('write in follower tab appears in leader tab (BroadcastChannel)');
    await until('other device receives follower write', async () => (await get(other, 'f1'))?.title === 'from follower');
    ok('leader pushes the follower\'s outbox to the server');

    await set(other, 'd2', { title: 'from device 2' });
    await until('follower sees remote write', async () => (await get(follower, 'd2'))?.title === 'from device 2');
    ok('remote change pulled by leader reaches the follower tab');

    await leader.close();
    await until('follower takes over', async () => {
      const s = await state(follower);
      return s.leader && s.status === 'online';
    });
    ok('closing the leader tab hands the connection to the other tab');
    await set(other, 'd3', { title: 'after failover' });
    await until('new leader syncs', async () => (await get(follower, 'd3'))?.title === 'after failover');
    await set(follower, 'f2', { title: 'new leader write' });
    await until('new leader pushes', async () => (await get(other, 'f2'))?.title === 'new leader write');
    ok('new leader syncs in both directions');

    const reopened = await openTab(device1, user, 'db-' + run);
    await follower.close();
    const persisted = await get(reopened, 'f1');
    if (persisted?.title !== 'from follower') throw new Error('IndexedDB data lost after closing all tabs');
    await until('reopened tab online', async () => (await state(reopened)).status === 'online');
    ok('data persists in IndexedDB after all tabs close; reopened tab syncs');
  } catch (e) {
    failures++;
    console.error(`  ✗ ${e.message}`);
  } finally {
    await browser.close();
  }
}
console.log(failures ? `\n${failures} browser(s) failed` : '\nall browsers passed');
process.exit(failures ? 1 : 0);
