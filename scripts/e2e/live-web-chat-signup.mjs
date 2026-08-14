#!/usr/bin/env node

import { randomBytes } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const repoRoot = resolve(import.meta.dirname, '../..')
const stateRoot = resolve(process.env.PIE_LIVE_DEMO_STATE_DIR?.trim() || resolve(repoRoot, '.local/pie-web-chat-demo'))
const appURL = (process.env.PIE_LIVE_DEMO_URL?.trim() || 'http://127.0.0.1:4175').replace(/\/$/, '')
const managerURL = (process.env.PIE_LIVE_DEMO_MANAGER_URL?.trim() || 'http://127.0.0.1:19190').replace(/\/$/, '')
const integrationID = process.env.PIE_LIVE_DEMO_INTEGRATION_ID?.trim() || 'sample-web-chat-demo'
const username = process.env.PIE_LIVE_SIGNUP_USERNAME?.trim() || `live-${Date.now().toString(36)}`
const password = process.env.PIE_LIVE_SIGNUP_PASSWORD?.trim() || `PieSignup-${randomBytes(10).toString('base64url')}`
const displayName = process.env.PIE_LIVE_SIGNUP_DISPLAY_NAME?.trim() || 'Live Signup User'
const expected = `PIE-SIGNUP-E2E-${randomBytes(6).toString('hex')}`
const imageExpected = `PIE-IMAGE-E2E-${randomBytes(6).toString('hex')}`
const imagePath = resolve(process.env.PIE_LIVE_IMAGE_PATH?.trim() || resolve(repoRoot, 'desktop/src-tauri/icons/128x128.png'))
const imageData = readFileSync(imagePath)

const health = await fetch(`${appURL}/api/health`, { signal: AbortSignal.timeout(5000) })
if (!health.ok) throw new Error(`web chat is not ready: HTTP ${health.status}`)

const signup = await fetch(`${appURL}/api/auth/signup`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
  body: JSON.stringify({ username, displayName, password }),
  signal: AbortSignal.timeout(120_000),
})
const signupText = await signup.text()
if (signup.status !== 201) throw new Error(`signup failed: HTTP ${signup.status}: ${signupText.slice(0, 300)}`)
const session = JSON.parse(signupText)
if (session.workspace?.status !== 'ready') throw new Error(`signup workspace is not ready: ${session.workspace?.status || 'missing'}`)
const cookie = signup.headers.get('set-cookie')?.split(';', 1)[0]
if (!cookie || !session.csrfToken) throw new Error('signup did not establish a browser session')

const users = JSON.parse(readFileSync(resolve(stateRoot, 'users.json'), 'utf8'))
const registeredUser = users.find((value) => value.username === username)
if (!registeredUser?.externalUserId) throw new Error('registered user was not persisted by the external web service')
const adminToken = readFileSync(resolve(stateRoot, 'manager-admin-token'), 'utf8').trim()
const integrationUsers = await managerJSON('/v1/admin/integration-users?limit=500', adminToken)
const binding = integrationUsers.find((value) => value.integrationId === integrationID && value.externalUserId === registeredUser.externalUserId)
if (!binding || binding.status !== 'ready') throw new Error('Manager did not create a ready user-to-Executor assignment')

const container = await poll(() => {
  const names = docker(['ps', '--filter', `label=pie.user_id=${binding.ownerUserId}`, '--filter', 'label=pie.manager_id=web-chat-demo', '--format', '{{.Names}}'])
    .trim().split(/\s+/).filter(Boolean)
  return names.length === 1 ? names[0] : null
}, 90_000, 'new user Executor container')
const status = docker(['inspect', '--format', '{{.State.Status}}|{{index .Config.Labels "pie.user_id"}}|{{index .Config.Labels "pie.manager_id"}}', container]).trim()
if (status !== `running|${binding.ownerUserId}|web-chat-demo`) throw new Error(`unexpected Executor ownership: ${status}`)
for (const path of ['/home/executor/.kroot/credential.json', '/home/executor/.claude/.credentials.json', '/home/executor/.pie-state-seed-v1']) {
  const modeOwner = docker(['exec', container, 'stat', '-c', '%a:%u:%g', path]).trim()
  if (modeOwner !== '600:10001:10001') throw new Error(`invalid private credential boundary for ${path}: ${modeOwner}`)
}
const krootCredential = JSON.parse(docker(['exec', container, 'node', '-e', `const fs=require('fs'),v=JSON.parse(fs.readFileSync('/home/executor/.kroot/credential.json'));process.stdout.write(JSON.stringify({validToken:typeof v.accessToken==='string'&&v.accessToken.startsWith('kpat_'),authKind:v.authKind,serverUrl:v.serverUrl,relayUrl:v.relayUrl,deviceId:v.deviceId}))`]))
if (!krootCredential.validToken || krootCredential.authKind !== 'pat' || !krootCredential.serverUrl || !krootCredential.relayUrl || !/^[a-f0-9]{32}$/.test(krootCredential.deviceId)) {
  throw new Error('new user Kroot credential schema is invalid')
}
docker(['exec', container, '/usr/local/bin/kroot', '--help'])

const project = await appJSON('/api/projects', {
  method: 'POST',
  body: { name: 'Live Signup Project', locale: 'ko', clientRequestId: `signup-project-${randomBytes(8).toString('hex')}` },
}, 201)
if (project.status !== 'ready') throw new Error(`signup Kroot project is not ready: ${project.status}`)
docker(['exec', container, 'test', '-f', `/workspace/projects/${project.id}/.pie-kroot-initialized`])

const conversation = await appJSON('/api/conversations', {
  method: 'POST', body: { projectId: project.id, clientRequestId: `signup-conversation-${randomBytes(8).toString('hex')}` },
}, 201)
await poll(async () => {
  const value = await appJSON(`/api/conversations/${encodeURIComponent(conversation.id)}`, {}, 200)
  if (value.status === 'error') throw new Error(value.lastError || 'conversation failed')
  return value.status === 'ready' ? value : null
}, 120_000, 'CookAI Relay conversation')
await appJSON(`/api/conversations/${encodeURIComponent(conversation.id)}/messages`, {
  method: 'POST',
  body: { prompt: `다른 설명 없이 다음 문자열만 정확히 답하세요: ${expected}`, clientRequestId: `signup-message-${randomBytes(8).toString('hex')}` },
}, 202)
const events = await terminalEvents(conversation.id, 180_000)
const responseText = events.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join('')
if (!responseText.includes(expected)) throw new Error(`Claude response mismatch: ${responseText.slice(0, 300)}`)

await appJSON(`/api/conversations/${encodeURIComponent(conversation.id)}/messages`, {
  method: 'POST',
  body: {
    prompt: `첨부 이미지가 실제로 보이면 다른 설명 없이 다음 문자열만 정확히 답하세요: ${imageExpected}`,
    images: [{ data: imageData.toString('base64'), mimeType: 'image/png', name: 'pie-relay-e2e.png', size: imageData.length }],
    clientRequestId: `signup-image-${randomBytes(8).toString('hex')}`,
  },
}, 202)
const imageEvents = await terminalEvents(conversation.id, 180_000, imageExpected)
const imageResponseText = imageEvents.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join('')
if (!imageResponseText.includes(imageExpected)) throw new Error(`Claude image response mismatch: ${imageResponseText.slice(0, 300)}`)

// Recreate through the Manager operation API, not by manipulating Docker
// directly. The container layer must change while the per-user credential and
// Claude auth volume remain byte-identical.
const runtimes = await managerJSON('/v1/admin/runtimes?limit=500', adminToken)
const runtime = runtimes.find((value) => value.ownerUserId === binding.ownerUserId)
if (!runtime?.id) throw new Error('Manager runtime for the new user was not found')
const beforeRecreate = executorSnapshot(container)
const operation = await managerRequestJSON('/v1/admin/operations', adminToken, {
  method: 'POST',
  headers: { 'Idempotency-Key': `live-recreate-${randomBytes(12).toString('hex')}` },
  body: { type: 'runtime.recreate', targetKind: 'runtimes', targetId: runtime.id, request: {} },
}, 202)
await poll(async () => {
  const operations = await managerJSON('/v1/admin/operations?limit=500', adminToken)
  const current = operations.find((value) => value.id === operation.id)
  if (current?.status === 'failed') throw new Error(`runtime recreation failed: ${current.error || 'unknown error'}`)
  return current?.status === 'succeeded' ? current : null
}, 120_000, 'Manager runtime recreation')
const afterRecreate = await poll(() => {
  try {
    const value = executorSnapshot(container)
    return value.containerId !== beforeRecreate.containerId && value.health === 'healthy' ? value : null
  } catch { return null }
}, 120_000, 'healthy recreated Executor container')
if (afterRecreate.krootCredentialDigest !== beforeRecreate.krootCredentialDigest
    || afterRecreate.claudeCredentialDigest !== beforeRecreate.claudeCredentialDigest
    || afterRecreate.seedMarkerDigest !== beforeRecreate.seedMarkerDigest) {
  throw new Error('private per-user state changed during runtime recreation')
}
docker(['exec', container, '/usr/local/bin/kroot', 'auth', 'status'])

const postRecreateExpected = `PIE-RECREATE-E2E-${randomBytes(6).toString('hex')}`
const postRecreateConversation = await appJSON('/api/conversations', {
  method: 'POST', body: { projectId: project.id, clientRequestId: `recreate-conversation-${randomBytes(8).toString('hex')}` },
}, 201)
await poll(async () => {
  const value = await appJSON(`/api/conversations/${encodeURIComponent(postRecreateConversation.id)}`, {}, 200)
  if (value.status === 'error') throw new Error(value.lastError || 'post-recreate conversation failed')
  return value.status === 'ready' ? value : null
}, 120_000, 'post-recreate CookAI Relay conversation')
await appJSON(`/api/conversations/${encodeURIComponent(postRecreateConversation.id)}/messages`, {
  method: 'POST',
  body: {
    prompt: `다른 설명 없이 다음 문자열만 정확히 답하세요: ${postRecreateExpected}`,
    clientRequestId: `recreate-message-${randomBytes(8).toString('hex')}`,
  },
}, 202)
const postRecreateEvents = await terminalEvents(postRecreateConversation.id, 180_000)
const postRecreateText = postRecreateEvents.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join('')
if (!postRecreateText.includes(postRecreateExpected)) throw new Error(`post-recreate Claude response mismatch: ${postRecreateText.slice(0, 300)}`)

console.log(JSON.stringify({
  ok: true,
  username,
  password,
  workspace: session.workspace.status,
  assignment: binding.id,
  ownerUserId: binding.ownerUserId,
  container,
  relay: 'CookAI Pie Relay',
  response: expected,
  imageResponse: imageExpected,
  postRecreateResponse: postRecreateExpected,
  recreatedContainer: beforeRecreate.containerId !== afterRecreate.containerId,
  verified: ['web-signup', 'persistent-external-user', 'manager-assignment', 'new-docker-container', 'kroot-cli', 'kroot-pat-credential', 'container-kroot-project-init', 'project-working-directory', 'claude-auth-seed', 'pie-relay-conversation', 'real-claude-response', 'real-claude-image-attachment', 'manager-runtime-recreate', 'private-volume-persistence', 'post-recreate-relay-chat'],
}))

async function appJSON(path, { method = 'GET', body } = {}, expectedStatus) {
  const headers = { Accept: 'application/json', Cookie: cookie }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (method !== 'GET' && method !== 'HEAD') headers['X-CSRF-Token'] = session.csrfToken
  const response = await fetch(`${appURL}${path}`, {
    method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(120_000),
  })
  const text = await response.text()
  if (response.status !== expectedStatus) throw new Error(`${path}: HTTP ${response.status}, expected ${expectedStatus}: ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : null
}

async function managerJSON(path, token) {
  return managerRequestJSON(path, token)
}

async function managerRequestJSON(path, token, { method = 'GET', body, headers = {} } = {}, expectedStatus = 200) {
  const requestHeaders = { ...headers, Authorization: `Bearer ${token}`, Accept: 'application/json' }
  if (body !== undefined) requestHeaders['Content-Type'] = 'application/json'
  const response = await fetch(`${managerURL}${path}`, {
    method,
    headers: requestHeaders,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  })
  const text = await response.text()
  if (response.status !== expectedStatus) throw new Error(`Manager ${path}: HTTP ${response.status}, expected ${expectedStatus}: ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : null
}

function executorSnapshot(containerName) {
  const inspected = docker(['inspect', '--format', '{{.Id}}|{{.State.Running}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}', containerName]).trim().split('|')
  if (inspected[1] !== 'true') throw new Error('Executor is not running')
  const script = `const fs=require('fs'),c=require('crypto');const d=p=>c.createHash('sha256').update(fs.readFileSync(p)).digest('hex');process.stdout.write(JSON.stringify({krootCredentialDigest:d('/home/executor/.kroot/credential.json'),claudeCredentialDigest:d('/home/executor/.claude/.credentials.json'),seedMarkerDigest:d('/home/executor/.pie-state-seed-v1')}))`
  return { containerId: inspected[0], health: inspected[2], ...JSON.parse(docker(['exec', containerName, 'node', '-e', script])) }
}

async function terminalEvents(conversationID, timeout, expectedText = '') {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(new Error('timed out waiting for Claude response')), timeout)
  try {
    const response = await fetch(`${appURL}/api/conversations/${encodeURIComponent(conversationID)}/events`, {
      headers: { Accept: 'text/event-stream', Cookie: cookie }, signal: controller.signal,
    })
    if (!response.ok) throw new Error(`event stream failed: HTTP ${response.status}`)
    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    const events = []
    let buffered = ''
    let combinedText = ''
    for (;;) {
      const { value, done } = await reader.read()
      buffered += decoder.decode(value || new Uint8Array(), { stream: !done })
      const blocks = buffered.split(/\r?\n\r?\n/)
      buffered = blocks.pop() || ''
      for (const block of blocks) {
        const data = block.split(/\r?\n/).filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trimStart()).join('\n')
        if (!data) continue
        const event = JSON.parse(data)
        events.push(event)
        if (event.type === 'text') combinedText += event.data?.text || ''
        if (event.type === 'error') throw new Error(event.data?.message || 'Claude execution failed')
        if (['done', 'aborted'].includes(event.type) && (!expectedText || combinedText.includes(expectedText))) {
          await reader.cancel()
          return events
        }
      }
      if (done) throw new Error('event stream ended before a terminal event')
    }
  } finally {
    clearTimeout(timer)
  }
}

async function poll(check, timeout, label) {
  const deadline = Date.now() + timeout
  let lastError
  while (Date.now() < deadline) {
    try {
      const value = await check()
      if (value) return value
    } catch (error) { lastError = error }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 500))
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`)
}

function docker(args) {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
}
