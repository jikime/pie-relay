import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto'
import { chmodSync, closeSync, fsyncSync, openSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'

import pg from 'pg'

const { Pool } = pg
const USERNAME = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/
const PROVISIONING_STATUSES = new Set(['registered', 'provisioning', 'ready', 'failed'])
const CREDENTIAL_FORMAT = 1
const IV_BYTES = 12
const TAG_BYTES = 16

export class UserExistsError extends Error {
  constructor(username) {
    super(`username already exists: ${username}`)
    this.name = 'UserExistsError'
  }
}

export class MemoryUserStore {
  constructor(seedUsers = []) {
    this.users = new Map(seedUsers.map((value) => [value.username, freezeUser(value)]))
  }

  async ready() {}

  async get(username) {
    return cloneUser(this.users.get(username))
  }

  async create(value) {
    validateUser(value)
    if (this.users.has(value.username)) throw new UserExistsError(value.username)
    const user = freezeUser({ ...value, provisioningStatus: 'registered', lastProvisionError: '' })
    this.users.set(user.username, user)
    return cloneUser(user)
  }

  async setProvisioningState(username, status, lastError = '') {
    if (!PROVISIONING_STATUSES.has(status)) throw new Error('invalid provisioning status')
    const current = this.users.get(username)
    if (!current) throw new Error(`user does not exist: ${username}`)
    const updated = freezeUser({ ...current, provisioningStatus: status, lastProvisionError: String(lastError).slice(0, 500) })
    this.users.set(username, updated)
    return cloneUser(updated)
  }

  async close() {}
}

export class FileUserStore extends MemoryUserStore {
  constructor(seedUsers, usersFile) {
    super(seedUsers)
    this.usersFile = usersFile
    this.pending = Promise.resolve()
  }

  async create(value) {
    return this.serialize(async () => {
      const user = await super.create(value)
      try {
        persistUsers(this.usersFile, this.users.values())
      } catch (error) {
        this.users.delete(user.username)
        throw error
      }
      return user
    })
  }

  async setProvisioningState(username, status, lastError = '') {
    return this.serialize(async () => {
      const before = this.users.get(username)
      const user = await super.setProvisioningState(username, status, lastError)
      try {
        persistUsers(this.usersFile, this.users.values())
      } catch (error) {
        this.users.set(username, before)
        throw error
      }
      return user
    })
  }

  serialize(operation) {
    const current = this.pending.then(operation)
    this.pending = current.catch(() => {})
    return current
  }
}

export class PostgresUserStore {
  constructor({ connectionString, credentialKey, seedUsers = [], PoolClass = Pool } = {}) {
    if (!connectionString) throw new Error('PostgreSQL connection string is required')
    if (!Buffer.isBuffer(credentialKey) || credentialKey.length !== 32) {
      throw new Error('credential encryption key must contain exactly 32 bytes')
    }
    this.credentialKey = Buffer.from(credentialKey)
    this.seedUsers = seedUsers.map((value) => freezeUser(value))
    this.pool = new PoolClass({
      connectionString,
      application_name: 'pie-third-party-web-chat',
      max: 5,
      idleTimeoutMillis: 30_000,
      connectionTimeoutMillis: 5_000,
    })
    this.initialization = null
  }

  async ready() {
    if (!this.initialization) this.initialization = this.initialize()
    return this.initialization
  }

  async initialize() {
    const client = await this.pool.connect()
    try {
      await client.query('BEGIN')
      await client.query(`
        CREATE TABLE IF NOT EXISTS pie_web_chat.users (
          username varchar(64) PRIMARY KEY,
          display_name varchar(120) NOT NULL,
          external_user_id varchar(512) NOT NULL UNIQUE,
          password_hash text NOT NULL,
          credential_encrypted bytea NOT NULL,
          provisioning_status varchar(32) NOT NULL DEFAULT 'registered',
          last_provision_error varchar(500) NOT NULL DEFAULT '',
          created_at timestamptz NOT NULL DEFAULT now(),
          updated_at timestamptz NOT NULL DEFAULT now(),
          CONSTRAINT pie_web_chat_users_status_check
            CHECK (provisioning_status IN ('registered', 'provisioning', 'ready', 'failed'))
        )
      `)
      for (const user of this.seedUsers) {
        validateUser(user)
        await client.query(`
          INSERT INTO pie_web_chat.users
            (username, display_name, external_user_id, password_hash, credential_encrypted,
             provisioning_status, last_provision_error)
          VALUES ($1, $2, $3, $4, $5, $6, $7)
          ON CONFLICT (username) DO NOTHING
        `, rowValues(user, this.credentialKey))
      }
      await client.query('COMMIT')
    } catch (error) {
      await client.query('ROLLBACK').catch(() => {})
      this.initialization = null
      throw error
    } finally {
      client.release()
    }
  }

  async get(username) {
    await this.ready()
    const result = await this.pool.query(`
      SELECT username, display_name, external_user_id, password_hash, credential_encrypted,
             provisioning_status, last_provision_error
        FROM pie_web_chat.users
       WHERE username = $1
    `, [username])
    return result.rowCount === 1 ? userFromRow(result.rows[0], this.credentialKey) : null
  }

  async create(value) {
    validateUser(value)
    await this.ready()
    try {
      const result = await this.pool.query(`
        INSERT INTO pie_web_chat.users
          (username, display_name, external_user_id, password_hash, credential_encrypted,
           provisioning_status, last_provision_error)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        RETURNING username, display_name, external_user_id, password_hash, credential_encrypted,
                  provisioning_status, last_provision_error
      `, rowValues({ ...value, provisioningStatus: 'registered', lastProvisionError: '' }, this.credentialKey))
      return userFromRow(result.rows[0], this.credentialKey)
    } catch (error) {
      if (error?.code === '23505') throw new UserExistsError(value.username)
      throw error
    }
  }

  async setProvisioningState(username, status, lastError = '') {
    if (!PROVISIONING_STATUSES.has(status)) throw new Error('invalid provisioning status')
    await this.ready()
    const result = await this.pool.query(`
      UPDATE pie_web_chat.users
         SET provisioning_status = $2,
             last_provision_error = $3,
             updated_at = now()
       WHERE username = $1
       RETURNING username, display_name, external_user_id, password_hash, credential_encrypted,
                 provisioning_status, last_provision_error
    `, [username, status, String(lastError).slice(0, 500)])
    if (result.rowCount !== 1) throw new Error(`user does not exist: ${username}`)
    return userFromRow(result.rows[0], this.credentialKey)
  }

  async close() {
    await this.pool.end()
  }
}

export function decodeCredentialKey(value) {
  const normalized = String(value ?? '').trim()
  if (!normalized) throw new Error('credential encryption key is empty')
  const decoded = Buffer.from(normalized, 'base64url')
  if (decoded.length !== 32 || decoded.toString('base64url') !== normalized.replace(/=+$/, '')) {
    throw new Error('credential encryption key must be canonical Base64URL for exactly 32 bytes')
  }
  return decoded
}

export function validateUser(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('each user must be an object')
  if (!USERNAME.test(value.username ?? '')) throw new Error('user.username is invalid')
  if (typeof value.displayName !== 'string' || value.displayName.trim() === '' || value.displayName.length > 120) {
    throw new Error(`displayName is invalid for ${value.username}`)
  }
  if (typeof value.externalUserId !== 'string' || value.externalUserId.trim() === '' || value.externalUserId.length > 512) {
    throw new Error(`externalUserId is invalid for ${value.username}`)
  }
  if (typeof value.passwordHash !== 'string' || !value.passwordHash.startsWith('scrypt-v1$')) {
    throw new Error(`passwordHash is invalid for ${value.username}`)
  }
  if (value.credential !== undefined && (!value.credential || typeof value.credential !== 'object' || Array.isArray(value.credential))) {
    throw new Error(`credential must be an object for ${value.username}`)
  }
  if (value.provisioningStatus !== undefined && !PROVISIONING_STATUSES.has(value.provisioningStatus)) {
    throw new Error(`provisioningStatus is invalid for ${value.username}`)
  }
}

function rowValues(user, key) {
  return [
    user.username,
    user.displayName.trim(),
    user.externalUserId,
    user.passwordHash,
    encryptCredential(key, user.credential ?? {}, user.externalUserId),
    user.provisioningStatus || 'registered',
    String(user.lastProvisionError ?? '').slice(0, 500),
  ]
}

function userFromRow(row, key) {
  return freezeUser({
    username: row.username,
    displayName: row.display_name,
    externalUserId: row.external_user_id,
    passwordHash: row.password_hash,
    credential: decryptCredential(key, row.credential_encrypted, row.external_user_id),
    provisioningStatus: row.provisioning_status,
    lastProvisionError: row.last_provision_error,
  })
}

function encryptCredential(key, value, associatedData) {
  const iv = randomBytes(IV_BYTES)
  const cipher = createCipheriv('aes-256-gcm', key, iv)
  cipher.setAAD(Buffer.from(associatedData))
  const ciphertext = Buffer.concat([cipher.update(JSON.stringify(value), 'utf8'), cipher.final()])
  return Buffer.concat([Buffer.from([CREDENTIAL_FORMAT]), iv, cipher.getAuthTag(), ciphertext])
}

function decryptCredential(key, encoded, associatedData) {
  const value = Buffer.from(encoded)
  if (value.length < 1 + IV_BYTES + TAG_BYTES || value[0] !== CREDENTIAL_FORMAT) {
    throw new Error('stored credential has an unsupported format')
  }
  const iv = value.subarray(1, 1 + IV_BYTES)
  const tag = value.subarray(1 + IV_BYTES, 1 + IV_BYTES + TAG_BYTES)
  const ciphertext = value.subarray(1 + IV_BYTES + TAG_BYTES)
  const decipher = createDecipheriv('aes-256-gcm', key, iv)
  decipher.setAAD(Buffer.from(associatedData))
  decipher.setAuthTag(tag)
  const plaintext = Buffer.concat([decipher.update(ciphertext), decipher.final()]).toString('utf8')
  const parsed = JSON.parse(plaintext)
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('stored credential is invalid')
  return parsed
}

function freezeUser(value) {
  validateUser(value)
  return Object.freeze({
    username: value.username,
    displayName: value.displayName.trim(),
    externalUserId: value.externalUserId,
    passwordHash: value.passwordHash,
    credential: Object.freeze(structuredClone(value.credential ?? {})),
    provisioningStatus: value.provisioningStatus || 'registered',
    lastProvisionError: String(value.lastProvisionError ?? '').slice(0, 500),
  })
}

function cloneUser(value) {
  return value ? freezeUser(structuredClone(value)) : null
}

function persistUsers(path, values) {
  const temporary = `${path}.${process.pid}.${randomBytes(8).toString('hex')}.tmp`
  let fd
  try {
    fd = openSync(temporary, 'wx', 0o600)
    writeFileSync(fd, `${JSON.stringify([...values], null, 2)}\n`)
    fsyncSync(fd)
    closeSync(fd)
    fd = undefined
    renameSync(temporary, path)
    chmodSync(path, 0o600)
    const directory = openSync(dirname(path), 'r')
    try { fsyncSync(directory) } finally { closeSync(directory) }
  } catch (error) {
    if (fd !== undefined) closeSync(fd)
    try { unlinkSync(temporary) } catch { /* already renamed or absent */ }
    throw error
  }
}
