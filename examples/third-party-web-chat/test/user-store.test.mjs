import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import pg from 'pg'

import { hashPassword } from '../src/auth.mjs'
import { FileUserStore, PostgresUserStore, UserExistsError } from '../src/user-store.mjs'

const roots = []

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true })
})

test('file user store persists provisioning state without exposing it through shared references', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pie-web-chat-users-'))
  roots.push(root)
  const usersFile = join(root, 'users.json')
  const alice = await fixtureUser('alice', 'external-alice', 'alice-private-pat')
  writeFileSync(usersFile, `${JSON.stringify([alice])}\n`, { mode: 0o600 })
  chmodSync(usersFile, 0o600)
  const store = new FileUserStore([alice], usersFile)

  const first = await store.get('alice')
  assert.throws(() => { first.credential.pat = 'caller-mutation' }, TypeError)
  assert.equal((await store.get('alice')).credential.pat, 'alice-private-pat')
  await store.setProvisioningState('alice', 'failed', 'capacity reached')

  const persisted = JSON.parse(readFileSync(usersFile, 'utf8'))
  assert.equal(persisted[0].provisioningStatus, 'failed')
  assert.equal(persisted[0].lastProvisionError, 'capacity reached')
})

test('PostgreSQL user store encrypts credentials and survives a new store instance', {
  skip: !process.env.PIE_WEB_CHAT_TEST_DATABASE_URL,
}, async () => {
  const connectionString = process.env.PIE_WEB_CHAT_TEST_DATABASE_URL
  const key = randomBytes(32)
  const migration = new pg.Pool({ connectionString })
  await migration.query('CREATE SCHEMA IF NOT EXISTS pie_web_chat')
  await migration.end()
  const alice = await fixtureUser('alice', 'external-alice', 'alice-database-private-pat')
  const first = new PostgresUserStore({ connectionString, credentialKey: key, seedUsers: [alice] })
  await first.ready()
  assert.equal((await first.get('alice')).credential.pat, 'alice-database-private-pat')

  const bob = await fixtureUser('bob', 'external-bob', 'bob-database-private-pat')
  await first.create(bob)
  await assert.rejects(() => first.create(bob), UserExistsError)
  await first.setProvisioningState('bob', 'ready')

  const inspector = new pg.Pool({ connectionString })
  const raw = await inspector.query('SELECT credential_encrypted::text AS encrypted FROM pie_web_chat.users WHERE username = $1', ['bob'])
  assert.equal(raw.rowCount, 1)
  assert.equal(raw.rows[0].encrypted.includes('bob-database-private-pat'), false)
  await inspector.end()
  await first.close()

  const reopened = new PostgresUserStore({ connectionString, credentialKey: key })
  const loaded = await reopened.get('bob')
  assert.equal(loaded.provisioningStatus, 'ready')
  assert.equal(loaded.credential.pat, 'bob-database-private-pat')
  await reopened.close()
})

async function fixtureUser(username, externalUserId, pat) {
  return {
    username,
    displayName: username.toUpperCase(),
    externalUserId,
    passwordHash: await hashPassword(`${username}-password-2026`),
    credential: { pat },
  }
}
