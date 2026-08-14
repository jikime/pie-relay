import { readFileSync, statSync } from 'node:fs'
import { resolve } from 'node:path'

import { DEFAULT_KROOT_RELAY_URL, DEFAULT_KROOT_SERVER_URL } from './kroot-credential.mjs'
import { decodeCredentialKey, FileUserStore, PostgresUserStore, UserExistsError, validateUser } from './user-store.mjs'

export { UserExistsError } from './user-store.mjs'

// Used only to keep unknown-user login verification on the same scrypt path.
// It is not a credential and can never authenticate an account.
const FALLBACK_PASSWORD_HASH = 'scrypt-v1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'

export function loadConfig(env = process.env) {
  const managerURL = requiredURL(env.PIE_MANAGER_URL, 'PIE_MANAGER_URL')
  const integrationID = required(env.PIE_INTEGRATION_ID, 'PIE_INTEGRATION_ID')
  const integrationToken = integrationSecret(env)
  if (!integrationToken.startsWith('pie_int_')) {
    throw new Error('PIE_INTEGRATION_TOKEN must be a Pie Integration service token')
  }
  const usersFile = resolve(required(env.PIE_WEB_CHAT_USERS_FILE, 'PIE_WEB_CHAT_USERS_FILE'))
  const info = privateFile(usersFile, 'PIE_WEB_CHAT_USERS_FILE', env.PIE_ALLOW_INSECURE_USERS_FILE === 'true')
  const parsed = JSON.parse(readFileSync(usersFile, 'utf8'))
  const databaseURL = optionalSecret(env, 'PIE_WEB_CHAT_DATABASE_URL', 'PIE_WEB_CHAT_DATABASE_URL_FILE')
  if (!Array.isArray(parsed) || (parsed.length === 0 && !databaseURL)) {
    throw new Error(databaseURL
      ? 'users file must contain a JSON array'
      : 'file user store requires at least one seed user')
  }
  const users = []
  const usernames = new Set()
  for (const value of parsed) {
    validateUser(value)
    if (usernames.has(value.username)) throw new Error(`duplicate username: ${value.username}`)
    usernames.add(value.username)
    users.push(Object.freeze({
      username: value.username,
      displayName: value.displayName,
      externalUserId: value.externalUserId,
      passwordHash: value.passwordHash,
      credential: structuredClone(value.credential ?? {}),
      provisioningStatus: value.provisioningStatus || 'registered',
      lastProvisionError: value.lastProvisionError || '',
    }))
  }
  const userStore = databaseURL
    ? new PostgresUserStore({
      connectionString: postgresURL(databaseURL),
      credentialKey: decodeCredentialKey(requiredSecret(env, 'PIE_WEB_CHAT_CREDENTIAL_KEY', 'PIE_WEB_CHAT_CREDENTIAL_KEY_FILE')),
      seedUsers: users,
    })
    : new FileUserStore(users, usersFile)
  return {
    host: env.PIE_WEB_CHAT_HOST?.trim() || '127.0.0.1',
    port: positiveInteger(env.PIE_WEB_CHAT_PORT, 4175, 65_535),
    managerURL,
    integrationID,
    integrationToken,
    krootServerURL: endpointURL(env.PIE_KROOT_SERVER_URL || DEFAULT_KROOT_SERVER_URL, 'PIE_KROOT_SERVER_URL', ['grpc:', 'grpcs:']),
    krootRelayURL: endpointURL(env.PIE_KROOT_RELAY_URL || DEFAULT_KROOT_RELAY_URL, 'PIE_KROOT_RELAY_URL', ['ws:', 'wss:']),
    // 로컬 실사용 E2E 전용이다. 운영 서비스는 가입 사용자의 credential을 자체 IdP/credential
    // broker에서 발급해 userStore에 넣어야 하며, 여러 사용자에게 이 값을 공유하면 안 된다.
    signupKrootPAT: optionalSecret(env, 'PIE_WEB_CHAT_SIGNUP_KROOT_PAT', 'PIE_WEB_CHAT_SIGNUP_KROOT_PAT_FILE'),
    userStore,
    fallbackPasswordHash: users[0]?.passwordHash || FALLBACK_PASSWORD_HASH,
    userStoreKind: databaseURL ? 'postgres' : 'file',
    registrationEnabled: env.PIE_WEB_CHAT_REGISTRATION_ENABLED === 'true',
    secureCookie: env.PIE_WEB_CHAT_SECURE_COOKIE === 'true',
    publicOrigin: env.PIE_WEB_CHAT_PUBLIC_ORIGIN ? requiredURL(env.PIE_WEB_CHAT_PUBLIC_ORIGIN, 'PIE_WEB_CHAT_PUBLIC_ORIGIN') : '',
    sessionTTLms: positiveInteger(env.PIE_WEB_CHAT_SESSION_TTL_SECONDS, 28_800, 604_800) * 1000,
  }
}

function integrationSecret(env) {
  return requiredSecret(env, 'PIE_INTEGRATION_TOKEN', 'PIE_INTEGRATION_TOKEN_FILE')
}

function required(value, name) {
  const trimmed = value?.trim()
  if (!trimmed) throw new Error(`${name} is required`)
  return trimmed
}

function optionalSecret(env, inlineName, fileName) {
  const inline = env[inlineName]?.trim()
  const file = env[fileName]?.trim()
  if (inline && file) throw new Error(`set only one of ${inlineName} or ${fileName}`)
  if (inline) return inline
  if (!file) return ''
  privateFile(file, fileName)
  const value = readFileSync(file, 'utf8').trim()
  if (!value) throw new Error(`${fileName} is empty`)
  return value
}

function requiredSecret(env, inlineName, fileName) {
  const value = optionalSecret(env, inlineName, fileName)
  if (!value) throw new Error(`${inlineName} or ${fileName} is required`)
  return value
}

function privateFile(path, name, allowInsecure = false) {
  const info = statSync(path)
  if (!info.isFile()) throw new Error(`${name} must be a regular file`)
  if (!allowInsecure && (info.mode & 0o077) !== 0) throw new Error(`${name} must not be readable by group or others (chmod 600)`)
  return info
}

function postgresURL(value) {
  const parsed = new URL(value)
  if (!['postgres:', 'postgresql:'].includes(parsed.protocol) || !parsed.hostname || !parsed.pathname || parsed.hash) {
    throw new Error('PIE_WEB_CHAT_DATABASE_URL must be a PostgreSQL connection URL')
  }
  return parsed.toString()
}

function requiredURL(value, name) {
  const parsed = new URL(required(value, name))
  if (!['http:', 'https:'].includes(parsed.protocol) || parsed.username || parsed.password || parsed.search || parsed.hash) {
    throw new Error(`${name} must be an HTTP(S) origin`)
  }
  parsed.pathname = parsed.pathname.replace(/\/$/, '')
  return parsed.toString().replace(/\/$/, '')
}

function endpointURL(value, name, protocols) {
  const parsed = new URL(required(value, name))
  if (!protocols.includes(parsed.protocol) || parsed.username || parsed.password || parsed.hash) {
    throw new Error(`${name} must use ${protocols.join(' or ')} without embedded credentials or a fragment`)
  }
  return parsed.toString()
}

function positiveInteger(value, fallback, max) {
  if (value === undefined || value === '') return fallback
  const parsed = Number(value)
  if (!Number.isSafeInteger(parsed) || parsed < 1 || parsed > max) throw new Error('invalid positive integer configuration')
  return parsed
}
