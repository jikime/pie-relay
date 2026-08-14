import assert from 'node:assert/strict'
import test from 'node:test'

import { translateClaudeToolBlock } from './executor.mjs'

test('preserves Claude tool IDs, input, result content, and error state', () => {
  assert.deepEqual(translateClaudeToolBlock({
    type: 'tool_use',
    id: 'toolu_123',
    name: 'Write',
    input: { file_path: '/workspace/result.md', content: '# result' },
  }), {
    type: 'tool_call',
    toolCallId: 'toolu_123',
    name: 'Write',
    input: { file_path: '/workspace/result.md', content: '# result' },
  })

  assert.deepEqual(translateClaudeToolBlock({
    type: 'tool_result',
    tool_use_id: 'toolu_123',
    content: [{ type: 'text', text: 'saved' }],
    is_error: true,
  }), {
    type: 'tool_result',
    toolCallId: 'toolu_123',
    content: [{ type: 'text', text: 'saved' }],
    isError: true,
  })
})

test('ignores non-tool Claude content blocks', () => {
  assert.equal(translateClaudeToolBlock({ type: 'text', text: 'hello' }), null)
  assert.equal(translateClaudeToolBlock(null), null)
})
