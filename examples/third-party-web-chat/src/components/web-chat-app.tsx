'use client'

import {
  FormEvent,
  KeyboardEvent,
  useCallback,
  useEffect,
  useRef,
  useState,
} from 'react'
import { setNonce } from 'get-nonce'
import { BarChart3, Code2, ExternalLink, FileText, Globe2, LockKeyhole, MessageSquare, Paperclip, Plus, RefreshCw, RotateCcw, Send, Square, Trash2, X } from 'lucide-react'
import dynamic from 'next/dynamic'

import { MarkdownContent } from '@/components/markdown-content'
import { Button } from '@/components/ui/button'
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { filterAssistantMarkdown } from '@/lib/assistant-output.mjs'
import { WebChatAPIError, webChatAPI } from '@/lib/client-api'
import { stableTaskIdentity } from '@/lib/task-identity.mjs'
import type {
  ChatEvent,
  ChatItem,
  ChatMessage,
  ConnectionState,
  Conversation,
  ImageAttachment,
  PermissionMessage,
  Preview,
  PreviewLaunch,
  Project,
  ProjectApplication,
  RuntimeState,
  Session,
  TaskMessage,
  TaskToolMessage,
  TaskUsage,
  ThinkingMessage,
  ToolMessage,
  UsageEvent,
  UsageEventPage,
  UsageSummary,
  Workspace,
} from '@/lib/web-chat-types'

const MAX_IMAGE_COUNT = 4
const MAX_IMAGE_BYTES = 4 << 20
const MAX_IMAGES_TOTAL_BYTES = 4 << 20
const SUPPORTED_IMAGE_TYPES = new Set(['image/jpeg', 'image/png', 'image/gif', 'image/webp'])

// monaco-editor accesses browser globals while its module is evaluated.  A
// client component can still be pre-rendered by Next.js, so keep the complete
// editor module out of the server bundle instead of merely guarding render
// code with `typeof window`.
const WorkspaceEditor = dynamic(
  () => import('@/components/workspace-editor').then((module) => module.WorkspaceEditor),
  { ssr: false, loading: () => <div className="workspace-code panel">코드 편집기를 준비하는 중…</div> },
)

function normalizePreviewAppPath(value: string) {
  const trimmed = value.trim()
  if (!trimmed || trimmed === '.') return '.'
  if (trimmed.length > 512 || trimmed.startsWith('/') || trimmed.includes('\\') || /[\u0000-\u001f\u007f]/u.test(trimmed)) {
    throw new Error('앱 경로는 프로젝트 내부의 상대 경로로 입력해 주세요.')
  }
  const segments: string[] = []
  for (const segment of trimmed.split('/')) {
    if (!segment || segment === '.') continue
    if (segment === '..' || new TextEncoder().encode(segment).length > 255) {
      throw new Error('상위 폴더(..)로 이동하는 앱 경로는 사용할 수 없습니다.')
    }
    segments.push(segment)
  }
  return segments.join('/') || '.'
}

function findPreviewForApp(values: Preview[], appPath: string) {
  const matches = values.filter((preview) => (preview.appPath || '.') === appPath)
  return matches.find((preview) => !['stopped', 'failed'].includes(preview.status)) || matches[0]
}

function conversationConnectionStates(value: Conversation): { relay: ConnectionState; client: ConnectionState } {
  const connection = value.connection
  if (value.status === 'deleted' || connection.reason === 'deleted') {
    return {
      relay: { kind: 'error', text: 'Relay 대화 삭제됨' },
      client: { kind: 'error', text: 'Docker clientd 연결 종료됨' },
    }
  }
  if (value.status === 'closed' || connection.reason === 'idle_timeout' || connection.reason === 'closed') {
    return {
      relay: { kind: 'error', text: connection.reason === 'idle_timeout' ? 'Relay 유휴 세션 종료됨' : 'Relay 세션 종료됨' },
      client: { kind: 'error', text: 'Docker clientd 세션 종료됨' },
    }
  }
  const client: ConnectionState = connection.clientConnected && connection.relayRegistered
    ? { kind: 'online', text: 'Docker clientd 연결됨' }
    : !connection.runtimeRunning
      ? { kind: 'error', text: 'Docker 컨테이너 중지됨' }
      : !connection.runtimeHealthy
        ? { kind: 'error', text: 'Docker 컨테이너 상태 이상' }
        : connection.reason === 'client_reconnecting'
          ? { kind: 'busy', text: 'Docker clientd 재연결 중' }
          : { kind: 'busy', text: 'Docker clientd 연결 대기 중' }

  if (value.status === 'ready' && connection.relayAvailable && connection.clientConnected && connection.relayRegistered) {
    return { relay: { kind: 'online', text: 'Pie Relay 세션 연결됨' }, client }
  }
  if (connection.reason === 'relay_unavailable') {
    return { relay: { kind: 'error', text: 'Pie Relay 응답 없음' }, client }
  }
  if (connection.reason === 'relay_unregistered') {
    return { relay: { kind: 'busy', text: 'Pie Relay 등록 대기 중' }, client }
  }
  return {
    relay: { kind: 'busy', text: value.status === 'reconnecting' ? 'Pie Relay 재연결 중' : 'Pie Relay 세션 준비 중' },
    client,
  }
}

function conversationNeedsReconnect(value: Conversation | null) {
  if (!value || value.status === 'deleted') return false
  return ['error', 'closed'].includes(value.status)
    || (value.status === 'ready' && value.connection.reason !== 'connected')
}

function stringValue(value: unknown) {
  return typeof value === 'string' ? value : ''
}

function taskUsage(value: unknown): TaskUsage | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined
  const usage = value as Record<string, unknown>
  const number = (candidate: unknown) => typeof candidate === 'number' && Number.isFinite(candidate) && candidate >= 0 ? candidate : 0
  return {
    totalTokens: number(usage.total_tokens ?? usage.totalTokens),
    toolUses: number(usage.tool_uses ?? usage.toolUses),
    durationMs: number(usage.duration_ms ?? usage.durationMs),
  }
}

function taskIdentity(event: ChatEvent, data: Record<string, unknown>, activeRequestID: string | null) {
  const requestID = event.requestId || activeRequestID || ''
  return { requestID, ...stableTaskIdentity(data, event.sequence, crypto.randomUUID()) }
}

function newTaskMessage(identity: ReturnType<typeof taskIdentity>, data: Record<string, unknown>): TaskMessage {
  return {
    kind: 'task', id: identity.id, taskId: identity.taskID,
    parentToolUseId: identity.parentToolUseID || undefined, requestId: identity.requestID,
    eventType: 'task_started', subagentType: stringValue(data.subagentType) || undefined,
    taskType: stringValue(data.taskType) || undefined,
    description: stringValue(data.taskDescription) || stringValue(data.description),
    summary: '', text: '', thinking: '', tools: [], data: { ...data }, state: 'running',
  }
}

function updateTaskItem(
  current: ChatItem[], identity: ReturnType<typeof taskIdentity>, data: Record<string, unknown>,
  updater: (task: TaskMessage) => TaskMessage,
) {
  const index = current.findIndex((item) => item.kind === 'task' && item.id === identity.id)
  const previous = index >= 0 ? current[index] as TaskMessage : newTaskMessage(identity, data)
  const task = updater({
    ...previous,
    taskId: identity.taskID,
    parentToolUseId: identity.parentToolUseID || previous.parentToolUseId,
    requestId: identity.requestID || previous.requestId,
  })
  if (index < 0) return [...current, task]
  const next = [...current]
  next[index] = task
  return next
}

export function WebChatApp({ cspNonce, registrationEnabled }: { cspNonce: string; registrationEnabled: boolean }) {
  const [authMode, setAuthMode] = useState<'login' | 'signup'>('login')
  const [session, setSession] = useState<Session | null>(null)
  const [appVisible, setAppVisible] = useState(false)
  const [workspace, setWorkspace] = useState<Workspace | null>(null)
  const [projects, setProjects] = useState<Project[]>([])
  const [selectedProject, setSelectedProject] = useState<Project | null>(null)
  const [projectApplications, setProjectApplications] = useState<ProjectApplication[]>([])
  const [projectApplicationsLoading, setProjectApplicationsLoading] = useState(false)
  const [previews, setPreviews] = useState<Preview[]>([])
  const [previewAppPath, setPreviewAppPath] = useState('')
  const [previewVisibility, setPreviewVisibility] = useState<'private' | 'public'>('private')
  const [previewPending, setPreviewPending] = useState('')
  const [previewError, setPreviewError] = useState('')
  const [previewLogs, setPreviewLogs] = useState('')
  const [previewLogsOpen, setPreviewLogsOpen] = useState(false)
  const [previewDeleteTarget, setPreviewDeleteTarget] = useState<Preview | null>(null)
  const [conversation, setConversation] = useState<Conversation | null>(null)
  const [items, setItems] = useState<ChatItem[]>([])
  const [attachments, setAttachments] = useState<ImageAttachment[]>([])
  const [draftPrompt, setDraftPrompt] = useState('')
  const [activeTurn, setActiveTurn] = useState(false)
  const [relayState, setRelayState] = useState<ConnectionState>({ kind: '', text: 'Relay 연결 대기' })
  const [clientState, setClientState] = useState<ConnectionState>({ kind: '', text: 'clientd 연결 대기' })
  const [streamState, setStreamState] = useState<ConnectionState>({ kind: '', text: '실시간 대기' })
  const [loginError, setLoginError] = useState('')
  const [signupError, setSignupError] = useState('')
  const [chatError, setChatError] = useState('')
  const [projectError, setProjectError] = useState('')
  const [loginPending, setLoginPending] = useState(false)
  const [signupPending, setSignupPending] = useState(false)
  const [provisionPending, setProvisionPending] = useState(false)
  const [projectPending, setProjectPending] = useState(false)
  const [retryPending, setRetryPending] = useState(false)
  const [streamRetryPending, setStreamRetryPending] = useState(false)
  const [projectDialogOpen, setProjectDialogOpen] = useState(false)
  const [projectLocale, setProjectLocale] = useState('ko')
  const [usageOpen, setUsageOpen] = useState(false)
  const [usageDays, setUsageDays] = useState<'7' | '30' | '90'>('30')
  const [usageSummary, setUsageSummary] = useState<UsageSummary | null>(null)
  const [usageItems, setUsageItems] = useState<UsageEvent[]>([])
  const [usageNextCursor, setUsageNextCursor] = useState('')
  const [usageLoading, setUsageLoading] = useState(false)
  const [usageLoadingMore, setUsageLoadingMore] = useState(false)
  const [usageError, setUsageError] = useState('')
  const [mainView, setMainView] = useState<'chat' | 'code'>('chat')
  const [workspaceRefreshVersion, setWorkspaceRefreshVersion] = useState(0)

  const initialized = useRef(false)
  const mounted = useRef(true)
  const usageLoadVersion = useRef(0)
  const csrfToken = useRef('')
  const conversationRef = useRef<Conversation | null>(null)
  const eventSource = useRef<EventSource | null>(null)
  const streamRetryTimer = useRef<number | null>(null)
  const streamHydrating = useRef(false)
  const automaticRecoveryAttempt = useRef('')
  const conversationGeneration = useRef(0)
  const projectApplicationsGeneration = useRef(0)
  const lastSequence = useRef(0)
  const knownRequestIDs = useRef(new Set<string>())
  const activeRequestID = useRef<string | null>(null)
  const runningToolMessageIDs = useRef<string[]>([])
  const toolMessageIDsByCallID = useRef(new Map<string, string>())
  const toolMessageSequence = useRef(0)
  const thinkingMessageID = useRef<string | null>(null)
  const thinkingText = useRef('')
  const thinkingRenderFrame = useRef<number | null>(null)
  const assistantMessageID = useRef<string | null>(null)
  const assistantMarkdown = useRef('')
  const assistantRenderFrame = useRef<number | null>(null)
  const taskDeltaBuffers = useRef(new Map<string, {
    identity: ReturnType<typeof taskIdentity>
    data: Record<string, unknown>
    text: string
    thinking: string
  }>())
  const taskRenderFrame = useRef<number | null>(null)
  const permissionRequests = useRef(new Set<string>())
  const promptInput = useRef<HTMLTextAreaElement>(null)
  const attachmentInput = useRef<HTMLInputElement>(null)
  const messagesElement = useRef<HTMLDivElement>(null)
  const projectForm = useRef<HTMLFormElement>(null)
  const unauthorizedHandler = useRef<(() => void) | null>(null)
  const lastStreamAuthProbeAt = useRef(0)
  const conversationSyncInFlight = useRef<{ conversationID: string; promise: Promise<Conversation> } | null>(null)

  useEffect(() => {
    setNonce(cspNonce)
  }, [cspNonce])

  const request = useCallback(<T,>(path: string, options: { method?: string; body?: unknown; csrf?: boolean; signal?: AbortSignal } = {}) => (
    webChatAPI<T>(path, {
      method: options.method,
      body: options.body,
      csrfToken: options.csrf === false ? '' : csrfToken.current,
      signal: options.signal,
    }).catch((error) => {
      if (error instanceof WebChatAPIError && error.status === 401 && csrfToken.current) {
        unauthorizedHandler.current?.()
      }
      throw error
    })
  ), [])

  const setCurrentConversation = useCallback((value: Conversation | null) => {
    conversationRef.current = value
    setConversation(value)
  }, [])

  const closeEvents = useCallback(() => {
    if (streamRetryTimer.current !== null) window.clearTimeout(streamRetryTimer.current)
    streamRetryTimer.current = null
    streamHydrating.current = false
    eventSource.current?.close()
    eventSource.current = null
  }, [])

  const clearAttachments = useCallback(() => {
    setAttachments([])
    if (attachmentInput.current) attachmentInput.current.value = ''
  }, [])

  const clearMessages = useCallback(() => {
    setItems([])
    lastSequence.current = 0
    knownRequestIDs.current.clear()
    activeRequestID.current = null
    runningToolMessageIDs.current = []
    toolMessageIDsByCallID.current.clear()
    toolMessageSequence.current = 0
    if (thinkingRenderFrame.current !== null) cancelAnimationFrame(thinkingRenderFrame.current)
    thinkingRenderFrame.current = null
    thinkingMessageID.current = null
    thinkingText.current = ''
    if (assistantRenderFrame.current !== null) cancelAnimationFrame(assistantRenderFrame.current)
    assistantRenderFrame.current = null
    assistantMessageID.current = null
    assistantMarkdown.current = ''
    if (taskRenderFrame.current !== null) cancelAnimationFrame(taskRenderFrame.current)
    taskRenderFrame.current = null
    taskDeltaBuffers.current.clear()
    permissionRequests.current.clear()
    setActiveTurn(false)
    clearAttachments()
  }, [clearAttachments])

  const clearApplicationState = useCallback(() => {
    ++conversationGeneration.current
    ++projectApplicationsGeneration.current
    closeEvents()
    csrfToken.current = ''
    lastStreamAuthProbeAt.current = 0
    conversationSyncInFlight.current = null
    automaticRecoveryAttempt.current = ''
    setSession(null)
    setWorkspace(null)
    setProjects([])
    setSelectedProject(null)
    setProjectApplications([])
    setProjectApplicationsLoading(false)
    setPreviews([])
    setPreviewAppPath('')
    setPreviewError('')
    setPreviewPending('')
    setPreviewDeleteTarget(null)
    setDraftPrompt('')
    setCurrentConversation(null)
    localStorage.removeItem('pie-demo-project')
    localStorage.removeItem('pie-demo-conversation')
    clearMessages()
    setAppVisible(false)
    setRetryPending(false)
    setStreamRetryPending(false)
    setRelayState({ kind: '', text: 'Relay 연결 대기' })
    setClientState({ kind: '', text: 'clientd 연결 대기' })
    setStreamState({ kind: '', text: '실시간 대기' })
    setMainView('chat')
    setWorkspaceRefreshVersion(0)
  }, [clearMessages, closeEvents, setCurrentConversation])

  const expireBrowserSession = useCallback(() => {
    clearApplicationState()
    setAuthMode('login')
    setSignupError('')
    setLoginError('로그인 세션이 만료되었습니다. 다시 로그인해 주세요.')
  }, [clearApplicationState])

  useEffect(() => {
    unauthorizedHandler.current = expireBrowserSession
    return () => { unauthorizedHandler.current = null }
  }, [expireBrowserSession])

  const applyConversationSnapshot = useCallback((current: Conversation) => {
    if (!mounted.current || (conversationRef.current && conversationRef.current.id !== current.id)) return
    setCurrentConversation(current)
    const states = conversationConnectionStates(current)
    setRelayState(states.relay)
    setClientState(states.client)
    if (current.status !== 'closed' && current.status !== 'deleted') return

    closeEvents()
    setStreamState({
      kind: 'error',
      text: current.status === 'deleted' || current.connection.reason === 'deleted'
        ? '대화가 삭제되어 실시간 수신 종료됨'
        : current.connection.reason === 'idle_timeout'
          ? '15분 동안 사용하지 않아 대화 세션 종료됨'
          : '대화 세션이 종료되어 실시간 수신 종료됨',
    })
    setActiveTurn(false)
    if (current.status === 'deleted' || current.connection.reason === 'deleted') {
      setChatError('대화가 삭제되었습니다. 새 대화를 시작해 주세요.')
      return
    }
    setChatError(current.connection.reason === 'idle_timeout'
      ? '오랫동안 사용하지 않아 Claude 세션을 정리했습니다. 다시 연결하면 같은 대화를 계속 사용할 수 있습니다.'
      : current.lastError || current.connection.lastError || 'Claude 세션이 종료되었습니다. 다시 연결해 주세요.')
  }, [closeEvents, setCurrentConversation])

  const refreshConversationState = useCallback(async (conversationID: string) => {
    const existing = conversationSyncInFlight.current
    const promise = existing?.conversationID === conversationID
      ? existing.promise
      : request<Conversation>(`/api/conversations/${encodeURIComponent(conversationID)}`)
    if (promise !== existing?.promise) conversationSyncInFlight.current = { conversationID, promise }
    try {
      const current = await promise
      if (!mounted.current || conversationRef.current?.id !== conversationID) return current
      applyConversationSnapshot(current)
      return current
    } finally {
      if (conversationSyncInFlight.current?.promise === promise) conversationSyncInFlight.current = null
    }
  }, [applyConversationSnapshot, request])

  const setAssistantText = useCallback((text: string, pending: boolean) => {
    let id = assistantMessageID.current
    if (!id) {
      id = crypto.randomUUID()
      assistantMessageID.current = id
      const message: ChatMessage = { kind: 'message', id, role: 'assistant', text, pending, attachments: [] }
      setItems((current) => [...current, message])
      return
    }
    setItems((current) => current.map((item) => item.kind === 'message' && item.id === id
      ? { ...item, text, pending }
      : item))
  }, [])

  const flushAssistantText = useCallback((pending: boolean) => {
    if (assistantRenderFrame.current !== null) {
      cancelAnimationFrame(assistantRenderFrame.current)
      assistantRenderFrame.current = null
    }
    const visibleMarkdown = filterAssistantMarkdown(assistantMarkdown.current)
    if (assistantMessageID.current || visibleMarkdown) setAssistantText(visibleMarkdown, pending)
  }, [setAssistantText])

  const scheduleAssistantText = useCallback(() => {
    if (assistantRenderFrame.current !== null) return
    assistantRenderFrame.current = requestAnimationFrame(() => {
      assistantRenderFrame.current = null
      const visibleMarkdown = filterAssistantMarkdown(assistantMarkdown.current)
      if (assistantMessageID.current || visibleMarkdown) setAssistantText(visibleMarkdown, true)
    })
  }, [setAssistantText])

  const finishAssistantSegment = useCallback(() => {
    flushAssistantText(false)
    assistantMessageID.current = null
    assistantMarkdown.current = ''
  }, [flushAssistantText])

  const setThinkingText = useCallback((text: string, pending: boolean) => {
    let id = thinkingMessageID.current
    if (!id) {
      id = crypto.randomUUID()
      thinkingMessageID.current = id
      const message: ThinkingMessage = { kind: 'thinking', id, text, pending }
      setItems((current) => [...current, message])
      return
    }
    setItems((current) => current.map((item) => item.kind === 'thinking' && item.id === id
      ? { ...item, text, pending }
      : item))
  }, [])

  const flushThinkingText = useCallback((pending: boolean) => {
    if (thinkingRenderFrame.current !== null) {
      cancelAnimationFrame(thinkingRenderFrame.current)
      thinkingRenderFrame.current = null
    }
    if (thinkingMessageID.current || thinkingText.current) setThinkingText(thinkingText.current, pending)
  }, [setThinkingText])

  const scheduleThinkingText = useCallback(() => {
    if (thinkingRenderFrame.current !== null) return
    thinkingRenderFrame.current = requestAnimationFrame(() => {
      thinkingRenderFrame.current = null
      setThinkingText(thinkingText.current, true)
    })
  }, [setThinkingText])

  const finishThinkingSegment = useCallback(() => {
    flushThinkingText(false)
    thinkingMessageID.current = null
    thinkingText.current = ''
  }, [flushThinkingText])

  const flushTaskDeltas = useCallback(() => {
    if (taskRenderFrame.current !== null) {
      cancelAnimationFrame(taskRenderFrame.current)
      taskRenderFrame.current = null
    }
    if (taskDeltaBuffers.current.size === 0) return
    const buffered = [...taskDeltaBuffers.current.values()]
    taskDeltaBuffers.current.clear()
    setItems((current) => buffered.reduce((next, delta) => updateTaskItem(next, delta.identity, delta.data, (task) => ({
      ...task,
      subagentType: stringValue(delta.data.subagentType) || task.subagentType,
      taskType: stringValue(delta.data.taskType) || task.taskType,
      description: stringValue(delta.data.taskDescription) || task.description,
      text: task.text + delta.text,
      thinking: task.thinking + delta.thinking,
      data: { ...task.data, ...delta.data },
      state: 'running',
    })), current))
  }, [])

  const scheduleTaskDelta = useCallback((event: ChatEvent, data: Record<string, unknown>, kind: 'text' | 'thinking', delta: string) => {
    if (!delta) return
    const identity = taskIdentity(event, data, activeRequestID.current)
    const buffered = taskDeltaBuffers.current.get(identity.id) ?? { identity, data: {}, text: '', thinking: '' }
    buffered.identity = identity
    buffered.data = { ...buffered.data, ...data }
    buffered[kind] += delta
    taskDeltaBuffers.current.set(identity.id, buffered)
    if (taskRenderFrame.current !== null) return
    taskRenderFrame.current = requestAnimationFrame(() => {
      taskRenderFrame.current = null
      flushTaskDeltas()
    })
  }, [flushTaskDeltas])

  const finishTurn = useCallback((suffix = '', state: RuntimeState = 'complete') => {
    const finishingRequestID = activeRequestID.current
    if (suffix) {
      assistantMarkdown.current += suffix
      const visibleMarkdown = filterAssistantMarkdown(assistantMarkdown.current)
      if (!assistantMessageID.current && visibleMarkdown) setAssistantText(visibleMarkdown, true)
    }
    flushAssistantText(false)
    flushThinkingText(false)
    setItems((current) => current.map((item) => item.kind === 'permission' && ['pending', 'sending'].includes(item.state)
      ? { ...item, state: 'expired', statusText: '요청이 종료되어 더 이상 응답할 수 없습니다.' }
      : item.kind === 'tool' && item.state === 'running'
        ? { ...item, state }
        : item.kind === 'task' && item.state === 'running' && state !== 'complete' && (!finishingRequestID || item.requestId === finishingRequestID)
          ? { ...item, state, status: state }
        : item.kind === 'thinking' && item.pending
          ? { ...item, pending: false }
        : item))
    permissionRequests.current.clear()
    activeRequestID.current = null
    runningToolMessageIDs.current = []
    toolMessageIDsByCallID.current.clear()
    assistantMessageID.current = null
    assistantMarkdown.current = ''
    thinkingMessageID.current = null
    thinkingText.current = ''
    setActiveTurn(false)
  }, [flushAssistantText, flushThinkingText, setAssistantText])

  const resolvePermission = useCallback((requestID: string, allow: boolean) => {
    permissionRequests.current.delete(requestID)
    setItems((current) => current.map((item) => item.kind === 'permission' && item.requestId === requestID
      ? {
          ...item,
          state: allow ? 'approved' : 'denied',
          statusText: allow
            ? '승인됨 · Claude가 작업을 계속합니다.'
            : '거절됨 · Claude에게 거절 결과를 전달했습니다.',
        }
      : item))
  }, [])

  const handleChatEvent = useCallback((event: ChatEvent) => {
    const data = event.data || {}
    if (event.type === 'request.accepted') {
      const requestID = event.requestId || `sequence-${event.sequence || crypto.randomUUID()}`
      activeRequestID.current = requestID
      if (!knownRequestIDs.current.has(requestID)) {
        knownRequestIDs.current.add(requestID)
        const prompt = typeof data.prompt === 'string' ? data.prompt : ''
        if (prompt) {
          setItems((current) => [...current, {
            kind: 'message', id: `request-${requestID}`, role: 'user', text: prompt, pending: false, attachments: [],
          }])
        }
      }
      setActiveTurn(true)
    } else if (event.type === 'text') {
      finishThinkingSegment()
      assistantMarkdown.current += typeof data.text === 'string' ? data.text : ''
      scheduleAssistantText()
    } else if (event.type === 'thinking') {
      finishAssistantSegment()
      thinkingText.current += typeof data.text === 'string' ? data.text : ''
      scheduleThinkingText()
      setActiveTurn(true)
    } else if (event.type === 'task_started' || event.type === 'task_progress' || event.type === 'task_complete') {
      flushTaskDeltas()
      finishAssistantSegment()
      finishThinkingSegment()
      const eventType = event.type as TaskMessage['eventType']
      const identity = taskIdentity(event, data, activeRequestID.current)
      const state: RuntimeState = eventType === 'task_complete'
        ? runtimeStateFromStatus(data.status)
        : 'running'
      setItems((current) => updateTaskItem(current, identity, data, (task) => ({
        ...task,
        parentToolUseId: identity.parentToolUseID || task.parentToolUseId,
        eventType,
        subagentType: stringValue(data.subagentType) || task.subagentType,
        taskType: stringValue(data.taskType) || task.taskType,
        description: stringValue(data.taskDescription) || stringValue(data.description) || task.description,
        summary: stringValue(data.summary) || task.summary,
        usage: taskUsage(data.usage) || task.usage,
        lastToolName: stringValue(data.lastToolName) || task.lastToolName,
        outputFile: stringValue(data.outputFile) || task.outputFile,
        status: stringValue(data.status) || task.status,
        data: { ...task.data, ...data }, state,
      })))
    } else if (event.type === 'subagent_text' || event.type === 'subagent_thinking') {
      scheduleTaskDelta(event, data, event.type === 'subagent_text' ? 'text' : 'thinking', stringValue(data.text))
    } else if (event.type === 'subagent_tool_call') {
      flushTaskDeltas()
      const identity = taskIdentity(event, data, activeRequestID.current)
      const toolCallID = stringValue(data.toolCallId) || undefined
      setItems((current) => updateTaskItem(current, identity, data, (task) => {
        const tool: TaskToolMessage = {
          id: toolCallID || `subagent-tool-${event.sequence || crypto.randomUUID()}`,
          toolCallId: toolCallID, name: stringValue(data.name) || 'tool_call',
          input: data.input, status: stringValue(data.status) || undefined, state: 'running',
        }
        const index = toolCallID ? task.tools.findIndex((candidate) => candidate.toolCallId === toolCallID) : -1
        const tools = index < 0 ? [...task.tools, tool] : task.tools.map((candidate, itemIndex) => itemIndex === index ? { ...candidate, ...tool } : candidate)
        return {
          ...task, parentToolUseId: identity.parentToolUseID || task.parentToolUseId,
          subagentType: stringValue(data.subagentType) || task.subagentType,
          description: stringValue(data.taskDescription) || task.description,
          tools, lastToolName: tool.name, data: { ...task.data, ...data }, state: 'running',
        }
      }))
    } else if (event.type === 'subagent_tool_result') {
      flushTaskDeltas()
      const identity = taskIdentity(event, data, activeRequestID.current)
      const toolCallID = stringValue(data.toolCallId) || undefined
      const status = stringValue(data.status) || (data.isError === true ? 'error' : 'complete')
      const result = Object.prototype.hasOwnProperty.call(data, 'content') ? data.content : data
      setItems((current) => updateTaskItem(current, identity, data, (task) => {
        const index = toolCallID ? task.tools.findIndex((candidate) => candidate.toolCallId === toolCallID) : task.tools.findIndex((candidate) => candidate.state === 'running')
        const completed: TaskToolMessage = index >= 0
          ? { ...task.tools[index], result, status, state: runtimeStateFromStatus(status) }
          : { id: toolCallID || `subagent-result-${event.sequence || crypto.randomUUID()}`, toolCallId: toolCallID, name: stringValue(data.name) || 'tool_result', result, status, state: runtimeStateFromStatus(status) }
        const tools = index < 0 ? [...task.tools, completed] : task.tools.map((candidate, itemIndex) => itemIndex === index ? completed : candidate)
        return { ...task, tools, data: { ...task.data, ...data }, state: 'running' }
      }))
    } else if (event.type === 'subagent_tool_progress') {
      flushTaskDeltas()
      const identity = taskIdentity(event, data, activeRequestID.current)
      const toolCallID = stringValue(data.toolCallId) || undefined
      const elapsedSeconds = typeof data.elapsedSeconds === 'number' && Number.isFinite(data.elapsedSeconds) ? data.elapsedSeconds : undefined
      setItems((current) => updateTaskItem(current, identity, data, (task) => {
        const index = toolCallID ? task.tools.findIndex((candidate) => candidate.toolCallId === toolCallID) : -1
        const progress: TaskToolMessage = index >= 0
          ? { ...task.tools[index], elapsedSeconds, retry: data.retry, status: 'running', state: 'running' }
          : { id: toolCallID || `subagent-progress-${event.sequence || crypto.randomUUID()}`, toolCallId: toolCallID, name: stringValue(data.name) || 'tool', elapsedSeconds, retry: data.retry, status: 'running', state: 'running' }
        const tools = index < 0 ? [...task.tools, progress] : task.tools.map((candidate, itemIndex) => itemIndex === index ? progress : candidate)
        return { ...task, tools, lastToolName: progress.name, state: 'running' }
      }))
    } else if (event.type === 'tool_call') {
      finishAssistantSegment()
      finishThinkingSegment()
      const toolCallID = typeof data.toolCallId === 'string' ? data.toolCallId : undefined
      const id = `tool-${activeRequestID.current || 'unknown'}-${++toolMessageSequence.current}`
      const tool: ToolMessage = {
        kind: 'tool', id, requestId: activeRequestID.current || '', toolCallId: toolCallID,
        name: typeof data.name === 'string' ? data.name : 'tool_call',
        input: data.input,
        status: typeof data.status === 'string' ? data.status : undefined,
        state: 'running',
      }
      runningToolMessageIDs.current.push(id)
      if (toolCallID) toolMessageIDsByCallID.current.set(toolCallID, id)
      setItems((current) => [...current, tool])
    } else if (event.type === 'tool_result') {
      finishAssistantSegment()
      finishThinkingSegment()
      const toolCallID = typeof data.toolCallId === 'string' ? data.toolCallId : undefined
      const matchedID = toolCallID
        ? toolMessageIDsByCallID.current.get(toolCallID)
        : runningToolMessageIDs.current[0]
      const status = typeof data.status === 'string' ? data.status : data.isError === true ? 'error' : undefined
      const result = Object.prototype.hasOwnProperty.call(data, 'content') ? data.content : data
      if (matchedID) {
        runningToolMessageIDs.current = runningToolMessageIDs.current.filter((id) => id !== matchedID)
        if (toolCallID) toolMessageIDsByCallID.current.delete(toolCallID)
        setItems((current) => current.map((item) => item.kind === 'tool' && item.id === matchedID
          ? { ...item, result, status, state: runtimeStateFromStatus(status) }
          : item))
      } else {
        const tool: ToolMessage = {
          kind: 'tool', id: `tool-result-${event.sequence || crypto.randomUUID()}`,
          requestId: activeRequestID.current || '', toolCallId: toolCallID,
          name: typeof data.name === 'string' ? data.name : 'tool_result',
          result, status, state: runtimeStateFromStatus(status),
        }
        setItems((current) => [...current, tool])
      }
    } else if (event.type === 'permission_request') {
      finishAssistantSegment()
      finishThinkingSegment()
      const requestID = typeof data.requestId === 'string' ? data.requestId : ''
      if (!requestID || permissionRequests.current.has(requestID)) return
      permissionRequests.current.add(requestID)
      const permission: PermissionMessage = {
        kind: 'permission',
        id: `permission-${requestID}`,
        requestId: requestID,
        toolName: typeof data.toolName === 'string' ? data.toolName : '도구',
        detail: permissionDetail(data.input),
        state: 'pending',
        statusText: '이 요청을 승인하거나 거절해야 대화가 계속됩니다.',
      }
      setItems((current) => [...current, permission])
      setActiveTurn(true)
    } else if (event.type === 'control.accepted' && data.type === 'permission_response' && typeof data.requestId === 'string') {
      resolvePermission(data.requestId, data.allow === true)
    } else if (event.type === 'task_timeout') {
      flushTaskDeltas()
      const message = stringValue(data.message) || '서브에이전트 작업이 제한 시간 안에 끝나지 않았습니다.'
      setItems((current) => current.map((item) => item.kind === 'task' && item.state === 'running'
        ? { ...item, state: 'error', status: 'timeout', summary: message }
        : item))
      setChatError(message)
    } else if (event.type === 'error') {
      const message = typeof data.message === 'string' ? data.message : 'Claude 실행 오류'
      if (!assistantMessageID.current) setAssistantText('', true)
      finishTurn(`\n\n오류: ${message}`, 'error')
      setWorkspaceRefreshVersion((current) => current + 1)
    } else if (event.type === 'aborted') {
      finishTurn(assistantMessageID.current ? '\n\n응답이 중단되었습니다.' : '', 'cancelled')
      setWorkspaceRefreshVersion((current) => current + 1)
    } else if (event.type === 'done') {
      finishTurn()
      setWorkspaceRefreshVersion((current) => current + 1)
    } else if (event.type === 'request.completed' && activeRequestID.current === event.requestId) {
      finishTurn()
    } else if (event.type === 'transport.reconnecting') {
      setRelayState({ kind: 'busy', text: 'Pie Relay 재연결 중' })
      setClientState({ kind: 'busy', text: 'Docker clientd 재연결 중' })
    } else if (event.type === 'transport.connected') {
      const current = conversationRef.current
      if (!current) return
      void refreshConversationState(current.id)
        .then((refreshed) => { if (refreshed?.status === 'ready') setChatError('') })
        .catch(() => setRelayState({ kind: 'busy', text: 'Pie Relay 상태 확인 중' }))
    } else if (event.type === 'conversation.idle') {
      // An idle marker is retained in the append-only journal. During initial
      // replay it describes a previous lifecycle, not necessarily the session
      // that was just reopened. replay_complete refreshes the authoritative
      // conversation snapshot before the browser is declared live.
      if (streamHydrating.current) return
      const current = conversationRef.current
      if (current) applyConversationSnapshot({
        ...current,
        status: 'closed',
        lastError: 'idle timeout',
        connection: { ...current.connection, reason: 'idle_timeout', sessionStatus: 'closed', clientConnected: false, relayRegistered: false },
      })
    }
  }, [applyConversationSnapshot, finishAssistantSegment, finishThinkingSegment, finishTurn, flushTaskDeltas, refreshConversationState, resolvePermission, scheduleAssistantText, scheduleTaskDelta, scheduleThinkingText, setAssistantText])

  const connectEvents = useCallback((conversationID: string) => {
    closeEvents()
    streamHydrating.current = true
    setStreamRetryPending(true)
    setStreamState({ kind: 'busy', text: '브라우저 연결 중' })
    const after = lastSequence.current > 0 ? `?after=${lastSequence.current}` : ''
    const source = new EventSource(`/api/conversations/${encodeURIComponent(conversationID)}/events${after}`)
    eventSource.current = source
    const armManualRetry = (restart = false) => {
      if (restart && streamRetryTimer.current !== null) {
        window.clearTimeout(streamRetryTimer.current)
        streamRetryTimer.current = null
      }
      if (streamRetryTimer.current !== null) return
      streamRetryTimer.current = window.setTimeout(() => {
        streamRetryTimer.current = null
        if (eventSource.current === source && (source.readyState !== EventSource.OPEN || streamHydrating.current)) setStreamRetryPending(false)
      }, 15_000)
    }
    armManualRetry()
    source.onopen = () => {
      if (eventSource.current !== source) return
      lastStreamAuthProbeAt.current = 0
      setStreamState({ kind: 'busy', text: '브라우저 연결됨 · 메시지 가져오는 중' })
      armManualRetry(true)
    }
    source.onmessage = (message) => {
      if (eventSource.current !== source) return
      if (streamHydrating.current) armManualRetry(true)
      let event: ChatEvent
      try { event = JSON.parse(message.data) } catch { return }
      if (event.sequence !== undefined) {
        const sequence = Number(event.sequence)
        if (Number.isFinite(sequence)) {
          if (sequence <= lastSequence.current) return
          lastSequence.current = sequence
        }
      }
      handleChatEvent(event)
    }
    source.addEventListener('replay_complete', (message) => {
      if (eventSource.current !== source) return
      try {
        const replay = JSON.parse((message as MessageEvent).data)
        const sequence = Number(replay?.lastSequence)
        if (Number.isFinite(sequence) && sequence > lastSequence.current) lastSequence.current = sequence
      } catch { /* the completion signal itself is sufficient */ }
      if (streamRetryTimer.current !== null) window.clearTimeout(streamRetryTimer.current)
      streamRetryTimer.current = null
      setStreamRetryPending(true)
      void refreshConversationState(conversationID)
        .then((snapshot) => {
          if (eventSource.current !== source || !snapshot || ['closed', 'deleted'].includes(snapshot.status)) return
          streamHydrating.current = false
          setStreamRetryPending(false)
          setStreamState({ kind: 'online', text: '브라우저 실시간 수신 중' })
        })
        .catch(() => {
          if (eventSource.current !== source) return
          setStreamState({ kind: 'busy', text: '최신 대화 상태 확인 중' })
          armManualRetry()
        })
    })
    source.addEventListener('heartbeat', () => {
      if (eventSource.current !== source) return
      if (!streamHydrating.current) setStreamState({ kind: 'online', text: '브라우저 실시간 수신 중' })
      void refreshConversationState(conversationID).catch(() => {
        if (eventSource.current === source) setRelayState({ kind: 'busy', text: 'Pie Relay 상태 재확인 중' })
      })
    })
    source.onerror = () => {
      if (eventSource.current !== source) return
      streamHydrating.current = true
      setStreamRetryPending(true)
      setStreamState({ kind: 'busy', text: '브라우저 연결 끊김 · 자동 재연결 중' })
      armManualRetry()
      const now = Date.now()
      if (now - lastStreamAuthProbeAt.current < 5_000) return
      lastStreamAuthProbeAt.current = now
      void refreshConversationState(conversationID).catch(() => {
        /* request() logs out an expired session; EventSource keeps its native retry policy for transient failures. */
      })
    }
  }, [closeEvents, handleChatEvent, refreshConversationState])

  const watchConversationReady = useCallback(async (conversationID: string, generation: number) => {
    let attempt = 0
    while (mounted.current && generation === conversationGeneration.current) {
      try {
        const current = await request<Conversation>(`/api/conversations/${encodeURIComponent(conversationID)}`)
        if (!mounted.current || generation !== conversationGeneration.current) return
        applyConversationSnapshot(current)
        if (current.status === 'ready') {
          setChatError('')
          requestAnimationFrame(() => promptInput.current?.focus())
          return
        }
        if (current.status === 'closed' || current.status === 'deleted') {
          return
        }
        if (current.status === 'error') {
          setChatError(current.lastError || '컨테이너 연결을 자동으로 복구하고 있습니다.')
        }
      } catch (error) {
        if (!mounted.current || generation !== conversationGeneration.current) return
        setChatError(`연결 상태를 확인하지 못했습니다. 자동으로 다시 확인합니다: ${errorMessage(error)}`)
        setRelayState({ kind: 'busy', text: 'Pie Relay 상태 확인 재시도 중' })
        setClientState({ kind: 'busy', text: 'Docker clientd 상태 확인 중' })
      }
      attempt += 1
      await delay(attempt < 90 ? 1000 : 5000)
    }
  }, [applyConversationSnapshot, request])

  const ensureConversation = useCallback(async (project: Project, forceNew: boolean) => {
    const generation = ++conversationGeneration.current
    const previousConversation = conversationRef.current
    closeEvents()
    setCurrentConversation(null)
    clearMessages()
    setRelayState({ kind: 'busy', text: 'Pie Relay 세션 준비 중' })
    setClientState({ kind: 'busy', text: 'Docker clientd 연결 준비 중' })
    let nextConversation: Conversation | null = null
    const storageKey = `pie-demo-conversation:${project.id}`
    if (previousConversation && (forceNew || previousConversation.projectId !== project.id)) {
      try {
        await request(`/api/conversations/${encodeURIComponent(previousConversation.id)}`, { method: 'DELETE' })
      } catch (error) {
        if (!(typeof error === 'object' && error && 'status' in error && error.status === 404)) throw error
      }
      localStorage.removeItem(`pie-demo-conversation:${previousConversation.projectId}`)
    }
    const storedID = !forceNew ? localStorage.getItem(storageKey) : ''
    if (storedID) {
      try {
        nextConversation = await request<Conversation>(`/api/conversations/${encodeURIComponent(storedID)}`)
        if (nextConversation.projectId !== project.id) nextConversation = null
      } catch { /* a replacement is created below */ }
      if (!nextConversation) localStorage.removeItem(storageKey)
    }
    if (!nextConversation && !forceNew) {
      const active = await request<Conversation[]>('/api/conversations')
      nextConversation = active
        .filter((candidate) => candidate.projectId === project.id && !['closed', 'deleted'].includes(candidate.status))
        .sort((left, right) => Date.parse(right.updatedAt) - Date.parse(left.updatedAt))[0] || null
    }
    if (nextConversation?.status === 'deleted') nextConversation = null
    if (!nextConversation) {
      nextConversation = await request<Conversation>('/api/conversations', {
        method: 'POST',
        body: { projectId: project.id, clientRequestId: crypto.randomUUID() },
      })
    } else if (nextConversation.status === 'closed' || nextConversation.status === 'error') {
      nextConversation = await request<Conversation>(`/api/conversations/${encodeURIComponent(nextConversation.id)}/retry`, {
        method: 'POST', body: { clientRequestId: crypto.randomUUID() },
      })
    }
    if (!mounted.current || generation !== conversationGeneration.current) return
    setCurrentConversation(nextConversation)
    applyConversationSnapshot(nextConversation)
    localStorage.setItem(storageKey, nextConversation.id)
    connectEvents(nextConversation.id)
    if (nextConversation.status === 'ready') {
      setChatError('')
      requestAnimationFrame(() => promptInput.current?.focus())
      return
    }
    void watchConversationReady(nextConversation.id, generation)
  }, [applyConversationSnapshot, clearMessages, closeEvents, connectEvents, request, setCurrentConversation, watchConversationReady])

  const selectProject = useCallback((available: Project[], projectID: string) => {
    ++projectApplicationsGeneration.current
    const project = available.find((candidate) => candidate.id === projectID && candidate.status === 'ready') || null
    setSelectedProject(project)
    setProjectApplications([])
    setPreviews([])
    setPreviewError('')
    if (project) {
      localStorage.setItem('pie-demo-project', project.id)
      setDraftPrompt(localStorage.getItem(`pie-demo-draft:${project.id}`) || '')
      setPreviewAppPath(project.previewAppPath || '')
    } else {
      localStorage.removeItem('pie-demo-project')
      setDraftPrompt('')
      setPreviewAppPath('')
    }
    return project
  }, [])

  const loadProjects = useCallback(async () => {
    const loaded = await request<Project[]>('/api/projects')
    setProjects(loaded)
    const ready = loaded.filter((project) => project.status === 'ready')
    const remembered = ready.find((project) => project.id === localStorage.getItem('pie-demo-project'))
    return selectProject(loaded, remembered?.id || ready[0]?.id || '')
  }, [request, selectProject])

  const loadProjectApplications = useCallback(async (project: Project) => {
    const generation = ++projectApplicationsGeneration.current
    setProjectApplicationsLoading(true)
    setPreviewError('')
    try {
      const applications = await request<ProjectApplication[]>(`/api/projects/${encodeURIComponent(project.id)}/apps`, { csrf: false })
      if (!mounted.current || generation !== projectApplicationsGeneration.current) return
      setProjectApplications(applications)
      const stored = applications.find((application) => application.path === project.previewAppPath)
      if (stored) {
        setPreviewAppPath(stored.path)
        return
      }
      if (applications.length !== 1) {
        setPreviewAppPath('')
        return
      }
      const updated = await request<Project>(`/api/projects/${encodeURIComponent(project.id)}/preview-app`, {
        method: 'PUT', body: { appPath: applications[0].path },
      })
      if (!mounted.current || generation !== projectApplicationsGeneration.current) return
      setProjects((current) => current.map((candidate) => candidate.id === updated.id ? updated : candidate))
      setSelectedProject((current) => current?.id === updated.id ? updated : current)
      setPreviewAppPath(updated.previewAppPath)
    } catch (error) {
      if (!(error instanceof WebChatAPIError && error.status === 401)) setPreviewError(errorMessage(error))
    } finally {
      if (mounted.current && generation === projectApplicationsGeneration.current) setProjectApplicationsLoading(false)
    }
  }, [request])

  useEffect(() => {
    if (!appVisible || !selectedProject) {
      setProjectApplications([])
      setProjectApplicationsLoading(false)
      return
    }
    void loadProjectApplications(selectedProject)
  }, [appVisible, loadProjectApplications, selectedProject?.id])

  useEffect(() => {
    if (!appVisible || !selectedProject) {
      setPreviews([])
      return
    }
    let active = true
    const projectID = selectedProject.id
    const refresh = async () => {
      try {
        const values = await request<Preview[]>(`/api/projects/${encodeURIComponent(projectID)}/previews`, { csrf: false })
        if (active) {
          setPreviews(values.sort((left, right) => Date.parse(right.createdAt) - Date.parse(left.createdAt)))
          setPreviewError('')
        }
      } catch (error) {
        if (active && !(error instanceof WebChatAPIError && error.status === 401)) setPreviewError(errorMessage(error))
      }
    }
    void refresh()
    const interval = window.setInterval(refresh, 3000)
    return () => {
      active = false
      window.clearInterval(interval)
    }
  }, [appVisible, request, selectedProject])

  const enterApp = useCallback(async () => {
    setAppVisible(true)
    setChatError('')
    const loadedWorkspace = await request<Workspace>('/api/workspace')
    if (!mounted.current) return
    setWorkspace(loadedWorkspace)
    if (loadedWorkspace.status !== 'ready') return
    const project = await loadProjects()
    if (project) await ensureConversation(project, false)
    else {
      setRelayState({ kind: '', text: '프로젝트 생성 대기' })
      setClientState({ kind: '', text: '프로젝트 생성 대기' })
    }
  }, [ensureConversation, loadProjects, request])

  const resetApp = useCallback(() => {
    clearApplicationState()
    setLoginError('')
    setSignupError('')
  }, [clearApplicationState])

  useEffect(() => {
    mounted.current = true
    if (!initialized.current) {
      initialized.current = true
      void (async () => {
        try {
          const current = await request<Session>('/api/auth/me')
          if (!mounted.current) return
          csrfToken.current = current.csrfToken
          setSession(current)
          await enterApp()
        } catch {
          if (mounted.current) resetApp()
        }
      })()
    }
    return () => {
      mounted.current = false
      ++conversationGeneration.current
      closeEvents()
      if (assistantRenderFrame.current !== null) cancelAnimationFrame(assistantRenderFrame.current)
      if (thinkingRenderFrame.current !== null) cancelAnimationFrame(thinkingRenderFrame.current)
      if (taskRenderFrame.current !== null) cancelAnimationFrame(taskRenderFrame.current)
    }
  }, [closeEvents, enterApp, request, resetApp])

  useEffect(() => {
    const target = messagesElement.current
    if (target) target.scrollTop = target.scrollHeight
  }, [items])

  async function handleLogin(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setLoginError('')
    setLoginPending(true)
    const form = new FormData(event.currentTarget)
    try {
      const current = await request<Session>('/api/auth/login', {
        method: 'POST',
        body: { username: form.get('username'), password: form.get('password') },
        csrf: false,
      })
      csrfToken.current = current.csrfToken
      setSession(current)
      localStorage.removeItem('pie-demo-conversation')
      await enterApp()
    } catch (error) {
      setLoginError(errorMessage(error))
    } finally {
      setLoginPending(false)
    }
  }

  async function handleSignup(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setSignupError('')
    const form = new FormData(event.currentTarget)
    const password = String(form.get('password') || '')
    if (password !== String(form.get('passwordConfirm') || '')) {
      setSignupError('비밀번호 확인이 일치하지 않습니다.')
      return
    }
    setSignupPending(true)
    try {
      const current = await request<Session>('/api/auth/signup', {
        method: 'POST',
        body: {
          displayName: form.get('displayName'),
          username: form.get('username'),
          password,
        },
        csrf: false,
      })
      csrfToken.current = current.csrfToken
      setSession(current)
      setWorkspace(current.workspace || null)
      localStorage.removeItem('pie-demo-conversation')
      await enterApp()
    } catch (error) {
      setSignupError(errorMessage(error))
    } finally {
      setSignupPending(false)
    }
  }

  async function handleLogout() {
    try { await request('/api/auth/logout', { method: 'POST', body: {} }) } catch { /* local session is cleared regardless */ }
    resetApp()
  }

  async function handleProvision() {
    setProvisionPending(true)
    setWorkspace({ status: 'provisioning' })
    setChatError('')
    try {
      const readyWorkspace = await request<Workspace>('/api/workspace/provision', { method: 'POST', body: {} })
      setWorkspace(readyWorkspace)
      const project = await loadProjects()
      if (project) await ensureConversation(project, false)
      else {
        setRelayState({ kind: '', text: '프로젝트 생성 대기' })
        setClientState({ kind: '', text: '프로젝트 생성 대기' })
      }
    } catch (error) {
      setWorkspace({ status: 'failed' })
      setChatError(errorMessage(error))
    } finally {
      setProvisionPending(false)
    }
  }

  async function handleProjectSelection(projectID: string) {
    const project = selectProject(projects, projectID)
    if (!project) return
    setChatError('')
    try {
      await ensureConversation(project, false)
    } catch (error) {
      setChatError(errorMessage(error))
      setRelayState({ kind: 'error', text: 'Pie Relay 연결 실패' })
      setClientState({ kind: 'error', text: 'Docker clientd 연결 실패' })
    }
  }

  async function handleNewChat() {
    if (workspace?.status !== 'ready' || !selectedProject) return
    setChatError('')
    try {
      await ensureConversation(selectedProject, true)
    } catch (error) {
      setChatError(errorMessage(error))
      setRelayState({ kind: 'error', text: 'Pie Relay 연결 실패' })
      setClientState({ kind: 'error', text: 'Docker clientd 연결 실패' })
    }
  }

  async function handleProjectApplicationSelection(appPath: string) {
    if (!selectedProject || previewPending) return
    setPreviewPending('select-app')
    setPreviewError('')
    try {
      const updated = await request<Project>(`/api/projects/${encodeURIComponent(selectedProject.id)}/preview-app`, {
        method: 'PUT', body: { appPath },
      })
      setProjects((current) => current.map((candidate) => candidate.id === updated.id ? updated : candidate))
      setSelectedProject(updated)
      setPreviewAppPath(updated.previewAppPath)
    } catch (error) {
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewPending('')
    }
  }

  function selectedPreviewAppPath() {
    if (!previewAppPath) throw new Error('이 프로젝트에서 실행할 웹 앱을 먼저 선택해 주세요.')
    const appPath = normalizePreviewAppPath(previewAppPath)
    if (appPath !== previewAppPath) setPreviewAppPath(appPath)
    return appPath
  }

  async function handleCreatePreview() {
    if (!selectedProject || previewPending) return
    setPreviewError('')
    try {
      const appPath = selectedPreviewAppPath()
      setPreviewPending('create')
      const launch = await request<PreviewLaunch>(`/api/projects/${encodeURIComponent(selectedProject.id)}/previews`, {
        method: 'POST',
        body: { appPath, visibility: previewVisibility, profile: 'auto', ttlSeconds: 14_400, clientRequestId: crypto.randomUUID() },
      })
      setPreviews((current) => [launch.preview, ...current.filter((value) => value.id !== launch.preview.id)])
    } catch (error) {
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewPending('')
    }
  }

  async function handlePrimaryPreview() {
    if (!selectedProject || previewPending) return
    setPreviewError('')
    try {
      const appPath = selectedPreviewAppPath()
      const existing = findPreviewForApp(previews, appPath)
      if (!existing) {
        await handleCreatePreview()
        return
      }
      if (existing.status === 'ready') {
        await handleOpenPreview(existing)
        return
      }
      if (existing.status === 'failed' || existing.status === 'stopped') {
        await handleRestartPreview(existing)
        return
      }
      setPreviewError(existing.status === 'starting' ? '프리뷰가 시작되는 중입니다.' : '프리뷰가 중지되는 중입니다.')
    } catch (error) {
      setPreviewError(errorMessage(error))
    }
  }

  async function handlePreviewVisibilityChange(visibility: 'private' | 'public') {
    if (!selectedProject || previewPending) return
    const previous = previewVisibility
    setPreviewVisibility(visibility)
    setPreviewError('')
    let existing: Preview | undefined
    try {
      const appPath = selectedPreviewAppPath()
      existing = findPreviewForApp(previews, appPath)
      if (!existing || existing.visibility === visibility) return
      setPreviewPending(`visibility:${existing.id}`)
      const launch = await request<PreviewLaunch>(`/api/projects/${encodeURIComponent(selectedProject.id)}/previews/${encodeURIComponent(existing.id)}/visibility`, {
        method: 'PUT', body: { visibility },
      })
      setPreviews((current) => current.map((value) => value.id === launch.preview.id ? launch.preview : value))
    } catch (error) {
      setPreviewVisibility(previous)
      setPreviewError(errorMessage(error))
    } finally {
      if (existing) setPreviewPending('')
    }
  }

  async function handleOpenPreview(preview: Preview) {
    if (!selectedProject || preview.status !== 'ready' || previewPending) return
    const popup = window.open('about:blank', '_blank')
    if (popup) popup.opener = null
    setPreviewPending(`open:${preview.id}`)
    setPreviewError('')
    try {
      const launch = await request<PreviewLaunch>(`/api/projects/${encodeURIComponent(selectedProject.id)}/previews/${encodeURIComponent(preview.id)}/access`, { method: 'POST', body: {} })
      const target = launch.accessUrl || launch.url
      if (popup) popup.location.replace(target)
      else window.location.assign(target)
    } catch (error) {
      popup?.close()
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewPending('')
    }
  }

  async function handleRestartPreview(preview: Preview) {
    if (!selectedProject || previewPending) return
    setPreviewPending(`restart:${preview.id}`)
    setPreviewError('')
    try {
      const launch = await request<PreviewLaunch>(`/api/projects/${encodeURIComponent(selectedProject.id)}/previews/${encodeURIComponent(preview.id)}/restart`, { method: 'POST', body: {} })
      setPreviews((current) => current.map((value) => value.id === preview.id ? launch.preview : value))
    } catch (error) {
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewPending('')
    }
  }

  async function handleStopPreview(preview: Preview) {
    if (!selectedProject || previewPending) return
    setPreviewPending(`stop:${preview.id}`)
    setPreviewError('')
    try {
      const stopped = await request<Preview>(`/api/projects/${encodeURIComponent(selectedProject.id)}/previews/${encodeURIComponent(preview.id)}/stop`, { method: 'POST', body: {} })
      setPreviews((current) => current.map((value) => value.id === preview.id ? stopped : value))
    } catch (error) {
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewPending('')
    }
  }

  async function handleDeletePreview() {
    const preview = previewDeleteTarget
    if (!selectedProject || !preview || previewPending) return
    setPreviewPending(`delete:${preview.id}`)
    setPreviewError('')
    try {
      await request<{ id: string; deleted: boolean }>(`/api/projects/${encodeURIComponent(selectedProject.id)}/previews/${encodeURIComponent(preview.id)}`, { method: 'DELETE' })
      setPreviews((current) => current.filter((value) => value.id !== preview.id))
      setPreviewDeleteTarget(null)
    } catch (error) {
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewPending('')
    }
  }

  async function handlePreviewLogs(preview: Preview) {
    if (!selectedProject || previewPending) return
    setPreviewPending(`logs:${preview.id}`)
    setPreviewError('')
    try {
      const result = await request<{ logs: string }>(`/api/projects/${encodeURIComponent(selectedProject.id)}/previews/${encodeURIComponent(preview.id)}/logs`, { csrf: false })
      setPreviewLogs(result.logs || '아직 출력된 로그가 없습니다.')
      setPreviewLogsOpen(true)
    } catch (error) {
      setPreviewError(errorMessage(error))
    } finally {
      setPreviewPending('')
    }
  }

  const handleRetryConversation = useCallback(async () => {
    const current = conversationRef.current
    if (!current || retryPending) return
    setRetryPending(true)
    setChatError('')
    setRelayState({ kind: 'busy', text: 'Pie Relay 다시 연결 준비 중' })
    setClientState({ kind: 'busy', text: 'Docker clientd 다시 연결 준비 중' })
    try {
      const retried = await request<Conversation>(`/api/conversations/${encodeURIComponent(current.id)}/retry`, {
        method: 'POST', body: { clientRequestId: crypto.randomUUID() },
      })
      if (!mounted.current || conversationRef.current?.id !== current.id) return
      const generation = ++conversationGeneration.current
      applyConversationSnapshot(retried)
      connectEvents(retried.id)
      if (retried.status !== 'ready') {
        void watchConversationReady(retried.id, generation)
      }
    } catch (error) {
      setChatError(errorMessage(error))
      setRelayState({ kind: 'error', text: 'Pie Relay 재연결 실패' })
      setClientState({ kind: 'error', text: 'Docker clientd 재연결 실패' })
    } finally {
      setRetryPending(false)
    }
  }, [applyConversationSnapshot, connectEvents, request, retryPending, watchConversationReady])

  useEffect(() => {
    if (!appVisible || !conversation || !conversationNeedsReconnect(conversation) || retryPending) return
    const recoveryKey = `${conversation.id}:${conversation.status}:${conversation.updatedAt}:${conversation.connection.reason}`
    if (automaticRecoveryAttempt.current === recoveryKey) return
    automaticRecoveryAttempt.current = recoveryKey
    setChatError('대화 세션이 종료되어 자동으로 다시 연결하고 있습니다.')
    const timer = window.setTimeout(() => { void handleRetryConversation() }, 0)
    return () => window.clearTimeout(timer)
  }, [appVisible, conversation, handleRetryConversation, retryPending])

  async function handleRetryStream() {
    const currentConversation = conversationRef.current
    if (!currentConversation || streamRetryPending) return
    setStreamRetryPending(true)
    setChatError('')
    setStreamState({ kind: 'busy', text: '브라우저 실시간 연결 확인 중' })
    try {
      const currentSession = await request<Session>('/api/auth/me', { csrf: false })
      if (!mounted.current || conversationRef.current?.id !== currentConversation.id) return
      csrfToken.current = currentSession.csrfToken
      setSession(currentSession)
      const snapshot = await request<Conversation>(`/api/conversations/${encodeURIComponent(currentConversation.id)}`, { csrf: false })
      if (!mounted.current || conversationRef.current?.id !== currentConversation.id) return
      applyConversationSnapshot(snapshot)
      if (snapshot.status === 'deleted') {
        setStreamRetryPending(false)
        return
      }
      // The conversation may have crossed its idle deadline between rendering
      // the stream button and this click. Reopening only /events would then
      // receive HTTP 409 forever, so promote the action to a full reconnect.
      if (conversationNeedsReconnect(snapshot)) {
        setStreamRetryPending(false)
        await handleRetryConversation()
        return
      }
      connectEvents(currentConversation.id)
    } catch (error) {
      if (error instanceof WebChatAPIError && error.status === 401) return
      setStreamRetryPending(false)
      setStreamState({ kind: 'error', text: '브라우저 실시간 재연결 실패' })
      setChatError(`실시간 연결을 다시 열지 못했습니다: ${errorMessage(error)}`)
    }
  }

  function openProjectDialog() {
    setProjectError('')
    setProjectLocale('ko')
    projectForm.current?.reset()
    setProjectDialogOpen(true)
  }

  async function handleCreateProject(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setProjectPending(true)
    setProjectError('')
    const form = new FormData(event.currentTarget)
    let project: Project
    try {
      project = await request<Project>('/api/projects', {
        method: 'POST',
        body: { name: form.get('name'), locale: projectLocale, clientRequestId: crypto.randomUUID() },
      })
      const nextProjects = [project, ...projects.filter((candidate) => candidate.id !== project.id)]
      setProjects(nextProjects)
      selectProject(nextProjects, project.id)
      setProjectDialogOpen(false)
    } catch (error) {
      setProjectError(errorMessage(error))
      return
    } finally {
      setProjectPending(false)
    }
    try {
      await ensureConversation(project, true)
    } catch (error) {
      setChatError(`프로젝트는 만들어졌지만 채팅 연결에 실패했습니다: ${errorMessage(error)}`)
      setRelayState({ kind: 'error', text: 'Pie Relay 연결 실패' })
      setClientState({ kind: 'error', text: 'Docker clientd 연결 실패' })
    }
  }

  async function handleAttachmentChange(files: File[]) {
    setChatError('')
    try {
      if (!files.length) return
      if (attachments.length + files.length > MAX_IMAGE_COUNT) throw new Error(`이미지는 최대 ${MAX_IMAGE_COUNT}개까지 첨부할 수 있습니다.`)
      const total = attachments.reduce((sum, image) => sum + image.size, 0) + files.reduce((sum, file) => sum + file.size, 0)
      if (total > MAX_IMAGES_TOTAL_BYTES) throw new Error('첨부 이미지 전체 크기는 4MiB 이하여야 합니다.')
      const additions: ImageAttachment[] = []
      for (const file of files) {
        if (!SUPPORTED_IMAGE_TYPES.has(file.type)) throw new Error('JPEG, PNG, GIF, WebP 이미지만 첨부할 수 있습니다.')
        if (!file.size || file.size > MAX_IMAGE_BYTES) throw new Error('이미지 한 개의 크기는 4MiB 이하여야 합니다.')
        const data = await fileToBase64(file)
        additions.push({ data, mimeType: file.type, name: file.name, size: file.size, previewURL: `data:${file.type};base64,${data}` })
      }
      setAttachments((current) => [...current, ...additions])
    } catch (error) {
      setChatError(errorMessage(error))
    } finally {
      if (attachmentInput.current) attachmentInput.current.value = ''
    }
  }

  async function handleSend(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const typedPrompt = draftPrompt.trim()
    if ((!typedPrompt && attachments.length === 0) || !conversation || activeTurn || conversation.status !== 'ready') return
    if (streamState.kind !== 'online' || eventSource.current?.readyState !== EventSource.OPEN) {
      setStreamState({ kind: 'error', text: '실시간 연결 필요' })
      setChatError('실시간 수신 연결이 확인되지 않아 요청을 보내지 않았습니다. 실시간 재연결 후 다시 전송해 주세요.')
      return
    }
    const prompt = typedPrompt || '첨부된 이미지를 분석해 주세요.'
    const sentAttachments = attachments
    const clientRequestID = crypto.randomUUID()
    knownRequestIDs.current.add(clientRequestID)
    activeRequestID.current = clientRequestID
    assistantMessageID.current = null
    assistantMarkdown.current = ''
    thinkingMessageID.current = null
    thinkingText.current = ''
    setActiveTurn(true)
    setItems((current) => [...current, {
      kind: 'message', id: `request-${clientRequestID}`, role: 'user', text: prompt, pending: false, attachments: sentAttachments,
    }])
    try {
      await request(`/api/conversations/${encodeURIComponent(conversation.id)}/messages`, {
        method: 'POST',
        body: {
          prompt,
          images: sentAttachments.map(({ data, mimeType, name, size }) => ({ data, mimeType, name, size })),
          clientRequestId: clientRequestID,
        },
      })
      setDraftPrompt('')
      if (selectedProject) localStorage.removeItem(`pie-demo-draft:${selectedProject.id}`)
      clearAttachments()
    } catch (error) {
      setChatError(`요청 실패: ${errorMessage(error)} 입력 내용은 그대로 보관했습니다.`)
      finishTurn('\n\n요청을 전달하지 못했습니다.', 'error')
    }
  }

  function handlePromptKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      event.currentTarget.form?.requestSubmit()
    }
  }

  async function handleCancel() {
    if (!conversation || !activeTurn) return
    try {
      await request(`/api/conversations/${encodeURIComponent(conversation.id)}/cancel`, {
        method: 'POST', body: { clientRequestId: crypto.randomUUID() },
      })
    } catch (error) {
      setChatError(errorMessage(error))
    }
  }

  async function loadUsage(days: '7' | '30' | '90') {
    const version = ++usageLoadVersion.current
    setUsageLoading(true)
    setUsageError('')
    setUsageSummary(null)
    setUsageItems([])
    setUsageNextCursor('')
    setUsageLoadingMore(false)
    try {
      const [summary, page] = await Promise.all([
        request<UsageSummary>(`/api/usage?days=${days}`, { csrf: false }),
        request<UsageEventPage>(`/api/usage/events?days=${days}`, { csrf: false }),
      ])
      if (version === usageLoadVersion.current) {
        setUsageSummary(summary)
        setUsageItems(page.items)
        setUsageNextCursor(page.nextCursor)
      }
    } catch (error) {
      if (version === usageLoadVersion.current) setUsageError(errorMessage(error))
    } finally {
      if (version === usageLoadVersion.current) setUsageLoading(false)
    }
  }

  async function loadMoreUsage() {
    if (!usageNextCursor || usageLoadingMore) return
    const version = usageLoadVersion.current
    setUsageLoadingMore(true)
    setUsageError('')
    try {
      const page = await request<UsageEventPage>(`/api/usage/events?days=${usageDays}&cursor=${encodeURIComponent(usageNextCursor)}`, { csrf: false })
      if (version === usageLoadVersion.current) {
        setUsageItems((current) => [...current, ...page.items])
        setUsageNextCursor(page.nextCursor)
      }
    } catch (error) {
      if (version === usageLoadVersion.current) setUsageError(errorMessage(error))
    } finally {
      if (version === usageLoadVersion.current) setUsageLoadingMore(false)
    }
  }

  function openUsage() {
    setUsageOpen(true)
    void loadUsage(usageDays)
  }

  async function answerPermission(requestID: string, allow: boolean) {
    if (!conversationRef.current || !permissionRequests.current.has(requestID)) return
    setChatError('')
    setItems((current) => current.map((item) => item.kind === 'permission' && item.requestId === requestID
      ? { ...item, state: 'sending', statusText: allow ? '승인 응답을 보내는 중…' : '거절 응답을 보내는 중…' }
      : item))
    try {
      await request(`/api/conversations/${encodeURIComponent(conversationRef.current.id)}/permissions/${encodeURIComponent(requestID)}`, {
        method: 'POST', body: { allow, clientRequestId: crypto.randomUUID() },
      })
      resolvePermission(requestID, allow)
    } catch (error) {
      setItems((current) => current.map((item) => item.kind === 'permission' && item.requestId === requestID
        ? { ...item, state: 'pending', statusText: '응답 전송에 실패했습니다. 다시 선택해 주세요.' }
        : item))
      setChatError(errorMessage(error))
    }
  }

  const workspaceStatus = workspace?.status || 'unknown'
  const statusLabels: Record<string, string> = {
    ready: '실행 중', provisioning: '준비 중', not_provisioned: '미발급', failed: '오류', suspended: '중지됨', unknown: '확인 중',
  }
  const previewStatusLabels: Record<string, string> = {
    starting: '시작 중', ready: '실행 중', stopping: '중지 중', stopped: '중지됨', failed: '오류',
  }
  const readyProjects = projects.filter((project) => project.status === 'ready')
  const activePreviews = previews.filter((preview) => !['stopped', 'failed'].includes(preview.status))
  const selectedApplication = projectApplications.find((application) => application.path === previewAppPath)
  let normalizedPreviewPath = ''
  try {
    normalizedPreviewPath = normalizePreviewAppPath(previewAppPath)
  } catch {
    normalizedPreviewPath = ''
  }
  const primaryPreview = normalizedPreviewPath ? findPreviewForApp(previews, normalizedPreviewPath) : undefined
  const displayedPreviews = primaryPreview ? [primaryPreview] : []
  const primaryPreviewLabel = previewPending
    ? '처리 중…'
    : primaryPreview?.status === 'ready'
      ? '열기'
      : primaryPreview?.status === 'starting'
        ? '시작 중…'
        : primaryPreview?.status === 'stopping'
          ? '중지 중…'
          : primaryPreview
            ? '다시 실행'
            : '실행'
  const primaryPreviewBlocked = primaryPreview?.status === 'starting' || primaryPreview?.status === 'stopping'
  const previewBaseDisabled = !selectedProject || workspaceStatus !== 'ready' || projectApplicationsLoading || !!previewPending || !selectedApplication || !normalizedPreviewPath
  const conversationReady = conversation?.status === 'ready' && conversation.connection.reason === 'connected'
  const streamReady = streamState.kind === 'online'
  const inputDisabled = activeTurn || workspaceStatus !== 'ready' || !selectedProject
  const sendDisabled = inputDisabled || !conversationReady || !streamReady || (!draftPrompt.trim() && attachments.length === 0)
  const canRetryConversation = conversationNeedsReconnect(conversation)
  const canRetryStream = !!conversation && conversation.status !== 'deleted' && !streamReady && !canRetryConversation
  const canProvision = ['not_provisioned', 'failed', 'suspended'].includes(workspaceStatus)

  useEffect(() => {
    if (primaryPreview && !previewPending) setPreviewVisibility(primaryPreview.visibility)
  }, [primaryPreview, previewPending])

  return (
    <>
      <div className="ambient ambient-one" />
      <div className="ambient ambient-two" />
      <main className="shell">
        <header className="topbar">
          <a className="brand" href="/" aria-label="Pie Workspace Chat">
            <span className="brand-mark">π</span>
            <span><strong>Pie</strong><small>Workspace Chat</small></span>
          </a>
          <div id="session-tools" className={`session-tools${appVisible ? '' : ' hidden'}`}>
            <span id="current-user">{session?.user.displayName || ''}</span>
            <div className="main-view-switch" role="group" aria-label="작업 화면 선택">
              <button type="button" className={mainView === 'chat' ? 'active' : ''} aria-pressed={mainView === 'chat'} onClick={() => setMainView('chat')}><MessageSquare aria-hidden="true" />채팅</button>
              <button type="button" className={mainView === 'code' ? 'active' : ''} aria-pressed={mainView === 'code'} disabled={!selectedProject || !conversation || conversation.projectId !== selectedProject.id || conversation.status !== 'ready'} onClick={() => setMainView('code')}><Code2 aria-hidden="true" />코드</button>
            </div>
            <Button id="usage-button" variant="ghost" onClick={openUsage}><BarChart3 aria-hidden="true" />사용량</Button>
            <Button id="logout-button" variant="ghost" onClick={handleLogout}>로그아웃</Button>
          </div>
        </header>

        <section id="login-view" className={`auth-layout${appVisible ? ' hidden' : ''}`}>
          <div className="intro">
            <p className="eyebrow">ISOLATED AI WORKSPACE</p>
            <h1>사용자마다 분리된<br />Claude Code 환경</h1>
            <p>이 샘플은 제3자 서비스의 로그인부터 Pie Manager, Pie Relay, Docker clientd까지 실제 연결 경로를 검증합니다.</p>
            <div className="route"><span>Web</span><i /><span>Manager</span><i /><span>Pie Relay</span><i /><span>Docker</span></div>
          </div>
          <div className="panel auth-panel">
            <div>
              <p className="eyebrow">DEMO SIGN IN</p>
              <h2>채팅 시작하기</h2>
              <p className="muted">{registrationEnabled
                ? '가입하면 사용자 전용 Docker 작업공간이 자동으로 생성됩니다.'
                : '운영자가 발급한 계정으로 사용자 전용 Docker 작업공간에 접속합니다.'}</p>
            </div>
            {registrationEnabled && <div className="auth-tabs" role="tablist" aria-label="인증 방식">
              <button id="show-login" className={`auth-tab${authMode === 'login' ? ' active' : ''}`} type="button" role="tab" aria-selected={authMode === 'login'} onClick={() => { setAuthMode('login'); setLoginError(''); setSignupError('') }}>로그인</button>
              <button id="show-signup" className={`auth-tab${authMode === 'signup' ? ' active' : ''}`} type="button" role="tab" aria-selected={authMode === 'signup'} onClick={() => { setAuthMode('signup'); setLoginError(''); setSignupError('') }}>회원가입</button>
            </div>}
            <form id="login-form" className={`auth-form${authMode === 'login' ? '' : ' hidden'}`} onSubmit={handleLogin}>
              <label>아이디<Input id="username" name="username" autoComplete="username" required /></label>
              <label>비밀번호<Input id="password" name="password" type="password" autoComplete="current-password" required /></label>
              <p id="login-error" className={`error${loginError ? '' : ' hidden'}`} role="alert">{loginError}</p>
              <Button type="submit" disabled={loginPending}>{loginPending ? '로그인 중…' : '로그인'}</Button>
            </form>
            {registrationEnabled && <form id="signup-form" className={`auth-form${authMode === 'signup' ? '' : ' hidden'}`} onSubmit={handleSignup}>
              <label>이름<Input id="signup-display-name" name="displayName" autoComplete="name" maxLength={120} required /></label>
              <label>아이디<Input id="signup-username" name="username" autoComplete="username" maxLength={64} required /></label>
              <label>비밀번호<Input id="signup-password" name="password" type="password" autoComplete="new-password" minLength={10} required /></label>
              <label>비밀번호 확인<Input id="signup-password-confirm" name="passwordConfirm" type="password" autoComplete="new-password" minLength={10} required /></label>
              <p id="signup-error" className={`error${signupError ? '' : ' hidden'}`} role="alert">{signupError}</p>
              <Button type="submit" disabled={signupPending}>{signupPending ? '가입 및 작업공간 생성 중…' : '가입하고 작업공간 만들기'}</Button>
            </form>}
          </div>
        </section>

        <section id="app-view" className={`workspace${appVisible ? '' : ' hidden'}`}>
          <aside className="sidebar panel">
            <div><p className="eyebrow">EXECUTION TARGET</p><h2>내 AI 작업공간</h2></div>
            <div className="target-card">
              <span className="target-icon">⌁</span>
              <div><strong>Docker Executor</strong><small>사용자 전용 격리 환경</small></div>
              <span id="workspace-dot" className={`status-dot ${workspaceStatus === 'ready' ? 'online' : workspaceStatus === 'provisioning' ? 'busy' : workspaceStatus === 'failed' ? 'error' : ''}`} />
            </div>
            <dl className="facts">
              <div><dt>상태</dt><dd id="workspace-status">{statusLabels[workspaceStatus] || workspaceStatus}</dd></div>
              <div><dt>인증정보</dt><dd id="credential-status">{workspace?.credentialConfigured ? `설정됨 · v${workspace.credentialVersion}` : '미설정'}</dd></div>
              <div><dt>연결 경로</dt><dd>Pie Relay</dd></div>
            </dl>
            <section className="project-picker" aria-labelledby="project-picker-title">
              <div className="project-picker-heading">
                <div><p className="eyebrow">KROOT PROJECT</p><strong id="project-picker-title">작업 프로젝트</strong></div>
                <button id="create-project-button" className="project-add" type="button" aria-label="새 프로젝트 만들기" disabled={activeTurn || workspaceStatus !== 'ready'} onClick={openProjectDialog}><Plus aria-hidden="true" /></button>
              </div>
              <label className="project-select-label" htmlFor="project-select">프로젝트 선택</label>
              <Select value={selectedProject?.id} onValueChange={handleProjectSelection} disabled={activeTurn || readyProjects.length === 0}>
                <SelectTrigger id="project-select" aria-label="프로젝트 선택"><SelectValue placeholder="프로젝트를 만들어 주세요" /></SelectTrigger>
                <SelectContent>
                  {readyProjects.map((project) => <SelectItem key={project.id} value={project.id}>{project.name}</SelectItem>)}
                </SelectContent>
              </Select>
              <small id="project-description">{selectedProject ? `${selectedProject.name} · Kroot 준비 완료${selectedApplication ? ` · 실행 앱 ${selectedApplication.name}` : ''}` : '새 프로젝트를 만들면 컨테이너 안에서 Kroot가 자동으로 초기화됩니다.'}</small>
            </section>
            <section className="preview-picker" aria-labelledby="preview-picker-title">
              <div className="preview-heading">
                <div><p className="eyebrow">WEB PREVIEW</p><strong id="preview-picker-title">프로젝트 미리보기</strong></div>
                <span>{primaryPreview ? (primaryPreview.visibility === 'private' ? '비공개' : '공개') : '기본 비공개'}</span>
              </div>
              <div className="preview-path">
                <div className="preview-app-label"><span>실행 앱</span><button type="button" disabled={!selectedProject || projectApplicationsLoading || !!previewPending} onClick={() => selectedProject && void loadProjectApplications(selectedProject)}><RefreshCw aria-hidden="true" />다시 찾기</button></div>
                {projectApplicationsLoading
                  ? <div className="preview-app-state">프로젝트에서 실행 가능한 웹 앱을 찾는 중…</div>
                  : projectApplications.length === 0
                    ? <div className="preview-app-state warning">아직 실행 가능한 웹 앱이 없습니다. Claude Code로 웹 프로젝트를 만든 뒤 ‘다시 찾기’를 눌러 주세요.</div>
                    : projectApplications.length === 1
                      ? <div className="preview-app-state"><strong>{projectApplications[0].name}</strong><small>{profileLabel(projectApplications[0].profile)} 자동 감지</small></div>
                      : <Select value={previewAppPath || undefined} onValueChange={(value) => void handleProjectApplicationSelection(value)} disabled={!!previewPending}>
                        <SelectTrigger aria-label="실행할 웹 앱 선택"><SelectValue placeholder="실행할 웹 앱을 선택해 주세요" /></SelectTrigger>
                        <SelectContent>{projectApplications.map((application) => <SelectItem key={application.path} value={application.path}>{application.name} · {profileLabel(application.profile)}</SelectItem>)}</SelectContent>
                      </Select>}
                <small>컨테이너 내부 경로는 Pie가 자동으로 관리하며 프로젝트 설정에 저장됩니다.</small>
              </div>
              <div className="preview-create">
                <Select value={previewVisibility} onValueChange={(value) => void handlePreviewVisibilityChange(value as 'private' | 'public')} disabled={!!previewPending}>
                  <SelectTrigger aria-label="프리뷰 공개 범위"><SelectValue /></SelectTrigger>
                  <SelectContent><SelectItem value="private">비공개</SelectItem><SelectItem value="public">공개</SelectItem></SelectContent>
                </Select>
                <button type="button" disabled={previewBaseDisabled || !!primaryPreviewBlocked || (!primaryPreview && activePreviews.length >= 4)} onClick={() => void handlePrimaryPreview()}>{primaryPreviewLabel}</button>
                <small className="preview-visibility-help">{previewVisibility === 'private' ? '비공개는 Pie 인증을 거친 사용자만 열 수 있습니다.' : '공개는 주소를 아는 누구나 열 수 있습니다.'}</small>
              </div>
              {displayedPreviews.length > 0
                ? <div className="preview-list">{displayedPreviews.map((preview) => (
                  <article key={preview.id} className={`preview-item ${preview.status}`}>
                    <div className="preview-summary">
                      <span className={`status-dot ${preview.status === 'ready' ? 'online' : preview.status === 'starting' || preview.status === 'stopping' ? 'busy' : preview.status === 'failed' ? 'error' : ''}`} />
                      <div><strong>{previewStatusLabels[preview.status] || preview.status}</strong><small>{preview.visibility === 'private' ? <LockKeyhole aria-hidden="true" /> : <Globe2 aria-hidden="true" />}{preview.visibility === 'private' ? '비공개' : '공개'} · {projectApplications.find((application) => application.path === preview.appPath)?.name || '웹 앱'} · {profileLabel(preview.profile)}</small></div>
                    </div>
                    {preview.lastError && <p title={preview.lastError}>{preview.lastError}</p>}
                    <div className="preview-actions">
                      <button type="button" title="새 창에서 열기" aria-label="프리뷰 열기" disabled={preview.status !== 'ready' || !!previewPending} onClick={() => void handleOpenPreview(preview)}><ExternalLink aria-hidden="true" /></button>
                      <button type="button" title="로그 보기" aria-label="프리뷰 로그 보기" disabled={preview.status === 'stopped' || !!previewPending} onClick={() => void handlePreviewLogs(preview)}><FileText aria-hidden="true" /></button>
                      <button type="button" title="재시작" aria-label="프리뷰 재시작" disabled={!['ready', 'failed', 'stopped'].includes(preview.status) || !!previewPending} onClick={() => void handleRestartPreview(preview)}><RotateCcw aria-hidden="true" /></button>
                      <button type="button" title="중지" aria-label="프리뷰 중지" disabled={['stopped', 'stopping'].includes(preview.status) || !!previewPending} onClick={() => void handleStopPreview(preview)}><Square aria-hidden="true" /></button>
                      <button type="button" className="delete" title="삭제" aria-label="프리뷰 삭제" disabled={preview.status === 'stopping' || !!previewPending} onClick={() => { setPreviewError(''); setPreviewDeleteTarget(preview) }}><Trash2 aria-hidden="true" /></button>
                    </div>
                  </article>
                ))}</div>
                : <p className="preview-empty">프로젝트의 개발 서버를 안전한 임시 주소로 열 수 있습니다.</p>}
              <p className={`error preview-error${previewError ? '' : ' hidden'}`} role="alert">{previewError}</p>
            </section>
            <Button id="provision-button" className={canProvision ? '' : 'hidden'} disabled={provisionPending} onClick={handleProvision}>{provisionPending ? '작업공간 준비 중…' : '작업공간 준비'}</Button>
            <Button id="new-chat-button" variant="secondary" disabled={workspaceStatus !== 'ready' || !selectedProject || activeTurn} onClick={handleNewChat}>새 대화</Button>
            <p className="privacy-note">다른 사용자는 이 컨테이너와 대화에 접근할 수 없습니다.</p>
          </aside>

          <section className={`chat panel${mainView === 'chat' ? '' : ' hidden'}`}>
            <header className="chat-header">
              <div><p className="eyebrow">CLAUDE CODE</p><h2>Workspace Assistant</h2></div>
              <div className="connection-group">
                <div className="connection">
                  <span id="relay-dot" className={`status-dot ${relayState.kind}`} /><span id="relay-status">{relayState.text}</span>
                  {canRetryConversation && <button id="conversation-retry-button" className="connection-retry" type="button" disabled={retryPending} onClick={() => void handleRetryConversation()}><RefreshCw aria-hidden="true" />{retryPending ? '재연결 중…' : '대화 다시 연결'}</button>}
                </div>
                <div className="connection client-connection" aria-live="polite">
                  <span id="client-dot" className={`status-dot ${clientState.kind}`} /><span id="client-status">{clientState.text}</span>
                </div>
                <div className="connection stream-connection" aria-live="polite">
                  <span id="stream-dot" className={`status-dot ${streamState.kind}`} /><span id="stream-status">{streamState.text}</span>
                  {canRetryStream && !streamRetryPending && <button id="stream-retry-button" className="connection-retry" type="button" onClick={() => void handleRetryStream()}><RefreshCw aria-hidden="true" />지금 다시 시도</button>}
                </div>
              </div>
            </header>
            <div id="messages" ref={messagesElement} className="messages" aria-live="polite">
              <div id="empty-state" className={`empty-state${items.length ? ' hidden' : ''}`}>
                <span>π</span><h3>무엇을 같이 만들어볼까요?</h3>
                <p>메시지는 Manager를 거쳐 내 Docker 컨테이너의 Claude Code로 전달됩니다.</p>
              </div>
              {items.map((item) => item.kind === 'message'
                ? <MessageView key={item.id} message={item} />
                : item.kind === 'permission'
                  ? <PermissionView key={item.id} permission={item} onAnswer={answerPermission} />
                  : item.kind === 'thinking'
                    ? <ThinkingView key={item.id} thinking={item} />
                    : item.kind === 'tool'
                      ? <ToolView key={item.id} tool={item} />
                      : <TaskView key={item.id} task={item} />)}
            </div>
            <div id="attachment-tray" className={`attachment-tray${attachments.length ? '' : ' hidden'}`} aria-live="polite">
              {attachments.map((image, index) => (
                <div className="attachment-item" key={`${image.name}-${index}`}>
                  {/* eslint-disable-next-line @next/next/no-img-element */}
                  <img src={image.previewURL} alt="" />
                  <div className="attachment-details"><strong>{image.name}</strong><small>{formatBytes(image.size)}</small></div>
                  <button className="attachment-remove" type="button" aria-label={`${image.name} 첨부 삭제`} onClick={() => setAttachments((current) => current.filter((_, itemIndex) => itemIndex !== index))}><X aria-hidden="true" /></button>
                </div>
              ))}
            </div>
            <form id="composer" className="composer" onSubmit={handleSend}>
              <input ref={attachmentInput} id="attachment-input" className="visually-hidden" type="file" accept="image/jpeg,image/png,image/gif,image/webp" multiple onChange={(event) => void handleAttachmentChange([...event.currentTarget.files || []])} />
              <button id="attachment-button" className="icon-button attach" type="button" aria-label="이미지 첨부" title="이미지 첨부" disabled={inputDisabled} onClick={() => attachmentInput.current?.click()}><Paperclip aria-hidden="true" /></button>
              <Textarea ref={promptInput} id="prompt" rows={1} placeholder={conversationReady && streamReady ? 'Claude Code에게 메시지 보내기' : '연결을 준비하는 동안 질문을 미리 작성할 수 있습니다'} maxLength={262144} disabled={inputDisabled} value={draftPrompt} onChange={(event) => {
                const value = event.currentTarget.value
                setDraftPrompt(value)
                if (selectedProject) localStorage.setItem(`pie-demo-draft:${selectedProject.id}`, value)
              }} onKeyDown={handlePromptKeyDown} />
              <button id="cancel-button" className={`icon-button${activeTurn ? '' : ' hidden'}`} type="button" aria-label="응답 중단" onClick={handleCancel}><Square aria-hidden="true" /></button>
              <button id="send-button" className="icon-button send" type="submit" aria-label="전송" disabled={sendDisabled}><Send aria-hidden="true" /></button>
            </form>
            {!conversationReady && selectedProject && !activeTurn && <p className="composer-hint">입력 내용은 이 프로젝트의 초안으로 보관되며, 연결이 완료되면 전송할 수 있습니다.</p>}
            {conversationReady && !streamReady && !activeTurn && <p className="composer-hint stream-warning">실시간 수신 연결을 자동으로 복구하고 있습니다. 8초 이상 지연되면 위의 ‘지금 다시 시도’를 사용할 수 있으며 작성한 초안은 그대로 유지됩니다.</p>}
            <p id="chat-error" className={`error chat-error${chatError ? '' : ' hidden'}`} role="alert">{chatError}</p>
          </section>
          <div className={`main-pane${mainView === 'code' ? '' : ' hidden'}`}>
            {selectedProject && conversation && conversation.projectId === selectedProject.id && conversation.status === 'ready'
              ? <WorkspaceEditor projectId={selectedProject.id} conversationId={conversation.id} csrfToken={session?.csrfToken || ''} refreshVersion={workspaceRefreshVersion} />
              : <section className="workspace-code panel workspace-code-unavailable"><Code2 aria-hidden="true" /><h2>코드 편집기를 준비하고 있습니다</h2><p>프로젝트의 Docker clientd와 대화 연결이 준비되면 파일을 안전하게 열 수 있습니다.</p><Button variant="secondary" onClick={() => setMainView('chat')}>채팅으로 돌아가기</Button></section>}
          </div>
        </section>
      </main>

      <Dialog open={projectDialogOpen} onOpenChange={(open) => { if (!projectPending) setProjectDialogOpen(open) }}>
        <DialogContent aria-describedby="project-dialog-description">
          <form ref={projectForm} id="project-form" className="project-form" onSubmit={handleCreateProject}>
            <div>
              <p className="eyebrow">NEW KROOT PROJECT</p>
              <DialogTitle>새 프로젝트 만들기</DialogTitle>
              <DialogDescription id="project-dialog-description" className="muted">내 전용 컨테이너 안에 프로젝트 폴더를 만들고 Kroot를 초기화합니다.</DialogDescription>
            </div>
            <label>프로젝트명<Input id="project-name" name="name" maxLength={120} autoComplete="off" required placeholder="예: 쇼핑몰 관리자" /></label>
            <label>기본 언어
              <Select value={projectLocale} onValueChange={setProjectLocale} disabled={projectPending}>
                <SelectTrigger id="project-locale" aria-label="기본 언어"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="ko">한국어</SelectItem><SelectItem value="en">English</SelectItem>
                  <SelectItem value="ja">日本語</SelectItem><SelectItem value="zh">中文</SelectItem>
                </SelectContent>
              </Select>
            </label>
            <p id="project-error" className={`error${projectError ? '' : ' hidden'}`} role="alert">{projectError}</p>
            <div className="dialog-actions">
              <DialogClose asChild><Button id="cancel-project-button" variant="ghost" disabled={projectPending}>취소</Button></DialogClose>
              <Button id="submit-project-button" type="submit" disabled={projectPending}>{projectPending ? 'Kroot 초기화 중…' : '프로젝트 만들기'}</Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
      <Dialog open={usageOpen} onOpenChange={setUsageOpen}>
        <DialogContent className="usage-dialog-content" aria-describedby="usage-dialog-description">
          <section className="usage-dialog">
            <header className="usage-heading">
              <div>
                <p className="eyebrow">AI USAGE</p>
                <DialogTitle>내 Claude Code 사용량</DialogTitle>
                <DialogDescription id="usage-dialog-description" className="muted">현재 로그인 사용자에게 귀속된 모델별 토큰과 SDK 보고 비용입니다.</DialogDescription>
              </div>
              <Select value={usageDays} onValueChange={(value) => { const days = value as '7' | '30' | '90'; setUsageDays(days); void loadUsage(days) }} disabled={usageLoading}>
                <SelectTrigger aria-label="사용량 조회 기간"><SelectValue /></SelectTrigger>
                <SelectContent><SelectItem value="7">최근 7일</SelectItem><SelectItem value="30">최근 30일</SelectItem><SelectItem value="90">최근 90일</SelectItem></SelectContent>
              </Select>
            </header>
            {usageLoading && !usageSummary
              ? <div className="usage-loading">사용량을 불러오는 중입니다…</div>
              : usageSummary && <>
                <div className="usage-cards">
                  <article><small>전체 토큰</small><strong>{formatCompactNumber(usageSummary.totals.totalTokens)}</strong><span>입력·출력·캐시 합계</span></article>
                  <article><small>대화 턴</small><strong>{formatCompactNumber(usageSummary.totals.turns)}</strong><span>완료된 SDK 결과</span></article>
                  <article><small>보고 비용</small><strong>{formatUSD(usageSummary.totals.costUsd)}</strong><span>USD · 기록 당시 값</span></article>
                  <article><small>캐시 활용</small><strong>{formatCompactNumber(usageSummary.totals.cacheReadInputTokens)}</strong><span>읽은 캐시 토큰</span></article>
                </div>
                <div className="usage-sections">
                  <section>
                    <h3>모델별 사용량</h3>
                    {usageSummary.byModel.length
                      ? <div className="usage-models">{usageSummary.byModel.map((item) => <article key={`${item.provider}:${item.model}`}>
                        <div><strong>{item.canonicalModel || item.model}</strong><small>{item.provider || 'provider unknown'} · {formatCompactNumber(item.turns)}턴</small></div>
                        <dl><div><dt>입력</dt><dd>{formatCompactNumber(item.inputTokens)}</dd></div><div><dt>출력</dt><dd>{formatCompactNumber(item.outputTokens)}</dd></div><div><dt>캐시 읽기</dt><dd>{formatCompactNumber(item.cacheReadInputTokens)}</dd></div><div><dt>비용</dt><dd>{formatUSD(item.costUsd)}</dd></div></dl>
                      </article>)}</div>
                      : <p className="usage-empty">선택한 기간에 기록된 사용량이 없습니다.</p>}
                  </section>
                  <section>
                    <h3>일별 사용량</h3>
                    {usageSummary.daily.length
                      ? <div className="usage-daily">{usageSummary.daily.map((item) => {
                        const peak = Math.max(...usageSummary.daily.map((day) => day.totalTokens), 1)
                        return <div key={item.date}><time>{formatUsageDate(item.date)}</time><span><i style={{ width: `${Math.max(2, item.totalTokens / peak * 100)}%` }} /></span><strong>{formatCompactNumber(item.totalTokens)}</strong></div>
                      })}</div>
                      : <p className="usage-empty">아직 일별 사용량이 없습니다.</p>}
                  </section>
                </div>
                <section className="usage-history" aria-labelledby="usage-history-title">
                  <header>
                    <div>
                      <h3 id="usage-history-title">상세 사용 내역</h3>
                      <p>모델별 측정 단위로 최근 사용 순서대로 표시합니다.</p>
                    </div>
                    <span>{formatCompactNumber(usageItems.length)}건 표시</span>
                  </header>
                  {usageItems.length
                    ? <div className="usage-history-table" role="table" aria-label="Claude Code 상세 사용 내역">
                      <div className="usage-history-head" role="row">
                        <span role="columnheader">실행 시각</span><span role="columnheader">프로젝트</span><span role="columnheader">모델</span><span role="columnheader">토큰</span><span role="columnheader">비용</span>
                      </div>
                      <div role="rowgroup">
                        {usageItems.map((item, index) => <article className="usage-history-row" role="row" key={`${item.requestId}:${item.model}:${item.occurredAt}:${index}`}>
                          <div role="cell" data-label="실행 시각"><time>{formatUsageDateTime(item.occurredAt)}</time><small>요청 {shortIdentifier(item.requestId)}</small></div>
                          <div role="cell" data-label="프로젝트"><strong>{item.projectName || `프로젝트 ${shortIdentifier(item.projectId)}`}</strong><small>대화 {shortIdentifier(item.conversationId)}</small></div>
                          <div role="cell" data-label="모델"><strong>{item.canonicalModel || item.model}</strong><small>{item.provider || '공급자 미확인'} <i className={`usage-status ${item.resultStatus === 'success' ? 'success' : ''}`}>{usageResultLabel(item.resultStatus)}</i></small></div>
                          <div role="cell" data-label="토큰"><strong>{formatCompactNumber(item.totalTokens)}</strong><small>입력 {formatCompactNumber(item.inputTokens)} · 출력 {formatCompactNumber(item.outputTokens)} · 캐시 {formatCompactNumber(item.cacheReadInputTokens + item.cacheCreationInputTokens)}</small></div>
                          <div role="cell" data-label="비용"><strong>{formatUSD(item.costUsd)}</strong><small>{usageCostSourceLabel(item.costSource)}{item.webSearchRequests ? ` · 검색 ${formatCompactNumber(item.webSearchRequests)}회` : ''}</small></div>
                        </article>)}
                      </div>
                    </div>
                    : <p className="usage-empty">선택한 기간에 상세 사용 내역이 없습니다.</p>}
                  {usageNextCursor && <Button className="usage-more" variant="secondary" disabled={usageLoadingMore} onClick={() => void loadMoreUsage()}>{usageLoadingMore ? '불러오는 중…' : '이전 내역 더 보기'}</Button>}
                </section>
                <p className="usage-note">입력 토큰 {formatCompactNumber(usageSummary.totals.inputTokens)} · 출력 토큰 {formatCompactNumber(usageSummary.totals.outputTokens)} · 캐시 생성 {formatCompactNumber(usageSummary.totals.cacheCreationInputTokens)} · 웹 검색 {formatCompactNumber(usageSummary.totals.webSearchRequests)}회</p>
              </>}
            <p className={`error${usageError ? '' : ' hidden'}`} role="alert">{usageError}</p>
            <div className="dialog-actions"><Button variant="secondary" disabled={usageLoading} onClick={() => void loadUsage(usageDays)}><RefreshCw aria-hidden="true" />새로고침</Button><DialogClose asChild><Button variant="ghost">닫기</Button></DialogClose></div>
          </section>
        </DialogContent>
      </Dialog>
      <Dialog open={previewLogsOpen} onOpenChange={setPreviewLogsOpen}>
        <DialogContent aria-describedby="preview-log-description">
          <div className="preview-log-dialog">
            <div><p className="eyebrow">PREVIEW LOG</p><DialogTitle>실행 로그</DialogTitle><DialogDescription id="preview-log-description" className="muted">최근 1MiB 범위의 프리뷰 프로세스 출력을 보여줍니다.</DialogDescription></div>
            <pre>{previewLogs}</pre>
            <div className="dialog-actions"><DialogClose asChild><Button variant="ghost">닫기</Button></DialogClose></div>
          </div>
        </DialogContent>
      </Dialog>
      <Dialog open={!!previewDeleteTarget} onOpenChange={(open) => { if (!open && !previewPending.startsWith('delete:')) setPreviewDeleteTarget(null) }}>
        <DialogContent aria-describedby="preview-delete-description">
          <div className="preview-delete-dialog">
            <div>
              <p className="eyebrow">DELETE PREVIEW</p>
              <DialogTitle>프로젝트 미리보기를 삭제할까요?</DialogTitle>
              <DialogDescription id="preview-delete-description" className="muted">
                {previewDeleteTarget && !['stopped', 'failed'].includes(previewDeleteTarget.status)
                  ? '실행 중인 개발 서버를 먼저 종료한 뒤 주소와 실행 기록을 삭제합니다.'
                  : '미리보기 주소와 실행 기록을 삭제합니다. 다음 실행 시 새로운 주소가 발급됩니다.'}
              </DialogDescription>
            </div>
            <p className={`error${previewError ? '' : ' hidden'}`} role="alert">{previewError}</p>
            <div className="dialog-actions">
              <DialogClose asChild><Button variant="ghost" disabled={previewPending.startsWith('delete:')}>취소</Button></DialogClose>
              <Button className="danger" disabled={previewPending.startsWith('delete:')} onClick={() => void handleDeletePreview()}>{previewPending.startsWith('delete:') ? '삭제 중…' : '삭제'}</Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

function MessageView({ message }: { message: ChatMessage }) {
  return (
    <article className={`message ${message.role}${message.pending ? ' pending' : ''}`}>
      <div className="role">{message.role === 'user' ? 'You' : message.role === 'assistant' ? 'Claude' : 'System'}</div>
      <div className="bubble">
        {message.attachments.length > 0 && <div className="message-attachments">
          {message.attachments.map((image, index) => <figure key={`${image.name}-${index}`}>
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img src={image.previewURL} alt={image.name || '첨부 이미지'} />
            <figcaption>{image.name || '첨부 이미지'}</figcaption>
          </figure>)}
        </div>}
        {message.role === 'assistant' ? <MarkdownContent source={message.text} /> : <div className="message-text">{message.text}</div>}
      </div>
    </article>
  )
}

function ThinkingView({ thinking }: { thinking: ThinkingMessage }) {
  return (
    <article className={`message runtime-message thinking-message${thinking.pending ? ' pending' : ''}`}>
      <div className="role">Claude · thinking</div>
      <pre className="runtime-payload thinking-payload">{thinking.text}</pre>
    </article>
  )
}

function ToolView({ tool }: { tool: ToolMessage }) {
  return (
    <article className={`message runtime-message tool-message ${tool.state}`} data-tool-name={tool.name}>
      <div className="role">Claude · tool</div>
      <div className="runtime-card">
        <div className="runtime-heading">
          <strong className="runtime-title">{tool.name}</strong>
          <span className="runtime-state">{tool.status || tool.state}</span>
        </div>
        {tool.input !== undefined && <RuntimePayload className="runtime-input" label="input" value={tool.input} />}
        {tool.result !== undefined && <RuntimePayload className="runtime-result" label="result" value={tool.result} />}
      </div>
    </article>
  )
}

function TaskView({ task }: { task: TaskMessage }) {
  const title = task.subagentType || task.taskType || '서브에이전트'
  const stateLabel = task.state === 'running'
    ? '진행 중'
    : task.state === 'complete'
      ? '완료'
      : task.state === 'cancelled'
        ? '중단됨'
        : '오류'
  const hasUsage = task.usage && (task.usage.totalTokens > 0 || task.usage.toolUses > 0 || task.usage.durationMs > 0)
  return (
    <article className={`message assistant runtime-message task-message ${task.state}`} data-task-id={task.taskId}>
      <div className="role">Claude · subagent</div>
      <div className="runtime-card subagent-card">
        <div className="runtime-heading subagent-heading">
          <div>
            <strong className="runtime-title">{title}</strong>
            {task.description && <span className="subagent-description">{task.description}</span>}
          </div>
          <span className="runtime-state">{task.state === 'running' && <i aria-hidden="true" />}{stateLabel}</span>
        </div>
        {task.summary && <p className="subagent-summary">{task.summary}</p>}
        {task.text && <div className="bubble subagent-markdown" aria-live={task.state === 'running' ? 'polite' : 'off'}><MarkdownContent source={task.text} /></div>}
        {task.thinking && <details className="subagent-thinking">
          <summary>서브에이전트 추론 보기</summary>
          <pre>{task.thinking}</pre>
        </details>}
        {task.tools.length > 0 && <div className="subagent-tools">
          {task.tools.map((tool) => <details key={tool.id} className={`subagent-tool ${tool.state}`} open>
            <summary>
              <span>{tool.name}</span>
              <small>{tool.state === 'running' ? `실행 중${tool.elapsedSeconds !== undefined ? ` · ${formatTaskDuration(tool.elapsedSeconds * 1_000)}` : ''}` : tool.state === 'complete' ? '완료' : tool.state === 'cancelled' ? '중단' : '오류'}</small>
            </summary>
            {tool.input !== undefined && <RuntimePayload className="runtime-input" label="input" value={tool.input} />}
            {tool.result !== undefined && <RuntimePayload className="runtime-result" label="result" value={tool.result} />}
          </details>)}
        </div>}
        {(hasUsage || task.lastToolName || task.outputFile) && <footer className="subagent-meta">
          {hasUsage && task.usage && <span>{formatCompactNumber(task.usage.totalTokens)} 토큰 · 도구 {formatCompactNumber(task.usage.toolUses)}회 · {formatTaskDuration(task.usage.durationMs)}</span>}
          {task.lastToolName && <span>최근 도구 {task.lastToolName}</span>}
          {task.outputFile && <span>결과 {task.outputFile}</span>}
        </footer>}
      </div>
    </article>
  )
}

function RuntimePayload({ className, label, value }: { className: string; label: string; value: unknown }) {
  return (
    <div className={`runtime-section ${className}`}>
      <span>{label}</span>
      <pre className="runtime-payload">{rawValue(value)}</pre>
    </div>
  )
}

function PermissionView({ permission, onAnswer }: { permission: PermissionMessage; onAnswer: (requestID: string, allow: boolean) => Promise<void> }) {
  const resolved = ['approved', 'denied', 'expired'].includes(permission.state)
  return (
    <article className={`message permission-message${permission.state === 'sending' ? ' sending' : ''}${resolved ? ` resolved ${permission.state}` : ''}`}>
      <div className="role">Claude · 승인이 필요합니다</div>
      <div className="permission-card">
        <div className="permission-heading"><span className="permission-badge">{permission.state === 'approved' ? '승인됨' : permission.state === 'denied' ? '거절됨' : permission.state === 'expired' ? '만료됨' : '승인 요청'}</span><strong>{permission.toolName} 실행</strong></div>
        <pre className="permission-detail">{permission.detail}</pre>
        <div className="permission-footer">
          <span className="permission-status">{permission.statusText}</span>
          <div className="permission-actions">
            <Button className="permission-deny" variant="ghost" disabled={permission.state !== 'pending'} onClick={() => void onAnswer(permission.requestId, false)}>거절</Button>
            <Button className="permission-allow" disabled={permission.state !== 'pending'} onClick={() => void onAnswer(permission.requestId, true)}>승인</Button>
          </div>
        </div>
      </div>
    </article>
  )
}

function permissionDetail(input: unknown) {
  if (input === undefined || input === null) return '도구 입력 정보가 없습니다.'
  if (typeof input === 'string') return input
  try { return JSON.stringify(input, null, 2) } catch { return String(input) }
}

function runtimeStateFromStatus(value: unknown): RuntimeState {
  const status = typeof value === 'string' ? value.toLowerCase() : ''
  if (['cancelled', 'canceled', 'killed', 'stopped'].includes(status)) return 'cancelled'
  if (['failed', 'error', 'denied', 'rejected'].includes(status)) return 'error'
  if (['running', 'pending', 'in_progress', 'paused'].includes(status)) return 'running'
  return 'complete'
}

function rawValue(value: unknown) {
  if (typeof value === 'string') return value
  if (value === undefined) return ''
  try { return JSON.stringify(value, null, 2) } catch { return String(value) }
}

function fileToBase64(file: File) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(new Error('이미지 파일을 읽지 못했습니다.'))
    reader.onload = () => {
      const value = String(reader.result || '')
      const comma = value.indexOf(',')
      if (comma < 0) reject(new Error('이미지 파일을 읽지 못했습니다.'))
      else resolve(value.slice(comma + 1))
    }
    reader.readAsDataURL(file)
  })
}

function formatBytes(bytes: number) {
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MiB`
  if (bytes >= 1 << 10) return `${(bytes / (1 << 10)).toFixed(1)} KiB`
  return `${bytes} B`
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : '알 수 없는 오류가 발생했습니다.'
}

function profileLabel(profile: string) {
  switch (profile) {
    case 'next': return 'Next.js'
    case 'vite': return 'Vite'
    default: return 'Node.js'
  }
}

const delay = (milliseconds: number) => new Promise((resolve) => setTimeout(resolve, milliseconds))

function formatCompactNumber(value: number) {
  return new Intl.NumberFormat('en-US', {
    notation: value >= 1_000 ? 'compact' : 'standard',
    compactDisplay: 'short',
    maximumFractionDigits: 1,
  }).format(value || 0)
}

function formatTaskDuration(milliseconds: number) {
  if (milliseconds < 1_000) return `${Math.round(milliseconds)}ms`
  if (milliseconds < 60_000) return `${(milliseconds / 1_000).toFixed(milliseconds < 10_000 ? 1 : 0)}초`
  const minutes = Math.floor(milliseconds / 60_000)
  const seconds = Math.round((milliseconds % 60_000) / 1_000)
  return seconds ? `${minutes}분 ${seconds}초` : `${minutes}분`
}

function formatUSD(value: number) {
  return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: value < 0.01 ? 4 : 2, maximumFractionDigits: value < 0.01 ? 6 : 2 }).format(value || 0)
}

function formatUsageDate(value: string) {
  const parsed = new Date(`${value}T00:00:00Z`)
  return Number.isNaN(parsed.getTime()) ? value : new Intl.DateTimeFormat('ko-KR', { month: 'short', day: 'numeric', timeZone: 'UTC' }).format(parsed)
}

function formatUsageDateTime(value: string) {
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? '-' : new Intl.DateTimeFormat('ko-KR', {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(parsed)
}

function shortIdentifier(value: string) {
  if (!value) return '-'
  const parts = value.split('-')
  const tail = parts.at(-1) || value
  return tail.length > 10 ? tail.slice(-10) : tail
}

function usageResultLabel(value: string) {
  if (value === 'success') return '완료'
  if (value === 'error') return '오류'
  if (value === 'cancelled' || value === 'aborted') return '중단'
  return value || '완료'
}

function usageCostSourceLabel(value: string) {
  if (value === 'manager-price-table') return 'Pie 가격표'
  if (value === 'claude-agent-sdk') return 'Claude SDK 보고값'
  return value || '산정 기준 미확인'
}
