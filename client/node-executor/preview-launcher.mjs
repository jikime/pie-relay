#!/usr/bin/env node

import { createHash } from 'node:crypto'
import { constants as fsConstants } from 'node:fs'
import { access, mkdir, readFile, rm, stat, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { spawn } from 'node:child_process'
import { pathToFileURL } from 'node:url'

const INSTALL_LOCK = '.pie-preview-install.lock'
const INSTALL_LOCK_OWNER = 'owner.json'
const INSTALL_STAMP = '.pie-preview-deps.sha256'
const LOCK_STALE_MS = 10 * 60 * 1000
const LOCK_WAIT_MS = 500

export async function prepareDependencies(cwd, options = {}) {
  const run = options.run || runCommand
  const sleep = options.sleep || ((milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds)))
  const lockPath = path.join(cwd, INSTALL_LOCK)

  for (;;) {
    const fingerprint = await dependencyFingerprint(cwd)
    if (await dependenciesCurrent(cwd, fingerprint)) return false
    try {
      await mkdir(lockPath)
      await writeFile(path.join(lockPath, INSTALL_LOCK_OWNER), JSON.stringify({ pid: process.pid, createdAt: new Date().toISOString() }), { mode: 0o600 })
    } catch (error) {
      if (error?.code !== 'EEXIST') throw error
      if (!await installLockActive(lockPath)) {
        await rm(lockPath, { recursive: true, force: true })
        continue
      }
      await sleep(LOCK_WAIT_MS)
      continue
    }

    try {
      const lockedFingerprint = await dependencyFingerprint(cwd)
      if (await dependenciesCurrent(cwd, lockedFingerprint)) return false
      const hasLockfile = await exists(path.join(cwd, 'package-lock.json')) || await exists(path.join(cwd, 'npm-shrinkwrap.json'))
      const operation = hasLockfile ? 'ci' : 'install'
      process.stdout.write(`[pie-preview] 의존성을 준비합니다: npm ${operation}\n`)
      await run('npm', [operation, '--no-audit', '--no-fund'], { cwd, env: process.env })
      const installedFingerprint = await dependencyFingerprint(cwd)
      const nodeModulesPath = path.join(cwd, 'node_modules')
      // 의존성이 없는 package.json에서는 npm이 node_modules를 만들지 않을 수
      // 있다. 이 경우에도 설치 완료 지문을 남겨 매 실행마다 npm install을
      // 반복하지 않도록 디렉터리를 명시적으로 준비한다.
      await mkdir(nodeModulesPath, { recursive: true })
      const stampPath = path.join(nodeModulesPath, INSTALL_STAMP)
      await writeFile(stampPath, `${installedFingerprint}\n`, { mode: 0o600 })
      return true
    } finally {
      await rm(lockPath, { recursive: true, force: true })
    }
  }
}

async function installLockActive(lockPath) {
  const owner = await readFile(path.join(lockPath, INSTALL_LOCK_OWNER), 'utf8')
    .then((value) => JSON.parse(value), () => null)
    .catch(() => null)
  if (Number.isInteger(owner?.pid) && owner.pid > 0) {
    try {
      process.kill(owner.pid, 0)
      return true
    } catch (error) {
      if (error?.code === 'EPERM') return true
      if (error?.code === 'ESRCH') return false
    }
  }
  const lockInfo = await stat(lockPath).catch(() => null)
  return !!lockInfo && Date.now() - lockInfo.mtimeMs <= LOCK_STALE_MS
}

export async function dependencyFingerprint(cwd) {
  const hash = createHash('sha256')
  for (const name of ['package.json', 'package-lock.json', 'npm-shrinkwrap.json']) {
    const value = await readFile(path.join(cwd, name)).catch((error) => {
      if (error?.code === 'ENOENT') return null
      throw error
    })
    if (value) hash.update(name).update('\0').update(value).update('\0')
  }
  return hash.digest('hex')
}

export function developmentCommand(profile, port) {
  if (!Number.isInteger(port) || port < 1024 || port > 65535) throw new Error('유효하지 않은 프리뷰 포트입니다.')
  switch (profile) {
    case 'next':
      return ['npm', ['run', 'dev', '--', '--hostname', '0.0.0.0', '--port', String(port)]]
    case 'vite':
      return ['npm', ['run', 'dev', '--', '--host', '0.0.0.0', '--port', String(port)]]
    case 'npm':
      return ['npm', ['run', 'dev']]
    default:
      throw new Error(`지원하지 않는 프리뷰 프로필입니다: ${profile}`)
  }
}

async function dependenciesCurrent(cwd, fingerprint) {
  const nodeModules = path.join(cwd, 'node_modules')
  try {
    await access(nodeModules, fsConstants.R_OK | fsConstants.X_OK)
    const value = await readFile(path.join(nodeModules, INSTALL_STAMP), 'utf8')
    return value.trim() === fingerprint
  } catch {
    return false
  }
}

function exists(value) {
  return access(value, fsConstants.R_OK).then(() => true, () => false)
}

function runCommand(command, args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { ...options, stdio: 'inherit' })
    child.once('error', reject)
    child.once('exit', (code, signal) => {
      if (code === 0) resolve()
      else reject(new Error(`${command} ${args[0] || ''} 종료: ${signal || `exit ${code}`}`))
    })
  })
}

function parseArguments(args) {
  let profile = ''
  let port = 0
  for (let index = 0; index < args.length; index += 2) {
    const name = args[index]
    const value = args[index + 1]
    if (!value) throw new Error(`인자가 필요합니다: ${name || '(없음)'}`)
    if (name === '--profile') profile = value
    else if (name === '--port') port = Number.parseInt(value, 10)
    else throw new Error(`지원하지 않는 인자입니다: ${name}`)
  }
  developmentCommand(profile, port)
  return { profile, port }
}

export async function main(args = process.argv.slice(2)) {
  const { profile, port } = parseArguments(args)
  const cwd = process.cwd()
  await prepareDependencies(cwd)
  const [command, commandArgs] = developmentCommand(profile, port)
  process.stdout.write(`[pie-preview] 개발 서버를 실행합니다: ${profile}\n`)
  await runCommand(command, commandArgs, { cwd, env: process.env })
}

if (import.meta.url === pathToFileURL(process.argv[1] || '').href) {
  main().catch((error) => {
    process.stderr.write(`[pie-preview] ${error?.message || String(error)}\n`)
    process.exitCode = 1
  })
}
