#!/usr/bin/env node

import { createHash, randomBytes } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { chmodSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, '../..');
const generatedRoot = resolve(repoRoot, 'deploy/local/.generated');
const runID = `${process.pid}-${Date.now()}`;
const managerID = `chat-e2e-${runID}`;
const managerContainer = `pie-${managerID}-manager`;
const dataRoot = resolve(generatedRoot, managerID);
const envFile = resolve(dataRoot, 'manager.env');
const adminToken = `pie-e2e-admin-${randomBytes(24).toString('hex')}`;
const relaySecret = required('RELAY_JWT_SECRET');
const routingSecret = randomBytes(32).toString('hex');
const relayNodeID = optional('PIE_E2E_RELAY_NODE_ID', 'relay-1');
const relayPoolID = optional('PIE_E2E_RELAY_POOL_ID', 'pie-relay-default');
const relayPublicURL = optional('PIE_E2E_RELAY_PUBLIC_URL', `http://host.docker.internal:${optional('PIE_LOCAL_RELAY_PORT', '13412')}`);
const controlNetwork = optional('PIE_E2E_CONTROL_NETWORK', `${optional('PIE_E2E_COMPOSE_PROJECT', 'pie-relay-local')}_control`);
const egressNetwork = optional('PIE_E2E_EGRESS_NETWORK', `${optional('PIE_E2E_COMPOSE_PROJECT', 'pie-relay-local')}_manager-egress`);
const executorNetwork = optional('PIE_E2E_EXECUTOR_NETWORK', 'pie-executor');
const relayContainer = resolveComposeContainer('relay');
const externalUserID = 'same-external-user';
const credentialValue = `pie-e2e-credential-${randomBytes(24).toString('hex')}`;
let managerURL = '';
const managerPort = await availablePort();

mkdirSync(dataRoot, { recursive: true, mode: 0o700 });
writeFileSync(envFile, [
  'PIE_EXECUTOR_MANAGER_ADDR=:19090',
  `PIE_EXECUTOR_MANAGER_TOKEN=${adminToken}`,
  `PIE_RELAY_JWT_SECRET=${relaySecret}`,
  `PIE_RELAY_ROUTING_SECRET=${routingSecret}`,
  'PIE_RELAY_URL=ws://relay:13412/ws/agent',
  `PIE_RELAY_DEFAULT_POOL_ID=${relayPoolID}`,
  `PIE_EXECUTOR_MANAGER_ID=${managerID}`,
  'PIE_EXECUTOR_IMAGE=pie-relay-client-e2e:latest',
  'PIE_EXECUTOR_CONTAINER_USER=10001:10001',
  `PIE_EXECUTOR_NETWORK=${executorNetwork}`,
  `PIE_EXECUTOR_REGISTRY_DIR=${dataRoot}/registry`,
  `PIE_CONTROL_REGISTRY_DIR=${dataRoot}/control`,
  `PIE_EXECUTOR_BLOB_DIR=${dataRoot}/blobs`,
  `PIE_EXECUTOR_WORK_DIR=${dataRoot}/workspaces`,
  `PIE_EXECUTOR_STATE_DIR=${dataRoot}/executor-state`,
  `PIE_CHAT_JOURNAL_DIR=${dataRoot}/chat-journal`,
  'PIE_CONTROL_RECONCILE_INTERVAL=500ms',
  'PIE_CONTROL_RECONCILE_CONCURRENCY=4',
  'PIE_CONTROL_OPERATION_CONCURRENCY=2',
].join('\n') + '\n', { mode: 0o600 });
chmodSync(envFile, 0o600);

try {
  assertDockerNetwork(controlNetwork);
  assertDockerNetwork(egressNetwork);
  assertDockerNetwork(executorNetwork);
  // Docker Desktop does not publish host ports for a container whose primary
  // network is `internal`. Use the egress network for the published E2E API,
  // then attach the private control network for Relay DNS/connectivity.
  docker(['run', '-d', '--name', managerContainer, '--network', egressNetwork,
    '-p', `127.0.0.1:${managerPort}:19090`, '-v', '/var/run/docker.sock:/var/run/docker.sock',
    '-v', `${dataRoot}:${dataRoot}`, '--env-file', envFile, 'pie-executor-manager:local']);
  docker(['network', 'connect', controlNetwork, managerContainer]);
  managerURL = `http://127.0.0.1:${managerPort}`;
  await waitFor(`${managerURL}/readyz`, 120_000);
  await admin('/v1/admin/nodes', {
    method: 'POST', body: {
      id: relayNodeID, kind: 'relay', status: 'ready', address: relayPublicURL,
      controlAddress: 'http://relay:13412', poolId: relayPoolID,
      allowedApplications: ['pie-control'], lastHeartbeat: new Date().toISOString(),
    },
  }, 201);

  const registeredA = await admin('/v1/admin/integrations', {
    method: 'POST', body: { id: 'partner-a', displayName: 'Partner A', status: 'active', maxUsers: 1, maxConversationsPerUser: 1, credential: { targetPath: '.partner/credential.json', format: 'json', maxBytes: 65536 } },
  }, 201);
  const tokenA = registeredA.serviceToken;
  if (!tokenA?.startsWith('pie_int_')) throw new Error('Integration A did not return its one-time service token');
  const listed = await admin('/v1/admin/integrations', {}, 200);
  const snapshot = await admin('/v1/admin/snapshot', {}, 200);
  const safeAdminPayload = JSON.stringify({ listed, snapshot });
  if (safeAdminPayload.includes(tokenA) || safeAdminPayload.includes('tokenHash')) throw new Error('Integration secret leaked through an admin read API');

  const firstA = await integration('partner-a', tokenA, `/users/${externalUserID}`, {
    method: 'PUT', headers: { 'Idempotency-Key': 'signup-a' }, body: { credential: { pat: credentialValue, issuer: 'partner-a' } },
  }, 201);
  const repeatedA = await integration('partner-a', tokenA, `/users/${externalUserID}`, {
    method: 'PUT', headers: { 'Idempotency-Key': 'signup-a' }, body: { credential: { pat: credentialValue, issuer: 'partner-a' } },
  }, 200);
  if (firstA.id !== repeatedA.id || firstA.ownerUserId !== repeatedA.ownerUserId || repeatedA.credentialVersion !== 1) {
    throw new Error('Provisioning idempotency did not preserve one owner/container/credential version');
  }
  if (JSON.stringify({ firstA, repeatedA }).includes(credentialValue)) throw new Error('credential value leaked in provisioning response');
  await expectStatus(`${managerURL}/v1/integrations/partner-a/users/quota-overflow`, {
    ...bearer(tokenA), method: 'PUT', headers: { ...bearer(tokenA).headers, 'Content-Type': 'application/json', 'Idempotency-Key': 'quota-overflow' }, body: JSON.stringify({ credential: { pat: 'must-not-provision' } }),
  }, 429, 'Integration user quota');

  const registeredB = await admin('/v1/admin/integrations', {
    method: 'POST', body: { id: 'partner-b', displayName: 'Partner B', status: 'active', credential: { targetPath: '.partner/credential.json', format: 'json', maxBytes: 65536 } },
  }, 201);
  const tokenB = registeredB.serviceToken;
  const firstB = await integration('partner-b', tokenB, `/users/${externalUserID}`, {
    method: 'PUT', headers: { 'Idempotency-Key': 'signup-b' }, body: { credential: { pat: `partner-b-${runID}` } },
  }, 201);
  if (firstA.ownerUserId === firstB.ownerUserId) throw new Error('two Integrations shared the same internal owner');

  const containerA = await waitForExecutor(firstA.ownerUserId);
  const containerB = await waitForExecutor(firstB.ownerUserId);
  if (containerA === containerB) throw new Error('two Integration users shared one container');
  verifyCredential(containerA, firstA.ownerUserId, credentialValue);

  const project = await integration('partner-a', tokenA, `/users/${externalUserID}/projects`, {
    method: 'POST', headers: { 'Idempotency-Key': 'project-a' }, body: { name: 'E2E Project', locale: 'ko' },
  }, 201);
  if (project.status !== 'ready' || project.workingDir !== `/workspace/projects/${project.id}`) throw new Error('Kroot project was not initialized in its opaque workspace');
  const initializedName = docker(['exec', containerA, 'cat', `${project.workingDir}/.kroot/e2e-project-name`]).trim();
  if (initializedName !== 'E2E Project') throw new Error(`kroot init received the wrong project name: ${initializedName}`);

  const conversation = await integration('partner-a', tokenA, `/users/${externalUserID}/conversations`, {
    method: 'POST', headers: { 'Idempotency-Key': 'conversation-a' }, body: { projectId: project.id },
  }, 201);
  await poll(async () => {
    const value = await integration('partner-a', tokenA, `/conversations/${conversation.id}`, {}, 200);
    if (value.status === 'error') throw new Error(`conversation entered error state: ${value.lastError || 'unknown'}`);
    return value.status === 'ready' ? value : null;
  }, 90_000, 'ready chat conversation');

  const workspaceBase = `/users/${externalUserID}/projects/${project.id}/workspace`;
  const workspaceQuery = `conversationId=${encodeURIComponent(conversation.id)}`;
  const rootTree = await integration('partner-a', tokenA, `${workspaceBase}/tree?${workspaceQuery}&path=`, {
    headers: { 'Idempotency-Key': 'workspace-tree-root' },
  }, 200);
  if (!rootTree.entries?.some((entry) => entry.path === 'apps' && entry.type === 'directory')) {
    throw new Error('workspace tree did not expose the initialized project directory');
  }
  const sourcePath = 'apps/web/server.mjs';
  const sourceQuery = `${workspaceQuery}&path=${encodeURIComponent(sourcePath)}`;
  const source = await integration('partner-a', tokenA, `${workspaceBase}/file?${sourceQuery}`, {
    headers: { 'Idempotency-Key': 'workspace-read-source' },
  }, 200);
  if (!source.content?.includes('createServer') || !source.revision?.startsWith('sha256:')) {
    throw new Error('workspace file read did not return source and revision');
  }
  const sourceMarker = `// workspace-e2e-${runID}`;
  const changed = await integration('partner-a', tokenA, `${workspaceBase}/file`, {
    method: 'PUT', headers: { 'Idempotency-Key': 'workspace-write-source' }, body: {
      conversationId: conversation.id,
      path: sourcePath,
      content: `${source.content}\n${sourceMarker}\n`,
      baseRevision: source.revision,
    },
  }, 200);
  if (!changed.revision?.startsWith('sha256:') || changed.revision === source.revision) {
    throw new Error('workspace write did not advance the content revision');
  }
  const duplicateWrite = await integration('partner-a', tokenA, `${workspaceBase}/file`, {
    method: 'PUT', headers: { 'Idempotency-Key': 'workspace-write-source' }, body: {
      conversationId: conversation.id,
      path: sourcePath,
      content: `${source.content}\n${sourceMarker}\n`,
      baseRevision: source.revision,
    },
  }, 200);
  if (duplicateWrite.revision !== changed.revision) throw new Error('workspace write retry was not idempotent');
  await expectStatus(`${managerURL}/v1/integrations/partner-a${workspaceBase}/file`, {
    ...bearer(tokenA), method: 'PUT', headers: { ...bearer(tokenA).headers, 'Content-Type': 'application/json', 'Idempotency-Key': 'workspace-stale-write' },
    body: JSON.stringify({ conversationId: conversation.id, path: sourcePath, content: source.content, baseRevision: source.revision }),
  }, 409, 'Workspace stale revision');
  const changedSource = await integration('partner-a', tokenA, `${workspaceBase}/file?${sourceQuery}`, {
    headers: { 'Idempotency-Key': 'workspace-read-changed' },
  }, 200);
  if (!changedSource.content.includes(sourceMarker)) throw new Error('workspace saved content was not readable');
  await integration('partner-a', tokenA, `${workspaceBase}/file`, {
    method: 'PUT', headers: { 'Idempotency-Key': 'workspace-restore-source' }, body: {
      conversationId: conversation.id, path: sourcePath, content: source.content, baseRevision: changedSource.revision,
    },
  }, 200);
  await expectStatus(`${managerURL}/v1/integrations/partner-a${workspaceBase}/tree?${workspaceQuery}&path=.kroot`, {
    ...bearer(tokenA), headers: { ...bearer(tokenA).headers, 'Idempotency-Key': 'workspace-protected-path' },
  }, 403, 'Workspace protected path');
  await expectStatus(`${managerURL}/v1/integrations/partner-b${workspaceBase}/file?${sourceQuery}`, {
    ...bearer(tokenB), headers: { ...bearer(tokenB).headers, 'Idempotency-Key': 'workspace-cross-integration' },
  }, 404, 'Workspace cross-Integration access');

  await expectStatus(`${managerURL}/v1/integrations/partner-a/users/${externalUserID}/conversations`, {
    ...bearer(tokenA), method: 'POST', headers: { ...bearer(tokenA).headers, 'Content-Type': 'application/json', 'Idempotency-Key': 'conversation-quota-overflow' }, body: JSON.stringify({ projectId: project.id }),
  }, 429, 'Conversation quota');

  await integration('partner-a', tokenA, `/conversations/${conversation.id}/messages`, {
    method: 'POST', headers: { 'Idempotency-Key': 'working-directory' }, body: { prompt: 'report-working-directory' },
  }, 202);
  await waitForText(tokenA, conversation.id, `pie-e2e:cwd:${project.workingDir}`);

  await integration('partner-a', tokenA, `/conversations/${conversation.id}/messages`, {
    method: 'POST', headers: { 'Idempotency-Key': 'message-a' }, body: { prompt: 'hello' },
  }, 202);
  const duplicate = await integration('partner-a', tokenA, `/conversations/${conversation.id}/messages`, {
    method: 'POST', headers: { 'Idempotency-Key': 'message-a' }, body: { prompt: 'hello' },
  }, 200);
  if (!duplicate.duplicate) throw new Error('duplicate message was not deduplicated');
  await waitForText(tokenA, conversation.id, 'pie-e2e:hello');
  const journalPath = resolve(dataRoot, 'chat-journal', `${conversation.id}.jsonl`);
  if (!fileExists(journalPath)) throw new Error('chat event journal was not materialized');
  const journalText = readFileSync(journalPath, 'utf8');
  if (journalText.includes(sourceMarker) || journalText.includes(source.content)) {
    throw new Error('workspace source content leaked into the durable chat journal');
  }
  const initialEvents = await events(tokenA, conversation.id);
  if (initialEvents.filter((event) => event.type === 'request.accepted' && event.requestId === 'message-a').length !== 1) throw new Error('message idempotency journal contains duplicate accepted requests');

  await expectStatus(`${managerURL}/v1/integrations/partner-b/conversations/${conversation.id}`, bearer(tokenB), 403, 'B token reading A conversation');

  await integration('partner-a', tokenA, `/conversations/${conversation.id}/messages`, {
    method: 'POST', headers: { 'Idempotency-Key': 'permission-turn' }, body: { prompt: 'request-permission' },
  }, 202);
  const permission = await poll(async () => {
    const values = await events(tokenA, conversation.id);
    return values.find((event) => event.type === 'permission_request') || null;
  }, 30_000, 'permission request');
  await expectStatus(`${managerURL}/v1/integrations/partner-a/conversations/${conversation.id}/messages`, {
    ...bearer(tokenA), method: 'POST', headers: { ...bearer(tokenA).headers, 'Content-Type': 'application/json', 'Idempotency-Key': 'overlapping-turn' }, body: JSON.stringify({ prompt: 'must-wait' }),
  }, 409, 'overlapping chat turn');
  await integration('partner-a', tokenA, `/conversations/${conversation.id}/permissions/${encodeURIComponent(permission.data.requestId)}`, {
    method: 'POST', headers: { 'Idempotency-Key': 'permission-answer' }, body: { allow: true },
  }, 202);
  await waitForText(tokenA, conversation.id, 'pie-e2e:permission-allowed');

  docker(['restart', '--time', '20', relayContainer]);
  await waitForRelay();
  await integration('partner-a', tokenA, `/conversations/${conversation.id}/messages`, {
    method: 'POST', headers: { 'Idempotency-Key': 'after-relay-restart' }, body: { prompt: 'relay-recovered' },
  }, 202);
  await waitForText(tokenA, conversation.id, 'pie-e2e:relay-recovered', 90_000);

  await integration('partner-a', tokenA, `/conversations/${conversation.id}/messages`, {
    method: 'POST', headers: { 'Idempotency-Key': 'manager-inflight-recovery' }, body: { prompt: 'slow-manager-recovery' },
  }, 202);
  docker(['restart', '--time', '20', managerContainer]);
  await waitFor(`${managerURL}/readyz`, 120_000);
  const restoredBinding = await integration('partner-a', tokenA, `/users/${externalUserID}`, {}, 200);
  if (restoredBinding.ownerUserId !== firstA.ownerUserId) throw new Error('Manager restart lost Integration ownership');
  await waitForText(tokenA, conversation.id, 'pie-e2e:slow-manager-recovery', 90_000);
  const recoveryEvents = await events(tokenA, conversation.id);
  if (recoveryEvents.filter((event) => event.type === 'text' && event.data?.text === 'pie-e2e:slow-manager-recovery').length !== 1) {
    throw new Error('Manager in-flight recovery executed or persisted the response more than once');
  }
  const metricsResponse = await fetch(`${managerURL}/metrics`, { ...bearer(adminToken), signal: AbortSignal.timeout(5000) });
  const metrics = await metricsResponse.text();
  if (!metricsResponse.ok || !metrics.includes('pie_control_integration_users ') || !metrics.includes('pie_chat_gateway_active_turns ')) {
    throw new Error('Integration/Chat operational metrics are missing');
  }

  docker(['restart', '--time', '20', containerA]);
  await integration('partner-a', tokenA, `/conversations/${conversation.id}/messages`, {
    method: 'POST', headers: { 'Idempotency-Key': 'after-executor-restart' }, body: { prompt: 'executor-recovered' },
  }, 202);
  await waitForText(tokenA, conversation.id, 'pie-e2e:executor-recovered', 90_000);

  const suspendedA = await integration('partner-a', tokenA, `/users/${externalUserID}`, { method: 'DELETE' }, 202);
  if (suspendedA.status !== 'suspended') throw new Error(`deleted Integration user state is ${suspendedA.status}`);
  await poll(() => !dockerContainerExists(containerA), 30_000, 'A container removal');
  const credentialPath = resolve(dataRoot, 'executor-state', firstA.ownerUserId, '.partner/credential.json');
  if (fileExists(credentialPath)) throw new Error('A credential remains after user suspension');
  if (fileExists(journalPath)) throw new Error('A conversation journal remains after user suspension');
  await expectStatus(`${managerURL}/v1/integrations/partner-a/conversations/${conversation.id}/messages`, {
    ...bearer(tokenA), method: 'POST', headers: { ...bearer(tokenA).headers, 'Content-Type': 'application/json', 'Idempotency-Key': 'after-delete' }, body: JSON.stringify({ prompt: 'must-fail' }),
  }, 409, 'message after user suspension');
  await integration('partner-b', tokenB, `/users/${externalUserID}`, { method: 'DELETE' }, 202);
  await poll(() => !dockerContainerExists(containerB), 30_000, 'B container removal');

  console.log(JSON.stringify({
    ok: true,
    verified: ['one-time-integration-token', 'provision-idempotency', 'credential-redaction', 'credential-mode-owner', 'integration-isolation', 'docker-isolation', 'container-kroot-project-init', 'project-working-directory', 'workspace-tree-read-write', 'workspace-revision-conflict', 'workspace-idempotency', 'workspace-secret-boundary', 'workspace-journal-redaction', 'user-and-conversation-quota', 'relay-chat', 'message-idempotency', 'single-active-turn', 'permission-roundtrip', 'relay-recovery', 'manager-inflight-recovery', 'executor-session-recovery', 'clientd-live-deduplication', 'operational-metrics', 'user-cleanup'],
  }));
} finally {
  try { docker(['rm', '-f', managerContainer]); } catch { /* already removed */ }
  try {
    const leftovers = docker(['ps', '-aq', '--filter', `label=pie.manager_id=${managerID}`]).trim().split(/\s+/).filter(Boolean);
    if (leftovers.length) docker(['rm', '-f', ...leftovers]);
  } catch { /* best-effort cleanup */ }
  rmSync(dataRoot, { recursive: true, force: true });
}

async function admin(path, options = {}, expected = 200) {
  return requestJSON(`${managerURL}${path}`, adminAuth(options), expected);
}

async function integration(id, token, suffix, options = {}, expected = 200) {
  return requestJSON(`${managerURL}/v1/integrations/${id}${suffix}`, withJSON(options, token), expected);
}

async function events(token, conversationID) {
  return integration('partner-a', token, `/conversations/${conversationID}/events?stream=false&limit=1000`, {}, 200);
}

async function waitForText(token, conversationID, expected, timeout = 30_000) {
  return poll(async () => {
    const values = await events(token, conversationID);
    const textEvent = values.find((event) => event.type === 'text' && event.data?.text === expected);
    if (!textEvent) return null;
    return values.find((event) => event.type === 'done' && event.sequence > textEvent.sequence) || null;
  }, timeout, expected);
}

async function waitForExecutor(ownerUserID) {
  return poll(async () => {
    const names = docker(['ps', '--filter', `label=pie.user_id=${ownerUserID}`, '--filter', `label=pie.manager_id=${managerID}`, '--format', '{{.Names}}']).trim().split(/\s+/).filter(Boolean);
    return names.length === 1 ? names[0] : null;
  }, 90_000, `Executor ${ownerUserID}`);
}

function verifyCredential(container, ownerUserID, expectedValue) {
  const script = `const fs=require('fs'),c=require('crypto'),p=process.env.HOME+'/.partner/credential.json',b=fs.readFileSync(p),s=fs.statSync(p);process.stdout.write(JSON.stringify({digest:c.createHash('sha256').update(b).digest('hex'),mode:s.mode&511,uid:s.uid,gid:s.gid}))`;
  const result = JSON.parse(docker(['exec', container, 'node', '-e', script]));
  const expected = createHash('sha256').update(JSON.stringify({ pat: expectedValue, issuer: 'partner-a' })).digest('hex');
  if (result.digest !== expected || result.mode !== 0o600 || result.uid !== 10001 || result.gid !== 10001) {
    throw new Error(`credential boundary mismatch for ${ownerUserID}`);
  }
}

function adminAuth(options = {}) {
  return withJSON(options, adminToken);
}

function withJSON(options = {}, token) {
  const headers = { ...(options.headers || {}), Authorization: `Bearer ${token}` };
  const value = { ...options, headers };
  if (options.body !== undefined && typeof options.body !== 'string') {
    headers['Content-Type'] = 'application/json';
    value.body = JSON.stringify(options.body);
  }
  return value;
}

function bearer(token) {
  return { headers: { Authorization: `Bearer ${token}` } };
}

async function requestJSON(url, options, expected) {
  const response = await fetch(url, { ...options, signal: AbortSignal.timeout(30_000) });
  const text = await response.text();
  if (response.status !== expected) throw new Error(`${options.method || 'GET'} ${new URL(url).pathname}: HTTP ${response.status}, expected ${expected}`);
  return text ? JSON.parse(text) : null;
}

async function expectStatus(url, options, expected, label) {
  const response = await fetch(url, { ...options, signal: AbortSignal.timeout(30_000) });
  await response.arrayBuffer();
  if (response.status !== expected) throw new Error(`${label}: HTTP ${response.status}, expected ${expected}`);
}

async function waitFor(url, timeout) {
  return poll(async () => {
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(2000) });
      await response.arrayBuffer();
      return response.ok;
    } catch {
      return false;
    }
  }, timeout, url);
}

async function waitForRelay() {
  const port = optional('PIE_LOCAL_RELAY_PORT', '13412');
  return waitFor(`http://127.0.0.1:${port}/readyz`, 90_000);
}

async function poll(check, timeout, label) {
  const deadline = Date.now() + timeout;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const value = await check();
      if (value) return value;
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 250));
  }
  throw new Error(`timed out waiting for ${label}${lastError ? ` (${lastError.message})` : ''}`);
}

function resolveComposeContainer(service) {
  const project = optional('PIE_E2E_COMPOSE_PROJECT', 'pie-relay-local');
  const value = docker(['ps', '-a', '--filter', `label=com.docker.compose.project=${project}`, '--filter', `label=com.docker.compose.service=${service}`, '--format', '{{.Names}}']).trim();
  if (!value || value.includes('\n')) throw new Error(`could not resolve ${service} container for ${project}`);
  return value;
}

function assertDockerNetwork(name) {
  docker(['network', 'inspect', name]);
}

function docker(args) {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

function dockerContainerExists(name) {
  try {
    docker(['inspect', name]);
    return true;
  } catch {
    return false;
  }
}

function fileExists(path) {
  try {
    statSync(path);
    return true;
  } catch {
    return false;
  }
}

function required(name) {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function optional(name, fallback) {
  return process.env[name]?.trim() || fallback;
}

function availablePort() {
  return new Promise((resolvePromise, reject) => {
    const server = createServer();
    server.unref();
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      server.close((error) => error ? reject(error) : resolvePromise(address.port));
    });
  });
}
