#!/usr/bin/env node
// Pie Relay ACP executor
//
// Keeps the Relay/browser NDJSON contract stable while speaking ACP v2
// JSON-RPC over stdio to a replaceable local agent process. The default agent
// is Claude Code's official ACP adapter, but any ACP-compatible command can be
// selected with PIE_ACP_AGENT_COMMAND and PIE_ACP_AGENT_ARGS_JSON.

import { spawn } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { existsSync, realpathSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'

const MAX_LINE_BYTES = 32 * 1024 * 1024
const RPC_TIMEOUT_MS = positiveNumber(process.env.PIE_ACP_RPC_TIMEOUT_MS, 60_000)
const TURN_TIMEOUT_MS = positiveNumber(process.env.PIE_ACP_TURN_TIMEOUT_MS, 2 * 60 * 60_000)
const PERMISSION_TIMEOUT_MS = positiveNumber(process.env.PIE_ACP_PERMISSION_TIMEOUT_MS, 5 * 60_000)

const out = (value) => process.stdout.write(`${JSON.stringify(value)}\n`)
const debug = (message) => {
  if (process.env.PIE_ACP_DEBUG) process.stderr.write(`[pie-acp] ${message}\n`)
}

function positiveNumber(value, fallback) {
  const parsed = Number(value)
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback
}

export function parseAgentArgs(raw = process.env.PIE_ACP_AGENT_ARGS_JSON) {
  if (!raw) return []
  let value
  try {
    value = JSON.parse(raw)
  } catch {
    throw new Error('PIE_ACP_AGENT_ARGS_JSON must be a JSON string array')
  }
  if (!Array.isArray(value) || !value.every((entry) => typeof entry === 'string')) {
    throw new Error('PIE_ACP_AGENT_ARGS_JSON must be a JSON string array')
  }
  return value
}

export function resolveWorkingDir(request = {}) {
  for (const candidate of [request.cwd, request.projectPath, process.env.CLI_RELAY_DEFAULT_CWD]) {
    if (typeof candidate === 'string' && path.isAbsolute(candidate) && existsSync(candidate)) return candidate
  }
  return os.homedir()
}

export function translateSessionUpdate(message) {
  const update = message?.params?.update
  if (!update || typeof update !== 'object') return null
  switch (update.sessionUpdate) {
    case 'agent_message_chunk':
      return typeof update.content?.text === 'string' ? { type: 'text', text: update.content.text } : null
    case 'agent_thought_chunk':
      return typeof update.content?.text === 'string' ? { type: 'thinking', text: update.content.text } : null
    case 'tool_call':
      return {
        type: 'tool_call',
        toolCallId: update.toolCallId,
        name: update.title || update.kind || 'tool',
        input: update.rawInput ?? update.content ?? {},
      }
    case 'tool_call_update':
      return {
        type: 'tool_result',
        toolCallId: update.toolCallId,
        status: update.status,
        content: update.content,
      }
    case 'plan':
      return { type: 'plan', entries: update.entries ?? [] }
    case 'available_commands_update':
      return { type: 'available_commands', commands: update.availableCommands ?? [] }
    default:
      return null
  }
}

export function buildPromptBlocks(request = {}) {
  const blocks = []
  if (Array.isArray(request.images)) {
    for (const image of request.images.slice(0, 10)) {
      if (!image || typeof image.data !== 'string' || typeof image.mimeType !== 'string') continue
      if (!/^image\/(png|jpeg|webp|gif)$/.test(image.mimeType)) continue
      blocks.push({ type: 'image', mimeType: image.mimeType, data: image.data })
    }
  }
  blocks.push({ type: 'text', text: String(request.prompt ?? '') })
  return blocks
}

function rpcKey(id) {
  return `${typeof id}:${JSON.stringify(id)}`
}

function selectPermissionOption(options, allow) {
  const preferred = allow ? ['allow_once', 'allow_always'] : ['reject_once', 'reject_always']
  for (const kind of preferred) {
    const option = options.find((entry) => entry?.kind === kind && typeof entry.optionId === 'string')
    if (option) return option.optionId
  }
  return null
}

export class AcpBridge {
  constructor({ command, args = [], cwd, env = process.env, emit = out } = {}) {
    this.command = command || env.PIE_ACP_AGENT_COMMAND || 'claude-agent-acp'
    this.args = args
    this.cwd = cwd || os.homedir()
    this.env = env
    this.emit = emit
    this.child = null
    this.nextId = 1
    this.pending = new Map()
    this.permissions = new Map()
    this.sessionId = ''
    this.sessionCwd = ''
    this.activeTurn = null
    this.closed = false
    this.stdoutBuffer = Buffer.alloc(0)
  }

  async start() {
    this.child = spawn(this.command, this.args, {
      cwd: this.cwd,
      env: this.env,
      stdio: ['pipe', 'pipe', 'inherit'],
      detached: process.platform !== 'win32',
    })
    this.child.stdout.on('data', (chunk) => this.onData(chunk))
    this.child.once('error', (error) => this.failAll(error))
    this.child.once('exit', (code, signal) => {
      if (!this.closed) this.failAll(new Error(`ACP agent exited (code=${code ?? 'null'}, signal=${signal ?? 'none'})`))
    })
    const initialized = await this.request('initialize', {
      protocolVersion: 2,
      clientCapabilities: {
        auth: { terminal: true },
        _meta: { 'terminal-auth': true },
      },
      clientInfo: { name: 'pie-relay-acp', version: '0.1.0' },
    })
    debug(`initialized protocol=${initialized?.protocolVersion ?? 'unknown'}`)
    return initialized
  }

  onData(chunk) {
    this.stdoutBuffer = Buffer.concat([this.stdoutBuffer, chunk])
    if (this.stdoutBuffer.length > MAX_LINE_BYTES && this.stdoutBuffer.indexOf(10) === -1) {
      this.failAll(new Error(`ACP agent emitted a line larger than ${MAX_LINE_BYTES} bytes`))
      this.close()
      return
    }
    let newline
    while ((newline = this.stdoutBuffer.indexOf(10)) !== -1) {
      const line = this.stdoutBuffer.subarray(0, newline)
      this.stdoutBuffer = this.stdoutBuffer.subarray(newline + 1)
      if (line.length > MAX_LINE_BYTES) {
        this.failAll(new Error(`ACP agent emitted a line larger than ${MAX_LINE_BYTES} bytes`))
        this.close()
        return
      }
      const trimmed = line.toString('utf8').trim()
      if (!trimmed) continue
      let message
      try {
        message = JSON.parse(trimmed)
      } catch {
        debug('ignored non-JSON stdout line from ACP agent')
        continue
      }
      this.onMessage(message)
    }
  }

  onMessage(message) {
    if (Object.hasOwn(message, 'id') && (Object.hasOwn(message, 'result') || Object.hasOwn(message, 'error'))) {
      const pending = this.pending.get(rpcKey(message.id))
      if (!pending) return
      this.pending.delete(rpcKey(message.id))
      clearTimeout(pending.timer)
      if (message.error) {
        const error = new Error(message.error.message || `ACP error ${message.error.code ?? -32000}`)
        error.code = message.error.code
        pending.reject(error)
      } else {
        pending.resolve(message.result)
      }
      return
    }
    if (message.method === 'session/update') {
      const translated = translateSessionUpdate(message)
      if (translated) this.emit(translated)
      return
    }
    if (message.method === 'pie/artifact') {
      this.emit({ type: 'artifact', ...(message.params ?? {}) })
      return
    }
    if (message.method === 'session/request_permission' && Object.hasOwn(message, 'id')) {
      this.onPermissionRequest(message)
      return
    }
    if (message.method && Object.hasOwn(message, 'id')) {
      this.write({
        jsonrpc: '2.0',
        id: message.id,
        error: { code: -32601, message: `Method not found: ${message.method}` },
      }).catch((error) => this.failAll(error))
    }
  }

  async onPermissionRequest(message) {
    const options = Array.isArray(message.params?.options) ? message.params.options : []
    const policy = this.env.PIE_ACP_PERMISSION_POLICY || 'interactive'
    if (policy === 'allow_once' || policy === 'deny_once') {
      await this.answerPermission(message.id, options, policy === 'allow_once')
      return
    }
    const requestId = randomUUID()
    const timer = setTimeout(() => {
      this.permissions.delete(requestId)
      this.write({ jsonrpc: '2.0', id: message.id, result: { outcome: { outcome: 'cancelled' } } }).catch(() => {})
    }, PERMISSION_TIMEOUT_MS)
    timer.unref?.()
    this.permissions.set(requestId, { rpcId: message.id, options, timer })
    const tool = message.params?.toolCall || {}
    this.emit({
      type: 'permission_request',
      requestId,
      toolName: tool.title || tool.kind || 'ACP tool',
      input: tool.rawInput ?? tool.content ?? {},
      options,
    })
  }

  async answerPermission(rpcId, options, allow) {
    const optionId = selectPermissionOption(options, allow)
    const outcome = optionId
      ? { outcome: 'selected', optionId }
      : { outcome: 'cancelled' }
    await this.write({ jsonrpc: '2.0', id: rpcId, result: { outcome } })
  }

  async permissionResponse(requestId, allow) {
    const pending = this.permissions.get(requestId)
    if (!pending) return false
    this.permissions.delete(requestId)
    clearTimeout(pending.timer)
    await this.answerPermission(pending.rpcId, pending.options, allow)
    return true
  }

  request(method, params, timeoutMs = RPC_TIMEOUT_MS) {
    const id = this.nextId++
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(rpcKey(id))
        reject(new Error(`ACP ${method} timed out after ${timeoutMs}ms`))
      }, timeoutMs)
      timer.unref?.()
      this.pending.set(rpcKey(id), { resolve, reject, timer, method })
      this.write({ jsonrpc: '2.0', id, method, params }).catch((error) => {
        clearTimeout(timer)
        this.pending.delete(rpcKey(id))
        reject(error)
      })
    })
  }

  write(value) {
    if (!this.child?.stdin?.writable) return Promise.reject(new Error('ACP agent stdin is closed'))
    const line = `${JSON.stringify(value)}\n`
    return new Promise((resolve, reject) => {
      this.child.stdin.write(line, (error) => error ? reject(error) : resolve())
    })
  }

  async ensureSession(request) {
    if (this.sessionId) return this.sessionId
    const cwd = resolveWorkingDir(request)
    const params = { cwd, mcpServers: [] }
    if (typeof request.model === 'string' && request.model.trim()) params.model = request.model.trim()
    if (typeof request.systemPrompt === 'string' && request.systemPrompt.trim()) params.systemPrompt = request.systemPrompt
    if (typeof request.title === 'string' && request.title.trim()) params._meta = { sessionTitle: request.title }
    const result = await this.request('session/new', params)
    if (typeof result?.sessionId !== 'string' || !result.sessionId) {
      throw new Error('ACP session/new response is missing sessionId')
    }
    this.sessionId = result.sessionId
    this.sessionCwd = cwd
    this.emit({ type: 'session_id', sessionId: this.sessionId })
    return this.sessionId
  }

  async prompt(request) {
    if (this.activeTurn) throw new Error('an ACP turn is already running')
    const sessionId = await this.ensureSession(request)
    const turn = { cancelled: false }
    this.activeTurn = turn
    try {
      const result = await this.request('session/prompt', {
        sessionId,
        prompt: buildPromptBlocks(request),
        ...(typeof request.model === 'string' && request.model.trim()
          ? { model: request.model.trim() }
          : {}),
      }, TURN_TIMEOUT_MS)
      if (!turn.cancelled) this.emit({ type: 'done', sessionId, stopReason: result?.stopReason || 'end_turn' })
    } finally {
      if (this.activeTurn === turn) this.activeTurn = null
    }
  }

  async cancel() {
    const turn = this.activeTurn
    if (!turn || !this.sessionId) return false
    turn.cancelled = true
    for (const [requestId, permission] of this.permissions) {
      clearTimeout(permission.timer)
      this.permissions.delete(requestId)
      await this.write({ jsonrpc: '2.0', id: permission.rpcId, result: { outcome: { outcome: 'cancelled' } } }).catch(() => {})
    }
    await this.request('session/cancel', { sessionId: this.sessionId })
    this.emit({ type: 'aborted' })
    return true
  }

  failAll(error) {
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer)
      pending.reject(error)
    }
    this.pending.clear()
  }

  async close() {
    if (this.closed) return
    this.closed = true
    for (const permission of this.permissions.values()) clearTimeout(permission.timer)
    this.permissions.clear()
    this.failAll(new Error('ACP executor closed'))
    if (!this.child || this.child.exitCode !== null) return
    if (process.platform !== 'win32' && this.child.pid) {
      try { process.kill(-this.child.pid, 'SIGTERM') } catch { this.child.kill('SIGTERM') }
    } else {
      this.child.kill('SIGTERM')
    }
    await Promise.race([
      new Promise((resolve) => this.child.once('exit', resolve)),
      new Promise((resolve) => setTimeout(resolve, 5_000)),
    ])
    if (this.child.exitCode === null) {
      if (process.platform !== 'win32' && this.child.pid) {
        try { process.kill(-this.child.pid, 'SIGKILL') } catch { this.child.kill('SIGKILL') }
      } else {
        this.child.kill('SIGKILL')
      }
    }
  }
}

export async function main() {
  let args
  try {
    args = parseAgentArgs()
  } catch (error) {
    out({ type: 'error', message: error.message })
    process.exitCode = 1
    return
  }
  const bridge = new AcpBridge({ args })
  try {
    await bridge.start()
  } catch (error) {
    out({ type: 'error', message: `ACP agent start failed: ${error.message}` })
    await bridge.close()
    process.exitCode = 1
    return
  }
  out({ type: 'ready', protocol: 'acp', protocolVersion: 2 })

  let turnQueue = Promise.resolve()
  const rl = createInterface({ input: process.stdin })
  for await (const line of rl) {
    if (Buffer.byteLength(line) > MAX_LINE_BYTES) {
      out({ type: 'error', message: 'request line is too large' })
      continue
    }
    let request
    try {
      request = JSON.parse(line)
    } catch {
      out({ type: 'error', message: 'invalid json request' })
      continue
    }
    if (request.type === 'chat') {
      turnQueue = turnQueue.then(() => bridge.prompt(request)).catch((error) => {
        out({ type: 'error', message: error.message })
      })
    } else if (request.type === 'permission_response') {
      bridge.permissionResponse(request.requestId, !!request.allow).catch((error) => {
        out({ type: 'error', message: error.message })
      })
    } else if (request.type === 'abort') {
      bridge.cancel().catch((error) => out({ type: 'error', message: error.message }))
    } else if (request.type === 'skills_list') {
      out({ type: 'skills_list', requestId: request.requestId, data: [] })
    }
  }
  await turnQueue
  await bridge.close()
}

let isMainModule = false
try {
  isMainModule = realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)
} catch {
  // Imported by tests.
}
if (isMainModule) await main()
