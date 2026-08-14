// Small, dependency-free Markdown renderer for streamed Claude responses.
// It intentionally does not support raw HTML. Every value becomes a DOM text
// node, and links are restricted to safe schemes, so model output cannot turn
// into executable markup.

export function parseMarkdown(source) {
  const lines = String(source ?? '').replace(/\r\n?/g, '\n').split('\n')
  const blocks = []
  let index = 0
  while (index < lines.length) {
    const line = lines[index]
    if (!line.trim()) { index++; continue }

    const fence = line.match(/^\s*(```+|~~~+)\s*([^\s`]*)?.*$/)
    if (fence) {
      const marker = fence[1][0]
      const minimum = fence[1].length
      const body = []
      index++
      while (index < lines.length && !new RegExp(`^\\s*${escapeRegExp(marker)}{${minimum},}\\s*$`).test(lines[index])) {
        body.push(lines[index++])
      }
      if (index < lines.length) index++
      blocks.push({ type: 'code', language: fence[2] || '', value: body.join('\n') })
      continue
    }

    const heading = line.match(/^\s*(#{1,6})\s+(.+?)\s*#*\s*$/)
    if (heading) {
      blocks.push({ type: 'heading', level: heading[1].length, children: parseInline(heading[2]) })
      index++
      continue
    }
    if (/^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/.test(line)) {
      blocks.push({ type: 'rule' })
      index++
      continue
    }

    if (/^\s*>/.test(line)) {
      const quote = []
      while (index < lines.length && (/^\s*>/.test(lines[index]) || !lines[index].trim())) {
        quote.push(lines[index].replace(/^\s*>\s?/, ''))
        index++
      }
      blocks.push({ type: 'quote', children: parseMarkdown(quote.join('\n')) })
      continue
    }

    const listStart = listMatch(line)
    if (listStart) {
      const ordered = listStart.ordered
      const items = []
      while (index < lines.length) {
        const match = listMatch(lines[index])
        if (!match || match.ordered !== ordered) break
        let text = match.text
        index++
        while (index < lines.length && /^\s{2,}\S/.test(lines[index]) && !listMatch(lines[index])) {
          text += `\n${lines[index].trim()}`
          index++
        }
        const task = text.match(/^\[([ xX])\]\s+(.+)$/)
        items.push({ checked: task ? task[1].toLowerCase() === 'x' : null, children: parseInline(task ? task[2] : text) })
      }
      blocks.push({ type: 'list', ordered, start: listStart.start, items })
      continue
    }

    if (index + 1 < lines.length && looksLikeTableRow(line) && isTableDivider(lines[index + 1])) {
      const headers = splitTableRow(line)
      const divider = splitTableRow(lines[index + 1])
      const align = divider.map((cell) => cell.startsWith(':') && cell.endsWith(':') ? 'center' : cell.endsWith(':') ? 'right' : 'left')
      const rows = []
      index += 2
      while (index < lines.length && looksLikeTableRow(lines[index]) && lines[index].trim()) {
        rows.push(splitTableRow(lines[index]))
        index++
      }
      blocks.push({ type: 'table', headers: headers.map(parseInline), align, rows: rows.map((row) => row.map(parseInline)) })
      continue
    }

    const paragraph = [line]
    index++
    while (index < lines.length && lines[index].trim() && !startsBlock(lines, index)) {
      paragraph.push(lines[index++])
    }
    blocks.push({ type: 'paragraph', children: parseInline(paragraph.join('\n')) })
  }
  return blocks
}

export function parseInline(source) {
  const value = String(source ?? '')
  const nodes = []
  let index = 0
  let text = ''
  const flush = () => {
    if (text) { nodes.push({ type: 'text', value: text }); text = '' }
  }
  while (index < value.length) {
    if (value[index] === '\\' && index + 1 < value.length) {
      text += value[index + 1]
      index += 2
      continue
    }
    if (value[index] === '\n') {
      flush(); nodes.push({ type: 'break' }); index++; continue
    }
    if (value[index] === '`') {
      const size = countRun(value, index, '`')
      const close = value.indexOf('`'.repeat(size), index + size)
      if (close >= 0) {
        flush()
        nodes.push({ type: 'code', value: value.slice(index + size, close).replace(/^ | $/g, '') })
        index = close + size
        continue
      }
    }
    const imageOrLink = value.slice(index).match(/^(!?)\[([^\]]+)\]\(([^\s)]+)(?:\s+["'][^"']*["'])?\)/)
    if (imageOrLink) {
      flush()
      const href = safeHref(imageOrLink[3])
      if (href && !imageOrLink[1]) nodes.push({ type: 'link', href, children: parseInline(imageOrLink[2]) })
      else nodes.push({ type: 'text', value: imageOrLink[2] })
      index += imageOrLink[0].length
      continue
    }
    const delimiter = value.startsWith('**', index) || value.startsWith('__', index)
      ? value.slice(index, index + 2)
      : value.startsWith('~~', index) ? '~~' : null
    if (delimiter) {
      const close = value.indexOf(delimiter, index + 2)
      if (close > index + 2) {
        flush()
        nodes.push({ type: delimiter === '~~' ? 'strike' : 'strong', children: parseInline(value.slice(index + 2, close)) })
        index = close + 2
        continue
      }
    }
    if (value[index] === '*' || value[index] === '_') {
      const marker = value[index]
      const close = value.indexOf(marker, index + 1)
      if (close > index + 1) {
        flush()
        nodes.push({ type: 'emphasis', children: parseInline(value.slice(index + 1, close)) })
        index = close + 1
        continue
      }
    }
    const url = value.slice(index).match(/^https?:\/\/[^\s<]+[^\s<.,:;!?)}\]]/)
    if (url) {
      flush()
      nodes.push({ type: 'link', href: url[0], children: [{ type: 'text', value: url[0] }] })
      index += url[0].length
      continue
    }
    text += value[index++]
  }
  flush()
  return nodes
}

export function renderMarkdown(target, source) {
  const fragment = document.createDocumentFragment()
  for (const block of parseMarkdown(source)) fragment.append(renderBlock(block))
  target.replaceChildren(fragment)
}

function renderBlock(block) {
  switch (block.type) {
  case 'heading': {
    const element = document.createElement(`h${block.level}`)
    appendInline(element, block.children)
    return element
  }
  case 'paragraph': {
    const element = document.createElement('p')
    appendInline(element, block.children)
    return element
  }
  case 'rule': return document.createElement('hr')
  case 'quote': {
    const element = document.createElement('blockquote')
    for (const child of block.children) element.append(renderBlock(child))
    return element
  }
  case 'list': {
    const element = document.createElement(block.ordered ? 'ol' : 'ul')
    if (block.ordered && block.start > 1) element.start = block.start
    for (const item of block.items) {
      const li = document.createElement('li')
      if (item.checked !== null) {
        li.classList.add('task-item')
        const checkbox = document.createElement('input')
        checkbox.type = 'checkbox'; checkbox.checked = item.checked; checkbox.disabled = true
        li.append(checkbox)
      }
      appendInline(li, item.children)
      element.append(li)
    }
    return element
  }
  case 'code': {
    const wrapper = document.createElement('div')
    wrapper.className = 'code-block'
    const toolbar = document.createElement('div')
    toolbar.className = 'code-toolbar'
    const language = document.createElement('span')
    language.textContent = block.language || 'code'
    const copy = document.createElement('button')
    copy.type = 'button'; copy.textContent = '복사'; copy.className = 'code-copy'
    copy.addEventListener('click', async () => {
      try { await navigator.clipboard.writeText(block.value); copy.textContent = '복사됨' }
      catch { copy.textContent = '복사 실패' }
      setTimeout(() => { copy.textContent = '복사' }, 1500)
    })
    toolbar.append(language, copy)
    const pre = document.createElement('pre')
    const code = document.createElement('code')
    if (block.language) code.className = `language-${block.language.replace(/[^a-zA-Z0-9_-]/g, '')}`
    code.textContent = block.value
    pre.append(code); wrapper.append(toolbar, pre)
    return wrapper
  }
  case 'table': {
    const wrapper = document.createElement('div')
    wrapper.className = 'table-scroll'
    const table = document.createElement('table')
    const head = document.createElement('thead')
    const headRow = document.createElement('tr')
    block.headers.forEach((cell, index) => {
      const th = document.createElement('th'); th.style.textAlign = block.align[index] || 'left'; appendInline(th, cell); headRow.append(th)
    })
    head.append(headRow); table.append(head)
    const body = document.createElement('tbody')
    for (const row of block.rows) {
      const tr = document.createElement('tr')
      for (let index = 0; index < block.headers.length; index++) {
        const td = document.createElement('td'); td.style.textAlign = block.align[index] || 'left'; appendInline(td, row[index] || []); tr.append(td)
      }
      body.append(tr)
    }
    table.append(body); wrapper.append(table)
    return wrapper
  }
  default: {
    const element = document.createElement('p'); element.textContent = ''; return element
  }
  }
}

function appendInline(parent, nodes) {
  for (const node of nodes) {
    if (node.type === 'text') parent.append(document.createTextNode(node.value))
    else if (node.type === 'break') parent.append(document.createElement('br'))
    else if (node.type === 'code') {
      const code = document.createElement('code'); code.textContent = node.value; parent.append(code)
    } else if (node.type === 'link') {
      const link = document.createElement('a'); link.href = node.href; link.target = '_blank'; link.rel = 'noopener noreferrer'; appendInline(link, node.children); parent.append(link)
    } else {
      const tag = node.type === 'strong' ? 'strong' : node.type === 'emphasis' ? 'em' : 's'
      const element = document.createElement(tag); appendInline(element, node.children); parent.append(element)
    }
  }
}

function startsBlock(lines, index) {
  const line = lines[index]
  return /^\s*(```+|~~~+)/.test(line) || /^\s*#{1,6}\s+/.test(line) || /^\s*>/.test(line) || Boolean(listMatch(line)) ||
    /^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/.test(line) ||
    (index + 1 < lines.length && looksLikeTableRow(line) && isTableDivider(lines[index + 1]))
}

function listMatch(line) {
  const unordered = line.match(/^\s{0,3}[-*+]\s+(.+)$/)
  if (unordered) return { ordered: false, start: 1, text: unordered[1] }
  const ordered = line.match(/^\s{0,3}(\d+)[.)]\s+(.+)$/)
  return ordered ? { ordered: true, start: Number(ordered[1]), text: ordered[2] } : null
}

function looksLikeTableRow(line) { return /\|/.test(line) }
function isTableDivider(line) {
  const cells = splitTableRow(line)
  return cells.length > 0 && cells.every((cell) => /^:?-{3,}:?$/.test(cell.replace(/\s/g, '')))
}
function splitTableRow(line) {
  let value = line.trim()
  if (value.startsWith('|')) value = value.slice(1)
  if (value.endsWith('|') && !value.endsWith('\\|')) value = value.slice(0, -1)
  const cells = []
  let cell = ''
  for (let index = 0; index < value.length; index++) {
    if (value[index] === '\\' && value[index + 1] === '|') { cell += '|'; index++; continue }
    if (value[index] === '|') { cells.push(cell.trim()); cell = ''; continue }
    cell += value[index]
  }
  cells.push(cell.trim())
  return cells
}

function safeHref(value) {
  try {
    const url = new URL(value, globalThis.location?.origin || 'http://localhost')
    return ['http:', 'https:', 'mailto:'].includes(url.protocol) ? url.href : ''
  } catch { return '' }
}
function countRun(value, index, marker) { let size = 0; while (value[index + size] === marker) size++; return size }
function escapeRegExp(value) { return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') }
