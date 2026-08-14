import { currentRequestID } from './request-context.mjs'

export class PieAPIError extends Error {
  constructor(status, message) {
    super(message)
    this.name = 'PieAPIError'
    this.status = status
  }
}

export class PieManagerClient {
  constructor({ managerURL, integrationID, integrationToken, fetchImpl = fetch }) {
    this.managerURL = managerURL.replace(/\/$/, '')
    this.integrationID = integrationID
    this.integrationToken = integrationToken
    this.fetch = fetchImpl
  }

  async getUser(externalUserID) {
    return this.request(`/users/${segment(externalUserID)}`)
  }

  async usageSummary(externalUserID, days = 30) {
    return this.request(`/users/${segment(externalUserID)}/usage/summary?days=${encodeURIComponent(String(days))}`)
  }

  async usageEvents(externalUserID, days = 30, cursor = '', limit = 30) {
    const query = new URLSearchParams({ days: String(days), limit: String(limit) })
    if (cursor) query.set('cursor', cursor)
    return this.request(`/users/${segment(externalUserID)}/usage/events?${query}`)
  }

  async provisionUser(externalUserID, credential, idempotencyKey) {
    return this.request(`/users/${segment(externalUserID)}`, {
      method: 'PUT',
      idempotencyKey,
      body: { credential },
      timeoutMs: 120_000,
    })
  }

  async listProjects(externalUserID) {
    return this.request(`/users/${segment(externalUserID)}/projects`)
  }

  async createProject(externalUserID, value, idempotencyKey) {
    return this.request(`/users/${segment(externalUserID)}/projects`, {
      method: 'POST',
      idempotencyKey,
      body: value,
      timeoutMs: 190_000,
    })
  }

  async listProjectApplications(externalUserID, projectID) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/apps`)
  }

  async selectProjectApplication(externalUserID, projectID, appPath) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/preview-app`, {
      method: 'PUT',
      body: { appPath },
    })
  }

  async workspaceTree(externalUserID, projectID, conversationID, path, idempotencyKey) {
    const query = new URLSearchParams({ conversationId: conversationID, path })
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/workspace/tree?${query}`, {
      idempotencyKey,
      timeoutMs: 25_000,
    })
  }

  async workspaceFile(externalUserID, projectID, conversationID, path, idempotencyKey) {
    const query = new URLSearchParams({ conversationId: conversationID, path })
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/workspace/file?${query}`, {
      idempotencyKey,
      timeoutMs: 25_000,
    })
  }

  async saveWorkspaceFile(externalUserID, projectID, value, idempotencyKey) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/workspace/file`, {
      method: 'PUT',
      idempotencyKey,
      body: value,
      timeoutMs: 25_000,
    })
  }

  async listPreviews(externalUserID, projectID) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews`)
  }

  async createPreview(externalUserID, projectID, value, idempotencyKey) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews`, {
      method: 'POST',
      idempotencyKey,
      body: value,
    })
  }

  async getPreview(externalUserID, projectID, previewID) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews/${segment(previewID)}`)
  }

  async stopPreview(externalUserID, projectID, previewID) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews/${segment(previewID)}/stop`, { method: 'POST', body: {} })
  }

  async deletePreview(externalUserID, projectID, previewID) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews/${segment(previewID)}/record`, { method: 'DELETE' })
  }

  async previewAccess(externalUserID, projectID, previewID) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews/${segment(previewID)}/access`, { method: 'POST', body: {} })
  }

  async setPreviewVisibility(externalUserID, projectID, previewID, visibility) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews/${segment(previewID)}/visibility`, {
      method: 'PUT',
      body: { visibility },
    })
  }

  async restartPreview(externalUserID, projectID, previewID) {
    return this.request(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews/${segment(previewID)}/restart`, { method: 'POST', body: {} })
  }

  async previewLogs(externalUserID, projectID, previewID) {
    return this.requestText(`/users/${segment(externalUserID)}/projects/${segment(projectID)}/previews/${segment(previewID)}/logs`)
  }

  async createConversation(externalUserID, projectID, idempotencyKey) {
    return this.request(`/users/${segment(externalUserID)}/conversations`, {
      method: 'POST',
      idempotencyKey,
      body: { projectId: projectID },
    })
  }

  async listConversations(externalUserID) {
    return this.request(`/users/${segment(externalUserID)}/conversations`)
  }

  async getConversation(conversationID) {
    return this.request(`/conversations/${segment(conversationID)}`)
  }

  async deleteConversation(conversationID) {
    return this.request(`/conversations/${segment(conversationID)}`, { method: 'DELETE' })
  }

  async retryConversation(conversationID, idempotencyKey) {
    return this.request(`/conversations/${segment(conversationID)}/retry`, {
      method: 'POST',
      idempotencyKey,
      body: {},
    })
  }

  async sendMessage(conversationID, prompt, images, idempotencyKey) {
    return this.request(`/conversations/${segment(conversationID)}/messages`, {
      method: 'POST',
      idempotencyKey,
      body: { prompt, ...(images?.length ? { images } : {}) },
    })
  }

  async cancel(conversationID, idempotencyKey) {
    return this.request(`/conversations/${segment(conversationID)}/cancel`, {
      method: 'POST',
      idempotencyKey,
      body: {},
    })
  }

  async permission(conversationID, requestID, value, idempotencyKey) {
    return this.request(`/conversations/${segment(conversationID)}/permissions/${segment(requestID)}`, {
      method: 'POST',
      idempotencyKey,
      body: value,
    })
  }

  async events(conversationID, { lastEventID = '', signal } = {}) {
    const headers = this.headers()
    if (lastEventID) headers.set('Last-Event-ID', lastEventID)
    return this.fetch(this.url(`/conversations/${segment(conversationID)}/events`), { headers, signal })
  }

  async request(path, { method = 'GET', body, idempotencyKey, timeoutMs = 30_000 } = {}) {
    const headers = this.headers()
    if (idempotencyKey) headers.set('Idempotency-Key', idempotencyKey)
    const options = { method, headers, signal: AbortSignal.timeout(timeoutMs) }
    if (body !== undefined) {
      headers.set('Content-Type', 'application/json')
      options.body = JSON.stringify(body)
    }
    const response = await this.fetch(this.url(path), options)
    const text = await response.text()
    if (!response.ok) throw new PieAPIError(response.status, safeUpstreamMessage(text, response.status))
    return text ? JSON.parse(text) : null
  }

  async requestText(path, { timeoutMs = 30_000 } = {}) {
    const response = await this.fetch(this.url(path), { headers: this.headers(), signal: AbortSignal.timeout(timeoutMs) })
    const text = await response.text()
    if (!response.ok) throw new PieAPIError(response.status, safeUpstreamMessage(text, response.status))
    return text
  }

  headers() {
    const headers = new Headers({ Authorization: `Bearer ${this.integrationToken}`, Accept: 'application/json' })
    const requestID = currentRequestID()
    if (requestID) headers.set('X-Request-ID', requestID)
    return headers
  }

  url(path) {
    return `${this.managerURL}/v1/integrations/${segment(this.integrationID)}${path}`
  }
}

function segment(value) {
  return encodeURIComponent(String(value))
}

function safeUpstreamMessage(text, status) {
  const normalized = String(text ?? '').replace(/[\r\n]+/g, ' ').trim().slice(0, 300)
  return normalized || `Pie Manager request failed with HTTP ${status}`
}
