import { randomBytes } from 'node:crypto'

export const DEFAULT_KROOT_SERVER_URL = 'grpcs://adk-server.kroot.io'
export const DEFAULT_KROOT_RELAY_URL = 'wss://adk-relay.kroot.io/ws/agent'
export const ZERO_TIME = '0001-01-01T00:00:00.000Z'

// This is the JSON representation expected by krootauth.Credential. The PAT
// stays in the external service and Manager-to-Executor provisioning path; it
// is never used as a Pie Relay token and is never returned to the browser.
export function createKrootCredential({
  pat,
  serverURL = DEFAULT_KROOT_SERVER_URL,
  relayURL = DEFAULT_KROOT_RELAY_URL,
  deviceID = randomBytes(16).toString('hex'),
  updatedAt = new Date(),
} = {}) {
  const accessToken = nonempty(pat, 'pat')
  const normalizedDeviceID = nonempty(deviceID, 'deviceID')
  if (!/^(?:[a-f0-9]{32}|dev-[a-zA-Z0-9._-]+)$/.test(normalizedDeviceID)) {
    throw new Error('deviceID must be a 16-byte lowercase hex value or a dev-* identifier')
  }
  const timestamp = updatedAt instanceof Date ? updatedAt : new Date(updatedAt)
  if (Number.isNaN(timestamp.getTime())) throw new Error('updatedAt must be a valid date')
  return {
    serverUrl: endpoint(serverURL, 'serverURL', ['grpc:', 'grpcs:']),
    accessToken,
    expiresAt: ZERO_TIME,
    authKind: 'pat',
    updatedAt: timestamp.toISOString(),
    relayUrl: endpoint(relayURL, 'relayURL', ['ws:', 'wss:']),
    deviceId: normalizedDeviceID,
  }
}

export function isKrootPATCredential(value) {
  return Boolean(
    value && typeof value === 'object' && !Array.isArray(value)
    && typeof value.accessToken === 'string' && value.accessToken.trim()
    && value.authKind === 'pat'
    && typeof value.serverUrl === 'string' && value.serverUrl.trim()
    && typeof value.relayUrl === 'string' && value.relayUrl.trim()
    && typeof value.deviceId === 'string' && value.deviceId.trim(),
  )
}

function endpoint(value, name, protocols) {
  const parsed = new URL(nonempty(value, name))
  if (!protocols.includes(parsed.protocol) || parsed.username || parsed.password || parsed.hash) {
    throw new Error(`${name} must use ${protocols.join(' or ')} without embedded credentials or a fragment`)
  }
  return parsed.toString()
}

function nonempty(value, name) {
  const trimmed = typeof value === 'string' ? value.trim() : ''
  if (!trimmed) throw new Error(`${name} is required`)
  return trimmed
}
