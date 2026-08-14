import assert from 'node:assert/strict'
import { chmod, mkdtemp, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  AcpBridge,
  buildPromptBlocks,
  parseAgentArgs,
  resolveWorkingDir,
  translateSessionUpdate,
} from './acp-executor.mjs'

async function fakeAgent() {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'pie-acp-test-'))
  const file = path.join(dir, 'fake-agent.mjs')
  await writeFile(file, `#!/usr/bin/env node
import { createInterface } from 'node:readline'
const send = (v) => process.stdout.write(JSON.stringify(v) + '\\n')
const rl = createInterface({ input: process.stdin })
for await (const line of rl) {
  const msg = JSON.parse(line)
  if (msg.method === 'initialize') {
    send({jsonrpc:'2.0',id:msg.id,result:{protocolVersion:2,agentCapabilities:{}}})
  } else if (msg.method === 'session/new') {
    send({jsonrpc:'2.0',id:msg.id,result:{sessionId:'acp-session-1'}})
  } else if (msg.method === 'session/prompt') {
    send({jsonrpc:'2.0',method:'session/update',params:{sessionId:'acp-session-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'안녕하세요'}}}})
    send({jsonrpc:'2.0',method:'session/update',params:{sessionId:'acp-session-1',update:{sessionUpdate:'tool_call',toolCallId:'tool-1',title:'파일 읽기',kind:'read',rawInput:{path:'README.md'}}}})
    send({jsonrpc:'2.0',id:'permission-1',method:'session/request_permission',params:{sessionId:'acp-session-1',toolCall:{title:'파일 읽기',rawInput:{path:'README.md'}},options:[{optionId:'yes-1',kind:'allow_once'},{optionId:'no-1',kind:'reject_once'}]}})
  } else if (msg.id === 'permission-1') {
    send({jsonrpc:'2.0',method:'session/update',params:{sessionId:'acp-session-1',update:{sessionUpdate:'tool_call_update',toolCallId:'tool-1',status:'completed',content:[{type:'content',content:{type:'text',text:'ok'}}]}}})
    send({jsonrpc:'2.0',id:3,result:{stopReason:'end_turn'}})
  }
}
`)
  await chmod(file, 0o755)
  return file
}

test('ACP update를 기존 Relay 이벤트로 변환한다', () => {
  assert.deepEqual(translateSessionUpdate({
    params: { update: { sessionUpdate: 'agent_message_chunk', content: { text: 'hello' } } },
  }), { type: 'text', text: 'hello' })
  assert.deepEqual(translateSessionUpdate({
    params: { update: { sessionUpdate: 'agent_thought_chunk', content: { text: 'think' } } },
  }), { type: 'thinking', text: 'think' })
})

test('텍스트와 지원 이미지 첨부를 ACP content block으로 만든다', () => {
  assert.deepEqual(buildPromptBlocks({
    prompt: '이미지를 봐 주세요',
    images: [
      { mimeType: 'image/png', data: 'YWJj' },
      { mimeType: 'application/pdf', data: 'ignored' },
    ],
  }), [
    { type: 'image', mimeType: 'image/png', data: 'YWJj' },
    { type: 'text', text: '이미지를 봐 주세요' },
  ])
})

test('에이전트 인자는 JSON 문자열 배열만 허용한다', () => {
  assert.deepEqual(parseAgentArgs('["--flag","value"]'), ['--flag', 'value'])
  assert.throws(() => parseAgentArgs('--flag value'), /JSON string array/)
  assert.throws(() => parseAgentArgs('{"flag":true}'), /JSON string array/)
})

test('상대 경로를 거부하고 안전한 절대 작업 경로를 선택한다', () => {
  assert.equal(resolveWorkingDir({ cwd: '.' }), os.homedir())
  assert.equal(resolveWorkingDir({ cwd: os.tmpdir() }), os.tmpdir())
})

test('initialize, session/new, prompt, 권한 응답을 끝까지 중계한다', async (t) => {
  const command = await fakeAgent()
  const events = []
  const bridge = new AcpBridge({ command, emit: (event) => events.push(event) })
  t.after(() => bridge.close())
  await bridge.start()

  const prompt = bridge.prompt({ prompt: '테스트', cwd: os.tmpdir() })
  while (!events.some((event) => event.type === 'permission_request')) {
    await new Promise((resolve) => setTimeout(resolve, 5))
  }
  const permission = events.find((event) => event.type === 'permission_request')
  assert.equal(permission.toolName, '파일 읽기')
  assert.equal(await bridge.permissionResponse(permission.requestId, true), true)
  await prompt

  assert.deepEqual(events.map((event) => event.type), [
    'session_id', 'text', 'tool_call', 'permission_request', 'tool_result', 'done',
  ])
  assert.equal(events.at(-1).sessionId, 'acp-session-1')
})
