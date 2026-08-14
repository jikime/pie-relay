#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import { existsSync, statSync } from 'node:fs'
import { resolve } from 'node:path'

const repoRoot = resolve(import.meta.dirname, '../..')
const krootADK = resolve(process.env.KROOT_ADK_DIR?.trim() || '/Users/jikime/Dev/Business/kaonsoftlab/kroot-adk')
const krootProto = resolve(process.env.KROOT_PROTO_DIR?.trim() || '/Users/jikime/Dev/Business/kaonsoftlab/kroot-proto')
const baseImage = process.env.PIE_EXECUTOR_BASE_IMAGE?.trim() || 'pie-relay-client:latest'
const image = process.env.PIE_KROOT_EXECUTOR_IMAGE?.trim() || 'pie-relay-client-kroot:local'
const skipBase = process.env.PIE_SKIP_EXECUTOR_BASE_BUILD === 'true'

assertDirectory(krootADK, 'KROOT_ADK_DIR')
assertDirectory(krootProto, 'KROOT_PROTO_DIR')
assertFile(resolve(krootADK, 'go.mod'), 'Kroot ADK go.mod')
assertFile(resolve(krootProto, 'go.mod'), 'Kroot proto go.mod')

if (!skipBase) {
  run('docker', [
    'build', '--progress=plain', '-f', resolve(repoRoot, 'executor-manager/Dockerfile.executor'),
    '-t', baseImage, repoRoot,
  ])
} else {
  run('docker', ['image', 'inspect', baseImage], { quiet: true })
}

const adkRevision = gitRevision(krootADK)
const protoRevision = gitRevision(krootProto)
run('docker', [
  'build', '--progress=plain',
  '-f', resolve(repoRoot, 'executor-manager/Dockerfile.executor-kroot'),
  '--build-context', `kroot-adk=${krootADK}`,
  '--build-context', `kroot-proto=${krootProto}`,
  '--build-arg', `PIE_EXECUTOR_BASE_IMAGE=${baseImage}`,
  '--build-arg', `KROOT_ADK_REVISION=${adkRevision}`,
  '--build-arg', `KROOT_PROTO_REVISION=${protoRevision}`,
  '-t', image, repoRoot,
])

const versionOutput = run('docker', [
  'run', '--rm', '--entrypoint', '/usr/local/bin/kroot', image, '--help',
], { capture: true }).trim().split('\n')[0]
const identity = JSON.parse(run('docker', [
  'run', '--rm', '--entrypoint', 'node', image, '-e',
  "const fs=require('fs'); const s=fs.statSync('/usr/local/bin/kroot'); process.stdout.write(JSON.stringify({uid:process.getuid(),gid:process.getgid(),home:process.env.HOME,executable:Boolean(s.mode&0o111)}))",
], { capture: true }))
if (identity.uid !== 10001 || identity.gid !== 10001 || identity.home !== '/home/executor' || !identity.executable) {
  throw new Error(`invalid Executor image identity: ${JSON.stringify(identity)}`)
}

console.log(JSON.stringify({
  ok: true,
  image,
  baseImage,
  krootADK: { path: krootADK, revision: adkRevision },
  krootProto: { path: krootProto, revision: protoRevision },
  command: versionOutput,
  runtime: identity,
}, null, 2))

function gitRevision(directory) {
  try {
    return run('git', ['rev-parse', '--short=12', 'HEAD'], { cwd: directory, capture: true }).trim()
  } catch {
    return 'unknown'
  }
}

function assertDirectory(path, name) {
  if (!existsSync(path) || !statSync(path).isDirectory()) throw new Error(`${name} is not a directory: ${path}`)
}

function assertFile(path, name) {
  if (!existsSync(path) || !statSync(path).isFile()) throw new Error(`${name} is missing: ${path}`)
}

function run(command, args, { cwd = repoRoot, capture = false, quiet = false } = {}) {
  return execFileSync(command, args, {
    cwd,
    encoding: 'utf8',
    stdio: capture ? ['ignore', 'pipe', 'pipe'] : quiet ? 'ignore' : 'inherit',
  }) || ''
}
