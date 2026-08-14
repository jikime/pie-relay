#!/usr/bin/env node

import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const command = process.argv[2]
if (!['dev', 'start'].includes(command)) {
  console.error('Usage: node scripts/run-next.mjs dev|start')
  process.exit(2)
}

const host = process.env.PIE_WEB_CHAT_HOST?.trim() || '127.0.0.1'
const port = process.env.PIE_WEB_CHAT_PORT?.trim() || '4175'
const nextBin = fileURLToPath(new URL('../node_modules/next/dist/bin/next', import.meta.url))
const args = [nextBin, command, '--hostname', host, '--port', port]
if (command === 'dev') args.push('--webpack')

const child = spawn(process.execPath, args, { stdio: 'inherit', env: process.env })
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.once(signal, () => child.kill(signal))
}
child.once('error', (error) => {
  console.error(error)
  process.exitCode = 1
})
child.once('exit', (code, signal) => {
  process.exitCode = signal ? 1 : (code ?? 1)
})
