#!/usr/bin/env node
// Build the Go client (../client) as a Tauri sidecar binary named
// clientd-<target-triple>[.exe] under src-tauri/binaries/. Tauri resolves the
// sidecar declared as "binaries/clientd" in tauri.conf.json by appending the
// host target triple (and .exe on Windows), so the file name must include it
// (e.g. clientd-aarch64-apple-darwin, clientd-x86_64-pc-windows-msvc.exe).
//
// Cross-platform (Node) port of build-sidecar.sh so it also runs on Windows,
// which has no bash.
import { spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const DESKTOP_DIR = path.resolve(__dirname, '..');
const CLIENT_DIR = path.resolve(DESKTOP_DIR, '..', 'client');
const OUT_DIR = path.join(DESKTOP_DIR, 'src-tauri', 'binaries');

function fail(msg) {
  console.error(`error: ${msg}`);
  process.exit(1);
}

// Preflight: this replaces the bash "presidecar" npm script check.
function checkGoAvailable() {
  const res = spawnSync('go', ['version'], { stdio: 'ignore' });
  if (res.error || res.status !== 0) {
    fail('Go toolchain (go) is required to build the sidecar');
  }
}

// Same triple Tauri appends. rustc reports it as e.g. "host: aarch64-apple-darwin".
function getRustHostTriple() {
  const res = spawnSync('rustc', ['-Vv'], { encoding: 'utf8' });
  if (res.error || res.status !== 0 || !res.stdout) {
    fail('could not determine rust host target triple (is rustc installed?)');
  }
  const line = res.stdout.split(/\r?\n/).find((l) => l.startsWith('host:'));
  const triple = line ? line.slice('host:'.length).trim() : '';
  if (!triple) {
    fail('could not determine rust host target triple (is rustc installed?)');
  }
  return triple;
}

checkGoAvailable();

const triple = getRustHostTriple();
const isWindows = process.platform === 'win32' || triple.includes('windows');
const outName = `clientd-${triple}${isWindows ? '.exe' : ''}`;
const out = path.join(OUT_DIR, outName);

mkdirSync(OUT_DIR, { recursive: true });

console.log(`Building Go client: ${path.join(CLIENT_DIR, 'cmd', 'client')} -> ${out}`);
// Native build for the host machine — do NOT set GOOS/GOARCH, the sidecar is
// built for the machine doing the build.
const build = spawnSync('go', ['build', '-o', out, './cmd/client'], {
  cwd: CLIENT_DIR,
  stdio: 'inherit',
});
if (build.error || build.status !== 0) {
  fail(`go build failed${build.error ? `: ${build.error.message}` : ''}`);
}

console.log(`Done: ${out}`);
