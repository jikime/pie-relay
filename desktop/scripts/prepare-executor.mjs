#!/usr/bin/env node
// Stage the Node executor (../client/node-executor) as a Tauri resource under
// src-tauri/resources/node-executor so it ships inside the app bundle. Host mode's
// daemon runs `node <executor.mjs>`, and executor.mjs drives
// @anthropic-ai/claude-agent-sdk — which finds its native `claude` CLI through the
// platform optional dependency (@anthropic-ai/claude-agent-sdk-darwin-arm64/claude).
// That native binary MUST be present or every chat fails, so we reinstall
// node_modules reproducibly from the lockfile and then assert the binary exists.
//
// Cross-platform (Node) port of prepare-executor.sh so it also runs on Windows,
// which has no bash.
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const DESKTOP_DIR = path.resolve(__dirname, '..');
const SRC_DIR = path.resolve(DESKTOP_DIR, '..', 'client', 'node-executor');
const STAGE_DIR = path.join(DESKTOP_DIR, 'src-tauri', 'resources', 'node-executor');

function fail(msg) {
  console.error(`error: ${msg}`);
  process.exit(1);
}

function checkCommandAvailable(cmd, versionArgs, message) {
  const res = spawnSync(cmd, versionArgs, { stdio: 'ignore', shell: process.platform === 'win32' });
  if (res.error || res.status !== 0) {
    fail(message);
  }
}

checkCommandAvailable('node', ['--version'], 'node is required to stage the executor (need Node >= 20)');
checkCommandAvailable('npm', ['--version'], 'npm is required to stage the executor');

console.log(`Staging executor: ${SRC_DIR} -> ${STAGE_DIR}`);
fs.rmSync(STAGE_DIR, { recursive: true, force: true });
fs.mkdirSync(STAGE_DIR, { recursive: true });

// Only the files the daemon actually needs at runtime. Tests/smoke stay out of
// the bundle. node_modules is reinstalled fresh below rather than copied, so a
// stale/dirty dev tree never leaks into the release.
fs.copyFileSync(path.join(SRC_DIR, 'executor.mjs'), path.join(STAGE_DIR, 'executor.mjs'));
// pty-host.mjs is the terminal-room host (node-pty shell). Terminal rooms are
// dead in a packaged app without it — the daemon (CLI_RELAY_ROOM_MODE=terminal)
// supervises this instead of executor.mjs.
fs.copyFileSync(path.join(SRC_DIR, 'pty-host.mjs'), path.join(STAGE_DIR, 'pty-host.mjs'));
fs.copyFileSync(path.join(SRC_DIR, 'package.json'), path.join(STAGE_DIR, 'package.json'));
fs.copyFileSync(path.join(SRC_DIR, 'package-lock.json'), path.join(STAGE_DIR, 'package-lock.json'));

console.log('Installing production deps (npm install --omit=dev) in stage...');
// --omit=dev drops devDependencies but keeps optionalDependencies, which is how
// the platform-native claude binary is delivered. We use `npm install` (not
// `npm ci`) on purpose: the SDK dep is a floating range (^0.3.x) that publishes
// often, so a committed lockfile goes stale and `npm ci`'s strict lock==package
// check then fails on fresh machines/CI. `npm install` reconciles the lock to
// package.json instead of failing. shell:true on win32 for the npm.cmd shim.
const install = spawnSync('npm', ['install', '--omit=dev', '--no-audit', '--no-fund'], {
  cwd: STAGE_DIR,
  stdio: 'inherit',
  shell: process.platform === 'win32',
});
if (install.error || install.status !== 0) {
  fail(`npm install --omit=dev failed${install.error ? `: ${install.error.message}` : ''}`);
}

// Assert the native claude CLI landed for at least one platform. We check the
// host's platform package by name; the daemon's SDK resolves the matching one.
function findNativeClaudeBin() {
  const scopeDir = path.join(STAGE_DIR, 'node_modules', '@anthropic-ai');
  let entries;
  try {
    entries = fs.readdirSync(scopeDir, { withFileTypes: true });
  } catch {
    return null;
  }
  for (const entry of entries) {
    if (!entry.isDirectory() || !entry.name.startsWith('claude-agent-sdk-')) continue;
    // The native binary is `claude` on unix packages and `claude.exe` on the
    // win32-* packages, so check both names.
    for (const bin of ['claude', 'claude.exe']) {
      const candidate = path.join(scopeDir, entry.name, bin);
      if (fs.existsSync(candidate)) return candidate;
    }
  }
  return null;
}

const nativeBin = findNativeClaudeBin();
if (!nativeBin) {
  console.error(
    'error: native claude binary missing under node_modules/@anthropic-ai/claude-agent-sdk-*/',
  );
  console.error('       chat would fail at runtime; aborting the staging.');
  process.exit(1);
}

try {
  fs.chmodSync(nativeBin, 0o755);
} catch {
  // best-effort, mirrors `chmod +x ... || true`
}
console.log(`OK: native claude binary present -> ${nativeBin}`);
console.log(`OK: executor staged at ${STAGE_DIR}`);

// Best-effort size report, mirrors `du -sh "$STAGE_DIR" 2>/dev/null || true`.
function dirSizeBytes(dir) {
  let total = 0;
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return 0;
  }
  for (const entry of entries) {
    const p = path.join(dir, entry.name);
    if (entry.isSymbolicLink()) continue;
    if (entry.isDirectory()) {
      total += dirSizeBytes(p);
    } else {
      try {
        total += fs.statSync(p).size;
      } catch {
        // ignore
      }
    }
  }
  return total;
}

try {
  const bytes = dirSizeBytes(STAGE_DIR);
  const mb = (bytes / (1024 * 1024)).toFixed(1);
  console.log(`${mb}M\t${STAGE_DIR}`);
} catch {
  // best-effort, mirrors `|| true`
}
