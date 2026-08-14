import assert from 'node:assert/strict'
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'

import { hashPassword } from '../src/auth.mjs'
import { loadConfig } from '../src/config.mjs'

const roots = []

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true })
})

test('loads the Integration token from a mounted secret file', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pie-web-chat-config-'))
  roots.push(root)
  const usersFile = join(root, 'users.json')
  const tokenFile = join(root, 'integration-token')
  writeFileSync(usersFile, JSON.stringify([{
    username: 'alice', displayName: 'Alice', externalUserId: 'alice-e2e',
    passwordHash: await hashPassword('alice-password'), credential: {},
  }]), { mode: 0o600 })
  writeFileSync(tokenFile, 'pie_int_from_secret_file\n', { mode: 0o600 })
  chmodSync(usersFile, 0o600)

  const config = loadConfig({
    PIE_MANAGER_URL: 'https://api-relay.cookai.dev',
    PIE_INTEGRATION_ID: 'cookai-e2e',
    PIE_INTEGRATION_TOKEN_FILE: tokenFile,
    PIE_WEB_CHAT_USERS_FILE: usersFile,
  })

  assert.equal(config.integrationToken, 'pie_int_from_secret_file')
  assert.equal(config.managerURL, 'https://api-relay.cookai.dev')
})

test('rejects ambiguous inline and file Integration secrets', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pie-web-chat-config-'))
  roots.push(root)
  const usersFile = join(root, 'users.json')
  const tokenFile = join(root, 'integration-token')
  writeFileSync(usersFile, JSON.stringify([{
    username: 'alice', displayName: 'Alice', externalUserId: 'alice-e2e',
    passwordHash: await hashPassword('alice-password'), credential: {},
  }]), { mode: 0o600 })
  writeFileSync(tokenFile, 'pie_int_from_secret_file\n', { mode: 0o600 })

  assert.throws(() => loadConfig({
    PIE_MANAGER_URL: 'https://api-relay.cookai.dev',
    PIE_INTEGRATION_ID: 'cookai-e2e',
    PIE_INTEGRATION_TOKEN: 'pie_int_inline',
    PIE_INTEGRATION_TOKEN_FILE: tokenFile,
    PIE_WEB_CHAT_USERS_FILE: usersFile,
  }), /set only one/)
})

test('loads the local signup Kroot PAT only from a protected server secret', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pie-web-chat-config-'))
  roots.push(root)
  const usersFile = join(root, 'users.json')
  const patFile = join(root, 'signup-kroot-pat')
  writeFileSync(usersFile, JSON.stringify([{
    username: 'alice', displayName: 'Alice', externalUserId: 'alice-e2e',
    passwordHash: await hashPassword('alice-password'), credential: {},
  }]), { mode: 0o600 })
  writeFileSync(patFile, 'kpat_local_signup_fixture\n', { mode: 0o600 })

  const config = loadConfig({
    PIE_MANAGER_URL: 'https://api-relay.cookai.dev',
    PIE_INTEGRATION_ID: 'cookai-e2e',
    PIE_INTEGRATION_TOKEN: 'pie_int_inline',
    PIE_WEB_CHAT_USERS_FILE: usersFile,
    PIE_WEB_CHAT_SIGNUP_KROOT_PAT_FILE: patFile,
  })

  assert.equal(config.signupKrootPAT, 'kpat_local_signup_fixture')
})

test('configures a PostgreSQL user store from protected secret files', async () => {
  const root = mkdtempSync(join(tmpdir(), 'pie-web-chat-config-'))
  roots.push(root)
  const usersFile = join(root, 'users.json')
  const tokenFile = join(root, 'integration-token')
  const databaseFile = join(root, 'database-url')
  const credentialKeyFile = join(root, 'credential-key')
  writeFileSync(usersFile, JSON.stringify([{
    username: 'alice', displayName: 'Alice', externalUserId: 'alice-e2e',
    passwordHash: await hashPassword('alice-password'), credential: {},
  }]), { mode: 0o600 })
  writeFileSync(tokenFile, 'pie_int_from_secret_file\n', { mode: 0o600 })
  writeFileSync(databaseFile, 'postgres://pie_web_chat:secret@postgres:5432/pie_relay?sslmode=disable\n', { mode: 0o600 })
  writeFileSync(credentialKeyFile, `${Buffer.alloc(32, 7).toString('base64url')}\n`, { mode: 0o600 })

  const config = loadConfig({
    PIE_MANAGER_URL: 'https://api-relay.cookai.dev',
    PIE_INTEGRATION_ID: 'cookai-e2e',
    PIE_INTEGRATION_TOKEN_FILE: tokenFile,
    PIE_WEB_CHAT_USERS_FILE: usersFile,
    PIE_WEB_CHAT_DATABASE_URL_FILE: databaseFile,
    PIE_WEB_CHAT_CREDENTIAL_KEY_FILE: credentialKeyFile,
  })

  assert.equal(config.userStoreKind, 'postgres')
  await config.userStore.close()
})

test('allows an empty seed after PostgreSQL becomes the durable user store', () => {
  const root = mkdtempSync(join(tmpdir(), 'pie-web-chat-config-'))
  roots.push(root)
  const usersFile = join(root, 'users.json')
  const tokenFile = join(root, 'integration-token')
  const databaseFile = join(root, 'database-url')
  const credentialKeyFile = join(root, 'credential-key')
  writeFileSync(usersFile, '[]\n', { mode: 0o600 })
  writeFileSync(tokenFile, 'pie_int_from_secret_file\n', { mode: 0o600 })
  writeFileSync(databaseFile, 'postgres://pie_web_chat:secret@postgres:5432/pie_relay?sslmode=disable\n', { mode: 0o600 })
  writeFileSync(credentialKeyFile, `${Buffer.alloc(32, 7).toString('base64url')}\n`, { mode: 0o600 })

  const config = loadConfig({
    PIE_MANAGER_URL: 'https://api-relay.cookai.dev',
    PIE_INTEGRATION_ID: 'cookai-e2e',
    PIE_INTEGRATION_TOKEN_FILE: tokenFile,
    PIE_WEB_CHAT_USERS_FILE: usersFile,
    PIE_WEB_CHAT_DATABASE_URL_FILE: databaseFile,
    PIE_WEB_CHAT_CREDENTIAL_KEY_FILE: credentialKeyFile,
  })

  assert.equal(config.userStoreKind, 'postgres')
  assert.match(config.fallbackPasswordHash, /^scrypt-v1\$/)
  return config.userStore.close()
})

test('still rejects an empty seed for the file user store', () => {
  const root = mkdtempSync(join(tmpdir(), 'pie-web-chat-config-'))
  roots.push(root)
  const usersFile = join(root, 'users.json')
  writeFileSync(usersFile, '[]\n', { mode: 0o600 })

  assert.throws(() => loadConfig({
    PIE_MANAGER_URL: 'https://api-relay.cookai.dev',
    PIE_INTEGRATION_ID: 'cookai-e2e',
    PIE_INTEGRATION_TOKEN: 'pie_int_inline',
    PIE_WEB_CHAT_USERS_FILE: usersFile,
  }), /requires at least one seed user/)
})
