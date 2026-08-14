#!/usr/bin/env node

import { mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawn } from 'node:child_process'

const appURL = required('PIE_WEB_CHAT_SMOKE_URL').replace(/\/$/, '')
const origin = (process.env.PIE_WEB_CHAT_SMOKE_ORIGIN?.trim() || new URL(appURL).origin).replace(/\/$/, '')
const login = JSON.parse(readFileSync(required('PIE_WEB_CHAT_SMOKE_LOGIN_FILE'), 'utf8'))
if (typeof login?.username !== 'string' || typeof login?.password !== 'string') {
  throw new Error('PIE_WEB_CHAT_SMOKE_LOGIN_FILE must contain username and password')
}

const chrome = findChrome()
const port = await availablePort()
const profile = mkdtempSync(join(tmpdir(), 'pie-web-chat-browser-'))
const processHandle = spawn(chrome, [
  '--headless=new', `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`,
  '--no-first-run', '--disable-default-apps', '--disable-extensions', '--disable-background-networking',
  'about:blank',
], { stdio: ['ignore', 'ignore', 'ignore'] })
let connection
const createdConversations = new Set()
const recoveryOnly = process.env.PIE_WEB_CHAT_SMOKE_RECOVERY_ONLY?.trim().toLowerCase() === 'true'
const subagentOnly = process.env.PIE_WEB_CHAT_SMOKE_SUBAGENT_ONLY?.trim().toLowerCase() === 'true'
const workspaceEditorOnly = process.env.PIE_WEB_CHAT_SMOKE_WORKSPACE_EDITOR_ONLY?.trim().toLowerCase() === 'true'
const smokePrompt = process.env.PIE_WEB_CHAT_SMOKE_PROMPT?.trim()
  || 'Bash 도구로 pwd를 실행해 현재 작업 경로를 확인해 주세요. 응답은 **결과**: `작업 경로` 형식의 Markdown으로 작성하고 마지막에 <kroot>DONE</kroot>를 붙여 주세요.'
const subagentPrompt = 'Agent 또는 Task 도구로 Explore 서브에이전트를 반드시 실제 실행해 주세요. 서브에이전트는 Bash로 `pwd`와 `printf pie-subagent-ok`를 각각 한 번 실행하고, 두 명령의 원본 출력만 Markdown 코드 블록으로 보고한 뒤 바로 종료해야 합니다. 메인 에이전트가 직접 명령을 실행하지 말고 서브에이전트 결과를 받아 마지막에 한 문장으로 요약해 주세요.'

try {
  await poll(async () => (await fetch(`http://127.0.0.1:${port}/json/version`)).ok, 20_000, 'Chrome debugger')
  const targetResponse = await fetch(`http://127.0.0.1:${port}/json/new?${encodeURIComponent(appURL)}`, { method: 'PUT' })
  if (!targetResponse.ok) throw new Error(`Chrome target creation failed: HTTP ${targetResponse.status}`)
  const target = await targetResponse.json()
  connection = await openCDPConnection(target.webSocketDebuggerUrl)
  await connection.send('Runtime.enable')
  await connection.send('Page.enable')
  await connection.send('Network.enable')
  await waitForExpression(connection, "document.readyState === 'complete' && !!document.getElementById('login-form')", 20_000, 'login form')
  await connection.evaluate(`(() => {
    const set = (id, value) => {
      const element = document.getElementById(id);
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      setter.call(element, value);
      element.dispatchEvent(new Event('input', { bubbles: true }));
    };
    set('username', ${JSON.stringify(login.username)});
    set('password', ${JSON.stringify(login.password)});
    document.getElementById('login-form').requestSubmit();
  })()`)
  await waitForExpression(connection, "!document.getElementById('app-view').classList.contains('hidden')", 30_000, 'authenticated app')
  await poll(async () => Boolean(await currentConversationID(connection)), 150_000, 'initial browser conversation')
  await waitForExpression(connection, "document.getElementById('relay-status').textContent.includes('Pie Relay 세션 연결됨') && document.getElementById('client-status').textContent.includes('Docker clientd 연결됨')", 150_000, 'initial conversation readiness')
  const first = await currentConversationID(connection)
  createdConversations.add(first)

  if (workspaceEditorOnly) {
    await connection.evaluate(`(() => {
      const button = [...document.querySelectorAll('.main-view-switch button')]
        .find((value) => value.textContent?.includes('코드'));
      if (!button || button.disabled) throw new Error('code view button is unavailable');
      button.click();
    })()`)
    await waitForExpression(connection, "!!document.querySelector('.workspace-code:not(.workspace-code-unavailable) .file-tree')", 30_000, 'workspace editor shell')
    await poll(async () => connection.evaluate(`(() => {
      if (window.__pieWorkspaceFileOpened) return true;
      const rows = [...document.querySelectorAll('.workspace-code .file-tree button')];
      const file = rows.find((row) => row.querySelector('.file-icon'));
      if (file) {
        window.__pieWorkspaceFileOpened = file.title || file.textContent || '';
        file.click();
        return true;
      }
      const directory = rows.find((row) => row.getAttribute('aria-expanded') === 'false');
      directory?.click();
      return false;
    })()`), 60_000, 'editable workspace file')
    await waitForExpression(connection, "!!document.querySelector('.workspace-code .monaco-editor .view-lines')", 60_000, 'Monaco editor')
    const editor = await connection.evaluate(`(() => ({
      file: window.__pieWorkspaceFileOpened || '',
      tabs: document.querySelectorAll('.workspace-code .editor-tab').length,
      lines: document.querySelectorAll('.workspace-code .monaco-editor .view-line').length,
      errors: [...document.querySelectorAll('.workspace-code .error')].map((value) => value.textContent?.trim()).filter(Boolean),
    }))()`)
    if (!editor.file || editor.tabs < 1 || editor.lines < 1 || editor.errors.length) {
      throw new Error(`Monaco workspace editor did not become usable: ${JSON.stringify(editor)}`)
    }
    console.log(JSON.stringify({ ok: true, workspaceEditorRendered: true, ...editor }))
  } else {
  await connection.evaluate("document.getElementById('new-chat-button').click()")
  await poll(async () => {
    const current = await currentConversationID(connection)
    if (current && current !== first) {
      createdConversations.add(current)
      return true
    }
    return false
  }, 150_000, 'replacement conversation')
  await waitForExpression(connection, "document.getElementById('prompt').disabled === false", 150_000, 'replacement conversation readiness')
  await waitForExpression(connection, "document.getElementById('stream-status').textContent.includes('실시간 수신 중')", 30_000, 'live stream readiness')
  const active = await connection.evaluate("fetch('/api/conversations').then(async (response) => { if (!response.ok) throw new Error('HTTP '+response.status); return response.json(); })")
  const second = await currentConversationID(connection)
  if (!Array.isArray(active) || active.length !== 1 || active[0].id !== second) {
    throw new Error(`browser conversation lifecycle leaked active sessions: ${JSON.stringify(active)}`)
  }

  const beforeCloseSnapshot = await connection.evaluate(`fetch('/api/conversations/${encodeURIComponent(second)}').then(async (response) => {
    if (!response.ok) throw new Error('HTTP ' + response.status);
    return response.json();
  })`)
  await connection.evaluate(`(async () => {
    const session = await fetch('/api/auth/me').then((response) => response.json());
    const response = await fetch('/api/conversations/${encodeURIComponent(second)}', {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': session.csrfToken },
      body: '{}',
    });
    if (!response.ok) throw new Error('conversation close failed: HTTP ' + response.status);
  })()`)
  await poll(async () => {
    const snapshot = await connection.evaluate(`fetch('/api/conversations/${encodeURIComponent(second)}').then(async (response) => {
      if (!response.ok) throw new Error('HTTP ' + response.status);
      return response.json();
    })`)
    return snapshot?.status !== 'closed' && snapshot?.updatedAt !== beforeCloseSnapshot.updatedAt
  }, 150_000, 'automatic conversation retry request')
  await waitForExpression(connection, "document.getElementById('relay-status').textContent.includes('Pie Relay 세션 연결됨') && document.getElementById('client-status').textContent.includes('Docker clientd 연결됨')", 150_000, 'closed conversation automatic reconnect')
  await waitForExpression(connection, "document.getElementById('stream-status').textContent.includes('실시간 수신 중')", 30_000, 'closed conversation automatic stream recovery')

  if (subagentOnly) {
    await connection.evaluate(`(() => {
      window.__pieSubagentObserved = { running: false, updates: 0 };
      const target = document.getElementById('messages');
      new MutationObserver(() => {
        const card = document.querySelector('.task-message');
        if (!card) return;
        window.__pieSubagentObserved.updates += 1;
        if (card.classList.contains('running')) window.__pieSubagentObserved.running = true;
      }).observe(target, { childList: true, subtree: true, characterData: true, attributes: true });
      const prompt = document.getElementById('prompt');
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
      setter.call(prompt, ${JSON.stringify(subagentPrompt)});
      prompt.dispatchEvent(new Event('input', { bubbles: true }));
      prompt.dispatchEvent(new Event('change', { bubbles: true }));
    })()`)
    await waitForExpression(connection, `document.getElementById('prompt')?.value === ${JSON.stringify(subagentPrompt)} && document.getElementById('send-button')?.disabled === false`, 5_000, 'subagent prompt state')
    await connection.evaluate("document.getElementById('composer').requestSubmit()")
    await waitForExpression(connection, `(() => {
      const card = document.querySelector('.task-message');
      return !!card && (card.querySelector('.subagent-markdown')?.textContent?.trim() || card.querySelector('.subagent-tool'));
    })()`, 180_000, 'live subagent transcript')
    await waitForExpression(connection, `(() => {
      const cards = [...document.querySelectorAll('.task-message')];
      return document.getElementById('cancel-button').classList.contains('hidden')
        && cards.length > 0
        && cards.every((card) => !card.classList.contains('running'));
    })()`, 240_000, 'subagent completion')
    const subagentResult = await connection.evaluate(`(() => ({
      observedRunning: window.__pieSubagentObserved?.running === true,
      updates: window.__pieSubagentObserved?.updates || 0,
      cards: [...document.querySelectorAll('.task-message')].map((card) => ({
        taskId: card.dataset.taskId || '',
        state: [...card.classList].find((value) => ['running', 'complete', 'error', 'cancelled'].includes(value)) || '',
        title: card.querySelector('.runtime-title')?.textContent || '',
        text: (card.querySelector('.subagent-markdown')?.textContent || '').trim().slice(0, 400),
        tools: [...card.querySelectorAll('.subagent-tool summary span')].map((value) => value.textContent || ''),
        openTools: [...card.querySelectorAll('.subagent-tool')].filter((value) => value.open).length,
        toolErrors: [...card.querySelectorAll('.subagent-tool.error')].map((tool) => ({
          name: tool.querySelector('summary span')?.textContent || '',
          result: (tool.querySelector('.runtime-result')?.textContent || '').trim().slice(0, 1_000),
        })),
      })),
    }))()`)
    const verifiedCard = subagentResult.cards.find((card) => card.state === 'complete'
      && card.text.includes('pie-subagent-ok')
      && card.tools.some((tool) => tool.includes('Bash'))
      && card.openTools === card.tools.length
      && card.toolErrors.length === 0)
    if (!subagentResult.observedRunning || !verifiedCard) {
      throw new Error(`subagent card did not stream and complete: ${JSON.stringify(subagentResult)}`)
    }
    const taskIDs = subagentResult.cards.map((card) => card.taskId).filter(Boolean)
    if (new Set(taskIDs).size !== taskIDs.length) {
      throw new Error(`one subagent was split into duplicate cards: ${JSON.stringify(subagentResult)}`)
    }
    const sandboxFailure = subagentResult.cards.flatMap((card) => card.toolErrors)
      .find((tool) => /sandbox|bwrap|bubblewrap|socat/i.test(tool.result))
    if (sandboxFailure) {
      throw new Error(`subagent sandbox failed: ${JSON.stringify(sandboxFailure)}`)
    }
    console.log(JSON.stringify({ ok: true, subagentStreamingObserved: true, ...subagentResult }))
  } else if (recoveryOnly) {
    console.log(JSON.stringify({
      ok: true,
      activeConversations: active.length,
      replacedConversation: first !== second,
      closedConversationRecoveryObserved: true,
      automaticConversationRecoveryObserved: true,
      liveStreamReady: true,
    }))
  } else {
    await connection.evaluate(`(() => {
    const prompt = document.getElementById('prompt');
    const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
    setter.call(prompt, ${JSON.stringify(smokePrompt)});
    prompt.dispatchEvent(new Event('input', { bubbles: true }));
    prompt.dispatchEvent(new Event('change', { bubbles: true }));
  })()`)
  await waitForExpression(connection, `(() => {
    const project = localStorage.getItem('pie-demo-project');
    return document.getElementById('prompt')?.value === ${JSON.stringify(smokePrompt)}
      && !!project
      && localStorage.getItem('pie-demo-draft:' + project) === ${JSON.stringify(smokePrompt)};
  })()`, 5_000, 'persisted streaming prompt')
  await waitForExpression(connection, "document.getElementById('send-button').disabled === false", 5_000, 'streaming prompt state')
  await connection.send('Network.setBlockedURLs', { urls: ['*api/conversations/*/events*'] })
  await connection.send('Page.reload', { ignoreCache: true })
  await waitForExpression(connection, "document.readyState === 'complete' && !document.getElementById('app-view').classList.contains('hidden')", 30_000, 'reloaded authenticated app')
  await waitForExpression(connection, "document.getElementById('prompt').disabled === false", 150_000, 'reloaded conversation readiness')
  await waitForExpression(connection, `document.getElementById('prompt')?.value === ${JSON.stringify(smokePrompt)}`, 5_000, 'restored streaming prompt')
  await waitForExpression(connection, `(() => {
    const stream = document.getElementById('stream-status')?.textContent || '';
    return stream.includes('끊김')
      && document.getElementById('send-button')?.disabled === true
      && !!document.getElementById('stream-retry-button');
  })()`, 30_000, 'offline stream send guard')
  await connection.send('Network.setBlockedURLs', { urls: [] })
  await connection.evaluate("document.getElementById('stream-retry-button')?.click()")
  await waitForExpression(connection, "document.getElementById('stream-status').textContent.includes('실시간 수신 중')", 30_000, 'recovered live stream')
  await waitForExpression(connection, "document.getElementById('send-button').disabled === false", 5_000, 'recovered streaming prompt state')
  await connection.evaluate("document.getElementById('composer').requestSubmit()")
  await waitForExpression(connection, `(() => {
    if (document.getElementById('cancel-button').classList.contains('hidden')) return false;
    const tool = document.querySelector('.tool-message.running');
    return !!tool
      && tool.querySelector('.runtime-title')?.textContent === 'Bash'
      && tool.querySelector('.runtime-input .runtime-payload')?.textContent.includes('pwd');
  })()`, 90_000, 'live raw tool call')
  const toolName = await connection.evaluate("document.querySelector('.tool-message.running .runtime-title')?.textContent || ''")
  await waitForExpression(connection, `(() => {
    const cancel = document.getElementById('cancel-button');
    const completed = document.querySelector('.tool-message.complete .runtime-result .runtime-payload');
    const answer = [...document.querySelectorAll('.message.assistant .bubble')].at(-1);
    const result = completed?.textContent?.trim() || '';
    const markerVisible = [...document.querySelectorAll('.message.assistant .bubble')]
      .some((value) => value.textContent?.toLowerCase().includes('<kroot>done</kroot>'));
    return cancel.classList.contains('hidden')
      && !!result
      && answer?.querySelector('strong')?.textContent === '결과'
      && answer?.querySelector('code')?.textContent === result
      && markerVisible === false;
  })()`, 180_000, 'streamed Claude completion')
  const rawToolResult = await connection.evaluate("document.querySelector('.tool-message.complete .runtime-result .runtime-payload')?.textContent?.trim() || ''")
  const assistantText = await connection.evaluate("[...document.querySelectorAll('.message.assistant .bubble')].at(-1)?.textContent?.trim() || ''")
    console.log(JSON.stringify({
      ok: true,
      activeConversations: active.length,
      replacedConversation: first !== second,
      liveStreamReady: true,
      closedConversationRecoveryObserved: true,
      automaticConversationRecoveryObserved: true,
      streamSendGuardObserved: true,
      liveActivityObserved: true,
      rawToolInputObserved: true,
      rawToolResultObserved: Boolean(rawToolResult),
      toolName,
      markdownObserved: true,
      krootDoneFiltered: true,
      rawToolResult: String(rawToolResult).slice(0, 120),
      assistantText: String(assistantText).slice(0, 120),
    }))
  }
  }
} catch (error) {
  let diagnostics = null
  if (connection) {
    try {
      diagnostics = await connection.evaluate(`(() => ({
        conversationId: (() => {
          const project = localStorage.getItem('pie-demo-project');
          return project ? localStorage.getItem('pie-demo-conversation:' + project) : '';
        })(),
        relay: document.getElementById('relay-status')?.textContent || '',
        client: document.getElementById('client-status')?.textContent || '',
        stream: document.getElementById('stream-status')?.textContent || '',
        activeTurn: !document.getElementById('cancel-button')?.classList.contains('hidden'),
        prompt: document.getElementById('prompt')?.value || '',
        error: document.getElementById('chat-error')?.textContent || '',
        tools: [...document.querySelectorAll('.tool-message')].map((tool) => ({
          state: [...tool.classList].filter((value) => ['running', 'complete', 'error', 'cancelled'].includes(value)),
          name: tool.querySelector('.runtime-title')?.textContent || '',
          input: (tool.querySelector('.runtime-input .runtime-payload')?.textContent || '').slice(0, 240),
          result: (tool.querySelector('.runtime-result .runtime-payload')?.textContent || '').slice(0, 240),
        })),
        assistants: [...document.querySelectorAll('.message.assistant .bubble')].map((value) => (value.textContent || '').slice(0, 240)),
      }))()`)
    } catch { /* retain the original failure */ }
  }
  throw new Error(`${error instanceof Error ? error.message : String(error)}${diagnostics ? `; diagnostics=${JSON.stringify(diagnostics)}` : ''}`)
} finally {
  if (connection) {
    try {
      await connection.send('Network.setBlockedURLs', { urls: [] })
    } catch { /* browser may already be closed */ }
    try { await connection.send('Browser.close') } catch { /* process fallback below */ }
    connection.close()
  }
  if (processHandle.exitCode === null) {
    processHandle.kill('SIGTERM')
    await Promise.race([new Promise((resolve) => processHandle.once('exit', resolve)), delay(3000)])
    if (processHandle.exitCode === null) processHandle.kill('SIGKILL')
  }
  await closeCreatedConversations([...createdConversations])
  rmSync(profile, { recursive: true, force: true })
}

async function currentConversationID(cdp) {
  return cdp.evaluate(`(() => {
    const project = localStorage.getItem('pie-demo-project');
    return project ? localStorage.getItem('pie-demo-conversation:' + project) : '';
  })()`)
}

async function closeCreatedConversations(ids) {
  if (!ids.length) return
  const response = await fetch(`${origin}/api/auth/login`, {
    method: 'POST', headers: { Origin: origin, 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: login.username, password: login.password }),
  })
  if (!response.ok) return
  const session = await response.json()
  const cookie = response.headers.get('set-cookie')?.split(';', 1)[0] || ''
  for (const id of ids) {
    await fetch(`${origin}/api/conversations/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      headers: { Cookie: cookie, Origin: origin, 'X-CSRF-Token': session.csrfToken, 'Content-Type': 'application/json' },
    }).catch(() => {})
  }
}

function required(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function findChrome() {
  const candidates = process.platform === 'darwin'
    ? ['/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/Applications/Chromium.app/Contents/MacOS/Chromium']
    : ['/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser']
  const selected = candidates.find((value) => {
    try { return statSync(value).isFile() } catch { return false }
  })
  if (!selected) throw new Error('Chrome or Chromium is required for the browser lifecycle smoke test')
  return selected
}

function availablePort() {
  return new Promise((resolve, reject) => {
    const server = createServer()
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      server.close((error) => error ? reject(error) : resolve(address.port))
    })
  })
}

async function openCDPConnection(url) {
  const socket = new WebSocket(url)
  await new Promise((resolve, reject) => {
    socket.addEventListener('open', resolve, { once: true })
    socket.addEventListener('error', reject, { once: true })
  })
  let nextID = 1
  const pending = new Map()
  socket.addEventListener('message', (message) => {
    const value = JSON.parse(message.data)
    if (!value.id || !pending.has(value.id)) return
    const request = pending.get(value.id)
    pending.delete(value.id)
    if (value.error) request.reject(new Error(value.error.message))
    else request.resolve(value.result)
  })
  const send = (method, params = {}) => new Promise((resolve, reject) => {
    const id = nextID++
    pending.set(id, { resolve, reject })
    socket.send(JSON.stringify({ id, method, params }))
  })
  return {
    send,
    async evaluate(expression) {
      const result = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
      if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description || result.exceptionDetails.text || 'browser evaluation failed')
      return result.result?.value
    },
    close: () => socket.close(),
  }
}

async function waitForExpression(connectionValue, expression, timeout, label) {
  return poll(async () => Boolean(await connectionValue.evaluate(expression)), timeout, label)
}

async function poll(check, timeout, label) {
  const deadline = Date.now() + timeout
  let lastError
  while (Date.now() < deadline) {
    try { if (await check()) return } catch (error) { lastError = error }
    await delay(250)
  }
  throw new Error(`timed out waiting for ${label}${lastError ? `: ${lastError.message}` : ''}`)
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}
