#!/usr/bin/env node
// Spawns executor.mjs, sends one chat request, asserts we get session_id + text + done.
// Skips (exit 0) if `claude` is not on PATH.
import { spawn, execSync } from 'node:child_process'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))

try {
  execSync('command -v claude', { stdio: 'ignore' })
} catch {
  console.log('SKIP: claude not on PATH')
  process.exit(0)
}

const child = spawn('node', [join(here, 'executor.mjs')], { stdio: ['pipe', 'pipe', 'inherit'] })
const rl = createInterface({ input: child.stdout })

let sawSession = false
let sawText = false
let sawDone = false
const timer = setTimeout(() => { console.error('TIMEOUT'); child.kill(); process.exit(1) }, 90_000)

rl.on('line', (line) => {
  let ev
  try { ev = JSON.parse(line) } catch { return }
  if (ev.type === 'ready') {
    child.stdin.write(JSON.stringify({ type: 'chat', prompt: 'Reply with exactly the word pong', cwd: '/tmp' }) + '\n')
  } else if (ev.type === 'session_id') sawSession = true
  else if (ev.type === 'text') sawText = true
  else if (ev.type === 'error') { console.error('ERROR event:', ev.message); clearTimeout(timer); child.kill(); process.exit(1) }
  else if (ev.type === 'done') {
    sawDone = true
    clearTimeout(timer)
    child.stdin.end()
    if (sawSession && sawText && sawDone) { console.log('OK: session_id + text + done'); process.exit(0) }
    console.error(`FAIL: session=${sawSession} text=${sawText} done=${sawDone}`)
    process.exit(1)
  }
})

child.on('exit', (code) => { if (!sawDone) { console.error('executor exited early, code', code); process.exit(1) } })
