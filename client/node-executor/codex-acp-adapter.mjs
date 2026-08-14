#!/usr/bin/env node
// ACP v2 facade for the official Codex App Server.
// Pie Relay keeps one protocol for Claude/Codex while this adapter translates
// ACP session lifecycle, streaming updates, approvals, cancellation and image artifacts.

import { spawn } from 'node:child_process'
import { readFile } from 'node:fs/promises'
import { createInterface } from 'node:readline'

const app = spawn(process.env.PIE_CODEX_BIN || 'codex', ['app-server', '--stdio'], {
  stdio: ['pipe', 'pipe', 'inherit'],
  env: process.env,
})
const acp = createInterface({ input: process.stdin })
const codex = createInterface({ input: app.stdout })
let nextCodexId = 1
let nextAcpId = 1_000_000
const codexPending = new Map()
const acpPending = new Map()
const sessions = new Map()

const writeAcp = (value) => process.stdout.write(`${JSON.stringify(value)}\n`)
const writeCodex = (value) => app.stdin.write(`${JSON.stringify(value)}\n`)

function codexRequest(method, params) {
  const id = nextCodexId++
  return new Promise((resolve, reject) => {
    codexPending.set(id, { resolve, reject })
    writeCodex({ method, id, params })
  })
}

function acpRequest(method, params) {
  const id = nextAcpId++
  return new Promise((resolve, reject) => {
    acpPending.set(id, { resolve, reject })
    writeAcp({ jsonrpc: '2.0', id, method, params })
  })
}

function acpResult(id, result) {
  writeAcp({ jsonrpc: '2.0', id, result })
}

function acpError(id, error) {
  writeAcp({
    jsonrpc: '2.0',
    id,
    error: { code: Number(error?.code) || -32000, message: error?.message || String(error) },
  })
}

function sessionUpdate(sessionId, update) {
  writeAcp({ jsonrpc: '2.0', method: 'session/update', params: { sessionId, update } })
}

function sessionForThread(threadId) {
  for (const [sessionId, state] of sessions) {
    if (state.threadId === threadId) return { sessionId, state }
  }
  return null
}

function promptInput(blocks = []) {
  return blocks.flatMap((block) => {
    if (block?.type === 'text') return [{ type: 'text', text: String(block.text ?? '') }]
    if (block?.type === 'image' && block.data && block.mimeType) {
      return [{ type: 'image', url: `data:${block.mimeType};base64,${block.data}` }]
    }
    return []
  })
}

async function handleCodexNotification(message) {
  const params = message.params || {}
  const threadId = params.threadId || params.thread?.id
  const found = threadId ? sessionForThread(threadId) : null
  if (message.method === 'item/agentMessage/delta' && found && typeof params.delta === 'string') {
    sessionUpdate(found.sessionId, {
      sessionUpdate: 'agent_message_chunk',
      content: { type: 'text', text: params.delta },
    })
    return
  }
  if (message.method === 'item/reasoning/textDelta' && found && typeof params.delta === 'string') {
    sessionUpdate(found.sessionId, {
      sessionUpdate: 'agent_thought_chunk',
      content: { type: 'text', text: params.delta },
    })
    return
  }
  if ((message.method === 'item/started' || message.method === 'item/completed') && found) {
    const item = params.item || {}
    if (item.type === 'imageGeneration' && message.method === 'item/completed' && item.savedPath) {
      try {
        const bytes = await readFile(item.savedPath)
        const ext = String(item.savedPath).split('.').pop()?.toLowerCase()
        const mimeType = ext === 'webp' ? 'image/webp' : ext === 'jpg' || ext === 'jpeg' ? 'image/jpeg' : 'image/png'
        writeAcp({
          jsonrpc: '2.0',
          method: 'pie/artifact',
          params: {
            sessionId: found.sessionId,
            kind: 'image',
            path: item.savedPath,
            mimeType,
            data: bytes.toString('base64'),
          },
        })
      } catch (error) {
        sessionUpdate(found.sessionId, {
          sessionUpdate: 'tool_call_update',
          toolCallId: item.id,
          status: 'failed',
          content: [{ type: 'content', content: { type: 'text', text: `이미지 결과 읽기 실패: ${error.message}` } }],
        })
      }
      return
    }
    if (['commandExecution', 'fileChange', 'mcpToolCall', 'dynamicToolCall'].includes(item.type)) {
      sessionUpdate(found.sessionId, message.method === 'item/started'
        ? {
            sessionUpdate: 'tool_call',
            toolCallId: item.id,
            title: item.type,
            kind: item.type,
            rawInput: item.command ?? item.changes ?? item.arguments ?? {},
          }
        : {
            sessionUpdate: 'tool_call_update',
            toolCallId: item.id,
            status: item.status ?? 'completed',
            content: item.aggregatedOutput ?? item.result ?? item.changes ?? [],
          })
    }
    return
  }
  if (message.method === 'turn/completed' && found) {
    const turnId = params.turn?.id
    if (turnId && found.state.turns.has(turnId)) {
      const pending = found.state.turns.get(turnId)
      found.state.turns.delete(turnId)
      if (found.state.activeTurnId === turnId) found.state.activeTurnId = null
      const status = params.turn?.status
      if (status === 'failed') pending.reject(new Error(params.turn?.error?.message || 'Codex turn failed'))
      else pending.resolve({ stopReason: status === 'interrupted' ? 'cancelled' : 'end_turn' })
    }
  }
}

async function handleCodexRequest(message) {
  const method = message.method
  const params = message.params || {}
  const found = sessionForThread(params.threadId)
  if (!found) {
    writeCodex({ id: message.id, error: { code: -32602, message: 'Unknown thread' } })
    return
  }
  if (method === 'item/commandExecution/requestApproval' || method === 'item/fileChange/requestApproval') {
    const allow = await requestPermission(found.sessionId, params, method)
    writeCodex({ id: message.id, result: { decision: allow ? 'accept' : 'decline' } })
    return
  }
  if (method === 'item/permissions/requestApproval') {
    const allow = await requestPermission(found.sessionId, params, method)
    writeCodex({
      id: message.id,
      result: allow
        ? { permissions: params.permissions ?? {}, scope: 'turn' }
        : { permissions: {}, scope: 'turn' },
    })
    return
  }
  writeCodex({ id: message.id, error: { code: -32601, message: `Unsupported Codex request: ${method}` } })
}

async function requestPermission(sessionId, params, method) {
  const result = await acpRequest('session/request_permission', {
    sessionId,
    toolCall: {
      toolCallId: params.itemId || params.approvalId || method,
      title: params.command || params.reason || method,
      kind: method,
      rawInput: params,
    },
    options: [
      { optionId: 'allow_once', name: '이번만 허용', kind: 'allow_once' },
      { optionId: 'reject_once', name: '거부', kind: 'reject_once' },
    ],
  })
  return result?.outcome?.outcome === 'selected' && result.outcome.optionId === 'allow_once'
}

codex.on('line', (line) => {
  let message
  try { message = JSON.parse(line) } catch { return }
  if (Object.hasOwn(message, 'id') && (Object.hasOwn(message, 'result') || Object.hasOwn(message, 'error'))) {
    const pending = codexPending.get(message.id)
    if (!pending) return
    codexPending.delete(message.id)
    message.error ? pending.reject(Object.assign(new Error(message.error.message), message.error)) : pending.resolve(message.result)
  } else if (Object.hasOwn(message, 'id') && message.method) {
    void handleCodexRequest(message).catch((error) => {
      writeCodex({ id: message.id, error: { code: -32000, message: error.message } })
    })
  } else if (message.method) {
    void handleCodexNotification(message).catch((error) => {
      process.stderr.write(`[pie-codex-acp] notification failed: ${error.message}\n`)
    })
  }
})

acp.on('line', async (line) => {
  let message
  try { message = JSON.parse(line) } catch { return }
  if (Object.hasOwn(message, 'id') && (Object.hasOwn(message, 'result') || Object.hasOwn(message, 'error'))) {
    const pending = acpPending.get(message.id)
    if (!pending) return
    acpPending.delete(message.id)
    message.error ? pending.reject(new Error(message.error.message)) : pending.resolve(message.result)
    return
  }
  if (!Object.hasOwn(message, 'id')) return
  try {
    if (message.method === 'initialize') {
      await codexRequest('initialize', {
        clientInfo: { name: 'pie-relay-codex-acp', title: 'Pie Relay Codex ACP', version: '0.1.0' },
        capabilities: { experimentalApi: true },
      })
      writeCodex({ method: 'initialized', params: {} })
      acpResult(message.id, {
        protocolVersion: 2,
        agentCapabilities: { loadSession: false, promptCapabilities: { image: true, audio: false, embeddedContext: false } },
        agentInfo: { name: 'codex', title: 'Codex', version: 'app-server' },
      })
      return
    }
    if (message.method === 'session/new') {
      const result = await codexRequest('thread/start', {
        cwd: message.params?.cwd,
        model: message.params?.model && message.params.model !== 'default' ? message.params.model : null,
        developerInstructions: message.params?.systemPrompt || null,
        approvalPolicy: 'on-request',
        ephemeral: false,
      })
      const sessionId = result.thread.id
      sessions.set(sessionId, { threadId: result.thread.id, turns: new Map(), activeTurnId: null, model: message.params?.model })
      acpResult(message.id, { sessionId })
      return
    }
    if (message.method === 'session/prompt') {
      const state = sessions.get(message.params?.sessionId)
      if (!state) throw new Error('Unknown ACP session')
      const started = await codexRequest('turn/start', {
        threadId: state.threadId,
        input: promptInput(message.params?.prompt),
        model: message.params?.model && message.params.model !== 'default' ? message.params.model : null,
      })
      const turnId = started.turn.id
      state.activeTurnId = turnId
      const result = await new Promise((resolve, reject) => state.turns.set(turnId, { resolve, reject }))
      acpResult(message.id, result)
      return
    }
    if (message.method === 'session/cancel') {
      const state = sessions.get(message.params?.sessionId)
      if (state?.activeTurnId) {
        await codexRequest('turn/interrupt', {
          threadId: state.threadId,
          turnId: state.activeTurnId,
        })
      }
      acpResult(message.id, {})
      return
    }
    throw Object.assign(new Error(`Method not found: ${message.method}`), { code: -32601 })
  } catch (error) {
    acpError(message.id, error)
  }
})

app.on('exit', (code) => {
  for (const pending of codexPending.values()) pending.reject(new Error(`codex app-server exited (${code})`))
  for (const state of sessions.values()) {
    for (const pending of state.turns.values()) pending.reject(new Error(`codex app-server exited (${code})`))
  }
  process.exit(code ?? 1)
})
