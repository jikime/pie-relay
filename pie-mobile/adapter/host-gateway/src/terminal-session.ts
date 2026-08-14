import os from 'node:os'
import { chmodSync, existsSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import type { IPty } from 'node-pty'
import pty from 'node-pty'

const DEFAULT_COLS = 120
const DEFAULT_ROWS = 34
const MAX_SCROLLBACK_BYTES = 256 * 1024

export type TerminalDataListener = (chunk: string) => void

export type TerminalSessionOptions = {
  cwd: string
  shell?: string
}

function defaultShell(): string {
  if (process.platform === 'win32') {
    return process.env.COMSPEC || 'powershell.exe'
  }
  return process.env.SHELL || '/bin/zsh'
}

function ensureSpawnHelperExecutable(): void {
  if (process.platform === 'win32') {
    return
  }
  const require = createRequire(import.meta.url)
  const packageDir = dirname(require.resolve('node-pty/package.json'))
  const candidates = [
    join(packageDir, 'prebuilds', `${process.platform}-${process.arch}`, 'spawn-helper'),
    join(packageDir, 'build', 'Release', 'spawn-helper')
  ]
  for (const candidate of candidates) {
    if (existsSync(candidate)) {
      chmodSync(candidate, 0o755)
    }
  }
}

export class TerminalSession {
  readonly handle = 'pie-relay-terminal-1'
  readonly title = 'Pie Relay'
  private readonly terminal: IPty
  private readonly listeners = new Set<TerminalDataListener>()
  private scrollback = ''
  private cols = DEFAULT_COLS
  private rows = DEFAULT_ROWS

  constructor(options: TerminalSessionOptions) {
    ensureSpawnHelperExecutable()
    this.terminal = pty.spawn(options.shell || defaultShell(), [], {
      name: 'xterm-256color',
      cols: this.cols,
      rows: this.rows,
      cwd: options.cwd,
      env: {
        ...process.env,
        HOME: process.env.HOME || os.homedir(),
        TERM: 'xterm-256color',
        COLORTERM: 'truecolor'
      } as Record<string, string>
    })
    this.terminal.onData((chunk) => {
      this.appendScrollback(chunk)
      for (const listener of this.listeners) {
        listener(chunk)
      }
    })
  }

  get snapshot(): string {
    return this.scrollback
  }

  get size(): { cols: number; rows: number } {
    return { cols: this.cols, rows: this.rows }
  }

  write(text: string): void {
    this.terminal.write(text)
  }

  resize(cols: number, rows: number): void {
    const nextCols = Math.max(20, Math.min(240, Math.floor(cols)))
    const nextRows = Math.max(8, Math.min(120, Math.floor(rows)))
    if (nextCols === this.cols && nextRows === this.rows) {
      return
    }
    this.terminal.resize(nextCols, nextRows)
    this.cols = nextCols
    this.rows = nextRows
  }

  onData(listener: TerminalDataListener): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  clear(): void {
    this.scrollback = ''
  }

  close(): void {
    this.listeners.clear()
    this.terminal.kill()
  }

  private appendScrollback(chunk: string): void {
    this.scrollback += chunk
    while (Buffer.byteLength(this.scrollback, 'utf8') > MAX_SCROLLBACK_BYTES) {
      this.scrollback = this.scrollback.slice(Math.max(1, Math.floor(this.scrollback.length / 8)))
    }
  }
}
