'use client'

import Editor, { loader, type OnMount } from '@monaco-editor/react'
import * as monaco from 'monaco-editor'
import { ChevronDown, ChevronRight, File, Folder, FolderOpen, RefreshCw, Save, X } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import { Button } from '@/components/ui/button'
import { WebChatAPIError, webChatAPI } from '@/lib/client-api'
import type { WorkspaceEntry, WorkspaceFile, WorkspaceTree } from '@/lib/web-chat-types'

loader.config({ monaco })

if (typeof window !== 'undefined') {
  self.MonacoEnvironment = {
    getWorker(_moduleId: string, label: string) {
      if (label === 'json') return new Worker(new URL('monaco-editor/esm/vs/language/json/json.worker.js', import.meta.url), { type: 'module' })
      if (label === 'css' || label === 'scss' || label === 'less') return new Worker(new URL('monaco-editor/esm/vs/language/css/css.worker.js', import.meta.url), { type: 'module' })
      if (label === 'html' || label === 'handlebars' || label === 'razor') return new Worker(new URL('monaco-editor/esm/vs/language/html/html.worker.js', import.meta.url), { type: 'module' })
      if (label === 'typescript' || label === 'javascript') return new Worker(new URL('monaco-editor/esm/vs/language/typescript/ts.worker.js', import.meta.url), { type: 'module' })
      return new Worker(new URL('monaco-editor/esm/vs/editor/editor.worker.js', import.meta.url), { type: 'module' })
    },
  }
}

type OpenFile = WorkspaceFile & {
  content: string
  savedContent: string
  saving: boolean
  error: string
}

type WorkspaceEditorProps = {
  projectId: string
  conversationId: string
  csrfToken: string
  refreshVersion: number
}

const parentPath = (value: string) => value.includes('/') ? value.slice(0, value.lastIndexOf('/')) : ''

function workspaceURL(projectId: string, resource: 'tree' | 'file', conversationId: string, path: string) {
  const query = new URLSearchParams({ conversationId, path })
  return `/api/projects/${encodeURIComponent(projectId)}/workspace/${resource}?${query}`
}

export function WorkspaceEditor({ projectId, conversationId, csrfToken, refreshVersion }: WorkspaceEditorProps) {
  const [trees, setTrees] = useState<Record<string, WorkspaceEntry[]>>({})
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [tabs, setTabs] = useState<OpenFile[]>([])
  const [activePath, setActivePath] = useState('')
  const [loadingPaths, setLoadingPaths] = useState<Set<string>>(new Set())
  const [treeError, setTreeError] = useState('')
  const [notice, setNotice] = useState('')
  const saveRef = useRef<() => void>(() => {})
  const initializedRefresh = useRef(false)
  const scopeRef = useRef('')
  const loadingRequests = useRef(new Map<string, string>())
  const scope = `${projectId}\u0000${conversationId}`
  scopeRef.current = scope

  const activeFile = tabs.find((tab) => tab.path === activePath) || null

  const loadTree = useCallback(async (path = '') => {
    if (!projectId || !conversationId) return
    const requestScope = `${projectId}\u0000${conversationId}`
    const loadingID = crypto.randomUUID()
    loadingRequests.current.set(path, loadingID)
    setLoadingPaths((current) => new Set(current).add(path))
    setTreeError('')
    try {
      const value = await webChatAPI<WorkspaceTree>(workspaceURL(projectId, 'tree', conversationId, path))
      if (scopeRef.current !== requestScope) return
      setTrees((current) => ({ ...current, [path]: value.entries }))
    } catch (error) {
      if (scopeRef.current !== requestScope) return
      setTreeError(error instanceof Error ? error.message : '파일 목록을 가져오지 못했습니다.')
    } finally {
      if (loadingRequests.current.get(path) !== loadingID) return
      loadingRequests.current.delete(path)
      setLoadingPaths((current) => {
        const next = new Set(current)
        next.delete(path)
        return next
      })
    }
  }, [conversationId, projectId])

  useEffect(() => {
    loadingRequests.current.clear()
    setTrees({})
    setExpanded(new Set())
    setTabs([])
    setActivePath('')
    setLoadingPaths(new Set())
    setTreeError('')
    setNotice('')
    initializedRefresh.current = false
    if (projectId && conversationId) void loadTree('')
  }, [conversationId, loadTree, projectId])

  useEffect(() => {
    if (!initializedRefresh.current) {
      initializedRefresh.current = true
      return
    }
    if (!projectId || !conversationId) return
    const paths = ['', ...expanded]
    void Promise.all(paths.map((path) => loadTree(path)))
    if (tabs.length) setNotice('Claude Code 작업이 끝났습니다. 열어 둔 파일은 저장 전에 다시 불러와 변경 내용을 확인해 주세요.')
  // refreshVersion is intentionally the trigger; the current tree state is read at that moment.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshVersion])

  const toggleDirectory = useCallback((entry: WorkspaceEntry) => {
    const willExpand = !expanded.has(entry.path)
    setExpanded((current) => {
      const next = new Set(current)
      if (next.has(entry.path)) next.delete(entry.path)
      else next.add(entry.path)
      return next
    })
    if (willExpand && !trees[entry.path]) void loadTree(entry.path)
  }, [expanded, loadTree, trees])

  const openFile = useCallback(async (entry: WorkspaceEntry) => {
    const existing = tabs.find((tab) => tab.path === entry.path)
    if (existing) {
      setActivePath(existing.path)
      return
    }
    setLoadingPaths((current) => new Set(current).add(entry.path))
    setTreeError('')
    const requestScope = `${projectId}\u0000${conversationId}`
    const loadingID = crypto.randomUUID()
    loadingRequests.current.set(entry.path, loadingID)
    try {
      const value = await webChatAPI<WorkspaceFile>(workspaceURL(projectId, 'file', conversationId, entry.path))
      if (scopeRef.current !== requestScope) return
      const opened: OpenFile = { ...value, content: value.content || '', savedContent: value.content || '', saving: false, error: '' }
      setTabs((current) => current.some((tab) => tab.path === opened.path) ? current : [...current, opened])
      setActivePath(opened.path)
    } catch (error) {
      if (scopeRef.current !== requestScope) return
      setTreeError(error instanceof Error ? error.message : '파일을 열지 못했습니다.')
    } finally {
      if (loadingRequests.current.get(entry.path) !== loadingID) return
      loadingRequests.current.delete(entry.path)
      setLoadingPaths((current) => {
        const next = new Set(current)
        next.delete(entry.path)
        return next
      })
    }
  }, [conversationId, projectId, tabs])

  const reloadFile = useCallback(async (file: OpenFile) => {
    if (file.content !== file.savedContent && !window.confirm('저장하지 않은 변경을 버리고 서버의 최신 파일을 불러올까요?')) return
    try {
      const requestScope = `${projectId}\u0000${conversationId}`
      const value = await webChatAPI<WorkspaceFile>(workspaceURL(projectId, 'file', conversationId, file.path))
      if (scopeRef.current !== requestScope) return
      setTabs((current) => current.map((tab) => tab.path === file.path
        ? { ...tab, ...value, content: value.content || '', savedContent: value.content || '', error: '', saving: false }
        : tab))
      setNotice('최신 파일을 불러왔습니다.')
    } catch (error) {
      if (scopeRef.current !== `${projectId}\u0000${conversationId}`) return
      setTabs((current) => current.map((tab) => tab.path === file.path
        ? { ...tab, error: error instanceof Error ? error.message : '파일을 다시 불러오지 못했습니다.' }
        : tab))
    }
  }, [conversationId, projectId])

  const saveFile = useCallback(async () => {
    const file = tabs.find((tab) => tab.path === activePath)
    if (!file || file.saving || file.content === file.savedContent) return
    setTabs((current) => current.map((tab) => tab.path === file.path ? { ...tab, saving: true, error: '' } : tab))
    const requestScope = `${projectId}\u0000${conversationId}`
    try {
      const saved = await webChatAPI<WorkspaceFile>(`/api/projects/${encodeURIComponent(projectId)}/workspace/file`, {
        method: 'PUT',
        csrfToken,
        body: {
          conversationId,
          path: file.path,
          content: file.content,
          baseRevision: file.revision,
          clientRequestId: crypto.randomUUID(),
        },
      })
      if (scopeRef.current !== requestScope) return
      setTabs((current) => current.map((tab) => tab.path === file.path
        ? { ...tab, ...saved, content: file.content, savedContent: file.content, saving: false, error: '' }
        : tab))
      setNotice(`${file.path} 파일을 안전하게 저장했습니다.`)
      void loadTree(parentPath(file.path))
    } catch (error) {
      if (scopeRef.current !== requestScope) return
      const conflict = error instanceof WebChatAPIError && error.status === 409
      setTabs((current) => current.map((tab) => tab.path === file.path
        ? { ...tab, saving: false, error: conflict ? '다른 작업이 이 파일을 먼저 변경했습니다. 최신 파일을 다시 불러온 뒤 변경 내용을 합쳐 주세요.' : error instanceof Error ? error.message : '파일을 저장하지 못했습니다.' }
        : tab))
    }
  }, [activePath, conversationId, csrfToken, loadTree, projectId, tabs])

  saveRef.current = () => { void saveFile() }

  useEffect(() => {
    if (!tabs.some((tab) => tab.content !== tab.savedContent)) return
    const warnBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', warnBeforeUnload)
    return () => window.removeEventListener('beforeunload', warnBeforeUnload)
  }, [tabs])

  const handleEditorMount: OnMount = useCallback((editor) => {
    editor.addAction({
      id: 'pie-save-workspace-file',
      label: 'Pie Workspace 파일 저장',
      keybindings: [monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS],
      run: () => saveRef.current(),
    })
    editor.focus()
  }, [])

  const closeTab = useCallback((path: string) => {
    const file = tabs.find((tab) => tab.path === path)
    if (file && file.content !== file.savedContent && !window.confirm('저장하지 않은 변경을 버리고 파일을 닫을까요?')) return
    const index = tabs.findIndex((tab) => tab.path === path)
    const remaining = tabs.filter((tab) => tab.path !== path)
    setTabs(remaining)
    if (activePath === path) setActivePath(remaining[Math.max(0, index - 1)]?.path || '')
  }, [activePath, tabs])

  const treeRows = useMemo(() => {
    const rows: Array<{ entry: WorkspaceEntry; depth: number }> = []
    const append = (directory: string, depth: number) => {
      for (const entry of trees[directory] || []) {
        rows.push({ entry, depth })
        if (entry.type === 'directory' && expanded.has(entry.path)) append(entry.path, depth + 1)
      }
    }
    append('', 0)
    return rows
  }, [expanded, trees])

  return (
    <section className="workspace-code panel" aria-label="프로젝트 코드 편집기">
      <header className="workspace-code-header">
        <div><p className="eyebrow">PROJECT FILES</p><h2>코드 편집기</h2></div>
        <div className="workspace-code-actions">
          <Button variant="ghost" disabled={loadingPaths.has('')} onClick={() => void loadTree('')}><RefreshCw aria-hidden="true" />새로고침</Button>
          <Button disabled={!activeFile || activeFile.content === activeFile.savedContent || activeFile.saving} onClick={() => void saveFile()}><Save aria-hidden="true" />{activeFile?.saving ? '저장 중…' : '저장'}</Button>
        </div>
      </header>
      <div className="workspace-code-body">
        <aside className="file-explorer" aria-label="파일 탐색기">
          <div className="file-explorer-title"><strong>탐색기</strong><small>프로젝트 내부</small></div>
          <div className="file-tree" role="tree">
            {loadingPaths.has('') && !trees[''] && <p>파일 목록을 불러오는 중…</p>}
            {treeRows.map(({ entry, depth }) => {
              const directory = entry.type === 'directory'
              const open = directory && expanded.has(entry.path)
              return <button
                key={entry.path}
                type="button"
                role="treeitem"
                aria-expanded={directory ? open : undefined}
                aria-selected={!directory && activePath === entry.path}
                className={activePath === entry.path ? 'active' : ''}
                style={{ paddingLeft: `${10 + depth * 15}px` }}
                title={entry.path}
                onClick={() => directory ? toggleDirectory(entry) : void openFile(entry)}
              >
                {directory ? open ? <ChevronDown /> : <ChevronRight /> : <span className="file-tree-indent" />}
                {directory ? open ? <FolderOpen className="folder-icon" /> : <Folder className="folder-icon" /> : <File className="file-icon" />}
                <span>{entry.name}</span>
                {loadingPaths.has(entry.path) && <i />}
              </button>
            })}
            {!loadingPaths.has('') && trees['']?.length === 0 && <p>프로젝트에 표시할 파일이 없습니다.</p>}
          </div>
        </aside>
        <div className="editor-area">
          <div className="editor-tabs" role="tablist" aria-label="열린 파일">
            {tabs.map((tab) => <div key={tab.path} className={`editor-tab${tab.path === activePath ? ' active' : ''}`}>
              <button type="button" role="tab" aria-selected={tab.path === activePath} className="editor-tab-select" onClick={() => setActivePath(tab.path)}>
                <File aria-hidden="true" /><span>{tab.path.split('/').at(-1)}</span>{tab.content !== tab.savedContent && <i title="저장되지 않음" />}
              </button>
              <button type="button" className="editor-tab-close" aria-label={`${tab.path} 닫기`} onClick={() => closeTab(tab.path)}><X aria-hidden="true" /></button>
            </div>)}
          </div>
          {activeFile ? <>
            <div className="editor-breadcrumb"><span>{activeFile.path}</span><small>{activeFile.language} · {formatBytes(activeFile.size)}</small></div>
            <div className="monaco-host">
              <Editor
                path={`pie-workspace:///${projectId}/${activeFile.path}`}
                language={activeFile.language}
                value={activeFile.content}
                theme="vs-dark"
                onMount={handleEditorMount}
                onChange={(value) => setTabs((current) => current.map((tab) => tab.path === activeFile.path ? { ...tab, content: value ?? '' } : tab))}
                options={{ automaticLayout: true, minimap: { enabled: false }, fontSize: 13, lineHeight: 21, padding: { top: 12 }, scrollBeyondLastLine: false, wordWrap: 'on', tabSize: 2 }}
              />
            </div>
            {activeFile.error && <div className="editor-file-error" role="alert"><span>{activeFile.error}</span><button type="button" onClick={() => void reloadFile(activeFile)}>최신 파일 다시 불러오기</button></div>}
          </> : <div className="editor-empty"><span>⌘</span><h3>편집할 파일을 선택해 주세요</h3><p>왼쪽 탐색기에서 텍스트 파일을 열면 Monaco Editor에서 바로 수정할 수 있습니다.</p></div>}
        </div>
      </div>
      {(treeError || notice) && <footer className={treeError ? 'workspace-code-status error' : 'workspace-code-status'} role={treeError ? 'alert' : 'status'}>{treeError || notice}</footer>}
    </section>
  )
}

function formatBytes(value: number) {
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`
  return `${(value / 1024 / 1024).toFixed(1)} MiB`
}
