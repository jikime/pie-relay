import assert from 'node:assert/strict'
import { access, symlink } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { mkdtemp } from 'node:fs/promises'
import test from 'node:test'

import { isMainModule, packageNameFor, resolveBundledClaude } from './claude-cli.mjs'

test('packageNameFor selects glibc and musl SDK packages deterministically', () => {
  const glibc = { getReport: () => ({ header: { glibcVersionRuntime: '2.36' } }) }
  const musl = { getReport: () => ({ header: {} }) }
  assert.equal(packageNameFor('linux', 'x64', glibc), '@anthropic-ai/claude-agent-sdk-linux-x64')
  assert.equal(packageNameFor('linux', 'arm64', musl), '@anthropic-ai/claude-agent-sdk-linux-arm64-musl')
  assert.equal(packageNameFor('darwin', 'arm64', musl), '@anthropic-ai/claude-agent-sdk-darwin-arm64')
  assert.throws(() => packageNameFor('linux', 'ppc64', glibc), /not bundled/)
})

test('resolveBundledClaude finds the SDK native executable for this platform', async () => {
  const executable = resolveBundledClaude()
  await access(executable)
  assert.match(executable, /claude(?:\.exe)?$/)
})

test('isMainModule recognizes the production symlink entrypoint', async () => {
  const root = await mkdtemp(join(tmpdir(), 'pie-claude-cli-'))
  const link = join(root, 'claude')
  await symlink(new URL('./claude-cli.mjs', import.meta.url), link)
  assert.equal(isMainModule(link), true)
})
