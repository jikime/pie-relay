#!/usr/bin/env node

// Live data-plane smoke test. It expects Manager, Relay and a ready host session
// to be running. No credential is printed; all bearer values stay in memory.
const control = required('PIE_E2E_CONTROL_URL').replace(/\/$/, '');
const relay = required('PIE_E2E_RELAY_URL').replace(/\/$/, '');
const adminToken = required('PIE_E2E_CONTROL_TOKEN');
const sessionId = required('PIE_E2E_SESSION_ID');
const ownerUserId = required('PIE_E2E_OWNER_USER_ID');
const exerciseControl = process.env.PIE_E2E_EXERCISE_CONTROL === '1';

function required(name) {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

async function json(url, options = {}) {
  const response = await fetch(url, options);
  const body = await response.text();
  if (!response.ok) throw new Error(`${options.method || 'GET'} ${url}: HTTP ${response.status} ${body}`);
  return body ? JSON.parse(body) : null;
}

const credential = await json(`${control}/v1/control/sessions/${encodeURIComponent(sessionId)}/credential`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${adminToken}`, 'Content-Type': 'application/json' },
  body: JSON.stringify({ subjectUserId: ownerUserId, role: 'host', access: 'control', ttlSeconds: 600 }),
});
const invite = await json(`${relay}/rooms/invites`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${credential.token}`, 'Content-Type': 'application/json' },
  body: JSON.stringify({ access: 'view' }),
});
const joined = await json(`${relay}/rooms/join`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ code: invite.code, name: 'e2e-viewer' }),
});

const wsOrigin = relay.replace(/^http:/, 'ws:').replace(/^https:/, 'wss:');
const socket = new WebSocket(`${wsOrigin}/ws/participant`, [`pie-relay.ticket.${joined.token}`]);
const seen = new Set();
const deadline = setTimeout(() => {
  socket.close();
  throw new Error(`participant data-plane timeout; seen=${[...seen].join(',')}`);
}, 10_000);

await new Promise((resolve, reject) => {
  socket.addEventListener('error', () => reject(new Error('participant WebSocket failed')));
  socket.addEventListener('open', () => {
    socket.send(JSON.stringify({ type: 'relay_join', protocolVersion: '2.0', streamId: 'terminal', clientId: 'e2e-viewer' }));
    socket.send(JSON.stringify({ type: 'request_screen' }));
  });
  socket.addEventListener('message', (event) => {
    try {
      const frame = JSON.parse(String(event.data));
      if (frame.type) seen.add(frame.type);
      if (seen.has('relay_join_ack') && seen.has('host:status') && seen.has('pty_snapshot')) resolve();
    } catch (error) {
      reject(error);
    }
  });
});

let participant;
for (let attempt = 0; attempt < 25 && !participant; attempt += 1) {
  const snapshot = await json(`${control}/v1/admin/snapshot`, {
    headers: { Authorization: `Bearer ${adminToken}` },
  });
  participant = snapshot.participants?.find((value) => value.sessionId === sessionId && value.transport === 'relay');
  if (!participant) await new Promise((resolve) => setTimeout(resolve, 100));
}
if (!participant) throw new Error('Relay participant was not projected into the Control Plane');
if (participant.access !== 'view') throw new Error(`unexpected participant access: ${participant.access}`);

async function snapshot() {
  return json(`${control}/v1/admin/snapshot`, { headers: { Authorization: `Bearer ${adminToken}` } });
}

async function queueOperation(type, targetKind, targetId, request = {}) {
  return json(`${control}/v1/admin/operations`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${adminToken}`, 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
    body: JSON.stringify({ type, targetKind, targetId, request }),
  });
}

async function waitForOperation(id) {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    const state = await snapshot();
    const operation = state.operations?.find((value) => value.id === id);
    if (operation?.status === 'succeeded') return operation;
    if (operation?.status === 'failed') throw new Error(`${operation.type} failed: ${operation.error}`);
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`operation ${id} timed out`);
}

let controlParticipant;
if (exerciseControl) {
  const controlInvite = await json(`${relay}/rooms/invites`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${credential.token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ access: 'control' }),
  });
  const controlJoin = await json(`${relay}/rooms/join`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code: controlInvite.code, name: 'e2e-controller' }),
  });
  const controlSocket = new WebSocket(`${wsOrigin}/ws/participant`, [`pie-relay.ticket.${controlJoin.token}`]);
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('control participant handshake timed out')), 10_000);
    controlSocket.addEventListener('error', () => reject(new Error('control participant WebSocket failed')));
    controlSocket.addEventListener('open', () => {
      controlSocket.send(JSON.stringify({ type: 'relay_join', protocolVersion: '2.0', streamId: 'terminal', clientId: 'e2e-controller' }));
    });
    controlSocket.addEventListener('message', (event) => {
      const frame = JSON.parse(String(event.data));
      if (frame.type === 'relay_join_ack') {
        clearTimeout(timeout);
        resolve();
      }
    });
  });
  for (let attempt = 0; attempt < 50 && !controlParticipant; attempt += 1) {
    const state = await snapshot();
    controlParticipant = state.participants?.find((value) => value.sessionId === sessionId && value.access === 'control' && value.userId !== ownerUserId);
    if (!controlParticipant) await new Promise((resolve) => setTimeout(resolve, 100));
  }
  if (!controlParticipant) throw new Error('control participant was not projected into the Control Plane');

  const driverOperation = await queueOperation('session.driver.set', 'sessions', sessionId, { userId: controlParticipant.userId });
  await waitForOperation(driverOperation.id);
  const driverState = await snapshot();
  const session = driverState.sessions?.find((value) => value.id === sessionId);
  if (session?.driverUserId !== controlParticipant.userId) throw new Error(`driver handoff was not persisted: ${session?.driverUserId}`);

  // Prove that the selected controller can traverse the complete data path,
  // not merely open a WebSocket: participant -> Relay -> clientd PTY -> Relay
  // -> participant. PTY output is base64-encoded by the host agent.
  const terminalMarker = `PIE_RELAY_E2E_${crypto.randomUUID().replaceAll('-', '')}`;
  let terminalOutput = '';
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('terminal input/output round trip timed out')), 10_000);
    const onMessage = (event) => {
      try {
        const frame = JSON.parse(String(event.data));
        if ((frame.type === 'pty_output' || frame.type === 'pty_snapshot') && typeof frame.data === 'string') {
          terminalOutput += Buffer.from(frame.data, 'base64').toString('utf8');
          if (terminalOutput.includes(terminalMarker)) {
            clearTimeout(timeout);
            controlSocket.removeEventListener('message', onMessage);
            resolve();
          }
        }
      } catch (error) {
        clearTimeout(timeout);
        controlSocket.removeEventListener('message', onMessage);
        reject(error);
      }
    };
    controlSocket.addEventListener('message', onMessage);
    controlSocket.send(JSON.stringify({ type: 'pty_input', data: `printf '${terminalMarker}\\n'\n` }));
  });

  const controlClosed = new Promise((resolve) => controlSocket.addEventListener('close', resolve, { once: true }));
  const disconnectController = await queueOperation('participant.disconnect', 'participants', controlParticipant.id);
  await waitForOperation(disconnectController.id);
  await Promise.race([controlClosed, new Promise((_, reject) => setTimeout(() => reject(new Error('control participant was not disconnected')), 5_000))]);

  const viewClosed = new Promise((resolve) => socket.addEventListener('close', resolve, { once: true }));
  const disconnectViewer = await queueOperation('participant.disconnect', 'participants', participant.id);
  await waitForOperation(disconnectViewer.id);
  await Promise.race([viewClosed, new Promise((_, reject) => setTimeout(() => reject(new Error('view participant was not disconnected')), 5_000))]);
}

clearTimeout(deadline);
if (socket.readyState < WebSocket.CLOSING) socket.close();
console.log(JSON.stringify({ ok: true, sessionId, frames: [...seen].sort(), participant: participant.userId, operations: exerciseControl ? ['driver-handoff', 'terminal-input-output', 'participant-disconnect'] : [] }));
