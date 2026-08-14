#!/usr/bin/env node

// Live standalone-host regression: two native clientd processes expose two
// independent PTYs through Relay. The test proves view/control enforcement,
// session isolation and reconnect with the same host credential. Tokens never
// leave this process or appear in output.

import { randomBytes } from 'node:crypto';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { execFileSync, spawn } from 'node:child_process';

const relayHTTP = required('PIE_E2E_RELAY_URL').replace(/\/$/, '');
const enrollSecret = required('PIE_E2E_ENROLL_SECRET');
const relayWS = `${relayHTTP.replace(/^http:/, 'ws:').replace(/^https:/, 'wss:')}/ws/agent`;
const participantWS = `${relayHTTP.replace(/^http:/, 'ws:').replace(/^https:/, 'wss:')}/ws/participant`;
const repoRoot = resolve(import.meta.dirname, '../..');
const configuredClientdPath = process.env.PIE_E2E_CLIENTD_PATH?.trim();
const ptyHostPath = resolve(optional(
  'PIE_E2E_PTY_HOST_PATH',
  join(repoRoot, 'client/node-executor/pty-host.mjs'),
));

const workRoot = mkdtempSync(join(tmpdir(), 'pie-clientd-e2e-'));
const clientdPath = configuredClientdPath ? resolve(configuredClientdPath) : join(workRoot, 'clientd');
const hosts = [];
const peers = [];

try {
  if (!configuredClientdPath) {
    execFileSync('go', ['build', '-trimpath', '-o', clientdPath, './cmd/client'], {
      cwd: join(repoRoot, 'client'),
      stdio: 'inherit',
    });
  }
  if (!existsSync(clientdPath)) throw new Error(`clientd binary not found: ${clientdPath}`);
  if (!existsSync(ptyHostPath)) throw new Error(`PTY host not found: ${ptyHostPath}`);

  const hostA = await enrollAndStart('a');
  const hostB = await enrollAndStart('b');
  hosts.push(hostA, hostB);

  const viewerA = await connectParticipant(hostA, 'view', 'native-viewer-a');
  const controllerA = await connectParticipant(hostA, 'control', 'native-controller-a');
  const controllerB = await connectParticipant(hostB, 'control', 'native-controller-b');
  peers.push(viewerA, controllerA, controllerB);

  await Promise.all([
    viewerA.waitReady(),
    controllerA.waitReady(true),
    controllerB.waitReady(true),
  ]);

  const blockedMarker = marker('VIEW_BLOCKED');
  viewerA.sendInput(`printf '${blockedMarker}\\n'\r`);
  await sleep(750);
  if (viewerA.output.includes(blockedMarker) || controllerA.output.includes(blockedMarker)) {
    throw new Error('view participant input reached the PTY');
  }

  const markerA = marker('CONTROL_A');
  const markerB = marker('CONTROL_B');
  controllerA.sendInput(`printf '${markerA}\\n'\r`);
  controllerB.sendInput(`printf '${markerB}\\n'\r`);
  await Promise.all([controllerA.waitForOutput(markerA), controllerB.waitForOutput(markerB)]);
  if (controllerA.output.includes(markerB) || controllerB.output.includes(markerA)) {
    throw new Error('PTY output crossed session boundaries');
  }

  await hostA.stop();
  await controllerA.waitForHost(false);
  await hostA.start();
  await controllerA.waitForHost(true);
  controllerA.requestScreen();
  const reconnectMarker = marker('RECONNECTED');
  controllerA.sendInput(`printf '${reconnectMarker}\\n'\r`);
  await controllerA.waitForOutput(reconnectMarker);

  console.log(JSON.stringify({
    ok: true,
    rooms: [hostA.room, hostB.room],
    verified: [
      'native-clientd',
      'two-independent-pty-sessions',
      'view-input-blocked',
      'control-input-output',
      'session-isolation',
      'clientd-reconnect',
      'pty-snapshot',
    ],
  }));
} finally {
  for (const peer of peers) peer.close();
  await Promise.all(hosts.map((host) => host.stop()));
  rmSync(workRoot, { recursive: true, force: true });
}

async function enrollAndStart(label) {
  const room = `native-${label}-${randomBytes(4).toString('hex')}`;
  const credential = await requestJSON(`${relayHTTP}/host/enroll`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ secret: enrollSecret, room, name: `native-host-${label}` }),
  });
  const host = {
    room,
    token: credential.token,
    child: null,
    logs: '',
    async start() {
      if (this.child) throw new Error(`${room} clientd is already running`);
      const child = spawn(clientdPath, [], {
        cwd: join(workRoot, label),
        env: {
          ...process.env,
          PIE_RELAY_URL: relayWS,
          RELAY_TICKET: this.token,
          CLI_RELAY_ROOM_MODE: 'terminal',
          PTY_HOST_PATH: ptyHostPath,
          CLI_RELAY_DEFAULT_CWD: join(workRoot, label),
          SHELL: '/bin/bash',
        },
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      this.child = child;
      for (const stream of [child.stdout, child.stderr]) {
        stream.on('data', (chunk) => {
          this.logs = (this.logs + String(chunk)).slice(-16_384);
        });
      }
      child.once('exit', (code, signal) => {
        if (this.child === child) this.child = null;
        if (code && code !== 0) this.logs += `\nclientd exit=${code ?? signal}`;
      });
      await sleep(100);
      if (!this.child) throw new Error(`${room} clientd exited during startup: ${redact(this.logs)}`);
    },
    async stop() {
      const child = this.child;
      if (!child) return;
      const exited = new Promise((resolvePromise) => child.once('exit', resolvePromise));
      child.kill('SIGTERM');
      await Promise.race([exited, sleep(5000)]);
      if (this.child === child) {
        child.kill('SIGKILL');
        await exited;
      }
    },
  };
  await import('node:fs/promises').then(({ mkdir }) => mkdir(join(workRoot, label), { recursive: true }));
  await host.start();
  return host;
}

async function connectParticipant(host, access, name) {
  const invite = await requestJSON(`${relayHTTP}/rooms/invites`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${host.token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ access }),
  });
  const joined = await requestJSON(`${relayHTTP}/rooms/join`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code: invite.code, name }),
  });
  const userID = jwtSubject(joined.token);
  const socket = new WebSocket(participantWS, [`pie-relay.ticket.${joined.token}`]);
  const state = {
    socket,
    userID,
    output: '',
    seen: new Set(),
    hostConnected: null,
    driver: '',
    sendInput(data) { socket.send(JSON.stringify({ type: 'pty_input', data })); },
    requestScreen() { socket.send(JSON.stringify({ type: 'request_screen' })); },
    close() { if (socket.readyState < WebSocket.CLOSING) socket.close(); },
    waitForOutput(text) { return poll(() => this.output.includes(text), 10_000, `PTY output ${text}`); },
    waitForHost(connected) { return poll(() => this.hostConnected === connected, 15_000, `host connected=${connected}`); },
    waitReady(expectDriver = false) {
      return poll(() => this.seen.has('relay_join_ack') && this.seen.has('pty_snapshot') && this.hostConnected === true && (!expectDriver || this.driver === this.userID), 15_000, `${name} ready`);
    },
  };
  socket.addEventListener('open', () => {
    socket.send(JSON.stringify({ type: 'relay_join', protocolVersion: '2.0', streamId: 'terminal', clientId: name }));
    socket.send(JSON.stringify({ type: 'request_screen' }));
  });
  socket.addEventListener('message', (event) => {
    const frame = JSON.parse(String(event.data));
    if (frame.type) state.seen.add(frame.type);
    if (frame.type === 'host:status') {
      state.hostConnected = frame.connected === true;
      // A participant can win the race against clientd startup. The first
      // request_screen is then correctly dropped while no host exists, so ask
      // again on the authoritative host-online transition just like the UI.
      if (state.hostConnected && socket.readyState === WebSocket.OPEN) state.requestScreen();
    }
    if (frame.type === 'driver_state' || frame.type === 'participant_roster') state.driver = frame.driver || '';
    if ((frame.type === 'pty_output' || frame.type === 'pty_snapshot') && typeof frame.data === 'string') {
      state.output = (state.output + Buffer.from(frame.data, 'base64').toString('utf8')).slice(-1_048_576);
    }
  });
  socket.addEventListener('error', () => { state.socketError = true; });
  return state;
}

async function requestJSON(url, options) {
  const response = await fetch(url, { ...options, signal: AbortSignal.timeout(10_000) });
  const body = await response.text();
  if (!response.ok) throw new Error(`${options.method || 'GET'} ${url}: HTTP ${response.status} ${body}`);
  return body ? JSON.parse(body) : null;
}

async function poll(check, timeoutMs, label) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (check()) return;
    await sleep(50);
  }
  throw new Error(`timed out waiting for ${label}`);
}

function jwtSubject(token) {
  const payload = token.split('.')[1];
  if (!payload) throw new Error('participant token is malformed');
  return JSON.parse(Buffer.from(payload, 'base64url').toString('utf8')).sub;
}

function marker(prefix) {
  return `__PIE_${prefix}_${randomBytes(6).toString('hex')}__`;
}

function redact(value) {
  return String(value).replace(/eyJ[A-Za-z0-9._-]+/g, '<redacted-token>');
}

function required(name) {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function optional(name, fallback) {
  return process.env[name]?.trim() || fallback;
}

function sleep(milliseconds) {
  return new Promise((resolvePromise) => setTimeout(resolvePromise, milliseconds));
}
