#!/usr/bin/env node
// Build the Pie Relay adapter around the vendored mobile stack and stage
// the runnable bundle as a Tauri resource. The bundle keeps node-pty external,
// because its native addon must be installed for the target platform.
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const DESKTOP_DIR = path.resolve(__dirname, '..');
const SOURCE_DIR = path.resolve(DESKTOP_DIR, '..', 'pie-mobile', 'adapter', 'host-gateway');
const STACK_DIR = path.resolve(SOURCE_DIR, '..', '..');
const STAGE_DIR = path.join(DESKTOP_DIR, 'src-tauri', 'resources', 'mobile-host');

function fail(message) {
  console.error(`error: ${message}`);
  process.exit(1);
}

function run(command, args, cwd) {
  const result = spawnSync(command, args, {
    cwd,
    stdio: 'inherit',
    shell: process.platform === 'win32',
  });
  if (result.error || result.status !== 0) {
    fail(`${command} ${args.join(' ')} failed${result.error ? `: ${result.error.message}` : ''}`);
  }
}

if (!fs.existsSync(path.join(SOURCE_DIR, 'package.json'))) {
  fail(`Pie Relay mobile host adapter is missing: ${SOURCE_DIR}`);
}

// Imports deliberately resolve into the read-only upstream tree, so install
// both dependency roots before building. Frozen lockfiles keep the copied Orca
// transport and the cli-relay adapter reproducible on a fresh checkout.
run('pnpm', ['install', '--frozen-lockfile'], STACK_DIR);
run('pnpm', ['install', '--frozen-lockfile'], SOURCE_DIR);

console.log(`Building Pie Relay mobile host: ${SOURCE_DIR}`);
run('pnpm', ['build'], SOURCE_DIR);

console.log(`Staging Pie Relay mobile host: ${SOURCE_DIR} -> ${STAGE_DIR}`);
fs.rmSync(STAGE_DIR, { recursive: true, force: true });
fs.mkdirSync(STAGE_DIR, { recursive: true });
fs.copyFileSync(
  path.join(SOURCE_DIR, 'dist', 'host-gateway.mjs'),
  path.join(STAGE_DIR, 'host-gateway.mjs'),
);
const sourceMap = path.join(SOURCE_DIR, 'dist', 'host-gateway.mjs.map');
if (fs.existsSync(sourceMap)) {
  fs.copyFileSync(sourceMap, path.join(STAGE_DIR, 'host-gateway.mjs.map'));
}

// Only node-pty remains external after esbuild. Pin it to the adapter's exact
// runtime version so development, tests, and packaged apps use one ABI.
fs.writeFileSync(
  path.join(STAGE_DIR, 'package.json'),
  `${JSON.stringify(
    {
      name: '@pielab/pie-relay-mobile-host-runtime',
      private: true,
      type: 'module',
      dependencies: { 'node-pty': '1.1.0' },
    },
    null,
    2,
  )}\n`,
);
run('npm', ['install', '--omit=dev', '--no-audit', '--no-fund'], STAGE_DIR);

console.log(`OK: Pie Relay mobile host staged at ${STAGE_DIR}`);
