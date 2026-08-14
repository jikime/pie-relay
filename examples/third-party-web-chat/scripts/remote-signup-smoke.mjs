#!/usr/bin/env node

import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'

const appURL = required('PIE_WEB_CHAT_SMOKE_URL').replace(/\/$/, '')
const publicOrigin = (process.env.PIE_WEB_CHAT_SMOKE_ORIGIN?.trim() || new URL(appURL).origin).replace(/\/$/, '')
const login = loginCredential()
const marker = `COOKAI-SIGNUP-WEB-${randomBytes(6).toString('hex')}`
let cookie = ''
let csrfToken = ''
let conversationID = ''
let workspace
let authenticationRoute = 'signup'

try {
  const health = await expect('/api/health', {}, 200)
  if (health.value.userStore !== 'postgres') throw new Error(`unexpected user store: ${health.value.userStore || 'missing'}`)

  const signup = await request('/api/auth/signup', {
    method: 'POST',
    headers: { Origin: publicOrigin, 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: login.username, displayName: login.displayName, password: login.password }),
  }, 125_000)
  if (signup.response.status === 201) {
    establishSession(signup, 'signup')
    workspace = signup.value.workspace
  } else {
    // 회원 정보 저장 후 Manager 프로비저닝만 실패한 경우에도 같은 계정으로
    // 안전하게 재시도할 수 있어야 한다. 원래 오류는 로그인마저 실패했을 때 보존한다.
    const signupError = responseError('/api/auth/signup', signup, 201)
    const existing = await request('/api/auth/login', {
      method: 'POST',
      headers: { Origin: publicOrigin, 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: login.username, password: login.password }),
    })
    if (existing.response.status !== 200) throw signupError
    establishSession(existing, 'login')
    authenticationRoute = 'login -> provisioning recovery'
    workspace = await mutate('/api/workspace/provision', {}, 200, 125_000)
  }
  if (workspace?.status !== 'ready') throw new Error(`signup workspace is ${workspace?.status || 'missing'}`)

  const projectName = `CookAI Signup ${login.username}`
  const existingProjects = await json('/api/projects')
  const project = existingProjects.find((candidate) => candidate.name === projectName && candidate.status === 'ready')
    || await mutate('/api/projects', {
      name: projectName,
      locale: 'ko',
      // 최초 생성 응답이 유실되어도 같은 Project로 수렴한다.
      clientRequestId: `remote-signup-project-${login.username}`,
    }, 201, 190_000)
  if (project.status !== 'ready') throw new Error(`signup project is ${project.status || 'missing'}`)

  const conversation = await mutate('/api/conversations', {
    projectId: project.id,
    clientRequestId: `remote-signup-conversation-${randomBytes(12).toString('hex')}`,
  }, 201)
  conversationID = conversation.id
  await poll(async () => {
    const current = await json(`/api/conversations/${encodeURIComponent(conversationID)}`)
    if (current.status === 'error') throw new Error(current.lastError || 'remote signup conversation failed')
    return current.status === 'ready'
  }, 120_000, 'remote signup conversation readiness')

  const stream = collectEvents(conversationID, marker, 180_000)
  await mutate(`/api/conversations/${encodeURIComponent(conversationID)}/messages`, {
    prompt: `다른 설명 없이 다음 문자열만 정확히 답하세요: ${marker}`,
    clientRequestId: `remote-signup-message-${randomBytes(12).toString('hex')}`,
  }, 202)
  const events = await stream
  const responseText = events.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join('')
  if (!responseText.includes(marker)) throw new Error('new user Claude response did not contain the marker')

  console.log(JSON.stringify({
    ok: true,
    url: publicOrigin,
    username: login.username,
    workspace: workspace.status,
    projectId: project.id,
    marker,
    eventTypes: [...new Set(events.map((event) => event.type))].sort(),
    authenticationRoute,
    route: 'public signup -> PostgreSQL user -> Manager provision -> new Docker clientd -> Relay -> Claude Code',
  }))
} finally {
  if (conversationID && cookie && csrfToken) {
    await expect(`/api/conversations/${encodeURIComponent(conversationID)}`, {
      method: 'DELETE', headers: sessionHeaders(),
    }, 200).catch(() => {})
  }
}

function required(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function loginCredential() {
  const file = required('PIE_WEB_CHAT_SIGNUP_LOGIN_FILE')
  const value = JSON.parse(readFileSync(file, 'utf8'))
  if (typeof value?.username !== 'string' || typeof value?.displayName !== 'string' || typeof value?.password !== 'string') {
    throw new Error('PIE_WEB_CHAT_SIGNUP_LOGIN_FILE must contain username, displayName, and password')
  }
  return value
}

function sessionHeaders() {
  return { Cookie: cookie, Origin: publicOrigin, 'X-CSRF-Token': csrfToken, 'Content-Type': 'application/json' }
}

async function json(path) {
  return (await expect(path, { headers: { Cookie: cookie } }, 200)).value
}

async function mutate(path, body, expectedStatus, timeout = 30_000) {
  return (await expect(path, {
    method: 'POST', headers: sessionHeaders(), body: JSON.stringify(body),
  }, expectedStatus, timeout)).value
}

async function expect(path, options, expectedStatus, timeout = 30_000) {
  const result = await request(path, options, timeout)
  if (result.response.status !== expectedStatus) throw responseError(path, result, expectedStatus)
  return result
}

async function request(path, options, timeout = 30_000) {
  const response = await fetch(`${appURL}${path}`, { ...options, signal: AbortSignal.timeout(timeout) })
  const text = await response.text()
  let value = null
  if (text) {
    try { value = JSON.parse(text) } catch { value = text }
  }
  return { response, value, text }
}

function responseError(path, result, expectedStatus) {
  return new Error(`${path}: HTTP ${result.response.status}, expected ${expectedStatus}: ${result.text.slice(0, 300)}`)
}

function establishSession(result, label) {
  cookie = result.response.headers.get('set-cookie')?.split(';', 1)[0] || ''
  csrfToken = result.value?.csrfToken || ''
  if (!cookie || !csrfToken) throw new Error(`${label} did not establish a secure session`)
}

async function collectEvents(id, expectedText, timeout) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(new Error('remote signup event stream timed out')), timeout)
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
        if (event.type === 'error') throw new Error(event.data?.message || 'new user Claude execution failed')
        if (['done', 'aborted'].includes(event.type) && combinedText.includes(expectedText)) {
          await reader.cancel()
          return events
        }
      }
      if (done) throw new Error('event stream ended before the new user Claude response completed')
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
      if (await check()) return
    } catch (error) { lastError = error }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`)
}
