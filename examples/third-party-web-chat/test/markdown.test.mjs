import assert from 'node:assert/strict'
import test from 'node:test'

import { parseInline, parseMarkdown } from '../public/markdown.js'

test('parses headings, lists, fenced code, quotes and tables', () => {
  const blocks = parseMarkdown(`# 제목

- 첫째
- [x] 완료

> 인용문

| 이름 | 상태 |
| :--- | ---: |
| Pie | 정상 |

\`\`\`js
const ok = true
\`\`\``)
  assert.deepEqual(blocks.map((block) => block.type), ['heading', 'list', 'quote', 'table', 'code'])
  assert.equal(blocks[1].items[1].checked, true)
  assert.deepEqual(blocks[3].align, ['left', 'right'])
  assert.equal(blocks[4].language, 'js')
  assert.equal(blocks[4].value, 'const ok = true')
})

test('keeps an unfinished streamed fence as a stable code block', () => {
  const blocks = parseMarkdown('```go\nfmt.Println("stream")')
  assert.equal(blocks.length, 1)
  assert.deepEqual(blocks[0], { type: 'code', language: 'go', value: 'fmt.Println("stream")' })
})

test('supports common inline markdown while rejecting executable link schemes', () => {
  const nodes = parseInline('**굵게**와 `code`, [안전](https://cookai.dev), [위험](javascript:alert(1))')
  assert.equal(nodes.some((node) => node.type === 'strong'), true)
  assert.equal(nodes.some((node) => node.type === 'code'), true)
  assert.equal(nodes.some((node) => node.type === 'link' && node.href.startsWith('https://cookai.dev')), true)
  assert.equal(nodes.some((node) => node.type === 'link' && node.href.startsWith('javascript:')), false)
})

test('raw HTML remains inert text in the parsed output', () => {
  const [paragraph] = parseMarkdown('<img src=x onerror=alert(1)> 일반 텍스트')
  assert.equal(paragraph.type, 'paragraph')
  assert.equal(paragraph.children.map((node) => node.value || '').join(''), '<img src=x onerror=alert(1)> 일반 텍스트')
})
