import { createHash, randomBytes, randomUUID } from 'node:crypto'

import { hashPassword, SessionStore, verifyPassword } from './auth.mjs'
import { UserExistsError } from './config.mjs'
import { createKrootCredential } from './kroot-credential.mjs'
import { MAX_CHAT_REQUEST_BYTES, sanitizeImageAttachments } from './image-attachments.mjs'
import { PieAPIError } from './pie-manager-client.mjs'
import { currentRequestID, runWithRequestContext } from './request-context.mjs'

const SESSION_COOKIE = 'pie_demo_session'
const SAFE_KEY = /^[a-zA-Z0-9._:-]{1,160}$/
const USERNAME = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/
const WORKSPACE_REVISION = /^sha256:[a-f0-9]{64}$/
const MAX_WORKSPACE_FILE_BYTES = 2 << 20

class HTTPError extends Error {
  constructor(status, message) {
    super(message)
    this.name = 'HTTPError'
    this.status = status
  }
}

export function createAPIHandler({ config, pieClient, now } = {}) {
  if (!config || !pieClient) throw new Error('config and pieClient are required')
  if (!config.userStore) throw new Error('config.userStore is required')
  const sessions = new SessionStore({ ttlMs: config.sessionTTLms, now })
  const attempts = new LoginAttempts({ now })

  return async function handleAPIRequest(request, { remoteAddress = 'unknown' } = {}) {
    return runWithRequestContext(request, async () => {
      try {
        const response = await routeAPI({ request, config, pieClient, sessions, attempts, remoteAddress })
        response.headers.set('Cache-Control', response.headers.get('Content-Type')?.startsWith('text/event-stream')
          ? 'no-cache, no-store, no-transform'
          : 'no-store')
        response.headers.set('X-Request-ID', currentRequestID())
        return response
      } catch (error) {
        const status = error instanceof HTTPError || error instanceof PieAPIError ? error.status : 500
        if (status >= 500) console.error(`[third-party-web-chat] request_id=${currentRequestID()}`, error)
        const response = json(status, { error: status >= 500 ? '서비스 처리 중 오류가 발생했습니다.' : error.message })
        response.headers.set('X-Request-ID', currentRequestID())
        return response
      }
    })
  }
}

async function routeAPI(context) {
  const { request, config, pieClient, sessions, attempts, remoteAddress } = context
  const url = new URL(request.url)
  if (request.method === 'GET' && url.pathname === '/api/health') {
    await config.userStore.ready()
    return json(200, { ok: true, userStore: config.userStoreKind || 'memory' })
  }

  if (request.method === 'POST' && url.pathname === '/api/auth/login') {
    verifySameOrigin(request, config)
    if (!attempts.allowed(remoteAddress)) throw new HTTPError(429, '로그인 시도가 너무 많습니다. 잠시 후 다시 시도하세요.')
    const body = await readJSON(request, 16 << 10)
    const username = typeof body.username === 'string' ? body.username : ''
    const user = await config.userStore.get(username)
    const fallbackHash = config.fallbackPasswordHash
    const passwordMatches = await verifyPassword(body.password, user?.passwordHash ?? fallbackHash)
    if (!user || !passwordMatches) {
      attempts.failed(remoteAddress)
      throw new HTTPError(401, '아이디 또는 비밀번호가 올바르지 않습니다.')
    }
    attempts.succeeded(remoteAddress)
    const created = sessions.create(username)
    return json(200, publicSession(user, created.session), {
      'Set-Cookie': sessionCookie(created.token, config, created.session.expiresAt),
    })
  }

  if (request.method === 'POST' && url.pathname === '/api/auth/signup') {
    verifySameOrigin(request, config)
    if (!config.registrationEnabled) {
      throw new HTTPError(404, '회원가입이 비활성화되어 있습니다.')
    }
    if (!attempts.allowed(remoteAddress)) throw new HTTPError(429, '요청이 너무 많습니다. 잠시 후 다시 시도하세요.')
    const body = await readJSON(request, 20 << 10)
    const username = typeof body.username === 'string' ? body.username.trim() : ''
    const displayName = typeof body.displayName === 'string' ? body.displayName.trim() : ''
    if (!USERNAME.test(username)) {
      throw new HTTPError(400, '아이디는 영문 또는 숫자로 시작하고 영문, 숫자, 점, 밑줄, 하이픈만 사용할 수 있습니다.')
    }
    if (!displayName || displayName.length > 120) throw new HTTPError(400, '이름을 120자 이내로 입력하세요.')
    if (typeof body.password !== 'string' || body.password.length < 10 || body.password.length > 1024) {
      throw new HTTPError(400, '비밀번호는 10자 이상이어야 합니다.')
    }
    let user
    try {
      user = await config.userStore.create({
        username,
        displayName,
        externalUserId: `signup-${randomUUID()}`,
        passwordHash: await hashPassword(body.password),
        credential: createKrootCredential({
          pat: config.signupKrootPAT || `kpat_demo_${randomBytes(32).toString('base64url')}`,
          serverURL: config.krootServerURL,
          relayURL: config.krootRelayURL,
        }),
      })
    } catch (error) {
      if (error instanceof UserExistsError) throw new HTTPError(409, '이미 사용 중인 아이디입니다.')
      throw error
    }
    const binding = await provisionWorkspace(config, pieClient, user)
    attempts.succeeded(remoteAddress)
    const created = sessions.create(username)
    return json(201, { ...publicSession(user, created.session), workspace: sanitizeWorkspace(binding) }, {
      'Set-Cookie': sessionCookie(created.token, config, created.session.expiresAt),
    })
  }

  const authenticated = await authenticate(request, config, sessions)
  if (!authenticated) throw new HTTPError(401, '로그인이 필요합니다.')
  const { user, session, token } = authenticated
  if (request.method === 'GET' && url.pathname === '/api/auth/me') return json(200, publicSession(user, session))
  if (request.method !== 'GET' && request.method !== 'HEAD') verifyMutation(request, config, session)

  if (request.method === 'POST' && url.pathname === '/api/auth/logout') {
    sessions.delete(token)
    return json(200, { ok: true }, { 'Set-Cookie': expiredSessionCookie(config) })
  }
  if (request.method === 'GET' && url.pathname === '/api/workspace') {
    try {
      return json(200, sanitizeWorkspace(await pieClient.getUser(user.externalUserId)))
    } catch (error) {
      if (error instanceof PieAPIError && error.status === 404) return json(200, { status: 'not_provisioned' })
      throw error
    }
  }
  if (request.method === 'GET' && url.pathname === '/api/usage') {
    const days = Number(url.searchParams.get('days') || 30)
    if (![7, 30, 90].includes(days)) throw new HTTPError(400, '사용량 조회 기간이 올바르지 않습니다.')
    return json(200, sanitizeUsageSummary(await pieClient.usageSummary(user.externalUserId, days)))
  }
  if (request.method === 'GET' && url.pathname === '/api/usage/events') {
    const days = Number(url.searchParams.get('days') || 30)
    if (![7, 30, 90].includes(days)) throw new HTTPError(400, '사용량 조회 기간이 올바르지 않습니다.')
    const cursor = url.searchParams.get('cursor') || ''
    if (cursor && !/^[a-zA-Z0-9_-]{1,1024}$/.test(cursor)) throw new HTTPError(400, '사용량 목록 커서가 올바르지 않습니다.')
    return json(200, sanitizeUsageEvents(await pieClient.usageEvents(user.externalUserId, days, cursor, 30)))
  }
  if (request.method === 'POST' && url.pathname === '/api/workspace/provision') {
    await readJSON(request, 8 << 10)
    const binding = await provisionWorkspace(config, pieClient, user)
    return json(200, sanitizeWorkspace(binding))
  }
  if (request.method === 'GET' && url.pathname === '/api/projects') {
    const projects = await pieClient.listProjects(user.externalUserId)
    return json(200, projects.map(sanitizeProject))
  }
  if (request.method === 'POST' && url.pathname === '/api/projects') {
    const body = await readJSON(request, 16 << 10)
    const name = typeof body.name === 'string' ? body.name.trim() : ''
    const locale = typeof body.locale === 'string' ? body.locale.trim() : 'ko'
    if (!name || [...name].length > 120 || /[\u0000-\u001f\u007f]/u.test(name)) {
      throw new HTTPError(400, '프로젝트명을 1~120자로 입력하세요.')
    }
    if (!['ko', 'en', 'ja', 'zh'].includes(locale)) throw new HTTPError(400, '지원하지 않는 프로젝트 언어입니다.')
    const project = await pieClient.createProject(user.externalUserId, { name, locale }, clientKey(body.clientRequestId))
    return json(201, sanitizeProject(project))
  }
  const workspaceRoute = url.pathname.match(/^\/api\/projects\/([^/]+)\/workspace\/(tree|file)$/)
  if (workspaceRoute) {
    const projectID = decodeSegment(workspaceRoute[1])
    const resource = workspaceRoute[2]
    await assertOwnedProject(pieClient, user, projectID)
    if (resource === 'tree' && request.method === 'GET') {
      const conversationID = decodeSafeQueryID(url.searchParams.get('conversationId'), '대화를 선택하세요.')
      const conversation = await assertOwnedConversation(pieClient, user, conversationID)
      if (conversation.projectId !== projectID) throw new HTTPError(409, '선택한 대화와 프로젝트가 일치하지 않습니다.')
      const path = normalizeWorkspacePath(url.searchParams.get('path') || '', true)
      const result = await pieClient.workspaceTree(user.externalUserId, projectID, conversationID, path, randomUUID())
      return json(200, sanitizeWorkspaceTree(result, path))
    }
    if (resource === 'file' && request.method === 'GET') {
      const conversationID = decodeSafeQueryID(url.searchParams.get('conversationId'), '대화를 선택하세요.')
      const conversation = await assertOwnedConversation(pieClient, user, conversationID)
      if (conversation.projectId !== projectID) throw new HTTPError(409, '선택한 대화와 프로젝트가 일치하지 않습니다.')
      const path = normalizeWorkspacePath(url.searchParams.get('path') || '')
      const result = await pieClient.workspaceFile(user.externalUserId, projectID, conversationID, path, randomUUID())
      return json(200, sanitizeWorkspaceFile(result, path, true))
    }
    if (resource === 'file' && request.method === 'PUT') {
      const body = await readJSON(request, MAX_WORKSPACE_FILE_BYTES + (16 << 10))
      const conversationID = decodeSafeQueryID(body.conversationId, '대화를 선택하세요.')
      const conversation = await assertOwnedConversation(pieClient, user, conversationID)
      if (conversation.projectId !== projectID) throw new HTTPError(409, '선택한 대화와 프로젝트가 일치하지 않습니다.')
      const path = normalizeWorkspacePath(body.path)
      if (typeof body.content !== 'string' || Buffer.byteLength(body.content) > MAX_WORKSPACE_FILE_BYTES) {
        throw new HTTPError(413, '파일은 UTF-8 기준 2MiB 이하만 저장할 수 있습니다.')
      }
      const create = body.create === true
      const baseRevision = typeof body.baseRevision === 'string' ? body.baseRevision : ''
      if ((!create || baseRevision) && !WORKSPACE_REVISION.test(baseRevision)) {
        throw new HTTPError(400, '파일 버전 정보가 올바르지 않습니다.')
      }
      const result = await pieClient.saveWorkspaceFile(user.externalUserId, projectID, {
        conversationId: conversationID,
        path,
        content: body.content,
        baseRevision,
        create,
      }, clientKey(body.clientRequestId))
      return json(200, sanitizeWorkspaceFile(result, path, false))
    }
    throw new HTTPError(405, '허용되지 않는 요청입니다.')
  }
  const projectAppsRoute = url.pathname.match(/^\/api\/projects\/([^/]+)\/(apps|preview-app)$/)
  if (projectAppsRoute) {
    const projectID = decodeSegment(projectAppsRoute[1])
    const action = projectAppsRoute[2]
    await assertOwnedProject(pieClient, user, projectID)
    if (action === 'apps' && request.method === 'GET') {
      const applications = await pieClient.listProjectApplications(user.externalUserId, projectID)
      return json(200, applications.map(sanitizeProjectApplication))
    }
    if (action === 'preview-app' && request.method === 'PUT') {
      const body = await readJSON(request, 8 << 10)
      const appPath = normalizePreviewAppPath(body.appPath)
      const project = await pieClient.selectProjectApplication(user.externalUserId, projectID, appPath)
      return json(200, sanitizeProject(project))
    }
    throw new HTTPError(405, '허용되지 않는 요청입니다.')
  }
  const previewRoute = url.pathname.match(/^\/api\/projects\/([^/]+)\/previews(?:\/([^/]+)(?:\/(access|restart|logs|visibility|stop))?)?$/)
  if (previewRoute) {
    const projectID = decodeSegment(previewRoute[1])
    const previewID = previewRoute[2] ? decodeSegment(previewRoute[2]) : ''
    const action = previewRoute[3] || ''
    await assertOwnedProject(pieClient, user, projectID)
    if (!previewID && request.method === 'GET') {
      const values = await pieClient.listPreviews(user.externalUserId, projectID)
      return json(200, values.map(sanitizePreview))
    }
    if (!previewID && request.method === 'POST') {
      const body = await readJSON(request, 16 << 10)
      const appPath = normalizePreviewAppPath(body.appPath)
      const visibility = typeof body.visibility === 'string' ? body.visibility : 'private'
      const profile = typeof body.profile === 'string' ? body.profile : 'auto'
      const ttlSeconds = body.ttlSeconds === undefined ? 14_400 : Number(body.ttlSeconds)
      if (!['private', 'public'].includes(visibility)) throw new HTTPError(400, '프리뷰 공개 범위가 올바르지 않습니다.')
      if (!['auto', 'next', 'vite', 'npm'].includes(profile)) throw new HTTPError(400, '지원하지 않는 프리뷰 실행 방식입니다.')
      if (!Number.isInteger(ttlSeconds) || ttlSeconds < 60 || ttlSeconds > 86_400) throw new HTTPError(400, '프리뷰 유지 시간은 1분에서 24시간 사이여야 합니다.')
      const launch = await pieClient.createPreview(user.externalUserId, projectID, { appPath, visibility, profile, ttlSeconds }, clientKey(body.clientRequestId))
      return json(202, sanitizePreviewLaunch(launch))
    }
    if (!previewID || action === 'logs' && request.method !== 'GET') throw new HTTPError(405, '허용되지 않는 요청입니다.')
    if (!action && request.method === 'GET') {
      return json(200, sanitizePreview(await pieClient.getPreview(user.externalUserId, projectID, previewID)))
    }
    if (!action && request.method === 'DELETE') {
      await pieClient.deletePreview(user.externalUserId, projectID, previewID)
      return json(200, { id: previewID, deleted: true })
    }
    if (action === 'access' && request.method === 'POST') {
      await readJSON(request, 8 << 10)
      return json(200, sanitizePreviewLaunch(await pieClient.previewAccess(user.externalUserId, projectID, previewID)))
    }
    if (action === 'visibility' && request.method === 'PUT') {
      const body = await readJSON(request, 8 << 10)
      const visibility = typeof body.visibility === 'string' ? body.visibility : ''
      if (!['private', 'public'].includes(visibility)) throw new HTTPError(400, '프리뷰 공개 범위가 올바르지 않습니다.')
      return json(200, sanitizePreviewLaunch(await pieClient.setPreviewVisibility(user.externalUserId, projectID, previewID, visibility)))
    }
    if (action === 'stop' && request.method === 'POST') {
      await readJSON(request, 8 << 10)
      return json(200, sanitizePreview(await pieClient.stopPreview(user.externalUserId, projectID, previewID)))
    }
    if (action === 'restart' && request.method === 'POST') {
      await readJSON(request, 8 << 10)
      return json(202, sanitizePreviewLaunch(await pieClient.restartPreview(user.externalUserId, projectID, previewID)))
    }
    if (action === 'logs' && request.method === 'GET') {
      const logs = await pieClient.previewLogs(user.externalUserId, projectID, previewID)
      return json(200, { logs })
    }
    throw new HTTPError(405, '허용되지 않는 요청입니다.')
  }
  if (request.method === 'GET' && url.pathname === '/api/conversations') {
    const binding = await pieClient.getUser(user.externalUserId)
    const conversations = await pieClient.listConversations(user.externalUserId)
    return json(200, conversations
      .filter((conversation) => conversation.integrationUserId === binding.id)
      .map(sanitizeConversation))
  }
  if (request.method === 'POST' && url.pathname === '/api/conversations') {
    const body = await readJSON(request, 8 << 10)
    if (typeof body.projectId !== 'string' || !SAFE_KEY.test(body.projectId)) throw new HTTPError(400, '프로젝트를 선택하세요.')
    const projects = await pieClient.listProjects(user.externalUserId)
    const project = projects.find((candidate) => candidate.id === body.projectId && candidate.status === 'ready')
    if (!project) throw new HTTPError(409, '선택한 프로젝트가 준비되지 않았습니다.')
    const conversation = await pieClient.createConversation(user.externalUserId, project.id, clientKey(body.clientRequestId))
    await assertOwnedConversation(pieClient, user, conversation.id, conversation)
    return json(201, sanitizeConversation(conversation))
  }

  const conversationRoute = url.pathname.match(/^\/api\/conversations\/([^/]+)(?:\/(messages|events|cancel|retry))?$/)
  if (conversationRoute) {
    const conversationID = decodeSegment(conversationRoute[1])
    const action = conversationRoute[2] || ''
    const conversation = await assertOwnedConversation(pieClient, user, conversationID)
    if (request.method === 'GET' && action === '') return json(200, sanitizeConversation(conversation))
    if (request.method === 'DELETE' && action === '') {
      await pieClient.deleteConversation(conversationID)
      return json(200, { ok: true })
    }
    if (request.method === 'POST' && action === 'retry') {
      const body = await readJSON(request, 8 << 10)
      const retried = await pieClient.retryConversation(conversationID, clientKey(body.clientRequestId))
      return json(202, sanitizeConversation(retried))
    }
    if (request.method === 'POST' && action === 'messages') {
      const body = await readJSON(request, MAX_CHAT_REQUEST_BYTES)
      if (typeof body.prompt !== 'string' || !body.prompt.trim() || body.prompt.length > 256 << 10) {
        throw new HTTPError(400, '메시지를 입력하세요.')
      }
      let images
      try {
        images = sanitizeImageAttachments(body.images)
      } catch (error) {
        throw new HTTPError(400, error.message)
      }
      const key = clientKey(body.clientRequestId)
      await pieClient.sendMessage(conversationID, body.prompt, images, key)
      return json(202, { accepted: true, requestId: key })
    }
    if (request.method === 'POST' && action === 'cancel') {
      const body = await readJSON(request, 8 << 10)
      await pieClient.cancel(conversationID, clientKey(body.clientRequestId))
      return json(202, { accepted: true })
    }
    if (request.method === 'GET' && action === 'events') {
      const resumeAfter = url.searchParams.get('after') || ''
      const upstream = await pieClient.events(conversationID, {
        lastEventID: request.headers.get('last-event-id') || resumeAfter,
        signal: request.signal,
      })
      if (!upstream.ok) {
        const message = (await upstream.text()).trim().slice(0, 300)
        throw new PieAPIError(upstream.status, message || '이벤트 연결에 실패했습니다.')
      }
      return new Response(upstream.body, {
        status: 200,
        headers: {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache, no-store, no-transform',
          'X-Accel-Buffering': 'no',
        },
      })
    }
  }

  const permissionRoute = url.pathname.match(/^\/api\/conversations\/([^/]+)\/permissions\/([^/]+)$/)
  if (permissionRoute && request.method === 'POST') {
    const conversationID = decodeSegment(permissionRoute[1])
    const requestID = decodeSegment(permissionRoute[2])
    await assertOwnedConversation(pieClient, user, conversationID)
    const body = await readJSON(request, 32 << 10)
    if (typeof body.allow !== 'boolean') throw new HTTPError(400, 'allow 값이 필요합니다.')
    const value = { allow: body.allow }
    if (body.updatedInput !== undefined) value.updatedInput = body.updatedInput
    await pieClient.permission(conversationID, requestID, value, clientKey(body.clientRequestId))
    return json(202, { accepted: true })
  }
  throw new HTTPError(404, '요청한 API를 찾을 수 없습니다.')
}

async function assertOwnedConversation(pieClient, user, conversationID, knownConversation) {
  let binding
  try {
    binding = await pieClient.getUser(user.externalUserId)
  } catch (error) {
    if (error instanceof PieAPIError && error.status === 404) throw new HTTPError(409, '먼저 전용 작업공간을 준비하세요.')
    throw error
  }
  const conversation = knownConversation ?? await pieClient.getConversation(conversationID)
  if (conversation.integrationUserId !== binding.id) throw new HTTPError(404, '대화를 찾을 수 없습니다.')
  return conversation
}

async function assertOwnedProject(pieClient, user, projectID) {
  const projects = await pieClient.listProjects(user.externalUserId)
  const project = projects.find((candidate) => candidate.id === projectID && candidate.status === 'ready')
  if (!project) throw new HTTPError(404, '프로젝트를 찾을 수 없습니다.')
  return project
}

async function authenticate(request, config, sessions) {
  const token = parseCookies(request.headers.get('cookie') || '')[SESSION_COOKIE]
  const session = sessions.get(token)
  if (!session) return null
  const user = await config.userStore.get(session.username)
  if (!user) {
    sessions.delete(token)
    return null
  }
  return { token, session, user }
}

async function provisionWorkspace(config, pieClient, user) {
  await config.userStore.setProvisioningState(user.username, 'provisioning')
  try {
    const binding = await pieClient.provisionUser(
      user.externalUserId,
      user.credential,
      stableKey('signup', config.integrationID, user.externalUserId),
    )
    await config.userStore.setProvisioningState(user.username, 'ready')
    return binding
  } catch (error) {
    try {
      await config.userStore.setProvisioningState(user.username, 'failed', provisionErrorMessage(error))
    } catch (stateError) {
      console.error('[third-party-web-chat] failed to persist provisioning failure', stateError)
    }
    throw error
  }
}

function provisionErrorMessage(error) {
  if (error instanceof PieAPIError) return `Pie Manager HTTP ${error.status}: ${error.message}`.slice(0, 500)
  return '작업공간 준비 중 내부 오류가 발생했습니다.'
}

function verifyMutation(request, config, session) {
  verifySameOrigin(request, config)
  if (request.headers.get('x-csrf-token') !== session.csrfToken) throw new HTTPError(403, 'CSRF 검증에 실패했습니다.')
}

function verifySameOrigin(request, config) {
  const origin = request.headers.get('origin')
  if (!origin) return
  const requestURL = new URL(request.url)
  const protocol = request.headers.get('x-forwarded-proto')?.split(',', 1)[0]?.trim() || requestURL.protocol.slice(0, -1)
  const host = request.headers.get('x-forwarded-host')?.split(',', 1)[0]?.trim()
    || request.headers.get('host')
    || requestURL.host
  const expected = config.publicOrigin || `${protocol}://${host}`
  if (origin !== expected) throw new HTTPError(403, '허용되지 않은 요청 출처입니다.')
}

async function readJSON(request, maxBytes) {
  const contentType = request.headers.get('content-type') || ''
  if (!contentType.toLowerCase().startsWith('application/json')) throw new HTTPError(415, 'application/json 요청이 필요합니다.')
  const declared = Number(request.headers.get('content-length') || 0)
  if (Number.isFinite(declared) && declared > maxBytes) throw new HTTPError(413, '요청 본문이 너무 큽니다.')
  const text = await request.text()
  if (Buffer.byteLength(text) > maxBytes) throw new HTTPError(413, '요청 본문이 너무 큽니다.')
  try {
    const parsed = JSON.parse(text || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('object required')
    return parsed
  } catch {
    throw new HTTPError(400, '올바른 JSON 객체가 필요합니다.')
  }
}

function json(status, value, headers = {}) {
  return Response.json(value, { status, headers: { 'Cache-Control': 'no-store', ...headers } })
}

function sanitizeUsageSummary(value) {
  const totals = sanitizeUsageTotals(value?.totals)
  return {
    from: typeof value?.from === 'string' ? value.from : '',
    to: typeof value?.to === 'string' ? value.to : '',
    currency: value?.currency === 'USD' ? 'USD' : 'USD',
    costSource: typeof value?.costSource === 'string' ? value.costSource : '',
    totals,
    byModel: Array.isArray(value?.byModel) ? value.byModel.slice(0, 50).map((item) => ({
      provider: safeText(item?.provider, 128),
      model: safeText(item?.model, 256),
      canonicalModel: safeText(item?.canonicalModel, 256),
      ...sanitizeUsageTotals(item),
    })) : [],
    daily: Array.isArray(value?.daily) ? value.daily.slice(0, 366).map((item) => ({
      date: safeText(item?.date, 10),
      ...sanitizeUsageTotals(item),
    })) : [],
  }
}

function sanitizeUsageEvents(value) {
  const cursor = typeof value?.nextCursor === 'string' && /^[a-zA-Z0-9_-]{1,1024}$/.test(value.nextCursor)
    ? value.nextCursor
    : ''
  return {
    items: Array.isArray(value?.items) ? value.items.slice(0, 100).map((item) => {
      const totals = sanitizeUsageTotals(item)
      return {
        occurredAt: safeTimestamp(item?.occurredAt),
        projectId: safeText(item?.projectId, 160),
        projectName: safeText(item?.projectName, 120),
        conversationId: safeText(item?.conversationId, 160),
        requestId: safeText(item?.requestId, 160),
        resultStatus: safeText(item?.resultStatus, 64),
        provider: safeText(item?.provider, 128),
        model: safeText(item?.model, 256),
        canonicalModel: safeText(item?.canonicalModel, 256),
        costSource: safeText(item?.costSource, 64),
        inputTokens: totals.inputTokens,
        outputTokens: totals.outputTokens,
        cacheReadInputTokens: totals.cacheReadInputTokens,
        cacheCreationInputTokens: totals.cacheCreationInputTokens,
        webSearchRequests: totals.webSearchRequests,
        totalTokens: totals.totalTokens,
        costUsd: totals.costUsd,
      }
    }) : [],
    nextCursor: cursor,
  }
}

function sanitizeUsageTotals(value) {
  const number = (candidate) => typeof candidate === 'number' && Number.isFinite(candidate) && candidate >= 0 ? candidate : 0
  return {
    turns: number(value?.turns),
    inputTokens: number(value?.inputTokens),
    outputTokens: number(value?.outputTokens),
    cacheReadInputTokens: number(value?.cacheReadInputTokens),
    cacheCreationInputTokens: number(value?.cacheCreationInputTokens),
    webSearchRequests: number(value?.webSearchRequests),
    totalTokens: number(value?.totalTokens),
    costUsd: number(value?.costUsd),
  }
}

function safeText(value, maxLength) {
  return typeof value === 'string' ? value.slice(0, maxLength) : ''
}

function safeTimestamp(value) {
  if (typeof value !== 'string') return ''
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? '' : parsed.toISOString()
}

function publicSession(user, session) {
  return {
    user: { username: user.username, displayName: user.displayName },
    csrfToken: session.csrfToken,
    expiresAt: new Date(session.expiresAt).toISOString(),
  }
}

function sanitizeWorkspace(binding) {
  return {
    status: binding.status,
    credentialConfigured: Number(binding.credentialVersion || 0) > 0,
    credentialVersion: Number(binding.credentialVersion || 0),
    updatedAt: binding.updatedAt,
  }
}

function sanitizeProject(value) {
  return {
    id: value.id,
    name: value.name,
    locale: value.locale,
    previewAppPath: typeof value.previewAppPath === 'string' ? value.previewAppPath : '',
    status: value.status,
    lastError: value.lastError || '',
    initializedAt: value.initializedAt || null,
    createdAt: value.createdAt,
    updatedAt: value.updatedAt,
  }
}

function normalizeWorkspacePath(value, allowEmpty = false) {
  if (typeof value !== 'string' || value.length > 4096 || value.startsWith('/') || value.includes('\\') || /[\u0000-\u001f\u007f]/u.test(value)) {
    throw new HTTPError(400, '파일 경로는 프로젝트 내부의 상대 경로여야 합니다.')
  }
  const segments = value.split('/').filter((segment) => segment && segment !== '.')
  if (!allowEmpty && segments.length === 0) throw new HTTPError(400, '파일 경로가 필요합니다.')
  if (segments.some((segment) => segment === '..' || Buffer.byteLength(segment) > 255)) {
    throw new HTTPError(400, '프로젝트 밖의 파일에는 접근할 수 없습니다.')
  }
  return segments.join('/')
}

function sanitizeWorkspaceTree(value, expectedPath) {
  if (!value || typeof value !== 'object' || value.path !== expectedPath || !Array.isArray(value.entries)) {
    throw new HTTPError(502, '파일 목록 응답을 검증하지 못했습니다.')
  }
  return {
    path: expectedPath,
    entries: value.entries.slice(0, 500).map((entry) => {
      const type = entry?.type === 'directory' ? 'directory' : entry?.type === 'file' ? 'file' : ''
      const path = normalizeWorkspacePath(entry?.path || '')
      const name = typeof entry?.name === 'string' ? entry.name : ''
      if (!type || !name || name.includes('/') || name.includes('\\') || path.split('/').at(-1) !== name) {
        throw new HTTPError(502, '파일 목록 항목을 검증하지 못했습니다.')
      }
      return {
        name,
        path,
        type,
        size: type === 'file' && Number.isSafeInteger(entry?.size) && entry.size >= 0 ? entry.size : undefined,
        modifiedAt: safeTimestamp(entry?.modifiedAt),
      }
    }),
  }
}

function sanitizeWorkspaceFile(value, expectedPath, includeContent) {
  if (!value || typeof value !== 'object' || value.path !== expectedPath || !WORKSPACE_REVISION.test(value.revision || '')) {
    throw new HTTPError(502, '파일 응답을 검증하지 못했습니다.')
  }
  const content = includeContent && typeof value.content === 'string' ? value.content : undefined
  if (includeContent && (content === undefined || Buffer.byteLength(content) > MAX_WORKSPACE_FILE_BYTES)) {
    throw new HTTPError(502, '파일 내용이 올바르지 않습니다.')
  }
  return {
    path: expectedPath,
    ...(includeContent ? { content } : {}),
    revision: value.revision,
    size: Number.isSafeInteger(value.size) && value.size >= 0 ? value.size : Buffer.byteLength(content || ''),
    modifiedAt: safeTimestamp(value.modifiedAt),
    language: typeof value.language === 'string' ? value.language.slice(0, 32) : 'plaintext',
    created: value.created === true,
  }
}

function decodeSafeQueryID(value, message) {
  if (typeof value !== 'string' || !SAFE_KEY.test(value)) throw new HTTPError(400, message)
  return value
}

function sanitizeProjectApplication(value) {
  const appPath = normalizePreviewAppPath(value.path)
  const name = typeof value.name === 'string' ? value.name.trim().slice(0, 120) : ''
  const profile = ['next', 'vite', 'npm'].includes(value.profile) ? value.profile : 'npm'
  return { path: appPath, name: name || (appPath === '.' ? '프로젝트 웹 앱' : appPath.split('/').at(-1)), profile }
}

function sanitizePreview(value) {
  return {
    id: value.id,
    projectId: value.projectId,
    appPath: typeof value.appPath === 'string' && value.appPath.trim() ? value.appPath : '.',
    hostname: value.hostname,
    port: Number(value.port || 0),
    profile: value.profile,
    visibility: value.visibility,
    status: value.status,
    lastError: value.lastError || '',
    lastReadyAt: value.lastReadyAt || null,
    expiresAt: value.expiresAt || null,
    createdAt: value.createdAt,
    updatedAt: value.updatedAt,
  }
}

function normalizePreviewAppPath(value) {
  if (value === undefined || value === null) return '.'
  if (typeof value !== 'string') throw new HTTPError(400, '앱 경로가 올바르지 않습니다.')
  const trimmed = value.trim()
  if (!trimmed || trimmed === '.') return '.'
  if (trimmed.length > 512 || trimmed.startsWith('/') || trimmed.includes('\\') || /[\u0000-\u001f\u007f]/u.test(trimmed)) {
    throw new HTTPError(400, '앱 경로는 프로젝트 내부의 상대 경로여야 합니다.')
  }
  const segments = []
  for (const segment of trimmed.split('/')) {
    if (!segment || segment === '.') continue
    if (segment === '..' || Buffer.byteLength(segment, 'utf8') > 255) {
      throw new HTTPError(400, '앱 경로는 프로젝트 내부의 상대 경로여야 합니다.')
    }
    segments.push(segment)
  }
  return segments.join('/') || '.'
}

function sanitizePreviewLaunch(value) {
  const preview = sanitizePreview(value.preview)
  return {
    preview,
    url: safePreviewURL(value.url, preview.hostname),
    accessUrl: value.accessUrl ? safePreviewURL(value.accessUrl, preview.hostname) : '',
  }
}

function safePreviewURL(value, hostname) {
  try {
    const parsed = new URL(value)
    if (parsed.protocol !== 'https:' || parsed.hostname !== hostname) throw new Error('unexpected preview origin')
    return parsed.toString()
  } catch {
    throw new HTTPError(502, '프리뷰 주소 검증에 실패했습니다.')
  }
}

function sanitizeConversation(value) {
  const connection = value && typeof value.connection === 'object' && value.connection !== null
    ? value.connection
    : {}
  return {
    id: value.id,
    projectId: value.projectId,
    status: value.status,
    lastError: value.lastError || '',
    createdAt: value.createdAt,
    updatedAt: value.updatedAt,
    connection: {
      relayAvailable: connection.relayAvailable === true,
      runtimeRunning: connection.runtimeRunning === true,
      runtimeHealthy: connection.runtimeHealthy === true,
      clientConnected: connection.clientConnected === true,
      relayRegistered: connection.relayRegistered === true,
      sessionStatus: typeof connection.sessionStatus === 'string' ? connection.sessionStatus.slice(0, 32) : '',
      reason: typeof connection.reason === 'string' ? connection.reason.slice(0, 64) : 'unknown',
      lastError: typeof connection.lastError === 'string' ? connection.lastError.slice(0, 500) : '',
      lastHeartbeat: typeof connection.lastHeartbeat === 'string' && Number.isFinite(Date.parse(connection.lastHeartbeat))
        ? connection.lastHeartbeat
        : null,
    },
  }
}

function sessionCookie(token, config, expiresAt) {
  return `${SESSION_COOKIE}=${token}; Path=/; HttpOnly; SameSite=Strict; Expires=${new Date(expiresAt).toUTCString()}${config.secureCookie ? '; Secure' : ''}`
}

function expiredSessionCookie(config) {
  return `${SESSION_COOKIE}=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0${config.secureCookie ? '; Secure' : ''}`
}

function parseCookies(value) {
  const result = {}
  for (const item of value.split(';')) {
    const at = item.indexOf('=')
    if (at < 1) continue
    result[item.slice(0, at).trim()] = item.slice(at + 1).trim()
  }
  return result
}

function clientKey(value) {
  if (value === undefined || value === '') return randomUUID()
  if (typeof value !== 'string' || !SAFE_KEY.test(value)) throw new HTTPError(400, 'clientRequestId가 올바르지 않습니다.')
  return value
}

function stableKey(prefix, ...values) {
  const digest = createHash('sha256').update(values.join('\0')).digest('hex').slice(0, 32)
  return `${prefix}-${digest}`
}

function decodeSegment(value) {
  try {
    const decoded = decodeURIComponent(value)
    if (!SAFE_KEY.test(decoded)) throw new Error('unsafe segment')
    return decoded
  } catch {
    throw new HTTPError(400, '잘못된 식별자입니다.')
  }
}

class LoginAttempts {
  constructor({ now = () => Date.now(), max = 8, windowMs = 5 * 60 * 1000 } = {}) {
    this.now = now
    this.max = max
    this.windowMs = windowMs
    this.values = new Map()
  }

  allowed(key) {
    const value = this.values.get(key)
    if (!value || value.since + this.windowMs <= this.now()) {
      this.values.delete(key)
      return true
    }
    return value.count < this.max
  }

  failed(key) {
    const now = this.now()
    const value = this.values.get(key)
    if (!value || value.since + this.windowMs <= now) this.values.set(key, { count: 1, since: now })
    else value.count++
  }

  succeeded(key) {
    this.values.delete(key)
  }
}
