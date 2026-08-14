import { createHash, randomBytes, scrypt as scryptCallback, timingSafeEqual } from 'node:crypto'
import { promisify } from 'node:util'

const scrypt = promisify(scryptCallback)
const KEY_LENGTH = 32
const SCRYPT_OPTIONS = { N: 16_384, r: 8, p: 1, maxmem: 64 * 1024 * 1024 }

export async function hashPassword(password, salt = randomBytes(16)) {
  if (typeof password !== 'string' || password.length < 10 || password.length > 1024) {
    throw new Error('password must be between 10 and 1024 characters')
  }
  const derived = await scrypt(password, salt, KEY_LENGTH, SCRYPT_OPTIONS)
  return `scrypt-v1$${Buffer.from(salt).toString('base64url')}$${Buffer.from(derived).toString('base64url')}`
}

export async function verifyPassword(password, encoded) {
  if (typeof password !== 'string' || typeof encoded !== 'string') return false
  const parts = encoded.split('$')
  if (parts.length !== 3 || parts[0] !== 'scrypt-v1') return false
  try {
    const salt = Buffer.from(parts[1], 'base64url')
    const expected = Buffer.from(parts[2], 'base64url')
    if (salt.length !== 16 || expected.length !== KEY_LENGTH) return false
    const actual = await scrypt(password, salt, KEY_LENGTH, SCRYPT_OPTIONS)
    return timingSafeEqual(expected, actual)
  } catch {
    return false
  }
}

export function tokenDigest(token) {
  return createHash('sha256').update(token).digest('base64url')
}

export class SessionStore {
  constructor({ ttlMs = 8 * 60 * 60 * 1000, now = () => Date.now() } = {}) {
    this.ttlMs = ttlMs
    this.now = now
    this.sessions = new Map()
  }

  create(username) {
    this.prune()
    const token = randomBytes(32).toString('base64url')
    const session = {
      username,
      csrfToken: randomBytes(24).toString('base64url'),
      expiresAt: this.now() + this.ttlMs,
    }
    this.sessions.set(tokenDigest(token), session)
    return { token, session: { ...session } }
  }

  get(token) {
    if (!token) return null
    const key = tokenDigest(token)
    const session = this.sessions.get(key)
    if (!session) return null
    if (session.expiresAt <= this.now()) {
      this.sessions.delete(key)
      return null
    }
    session.expiresAt = this.now() + this.ttlMs
    return { ...session }
  }

  delete(token) {
    if (token) this.sessions.delete(tokenDigest(token))
  }

  prune() {
    const now = this.now()
    for (const [key, session] of this.sessions) {
      if (session.expiresAt <= now) this.sessions.delete(key)
    }
  }
}
