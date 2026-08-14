#!/usr/bin/env node

// Expose the native Claude Code binary bundled by @anthropic-ai/claude-agent-sdk
// as an ordinary `claude` command inside a Pie Executor container.  The SDK
// already pins and installs the platform package; this wrapper avoids shipping
// a second, potentially mismatched CLI while preserving normal interactive
// terminal behaviour and the user's persisted ~/.claude state.

import { spawnSync } from 'node:child_process'
import { realpathSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const require = createRequire(import.meta.url)

export function packageNameFor(platform = process.platform, arch = process.arch, report = process.report) {
  const supportedPlatform = platform === 'linux' || platform === 'darwin' || platform === 'win32'
  const supportedArch = arch === 'x64' || arch === 'arm64'
  if (!supportedPlatform || !supportedArch) {
    throw new Error(`Claude Code is not bundled for ${platform}-${arch}`)
  }
  let libc = ''
  if (platform === 'linux') {
    const header = report?.getReport?.()?.header
    if (!header?.glibcVersionRuntime) libc = '-musl'
  }
  return `@anthropic-ai/claude-agent-sdk-${platform}-${arch}${libc}`
}

export function resolveBundledClaude(options = {}) {
  const packageName = packageNameFor(options.platform, options.arch, options.report)
  const packageJSON = require.resolve(`${packageName}/package.json`)
  return join(dirname(packageJSON), process.platform === 'win32' ? 'claude.exe' : 'claude')
}

export function runClaude(args = process.argv.slice(2)) {
  let executable
  try {
    executable = resolveBundledClaude()
  } catch (error) {
    console.error(`Pie Relay: bundled Claude Code executable is unavailable: ${error.message}`)
    return 127
  }
  const result = spawnSync(executable, args, {
    stdio: 'inherit',
    env: process.env,
  })
  if (result.error) {
    console.error(`Pie Relay: could not start Claude Code: ${result.error.message}`)
    return 126
  }
  if (result.signal) {
    console.error(`Pie Relay: Claude Code exited from signal ${result.signal}`)
    return 128
  }
  return result.status ?? 1
}

export function isMainModule(argvPath = process.argv[1]) {
  if (!argvPath) return false
  try {
    return realpathSync(argvPath) === realpathSync(fileURLToPath(import.meta.url))
  } catch {
    return pathToFileURL(argvPath).href === import.meta.url
  }
}

if (isMainModule()) {
  process.exitCode = runClaude()
}
