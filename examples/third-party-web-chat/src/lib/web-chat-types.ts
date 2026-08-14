export type User = { username: string; displayName: string }

export type Session = {
  user: User
  csrfToken: string
  expiresAt: string
  workspace?: Workspace
}

export type Workspace = {
  status: string
  credentialConfigured?: boolean
  credentialVersion?: number
  updatedAt?: string
}

export type Project = {
  id: string
  name: string
  locale: string
  previewAppPath: string
  status: string
  lastError: string
  initializedAt: string | null
  createdAt: string
  updatedAt: string
}

export type ProjectApplication = {
  path: string
  name: string
  profile: 'next' | 'vite' | 'npm'
}

export type WorkspaceEntry = {
  name: string
  path: string
  type: 'directory' | 'file'
  size?: number
  modifiedAt: string
}

export type WorkspaceTree = {
  path: string
  entries: WorkspaceEntry[]
}

export type WorkspaceFile = {
  path: string
  content?: string
  revision: string
  size: number
  modifiedAt: string
  language: string
  created?: boolean
}

export type Preview = {
  id: string
  projectId: string
  appPath: string
  hostname: string
  port: number
  profile: 'auto' | 'next' | 'vite' | 'npm'
  visibility: 'private' | 'public'
  status: 'starting' | 'ready' | 'stopping' | 'stopped' | 'failed'
  lastError: string
  lastReadyAt: string | null
  expiresAt: string | null
  createdAt: string
  updatedAt: string
}

export type PreviewLaunch = {
  preview: Preview
  url: string
  accessUrl: string
}

export type Conversation = {
  id: string
  projectId: string
  status: string
  lastError: string
  createdAt: string
  updatedAt: string
  connection: ConversationConnection
}

export type ConversationConnection = {
  relayAvailable: boolean
  runtimeRunning: boolean
  runtimeHealthy: boolean
  clientConnected: boolean
  relayRegistered: boolean
  sessionStatus: string
  reason: string
  lastError: string
  lastHeartbeat: string | null
}

export type ImageAttachment = {
  data: string
  mimeType: string
  name: string
  size: number
  previewURL: string
}

export type ChatMessage = {
  kind: 'message'
  id: string
  role: 'user' | 'assistant' | 'system'
  text: string
  pending: boolean
  attachments: ImageAttachment[]
}

export type PermissionMessage = {
  kind: 'permission'
  id: string
  requestId: string
  toolName: string
  detail: string
  state: 'pending' | 'sending' | 'approved' | 'denied' | 'expired'
  statusText: string
}

export type RuntimeState = 'running' | 'complete' | 'error' | 'cancelled'

export type ThinkingMessage = {
  kind: 'thinking'
  id: string
  text: string
  pending: boolean
}

export type ToolMessage = {
  kind: 'tool'
  id: string
  requestId: string
  toolCallId?: string
  name: string
  input?: unknown
  result?: unknown
  status?: string
  state: RuntimeState
}

export type TaskUsage = {
  totalTokens: number
  toolUses: number
  durationMs: number
}

export type TaskToolMessage = {
  id: string
  toolCallId?: string
  name: string
  input?: unknown
  result?: unknown
  status?: string
  elapsedSeconds?: number
  retry?: unknown
  state: RuntimeState
}

export type TaskMessage = {
  kind: 'task'
  id: string
  taskId: string
  parentToolUseId?: string
  requestId: string
  eventType: 'task_started' | 'task_progress' | 'task_complete'
  subagentType?: string
  taskType?: string
  description: string
  summary: string
  text: string
  thinking: string
  tools: TaskToolMessage[]
  usage?: TaskUsage
  lastToolName?: string
  outputFile?: string
  status?: string
  data: Record<string, unknown>
  state: RuntimeState
}

export type ChatItem = ChatMessage | PermissionMessage | ThinkingMessage | ToolMessage | TaskMessage

export type ChatEvent = {
  sequence?: number | string
  requestId?: string
  type?: string
  at?: string
  data?: Record<string, unknown>
}

export type ConnectionState = {
  kind: '' | 'online' | 'busy' | 'error'
  text: string
}

export type UsageTotals = {
  turns: number
  inputTokens: number
  outputTokens: number
  cacheReadInputTokens: number
  cacheCreationInputTokens: number
  webSearchRequests: number
  totalTokens: number
  costUsd: number
}

export type UsageSummary = {
  from: string
  to: string
  currency: 'USD'
  costSource: string
  totals: UsageTotals
  byModel: Array<UsageTotals & { provider: string; model: string; canonicalModel: string }>
  daily: Array<UsageTotals & { date: string }>
}

export type UsageEvent = Omit<UsageTotals, 'turns'> & {
  occurredAt: string
  projectId: string
  projectName: string
  conversationId: string
  requestId: string
  resultStatus: string
  provider: string
  model: string
  canonicalModel: string
  costSource: string
}

export type UsageEventPage = {
  items: UsageEvent[]
  nextCursor: string
}
