import { randomUUID } from 'node:crypto'
import { basename } from 'node:path'
import { z } from 'zod'
import type { AuthenticatedMobileSocket } from '../../../upstream/src/main/runtime/rpc/mobile-socket-wiring.ts'
import { failure, success } from './rpc-response.ts'
import type { Reply, RpcRequest } from './types.ts'
import type { TerminalSession } from './terminal-session.ts'
import type { RelayGateway } from './relay-gateway.ts'

const RequestSchema = z
  .object({
    id: z.string().min(1).max(128),
    deviceToken: z.string().min(1).max(512),
    method: z.string().min(1).max(128),
    params: z.record(z.string(), z.unknown()).optional()
  })
  .strict()

type Subscription = {
  connectionId: string
  requestId: string
  reply: Reply
}

export type MobileRpcHandlerOptions = {
  cwd: string
  terminal: TerminalSession
  relay?: RelayGateway
}

export class MobileRpcHandler {
  readonly runtimeId = `pie-relay-${randomUUID()}`
  readonly worktreeId: string
  readonly repoId = 'pie-relay-repo'
  private readonly cwd: string
  private readonly terminal: TerminalSession
  private readonly relay: RelayGateway | undefined
  private readonly subscriptions = new Map<string, Subscription>()
  private readonly startedAt = Date.now()
  private lastOutputAt = this.startedAt
  private terminalSeq = 1
  // The adapter currently exposes one immutable terminal tab. Orca clients use
  // this version to decide whether to refetch tab state, so reads must not bump
  // it; only a real tab mutation should do that.
  private sessionTabsVersion = 1

  constructor(options: MobileRpcHandlerOptions) {
    this.cwd = options.cwd
    this.terminal = options.terminal
    this.relay = options.relay
    this.worktreeId = `${this.repoId}::${this.cwd}`
    this.terminal.onData((chunk) => this.publishTerminalData(chunk))
  }

  handle(socket: AuthenticatedMobileSocket, plaintext: string, reply: Reply): void {
    let raw: unknown
    try {
      raw = JSON.parse(plaintext)
    } catch {
      reply(JSON.stringify(failure(this.runtimeId, 'unknown', 'bad_request', 'Invalid JSON')))
      return
    }
    const parsed = RequestSchema.safeParse(raw)
    if (!parsed.success) {
      const id = typeof (raw as { id?: unknown })?.id === 'string' ? (raw as { id: string }).id : 'unknown'
      reply(JSON.stringify(failure(this.runtimeId, id, 'bad_request', 'Invalid RPC request')))
      return
    }
    const request = parsed.data as RpcRequest
    if (request.deviceToken !== socket.device.deviceToken) {
      reply(JSON.stringify(failure(this.runtimeId, request.id, 'unauthorized', 'Device token mismatch')))
      return
    }
    this.dispatch(socket, request, reply)
  }

  closeConnection(connectionId: string): void {
    for (const [key, subscription] of this.subscriptions) {
      if (subscription.connectionId === connectionId) {
        this.subscriptions.delete(key)
      }
    }
  }

  private send(reply: Reply, response: ReturnType<typeof success> | ReturnType<typeof failure>): void {
    reply(JSON.stringify(response))
  }

  private dispatch(socket: AuthenticatedMobileSocket, request: RpcRequest, reply: Reply): void {
    const params = request.params ?? {}
    switch (request.method) {
      case 'status.get':
        this.send(
          reply,
          success(this.runtimeId, request.id, {
            runtimeId: this.runtimeId,
            protocolVersion: 3,
            minCompatibleMobileVersion: 2,
            graphStatus: 'ready',
            windowCount: 1,
            tabCount: 1,
            terminalCount: 1,
            capabilities: ['runtime.status.compat.v1'],
            clientScope: socket.device.scope
          })
        )
        return

      case 'repo.list':
        this.send(reply, success(this.runtimeId, request.id, { repos: [this.repoSummary()] }))
        return

      case 'worktree.ps':
        this.send(
          reply,
          success(this.runtimeId, request.id, {
            worktrees: [this.worktreeSummary()],
            totalCount: 1,
            truncated: false
          })
        )
        return

      case 'worktree.show':
        this.send(reply, success(this.runtimeId, request.id, { worktree: this.worktreeSummary() }))
        return

      case 'worktree.activate':
        this.send(reply, success(this.runtimeId, request.id, { ok: true }))
        return

      case 'settings.get':
        this.send(
          reply,
          success(this.runtimeId, request.id, {
            settings: { defaultTuiAgent: 'codex', disabledTuiAgents: [], agentCmdOverrides: {} }
          })
        )
        return

      case 'ui.get':
        this.send(
          reply,
          success(this.runtimeId, request.id, {
            ui: {
              groupBy: 'repo',
              sortBy: 'recent',
              hideSleepingWorkspaces: false,
              hideDefaultBranchWorkspace: false,
              filterRepoIds: [],
              collapsedGroups: [],
              trustedOrcaHooks: {}
            }
          })
        )
        return

      case 'ui.set':
      case 'session.tabs.activate':
      case 'terminal.setDisplayMode':
        this.send(reply, success(this.runtimeId, request.id, { ok: true }))
        return

      case 'preflight.detectAgents':
      case 'preflight.detectRemoteAgents':
        this.send(reply, success(this.runtimeId, request.id, ['codex']))
        return

      case 'accounts.list':
        this.send(reply, success(this.runtimeId, request.id, { accounts: [] }))
        return

      case 'stats.summary':
        this.send(reply, success(this.runtimeId, request.id, { totals: {} }))
        return

      case 'linear.status':
        this.send(reply, success(this.runtimeId, request.id, { configured: false }))
        return

      case 'session.tabs.list':
        this.send(reply, success(this.runtimeId, request.id, this.sessionTabs()))
        return

      case 'session.tabs.subscribe':
        this.send(
          reply,
          success(this.runtimeId, request.id, { type: 'snapshot', ...this.sessionTabs() }, true)
        )
        return

      case 'session.tabs.unsubscribe':
        this.send(reply, success(this.runtimeId, request.id, { unsubscribed: true }))
        return

      case 'terminal.list':
        this.send(
          reply,
          success(this.runtimeId, request.id, {
            terminals: [this.terminalSummary()],
            totalCount: 1,
            truncated: false
          })
        )
        return

      case 'terminal.resolveActive':
        this.send(reply, success(this.runtimeId, request.id, { handle: this.terminal.handle }))
        return

      case 'terminal.show':
        this.send(reply, success(this.runtimeId, request.id, { terminal: this.terminalSummary() }))
        return

      case 'terminal.read':
        this.send(
          reply,
          success(this.runtimeId, request.id, {
            terminal: {
              handle: this.terminal.handle,
              tail: this.terminal.snapshot,
              truncated: false
            }
          })
        )
        return

      case 'terminal.subscribe':
        this.handleTerminalSubscribe(socket, request, reply)
        return

      case 'terminal.unsubscribe':
        this.handleTerminalUnsubscribe(socket, request, reply)
        return

      case 'terminal.send':
        this.handleTerminalSend(request, reply)
        return

      case 'terminal.updateViewport':
        this.applyViewport(params)
        this.send(reply, success(this.runtimeId, request.id, { updated: true }))
        return

      case 'terminal.clearBuffer':
        this.terminal.clear()
        this.send(reply, success(this.runtimeId, request.id, { clear: { ok: true } }))
        return

      case 'pairing.getEndpoints':
        if (!this.relay) {
          this.send(reply, success(this.runtimeId, request.id, { v: 1, relay: null }))
          return
        }
        void this.relay
          .getEndpoints(socket, {
            ...(typeof params.installReqId === 'string' ? { installReqId: params.installReqId } : {}),
            ...(typeof params.resumeConfirmReqId === 'string'
              ? { resumeConfirmReqId: params.resumeConfirmReqId }
              : {})
          })
          .then((result) => this.send(reply, success(this.runtimeId, request.id, result)))
          .catch((error) =>
            this.send(
              reply,
              failure(
                this.runtimeId,
                request.id,
                'relay_error',
                error instanceof Error ? error.message : String(error)
              )
            )
          )
        return

      case 'pairing.provisionRelay':
        if (
          !this.relay ||
          typeof params.reqId !== 'string' ||
          typeof params.newResumeTokenHash !== 'string'
        ) {
          this.send(
            reply,
            failure(this.runtimeId, request.id, 'relay_unavailable', 'Pie Relay is not configured')
          )
          return
        }
        void this.relay
          .provisionRelay(socket, {
            reqId: params.reqId,
            newResumeTokenHash: params.newResumeTokenHash,
            ...(typeof params.expectedCurrentHash === 'string'
              ? { expectedCurrentHash: params.expectedCurrentHash }
              : {})
          })
          .then((result) => this.send(reply, success(this.runtimeId, request.id, result)))
          .catch((error) =>
            this.send(
              reply,
              failure(
                this.runtimeId,
                request.id,
                'relay_error',
                error instanceof Error ? error.message : String(error)
              )
            )
          )
        return

      case 'terminal.rename':
        this.send(reply, success(this.runtimeId, request.id, { rename: { ok: true } }))
        return

      default:
        this.send(
          reply,
          failure(this.runtimeId, request.id, 'method_not_found', `Unknown method: ${request.method}`)
        )
    }
  }

  private handleTerminalSubscribe(
    socket: AuthenticatedMobileSocket,
    request: RpcRequest,
    reply: Reply
  ): void {
    this.applyViewport(request.params ?? {})
    const key = `${socket.connectionId}:${request.id}`
    this.subscriptions.set(key, {
      connectionId: socket.connectionId,
      requestId: request.id,
      reply
    })
    const size = this.terminal.size
    this.send(
      reply,
      success(
        this.runtimeId,
        request.id,
        {
          type: 'scrollback',
          lines: this.terminal.snapshot,
          truncated: false,
          cols: size.cols,
          rows: size.rows,
          seq: this.terminalSeq++
        },
        true
      )
    )
  }

  private handleTerminalUnsubscribe(
    socket: AuthenticatedMobileSocket,
    request: RpcRequest,
    reply: Reply
  ): void {
    const subscriptionId = request.params?.subscriptionId
    if (typeof subscriptionId === 'string') {
      this.subscriptions.delete(`${socket.connectionId}:${subscriptionId}`)
    } else {
      for (const [key, subscription] of this.subscriptions) {
        if (subscription.connectionId === socket.connectionId) {
          this.subscriptions.delete(key)
        }
      }
    }
    this.send(reply, success(this.runtimeId, request.id, { unsubscribed: true }))
  }

  private handleTerminalSend(request: RpcRequest, reply: Reply): void {
    const params = request.params ?? {}
    this.applyViewport(params)
    let text = typeof params.text === 'string' ? params.text : ''
    if (params.interrupt === true) {
      text += '\u0003'
    }
    if (params.enter === true) {
      text += '\r'
    }
    if (Buffer.byteLength(text, 'utf8') > 64 * 1024) {
      this.send(reply, failure(this.runtimeId, request.id, 'invalid_argument', 'Input too large'))
      return
    }
    if (text) {
      this.terminal.write(text)
    }
    this.send(
      reply,
      success(this.runtimeId, request.id, {
        send: { handle: this.terminal.handle, accepted: true, bytesWritten: Buffer.byteLength(text) }
      })
    )
  }

  private applyViewport(params: Record<string, unknown>): void {
    const viewport = params.viewport
    if (!viewport || typeof viewport !== 'object') {
      return
    }
    const { cols, rows } = viewport as { cols?: unknown; rows?: unknown }
    if (typeof cols === 'number' && typeof rows === 'number' && Number.isFinite(cols) && Number.isFinite(rows)) {
      this.terminal.resize(cols, rows)
    }
  }

  private publishTerminalData(chunk: string): void {
    this.lastOutputAt = Date.now()
    for (const subscription of this.subscriptions.values()) {
      this.send(
        subscription.reply,
        success(
          this.runtimeId,
          subscription.requestId,
          { type: 'data', chunk, seq: this.terminalSeq++ },
          true
        )
      )
    }
  }

  private repoSummary() {
    return {
      id: this.repoId,
      displayName: basename(this.cwd) || 'pie-relay',
      path: this.cwd,
      badgeColor: '#2563eb',
      connectionId: null
    }
  }

  private worktreeSummary() {
    return {
      worktreeId: this.worktreeId,
      repoId: this.repoId,
      repo: basename(this.cwd) || 'pie-relay',
      path: this.cwd,
      branch: 'local',
      isArchived: false,
      isMainWorktree: true,
      hasHostSidebarActivity: true,
      parentWorktreeId: null,
      childWorktreeIds: [],
      displayName: 'Pie Relay terminal',
      workspaceStatus: 'in-progress',
      sortOrder: this.startedAt,
      linkedIssue: null,
      linkedPR: null,
      linkedLinearIssue: null,
      linkedGitLabMR: null,
      linkedGitLabIssue: null,
      comment: '',
      isPinned: true,
      isActive: true,
      unread: false,
      liveTerminalCount: 1,
      hasAttachedPty: true,
      lastOutputAt: this.lastOutputAt,
      preview: 'Pie Relay mobile terminal',
      status: 'active',
      agents: []
    }
  }

  private terminalSummary() {
    return {
      handle: this.terminal.handle,
      worktreeId: this.worktreeId,
      title: this.terminal.title,
      isActive: true,
      hasRunningProcess: true
    }
  }

  private sessionTabs() {
    return {
      worktree: this.worktreeId,
      snapshotVersion: this.sessionTabsVersion,
      tabs: [
        {
          type: 'terminal' as const,
          id: 'pie-relay-tab-1',
          title: this.terminal.title,
          status: 'ready' as const,
          terminal: this.terminal.handle,
          isActive: true
        }
      ],
      activeTabId: 'pie-relay-tab-1',
      activeTabType: 'terminal' as const
    }
  }
}
