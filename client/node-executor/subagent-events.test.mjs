import { test } from 'node:test'
import assert from 'node:assert/strict'

import { createSubagentEventTracker } from './executor.mjs'

test('task lifecycle과 parent_tool_use_id를 같은 서브에이전트로 연결한다', () => {
  const tracker = createSubagentEventTracker()
  assert.deepEqual(tracker.rememberTask({
    task_id: 'task-a', tool_use_id: 'tool-a', request_id: 'req-sdk-start',
    subagent_type: 'Explore', task_type: 'local_agent', description: '인증 흐름 조사',
  }, 'request-a'), {
    taskId: 'task-a', parentToolUseId: 'tool-a', requestId: 'request-a',
    subagentType: 'Explore', taskType: 'local_agent', taskDescription: '인증 흐름 조사',
  })

  assert.deepEqual(tracker.contextForMessage({
    parent_tool_use_id: 'tool-a', request_id: 'req-sdk-nested',
  }), {
    taskId: 'task-a', parentToolUseId: 'tool-a', requestId: 'request-a',
    subagentType: 'Explore', taskType: 'local_agent', taskDescription: '인증 흐름 조사',
  })
})

test('SDK 내부 request_id가 백그라운드 작업의 Pie 채팅 요청 ID를 덮어쓰지 않는다', () => {
  const tracker = createSubagentEventTracker()
  tracker.rememberTask({
    task_id: 'task-background', tool_use_id: 'tool-background', request_id: 'req-sdk-start',
  }, 'pie-chat-request')

  assert.equal(tracker.contextForMessage({
    parent_tool_use_id: 'tool-background', request_id: 'req-sdk-after-main-done',
  }).requestId, 'pie-chat-request')
  assert.equal(tracker.contextForTask({
    task_id: 'task-background', request_id: 'req-sdk-progress',
  }).requestId, 'pie-chat-request')
})

test('태스크 시작 이벤트보다 먼저 온 nested message도 안정적인 임시 식별자를 가진다', () => {
  const tracker = createSubagentEventTracker()
  assert.deepEqual(tracker.contextForMessage({
    parent_tool_use_id: 'tool-early', subagent_type: 'Plan', task_description: '구현 계획',
  }), {
    taskId: 'tool-early', parentToolUseId: 'tool-early', requestId: undefined,
    subagentType: 'Plan', taskType: undefined, taskDescription: '구현 계획',
  })
})

test('병렬 서브에이전트의 text/thinking 스트림 상태는 서로 간섭하지 않는다', () => {
  const tracker = createSubagentEventTracker()
  const first = { parent_tool_use_id: 'tool-a' }
  const second = { parent_tool_use_id: 'tool-b' }
  const main = { parent_tool_use_id: null }

  tracker.markStreamed(first, 'text')
  tracker.markStreamed(second, 'thinking')
  tracker.markStreamed(main, 'text')

  assert.equal(tracker.wasStreamed(first, 'text'), true)
  assert.equal(tracker.wasStreamed(first, 'thinking'), false)
  assert.equal(tracker.wasStreamed(second, 'text'), false)
  assert.equal(tracker.wasStreamed(second, 'thinking'), true)
  assert.equal(tracker.wasStreamed(main, 'text'), true)

  tracker.resetStream(first)
  assert.equal(tracker.wasStreamed(first, 'text'), false)
  assert.equal(tracker.wasStreamed(second, 'thinking'), true)
})
