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
const externalUserID = process.env.PIE_EXTERNAL_USER_ID?.trim()
const integrationUserIDs = new Set((process.env.PIE_INTEGRATION_USER_IDS || '')
  .split(',').map((value) => value.trim()).filter(Boolean))
const verifyDenyRules = process.env.PIE_VERIFY_DENY_RULES === 'true'

const snapshot = await request('/v1/admin/snapshot', adminToken)
const targets = []
for (const binding of snapshot.integrationUsers || []) {
  if (binding.integrationId !== integrationID || binding.status !== 'ready') continue
  if (externalUserID && binding.externalUserId !== externalUserID) continue
  if (integrationUserIDs.size > 0 && !integrationUserIDs.has(binding.id)) continue
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
const settledTargets = await Promise.allSettled(selectedTargets.map(runTarget))
const failedTargets = settledTargets.filter((result) => result.status === 'rejected')
if (failedTargets.length > 0) {
  throw new Error(`write bypass targets failed after cleanup: ${failedTargets.map((result) => result.reason?.message || String(result.reason)).join(' | ')}`)
}
const results = settledTargets.map((result) => result.value)
const denyResult = verifyDenyRules ? await runDenyTarget(selectedTargets[0]) : null
console.log(JSON.stringify({
  ok: true,
  users: results.length,
  elapsedMs: Date.now() - startedAt,
  tools: [...new Set(results.flatMap((value) => value.tools))].sort(),
  permissionRequests: results.reduce((sum, value) => sum + value.permissionRequests, 0),
  explicitDenyPreserved: denyResult?.denied ?? null,
  denyEnforcement: denyResult?.enforcement ?? null,
  route: 'Manager API -> Relay -> isolated Docker clientd -> Claude Code Write/Edit/Read/Bash(rm)',
}))

async function runDenyTarget({ binding, project }) {
  const userBase = `/v1/integrations/${encodeURIComponent(integrationID)}/users/${encodeURIComponent(binding.externalUserId)}`
  const conversation = await request(`${userBase}/conversations`, integrationToken, {
    method: 'POST',
    headers: { 'Idempotency-Key': `write-deny-conversation-${randomBytes(12).toString('hex')}` },
    body: JSON.stringify({ projectId: project.id }),
    expectedStatus: 201,
  })
  const base = `/v1/integrations/${encodeURIComponent(integrationID)}/conversations/${encodeURIComponent(conversation.id)}`
  try {
    await poll(async () => {
      const current = await request(base, integrationToken)
      if (current.status === 'error') throw new Error(current.lastError || 'conversation failed')
      return current.status === 'ready' ? current : null
    }, Math.min(timeoutMs, 120_000), 'deny conversation readiness')
    return await runDenyConversation(base)
  } finally {
    await request(`${base}/cancel`, integrationToken, {
      method: 'POST',
      headers: { 'Idempotency-Key': `write-deny-cancel-${randomBytes(12).toString('hex')}` },
      body: '{}',
      expectedStatus: 202,
    }).catch(() => {})
    await request(base, integrationToken, { method: 'DELETE' }).catch(() => {})
  }
}

async function runDenyConversation(base) {
  const suffix = randomBytes(8).toString('hex')
  const directory = `secrets/pie-deny-${suffix}`
  const file = `${directory}/blocked.txt`
  let cursor = 0
  const previous = await request(`${base}/events?stream=false&limit=1000`, integrationToken)
  cursor = previous.reduce((max, event) => Math.max(max, Number(event.sequence) || 0), 0)
  const observed = []
  let done = false
  await request(`${base}/messages`, integrationToken, {
    method: 'POST',
    headers: { 'Idempotency-Key': `write-deny-message-${randomBytes(12).toString('hex')}` },
    body: JSON.stringify({
      prompt: [
        `보안 정책 검증입니다. Bash 도구로 mkdir -p ${directory} 를 실행하세요.`,
        `그다음 반드시 Write 도구로 ${file} 파일에 blocked 내용을 쓰세요. 다른 도구로 파일 쓰기를 우회하지 마세요.`,
        `Write 결과와 관계없이 마지막에 Bash 도구로 rm -f ${file}; rmdir ${directory} 를 실행해 정리하세요.`,
        '도구 결과를 짧게 보고하세요.',
      ].join('\n'),
    }),
    expectedStatus: 202,
  })
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const events = await request(`${base}/events?stream=false&after=${cursor}&limit=1000`, integrationToken)
    for (const event of events) {
      cursor = Math.max(cursor, Number(event.sequence) || 0)
      observed.push(event)
      if (event.type === 'permission_request') throw new Error('explicit deny leaked into an interactive permission request')
      if (event.type === 'error') throw new Error(event.data?.message || 'deny verification failed')
      if (event.type === 'done') done = true
    }
    if (done) break
    await delay(500)
  }
  if (!done) throw new Error('timed out waiting for deny verification')

  const calls = new Map(observed
    .filter((event) => event.type === 'tool_call' && event.data?.toolCallId)
    .map((event) => [event.data.toolCallId, event.data.name]))
  const writeResults = observed.filter((event) =>
    event.type === 'tool_result'
    && event.data?.toolCallId
    && calls.get(event.data.toolCallId) === 'Write')
  if (writeResults.length === 0) {
    const text = observed.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join(' ')
    const protectedPathRecognized = /secrets|보호|차단|금지|deny|refus/i.test(text)
    const executionRefused = /실행하지|거부|차단|금지|보호|cannot|won't|refus/i.test(text)
    if (calls.size === 0 && protectedPathRecognized && executionRefused) {
      return { denied: true, enforcement: 'model-policy-refusal-before-tool' }
    }
    throw new Error(`Claude did not exercise or recognize protected Write rule: ${JSON.stringify({ tools: [...calls.values()], text: text.slice(0, 300) })}`)
  }
  if (writeResults.some((event) => event.data?.isError !== true)) {
    throw new Error('protected Write unexpectedly succeeded under managed auto-approval')
  }
  return { denied: true, enforcement: 'tool-policy-deny' }
}

async function runTarget({ binding, project }) {
  const userBase = `/v1/integrations/${encodeURIComponent(integrationID)}/users/${encodeURIComponent(binding.externalUserId)}`
  const conversation = await request(`${userBase}/conversations`, integrationToken, {
    method: 'POST',
    headers: { 'Idempotency-Key': `write-bypass-conversation-${randomBytes(12).toString('hex')}` },
    body: JSON.stringify({ projectId: project.id }),
    expectedStatus: 201,
  })
  const base = `/v1/integrations/${encodeURIComponent(integrationID)}/conversations/${encodeURIComponent(conversation.id)}`
  try {
    await poll(async () => {
      const current = await request(base, integrationToken)
      if (current.status === 'error') throw new Error(current.lastError || 'conversation failed')
      return current.status === 'ready' ? current : null
    }, Math.min(timeoutMs, 120_000), 'fresh conversation readiness')
    return await runConversation(base)
  } finally {
    await request(`${base}/cancel`, integrationToken, {
      method: 'POST',
      headers: { 'Idempotency-Key': `write-bypass-cancel-${randomBytes(12).toString('hex')}` },
      body: '{}',
      expectedStatus: 202,
    }).catch(() => {})
    await request(base, integrationToken, { method: 'DELETE' }).catch(() => {})
  }
}

async function runConversation(base) {
  const marker = `pie-write-edit-${randomBytes(10).toString('hex')}`
  // Claude Code intentionally gives hidden/control files extra protection.
  // Use an ordinary project source file so this test measures the Executor's
  // configured bypass policy rather than the separate protected-path guard.
  const file = `pie-bypass-smoke-${randomBytes(10).toString('hex')}.txt`
  const previous = await request(`${base}/events?stream=false&limit=1000`, integrationToken)
  let cursor = previous.reduce((max, event) => Math.max(max, Number(event.sequence) || 0), 0)
  const observed = []
  let done = false
  await request(`${base}/messages`, integrationToken, {
    method: 'POST',
    headers: { 'Idempotency-Key': `write-bypass-message-${randomBytes(12).toString('hex')}` },
    body: JSON.stringify({
      prompt: [
        `반드시 Write 도구로 현재 작업 폴더의 ${file} 파일을 만들고 내용은 before로 하세요.`,
        `그다음 반드시 Edit 도구로 before를 ${marker}로 바꾸세요.`,
        '반드시 Read 도구로 변경 결과를 확인하세요.',
        `마지막으로 반드시 Bash 도구의 rm -- ${file} 명령 하나만 실행해 시험 파일을 삭제하세요.`,
        `모든 작업이 성공한 경우에만 다른 설명 없이 ${marker} 문자열만 답하세요.`,
        'Write/Edit을 Bash나 다른 도구로 우회하지 마세요.',
      ].join('\n'),
    }),
    expectedStatus: 202,
  })
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const events = await request(`${base}/events?stream=false&after=${cursor}&limit=1000`, integrationToken)
    for (const event of events) {
      cursor = Math.max(cursor, Number(event.sequence) || 0)
      observed.push(event)
      if (event.type === 'permission_request') throw new Error('unexpected permission_request under bypass policy')
      if (event.type === 'error') throw new Error(event.data?.message || 'Claude execution failed')
      if (event.type === 'done') done = true
    }
    if (done) break
    await delay(500)
  }
  if (!done) throw new Error('timed out waiting for Claude completion')

  const calls = new Map(observed
    .filter((event) => event.type === 'tool_call' && event.data?.toolCallId)
    .map((event) => [event.data.toolCallId, event.data.name]))
  const tools = [...calls.values()]
  const resultSummary = observed
    .filter((event) => event.type === 'tool_result' && event.data?.toolCallId)
    .map((event) => ({
      tool: calls.get(event.data.toolCallId) || 'unknown',
      isError: event.data?.isError === true,
      content: String(event.data?.content || '').slice(0, 300),
    }))
  for (const requiredTool of ['Write', 'Edit', 'Read', 'Bash']) {
    if (!tools.includes(requiredTool)) {
      throw new Error(`Claude did not execute required ${requiredTool} tool: ${JSON.stringify({ tools, resultSummary })}`)
    }
  }
  for (const requiredTool of ['Write', 'Edit', 'Read', 'Bash']) {
    const results = observed.filter((event) =>
      event.type === 'tool_result'
      && event.data?.toolCallId
      && calls.get(event.data.toolCallId) === requiredTool)
    // Claude may correct a typo and retry the same tool. The end-to-end
    // contract is satisfied when at least one invocation succeeds; retaining
    // failed retries in resultSummary still makes genuine failures diagnosable.
    if (!results.some((event) => event.data?.isError !== true)) {
      throw new Error(`${requiredTool} never succeeded under bypass policy: ${JSON.stringify(resultSummary)}`)
    }
  }
  const text = observed.filter((event) => event.type === 'text').map((event) => event.data?.text || '').join('').trim()
  if (text !== marker) throw new Error(`unexpected Claude response: ${text.slice(0, 300)}`)
  return {
    tools,
    permissionRequests: observed.filter((event) => event.type === 'permission_request').length,
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
