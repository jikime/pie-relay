#!/usr/bin/env node

import { createHash } from 'node:crypto'
import { createServer } from 'node:http'
import {
  chmodSync,
  cpSync,
  existsSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  readlinkSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawn } from 'node:child_process'

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const fixtureRoot = mkdtempSync(join(tmpdir(), 'pie-client-installer-e2e-'))
const releaseRoot = join(fixtureRoot, 'releases')
const payloadRoot = join(fixtureRoot, 'payload')
const version = 'v9.9.9'
const targetOS = process.platform === 'darwin' ? 'darwin' : process.platform === 'linux' ? 'linux' : ''
const targetArch = process.arch === 'arm64' ? 'arm64' : process.arch === 'x64' ? 'amd64' : ''
if (!targetOS || !targetArch) throw new Error(`unsupported test platform: ${process.platform}/${process.arch}`)
const assetName = `pie-client_${targetOS}_${targetArch}.tar.gz`
let server

try {
  const fakeBin = join(fixtureRoot, 'bin')
  mkdirSync(fakeBin, { recursive: true })
  writeFileSync(join(fakeBin, 'npm'), '#!/bin/sh\necho "npm must not run for a native release" >&2\nexit 97\n')
  chmodSync(join(fakeBin, 'npm'), 0o755)
  mkdirSync(join(payloadRoot, 'node-executor'), { recursive: true })
  await run('go', [
    'build', '-trimpath', '-buildvcs=false',
    '-ldflags', `-s -w -X main.clientVersion=${version} -X main.clientCommit=installer-e2e -X main.clientBuildDate=2026-08-14T00:00:00Z`,
    '-o', join(payloadRoot, 'pie-client-bin'), './cmd/client',
  ], { cwd: join(repoRoot, 'client') })
  chmodSync(join(payloadRoot, 'pie-client-bin'), 0o755)
  writeFileSync(join(payloadRoot, 'VERSION'), `${version}\n`)
  writeFileSync(join(payloadRoot, 'RUNTIME_READY'), `${targetOS}/${targetArch}\n`)
  writeFileSync(join(payloadRoot, 'node-executor', 'package.json'), JSON.stringify({
    name: 'pie-client-installer-fixture', version: '1.0.0', private: true, type: 'module', dependencies: {},
  }))
  writeFileSync(join(payloadRoot, 'node-executor', 'package-lock.json'), JSON.stringify({
    name: 'pie-client-installer-fixture', version: '1.0.0', lockfileVersion: 3, requires: true,
    packages: { '': { name: 'pie-client-installer-fixture', version: '1.0.0' } },
  }))
  for (const file of ['executor.mjs', 'acp-executor.mjs', 'pty-host.mjs']) {
    writeFileSync(join(payloadRoot, 'node-executor', file), '#!/usr/bin/env node\n')
  }
  const nodePTY = join(payloadRoot, 'node-executor', 'node_modules', 'node-pty')
  const nodeBin = join(payloadRoot, 'node-executor', 'node_modules', '.bin')
  mkdirSync(nodePTY, { recursive: true })
  mkdirSync(nodeBin, { recursive: true })
  writeFileSync(join(nodePTY, 'package.json'), JSON.stringify({ name: 'node-pty', version: '0.0.0', main: 'index.js' }))
  writeFileSync(join(nodePTY, 'index.js'), 'module.exports = {}\n')
  writeFileSync(join(nodeBin, 'claude-agent-acp'), '#!/bin/sh\nexit 0\n')
  chmodSync(join(nodeBin, 'claude-agent-acp'), 0o755)

  const latestDir = join(releaseRoot, 'latest', 'download')
  const versionDir = join(releaseRoot, 'download', version)
  mkdirSync(latestDir, { recursive: true })
  mkdirSync(versionDir, { recursive: true })
  const assetPath = join(latestDir, assetName)
  await run('tar', ['-C', payloadRoot, '-czf', assetPath, '.'])
  const checksum = createHash('sha256').update(readFileSync(assetPath)).digest('hex')
  const checksums = `${checksum}  ${assetName}\n`
  writeFileSync(join(latestDir, 'pie-client_checksums.txt'), checksums)
  cpSync(assetPath, join(versionDir, assetName))
  writeFileSync(join(versionDir, 'pie-client_checksums.txt'), checksums)

  server = createServer((request, response) => {
    const path = join(releaseRoot, decodeURIComponent(new URL(request.url, 'http://localhost').pathname))
    if (!path.startsWith(`${releaseRoot}/`) || !existsSync(path)) {
      response.writeHead(404).end('not found')
      return
    }
    response.writeHead(200, { 'Content-Type': 'application/octet-stream' })
    response.end(readFileSync(path))
  })
  await new Promise((resolvePromise, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolvePromise)
  })
  const port = server.address().port
  const releaseURL = `http://127.0.0.1:${port}`

  const firstHome = join(fixtureRoot, 'home-success')
  mkdirSync(firstHome, { recursive: true })
  const first = await install(firstHome, releaseURL, [])
  assertIncludes(first.stdout, `pie-client ${version}`)
  const installed = join(firstHome, '.local', 'bin', 'pie-client')
  if (!existsSync(installed)) throw new Error('installer did not create ~/.local/bin/pie-client')
  if (!readlinkSync(installed).includes(`${version}-${checksum.slice(0, 12)}`)) {
    throw new Error(`versioned install target is missing: ${readlinkSync(installed)}`)
  }
  const versionOutput = await run(installed, ['version', '--json'])
  const metadata = JSON.parse(versionOutput.stdout)
  if (metadata.version !== version || metadata.commit !== 'installer-e2e') {
    throw new Error(`installed version metadata mismatch: ${versionOutput.stdout}`)
  }
  const repeated = await install(firstHome, releaseURL, [])
  assertIncludes(repeated.stdout, `pie-client ${version}`)

  const pinnedHome = join(fixtureRoot, 'home-pinned')
  mkdirSync(pinnedHome, { recursive: true })
  const pinned = await install(pinnedHome, releaseURL, ['--version', version])
  assertIncludes(pinned.stdout, `pie-client ${version}`)

  writeFileSync(assetPath, Buffer.concat([readFileSync(assetPath), Buffer.from('tampered')]))
  const tamperedHome = join(fixtureRoot, 'home-tampered')
  mkdirSync(tamperedHome, { recursive: true })
  const rejected = await install(tamperedHome, releaseURL, [], true)
  if (rejected.code === 0 || !`${rejected.stdout}\n${rejected.stderr}`.includes('SHA-256')) {
    throw new Error(`tampered package was not rejected: ${JSON.stringify(rejected)}`)
  }

  console.log(JSON.stringify({
    ok: true,
    platform: `${targetOS}/${targetArch}`,
    latestInstalled: true,
    idempotentReinstall: true,
    pinnedVersionInstalled: true,
    checksumTamperingRejected: true,
    version,
  }))
} finally {
  if (server) await new Promise((resolvePromise) => server.close(resolvePromise))
  rmSync(fixtureRoot, { recursive: true, force: true })
}

function install(home, releaseURL, args, allowFailure = false) {
  return run('sh', [join(repoRoot, 'install.sh'), ...args], {
    env: {
      ...process.env,
      PATH: `${join(fixtureRoot, 'bin')}:${process.env.PATH}`,
      HOME: home,
      XDG_DATA_HOME: join(home, '.local', 'share'),
      PIE_CLIENT_RELEASE_BASE_URL: releaseURL,
    },
    allowFailure,
  })
}

function assertIncludes(value, expected) {
  if (!value.includes(expected)) throw new Error(`expected ${JSON.stringify(expected)} in ${JSON.stringify(value)}`)
}

function run(command, args, { cwd = repoRoot, env = process.env, allowFailure = false } = {}) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, { cwd, env, stdio: ['ignore', 'pipe', 'pipe'] })
    let stdout = ''
    let stderr = ''
    child.stdout.on('data', (chunk) => { stdout += chunk })
    child.stderr.on('data', (chunk) => { stderr += chunk })
    child.once('error', reject)
    child.once('exit', (code, signal) => {
      const result = { code: code ?? 1, signal, stdout, stderr }
      if (!allowFailure && (code !== 0 || signal)) {
        reject(new Error(`${command} ${args.join(' ')} failed: ${JSON.stringify(result)}`))
        return
      }
      resolvePromise(result)
    })
  })
}
