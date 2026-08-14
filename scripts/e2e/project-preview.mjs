#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import https from 'node:https'
import { resolve } from 'node:path'
import tls from 'node:tls'

const managerURL = optional('PIE_E2E_CONTROL_URL', 'http://127.0.0.1:19090').replace(/\/$/, '')
const adminToken = optional('PIE_E2E_CONTROL_TOKEN', 'pat-local-admin')
const previewPort = Number.parseInt(optional('PIE_E2E_PREVIEW_PORT', '18443'), 10)
const previewDomain = optional('PIE_E2E_PREVIEW_DOMAIN', 'preview.localhost')
const previewIP = optional('PIE_E2E_PREVIEW_IP', '127.0.0.1')
const managerID = optional('PIE_E2E_MANAGER_ID', 'local-manager')
const gatewayContainer = optional('PIE_E2E_PREVIEW_GATEWAY_CONTAINER', 'pie-preview-gateway')
const expectedFirstPreviewPort = Number.parseInt(optional('PIE_E2E_EXPECTED_FIRST_PREVIEW_PORT', '20000'), 10)
const caPath = optional('PIE_E2E_LOCAL_CA', 'deploy/local/.generated/certs/local-ca.crt')
const ca = caPath === 'system' ? undefined : readFileSync(resolve(caPath))
const runID = Date.now().toString(36)
const appPath = optional('PIE_E2E_PREVIEW_APP_PATH', '.')
const integrationID = `preview-e2e-${runID}`
const externalUserID = `preview-user-${runID}`
let serviceToken = ''
let ownerUserID = ''
let project
let previews = []
let cleaned = false

try {
  await waitFor(`${managerURL}/readyz`, 30_000)
  const registered = await json(`${managerURL}/v1/admin/integrations`, {
    method: 'POST',
    headers: bearer(adminToken),
    body: {
      id: integrationID,
      displayName: 'Project Preview E2E',
      status: 'active',
      maxUsers: 1,
      maxProjectsPerUser: 2,
      maxPreviewsPerUser: 4,
      maxConversationsPerUser: 1,
      credential: { targetPath: '.preview-e2e/credential.json', format: 'json', maxBytes: 4096 },
    },
  }, 201)
  serviceToken = registered.serviceToken
  if (!serviceToken?.startsWith('pie_int_')) throw new Error('integration service token was not returned')

  const binding = await integration(`/users/${externalUserID}`, {
    method: 'PUT',
    headers: { 'Idempotency-Key': `signup-${runID}` },
    body: { credential: { subject: externalUserID, test: true } },
  }, 201)
  ownerUserID = binding.ownerUserId
  const executorContainer = await poll(() => findExecutor(ownerUserID), 30_000, 'new Executor container')

  project = await integration(`/users/${externalUserID}/projects`, {
    method: 'POST',
    headers: { 'Idempotency-Key': `project-${runID}` },
    body: { name: 'Preview E2E App', locale: 'ko' },
    timeoutMs: 120_000,
  }, 201)
  if (project.status !== 'ready') throw new Error(`project is not ready: ${JSON.stringify(project)}`)
  const applications = await integration(`/users/${externalUserID}/projects/${project.id}/apps`)
  const selectedApplication = applications.find((value) => value.path === appPath)
  if (!selectedApplication || !selectedApplication.name || !['next', 'vite', 'npm'].includes(selectedApplication.profile)) {
    throw new Error(`expected runnable application was not discovered: ${JSON.stringify(applications)}`)
  }
  project = await integration(`/users/${externalUserID}/projects/${project.id}/preview-app`, {
    method: 'PUT', body: { appPath },
  })
  if (project.previewAppPath !== appPath) throw new Error(`project preview application was not persisted: ${JSON.stringify(project)}`)

  const requests = ['private-a', 'private-b', 'private-c', 'private-d']
  const launches = await Promise.all(requests.map((key) => integration(
    `/users/${externalUserID}/projects/${project.id}/previews`,
    {
      method: 'POST',
      headers: { 'Idempotency-Key': `${key}-${runID}` },
      body: { profile: 'npm', visibility: 'private', ttlSeconds: 600 },
    },
    [200, 202],
  )))
  previews = [launches[0].preview]
  if (!previews.every((value) => value.appPath === appPath)) {
    throw new Error(`preview application path was not preserved: ${previews.map((value) => value.appPath).join(',')}`)
  }
  if (!launches.every((value) => value.preview.id === previews[0].id && value.preview.hostname === previews[0].hostname && value.preview.port === previews[0].port)) {
    throw new Error(`concurrent requests did not converge on one preview: ${JSON.stringify(launches.map((value) => value.preview))}`)
  }
  const previewAuthoritySuffix = previewPort === 443 ? '' : `:${previewPort}`
  if (!launches.every((value) => value.url.startsWith(`https://${value.preview.hostname}${previewAuthoritySuffix}/`))) {
    throw new Error('Manager returned a preview URL that does not match the configured public port')
  }

  previews = await Promise.all(previews.map((preview) => poll(async () => {
    const value = await integration(`/users/${externalUserID}/projects/${project.id}/previews/${preview.id}`)
    if (value.status === 'failed') throw new Error(`preview failed: ${value.lastError}`)
    return value.status === 'ready' ? value : null
  }, 45_000, `ready preview ${preview.id}`)))

  verifyRuntimeBoundary(ownerUserID, executorContainer)

  const privateLaunch = launches[0]
  const privatePreview = previews.find((value) => value.id === privateLaunch.preview.id)

  await expectPreview(privatePreview.hostname, '/', 401)
  const launchToken = new URL(privateLaunch.accessUrl).searchParams.get('__pie_token')
  if (!launchToken) throw new Error('private launch URL does not contain its short-lived token')

  const exchange = await previewRequest(privatePreview.hostname, `/?__pie_token=${encodeURIComponent(launchToken)}`)
  if (![302, 303].includes(exchange.status) || exchange.headers.location !== '/') {
    throw new Error(`launch token exchange did not strip the token: HTTP ${exchange.status} ${exchange.headers.location || ''}`)
  }
  const setCookie = exchange.headers['set-cookie']?.[0] || ''
  if (!setCookie.startsWith('__Host-pie_preview=') || !/; Path=\//i.test(setCookie) || !/; Secure/i.test(setCookie) || !/; HttpOnly/i.test(setCookie) || /; Domain=/i.test(setCookie)) {
    throw new Error(`preview session cookie is not host-only and secure: ${setCookie}`)
  }
  const cookie = setCookie.split(';', 1)[0]
  const privateResponse = await previewRequest(privatePreview.hostname, '/', {
    headers: {
      Cookie: `${cookie}; app_session=preserved`,
      'X-Pie-User-Id': 'spoofed-user',
      'X-Pie-Preview-Id': 'spoofed-preview',
      'X-Pie-Backend': 'spoofed-backend',
    },
  })
  const privatePayload = assertPreviewJSON(privateResponse, 200)
  if (privatePayload.cookie !== 'app_session=preserved' || Object.values(privatePayload.internalHeaders || {}).some(Boolean)) {
    throw new Error(`gateway leaked its authentication cookie or spoofed internal headers: ${privateResponse.body}`)
  }
  const backendCookies = privateResponse.headers['set-cookie'] || []
  if (backendCookies.length !== 1 || !backendCookies[0].startsWith('app_session=updated')) {
    throw new Error(`backend replaced the gateway authentication cookie: ${JSON.stringify(backendCookies)}`)
  }

  const postResponse = await previewRequest(privatePreview.hostname, '/echo', { method: 'POST', headers: { Cookie: cookie, 'Content-Type': 'text/plain' }, body: 'pie-preview-body' })
  const postPayload = assertPreviewJSON(postResponse, 200)
  if (postPayload.method !== 'POST' || postPayload.body !== 'pie-preview-body') throw new Error('request body did not reach the isolated preview process')

  const streamResponse = await previewRequest(privatePreview.hostname, '/stream', { headers: { Cookie: cookie }, trackChunks: true })
  if (streamResponse.status !== 200 || streamResponse.body !== 'first\nsecond\n' || streamResponse.chunks < 2 || streamResponse.lastChunkAt-streamResponse.firstChunkAt < 50) {
    throw new Error(`streaming response was buffered or corrupted: ${JSON.stringify(streamResponse)}`)
  }

  const publicLaunch = await integration(`/users/${externalUserID}/projects/${project.id}/previews/${privatePreview.id}/visibility`, {
    method: 'PUT', body: { visibility: 'public' },
  })
  if (publicLaunch.preview.hostname !== privatePreview.hostname || publicLaunch.preview.port !== privatePreview.port || publicLaunch.accessUrl) {
    throw new Error(`visibility change replaced the preview identity: ${JSON.stringify(publicLaunch)}`)
  }
  await sleep(2300)
  const publicResponse = await previewRequest(privatePreview.hostname, '/')
  assertPreviewJSON(publicResponse, 200)
  await expectWebSocket(privatePreview.hostname)

  const privateAgain = await integration(`/users/${externalUserID}/projects/${project.id}/previews/${privatePreview.id}/visibility`, {
    method: 'PUT', body: { visibility: 'private' },
  })
  if (!privateAgain.accessUrl || privateAgain.preview.hostname !== privatePreview.hostname || privateAgain.preview.port !== privatePreview.port) {
    throw new Error(`private visibility did not preserve the preview identity: ${JSON.stringify(privateAgain)}`)
  }
  await sleep(2300)
  const staleCookieResponse = await previewRequest(privatePreview.hostname, '/', { headers: { Cookie: cookie } })
  if (staleCookieResponse.status !== 401) {
    throw new Error(`old private session survived an access policy change: HTTP ${staleCookieResponse.status}`)
  }
  await expectPreview(`p-aaaaaaaaaaaaaaaaaaaaaaaaaa.${previewDomain}`, '/', 404)

  const logs = await text(`${managerURL}/v1/integrations/${integrationID}/users/${externalUserID}/projects/${project.id}/previews/${privatePreview.id}/logs?tailBytes=65536`, { headers: bearer(serviceToken) }, 200)
  if (!logs.includes('node server.mjs')) throw new Error(`preview logs did not include npm output: ${logs}`)

  await integration(`/users/${externalUserID}/projects/${project.id}/previews/${privatePreview.id}/restart`, { method: 'POST' }, 202)
  await poll(async () => {
    const value = await integration(`/users/${externalUserID}/projects/${project.id}/previews/${privatePreview.id}`)
    if (value.status === 'failed') throw new Error(`restarted preview failed: ${value.lastError}`)
    return value.status === 'ready' ? value : null
  }, 45_000, 'restarted preview')

  for (const preview of previews) {
    await integration(`/users/${externalUserID}/projects/${project.id}/previews/${preview.id}`, { method: 'DELETE' }, 200)
  }
  await sleep(2300)
  await expectPreview(privatePreview.hostname, '/', 404)

  await integration(`/users/${externalUserID}/projects/${project.id}/previews/${privatePreview.id}/record`, { method: 'DELETE' }, 200)
  await integration(`/users/${externalUserID}/projects/${project.id}/previews/${privatePreview.id}`, {}, 404)

  const lifecycleLaunch = await integration(`/users/${externalUserID}/projects/${project.id}/previews`, {
    method: 'POST',
    headers: { 'Idempotency-Key': `lifecycle-${runID}` },
    body: { profile: 'npm', visibility: 'public', ttlSeconds: 600 },
  }, 202)
  const lifecyclePreview = await poll(async () => {
    const value = await integration(`/users/${externalUserID}/projects/${project.id}/previews/${lifecycleLaunch.preview.id}`)
    if (value.status === 'failed') throw new Error(`lifecycle preview failed: ${value.lastError}`)
    return value.status === 'ready' ? value : null
  }, 45_000, 'lifecycle preview')
  if (lifecyclePreview.port !== expectedFirstPreviewPort) {
    throw new Error(`stopped preview ports were not reused: received ${lifecyclePreview.port}`)
  }
  if (lifecyclePreview.hostname === privatePreview.hostname) {
    throw new Error('deleted preview hostname was unexpectedly reused')
  }
  previews = [lifecyclePreview]
  await integration(`/users/${externalUserID}`, { method: 'DELETE' }, 202)
  await poll(async () => {
    const values = await json(`${managerURL}/v1/admin/previews?limit=5000`, { headers: bearer(adminToken) })
    const value = values.find((candidate) => candidate.id === lifecyclePreview.id)
    return value?.status === 'stopped' ? value : null
  }, 30_000, 'preview revocation after user suspension')
  await poll(() => findExecutor(ownerUserID).then((value) => value ? null : true), 30_000, 'Executor removal')
  await poll(() => previewNetworks(ownerUserID).length === 0, 30_000, 'private preview network removal')
  await sleep(2300)
  await expectPreview(lifecyclePreview.hostname, '/', 404)
  cleaned = true

  console.log(JSON.stringify({
    ok: true,
    integrationID,
    ownerUserID,
    projectID: project.id,
    previewCount: previews.length,
    verified: ['provision', 'project-init', 'app-auto-discovery', 'project-app-persistence', 'nested-app-path', 'dependency-prepare', 'concurrent-singleton', 'identity-preserving-visibility', 'access-generation-revocation', 'record-delete', 'new-host-after-delete', 'port-reuse', 'private-network', 'no-host-port', 'private-cookie', 'backend-cookie-isolation', 'internal-header-isolation', 'request-body', 'streaming', 'websocket-upgrade', 'public-access', 'restart', 'logs', 'stop', 'suspension-revocation', 'cleanup'],
  }))
} finally {
  if (!cleaned && serviceToken && externalUserID && optional('PIE_E2E_KEEP_RESOURCES', '0') !== '1') {
    try {
      if (project) {
        for (const preview of previews) {
          await integration(`/users/${externalUserID}/projects/${project.id}/previews/${preview.id}`, { method: 'DELETE' }, 200).catch(() => {})
        }
      }
      await integration(`/users/${externalUserID}`, { method: 'DELETE' }, 202).catch(() => {})
    } catch {}
  }
}

async function integration(path, options = {}, expected = 200) {
  return json(`${managerURL}/v1/integrations/${integrationID}${path}`, { ...options, headers: { ...bearer(serviceToken), ...(options.headers || {}) } }, expected)
}

async function json(url, options = {}, expected = 200) {
  const response = await fetchWithTimeout(url, withJSON(options))
  const body = await response.text()
  const expectedStatuses = Array.isArray(expected) ? expected : [expected]
  if (!expectedStatuses.includes(response.status)) throw new Error(`${options.method || 'GET'} ${url}: HTTP ${response.status}, expected ${expectedStatuses.join(' or ')}: ${body}`)
  if (!body) return null
  try {
    return JSON.parse(body)
  } catch {
    return body
  }
}

async function text(url, options = {}, expected = 200) {
  const response = await fetchWithTimeout(url, options)
  const body = await response.text()
  if (response.status !== expected) throw new Error(`${options.method || 'GET'} ${url}: HTTP ${response.status}, expected ${expected}: ${body}`)
  return body
}

function withJSON(options) {
  if (options.body === undefined || typeof options.body === 'string') return options
  return { ...options, headers: { 'Content-Type': 'application/json', ...(options.headers || {}) }, body: JSON.stringify(options.body) }
}

function bearer(token) {
  return { Authorization: `Bearer ${token}` }
}

async function fetchWithTimeout(url, options = {}) {
  const timeout = options.timeoutMs || 15_000
  const clean = { ...options, signal: AbortSignal.timeout(timeout) }
  delete clean.timeoutMs
  return fetch(url, clean)
}

async function waitFor(url, timeoutMs) {
  return poll(async () => {
    try {
      const response = await fetchWithTimeout(url, { timeoutMs: 1500 })
      await response.arrayBuffer()
      return response.ok
    } catch {
      return false
    }
  }, timeoutMs, url)
}

async function poll(check, timeoutMs, label) {
  const deadline = Date.now() + timeoutMs
  let lastError
  while (Date.now() < deadline) {
    try {
      const value = await check()
      if (value) return value
    } catch (error) {
      lastError = error
      if (!String(error.message).includes('fetch failed')) throw error
    }
    await sleep(200)
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`)
}

function previewRequest(hostname, path, options = {}) {
  return new Promise((resolveRequest, reject) => {
    const startedAt = Date.now()
    const request = https.request({
      hostname,
      servername: hostname,
      port: previewPort,
      path,
      method: options.method || 'GET',
      headers: options.headers || {},
      ...(ca ? { ca } : {}),
      lookup: (_hostname, lookupOptions, callback) => {
        if (lookupOptions?.all) callback(null, [{ address: previewIP, family: 4 }])
        else callback(null, previewIP, 4)
      },
      timeout: 10_000,
    }, (response) => {
      const chunks = []
      let firstChunkAt = 0
      let lastChunkAt = 0
      response.on('data', (chunk) => {
        const now = Date.now()
        if (!firstChunkAt) firstChunkAt = now
        lastChunkAt = now
        chunks.push(chunk)
      })
      response.on('end', () => resolveRequest({
        status: response.statusCode,
        headers: response.headers,
        body: Buffer.concat(chunks).toString(),
        chunks: chunks.length,
        firstChunkAt: firstChunkAt || startedAt,
        lastChunkAt: lastChunkAt || startedAt,
      }))
    })
    request.on('timeout', () => request.destroy(new Error('preview request timed out')))
    request.on('error', reject)
    if (options.body) request.write(options.body)
    request.end()
  })
}

function expectWebSocket(hostname) {
  return new Promise((resolveSocket, reject) => {
    const key = Buffer.from(`pie-preview-${Date.now()}`).toString('base64')
    let received = Buffer.alloc(0)
    const socket = tls.connect({
      host: previewIP,
      port: previewPort,
      servername: hostname,
      ...(ca ? { ca } : {}),
      rejectUnauthorized: true,
    })
    const timer = setTimeout(() => socket.destroy(new Error('preview WebSocket timed out')), 10_000)
    socket.once('secureConnect', () => {
      socket.write([
        'GET /ws HTTP/1.1',
        `Host: ${hostname}`,
        'Connection: Upgrade',
        'Upgrade: websocket',
        'Sec-WebSocket-Version: 13',
        `Sec-WebSocket-Key: ${key}`,
        '',
        '',
      ].join('\r\n'))
    })
    socket.on('data', (chunk) => {
      received = Buffer.concat([received, chunk])
      if (received.includes(Buffer.from('101 Switching Protocols')) && received.includes(Buffer.from('preview-websocket-ok'))) {
        clearTimeout(timer)
        socket.end()
        resolveSocket()
      }
    })
    socket.once('error', (error) => {
      clearTimeout(timer)
      reject(error)
    })
    socket.once('close', () => {
      clearTimeout(timer)
      if (!received.includes(Buffer.from('preview-websocket-ok'))) {
        reject(new Error(`preview WebSocket closed before a frame arrived: ${received.toString()}`))
      }
    })
  })
}

async function expectPreview(hostname, path, expected) {
  const response = await previewRequest(hostname, path)
  if (response.status !== expected) throw new Error(`https://${hostname}:${previewPort}${path}: HTTP ${response.status}, expected ${expected}: ${response.body}`)
  return response
}

function assertPreviewJSON(response, expected) {
  if (response.status !== expected) throw new Error(`preview returned HTTP ${response.status}: ${response.body}`)
  const payload = JSON.parse(response.body)
  if (!payload.ok) throw new Error(`unexpected preview payload: ${response.body}`)
  return payload
}

async function findExecutor(userID) {
  const names = docker(['ps', '-a', '--filter', `label=pie.user_id=${userID}`, '--filter', `label=pie.manager_id=${managerID}`, '--format', '{{.Names}}']).trim()
  if (!names) return null
  if (names.includes('\n')) throw new Error(`multiple Executor containers found for ${userID}: ${names}`)
  return names
}

function previewNetworks(userID) {
  const output = docker(['network', 'ls', '--filter', `label=pie.user_id=${userID}`, '--filter', 'label=pie.network_purpose=preview', '--format', '{{.Name}}']).trim()
  return output ? output.split('\n') : []
}

function verifyRuntimeBoundary(userID, executorContainer) {
  const inspect = JSON.parse(docker(['inspect', executorContainer]))[0]
  const bindings = inspect.HostConfig?.PortBindings || {}
  if (Object.keys(bindings).length) throw new Error(`Executor published a host port: ${JSON.stringify(bindings)}`)
  const networks = previewNetworks(userID)
  if (networks.length !== 1) throw new Error(`expected one private preview network, found ${networks.join(',')}`)
  const network = JSON.parse(docker(['network', 'inspect', networks[0]]))[0]
  if (!network.Internal) throw new Error('preview network is not internal')
  const members = Object.values(network.Containers || {}).map((value) => value.Name).sort()
  if (members.length !== 2 || !members.includes(executorContainer) || !members.includes(gatewayContainer)) {
    throw new Error(`unexpected private network members: ${members.join(',')}`)
  }
}

function docker(args) {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
}

function optional(name, fallback) {
  return process.env[name]?.trim() || fallback
}

function sleep(ms) {
  return new Promise((resolveSleep) => setTimeout(resolveSleep, ms))
}
