#!/usr/bin/env node

import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'

const appURL = required('PIE_WEB_CHAT_SMOKE_URL').replace(/\/$/, '')
const publicOrigin = (process.env.PIE_WEB_CHAT_SMOKE_ORIGIN?.trim() || new URL(appURL).origin).replace(/\/$/, '')
const login = loginCredential()
const username = process.env.PIE_WEB_CHAT_SMOKE_USERNAME?.trim() || login?.username || required('PIE_WEB_CHAT_SMOKE_USERNAME')
const password = process.env.PIE_WEB_CHAT_SMOKE_PASSWORD?.trim() || secretFile('PIE_WEB_CHAT_SMOKE_PASSWORD_FILE') || login?.password
if (!password) throw new Error('PIE_WEB_CHAT_SMOKE_PASSWORD, PIE_WEB_CHAT_SMOKE_PASSWORD_FILE or PIE_WEB_CHAT_SMOKE_LOGIN_FILE is required')
const createProject = process.env.PIE_WEB_CHAT_SMOKE_CREATE_PROJECT?.trim() === '1'
const requestedProjectID = process.env.PIE_WEB_CHAT_SMOKE_PROJECT_ID?.trim() || ''
const marker = `COOKAI-REMOTE-WEB-${randomBytes(6).toString('hex')}`
let cookie = ''
let csrfToken = ''
let conversationID = ''

try {
  await expect('/api/health', {}, 200)
  const login = await expect('/api/auth/login', {
    method: 'POST',
    headers: { Origin: publicOrigin, 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  }, 200)
  cookie = login.response.headers.get('set-cookie')?.split(';', 1)[0] || ''
  csrfToken = login.value.csrfToken
  if (!cookie || !csrfToken) throw new Error('login did not establish a secure session')

  const workspace = await json('/api/workspace')
  if (workspace.status !== 'ready') throw new Error(`remote workspace is ${workspace.status || 'missing'}`)
  let projects = await json('/api/projects')
  let project
  if (createProject) {
    const created = await mutate('/api/projects', {
      name: `local-usage-e2e-${Date.now()}`,
      locale: 'ko',
      clientRequestId: `remote-project-${randomBytes(12).toString('hex')}`,
    }, 201)
    project = await poll(async () => {
      projects = await json('/api/projects')
      const current = projects.find((value) => value.id === created.id)
      if (current?.status === 'error') throw new Error(current.lastError || 'remote project failed')
      return current?.status === 'ready' ? current : null
    }, 180_000, 'remote project readiness')
  } else {
    project = requestedProjectID
      ? projects.find((value) => value.id === requestedProjectID && value.status === 'ready')
      : projects.find((value) => value.status === 'ready')
  }
  if (!project) throw new Error('no ready remote project exists')

  const conversation = await mutate('/api/conversations', {
    projectId: project.id,
    clientRequestId: `remote-conversation-${randomBytes(12).toString('hex')}`,
  }, 201)
  conversationID = conversation.id
  const connection = await poll(async () => {
    const current = await json(`/api/conversations/${encodeURIComponent(conversationID)}`)
    if (current.status === 'error') throw new Error(current.lastError || 'remote conversation failed')
    if (current.status === 'closed' || current.status === 'deleted') {
      throw new Error(`remote conversation unexpectedly became ${current.status}: ${current.connection?.reason || current.lastError || 'unknown'}`)
    }
    return current.status === 'ready'
      && current.connection?.reason === 'connected'
      && current.connection?.relayAvailable === true
      && current.connection?.clientConnected === true
      && current.connection?.relayRegistered === true
      ? current.connection
      : null
  }, 120_000, 'remote conversation readiness')

  const stream = collectEvents(conversationID, marker, 180_000)
  await mutate(`/api/conversations/${encodeURIComponent(conversationID)}/messages`, {
    prompt: `다른 설명 없이 다음 문자열만 정확히 답하세요: ${marker}`,
    clientRequestId: `remote-message-${randomBytes(12).toString('hex')}`,
  }, 202)
  const events = await stream
  const responseText = events.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join('')
  if (!responseText.includes(marker)) throw new Error('Claude response did not contain the marker')

  console.log(JSON.stringify({
    ok: true,
    url: publicOrigin,
    workspace: workspace.status,
    projectId: project.id,
    marker,
    connection: {
      reason: connection.reason,
      relayAvailable: connection.relayAvailable,
      clientConnected: connection.clientConnected,
      relayRegistered: connection.relayRegistered,
    },
    eventTypes: [...new Set(events.map((event) => event.type))].sort(),
    route: 'public web BFF -> official Manager API -> official Relay -> remote Docker clientd -> Claude Code',
  }))
} finally {
  if (conversationID && cookie && csrfToken) {
    await expect(`/api/conversations/${encodeURIComponent(conversationID)}`, {
      method: 'DELETE',
      headers: sessionHeaders(),
    }, 200).catch(() => {})
  }
}

function required(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function secretFile(fileName) {
  const file = process.env[fileName]?.trim()
  if (!file) return ''
  const loaded = readFileSync(file, 'utf8').trim()
  if (!loaded) throw new Error(`${fileName} is empty`)
  return loaded
}

function loginCredential() {
  const file = process.env.PIE_WEB_CHAT_SMOKE_LOGIN_FILE?.trim()
  if (!file) return null
  const value = JSON.parse(readFileSync(file, 'utf8'))
  if (typeof value?.username !== 'string' || typeof value?.password !== 'string') {
    throw new Error('PIE_WEB_CHAT_SMOKE_LOGIN_FILE must contain username and password')
  }
  return value
}

function sessionHeaders() {
  return { Cookie: cookie, Origin: publicOrigin, 'X-CSRF-Token': csrfToken, 'Content-Type': 'application/json' }
}

async function json(path) {
  return (await expect(path, { headers: { Cookie: cookie } }, 200)).value
}

async function mutate(path, body, expectedStatus) {
  return (await expect(path, {
    method: 'POST', headers: sessionHeaders(), body: JSON.stringify(body),
  }, expectedStatus)).value
}

async function expect(path, options, expectedStatus) {
  const response = await fetch(`${appURL}${path}`, { ...options, signal: AbortSignal.timeout(190_000) })
  const text = await response.text()
  if (response.status !== expectedStatus) {
    throw new Error(`${path}: HTTP ${response.status}, expected ${expectedStatus}: ${text.slice(0, 300)}`)
  }
  return { response, value: text ? JSON.parse(text) : null }
}

async function collectEvents(id, expectedText, timeout) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(new Error('remote event stream timed out')), timeout)
  try {
    const response = await fetch(`${appURL}/api/conversations/${encodeURIComponent(id)}/events`, {
      headers: { Cookie: cookie }, signal: controller.signal,
    })
    if (!response.ok) throw new Error(`event stream failed with HTTP ${response.status}`)
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
        if (event.type === 'error') throw new Error(event.data?.message || 'remote Claude execution failed')
        if (['done', 'aborted'].includes(event.type) && combinedText.includes(expectedText)) {
          await reader.cancel()
          return events
        }
      }
      if (done) throw new Error('event stream ended before the Claude response completed')
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
    } catch (error) {
      lastError = error
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`)
}
