// Bundler test: does the published package work when an app bundles it?
//
// Packs sdk/js exactly as npm would, builds a minimal app with Vite and with
// Next.js (Turbopack and webpack), serves each build, and checks in Chromium
// that the app saves locally and goes online against a real GoSync server.
//
//   GOSYNC_SERVER_BIN=../../gosync-server node test/bundlers.mjs
//
// Needs the SDK built first (node build.mjs) and network access for npm.
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { chromium } from 'playwright';

const sdkDir = resolve(import.meta.dirname, '..');
const serverBin = process.env.GOSYNC_SERVER_BIN;
if (!serverBin) throw new Error('set GOSYNC_SERVER_BIN to a built gosync-server');

const work = mkdtempSync(join(tmpdir(), 'gosync-bundlers-'));
const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';
const run = (cmd, args, cwd) => execFileSync(cmd, args, { cwd, stdio: 'pipe', shell: process.platform === 'win32' });

execFileSync(npm, ['pack', '--pack-destination', work], { cwd: sdkDir, stdio: 'pipe', shell: process.platform === 'win32' });
const tarball = join(work, readdirSync(work).find((f) => f.endsWith('.tgz')));

const SERVER_PORT = 8199;
const server = spawn(resolve(serverBin), [], {
  env: {
    ...process.env,
    GOSYNC_ADDR: `127.0.0.1:${SERVER_PORT}`,
    GOSYNC_INSECURE_DEV_AUTH: 'true',
    GOSYNC_ALLOWED_ORIGINS: '*',
    GOSYNC_DATABASE_URL: join(work, 'bundlers.db'),
    GOSYNC_LOG_LEVEL: 'warn',
  },
  stdio: 'ignore',
});

const clientCode = (label) => `
  const out = document.getElementById('out');
  try {
    const db = await createClient({ url: 'ws://127.0.0.1:${SERVER_PORT}/sync', token: 'bundler-${label}', dbName: 'bundler-${label}' });
    await db.set('todos', 't1', { title: '${label}' });
    const t = await db.get('todos', 't1');
    db.onStatus((s) => { if (s === 'online') out.textContent = 'OK ' + t.title; });
  } catch (e) { out.textContent = 'ERROR ' + e.message; }
`;

function viteApp(dir) {
  writeFileSync(join(dir, 'index.html'), '<!doctype html><html><body><pre id="out">loading</pre><script type="module" src="/main.js"></script></body></html>');
  writeFileSync(join(dir, 'main.js'), `import { createClient } from '@harshalpatel2868/gosync-client';\n${clientCode('vite')}`);
  run(npm, ['install', '--silent', 'vite@latest', tarball], dir);
  run(npx, ['vite', 'build'], dir);
  return { cmd: npx, args: ['vite', 'preview', '--port', '4171', '--strictPort', '--host', '127.0.0.1'], url: 'http://127.0.0.1:4171/' };
}

function nextApp(dir, bundler) {
  mkdirSync(join(dir, 'app'));
  writeFileSync(join(dir, 'app', 'layout.js'), 'export default function L({ children }) { return <html lang="en"><body>{children}</body></html>; }');
  writeFileSync(
    join(dir, 'app', 'page.js'),
    `"use client";
import { useEffect } from "react";
import { createClient } from "@harshalpatel2868/gosync-client";
export default function Page() {
  useEffect(() => { (async () => { ${clientCode('next-' + bundler)} })(); }, []);
  return <pre id="out">loading</pre>;
}`,
  );
  run(npm, ['install', '--silent', 'next@latest', 'react', 'react-dom', tarball], dir);
  run(npx, ['next', 'build', ...(bundler === 'webpack' ? ['--webpack'] : [])], dir);
  return { cmd: npx, args: ['next', 'start', '-p', '4172', '-H', '127.0.0.1'], url: 'http://127.0.0.1:4172/' };
}

async function waitFor(url) {
  for (let i = 0; i < 120; i++) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error('server did not start: ' + url);
}

const apps = [
  ['Vite', (d) => viteApp(d)],
  ['Next.js (Turbopack)', (d) => nextApp(d, 'turbopack')],
  ['Next.js (webpack)', (d) => nextApp(d, 'webpack')],
];

const browser = await chromium.launch();
let failures = 0;
try {
  for (const [name, build] of apps) {
    const dir = join(work, name.replace(/\W+/g, '-'));
    mkdirSync(dir);
    writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'app', private: true, type: 'module' }));
    let preview;
    try {
      const app = build(dir);
      preview = spawn(app.cmd, app.args, { cwd: dir, stdio: 'ignore', shell: process.platform === 'win32' });
      await waitFor(app.url);
      const page = await browser.newPage();
      await page.goto(app.url);
      await page.waitForFunction(() => /^(OK|ERROR)/.test(document.getElementById('out')?.textContent || ''), null, { timeout: 30000 });
      const text = await page.textContent('#out');
      if (!text.startsWith('OK')) throw new Error(text);
      console.log(`  ✓ ${name}: bundled, saved locally and synced`);
      await page.close();
    } catch (e) {
      failures++;
      console.error(`  ✗ ${name}: ${e.message.split('\n')[0]}`);
    } finally {
      if (preview) {
        if (process.platform === 'win32') spawn('taskkill', ['/F', '/T', '/PID', String(preview.pid)], { stdio: 'ignore' });
        else preview.kill();
      }
    }
  }
} finally {
  await browser.close();
  server.kill();
}
console.log(failures ? `\n${failures} bundler(s) failed` : '\nall bundlers passed');
process.exit(failures ? 1 : 0);
