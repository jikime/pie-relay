#!/usr/bin/env node

import { createHash, randomBytes } from 'node:crypto'
import { execFileSync, spawn } from 'node:child_process'
import { chmodSync, copyFileSync, existsSync, mkdirSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { hashPassword } from '../../examples/third-party-web-chat/src/auth.mjs'
import { createKrootCredential } from '../../examples/third-party-web-chat/src/kroot-credential.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(here, '../..')
const webChatRoot = resolve(repoRoot, 'examples/third-party-web-chat')
const generatedRoot = resolve(repoRoot, 'deploy/local/.generated')
const runID = `${process.pid}-${Date.now()}`
const webChatDistDir = '.next-e2e'
const managerID = `web-chat-e2e-${runID}`
const managerContainer = `pie-${managerID}-manager`
const dataRoot = resolve(generatedRoot, managerID)
const managerEnvFile = resolve(dataRoot, 'manager.env')
const usersFile = resolve(dataRoot, 'web-chat-users.json')
const adminToken = `pie-web-chat-admin-${randomBytes(24).toString('hex')}`
const relaySecret = required('RELAY_JWT_SECRET')
const routingSecret = randomBytes(32).toString('hex')
const relayURL = optional('PIE_E2E_RELAY_URL', 'https://relay.cookai.dev')
const relayPublicURL = optional('PIE_E2E_RELAY_PUBLIC_URL', relayURL)
const relayNodeID = optional('PIE_E2E_RELAY_NODE_ID', 'relay-1')
const relayPoolID = optional('PIE_E2E_RELAY_POOL_ID', 'pie-relay-default')
const managerImage = optional('PIE_E2E_MANAGER_IMAGE', 'pie-executor-manager:local')
const executorImage = optional('PIE_E2E_EXECUTOR_IMAGE', 'pie-relay-client-e2e:latest')
const egressNetwork = optional('PIE_E2E_EGRESS_NETWORK', `${optional('PIE_E2E_COMPOSE_PROJECT', 'pie-relay-local')}_manager-egress`)
const controlNetwork = process.env.PIE_E2E_CONTROL_NETWORK?.trim() || ''
const executorNetwork = optional('PIE_E2E_EXECUTOR_NETWORK', 'pie-executor')
const integrationID = `sample-web-chat-${runID}`
const alicePassword = `Alice-${randomBytes(12).toString('base64url')}`
const bobPassword = `Bob-${randomBytes(12).toString('base64url')}`
const signupUsername = `signup-${process.pid}-${Date.now()}`
const signupPassword = `Signup-${randomBytes(12).toString('base64url')}`
const testPNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII='
const testPNGBytes = Buffer.from(testPNG, 'base64')
const imageExpectedText = `pie-e2e:image-attachment:1:image/png:${createHash('sha256').update(testPNGBytes).digest('hex').slice(0, 16)}`
const krootServerURL = optional('PIE_KROOT_SERVER_URL', 'grpcs://adk-server.kroot.io')
const krootRelayURL = optional('PIE_KROOT_RELAY_URL', 'wss://adk-relay.kroot.io/ws/agent')
const aliceCredential = createKrootCredential({
  pat: `kpat_e2e_alice_${randomBytes(16).toString('hex')}`,
  serverURL: krootServerURL,
  relayURL: krootRelayURL,
  deviceID: randomBytes(16).toString('hex'),
})
const bobCredential = createKrootCredential({
  pat: `kpat_e2e_bob_${randomBytes(16).toString('hex')}`,
  serverURL: krootServerURL,
  relayURL: krootRelayURL,
  deviceID: randomBytes(16).toString('hex'),
})
const managerPort = await availablePort()
const appPort = await availablePort()
const managerURL = `http://127.0.0.1:${managerPort}`
const appURL = `http://127.0.0.1:${appPort}`
let serviceToken = ''
let appProcess = null
let aliceBinding = null
let bobBinding = null

mkdirSync(dataRoot, { recursive: true, mode: 0o700 })
assertDockerImage(managerImage)
assertDockerImage(executorImage)
assertDockerNetwork(egressNetwork)
assertDockerNetwork(executorNetwork)
if (controlNetwork) assertDockerNetwork(controlNetwork)

writeFileSync(managerEnvFile, [
  'PIE_EXECUTOR_MANAGER_ADDR=:19090',
  `PIE_EXECUTOR_MANAGER_TOKEN=${adminToken}`,
  `PIE_RELAY_JWT_SECRET=${relaySecret}`,
  `PIE_RELAY_ROUTING_SECRET=${routingSecret}`,
  `PIE_RELAY_URL=${relayURL}`,
  `PIE_RELAY_PUBLIC_URL=${relayPublicURL}`,
  `PIE_RELAY_DEFAULT_POOL_ID=${relayPoolID}`,
  `PIE_EXECUTOR_MANAGER_ID=${managerID}`,
  `PIE_EXECUTOR_IMAGE=${executorImage}`,
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
].join('\n') + '\n', { mode: 0o600 })
chmodSync(managerEnvFile, 0o600)

try {
  docker(['run', '-d', '--name', managerContainer, '--network', egressNetwork,
    '-p', `127.0.0.1:${managerPort}:19090`, '-v', '/var/run/docker.sock:/var/run/docker.sock',
    '-v', `${dataRoot}:${dataRoot}`, '--env-file', managerEnvFile, managerImage])
  if (controlNetwork) docker(['network', 'connect', controlNetwork, managerContainer])
  await waitFor(`${managerURL}/readyz`, 120_000)
  const relayOrigin = relayHTTPOrigin(relayURL)
  const relayPublicOrigin = relayHTTPOrigin(relayPublicURL)
  await managerJSON('/v1/admin/nodes', {
    method: 'POST',
    body: {
      id: relayNodeID,
      kind: 'relay',
      status: 'ready',
      address: relayPublicOrigin,
      controlAddress: relayOrigin,
      poolId: relayPoolID,
      allowedApplications: ['pie-control'],
      lastHeartbeat: new Date().toISOString(),
    },
  }, adminToken, 201)

  const registered = await managerJSON('/v1/admin/integrations', {
    method: 'POST',
    body: {
      id: integrationID,
      displayName: 'Pie Web Chat E2E',
      status: 'active',
      maxUsers: 3,
      maxConversationsPerUser: 2,
      credential: { targetPath: '.kroot/credential.json', format: 'json', maxBytes: 65_536 },
    },
  }, adminToken, 201)
  serviceToken = registered.serviceToken
  if (!serviceToken?.startsWith('pie_int_')) throw new Error('Manager did not return the one-time Integration service token')

  writeFileSync(usersFile, JSON.stringify([
    {
      username: 'alice', displayName: 'Alice', externalUserId: 'web-alice',
      passwordHash: await hashPassword(alicePassword),
      credential: aliceCredential,
    },
    {
      username: 'bob', displayName: 'Bob', externalUserId: 'web-bob',
      passwordHash: await hashPassword(bobPassword),
      credential: bobCredential,
    },
  ], null, 2), { mode: 0o600 })
  chmodSync(usersFile, 0o600)

  await removeTreeEventually(resolve(webChatRoot, webChatDistDir))
  execFileSync('npm', ['run', 'build'], {
    cwd: webChatRoot,
    env: { ...process.env, PIE_WEB_CHAT_DIST_DIR: webChatDistDir },
    stdio: 'inherit',
  })
  appProcess = spawn(process.execPath, [resolve(webChatRoot, 'node_modules/next/dist/bin/next'), 'start', '--hostname', '127.0.0.1', '--port', String(appPort)], {
    cwd: webChatRoot,
    env: {
      ...process.env,
      PIE_MANAGER_URL: managerURL,
      PIE_INTEGRATION_ID: integrationID,
      PIE_INTEGRATION_TOKEN: serviceToken,
      PIE_WEB_CHAT_USERS_FILE: usersFile,
      PIE_WEB_CHAT_HOST: '127.0.0.1',
      PIE_WEB_CHAT_PORT: String(appPort),
      PIE_WEB_CHAT_SECURE_COOKIE: 'false',
      PIE_WEB_CHAT_REGISTRATION_ENABLED: 'true',
      PIE_WEB_CHAT_DIST_DIR: webChatDistDir,
      PIE_KROOT_SERVER_URL: krootServerURL,
      PIE_KROOT_RELAY_URL: krootRelayURL,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  const appOutput = boundedOutput(appProcess)
  await waitFor(`${appURL}/api/health`, 30_000, () => appOutput())

  const alice = await login('alice', alicePassword)
  const bob = await login('bob', bobPassword)
  const aliceWorkspace = await appJSON(alice, '/api/workspace/provision', { method: 'POST', body: {} }, 200)
  const bobWorkspace = await appJSON(bob, '/api/workspace/provision', { method: 'POST', body: {} }, 200)
  if (aliceWorkspace.status !== 'ready' || bobWorkspace.status !== 'ready') throw new Error('web app did not provision both workspaces')

  aliceBinding = await integrationJSON('/users/web-alice')
  bobBinding = await integrationJSON('/users/web-bob')
  if (aliceBinding.ownerUserId === bobBinding.ownerUserId) throw new Error('Alice and Bob share the same internal owner')
  const aliceContainer = await waitForExecutor(aliceBinding.ownerUserId)
  const bobContainer = await waitForExecutor(bobBinding.ownerUserId)
  if (aliceContainer === bobContainer) throw new Error('Alice and Bob share one Docker container')
  verifyCredential(aliceContainer, aliceCredential)
  verifyCredential(bobContainer, bobCredential)

  const aliceProject = await appJSON(alice, '/api/projects', {
    method: 'POST', body: { name: 'Alice E2E Project', locale: 'ko', clientRequestId: 'alice-project' },
  }, 201)
  const bobProject = await appJSON(bob, '/api/projects', {
    method: 'POST', body: { name: 'Bob E2E Project', locale: 'ko', clientRequestId: 'bob-project' },
  }, 201)
  if (aliceProject.id === bobProject.id || aliceProject.status !== 'ready' || bobProject.status !== 'ready') {
    throw new Error('web project provisioning did not preserve user isolation')
  }

  const claudeConfig = process.env.PIE_E2E_CLAUDE_CONFIG_DIR?.trim()
  if (claudeConfig) {
    seedClaudeConfig(claudeConfig, aliceBinding.ownerUserId, aliceContainer)
    seedClaudeConfig(claudeConfig, bobBinding.ownerUserId, bobContainer)
  }

  const aliceConversation = await appJSON(alice, '/api/conversations', {
    method: 'POST', body: { projectId: aliceProject.id, clientRequestId: 'alice-conversation' },
  }, 201)
  const bobConversation = await appJSON(bob, '/api/conversations', {
    method: 'POST', body: { projectId: bobProject.id, clientRequestId: 'bob-conversation' },
  }, 201)
  await Promise.all([
    waitConversation(alice, aliceConversation.id),
    waitConversation(bob, bobConversation.id),
  ])

  const crossRead = await rawApp(alice, `/api/conversations/${bobConversation.id}`)
  await crossRead.arrayBuffer()
  if (crossRead.status !== 404) throw new Error(`Alice reading Bob conversation returned HTTP ${crossRead.status}, expected 404`)
  const crossWrite = await rawApp(alice, `/api/conversations/${bobConversation.id}/messages`, {
    method: 'POST', body: { prompt: 'cross-user-attempt', clientRequestId: 'cross-user-attempt' },
  })
  await crossWrite.arrayBuffer()
  if (crossWrite.status !== 404) throw new Error(`Alice writing Bob conversation returned HTTP ${crossWrite.status}, expected 404`)

  const prompt = optional('PIE_E2E_CHAT_PROMPT', 'web-chat-azure')
  const expectedText = optional('PIE_E2E_EXPECTED_TEXT', `pie-e2e:${prompt}`)
  await appJSON(alice, `/api/conversations/${aliceConversation.id}/messages`, {
    method: 'POST', body: { prompt, clientRequestId: 'alice-message' },
  }, 202)
  const aliceEvents = await waitForSSE(alice, aliceConversation.id, expectedText, Number(optional('PIE_E2E_CHAT_TIMEOUT_MS', '120000')))
  if (!aliceEvents.some((event) => event.type === 'done')) throw new Error('Alice SSE stream did not return a terminal done event')

  await appJSON(alice, `/api/conversations/${aliceConversation.id}/messages`, {
    method: 'POST',
    body: {
      prompt: 'image-attachment',
      clientRequestId: 'alice-image-message',
      images: [{ data: testPNG, mimeType: 'image/png', name: 'pixel.png', size: testPNGBytes.length }],
    },
  }, 202)
  await waitForSSE(alice, aliceConversation.id, imageExpectedText, 120_000)

  if (optional('PIE_E2E_SKIP_BOB_CHAT', 'false') !== 'true') {
    await appJSON(bob, `/api/conversations/${bobConversation.id}/messages`, {
      method: 'POST', body: { prompt: 'bob-isolated', clientRequestId: 'bob-message' },
    }, 202)
    await waitForSSE(bob, bobConversation.id, optional('PIE_E2E_BOB_EXPECTED_TEXT', 'pie-e2e:bob-isolated'), 120_000)
  }

  const staticHTML = await (await fetch(`${appURL}/`)).text()
  const me = await appJSON(alice, '/api/auth/me', {}, 200)
  if (staticHTML.includes(serviceToken) || JSON.stringify(me).includes(serviceToken)) throw new Error('Integration service token leaked to the browser')

  const browserVerified = optional('PIE_E2E_BROWSER_SMOKE', 'false') === 'true'
  if (browserVerified) {
    await browserSmoke({
      appURL,
      username: signupUsername,
      displayName: 'Signup E2E User',
      password: signupPassword,
      signup: true,
      prompt: optional('PIE_E2E_BROWSER_PROMPT', 'browser-ui-check'),
      expectedText: optional('PIE_E2E_BROWSER_EXPECTED_TEXT', 'pie-e2e:browser-ui-check'),
      attachmentBase64: testPNG,
      attachmentExpectedText: imageExpectedText,
      profilePath: resolve(dataRoot, 'chrome-profile'),
      timeout: Number(optional('PIE_E2E_BROWSER_TIMEOUT_MS', '180000')),
    })
    const integrationUsers = await managerJSON('/v1/admin/integration-users?limit=50', {}, adminToken, 200)
    const signupBinding = integrationUsers.find((value) => value.integrationId === integrationID && !['web-alice', 'web-bob'].includes(value.externalUserId))
    if (!signupBinding || signupBinding.status !== 'ready') throw new Error('browser signup did not create a ready Integration user assignment')
    const signupContainer = await waitForExecutor(signupBinding.ownerUserId)
    if ([aliceContainer, bobContainer].includes(signupContainer)) throw new Error('browser signup reused another user\'s Docker container')
    verifySignupCredential(signupContainer)
  }

  console.log(JSON.stringify({
    ok: true,
    route: `web-bff -> manager -> ${new URL(relayPublicURL).host} -> docker-clientd -> claude`,
    executorImage,
    verified: [
      'password-login', 'http-only-session', 'csrf', 'server-side-integration-token',
      'two-user-container-isolation', 'credential-isolation', 'conversation-owner-enforcement',
      'kroot-cli-installed', 'kroot-pat-schema', 'container-kroot-project-init', 'project-selection', 'project-working-directory', 'relay-chat-roundtrip', 'image-attachment-relay-roundtrip', 'sse-response',
      ...(browserVerified ? ['browser-signup-provisioning', 'browser-ui-chat', 'browser-file-attachment', 'browser-permission-buttons'] : []),
    ],
  }))
} finally {
  if (serviceToken) {
    if (aliceBinding) await bestEffortDelete('web-alice', aliceBinding.ownerUserId)
    if (bobBinding) await bestEffortDelete('web-bob', bobBinding.ownerUserId)
  }
  if (appProcess && appProcess.exitCode === null) {
    appProcess.kill('SIGTERM')
    await Promise.race([new Promise((resolvePromise) => appProcess.once('exit', resolvePromise)), delay(5000)])
    if (appProcess.exitCode === null) appProcess.kill('SIGKILL')
  }
  try { docker(['rm', '-f', managerContainer]) } catch { /* already removed */ }
  try {
    const leftovers = docker(['ps', '-aq', '--filter', `label=pie.manager_id=${managerID}`]).trim().split(/\s+/).filter(Boolean)
    if (leftovers.length) docker(['rm', '-f', ...leftovers])
  } catch { /* best-effort cleanup */ }
  await removeTreeEventually(dataRoot)
  await removeTreeEventually(resolve(webChatRoot, webChatDistDir))
}

async function login(username, password) {
  const response = await fetch(`${appURL}/api/auth/login`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username, password }),
  })
  const value = await response.json()
  if (response.status !== 200) throw new Error(`login ${username}: HTTP ${response.status}`)
  const cookie = response.headers.get('set-cookie')?.split(';', 1)[0]
  if (!cookie || !value.csrfToken) throw new Error(`login ${username} did not return session state`)
  return { cookie, csrfToken: value.csrfToken }
}

async function appJSON(session, path, options = {}, expected = 200) {
  const response = await rawApp(session, path, options)
  const text = await response.text()
  if (response.status !== expected) throw new Error(`${options.method || 'GET'} ${path}: HTTP ${response.status}, expected ${expected}: ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : null
}

function rawApp(session, path, options = {}) {
  const headers = { ...(options.headers || {}), Cookie: session.cookie, Accept: 'application/json' }
  const request = { ...options, headers, signal: AbortSignal.timeout(30_000) }
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    headers['X-CSRF-Token'] = session.csrfToken
    request.body = JSON.stringify(options.body)
  }
  return fetch(`${appURL}${path}`, request)
}

async function waitConversation(session, id) {
  return poll(async () => {
    const value = await appJSON(session, `/api/conversations/${id}`, {}, 200)
    if (value.status === 'error') throw new Error(`conversation ${id}: ${value.lastError || 'error'}`)
    return value.status === 'ready' ? value : null
  }, 120_000, `ready conversation ${id}`)
}

async function waitForSSE(session, conversationID, expectedText, timeout) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(new Error(`SSE timeout waiting for ${expectedText}`)), timeout)
  const response = await fetch(`${appURL}/api/conversations/${conversationID}/events`, {
    headers: { Cookie: session.cookie }, signal: controller.signal,
  })
  if (!response.ok) throw new Error(`SSE returned HTTP ${response.status}`)
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  const events = []
  let buffer = ''
  let combinedText = ''
  let sawExpected = false
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n')
      for (;;) {
        const boundary = buffer.indexOf('\n\n')
        if (boundary < 0) break
        const block = buffer.slice(0, boundary)
        buffer = buffer.slice(boundary + 2)
        const data = block.split('\n').filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trimStart()).join('\n')
        if (!data) continue
        const event = JSON.parse(data)
        events.push(event)
        if (event.type === 'text') {
          combinedText += event.data?.text || ''
          sawExpected ||= combinedText.includes(expectedText)
        }
        if ((event.type === 'done' || event.type === 'error' || event.type === 'aborted') && sawExpected) {
          return events
        }
        if (event.type === 'error') throw new Error(`Claude executor: ${event.data?.message || 'unknown error'}`)
      }
    }
    throw new Error(`SSE ended before expected text: ${expectedText}`)
  } finally {
    clearTimeout(timer)
    controller.abort()
    reader.cancel().catch(() => {})
  }
}

async function managerJSON(path, options, token, expected) {
  const headers = { ...(options.headers || {}), Authorization: `Bearer ${token}` }
  const request = { ...options, headers, signal: AbortSignal.timeout(30_000) }
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    request.body = JSON.stringify(options.body)
  }
  const response = await fetch(`${managerURL}${path}`, request)
  const text = await response.text()
  if (response.status !== expected) throw new Error(`${path}: HTTP ${response.status}, expected ${expected}: ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : null
}

function integrationJSON(path, options = {}, expected = 200) {
  return managerJSON(`/v1/integrations/${integrationID}${path}`, options, serviceToken, expected)
}

async function bestEffortDelete(externalUserID, ownerUserID) {
  try {
    await integrationJSON(`/users/${externalUserID}`, { method: 'DELETE' }, 202)
    await poll(() => docker(['ps', '-aq', '--filter', `label=pie.user_id=${ownerUserID}`]).trim() === '', 30_000, `delete ${externalUserID}`)
  } catch { /* final container label cleanup below is authoritative */ }
}

async function waitForExecutor(ownerUserID) {
  return poll(async () => {
    const names = docker(['ps', '--filter', `label=pie.user_id=${ownerUserID}`, '--filter', `label=pie.manager_id=${managerID}`, '--format', '{{.Names}}']).trim().split(/\s+/).filter(Boolean)
    return names.length === 1 ? names[0] : null
  }, 90_000, `Executor ${ownerUserID}`)
}

function verifyCredential(container, expectedCredential) {
  const script = `const fs=require('fs'),c=require('crypto'),p=process.env.HOME+'/.kroot/credential.json',b=fs.readFileSync(p),s=fs.statSync(p),v=JSON.parse(b);process.stdout.write(JSON.stringify({digest:c.createHash('sha256').update(b).digest('hex'),mode:s.mode&511,uid:s.uid,gid:s.gid,authKind:v.authKind,serverUrl:v.serverUrl,relayUrl:v.relayUrl,deviceId:v.deviceId,accessTokenDigest:c.createHash('sha256').update(v.accessToken||'').digest('hex')}))`
  const result = JSON.parse(docker(['exec', container, 'node', '-e', script]))
  const expected = createHash('sha256').update(JSON.stringify(expectedCredential)).digest('hex')
  const expectedTokenDigest = createHash('sha256').update(expectedCredential.accessToken).digest('hex')
  if (result.digest !== expected || result.accessTokenDigest !== expectedTokenDigest || result.authKind !== 'pat'
      || result.serverUrl !== expectedCredential.serverUrl || result.relayUrl !== expectedCredential.relayUrl
      || result.deviceId !== expectedCredential.deviceId || result.mode !== 0o600 || result.uid !== 10001 || result.gid !== 10001) {
    throw new Error(`credential boundary mismatch in ${container}`)
  }
  docker(['exec', container, '/usr/local/bin/kroot', '--help'])
}

function verifySignupCredential(container) {
  const script = `const fs=require('fs'),v=JSON.parse(fs.readFileSync('/home/executor/.kroot/credential.json'));process.stdout.write(JSON.stringify({validToken:typeof v.accessToken==='string'&&v.accessToken.startsWith('kpat_demo_'),authKind:v.authKind,serverUrl:v.serverUrl,relayUrl:v.relayUrl,deviceId:v.deviceId}))`
  const value = JSON.parse(docker(['exec', container, 'node', '-e', script]))
  if (!value.validToken || value.authKind !== 'pat' || value.serverUrl !== krootServerURL
      || value.relayUrl !== krootRelayURL || !/^[a-f0-9]{32}$/.test(value.deviceId)) {
    throw new Error(`browser signup credential was not materialized for ${container}`)
  }
  docker(['exec', container, '/usr/local/bin/kroot', '--help'])
}

function seedClaudeConfig(source, ownerUserID, container) {
  const info = statSync(source)
  if (!info.isDirectory()) throw new Error('PIE_E2E_CLAUDE_CONFIG_DIR must be a directory')
  const destination = resolve(dataRoot, 'executor-state', ownerUserID, '.claude')
  rmSync(destination, { recursive: true, force: true })
  mkdirSync(destination, { recursive: true, mode: 0o700 })
  // Do not clone the host's history, sessions, MCP cache or plugin Git
  // repositories into a disposable user container. Only portable auth and
  // minimal settings are relevant to a headless Claude SDK smoke test.
  for (const name of ['.credentials.json', '.claude.json', 'settings.json']) {
    const sourceFile = resolve(source, name)
    if (existsSync(sourceFile) && statSync(sourceFile).isFile()) {
      copyFileSync(sourceFile, resolve(destination, name))
    }
  }
  const keychainService = process.env.PIE_E2E_CLAUDE_KEYCHAIN_SERVICE?.trim()
  if (keychainService) {
    // macOS stores Claude Code OAuth state in Keychain while Linux expects
    // ~/.claude/.credentials.json. Materialize it only inside this disposable
    // E2E state root; never print it or add it to an image layer.
    const credential = execFileSync('security', ['find-generic-password', '-s', keychainService, '-w'], {
      encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    })
    writeFileSync(resolve(destination, '.credentials.json'), credential, { mode: 0o600 })
  }
  docker(['exec', '--user', '0:0', container, 'chown', '-R', '10001:10001', '/home/executor/.claude'])
}

async function browserSmoke({ appURL, username, displayName, password, signup = false, prompt, expectedText, attachmentBase64, attachmentExpectedText, profilePath, timeout }) {
  const chrome = findChrome()
  const port = await availablePort()
  mkdirSync(profilePath, { recursive: true, mode: 0o700 })
  const processHandle = spawn(chrome, [
    '--headless=new',
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profilePath}`,
    '--no-first-run',
    '--disable-default-apps',
    '--disable-extensions',
    '--disable-background-networking',
    'about:blank',
  ], { stdio: ['ignore', 'ignore', 'pipe'] })
  const output = boundedOutput(processHandle)
  let connection
  try {
    await waitFor(`http://127.0.0.1:${port}/json/version`, 20_000, output)
    const targetResponse = await fetch(`http://127.0.0.1:${port}/json/new?${encodeURIComponent(appURL)}`, { method: 'PUT' })
    if (!targetResponse.ok) throw new Error(`Chrome target creation failed: HTTP ${targetResponse.status}`)
    const target = await targetResponse.json()
    connection = await openCDPConnection(target.webSocketDebuggerUrl)
    await connection.send('Runtime.enable')
    await connection.send('Page.enable')
    await waitForExpression(connection, "document.readyState === 'complete' && !!document.getElementById('login-form')", 20_000, 'web chat login page')
    const authFormInteractive = await connection.evaluate(`(() => {
      const input = document.getElementById('username');
      const bounds = input.getBoundingClientRect();
      const target = document.elementFromPoint(bounds.left + bounds.width / 2, bounds.top + bounds.height / 2);
      return getComputedStyle(document.body).pointerEvents !== 'none' && (target === input || input.contains(target));
    })()`)
    if (!authFormInteractive) throw new Error('login form is covered by a non-interactive layer')
    const markdownSample = JSON.stringify('## 제목\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n```js\nconst ok = true\n```\n\n<img src=x onerror=alert(1)>')
    const markdownDOMSafe = await connection.evaluate(`(async () => {
      const { renderMarkdown } = await import('/markdown.js');
      const target = document.createElement('div');
      renderMarkdown(target, ${markdownSample});
      return !!target.querySelector('h2') && !!target.querySelector('table') && !!target.querySelector('pre code') && !target.querySelector('img,script');
    })()`)
    if (!markdownDOMSafe) throw new Error('browser Markdown renderer did not produce safe structured DOM')
    await connection.evaluate(`(() => {
      const set = (id, value) => { const element = document.getElementById(id); element.value = value; element.dispatchEvent(new Event('input', { bubbles: true })); };
      if (${JSON.stringify(signup)}) {
        document.getElementById('show-signup').click();
        set('signup-display-name', ${JSON.stringify(displayName || username)});
        set('signup-username', ${JSON.stringify(username)});
        set('signup-password', ${JSON.stringify(password)});
        set('signup-password-confirm', ${JSON.stringify(password)});
        document.getElementById('signup-form').requestSubmit();
      } else {
        set('username', ${JSON.stringify(username)});
        set('password', ${JSON.stringify(password)});
        document.getElementById('login-form').requestSubmit();
      }
    })()`)
    await waitForExpression(connection, "!document.getElementById('app-view').classList.contains('hidden')", timeout, signup ? 'signed-up chat app' : 'authenticated chat app')
    const needsProject = await connection.evaluate("document.getElementById('project-select').disabled")
    if (needsProject) {
      await connection.evaluate("document.getElementById('create-project-button').click()")
      await waitForExpression(connection, "!!document.getElementById('project-form')", 5_000, 'project dialog')
      const projectDialogInteractive = await connection.evaluate(`(() => {
        const input = document.getElementById('project-name');
        const bounds = input.getBoundingClientRect();
        const target = document.elementFromPoint(bounds.left + bounds.width / 2, bounds.top + bounds.height / 2);
        const dynamicStyle = Array.from(document.querySelectorAll('style'))
          .find((style) => style.textContent?.includes('with-scroll-bars-hidden'));
        return (target === input || input.contains(target)) && !!dynamicStyle?.nonce;
      })()`)
      if (!projectDialogInteractive) throw new Error('project dialog is not interactive or its CSP nonce is missing')
      await connection.evaluate(`(() => {
        const name = document.getElementById('project-name');
        name.value = 'Browser E2E Project';
        name.dispatchEvent(new Event('input', { bubbles: true }));
        document.getElementById('project-form').requestSubmit();
      })()`)
      await waitForExpression(connection, "document.getElementById('project-select').disabled === false", timeout, 'browser Kroot project initialization')
    }
    await waitForExpression(connection, "document.getElementById('prompt').disabled === false", timeout, 'ready Docker conversation')
    await connection.evaluate(`(async () => {
      const binary = atob(${JSON.stringify(attachmentBase64)});
      const bytes = Uint8Array.from(binary, (value) => value.charCodeAt(0));
      const file = new File([bytes], 'pixel.png', { type: 'image/png' });
      const transfer = new DataTransfer();
      transfer.items.add(file);
      const input = document.getElementById('attachment-input');
      input.files = transfer.files;
      input.dispatchEvent(new Event('change', { bubbles: true }));
    })()`)
    await waitForExpression(connection, "document.querySelector('#attachment-tray:not(.hidden) .attachment-item img')?.naturalWidth > 0", timeout, 'decoded attachment preview tray')
    await connection.evaluate(`(() => {
      const prompt = document.getElementById('prompt');
      prompt.value = 'image-attachment';
      prompt.dispatchEvent(new Event('input', { bubbles: true }));
      document.getElementById('composer').requestSubmit();
    })()`)
    const encodedAttachmentExpected = JSON.stringify(attachmentExpectedText)
    await waitForExpression(connection, `Array.from(document.querySelectorAll('.message.assistant .bubble')).some((item) => item.textContent.includes(${encodedAttachmentExpected}))`, timeout, 'browser image attachment response')
    const attachmentRendered = await connection.evaluate("document.querySelector('.message.user .message-attachments img')?.naturalWidth > 0 && document.getElementById('attachment-tray').classList.contains('hidden')")
    if (!attachmentRendered) throw new Error('browser did not preserve the sent image preview or clear the attachment tray')
    await waitForExpression(connection, "document.getElementById('prompt').disabled === false", timeout, 'composer unlocked after image turn')
    await connection.evaluate(`(() => {
      const prompt = document.getElementById('prompt');
      prompt.value = 'request-permission';
      prompt.dispatchEvent(new Event('input', { bubbles: true }));
      document.getElementById('composer').requestSubmit();
    })()`)
    await waitForExpression(connection, "!!document.querySelector('.permission-message:not(.resolved) .permission-allow')", timeout, 'inline permission approval button')
    const permissionUIReady = await connection.evaluate("document.getElementById('prompt').disabled === true && !!document.querySelector('.permission-message .permission-deny')")
    if (!permissionUIReady) throw new Error('permission request did not lock chat input or expose both decisions')
    await connection.evaluate("document.querySelector('.permission-message:not(.resolved) .permission-allow').click()")
    await waitForExpression(connection, "!!document.querySelector('.permission-message.approved')", timeout, 'approved permission state')
    await waitForExpression(connection, "Array.from(document.querySelectorAll('.message.assistant .bubble')).some((item) => item.textContent.includes('pie-e2e:permission-allowed'))", timeout, 'permission continuation response')
    await waitForExpression(connection, "document.getElementById('prompt').disabled === false", timeout, 'composer unlocked after permission turn')
    await connection.evaluate(`(() => {
      const prompt = document.getElementById('prompt');
      prompt.value = ${JSON.stringify(prompt)};
      prompt.dispatchEvent(new Event('input', { bubbles: true }));
      document.getElementById('composer').requestSubmit();
    })()`)
    const encodedExpected = JSON.stringify(expectedText)
    await waitForExpression(connection, `Array.from(document.querySelectorAll('.message.assistant .bubble')).some((item) => item.textContent.includes(${encodedExpected}))`, timeout, `browser response ${expectedText}`)
    await waitForExpression(connection, "!Array.from(document.querySelectorAll('.message.assistant')).some((item) => item.classList.contains('pending'))", timeout, 'browser terminal event')
    const status = await connection.evaluate("document.getElementById('relay-status').textContent")
    if (!String(status).includes('Relay')) throw new Error(`browser did not report a Relay connection: ${status}`)
  } finally {
    if (connection) {
      try { await connection.send('Browser.close') } catch { /* process fallback below */ }
      connection.close()
    }
    if (processHandle.exitCode === null) {
      processHandle.kill('SIGTERM')
      await Promise.race([new Promise((resolvePromise) => processHandle.once('exit', resolvePromise)), delay(3000)])
      if (processHandle.exitCode === null) processHandle.kill('SIGKILL')
    }
  }
}

async function waitForExpression(connection, expression, timeout, label) {
  return poll(async () => Boolean(await connection.evaluate(expression)), timeout, label)
}

async function openCDPConnection(url) {
  const socket = new WebSocket(url)
  await new Promise((resolvePromise, reject) => {
    socket.addEventListener('open', resolvePromise, { once: true })
    socket.addEventListener('error', reject, { once: true })
  })
  let nextID = 1
  const pending = new Map()
  socket.addEventListener('message', (message) => {
    const value = JSON.parse(message.data)
    if (!value.id) return
    const request = pending.get(value.id)
    if (!request) return
    pending.delete(value.id)
    if (value.error) request.reject(new Error(value.error.message))
    else request.resolve(value.result)
  })
  socket.addEventListener('close', () => {
    for (const request of pending.values()) request.reject(new Error('Chrome DevTools connection closed'))
    pending.clear()
  })
  const send = (method, params = {}) => {
    const id = nextID++
    return new Promise((resolvePromise, reject) => {
      pending.set(id, { resolve: resolvePromise, reject })
      socket.send(JSON.stringify({ id, method, params }))
    })
  }
  return {
    send,
    async evaluate(expression) {
      const result = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
      if (result.exceptionDetails) throw new Error(result.exceptionDetails.text || 'browser evaluation failed')
      return result.result?.value
    },
    close() { socket.close() },
  }
}

function findChrome() {
  const configured = process.env.PIE_E2E_CHROME_PATH?.trim()
  const candidates = [
    configured,
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    '/Applications/Chromium.app/Contents/MacOS/Chromium',
  ].filter(Boolean)
  for (const candidate of candidates) {
    if (existsSync(candidate)) return candidate
  }
  throw new Error('Chrome was not found; set PIE_E2E_CHROME_PATH')
}

async function waitFor(url, timeout, detail = () => '') {
  return poll(async () => {
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(2000) })
      await response.arrayBuffer()
      return response.ok
    } catch { return false }
  }, timeout, url, detail)
}

async function poll(check, timeout, label, detail = () => '') {
  const deadline = Date.now() + timeout
  let lastError
  while (Date.now() < deadline) {
    try {
      const value = await check()
      if (value) return value
    } catch (error) { lastError = error }
    await delay(250)
  }
  throw new Error(`timed out waiting for ${label}${lastError ? ` (${lastError.message})` : ''}${detail() ? `\n${detail()}` : ''}`)
}

function boundedOutput(child) {
  let output = ''
  const collect = (chunk) => { output = (output + chunk.toString('utf8')).slice(-8000) }
  child.stdout?.on('data', collect)
  child.stderr?.on('data', collect)
  return () => output
}

function assertDockerImage(name) { docker(['image', 'inspect', name]) }
function assertDockerNetwork(name) { docker(['network', 'inspect', name]) }
function docker(args) { return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }) }

function availablePort() {
  return new Promise((resolvePromise, reject) => {
    const server = createServer()
    server.unref()
    server.on('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      server.close((error) => error ? reject(error) : resolvePromise(address.port))
    })
  })
}

function required(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function relayHTTPOrigin(raw) {
  const value = new URL(raw)
  if (value.protocol === 'ws:') value.protocol = 'http:'
  else if (value.protocol === 'wss:') value.protocol = 'https:'
  if (value.protocol !== 'http:' && value.protocol !== 'https:') throw new Error(`invalid Relay URL: ${raw}`)
  return value.origin
}

function optional(name, fallback) { return process.env[name]?.trim() || fallback }
function delay(ms) { return new Promise((resolvePromise) => setTimeout(resolvePromise, ms)) }

async function removeTreeEventually(path) {
  let lastError
  for (let attempt = 0; attempt < 20; attempt++) {
    try {
      rmSync(path, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 })
      return
    } catch (error) {
      lastError = error
      await delay(250)
    }
  }
  throw lastError
}
