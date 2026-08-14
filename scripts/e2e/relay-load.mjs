#!/usr/bin/env node

import { pathToFileURL } from 'node:url';

export async function runRelayLoad(options = {}) {
  const control = requiredValue(options.controlURL ?? process.env.PIE_E2E_CONTROL_URL, 'controlURL').replace(/\/$/, '');
  const relay = requiredValue(options.relayURL ?? process.env.PIE_E2E_RELAY_URL, 'relayURL').replace(/\/$/, '');
  const adminToken = requiredValue(options.controlToken ?? process.env.PIE_E2E_CONTROL_TOKEN, 'controlToken');
  const sessionId = requiredValue(options.sessionId ?? process.env.PIE_E2E_SESSION_ID, 'sessionId');
  const ownerUserId = requiredValue(options.ownerUserId ?? process.env.PIE_E2E_OWNER_USER_ID, 'ownerUserId');
  const participants = positiveInteger(options.participants ?? process.env.PIE_E2E_LOAD_PARTICIPANTS ?? 20, 'participants');
  const maxP95Ms = positiveInteger(options.maxP95Ms ?? process.env.PIE_E2E_LOAD_MAX_P95_MS ?? 5000, 'maxP95Ms');

  const credential = await requestJSON(`${control}/v1/control/sessions/${encodeURIComponent(sessionId)}/credential`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${adminToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ subjectUserId: ownerUserId, role: 'host', access: 'control', ttlSeconds: 600 }),
  });

  const sockets = [];
  try {
    const starts = [];
    for (let index = 0; index < participants; index += 1) {
      const invite = await requestJSON(`${relay}/rooms/invites`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${credential.token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ access: 'view' }),
      });
      const joined = await requestJSON(`${relay}/rooms/join`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: invite.code, name: `load-viewer-${index}` }),
      });
      starts.push(connectParticipant(relay, joined.token, `load-viewer-${index}`).then((result) => {
        sockets.push(result.socket);
        return result.elapsedMs;
      }));
    }
    const latencies = (await Promise.all(starts)).sort((a, b) => a - b);
    const result = {
      ok: true,
      sessionId,
      participants,
      handshakeMs: {
        min: round(latencies[0]),
        p50: round(percentile(latencies, 0.50)),
        p95: round(percentile(latencies, 0.95)),
        p99: round(percentile(latencies, 0.99)),
        max: round(latencies.at(-1)),
      },
    };
    if (result.handshakeMs.p95 > maxP95Ms) {
      throw new Error(`relay participant handshake p95 ${result.handshakeMs.p95}ms exceeded ${maxP95Ms}ms`);
    }
    return result;
  } finally {
    for (const socket of sockets) {
      if (socket.readyState < WebSocket.CLOSING) socket.close(1000, 'load test complete');
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

function connectParticipant(relay, token, clientId) {
  const wsOrigin = relay.replace(/^http:/, 'ws:').replace(/^https:/, 'wss:');
  const startedAt = performance.now();
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(`${wsOrigin}/ws/participant`, [`pie-relay.ticket.${token}`]);
    let joined = false;
    let snapshot = false;
    const timeout = setTimeout(() => {
      socket.close();
      reject(new Error(`${clientId} relay handshake timed out`));
    }, 10_000);
    socket.addEventListener('error', () => {
      clearTimeout(timeout);
      reject(new Error(`${clientId} WebSocket failed`));
    }, { once: true });
    socket.addEventListener('open', () => {
      socket.send(JSON.stringify({ type: 'relay_join', protocolVersion: '2.0', streamId: 'terminal', clientId }));
    }, { once: true });
    socket.addEventListener('message', (event) => {
      let frame;
      try {
        frame = JSON.parse(String(event.data));
      } catch {
        return;
      }
      if (frame.type === 'relay_join_ack') {
        joined = true;
        socket.send(JSON.stringify({ type: 'request_screen' }));
      }
      if (frame.type === 'pty_snapshot') snapshot = true;
      if (joined && snapshot) {
        clearTimeout(timeout);
        resolve({ socket, elapsedMs: performance.now() - startedAt });
      }
    });
  });
}

async function requestJSON(url, options = {}) {
  const response = await fetch(url, options);
  const body = await response.text();
  if (!response.ok) throw new Error(`${options.method || 'GET'} ${url}: HTTP ${response.status} ${body}`);
  return body ? JSON.parse(body) : null;
}

function requiredValue(value, name) {
  const normalized = String(value ?? '').trim();
  if (!normalized) throw new Error(`${name} is required`);
  return normalized;
}

function positiveInteger(value, name) {
  const parsed = Number.parseInt(String(value), 10);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) throw new Error(`${name} must be a positive integer`);
  return parsed;
}

function percentile(values, fraction) {
  return values[Math.min(values.length - 1, Math.max(0, Math.ceil(values.length * fraction) - 1))];
}

function round(value) {
  return Math.round(value * 10) / 10;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  runRelayLoad().then((result) => console.log(JSON.stringify(result))).catch((error) => {
    console.error(error.stack || error.message);
    process.exitCode = 1;
  });
}
