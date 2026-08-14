import { test } from 'node:test'
import assert from 'node:assert/strict'

import { stableTaskIdentity } from '../src/lib/task-identity.mjs'

test('메인 응답 종료 뒤 requestId가 사라져도 같은 서브에이전트 카드 ID를 유지한다', () => {
  const started = stableTaskIdentity({
    taskId: 'task-a', parentToolUseId: 'tool-a', requestId: 'pie-request-a',
  }, 10, 'fallback-a')
  const background = stableTaskIdentity({
    taskId: 'task-a', parentToolUseId: 'tool-a', requestId: 'req-sdk-internal',
  }, 20, 'fallback-b')

  assert.equal(started.id, 'task-tool-a')
  assert.deepEqual(background, started)
})

test('task_started보다 먼저 온 이벤트도 parentToolUseId로 이후 카드와 병합된다', () => {
  const early = stableTaskIdentity({ parentToolUseId: 'tool-early' }, 1, 'fallback-a')
  const started = stableTaskIdentity({ taskId: 'task-late', parentToolUseId: 'tool-early' }, 2, 'fallback-b')

  assert.equal(early.id, started.id)
})
