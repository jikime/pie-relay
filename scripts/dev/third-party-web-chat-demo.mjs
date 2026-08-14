#!/usr/bin/env node

import { randomBytes } from 'node:crypto'
import { execFileSync, spawn } from 'node:child_process'
import {
  chmodSync,
  closeSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { hashPassword } from '../../examples/third-party-web-chat/src/auth.mjs'
import { createKrootCredential, isKrootPATCredential } from '../../examples/third-party-web-chat/src/kroot-credential.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(here, '../..')
const stateRoot = resolve(repoRoot, '.local/pie-web-chat-demo')
const managerName = 'pie-web-chat-demo-manager'
const postgresName = 'pie-web-chat-demo-postgres'
const managerID = 'web-chat-demo'
const managerURL = 'http://127.0.0.1:19190'
const appURL = 'http://127.0.0.1:4175'
const integrationID = 'sample-web-chat-demo'
const externalUserID = 'demo-alice'
const username = 'alice'
const password = process.env.PIE_DEMO_PASSWORD?.trim() || 'PieDemo!2026'
const relayURL = process.env.PIE_E2E_RELAY_URL?.trim() || 'https://relay.cookai.dev'
// The public cookai.dev Relay runs with strict scoped-token validation.  Its
// advertised node/pool must therefore be the demo defaults as well; a
// syntactically valid token for another pool only fails later at the WSS
// handshake and leaves clientd looking like it is stuck on "connecting".
// Local/custom Relay installations can still override both values explicitly.
const relayNodeID = process.env.PIE_E2E_RELAY_NODE_ID?.trim() || 'sandbox-relay-01'
const relayPoolID = process.env.PIE_E2E_RELAY_POOL_ID?.trim() || 'pie-relay-sandbox'
const managerImage = 'pie-executor-manager:local'
const executorImage = process.env.PIE_DEMO_EXECUTOR_IMAGE?.trim() || 'pie-relay-client-kroot:local'
const krootServerURL = process.env.PIE_KROOT_SERVER_URL?.trim() || 'grpcs://adk-server.kroot.io'
const krootRelayURL = process.env.PIE_KROOT_RELAY_URL?.trim() || 'wss://adk-relay.kroot.io/ws/agent'
const demoCredential = createKrootCredential({
  pat: process.env.PIE_DEMO_KROOT_PAT?.trim() || 'kpat_demo_external_alice',
  serverURL: krootServerURL,
  relayURL: krootRelayURL,
  deviceID: '64656d6f2d616c6963652d7069653031',
  updatedAt: '2026-07-28T00:00:00.000Z',
})
const egressNetwork = 'pie-relay-local_manager-egress'
const executorNetwork = 'pie-executor'
const command = process.argv[2] || 'start'

switch (command) {
case 'start':
  await start()
  break
case 'status':
  await status()
  break
case 'stop':
  await stop()
  break
case 'relay-heartbeat':
  await relayHeartbeat()
  break
default:
  console.error('Usage: third-party-web-chat-demo.mjs start|status|stop|relay-heartbeat')
  process.exitCode = 2
}

async function start() {
  const relaySecret = required('RELAY_JWT_SECRET')
  assertImage(managerImage)
  assertImage(executorImage)
  assertNetwork(egressNetwork)
  assertNetwork(executorNetwork)
  assertPortAvailableOrOwned(4175, appURL)
  assertPortAvailableOrOwned(19190, `${managerURL}/readyz`)
  mkdirSync(stateRoot, { recursive: true, mode: 0o700 })
  const postgresPassword = secretFile('postgres-password', () => randomBytes(32).toString('hex'))
  ensurePostgres(postgresPassword)
  const usageDatabaseURL = `postgres://pie_demo:${encodeURIComponent(postgresPassword)}@${postgresName}:5432/pie_demo?sslmode=disable`
  const claudeSeedRoot = resolve(stateRoot, 'claude-state-seed')
  prepareClaudeSeed(claudeSeedRoot)
  const krootCommonRoot = resolve(stateRoot, 'kroot-common')
  execFileSync(resolve(repoRoot, 'scripts/ops/prepare-kroot-common-bundle.sh'), [
    executorImage,
    krootCommonRoot,
  ], { cwd: repoRoot, stdio: 'inherit' })

  const adminToken = secretFile('manager-admin-token', () => `pie-demo-admin-${randomBytes(32).toString('hex')}`)
  const relayRoutingSecret = secretFile('relay-routing-secret', () => randomBytes(32).toString('hex'))
  writeSecret('signup-kroot-pat', demoCredential.accessToken)
  const signupKrootPATFile = resolve(stateRoot, 'signup-kroot-pat')
  const envFile = resolve(stateRoot, 'manager.env')
  writeFileSync(envFile, [
    'PIE_EXECUTOR_MANAGER_ADDR=:19090',
    `PIE_EXECUTOR_MANAGER_TOKEN=${adminToken}`,
    `PIE_RELAY_JWT_SECRET=${relaySecret}`,
    `PIE_RELAY_ROUTING_SECRET=${relayRoutingSecret}`,
    `PIE_RELAY_URL=${relayURL}`,
    `PIE_RELAY_PUBLIC_URL=${relayURL}`,
    `PIE_RELAY_DEFAULT_POOL_ID=${relayPoolID}`,
    `PIE_EXECUTOR_MANAGER_ID=${managerID}`,
    `PIE_EXECUTOR_IMAGE=${executorImage}`,
    'PIE_EXECUTOR_CONTAINER_USER=10001:10001',
    `PIE_EXECUTOR_NETWORK=${executorNetwork}`,
    `PIE_EXECUTOR_REGISTRY_DIR=${stateRoot}/registry`,
    `PIE_CONTROL_REGISTRY_DIR=${stateRoot}/control`,
    `PIE_EXECUTOR_BLOB_DIR=${stateRoot}/blobs`,
    `PIE_EXECUTOR_WORK_DIR=${stateRoot}/workspaces`,
    `PIE_EXECUTOR_STATE_DIR=${stateRoot}/executor-state`,
    `PIE_EXECUTOR_STATE_SEED_DIR=${claudeSeedRoot}`,
    `PIE_KROOT_COMMON_BUNDLE_DIR=${krootCommonRoot}/current`,
    `PIE_CLAUDE_AUTH_DIR=${stateRoot}/claude-auth`,
    `PIE_CLAUDE_AUTH_LOGIN_DIR=${stateRoot}/claude-auth/login`,
    'PIE_CLAUDE_AUTH_REQUIRED=true',
    'PIE_CLAUDE_AUTH_ROLLOUT_CONCURRENCY=2',
    `PIE_CHAT_JOURNAL_DIR=${stateRoot}/chat-journal`,
    `PIE_USAGE_DATABASE_URL=${usageDatabaseURL}`,
    'PIE_USAGE_RECONCILE_INTERVAL=5s',
    'PIE_CONTROL_RECONCILE_INTERVAL=1s',
    'PIE_CONTROL_RECONCILE_CONCURRENCY=4',
    // 로컬 예제 PAT는 외부 Kroot 운영 계정이 아니므로 프로젝트 초기화만
    // 검증한다. 실제 Integration E2E에서만 Kroot auto-link를 켠다.
    'PIE_EXECUTOR_KROOT_AUTO_LINK=false',
    'PIE_RELAY_HEARTBEAT_TIMEOUT=24h',
  ].join('\n') + '\n', { mode: 0o600 })
  chmodSync(envFile, 0o600)

  if (containerRunning(managerName) && (
    managerEnvValue(managerName, 'PIE_EXECUTOR_IMAGE') !== executorImage
    || managerEnvValue(managerName, 'PIE_RELAY_URL') !== relayURL
    || managerEnvValue(managerName, 'PIE_RELAY_DEFAULT_POOL_ID') !== relayPoolID
    || managerEnvValue(managerName, 'PIE_USAGE_DATABASE_URL') !== usageDatabaseURL
    || managerEnvValue(managerName, 'PIE_EXECUTOR_KROOT_AUTO_LINK') !== 'false'
    || managerEnvValue(managerName, 'PIE_CLAUDE_AUTH_REQUIRED') !== 'true'
    || managerEnvValue(managerName, 'PIE_CLAUDE_AUTH_DIR') !== `${stateRoot}/claude-auth`
    || managerEnvValue(managerName, 'PIE_KROOT_COMMON_BUNDLE_DIR') !== `${krootCommonRoot}/current`
    || !containerUsesImage(managerName, managerImage)
  )) {
    docker(['rm', '-f', managerName])
  }
  if (!containerRunning(managerName)) {
    if (containerExists(managerName)) docker(['rm', '-f', managerName])
    docker([
      'run', '-d', '--name', managerName, '--network', egressNetwork,
      '-p', '127.0.0.1:19190:19090',
      '-v', '/var/run/docker.sock:/var/run/docker.sock',
      '-v', `${stateRoot}:${stateRoot}`,
      '--env-file', envFile,
      managerImage,
    ])
  }
  await waitFor(`${managerURL}/readyz`, 120_000)
  await ensureRelayNode(adminToken)
  startRelayHeartbeat()
  removeOutdatedExecutors(executorImage)

  let integrationToken = existsSync(resolve(stateRoot, 'integration-token'))
    ? readFileSync(resolve(stateRoot, 'integration-token'), 'utf8').trim()
    : ''
  const integrationResponse = await managerFetch(`/v1/admin/integrations/${integrationID}`, {}, adminToken)
  if (integrationResponse.status === 404) {
    const registered = await managerJSON('/v1/admin/integrations', {
      method: 'POST',
      body: {
        id: integrationID,
        displayName: 'Pie Web Chat Demo',
        status: 'active',
        maxUsers: 100,
        maxProjectsPerUser: 32,
        maxConversationsPerUser: 8,
        credential: { targetPath: '.kroot/credential.json', format: 'json', maxBytes: 65_536 },
      },
    }, adminToken, 201)
    integrationToken = registered.serviceToken
    writeSecret('integration-token', integrationToken)
  } else if (!integrationResponse.ok) {
    throw new Error(`read demo Integration: HTTP ${integrationResponse.status}`)
  } else {
    const currentIntegration = await integrationResponse.json()
    if (currentIntegration.maxUsers < 100 || currentIntegration.maxProjectsPerUser < 32 || currentIntegration.credential?.targetPath !== '.kroot/credential.json') {
      await managerJSON(`/v1/admin/integrations/${integrationID}`, {
        method: 'PATCH', body: {
          maxUsers: Math.max(currentIntegration.maxUsers, 100),
          maxProjectsPerUser: Math.max(currentIntegration.maxProjectsPerUser || 0, 32),
          credential: { targetPath: '.kroot/credential.json', format: 'json', maxBytes: 65_536 },
        },
      }, adminToken, 200)
    }
  }
  if (!integrationToken) {
    const rotated = await managerJSON(`/v1/admin/integrations/${integrationID}/rotate-token`, { method: 'POST', body: {} }, adminToken, 200)
    integrationToken = rotated.serviceToken
    writeSecret('integration-token', integrationToken)
  }
  const authProbe = await managerFetch(`/v1/integrations/${integrationID}/users/${externalUserID}`, {}, integrationToken)
  if (authProbe.status === 401) {
    const rotated = await managerJSON(`/v1/admin/integrations/${integrationID}/rotate-token`, { method: 'POST', body: {} }, adminToken, 200)
    integrationToken = rotated.serviceToken
    writeSecret('integration-token', integrationToken)
  }

  const binding = await managerJSON(`/v1/integrations/${integrationID}/users/${externalUserID}`, {
    method: 'PUT',
    headers: { 'Idempotency-Key': 'demo-alice-signup' },
    body: { credential: demoCredential },
  }, integrationToken, [200, 201])
  const executorName = await waitForExecutor(binding.ownerUserId)

  const usersFile = resolve(stateRoot, 'users.json')
  const users = existsSync(usersFile) ? JSON.parse(readFileSync(usersFile, 'utf8')) : []
  const alice = users.find((user) => user.username === username)
  if (!alice) {
    users.unshift({
      username,
      displayName: 'Demo Alice',
      externalUserId: externalUserID,
      passwordHash: await hashPassword(password),
      credential: demoCredential,
    })
  } else {
    alice.credential = demoCredential
  }
  for (const user of users) {
    if (isKrootPATCredential(user.credential)) continue
    user.credential = createKrootCredential({
      pat: typeof user.credential?.pat === 'string' && user.credential.pat.trim()
        ? user.credential.pat
        : `kpat_demo_migrated_${randomBytes(24).toString('base64url')}`,
      serverURL: krootServerURL,
      relayURL: krootRelayURL,
    })
  }
  writeFileSync(usersFile, `${JSON.stringify(users, null, 2)}\n`, { mode: 0o600 })
  chmodSync(usersFile, 0o600)
  for (const user of users) {
    if (user.externalUserId === externalUserID) continue
    await managerJSON(`/v1/integrations/${integrationID}/users/${encodeURIComponent(user.externalUserId)}`, {
      method: 'PUT',
      headers: { 'Idempotency-Key': `demo-user-sync-${user.username}` },
      body: { credential: user.credential },
    }, integrationToken, [200, 201])
  }

  stopPIDFile()
  await poll(() => !appHealthy(), 10_000, 'previous web chat process to stop').catch(() => {})
  const webChatRoot = resolve(repoRoot, 'examples/third-party-web-chat')
  execFileSync('npm', ['run', 'build'], { cwd: webChatRoot, stdio: 'inherit' })
  const logPath = resolve(stateRoot, 'web-chat.log')
  const log = openSync(logPath, 'a', 0o600)
  const child = spawn(process.execPath, [resolve(webChatRoot, 'node_modules/next/dist/bin/next'), 'start', '--hostname', '127.0.0.1', '--port', '4175'], {
      cwd: webChatRoot,
      detached: true,
      env: {
        ...process.env,
        PIE_MANAGER_URL: managerURL,
        PIE_INTEGRATION_ID: integrationID,
        PIE_INTEGRATION_TOKEN: integrationToken,
        PIE_WEB_CHAT_USERS_FILE: usersFile,
        PIE_WEB_CHAT_HOST: '127.0.0.1',
        PIE_WEB_CHAT_PORT: '4175',
        PIE_WEB_CHAT_SECURE_COOKIE: 'false',
        PIE_WEB_CHAT_REGISTRATION_ENABLED: 'true',
        PIE_WEB_CHAT_SIGNUP_KROOT_PAT_FILE: signupKrootPATFile,
        PIE_KROOT_SERVER_URL: krootServerURL,
        PIE_KROOT_RELAY_URL: krootRelayURL,
      },
      stdio: ['ignore', log, log],
  })
  closeSync(log)
  child.unref()
  writeFileSync(resolve(stateRoot, 'web-chat.pid'), String(child.pid), { mode: 0o600 })
  await waitFor(`${appURL}/api/health`, 30_000)

  console.log(JSON.stringify({
    ok: true,
    url: appURL,
    username,
    password,
    relay: relayURL,
    manager: managerURL,
    executor: executorName,
    route: `browser -> BFF -> Manager -> ${new URL(relayURL).host} -> Docker clientd -> Claude Code`,
  }))
}

async function status() {
  console.log(JSON.stringify({
    web: appHealthy() ? 'ready' : 'stopped',
    manager: await urlHealthy(`${managerURL}/readyz`) ? 'ready' : 'stopped',
    managerContainer: containerRunning(managerName) ? 'running' : 'stopped',
    usageDatabase: containerRunning(postgresName) ? 'running' : 'stopped',
    executors: docker(['ps', '--filter', `label=pie.manager_id=${managerID}`, '--format', '{{.Names}}']).trim().split(/\s+/).filter(Boolean),
    url: appURL,
  }))
}

async function stop() {
  stopPIDFile()
  stopRelayHeartbeatPIDFile()
  const executors = docker(['ps', '-aq', '--filter', `label=pie.manager_id=${managerID}`]).trim().split(/\s+/).filter(Boolean)
  if (executors.length) docker(['rm', '-f', ...executors])
  if (containerExists(managerName)) docker(['rm', '-f', managerName])
  if (containerExists(postgresName)) docker(['rm', '-f', postgresName])
  if (existsSync(stateRoot)) rmSync(stateRoot, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 })
  console.log(JSON.stringify({ ok: true, stopped: true }))
}

function ensurePostgres(password) {
  mkdirSync(resolve(stateRoot, 'postgres-data'), { recursive: true, mode: 0o700 })
  const envFile = resolve(stateRoot, 'postgres.env')
  writeFileSync(envFile, [
    'POSTGRES_USER=pie_demo',
    'POSTGRES_DB=pie_demo',
    `POSTGRES_PASSWORD=${password}`,
  ].join('\n') + '\n', { mode: 0o600 })
  chmodSync(envFile, 0o600)
  if (!containerRunning(postgresName)) {
    if (containerExists(postgresName)) docker(['rm', '-f', postgresName])
    docker([
      'run', '-d', '--name', postgresName, '--network', egressNetwork,
      '--env-file', envFile,
      '-v', `${stateRoot}/postgres-data:/var/lib/postgresql/data`,
      'postgres:16-alpine',
    ])
  }
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    if (execOK('docker', ['exec', postgresName, 'pg_isready', '-U', 'pie_demo', '-d', 'pie_demo'])) return
    execFileSync('sleep', ['0.25'])
  }
  throw new Error('timed out waiting for the demo usage PostgreSQL')
}

async function relayHeartbeat() {
  const adminToken = readFileSync(resolve(stateRoot, 'manager-admin-token'), 'utf8').trim()
  for (;;) {
    try {
      await ensureRelayNode(adminToken)
    } catch (error) {
      console.error(`[relay-heartbeat] ${error.message}`)
    }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 30_000))
  }
}

function prepareClaudeSeed(seedRoot) {
  const source = '/Users/jikime/.claude'
  if (!existsSync(source) || !statSync(source).isDirectory()) throw new Error('host Claude configuration is unavailable')
  const destination = resolve(seedRoot, '.claude')
  mkdirSync(destination, { recursive: true, mode: 0o700 })
  for (const name of ['.claude.json', 'settings.json']) {
    const sourceFile = resolve(source, name)
    if (existsSync(sourceFile) && statSync(sourceFile).isFile()) {
      const targetFile = resolve(destination, name)
      copyFileSync(sourceFile, targetFile)
      chmodSync(targetFile, 0o600)
    }
  }
  const credential = execFileSync('security', ['find-generic-password', '-s', 'Claude Code-credentials', '-w'], {
    encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
  })
  writeFileSync(resolve(destination, '.credentials.json'), credential, { mode: 0o600 })
}

async function managerJSON(path, options, token, expected) {
  const response = await managerFetch(path, options, token)
  const text = await response.text()
  const expectedStatuses = Array.isArray(expected) ? expected : [expected]
  if (!expectedStatuses.includes(response.status)) throw new Error(`${path}: HTTP ${response.status}, expected ${expectedStatuses.join('/')}: ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : null
}

async function ensureRelayNode(adminToken) {
  const nodes = await managerJSON('/v1/admin/nodes', {}, adminToken, 200)
  const current = nodes.find((node) => node.id === relayNodeID)
  const relayOrigin = relayHTTPOrigin(relayURL)
  const value = {
    ...(current || {}),
    id: relayNodeID,
    kind: 'relay',
    status: 'ready',
    address: relayOrigin,
    controlAddress: relayOrigin,
    poolId: relayPoolID,
    allowedApplications: ['pie-control', 'pie-canvas'],
    lastHeartbeat: new Date().toISOString(),
  }
  await managerJSON(
    current ? `/v1/admin/nodes/${encodeURIComponent(relayNodeID)}` : '/v1/admin/nodes',
    { method: current ? 'PUT' : 'POST', body: value },
    adminToken,
    current ? 200 : 201,
  )
}

function relayHTTPOrigin(value) {
  const url = new URL(value)
  if (url.protocol === 'ws:') url.protocol = 'http:'
  if (url.protocol === 'wss:') url.protocol = 'https:'
  if (!['http:', 'https:'].includes(url.protocol)) throw new Error(`unsupported Relay URL protocol: ${url.protocol}`)
  return url.origin
}

function managerFetch(path, options = {}, token) {
  const headers = { ...(options.headers || {}), Authorization: `Bearer ${token}` }
  const request = { ...options, headers, signal: AbortSignal.timeout(30_000) }
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    request.body = JSON.stringify(options.body)
  }
  return fetch(`${managerURL}${path}`, request)
}

async function waitForExecutor(ownerUserID) {
  return poll(async () => {
    const names = docker(['ps', '--filter', `label=pie.user_id=${ownerUserID}`, '--filter', `label=pie.manager_id=${managerID}`, '--format', '{{.Names}}']).trim().split(/\s+/).filter(Boolean)
    return names.length === 1 ? names[0] : null
  }, 90_000, `Executor ${ownerUserID}`)
}

function stopPIDFile() {
  stopNamedPIDFile('web-chat.pid')
}

function stopRelayHeartbeatPIDFile() {
  stopNamedPIDFile('relay-heartbeat.pid')
}

function stopNamedPIDFile(name) {
  const path = resolve(stateRoot, name)
  if (!existsSync(path)) return
  const pid = Number(readFileSync(path, 'utf8').trim())
  if (Number.isSafeInteger(pid) && pid > 1) {
    try { process.kill(pid, 'SIGTERM') } catch { /* already stopped */ }
  }
  rmSync(path, { force: true })
}

function startRelayHeartbeat() {
  stopRelayHeartbeatPIDFile()
  const logPath = resolve(stateRoot, 'relay-heartbeat.log')
  const log = openSync(logPath, 'a', 0o600)
  const child = spawn(process.execPath, [fileURLToPath(import.meta.url), 'relay-heartbeat'], {
    detached: true,
    env: process.env,
    stdio: ['ignore', log, log],
  })
  closeSync(log)
  child.unref()
  writeFileSync(resolve(stateRoot, 'relay-heartbeat.pid'), String(child.pid), { mode: 0o600 })
}

function removeOutdatedExecutors(image) {
  const names = docker(['ps', '-a', '--filter', `label=pie.manager_id=${managerID}`, '--format', '{{.Names}}'])
    .trim().split(/\s+/).filter(Boolean)
  for (const name of names) {
    if (!containerUsesImage(name, image)) docker(['rm', '-f', name])
  }
}

function containerUsesImage(name, image) {
  if (!containerExists(name)) return false
  return docker(['inspect', '--format', '{{.Image}}', name]).trim()
    === docker(['image', 'inspect', '--format', '{{.Id}}', image]).trim()
}

function appHealthy() { return execOK('curl', ['--noproxy', '*', '--fail', '--silent', '--max-time', '2', `${appURL}/api/health`]) }
async function urlHealthy(url) {
  try { return (await fetch(url, { signal: AbortSignal.timeout(2000) })).ok } catch { return false }
}
async function waitFor(url, timeout) { return poll(() => urlHealthy(url), timeout, url) }
async function poll(check, timeout, label) {
  const deadline = Date.now() + timeout
  let lastError
  while (Date.now() < deadline) {
    try {
      const value = await check()
      if (value) return value
    } catch (error) { lastError = error }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 250))
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`)
}

function assertPortAvailableOrOwned(port, healthURL) {
  if (!execOK('lsof', ['-nP', `-iTCP:${port}`, '-sTCP:LISTEN'])) return
  if (execOK('curl', ['--noproxy', '*', '--fail', '--silent', '--max-time', '2', healthURL])) return
  throw new Error(`TCP port ${port} is already in use by another process`)
}

function secretFile(name, create) {
  const path = resolve(stateRoot, name)
  if (existsSync(path)) return readFileSync(path, 'utf8').trim()
  const value = create()
  writeSecret(name, value)
  return value
}
function writeSecret(name, value) {
  const path = resolve(stateRoot, name)
  writeFileSync(path, `${value}\n`, { mode: 0o600 })
  chmodSync(path, 0o600)
}
function required(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}
function assertImage(name) { docker(['image', 'inspect', name]) }
function assertNetwork(name) { docker(['network', 'inspect', name]) }
function containerExists(name) { return execOK('docker', ['inspect', name]) }
function containerRunning(name) {
  try { return docker(['inspect', '--format', '{{.State.Running}}', name]).trim() === 'true' } catch { return false }
}
function managerEnvValue(name, key) {
  try {
    const values = docker(['inspect', '--format', '{{range .Config.Env}}{{println .}}{{end}}', name]).split('\n')
    return values.find((value) => value.startsWith(`${key}=`))?.slice(key.length + 1) || ''
  } catch { return '' }
}
function docker(args) { return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }) }
function execOK(commandName, args) {
  try { execFileSync(commandName, args, { stdio: 'ignore' }); return true } catch { return false }
}
