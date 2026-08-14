import assert from 'node:assert/strict'
import test from 'node:test'

import { filterAssistantMarkdown } from '../src/lib/assistant-output.mjs'

test('removes Kroot completion markers while preserving Markdown', () => {
  assert.equal(
    filterAssistantMarkdown('# 완료\n\n- **파일** 저장\n\n<kroot>DONE</kroot>'),
    '# 완료\n\n- **파일** 저장\n\n',
  )
  assert.equal(filterAssistantMarkdown('답변<KROOT> DONE </KROOT>'), '답변')
})

test('withholds a completion marker split across streaming chunks', () => {
  const markdown = '**완료**\n\n`result.txt`'
  const marker = '<kroot>DONE</kroot>'
  let streamed = markdown

  for (const character of marker) {
    streamed += character
    assert.equal(filterAssistantMarkdown(streamed), markdown)
  }
})

test('does not remove ordinary Kroot text', () => {
  assert.equal(filterAssistantMarkdown('`kroot init demo`를 실행했습니다.'), '`kroot init demo`를 실행했습니다.')
})
