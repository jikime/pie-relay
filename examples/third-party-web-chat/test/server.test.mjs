import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { after, before, test } from 'node:test'

import { hashPassword, verifyPassword } from '../src/auth.mjs'
import { createAPIHandler } from '../src/api-handler.mjs'
import { PieManagerClient } from '../src/pie-manager-client.mjs'
import { MemoryUserStore } from '../src/user-store.mjs'

const integrationToken = 'pie_int_test_token_that_must_never_reach_the_browser'
const png = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII='
const managerState = {
  bindings: new Map(),
  projects: new Map(),
  projectApps: new Map(),
  previews: new Map(),
  conversations: new Map([
    ['conv-alice-active', conversation('conv-alice-active', 'iu-alice', 'project-alice')],
    ['conv-bob', conversation('conv-bob', 'iu-bob')],
  ]),
  provisions: [],
  messages: [],
  permissions: [],
  retries: [],
  eventLastIDs: [],
  requestIDs: [],
  usageRequests: [],
  usageEventRequests: [],
  workspaceFiles: new Map([['src/index.ts', { content: 'export const answer = 42\n', revision: 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' }]]),
  streamGate: null,
  failNextProvision: false,
}
let manager
let app
let managerOrigin
let appOrigin
let aliceHash
let bobHash
let userStore

before(async () => {
  ;[aliceHash, bobHash] = await Promise.all([hashPassword('alice-password'), hashPassword('bob-password')])
  manager = createServer(fakeManager)
  managerOrigin = await listen(manager)
  userStore = new MemoryUserStore([
    user('alice', 'Alice', 'external-alice', aliceHash, { pat: 'alice-private-pat' }),
    user('bob', 'Bob', 'external-bob', bobHash, { pat: 'bob-private-pat' }),
  ])
  const config = {
    integrationID: 'sample-chat',
    integrationToken,
    krootServerURL: 'grpcs://api.kroot.test',
    krootRelayURL: 'wss://relay.kroot.test/ws/agent',
    signupKrootPAT: 'kpat_local_signup_test',
    managerURL: managerOrigin,
    userStore,
    userStoreKind: 'memory',
    fallbackPasswordHash: aliceHash,
    registrationEnabled: true,
    secureCookie: false,
    publicOrigin: '',
    sessionTTLms: 60_000,
  }
  app = createTestAppServer(createAPIHandler({ config, pieClient: new PieManagerClient(config) }))
  appOrigin = await listen(app)
})

after(async () => {
  await Promise.all([close(app), close(manager)])
})

test('password hashes are salted and verified in constant-length form', async () => {
  const other = await hashPassword('alice-password')
  assert.notEqual(other, aliceHash)
  assert.equal(await verifyPassword('alice-password', aliceHash), true)
  assert.equal(await verifyPassword('wrong-password', aliceHash), false)
})

test('login uses an HttpOnly session and enforces CSRF on mutations', async () => {
  const failed = await json(`${appOrigin}/api/auth/login`, {
    method: 'POST', body: { username: 'alice', password: 'wrong-password' },
  })
  assert.equal(failed.response.status, 401)
  const login = await loginAs('alice', 'alice-password')
  assert.match(login.cookie, /^pie_demo_session=/)
  assert.match(login.setCookie, /HttpOnly/)
  assert.match(login.setCookie, /SameSite=Strict/)
  assert.equal(login.value.user.displayName, 'Alice')
  assert.equal(typeof login.value.csrfToken, 'string')

  const sameOrigin = await json(`${appOrigin}/api/auth/login`, {
    method: 'POST', origin: appOrigin, body: { username: 'alice', password: 'alice-password' },
  })
  assert.equal(sameOrigin.response.status, 200)
  const crossOrigin = await json(`${appOrigin}/api/auth/login`, {
    method: 'POST', origin: 'https://attacker.invalid', body: { username: 'alice', password: 'alice-password' },
  })
  assert.equal(crossOrigin.response.status, 403)

  const rejected = await json(`${appOrigin}/api/workspace/provision`, {
    method: 'POST', cookie: login.cookie, body: {}, csrfToken: 'wrong',
  })
  assert.equal(rejected.response.status, 403)

  const provisioned = await json(`${appOrigin}/api/workspace/provision`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    requestID: 'browser-request-alice-123',
    body: { externalUserId: 'external-bob', credential: { pat: 'attacker-value' } },
  })
  assert.equal(provisioned.response.status, 200)
  assert.equal(provisioned.value.status, 'ready')
  assert.equal(provisioned.value.credentialConfigured, true)
  assert.deepEqual(managerState.provisions.at(-1), {
    externalUserID: 'external-alice', credential: { pat: 'alice-private-pat' },
  })
  assert.equal(JSON.stringify(provisioned.value).includes('alice-private-pat'), false)
  assert.equal(provisioned.response.headers.get('x-request-id'), 'browser-request-alice-123')
  assert.equal(managerState.requestIDs.at(-1), 'browser-request-alice-123')
})

test('signup creates an external user, provisions its isolated workspace, and starts a session', async () => {
  const signup = await json(`${appOrigin}/api/auth/signup`, {
    method: 'POST', body: { username: 'carol', displayName: 'Carol', password: 'carol-password-2026' },
  })
  assert.equal(signup.response.status, 201)
  assert.equal(signup.value.user.username, 'carol')
  assert.equal(signup.value.workspace.status, 'ready')
  assert.match(signup.setCookie ?? signup.response.headers.get('set-cookie'), /HttpOnly/)
  const provision = managerState.provisions.at(-1)
  assert.match(provision.externalUserID, /^signup-/)
  assert.equal(provision.credential.accessToken, 'kpat_local_signup_test')
  assert.equal(provision.credential.authKind, 'pat')
  assert.equal(provision.credential.serverUrl, 'grpcs://api.kroot.test')
  assert.equal(provision.credential.relayUrl, 'wss://relay.kroot.test/ws/agent')
  assert.match(provision.credential.deviceId, /^[a-f0-9]{32}$/)

  const duplicate = await json(`${appOrigin}/api/auth/signup`, {
    method: 'POST', body: { username: 'carol', displayName: 'Other Carol', password: 'different-password-2026' },
  })
  assert.equal(duplicate.response.status, 409)
})

test('a persisted account can retry provisioning after a transient Manager failure', async () => {
  managerState.failNextProvision = true
  const signup = await json(`${appOrigin}/api/auth/signup`, {
    method: 'POST', body: { username: 'dave', displayName: 'Dave', password: 'dave-password-2026' },
  })
  assert.equal(signup.response.status, 503)
  assert.equal((await userStore.get('dave')).provisioningStatus, 'failed')

  const login = await loginAs('dave', 'dave-password-2026')
  const recovered = await json(`${appOrigin}/api/workspace/provision`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken, body: {},
  })
  assert.equal(recovered.response.status, 200)
  assert.equal(recovered.value.status, 'ready')
  assert.equal((await userStore.get('dave')).provisioningStatus, 'ready')
})

test('the BFF never exposes its service token and blocks cross-user conversation IDs', async () => {
  const login = await loginAs('alice', 'alice-password')
  const page = await fetch(`${appOrigin}/`)
  const html = await page.text()
  const me = await json(`${appOrigin}/api/auth/me`, { cookie: login.cookie })
  assert.equal(html.includes(integrationToken), false)
  assert.equal(JSON.stringify(me.value).includes(integrationToken), false)

  const listed = await json(`${appOrigin}/api/conversations`, { cookie: login.cookie })
  assert.equal(listed.response.status, 200)
  assert.deepEqual(listed.value.map((value) => value.id), ['conv-alice-active'])
  assert.deepEqual(listed.value[0].connection, {
    relayAvailable: true,
    runtimeRunning: true,
    runtimeHealthy: true,
    clientConnected: true,
    relayRegistered: true,
    sessionStatus: 'active',
    reason: 'connected',
    lastError: '',
    lastHeartbeat: listed.value[0].connection.lastHeartbeat,
  })
  assert.equal('relayNodeId' in listed.value[0].connection, false)
  assert.equal(JSON.stringify(listed.value[0]).includes('must-not-leak'), false)

  const beforeMessages = managerState.messages.length
  const crossRead = await json(`${appOrigin}/api/conversations/conv-bob`, { cookie: login.cookie })
  assert.equal(crossRead.response.status, 404)
  const crossWrite = await json(`${appOrigin}/api/conversations/conv-bob/messages`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { prompt: 'read Bob data', clientRequestId: 'cross-user-test' },
  })
  assert.equal(crossWrite.response.status, 404)
  assert.equal(managerState.messages.length, beforeMessages)
})

test('usage summary is scoped to the authenticated external user and sanitized', async () => {
  const login = await loginAs('alice', 'alice-password')
  const result = await json(`${appOrigin}/api/usage?days=30`, { cookie: login.cookie })
  assert.equal(result.response.status, 200)
  assert.deepEqual(managerState.usageRequests.at(-1), { externalUserID: 'external-alice', days: '30' })
  assert.equal(result.value.totals.totalTokens, 180)
  assert.equal(result.value.byModel[0].canonicalModel, 'claude-test-v1')
  assert.equal('integrationUserId' in result.value, false)
  assert.equal(JSON.stringify(result.value).includes('must-not-leak'), false)

  const events = await json(`${appOrigin}/api/usage/events?days=30&cursor=next_cursor_1`, { cookie: login.cookie })
  assert.equal(events.response.status, 200)
  assert.deepEqual(managerState.usageEventRequests.at(-1), { externalUserID: 'external-alice', days: '30', cursor: 'next_cursor_1', limit: '30' })
  assert.equal(events.value.items[0].projectName, 'Alice Project')
  assert.equal(events.value.items[0].totalTokens, 180)
  assert.equal(events.value.nextCursor, 'next_cursor_2')
  assert.equal('ownerUserId' in events.value.items[0], false)
  assert.equal(JSON.stringify(events.value).includes('must-not-leak'), false)

  const rejected = await json(`${appOrigin}/api/usage?days=365`, { cookie: login.cookie })
  assert.equal(rejected.response.status, 400)
  const badCursor = await json(`${appOrigin}/api/usage/events?days=30&cursor=%25bad`, { cookie: login.cookie })
  assert.equal(badCursor.response.status, 400)
})

test('an authenticated user can provision, create a conversation, send, and stream events', async () => {
  const login = await loginAs('alice', 'alice-password')
  await json(`${appOrigin}/api/workspace/provision`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken, body: {},
  })
  const project = await json(`${appOrigin}/api/projects`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { name: 'Alice Project', locale: 'ko', clientRequestId: 'project-alice' },
  })
  assert.equal(project.response.status, 201)
  assert.equal(project.value.name, 'Alice Project')
  assert.equal(project.value.status, 'ready')
  assert.equal('workingDir' in project.value, false)
  const created = await json(`${appOrigin}/api/conversations`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { projectId: project.value.id, clientRequestId: 'conversation-alice' },
  })
  assert.equal(created.response.status, 201)
  assert.equal(created.value.status, 'ready')
  assert.equal(created.value.projectId, project.value.id)
  assert.equal('ownerUserId' in created.value, false)
  const conversationID = created.value.id

  const sent = await json(`${appOrigin}/api/conversations/${conversationID}/messages`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { prompt: 'hello from browser', clientRequestId: 'message-alice' },
  })
  assert.equal(sent.response.status, 202)
  assert.deepEqual(managerState.messages.at(-1), { conversationID, prompt: 'hello from browser', images: [] })

  const imageSent = await json(`${appOrigin}/api/conversations/${conversationID}/messages`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: {
      prompt: 'analyze image', clientRequestId: 'message-image-alice',
      images: [{ data: png, mimeType: 'image/png', name: 'pixel.png', size: 68 }],
    },
  })
  assert.equal(imageSent.response.status, 202)
  assert.deepEqual(managerState.messages.at(-1), {
    conversationID,
    prompt: 'analyze image',
    images: [{ data: png, mimeType: 'image/png', name: 'pixel.png', size: 68 }],
  })

  const beforeRejectedImage = managerState.messages.length
  const spoofed = await json(`${appOrigin}/api/conversations/${conversationID}/messages`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: {
      prompt: 'spoofed image', clientRequestId: 'message-spoofed-alice',
      images: [{ data: Buffer.from('not a png').toString('base64'), mimeType: 'image/png', name: 'fake.png' }],
    },
  })
  assert.equal(spoofed.response.status, 400)
  assert.equal(managerState.messages.length, beforeRejectedImage)

  const eventsResponse = await fetch(`${appOrigin}/api/conversations/${conversationID}/events`, {
    headers: { Cookie: login.cookie },
  })
  const events = await eventsResponse.text()
  assert.equal(eventsResponse.status, 200)
  assert.match(eventsResponse.headers.get('content-type'), /^text\/event-stream/)
  assert.match(events, /"type":"text"/)
  assert.match(events, /hello-from-fake-manager/)

  const permission = await json(`${appOrigin}/api/conversations/${conversationID}/permissions/permission-1`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { allow: true, clientRequestId: 'permission-answer-alice' },
  })
  assert.equal(permission.response.status, 202)
  assert.deepEqual(managerState.permissions.at(-1), {
    conversationID, requestID: 'permission-1', allow: true,
  })

  const retried = await json(`${appOrigin}/api/conversations/${conversationID}/retry`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { clientRequestId: 'retry-conversation-alice' },
  })
  assert.equal(retried.response.status, 202)
  assert.equal(retried.value.id, conversationID)
  assert.equal(retried.value.status, 'ready')
  assert.deepEqual(managerState.retries.at(-1), { conversationID, idempotencyKey: 'retry-conversation-alice' })

  const tree = await json(`${appOrigin}/api/projects/${project.value.id}/workspace/tree?conversationId=${conversationID}&path=`, { cookie: login.cookie })
  assert.equal(tree.response.status, 200)
  assert.deepEqual(tree.value.entries.map((entry) => entry.path), ['src'])
  assert.equal('workingDir' in tree.value, false)

  const nested = await json(`${appOrigin}/api/projects/${project.value.id}/workspace/tree?conversationId=${conversationID}&path=src`, { cookie: login.cookie })
  assert.equal(nested.response.status, 200)
  assert.deepEqual(nested.value.entries.map((entry) => entry.path), ['src/index.ts'])

  const opened = await json(`${appOrigin}/api/projects/${project.value.id}/workspace/file?conversationId=${conversationID}&path=src%2Findex.ts`, { cookie: login.cookie })
  assert.equal(opened.response.status, 200)
  assert.equal(opened.value.content, 'export const answer = 42\n')
  assert.equal(opened.value.revision, 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')

  const saved = await json(`${appOrigin}/api/projects/${project.value.id}/workspace/file`, {
    method: 'PUT', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { conversationId: conversationID, path: 'src/index.ts', content: 'export const answer = 43\n', baseRevision: opened.value.revision, clientRequestId: 'workspace-save-alice' },
  })
  assert.equal(saved.response.status, 200)
  assert.notEqual(saved.value.revision, opened.value.revision)

  const stale = await json(`${appOrigin}/api/projects/${project.value.id}/workspace/file`, {
    method: 'PUT', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { conversationId: conversationID, path: 'src/index.ts', content: 'stale', baseRevision: opened.value.revision, clientRequestId: 'workspace-stale-alice' },
  })
  assert.equal(stale.response.status, 409)

  const escaped = await json(`${appOrigin}/api/projects/${project.value.id}/workspace/file?conversationId=${conversationID}&path=..%2F.kroot%2Fcredential.json`, { cookie: login.cookie })
  assert.equal(escaped.response.status, 400)
  const crossConversation = await json(`${appOrigin}/api/projects/${project.value.id}/workspace/tree?conversationId=conv-bob&path=`, { cookie: login.cookie })
  assert.equal(crossConversation.response.status, 404)
})

test('project previews stay user-scoped and expose only safe launch data', async () => {
  const login = await loginAs('alice', 'alice-password')
  await json(`${appOrigin}/api/workspace/provision`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken, body: {},
  })
  const projectResult = await json(`${appOrigin}/api/projects`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { name: 'Preview Project', locale: 'ko', clientRequestId: 'preview-project' },
  })
  const projectID = projectResult.value.id
  const applications = await json(`${appOrigin}/api/projects/${projectID}/apps`, { cookie: login.cookie })
  assert.equal(applications.response.status, 200)
  assert.deepEqual(applications.value, [{ path: 'apps/web', name: 'Preview Web', profile: 'next' }])
  const selected = await json(`${appOrigin}/api/projects/${projectID}/preview-app`, {
    method: 'PUT', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { appPath: 'apps/web' },
  })
  assert.equal(selected.response.status, 200)
  assert.equal(selected.value.previewAppPath, 'apps/web')
  assert.equal('workingDir' in selected.value, false)
  const created = await json(`${appOrigin}/api/projects/${projectID}/previews`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { appPath: 'apps/web', visibility: 'private', profile: 'auto', ttlSeconds: 600, clientRequestId: 'preview-create' },
  })
  assert.equal(created.response.status, 202)
  assert.equal(created.value.preview.projectId, projectID)
  assert.equal(created.value.preview.appPath, 'apps/web')
  assert.equal(created.value.preview.visibility, 'private')
  assert.match(created.value.accessUrl, /^https:\/\/p-[a-z2-7]+\.preview\.kroot\.io\/\?__pie_token=/)
  assert.equal('ownerUserId' in created.value.preview, false)
  assert.equal('backendHost' in created.value.preview, false)

  for (const appPath of ['../outside', '/workspace/other', 'apps\\web']) {
    const rejected = await json(`${appOrigin}/api/projects/${projectID}/previews`, {
      method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken,
      body: { appPath, visibility: 'private', profile: 'auto', ttlSeconds: 600, clientRequestId: `invalid-${Buffer.from(appPath).toString('hex')}` },
    })
    assert.equal(rejected.response.status, 400)
  }

  const listed = await json(`${appOrigin}/api/projects/${projectID}/previews`, { cookie: login.cookie })
  assert.equal(listed.response.status, 200)
  assert.deepEqual(listed.value.map((value) => value.id), [created.value.preview.id])

  const access = await json(`${appOrigin}/api/projects/${projectID}/previews/${created.value.preview.id}/access`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken, body: {},
  })
  assert.equal(access.response.status, 200)
  assert.match(access.value.accessUrl, /__pie_token=/)

  const published = await json(`${appOrigin}/api/projects/${projectID}/previews/${created.value.preview.id}/visibility`, {
    method: 'PUT', cookie: login.cookie, csrfToken: login.value.csrfToken,
    body: { visibility: 'public' },
  })
  assert.equal(published.response.status, 200)
  assert.equal(published.value.preview.id, created.value.preview.id)
  assert.equal(published.value.preview.hostname, created.value.preview.hostname)
  assert.equal(published.value.preview.visibility, 'public')
  assert.equal(published.value.accessUrl, '')

  const logs = await json(`${appOrigin}/api/projects/${projectID}/previews/${created.value.preview.id}/logs`, { cookie: login.cookie })
  assert.equal(logs.response.status, 200)
  assert.equal(logs.value.logs, 'preview output\n')

  const crossProject = await json(`${appOrigin}/api/projects/project-bob/previews`, { cookie: login.cookie })
  assert.equal(crossProject.response.status, 404)

  const stopped = await json(`${appOrigin}/api/projects/${projectID}/previews/${created.value.preview.id}/stop`, {
    method: 'POST', cookie: login.cookie, csrfToken: login.value.csrfToken, body: {},
  })
  assert.equal(stopped.response.status, 200)
  assert.equal(stopped.value.status, 'stopped')
  const deleted = await json(`${appOrigin}/api/projects/${projectID}/previews/${created.value.preview.id}`, {
    method: 'DELETE', cookie: login.cookie, csrfToken: login.value.csrfToken,
  })
  assert.equal(deleted.response.status, 200)
  assert.deepEqual(deleted.value, { id: created.value.preview.id, deleted: true })
})

test('the BFF forwards SSE chunks immediately and resumes from the requested sequence', async () => {
  const login = await loginAs('alice', 'alice-password')
  const gate = deferred()
  managerState.streamGate = gate
  try {
    const response = await fetch(`${appOrigin}/api/conversations/conv-alice-active/events?after=41`, {
      headers: { Cookie: login.cookie },
    })
    assert.equal(response.status, 200)
    assert.equal(response.headers.get('cache-control'), 'no-cache, no-store, no-transform')
    assert.equal(response.headers.get('x-accel-buffering'), 'no')
    assert.equal(managerState.eventLastIDs.at(-1), '41')

    const reader = response.body.getReader()
    const first = await readSSEEvent(reader)
    assert.match(first, /"type":"task_progress"/)

    let completed = false
    const remainder = readAll(reader).then((value) => {
      completed = true
      return value
    })
    await new Promise((resolve) => setTimeout(resolve, 25))
    assert.equal(completed, false, 'the first progress event must arrive before the stream completes')

    gate.resolve()
    const finalEvents = await remainder
    assert.match(finalEvents, /"type":"text"/)
    assert.match(finalEvents, /"type":"done"/)
  } finally {
    managerState.streamGate?.resolve()
    managerState.streamGate = null
  }
})

async function loginAs(username, password) {
  const result = await json(`${appOrigin}/api/auth/login`, { method: 'POST', body: { username, password } })
  assert.equal(result.response.status, 200)
  const setCookie = result.response.headers.get('set-cookie')
  return { ...result, setCookie, cookie: setCookie.split(';', 1)[0] }
}

async function json(url, { method = 'GET', body, cookie, csrfToken, origin, requestID } = {}) {
  const headers = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (cookie) headers.Cookie = cookie
  if (csrfToken) headers['X-CSRF-Token'] = csrfToken
  if (origin) headers.Origin = origin
  if (requestID) headers['X-Request-ID'] = requestID
  const response = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  const text = await response.text()
  return { response, value: text ? JSON.parse(text) : null }
}

async function fakeManager(request, response) {
  if (request.headers.authorization !== `Bearer ${integrationToken}`) return send(response, 401, { error: 'bad integration auth' })
  managerState.requestIDs.push(String(request.headers['x-request-id'] || ''))
  const url = new URL(request.url, 'http://manager.test')
  const prefix = '/v1/integrations/sample-chat'
  if (!url.pathname.startsWith(prefix)) return send(response, 404, {})
  const path = url.pathname.slice(prefix.length)
  const usageMatch = path.match(/^\/users\/([^/]+)\/usage\/summary$/)
  if (usageMatch && request.method === 'GET') {
    const externalUserID = decodeURIComponent(usageMatch[1])
    managerState.usageRequests.push({ externalUserID, days: url.searchParams.get('days') })
    return send(response, 200, {
      from: '2026-07-06T00:00:00Z', to: '2026-08-05T00:00:00Z', currency: 'USD', costSource: 'claude-agent-sdk',
      integrationUserId: 'must-not-leak',
      totals: { turns: 2, inputTokens: 100, outputTokens: 20, cacheReadInputTokens: 50, cacheCreationInputTokens: 10, webSearchRequests: 0, totalTokens: 180, costUsd: 0.0123 },
      byModel: [{ provider: 'firstParty', model: 'claude-test', canonicalModel: 'claude-test-v1', turns: 2, inputTokens: 100, outputTokens: 20, cacheReadInputTokens: 50, cacheCreationInputTokens: 10, webSearchRequests: 0, totalTokens: 180, costUsd: 0.0123, internal: 'must-not-leak' }],
      daily: [{ date: '2026-08-05', turns: 2, inputTokens: 100, outputTokens: 20, cacheReadInputTokens: 50, cacheCreationInputTokens: 10, webSearchRequests: 0, totalTokens: 180, costUsd: 0.0123 }],
    })
  }
  const usageEventsMatch = path.match(/^\/users\/([^/]+)\/usage\/events$/)
  if (usageEventsMatch && request.method === 'GET') {
    const externalUserID = decodeURIComponent(usageEventsMatch[1])
    managerState.usageEventRequests.push({ externalUserID, days: url.searchParams.get('days'), cursor: url.searchParams.get('cursor') || '', limit: url.searchParams.get('limit') })
    return send(response, 200, {
      items: [{
        occurredAt: '2026-08-05T12:34:56Z', projectId: 'project-alice', projectName: 'Alice Project',
        conversationId: 'conv-alice-active', requestId: 'request-alice', resultStatus: 'success',
        provider: 'firstParty', model: 'claude-test', canonicalModel: 'claude-test-v1',
        inputTokens: 100, outputTokens: 20, cacheReadInputTokens: 50, cacheCreationInputTokens: 10,
        webSearchRequests: 0, totalTokens: 180, costUsd: 0.0123, costSource: 'claude-agent-sdk',
        ownerUserId: 'must-not-leak', rawEvent: 'must-not-leak',
      }],
      nextCursor: 'next_cursor_2',
      integrationUserId: 'must-not-leak',
    })
  }
  const userMatch = path.match(/^\/users\/([^/]+)$/)
  if (userMatch) {
    const externalUserID = decodeURIComponent(userMatch[1])
    if (request.method === 'GET') {
      const binding = managerState.bindings.get(externalUserID)
      return binding ? send(response, 200, binding) : send(response, 404, {})
    }
    if (request.method === 'PUT') {
      if (managerState.failNextProvision) {
        managerState.failNextProvision = false
        return send(response, 503, { error: 'temporary capacity failure' })
      }
      const body = await requestJSON(request)
      managerState.provisions.push({ externalUserID, credential: body.credential })
      const binding = bindingFor(externalUserID)
      managerState.bindings.set(externalUserID, binding)
      return send(response, 201, binding)
    }
  }
  const createMatch = path.match(/^\/users\/([^/]+)\/conversations$/)
  if (createMatch && request.method === 'GET') {
    const externalUserID = decodeURIComponent(createMatch[1])
    const binding = managerState.bindings.get(externalUserID)
    if (!binding) return send(response, 409, { error: 'not ready' })
    return send(response, 200, [...managerState.conversations.values()].filter((value) => (
      value.integrationUserId === binding.id && !['closed', 'deleted'].includes(value.status)
    )))
  }
  if (createMatch && request.method === 'POST') {
    const externalUserID = decodeURIComponent(createMatch[1])
    const binding = managerState.bindings.get(externalUserID)
    if (!binding) return send(response, 409, { error: 'not ready' })
    const body = await requestJSON(request)
    const projects = managerState.projects.get(externalUserID) ?? []
    if (!projects.some((project) => project.id === body.projectId)) return send(response, 409, { error: 'project not ready' })
    const id = `conv-${externalUserID}`
    const value = conversation(id, binding.id, body.projectId)
    managerState.conversations.set(id, value)
    return send(response, 201, value)
  }
  const projectsMatch = path.match(/^\/users\/([^/]+)\/projects$/)
  if (projectsMatch) {
    const externalUserID = decodeURIComponent(projectsMatch[1])
    if (!managerState.bindings.has(externalUserID)) return send(response, 409, { error: 'not ready' })
    if (request.method === 'GET') return send(response, 200, managerState.projects.get(externalUserID) ?? [])
    if (request.method === 'POST') {
      const body = await requestJSON(request)
      const value = project(`project-${externalUserID}`, body.name, body.locale)
      managerState.projects.set(externalUserID, [value])
      managerState.projectApps.set(`${externalUserID}:${value.id}`, [{ path: 'apps/web', name: 'Preview Web', profile: 'next' }])
      return send(response, 201, value)
    }
  }
  const projectAppsMatch = path.match(/^\/users\/([^/]+)\/projects\/([^/]+)\/(apps|preview-app)$/)
  if (projectAppsMatch) {
    const externalUserID = decodeURIComponent(projectAppsMatch[1])
    const projectID = decodeURIComponent(projectAppsMatch[2])
    const action = projectAppsMatch[3]
    const projects = managerState.projects.get(externalUserID) ?? []
    const value = projects.find((candidate) => candidate.id === projectID)
    if (!value) return send(response, 404, {})
    const key = `${externalUserID}:${projectID}`
    if (action === 'apps' && request.method === 'GET') return send(response, 200, managerState.projectApps.get(key) ?? [])
    if (action === 'preview-app' && request.method === 'PUT') {
      const body = await requestJSON(request)
      if (!(managerState.projectApps.get(key) ?? []).some((application) => application.path === body.appPath)) return send(response, 400, { error: 'invalid app' })
      value.previewAppPath = body.appPath
      value.updatedAt = new Date().toISOString()
      return send(response, 200, value)
    }
  }
  const projectWorkspaceMatch = path.match(/^\/users\/([^/]+)\/projects\/([^/]+)\/workspace\/(tree|file)$/)
  if (projectWorkspaceMatch) {
    const externalUserID = decodeURIComponent(projectWorkspaceMatch[1])
    const projectID = decodeURIComponent(projectWorkspaceMatch[2])
    const resource = projectWorkspaceMatch[3]
    const projects = managerState.projects.get(externalUserID) ?? []
    const binding = managerState.bindings.get(externalUserID)
    const body = request.method === 'PUT' ? await requestJSON(request) : null
    const conversationID = request.method === 'PUT' ? body.conversationId : url.searchParams.get('conversationId')
    const selectedConversation = managerState.conversations.get(conversationID)
    if (!binding || !projects.some((value) => value.id === projectID) || !selectedConversation || selectedConversation.integrationUserId !== binding.id || selectedConversation.projectId !== projectID) {
      return send(response, 404, {})
    }
    if (resource === 'tree' && request.method === 'GET') {
      const requestedPath = url.searchParams.get('path') || ''
      if (requestedPath === '') return send(response, 200, { path: '', entries: [{ name: 'src', path: 'src', type: 'directory', modifiedAt: new Date().toISOString(), internal: 'must-not-leak' }] })
      if (requestedPath === 'src') return send(response, 200, { path: 'src', entries: [{ name: 'index.ts', path: 'src/index.ts', type: 'file', size: managerState.workspaceFiles.get('src/index.ts').content.length, modifiedAt: new Date().toISOString() }] })
      return send(response, 404, { error: 'not found' })
    }
    if (resource === 'file' && request.method === 'GET') {
      const requestedPath = url.searchParams.get('path') || ''
      const value = managerState.workspaceFiles.get(requestedPath)
      if (!value) return send(response, 404, { error: 'not found' })
      return send(response, 200, { path: requestedPath, content: value.content, revision: value.revision, size: Buffer.byteLength(value.content), modifiedAt: new Date().toISOString(), language: 'typescript', credential: 'must-not-leak' })
    }
    if (resource === 'file' && request.method === 'PUT') {
      const value = managerState.workspaceFiles.get(body.path)
      if (!value) return send(response, 404, { error: 'not found' })
      if (value.revision !== body.baseRevision) return send(response, 409, { error: 'version conflict', code: 'conflict', currentRevision: value.revision })
      const revision = `sha256:${'b'.repeat(64)}`
      managerState.workspaceFiles.set(body.path, { content: body.content, revision })
      return send(response, 200, { path: body.path, revision, size: Buffer.byteLength(body.content), modifiedAt: new Date().toISOString(), language: 'typescript' })
    }
  }
  const previewsMatch = path.match(/^\/users\/([^/]+)\/projects\/([^/]+)\/previews(?:\/([^/]+)(?:\/(access|restart|logs|visibility|stop|record))?)?$/)
  if (previewsMatch) {
    const externalUserID = decodeURIComponent(previewsMatch[1])
    const projectID = decodeURIComponent(previewsMatch[2])
    const previewID = previewsMatch[3] ? decodeURIComponent(previewsMatch[3]) : ''
    const action = previewsMatch[4] || ''
    const projects = managerState.projects.get(externalUserID) ?? []
    if (!projects.some((value) => value.id === projectID)) return send(response, 404, {})
    const key = `${externalUserID}:${projectID}`
    const values = managerState.previews.get(key) ?? []
    if (!previewID && request.method === 'GET') return send(response, 200, values)
    if (!previewID && request.method === 'POST') {
      const body = await requestJSON(request)
      const value = preview(`preview-${externalUserID}`, projectID, body.appPath, body.visibility, body.profile)
      managerState.previews.set(key, [value])
      return send(response, 202, launch(value))
    }
    const value = values.find((candidate) => candidate.id === previewID)
    if (!value) return send(response, 404, {})
    if (!action && request.method === 'GET') return send(response, 200, value)
    if (!action && request.method === 'DELETE') {
      value.status = 'stopped'
      value.updatedAt = new Date().toISOString()
      return send(response, 200, value)
    }
    if (action === 'access' && request.method === 'POST') return send(response, 200, launch(value))
    if (action === 'stop' && request.method === 'POST') {
      value.status = 'stopped'
      value.updatedAt = new Date().toISOString()
      return send(response, 200, value)
    }
    if (action === 'record' && request.method === 'DELETE') {
      managerState.previews.set(key, values.filter((candidate) => candidate.id !== previewID))
      return send(response, 200, value)
    }
    if (action === 'visibility' && request.method === 'PUT') {
      const body = await requestJSON(request)
      value.visibility = body.visibility
      value.updatedAt = new Date().toISOString()
      return send(response, 200, launch(value))
    }
    if (action === 'restart' && request.method === 'POST') {
      value.status = 'starting'
      return send(response, 202, launch(value))
    }
    if (action === 'logs' && request.method === 'GET') {
      response.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8' })
      response.end('preview output\n')
      return
    }
  }
  const conversationMatch = path.match(/^\/conversations\/([^/]+)(?:\/(messages|events|retry))?$/)
  const permissionMatch = path.match(/^\/conversations\/([^/]+)\/permissions\/([^/]+)$/)
  if (permissionMatch && request.method === 'POST') {
    const conversationID = decodeURIComponent(permissionMatch[1])
    const requestID = decodeURIComponent(permissionMatch[2])
    if (!managerState.conversations.has(conversationID)) return send(response, 404, {})
    const body = await requestJSON(request)
    managerState.permissions.push({ conversationID, requestID, allow: body.allow })
    return send(response, 202, { accepted: true })
  }
  if (conversationMatch) {
    const conversationID = decodeURIComponent(conversationMatch[1])
    const action = conversationMatch[2]
    const value = managerState.conversations.get(conversationID)
    if (!value) return send(response, 404, {})
    if (!action && request.method === 'GET') return send(response, 200, value)
    if (!action && request.method === 'DELETE') {
      managerState.conversations.delete(conversationID)
      return send(response, 200, { ok: true })
    }
    if (action === 'messages' && request.method === 'POST') {
      const body = await requestJSON(request)
      managerState.messages.push({ conversationID, prompt: body.prompt, images: body.images ?? [] })
      return send(response, 202, { accepted: true })
    }
    if (action === 'retry' && request.method === 'POST') {
      managerState.retries.push({ conversationID, idempotencyKey: request.headers['idempotency-key'] })
      value.status = 'ready'
      value.updatedAt = new Date().toISOString()
      return send(response, 202, value)
    }
    if (action === 'events' && request.method === 'GET') {
      managerState.eventLastIDs.push(String(request.headers['last-event-id'] || ''))
      response.writeHead(200, { 'Content-Type': 'text/event-stream' })
      if (managerState.streamGate) {
        response.write(`id: 42\ndata: ${JSON.stringify({ sequence: 42, type: 'task_progress', data: { taskId: 'task-1', summary: 'building' } })}\n\n`)
        await managerState.streamGate.promise
        response.end([
          `id: 43\ndata: ${JSON.stringify({ sequence: 43, type: 'text', data: { text: 'streamed-answer' } })}\n\n`,
          `id: 44\ndata: ${JSON.stringify({ sequence: 44, type: 'done', data: {} })}\n\n`,
        ].join(''))
        return
      }
      response.end(`id: 1\ndata: ${JSON.stringify({ sequence: 1, type: 'text', data: { text: 'hello-from-fake-manager' } })}\n\n`)
      return
    }
  }
  send(response, 404, {})
}

function bindingFor(externalUserID) {
  return {
    id: externalUserID === 'external-alice' ? 'iu-alice' : 'iu-bob',
    externalUserId: externalUserID,
    status: 'ready', credentialVersion: 1, updatedAt: new Date().toISOString(),
  }
}

function project(id, name, locale) {
  return { id, name, locale, previewAppPath: '', status: 'ready', workingDir: `/workspace/projects/${id}`, initializedAt: new Date().toISOString(), createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }
}

function preview(id, projectId, appPath = '.', visibility = 'private', profile = 'auto') {
  const now = new Date().toISOString()
  return { id, projectId, appPath, hostname: 'p-aaaaaaaaaaaaaaaaaaaaaaaaaa.preview.kroot.io', backendHost: 'must-not-leak', ownerUserId: 'must-not-leak', port: 20000, profile, visibility, status: 'ready', createdAt: now, updatedAt: now, expiresAt: new Date(Date.now() + 600_000).toISOString() }
}

function launch(value) {
  const url = `https://${value.hostname}/`
  return { preview: value, url, accessUrl: value.visibility === 'private' ? `${url}?__pie_token=test-token` : '' }
}

function conversation(id, integrationUserId, projectId = 'project-bob') {
  const now = new Date().toISOString()
  return {
    id, integrationUserId, projectId, status: 'ready', createdAt: now, updatedAt: now,
    connection: {
      relayAvailable: true, runtimeRunning: true, runtimeHealthy: true,
      clientConnected: true, relayRegistered: true, sessionStatus: 'active',
      reason: 'connected', lastError: '', lastHeartbeat: now,
      relayNodeId: 'must-not-leak', internal: 'must-not-leak',
    },
  }
}

function user(username, displayName, externalUserId, passwordHash, credential) {
  return Object.freeze({ username, displayName, externalUserId, passwordHash, credential })
}

async function requestJSON(request) {
  const chunks = []
  for await (const chunk of request) chunks.push(chunk)
  return JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')
}

function send(response, status, value) {
  const body = JSON.stringify(value)
  response.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) })
  response.end(body)
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.on('error', reject)
    server.listen(0, '127.0.0.1', () => resolve(`http://127.0.0.1:${server.address().port}`))
  })
}

function close(server) {
  return new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()))
}

function deferred() {
  let resolve
  const promise = new Promise((done) => { resolve = done })
  return { promise, resolve }
}

async function readSSEEvent(reader) {
  let text = ''
  while (!text.includes('\n\n')) {
    const { done, value } = await reader.read()
    if (done) break
    text += Buffer.from(value).toString('utf8')
  }
  return text
}

async function readAll(reader) {
  let text = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) return text
    text += Buffer.from(value).toString('utf8')
  }
}

function createTestAppServer(handler) {
  return createServer(async (request, response) => {
    try {
      if (!request.url?.startsWith('/api/')) {
        response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' })
        response.end('<!doctype html><html lang="ko"><title>Pie Workspace Chat</title></html>')
        return
      }
      const chunks = []
      for await (const chunk of request) chunks.push(chunk)
      const headers = new Headers()
      for (const [name, value] of Object.entries(request.headers)) {
        if (Array.isArray(value)) value.forEach((item) => headers.append(name, item))
        else if (value !== undefined) headers.set(name, value)
      }
      const body = chunks.length ? Buffer.concat(chunks) : undefined
      const webRequest = new Request(`http://${request.headers.host}${request.url}`, {
        method: request.method,
        headers,
        body,
      })
      const webResponse = await handler(webRequest, { remoteAddress: request.socket.remoteAddress || 'unknown' })
      response.writeHead(webResponse.status, Object.fromEntries(webResponse.headers.entries()))
      if (webResponse.body) {
        const reader = webResponse.body.getReader()
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          response.write(value)
        }
      }
      response.end()
    } catch (error) {
      response.writeHead(500, { 'Content-Type': 'application/json' })
      response.end(JSON.stringify({ error: error.message }))
    }
  })
}
