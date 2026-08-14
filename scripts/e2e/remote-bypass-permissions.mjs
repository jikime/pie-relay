#!/usr/bin/env node

import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'

const managerURL = required('PIE_MANAGER_URL').replace(/\/$/, '')
const integrationID = required('PIE_INTEGRATION_ID')
const adminToken = process.env.PIE_ADMIN_TOKEN_FILE
  ? secretFile('PIE_ADMIN_TOKEN_FILE')
  : envFileSecret('PIE_ADMIN_ENV_FILE', 'PIE_MANAGER_ADMIN_TOKEN')
const integrationToken = secretFile('PIE_INTEGRATION_TOKEN_FILE')
const expectedUsers = positiveInteger(process.env.PIE_EXPECTED_USERS || '2')
const timeoutMs = positiveInteger(process.env.PIE_SMOKE_TIMEOUT_MS || '180000')

const snapshot = await request('/v1/admin/snapshot', adminToken)
const targets = []
for (const binding of snapshot.integrationUsers || []) {
  if (binding.integrationId !== integrationID || binding.status !== 'ready') continue
  const project = (snapshot.projects || []).find((candidate) =>
    candidate.integrationId === integrationID
    && candidate.integrationUserId === binding.id
    && candidate.status === 'ready')
  if (project) targets.push({ binding, project })
}
const selectedTargets = targets.slice(0, expectedUsers)
if (selectedTargets.length !== expectedUsers) {
  throw new Error(`expected ${expectedUsers} ready users with projects, found ${selectedTargets.length}`)
}

const startedAt = Date.now()
const results = await Promise.all(selectedTargets.map(runTarget))
console.log(JSON.stringify({
  ok: true,
  users: results.length,
  elapsedMs: Date.now() - startedAt,
  permissionRequests: results.reduce((sum, value) => sum + value.permissionRequests, 0),
  eventTypes: [...new Set(results.flatMap((value) => value.eventTypes))].sort(),
  route: 'Manager API -> Relay -> isolated Docker clientd -> Claude Code Bash tool',
}))

async function runTarget({ binding, project }) {
  const userBase = `/v1/integrations/${encodeURIComponent(integrationID)}/users/${encodeURIComponent(binding.externalUserId)}`
  const conversation = await request(`${userBase}/conversations`, integrationToken, {
    method: 'POST',
    headers: { 'Idempotency-Key': `bypass-conversation-${randomBytes(12).toString('hex')}` },
    body: JSON.stringify({ projectId: project.id }),
    expectedStatus: 201,
  })
  const base = `/v1/integrations/${encodeURIComponent(integrationID)}/conversations/${encodeURIComponent(conversation.id)}`
  let completed = false
  try {
    const ready = await poll(async () => {
      const current = await request(base, integrationToken)
      if (current.status === 'error') throw new Error(current.lastError || 'conversation failed')
      return current.status === 'ready' ? current : null
    }, Math.min(timeoutMs, 120_000), 'fresh conversation readiness')
    return await runConversation(base, () => { completed = true })
  } finally {
    if (!completed) {
      await request(`${base}/cancel`, integrationToken, {
        method: 'POST',
        headers: { 'Idempotency-Key': `bypass-cancel-${randomBytes(12).toString('hex')}` },
        body: '{}',
        expectedStatus: 202,
      }).catch(() => {})
    }
    await request(base, integrationToken, { method: 'DELETE' }).catch(() => {})
  }
}

async function runConversation(base, markCompleted) {
  const previous = await request(`${base}/events?stream=false&limit=1000`, integrationToken)
  let cursor = previous.reduce((max, event) => Math.max(max, Number(event.sequence) || 0), 0)
  let done = false
  const observed = []
  await request(`${base}/messages`, integrationToken, {
    method: 'POST',
    headers: { 'Idempotency-Key': `bypass-message-${randomBytes(12).toString('hex')}` },
    body: JSON.stringify({
      // The answer is intentionally unknown to the model. A prompt that
      // includes its expected marker can be answered without using Bash and
      // would make this permission-policy smoke test a false positive.
      prompt: "Bash 도구로 cat /proc/sys/kernel/random/uuid 명령을 정확히 한 번 실행한 뒤, 출력된 UUID 한 줄만 그대로 답하세요. 추측해서 답하지 마세요.",
    }),
    expectedStatus: 202,
  })
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const events = await request(`${base}/events?stream=false&after=${cursor}&limit=1000`, integrationToken)
    for (const event of events) {
      cursor = Math.max(cursor, Number(event.sequence) || 0)
      observed.push(event)
      if (event.type === 'permission_request') {
        throw new Error('unexpected permission_request under bypass policy')
      }
      if (event.type === 'error') {
        throw new Error(event.data?.message || 'Claude execution failed')
      }
      if (event.type === 'done') done = true
    }
    if (done) break
    await delay(500)
  }
  if (!done) throw new Error('timed out waiting for Claude completion')
  markCompleted()
  const text = observed.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join('').trim()
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(text)) {
    throw new Error('Claude response is not the UUID produced by Bash')
  }
  if (!observed.some((event) => event.type === 'tool_call' && event.data?.name === 'Bash')) {
    throw new Error('Claude did not execute the required Bash tool')
  }
  return {
    permissionRequests: observed.filter((event) => event.type === 'permission_request').length,
    eventTypes: [...new Set(observed.map((event) => event.type))],
  }
}

async function request(path, token, options = {}) {
  const expectedStatus = options.expectedStatus ?? 200
  const response = await fetch(`${managerURL}${path}`, {
    ...options,
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      ...(options.headers || {}),
    },
    signal: AbortSignal.timeout(timeoutMs),
  })
  const text = await response.text()
  if (response.status !== expectedStatus) {
    throw new Error(`${path}: HTTP ${response.status}, expected ${expectedStatus}: ${text.slice(0, 240)}`)
  }
  return text ? JSON.parse(text) : null
}

function required(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function secretFile(name) {
  const path = required(name)
  const value = readFileSync(path, 'utf8').trim()
  if (!value) throw new Error(`${name} is empty`)
  return value
}

function envFileSecret(fileName, key) {
  const path = required(fileName)
  const line = readFileSync(path, 'utf8').split(/\r?\n/).find((candidate) => candidate.startsWith(`${key}=`))
  const value = line?.slice(key.length + 1).trim()
  if (!value) throw new Error(`${fileName} does not contain ${key}`)
  return value
}

function positiveInteger(value) {
  const parsed = Number.parseInt(value, 10)
  if (!Number.isSafeInteger(parsed) || parsed <= 0) throw new Error(`invalid positive integer: ${value}`)
  return parsed
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
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
    await delay(500)
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`)
}
