import assert from 'node:assert/strict'
import { mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  handleWorkspaceRequest,
  listWorkspace,
  normalizeWorkspacePath,
  readWorkspaceFile,
  writeWorkspaceFile,
} from './workspace.mjs'

function fixture(t) {
  const root = path.join(os.tmpdir(), `pie-workspace-${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}`)
  const project = path.join(root, 'project-a')
  mkdirSync(path.join(project, 'src'), { recursive: true })
  writeFileSync(path.join(project, 'src', 'index.ts'), 'export const value = 1\n')
  mkdirSync(path.join(project, '.kroot'))
  writeFileSync(path.join(project, '.kroot', 'credential.json'), '{"pat":"secret"}')
  writeFileSync(path.join(project, '.env'), 'SECRET=yes\n')
  process.env.PIE_WORKSPACE_ROOT = root
  t.after(() => {
    delete process.env.PIE_WORKSPACE_ROOT
    rmSync(root, { recursive: true, force: true })
  })
  return { root, project }
}

test('workspace path accepts only protected-free relative paths', () => {
  assert.equal(normalizeWorkspacePath('src/app.ts'), 'src/app.ts')
  for (const value of ['/etc/passwd', '../secret', 'src/../../secret', 'src\\app.ts', '.env', '.kroot/credential.json']) {
    assert.throws(() => normalizeWorkspacePath(value))
  }
})

test('workspace list hides credentials and returns directories before files', (t) => {
  const { project } = fixture(t)
  writeFileSync(path.join(project, 'README.md'), '# Hello\n')
  const result = listWorkspace(project)
  assert.deepEqual(result.entries.map(({ name, type }) => ({ name, type })), [
    { name: 'src', type: 'directory' },
    { name: 'README.md', type: 'file' },
  ])
})

test('workspace read and atomic revision-checked write', (t) => {
  const { project } = fixture(t)
  const opened = readWorkspaceFile(project, 'src/index.ts')
  assert.equal(opened.content, 'export const value = 1\n')
  assert.match(opened.revision, /^sha256:[a-f0-9]{64}$/)
  assert.equal(opened.language, 'typescript')

  const saved = writeWorkspaceFile(project, 'src/index.ts', 'export const value = 2\n', opened.revision)
  assert.notEqual(saved.revision, opened.revision)
  assert.equal(readFileSync(path.join(project, 'src', 'index.ts'), 'utf8'), 'export const value = 2\n')

  assert.throws(
    () => writeWorkspaceFile(project, 'src/index.ts', 'stale\n', opened.revision),
    (error) => error.code === 'conflict' && error.details.currentRevision === saved.revision,
  )
})

test('workspace rejects symlink escapes and binary files', (t) => {
  const { root, project } = fixture(t)
  const outside = path.join(root, 'outside.txt')
  writeFileSync(outside, 'outside')
  symlinkSync(outside, path.join(project, 'link.txt'))
  symlinkSync(path.join(project, '.kroot'), path.join(project, 'innocent-directory'))
  writeFileSync(path.join(project, 'binary.dat'), Buffer.from([1, 0, 2]))
  assert.throws(() => readWorkspaceFile(project, 'link.txt'), (error) => error.code === 'forbidden')
  assert.throws(() => readWorkspaceFile(project, 'innocent-directory/credential.json'), (error) => error.code === 'forbidden')
  assert.throws(() => readWorkspaceFile(project, 'binary.dat'), (error) => error.code === 'binary')
})

test('workspace result keeps errors out of the generic chat error channel', (t) => {
  const { project } = fixture(t)
  const denied = handleWorkspaceRequest({
    type: 'workspace', requestId: 'request-a', operation: 'read', projectPath: project, path: '.kroot/credential.json',
  })
  assert.equal(denied.type, 'workspace_result')
  assert.equal(denied.ok, false)
  assert.equal(denied.error.code, 'forbidden')
})
