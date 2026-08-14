import {
  closeSync,
  constants as fsConstants,
  fstatSync,
  fsyncSync,
  lstatSync,
  openSync,
  readFileSync,
  readdirSync,
  realpathSync,
  renameSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from 'node:fs'
import { createHash, randomUUID } from 'node:crypto'
import path from 'node:path'

export const MAX_WORKSPACE_FILE_BYTES = 2 << 20
export const MAX_WORKSPACE_ENTRIES = 500

const ALWAYS_HIDDEN_SEGMENTS = new Set([
  '.git',
  '.claude',
  '.kroot',
  '.pie',
  '.ssh',
  '.aws',
  'node_modules',
  '.next',
  '.nuxt',
  '.turbo',
  '.cache',
  'coverage',
  'dist',
  'build',
  'secrets',
])

const LANGUAGE_BY_EXTENSION = new Map([
  ['.c', 'c'], ['.cc', 'cpp'], ['.cpp', 'cpp'], ['.cs', 'csharp'],
  ['.css', 'css'], ['.dockerfile', 'dockerfile'], ['.go', 'go'],
  ['.graphql', 'graphql'], ['.gql', 'graphql'], ['.html', 'html'],
  ['.java', 'java'], ['.js', 'javascript'], ['.jsx', 'javascript'],
  ['.json', 'json'], ['.kt', 'kotlin'], ['.less', 'less'], ['.lua', 'lua'],
  ['.md', 'markdown'], ['.php', 'php'], ['.prisma', 'graphql'],
  ['.py', 'python'], ['.rb', 'ruby'], ['.rs', 'rust'], ['.scss', 'scss'],
  ['.sh', 'shell'], ['.sql', 'sql'], ['.svelte', 'html'], ['.swift', 'swift'],
  ['.toml', 'ini'], ['.ts', 'typescript'], ['.tsx', 'typescript'],
  ['.vue', 'html'], ['.xml', 'xml'], ['.yaml', 'yaml'], ['.yml', 'yaml'],
])

export class WorkspaceError extends Error {
  constructor(code, message, details = undefined) {
    super(message)
    this.name = 'WorkspaceError'
    this.code = code
    this.details = details
  }
}

const workspaceRoot = () => realpathSync(process.env.PIE_WORKSPACE_ROOT || '/workspace/projects')

const isInside = (root, candidate) => candidate === root || candidate.startsWith(root + path.sep)

function protectedSegment(segment) {
  const lower = segment.toLowerCase()
  return ALWAYS_HIDDEN_SEGMENTS.has(lower) || lower === '.env' || lower.startsWith('.env.')
}

export function normalizeWorkspacePath(value, { allowEmpty = false } = {}) {
  if (value === undefined || value === null) value = ''
  if (typeof value !== 'string' || value.length > 4096 || /[\0\x00-\x1f\x7f]/.test(value)) {
    throw new WorkspaceError('invalid_path', '유효하지 않은 파일 경로입니다.')
  }
  if (value.includes('\\') || path.posix.isAbsolute(value)) {
    throw new WorkspaceError('invalid_path', '프로젝트 내부의 상대 경로만 사용할 수 있습니다.')
  }
  const parts = value.split('/').filter((segment) => segment !== '' && segment !== '.')
  if (parts.some((segment) => segment === '..' || Buffer.byteLength(segment) > 255)) {
    throw new WorkspaceError('invalid_path', '프로젝트 밖으로 이동하는 경로는 사용할 수 없습니다.')
  }
  if (!allowEmpty && parts.length === 0) {
    throw new WorkspaceError('invalid_path', '파일 경로가 필요합니다.')
  }
  if (parts.some(protectedSegment)) {
    throw new WorkspaceError('forbidden', '인증정보나 빌드 산출물 경로에는 접근할 수 없습니다.')
  }
  return parts.join('/')
}

function resolveProjectRoot(projectPath) {
  if (typeof projectPath !== 'string' || !projectPath) {
    throw new WorkspaceError('invalid_project', '프로젝트 작업 경로가 없습니다.')
  }
  let root
  let project
  try {
    root = workspaceRoot()
    project = realpathSync(projectPath)
  } catch {
    throw new WorkspaceError('not_found', '프로젝트 작업공간을 찾을 수 없습니다.')
  }
  // A managed project is always one direct child of /workspace/projects.
  // Requiring that exact shape prevents a caller from selecting /workspace or
  // another tenant's nested directory as an editor root.
  if (path.dirname(project) !== root || !isInside(root, project)) {
    throw new WorkspaceError('forbidden', '허용된 프로젝트 작업공간이 아닙니다.')
  }
  return project
}

function resolveExisting(projectRoot, relativePath, kind = 'any') {
  const candidate = path.join(projectRoot, relativePath)
  let resolved
  let info
  try {
    if (relativePath) {
      const linkInfo = lstatSync(candidate)
      if (linkInfo.isSymbolicLink()) throw new WorkspaceError('forbidden', '심볼릭 링크에는 접근할 수 없습니다.')
    }
    resolved = realpathSync(candidate)
    info = statSync(resolved)
  } catch (error) {
    if (error instanceof WorkspaceError) throw error
    throw new WorkspaceError('not_found', '파일 또는 폴더를 찾을 수 없습니다.')
  }
  if (!isInside(projectRoot, resolved)) {
    throw new WorkspaceError('forbidden', '프로젝트 밖의 경로에는 접근할 수 없습니다.')
  }
  // Validate the canonical target as well as the user-facing path. Otherwise
  // an innocently named in-project symlink could point at `.kroot` or another
  // protected directory while still remaining under the project root.
  const canonicalRelative = path.relative(projectRoot, resolved).split(path.sep).join('/')
  normalizeWorkspacePath(canonicalRelative, { allowEmpty: true })
  if (kind === 'file' && !info.isFile()) throw new WorkspaceError('not_file', '일반 파일이 아닙니다.')
  if (kind === 'directory' && !info.isDirectory()) throw new WorkspaceError('not_directory', '폴더가 아닙니다.')
  return { resolved, info }
}

function revisionFor(buffer) {
  return `sha256:${createHash('sha256').update(buffer).digest('hex')}`
}

function languageFor(relativePath) {
  const basename = path.posix.basename(relativePath).toLowerCase()
  if (basename === 'dockerfile') return 'dockerfile'
  if (basename === 'makefile') return 'shell'
  return LANGUAGE_BY_EXTENSION.get(path.posix.extname(basename)) || 'plaintext'
}

function readRegularFile(resolved) {
  let fd
  try {
    fd = openSync(resolved, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW || 0))
    const info = fstatSync(fd)
    if (!info.isFile()) throw new WorkspaceError('not_file', '일반 파일이 아닙니다.')
    if (info.size > MAX_WORKSPACE_FILE_BYTES) {
      throw new WorkspaceError('too_large', '2MiB보다 큰 파일은 편집기에서 열 수 없습니다.')
    }
    const buffer = readFileSync(fd)
    if (buffer.includes(0)) throw new WorkspaceError('binary', '바이너리 파일은 편집기에서 열 수 없습니다.')
    let content
    try {
      content = new TextDecoder('utf-8', { fatal: true }).decode(buffer)
    } catch {
      throw new WorkspaceError('binary', 'UTF-8 텍스트 파일만 편집할 수 있습니다.')
    }
    return { buffer, content, info }
  } finally {
    if (fd !== undefined) closeSync(fd)
  }
}

export function listWorkspace(projectPath, requestedPath = '') {
  const projectRoot = resolveProjectRoot(projectPath)
  const relativePath = normalizeWorkspacePath(requestedPath, { allowEmpty: true })
  const { resolved } = resolveExisting(projectRoot, relativePath, 'directory')
  const values = []
  for (const name of readdirSync(resolved)) {
    if (protectedSegment(name)) continue
    const childRelativePath = relativePath ? `${relativePath}/${name}` : name
    const childPath = path.join(resolved, name)
    let info
    try {
      info = lstatSync(childPath)
    } catch {
      continue
    }
    if (info.isSymbolicLink() || (!info.isDirectory() && !info.isFile())) continue
    values.push({
      name,
      path: childRelativePath,
      type: info.isDirectory() ? 'directory' : 'file',
      size: info.isFile() ? info.size : undefined,
      modifiedAt: info.mtime.toISOString(),
    })
    if (values.length > MAX_WORKSPACE_ENTRIES) {
      throw new WorkspaceError('too_many_entries', `한 폴더에는 최대 ${MAX_WORKSPACE_ENTRIES}개 항목만 표시할 수 있습니다.`)
    }
  }
  values.sort((left, right) => {
    if (left.type !== right.type) return left.type === 'directory' ? -1 : 1
    return left.name.localeCompare(right.name, undefined, { numeric: true, sensitivity: 'base' })
  })
  return { path: relativePath, entries: values }
}

export function readWorkspaceFile(projectPath, requestedPath) {
  const projectRoot = resolveProjectRoot(projectPath)
  const relativePath = normalizeWorkspacePath(requestedPath)
  const { resolved } = resolveExisting(projectRoot, relativePath, 'file')
  const { buffer, content, info } = readRegularFile(resolved)
  return {
    path: relativePath,
    content,
    revision: revisionFor(buffer),
    size: buffer.length,
    modifiedAt: info.mtime.toISOString(),
    language: languageFor(relativePath),
  }
}

export function writeWorkspaceFile(projectPath, requestedPath, content, baseRevision, create = false) {
  const projectRoot = resolveProjectRoot(projectPath)
  const relativePath = normalizeWorkspacePath(requestedPath)
  if (typeof content !== 'string' || Buffer.byteLength(content) > MAX_WORKSPACE_FILE_BYTES) {
    throw new WorkspaceError('too_large', '저장할 내용은 UTF-8 기준 2MiB 이하여야 합니다.')
  }
  if (typeof baseRevision !== 'string' || baseRevision.length > 80) {
    throw new WorkspaceError('invalid_revision', '파일 버전 정보가 올바르지 않습니다.')
  }

  const destination = path.join(projectRoot, relativePath)
  const parentRelative = path.posix.dirname(relativePath) === '.' ? '' : path.posix.dirname(relativePath)
  const { resolved: parent } = resolveExisting(projectRoot, parentRelative, 'directory')
  if (path.dirname(destination) !== parent) {
    throw new WorkspaceError('forbidden', '허용된 프로젝트 경로가 아닙니다.')
  }

  let previousMode = 0o600
  let exists = false
  try {
    const linkInfo = lstatSync(destination)
    if (linkInfo.isSymbolicLink()) throw new WorkspaceError('forbidden', '심볼릭 링크에는 저장할 수 없습니다.')
    const current = readRegularFile(destination)
    exists = true
    previousMode = current.info.mode & 0o777
    const currentRevision = revisionFor(current.buffer)
    if (!baseRevision || currentRevision !== baseRevision) {
      throw new WorkspaceError('conflict', '다른 작업에서 파일이 먼저 변경되었습니다.', { currentRevision })
    }
  } catch (error) {
    if (error instanceof WorkspaceError) throw error
    if (!create || baseRevision) throw new WorkspaceError('not_found', '저장할 파일을 찾을 수 없습니다.')
  }
  if (!exists && !create) throw new WorkspaceError('not_found', '저장할 파일을 찾을 수 없습니다.')

  const buffer = Buffer.from(content, 'utf8')
  const temporary = path.join(parent, `.${path.basename(destination)}.pie-${randomUUID()}.tmp`)
  let fd
  try {
    fd = openSync(temporary, fsConstants.O_CREAT | fsConstants.O_EXCL | fsConstants.O_WRONLY, previousMode)
    writeFileSync(fd, buffer)
    fsyncSync(fd)
    closeSync(fd)
    fd = undefined
    renameSync(temporary, destination)
    // Persist the directory entry as well as file contents. This matters when
    // a host loses power immediately after an otherwise successful save.
    const directoryFD = openSync(parent, fsConstants.O_RDONLY)
    try { fsyncSync(directoryFD) } finally { closeSync(directoryFD) }
  } catch (error) {
    try { if (fd !== undefined) closeSync(fd) } catch { /* ignore cleanup failure */ }
    try { unlinkSync(temporary) } catch { /* ignore cleanup failure */ }
    if (error instanceof WorkspaceError) throw error
    throw new WorkspaceError('write_failed', '파일을 안전하게 저장하지 못했습니다.')
  }
  const info = statSync(destination)
  return {
    path: relativePath,
    revision: revisionFor(buffer),
    size: buffer.length,
    modifiedAt: info.mtime.toISOString(),
    language: languageFor(relativePath),
    created: !exists,
  }
}

export function handleWorkspaceRequest(request) {
  const requestId = typeof request?.requestId === 'string' ? request.requestId : ''
  const operation = typeof request?.operation === 'string' ? request.operation : ''
  const response = { type: 'workspace_result', requestId, operation, ok: false }
  try {
    if (!requestId || requestId.length > 160) throw new WorkspaceError('invalid_request', '요청 식별자가 필요합니다.')
    switch (operation) {
      case 'list':
        response.data = listWorkspace(request.projectPath, request.path)
        break
      case 'read':
        response.data = readWorkspaceFile(request.projectPath, request.path)
        break
      case 'write':
        response.data = writeWorkspaceFile(request.projectPath, request.path, request.content, request.baseRevision, request.create === true)
        break
      default:
        throw new WorkspaceError('unsupported_operation', '지원하지 않는 파일 작업입니다.')
    }
    response.ok = true
  } catch (error) {
    const safe = error instanceof WorkspaceError
      ? error
      : new WorkspaceError('workspace_failed', '파일 작업을 처리하지 못했습니다.')
    response.error = { code: safe.code, message: safe.message, ...(safe.details || {}) }
  }
  return response
}
