// Builds gosync.wasm and copies the matching wasm_exec.js from the Go
// toolchain. The two must come from the same Go version.
import { execFileSync } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..', '..');

execFileSync('go', ['build', '-trimpath', '-ldflags=-s -w', '-o', join(here, 'gosync.wasm'), './cmd/gosync-wasm'], {
  cwd: root,
  stdio: 'inherit',
  env: { ...process.env, GOOS: 'js', GOARCH: 'wasm' },
});

const goroot = execFileSync('go', ['env', 'GOROOT'], { encoding: 'utf8' }).trim();
const candidates = [join(goroot, 'lib', 'wasm', 'wasm_exec.js'), join(goroot, 'misc', 'wasm', 'wasm_exec.js')];
const wasmExec = candidates.find(existsSync);
if (!wasmExec) throw new Error('wasm_exec.js not found in GOROOT');
copyFileSync(wasmExec, join(here, 'wasm_exec.js'));

// `node build.mjs --out <dir>` also copies the SDK into an app directory.
const outIdx = process.argv.indexOf('--out');
if (outIdx !== -1) {
  const out = process.argv[outIdx + 1];
  mkdirSync(out, { recursive: true });
  for (const f of ['gosync.js', 'gosync.wasm', 'wasm_exec.js']) copyFileSync(join(here, f), join(out, f));
  console.log(`copied SDK to ${out}`);
}

const mb = (statSync(join(here, 'gosync.wasm')).size / 1e6).toFixed(1);
console.log(`built gosync.wasm (${mb} MB, serve it with brotli/gzip) and wasm_exec.js`);
