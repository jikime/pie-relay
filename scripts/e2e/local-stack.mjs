#!/usr/bin/env node

import { createHmac } from 'node:crypto';
import { execFileSync, spawn } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { runRelayLoad } from './relay-load.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, '../..');
const controlURL = optional('PIE_E2E_CONTROL_URL', 'http://127.0.0.1:19090').replace(/\/$/, '');
const relayURL = optional('PIE_E2E_RELAY_URL', 'http://127.0.0.1:13412').replace(/\/$/, '');
const mockURL = optional('PIE_E2E_MOCK_AUTH_URL', 'http://127.0.0.1:18080').replace(/\/$/, '');
const tlsRelayURL = process.env.PIE_E2E_TLS_RELAY_URL?.trim().replace(/\/$/, '');
const localCA = process.env.PIE_E2E_LOCAL_CA?.trim();
const adminToken = required('PIE_E2E_CONTROL_TOKEN');
const webhookSecret = required('PIE_E2E_WEBHOOK_SECRET');
const mockControlToken = required('PIE_E2E_MOCK_AUTH_CONTROL_TOKEN');
const composeProject = optional('PIE_E2E_COMPOSE_PROJECT', 'pie-relay-local');
const managerID = optional('PIE_EXECUTOR_MANAGER_ID', 'local-manager');
const userToken = 'pat-local-user';
const userID = 'local-user';

await waitFor(`${relayURL}/readyz`, 120_000);
await waitFor(`${controlURL}/readyz`, 120_000);
await waitFor(`${mockURL}/healthz`, 30_000);

await expectStatus(`${controlURL}/v1/admin/overview`, {}, 401, 'unauthenticated Manager API');
await expectStatus(`${controlURL}/v1/admin/overview`, auth('pat-local-inactive'), 401, 'inactive PAT');
await expectStatus(`${controlURL}/v1/admin/overview`, auth('pat-local-viewer'), 200, 'admin viewer PAT');
await expectStatus(`${controlURL}/v1/admin/overview`, auth('pat-local-operator'), 200, 'operator PAT');
await expectStatus(`${controlURL}/v1/admin/overview`, auth('pat-local-admin'), 200, 'admin PAT');

const timeoutStarted = performance.now();
await expectStatus(`${controlURL}/v1/control/sessions`, auth('pat-local-slow'), 401, 'introspection timeout');
if (performance.now() - timeoutStarted > 2500) throw new Error('introspection timeout exceeded 2.5 seconds');

await expectStatus(`${controlURL}/v1/control/sessions`, auth(userToken), 200, 'active user PAT');
await setRevocation(userToken, true);
await sleep(1300);
await expectStatus(`${controlURL}/v1/control/sessions`, auth(userToken), 401, 'revoked user PAT');
await setRevocation(userToken, false);
await sleep(700);
await expectStatus(`${controlURL}/v1/control/sessions`, auth(userToken), 200, 'reactivated user PAT');

const event = {
  id: `local-e2e-${Date.now()}`,
  type: 'user.created',
  occurredAt: new Date().toISOString(),
  provision: true,
  user: {
    id: userID,
    externalSubject: 'local-subject',
    organizationId: 'org-local',
    quota: { cpus: '0.5', memoryBytes: 536870912, pids: 64, maxSessions: 8, maxParticipants: 32 },
  },
};
const eventBody = JSON.stringify(event);
await expectStatus(`${controlURL}/v1/hooks/users`, {
  method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Pie-Timestamp': String(Math.floor(Date.now() / 1000)), 'X-Pie-Signature': 'v1=bad' }, body: eventBody,
}, 401, 'invalid lifecycle signature');
const lifecycle = await signedLifecycle(eventBody);
if (!lifecycle.provisioned || lifecycle.user?.id !== userID) throw new Error(`unexpected lifecycle response: ${JSON.stringify(lifecycle)}`);

const deviceID = `executor-${userID}`;
await poll(async () => {
  const snapshot = await managerJSON('/v1/admin/snapshot', 'pat-local-admin');
  const device = snapshot.devices?.find((value) => value.id === deviceID);
  return device?.runtimeHealthy ? device : null;
}, 90_000, 'healthy Docker Executor device');

const executorContainer = dockerOutput([
  'ps', '-a', '--filter', `label=pie.user_id=${userID}`, '--filter', `label=pie.manager_id=${managerID}`, '--format', '{{.Names}}',
]).trim();
if (!executorContainer || executorContainer.includes('\n')) throw new Error(`could not resolve one Executor container: ${executorContainer}`);
const inspect = JSON.parse(dockerOutput(['inspect', executorContainer]))[0];
if (inspect.Config?.User !== '10001:10001') throw new Error(`Executor user boundary is ${inspect.Config?.User}`);
if (!inspect.HostConfig?.ReadonlyRootfs) throw new Error('Executor root filesystem is writable');
if (inspect.HostConfig?.NanoCpus !== 500000000) throw new Error(`Executor NanoCpus=${inspect.HostConfig?.NanoCpus}`);
if (inspect.HostConfig?.Memory !== 536870912) throw new Error(`Executor Memory=${inspect.HostConfig?.Memory}`);
if (inspect.HostConfig?.PidsLimit !== 64) throw new Error(`Executor PidsLimit=${inspect.HostConfig?.PidsLimit}`);
if (!(inspect.HostConfig?.CapDrop || []).includes('ALL')) throw new Error('Executor did not drop all capabilities');

const job = await managerJSON(`/v1/users/${userID}/jobs`, userToken, {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: 'printf pie-local-job' }),
});
const completedJob = await poll(async () => {
  const value = await managerJSON(`/v1/jobs/${job.id}`, userToken);
  if (value.status === 'failed' || value.status === 'canceled') throw new Error(`local job ${value.status}: ${value.err || ''}`);
  return value.status === 'succeeded' ? value : null;
}, 30_000, 'Executor job completion');
if (Buffer.from(completedJob.output || '', 'base64').toString() !== 'pie-local-job') {
  throw new Error('Executor job output did not round-trip');
}

const sessionID = `local-session-${Date.now()}`;
await managerJSON('/v1/control/sessions', userToken, {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ id: sessionID, deviceId: deviceID, executionTarget: 'docker', accessMode: 'shared', transportMode: 'relay', status: 'starting', name: 'Local E2E' }),
});
const activeSession = await poll(async () => {
  const sessions = await managerJSON('/v1/control/sessions', userToken);
  const value = sessions.find((candidate) => candidate.id === sessionID);
  if (value?.status === 'error') throw new Error(`Docker session failed: ${value.lastError}`);
  return value?.status === 'active' && value.hostConnectionId ? value : null;
}, 60_000, 'active Relay-backed Docker PTY session');

await runChild(process.execPath, [resolve(repoRoot, 'scripts/e2e/relay-smoke.mjs')], {
  PIE_E2E_CONTROL_URL: controlURL,
  PIE_E2E_RELAY_URL: relayURL,
  PIE_E2E_CONTROL_TOKEN: 'pat-local-admin',
  PIE_E2E_SESSION_ID: sessionID,
  PIE_E2E_OWNER_USER_ID: userID,
  PIE_E2E_EXERCISE_CONTROL: '1',
});

const loadResult = await runRelayLoad({
  controlURL,
  relayURL,
  controlToken: 'pat-local-admin',
  sessionId: sessionID,
  ownerUserId: userID,
  participants: Number.parseInt(optional('PIE_E2E_LOAD_PARTICIPANTS', '20'), 10),
  maxP95Ms: Number.parseInt(optional('PIE_E2E_LOAD_MAX_P95_MS', '5000'), 10),
});
console.log(JSON.stringify({ stage: 'load', ...loadResult }));

const relayContainer = dockerOutput([
  'ps', '-a', '--filter', `label=com.docker.compose.project=${composeProject}`, '--filter', 'label=com.docker.compose.service=relay', '--format', '{{.Names}}',
]).trim();
if (!relayContainer || relayContainer.includes('\n')) throw new Error(`could not resolve Relay container: ${relayContainer}`);
dockerOutput(['restart', '--time', '20', relayContainer]);
await waitFor(`${relayURL}/readyz`, 60_000);
await poll(async () => {
  const snapshot = await managerJSON('/v1/admin/snapshot', 'pat-local-admin');
  const value = snapshot.sessions?.find((candidate) => candidate.id === sessionID);
  return value?.status === 'active' && value.hostConnectionId && value.hostConnectionId !== activeSession.hostConnectionId ? value : null;
}, 60_000, 'clientd reconnect after Relay restart');

await runChild(process.execPath, [resolve(repoRoot, 'scripts/e2e/relay-smoke.mjs')], {
  PIE_E2E_CONTROL_URL: controlURL,
  PIE_E2E_RELAY_URL: relayURL,
  PIE_E2E_CONTROL_TOKEN: 'pat-local-admin',
  PIE_E2E_SESSION_ID: sessionID,
  PIE_E2E_OWNER_USER_ID: userID,
  PIE_E2E_EXERCISE_CONTROL: '0',
});

if (tlsRelayURL || localCA) {
  if (!tlsRelayURL || !localCA) throw new Error('PIE_E2E_TLS_RELAY_URL and PIE_E2E_LOCAL_CA must be configured together');
  await runChild(process.execPath, [resolve(repoRoot, 'scripts/e2e/relay-smoke.mjs')], {
    NODE_EXTRA_CA_CERTS: localCA,
    PIE_E2E_CONTROL_URL: controlURL,
    PIE_E2E_RELAY_URL: tlsRelayURL,
    PIE_E2E_CONTROL_TOKEN: 'pat-local-admin',
    PIE_E2E_SESSION_ID: sessionID,
    PIE_E2E_OWNER_USER_ID: userID,
    PIE_E2E_EXERCISE_CONTROL: '0',
  });
}

const closeOperation = await managerJSON('/v1/admin/operations', 'pat-local-admin', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', 'Idempotency-Key': `close-${sessionID}` },
  body: JSON.stringify({ type: 'session.close', targetKind: 'sessions', targetId: sessionID }),
});
await poll(async () => {
  const snapshot = await managerJSON('/v1/admin/snapshot', 'pat-local-admin');
  const value = snapshot.operations?.find((candidate) => candidate.id === closeOperation.id);
  if (value?.status === 'failed') throw new Error(`session close failed: ${value.error}`);
  return value?.status === 'succeeded' ? value : null;
}, 30_000, 'session close operation');

await expectStatus(`${relayURL}/metrics`, {}, 401, 'unauthenticated Relay metrics');
await expectStatus(`${controlURL}/metrics`, {}, 401, 'unauthenticated Manager metrics');

console.log(JSON.stringify({
  ok: true,
  userID,
  executorContainer,
  sessionID,
  verified: ['pat', 'revocation', 'timeout', 'lifecycle', 'docker-quota', 'job', 'pty-relay', 'driver', 'load', 'relay-reconnect', ...(tlsRelayURL ? ['tls-wss'] : []), 'metrics-auth'],
}));

function auth(token) {
  return { headers: { Authorization: `Bearer ${token}` } };
}

async function managerJSON(path, token, options = {}) {
  const headers = { ...(options.headers || {}), Authorization: `Bearer ${token}` };
  return requestJSON(`${controlURL}${path}`, { ...options, headers });
}

async function signedLifecycle(body) {
  const timestamp = String(Math.floor(Date.now() / 1000));
  const signature = createHmac('sha256', webhookSecret).update(`${timestamp}.${body}`).digest('hex');
  return requestJSON(`${controlURL}/v1/hooks/users`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Pie-Timestamp': timestamp, 'X-Pie-Signature': `v1=${signature}` },
    body,
    timeoutMs: 120_000,
  });
}

async function setRevocation(token, revoked) {
  await requestJSON(`${mockURL}/v1/tokens/revocation`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${mockControlToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ token, revoked }),
  });
}

async function expectStatus(url, options, expected, label) {
  const response = await timedFetch(url, options);
  await response.arrayBuffer();
  if (response.status !== expected) throw new Error(`${label}: HTTP ${response.status}, expected ${expected}`);
}

async function requestJSON(url, options = {}) {
  const response = await timedFetch(url, options);
  const body = await response.text();
  if (!response.ok) throw new Error(`${options.method || 'GET'} ${url}: HTTP ${response.status} ${body}`);
  return body ? JSON.parse(body) : null;
}

async function timedFetch(url, options = {}) {
  const timeoutMs = options.timeoutMs ?? 15_000;
  const signal = AbortSignal.timeout(timeoutMs);
  const clean = { ...options, signal };
  delete clean.timeoutMs;
  return fetch(url, clean);
}

async function waitFor(url, timeoutMs) {
  await poll(async () => {
    try {
      const response = await timedFetch(url, { timeoutMs: 2000 });
      await response.arrayBuffer();
      return response.ok;
    } catch {
      return false;
    }
  }, timeoutMs, url);
}

async function poll(check, timeoutMs, label) {
  const deadline = Date.now() + timeoutMs;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const result = await check();
      if (result) return result;
    } catch (error) {
      lastError = error;
      if (!String(error.message).includes('fetch failed')) throw error;
    }
    await sleep(250);
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`);
}

function dockerOutput(args) {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function runChild(command, args, extraEnv) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, ...extraEnv } });
    child.on('error', reject);
    child.on('exit', (code, signal) => {
      if (code === 0) resolvePromise();
      else reject(new Error(`${command} exited with ${code ?? signal}`));
    });
  });
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
