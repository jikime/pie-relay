import assert from 'node:assert/strict'
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import { dependencyFingerprint, developmentCommand, prepareDependencies } from './preview-launcher.mjs'

test('개발 서버 프로필은 shell 문자열 없이 고정된 npm 인자로 변환된다', () => {
  assert.deepEqual(developmentCommand('next', 20000), ['npm', ['run', 'dev', '--', '--hostname', '0.0.0.0', '--port', '20000']])
  assert.deepEqual(developmentCommand('vite', 20001), ['npm', ['run', 'dev', '--', '--host', '0.0.0.0', '--port', '20001']])
  assert.deepEqual(developmentCommand('npm', 20002), ['npm', ['run', 'dev']])
  assert.throws(() => developmentCommand('shell', 20000), /지원하지 않는/)
  assert.throws(() => developmentCommand('npm', 80), /유효하지 않은/)
})

test('의존성은 최초와 package 변경 때만 다시 설치된다', async () => {
  const cwd = await mkdtemp(path.join(os.tmpdir(), 'pie-preview-launcher-'))
  const calls = []
  const run = async (command, args, options) => {
    calls.push({ command, args, cwd: options.cwd })
    await mkdir(path.join(cwd, 'node_modules'), { recursive: true })
  }
  try {
    await writeFile(path.join(cwd, 'package.json'), JSON.stringify({ scripts: { dev: 'next dev' }, dependencies: { next: '1.0.0' } }))
    assert.equal(await prepareDependencies(cwd, { run }), true)
    assert.equal(await prepareDependencies(cwd, { run }), false)
    assert.equal(calls.length, 1)
    assert.deepEqual(calls[0], { command: 'npm', args: ['install', '--no-audit', '--no-fund'], cwd })

    await writeFile(path.join(cwd, 'package.json'), JSON.stringify({ scripts: { dev: 'next dev' }, dependencies: { next: '2.0.0' } }))
    assert.equal(await prepareDependencies(cwd, { run }), true)
    assert.equal(calls.length, 2)
    assert.notEqual(await dependencyFingerprint(cwd), '')
  } finally {
    await rm(cwd, { recursive: true, force: true })
  }
})

test('package-lock이 있으면 npm ci를 선택한다', async () => {
  const cwd = await mkdtemp(path.join(os.tmpdir(), 'pie-preview-launcher-lock-'))
  const calls = []
  try {
    await writeFile(path.join(cwd, 'package.json'), JSON.stringify({ scripts: { dev: 'vite' } }))
    await writeFile(path.join(cwd, 'package-lock.json'), JSON.stringify({ lockfileVersion: 3 }))
    await prepareDependencies(cwd, {
      run: async (_command, args) => {
        calls.push(args)
        await mkdir(path.join(cwd, 'node_modules'), { recursive: true })
      },
    })
    assert.deepEqual(calls, [['ci', '--no-audit', '--no-fund']])
  } finally {
    await rm(cwd, { recursive: true, force: true })
  }
})

test('동시 준비 요청은 하나의 설치만 수행한다', async () => {
  const cwd = await mkdtemp(path.join(os.tmpdir(), 'pie-preview-launcher-concurrent-'))
  let installs = 0
  try {
    await writeFile(path.join(cwd, 'package.json'), JSON.stringify({ scripts: { dev: 'node server.mjs' } }))
    const run = async () => {
      installs += 1
      await new Promise((resolve) => setTimeout(resolve, 40))
      await mkdir(path.join(cwd, 'node_modules'), { recursive: true })
    }
    const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, Math.min(milliseconds, 5)))
    await Promise.all([prepareDependencies(cwd, { run, sleep }), prepareDependencies(cwd, { run, sleep })])
    assert.equal(installs, 1)
  } finally {
    await rm(cwd, { recursive: true, force: true })
  }
})

test('중단된 설치 프로세스의 잠금은 다음 실행에서 즉시 회수한다', async () => {
  const cwd = await mkdtemp(path.join(os.tmpdir(), 'pie-preview-launcher-orphan-'))
  let installs = 0
  try {
    await writeFile(path.join(cwd, 'package.json'), JSON.stringify({ scripts: { dev: 'node server.mjs' } }))
    const lockPath = path.join(cwd, '.pie-preview-install.lock')
    await mkdir(lockPath)
    await writeFile(path.join(lockPath, 'owner.json'), JSON.stringify({ pid: 2_147_483_647 }))
    await prepareDependencies(cwd, {
      run: async () => { installs += 1 },
      sleep: async () => {},
    })
    assert.equal(installs, 1)
  } finally {
    await rm(cwd, { recursive: true, force: true })
  }
})
