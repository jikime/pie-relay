export const DEFAULT_RELAY_ORIGIN =
  'https://relay.cookai.dev'

type RelayEnv = Record<string, string | undefined>

export function resolveRelayUrl(
  env: RelayEnv = process.env,
  explicitUrl?: string
): string {
  const relayUrl = explicitUrl?.trim() || env.PIE_RELAY_URL?.trim() || DEFAULT_RELAY_ORIGIN
  const parsed = new URL(relayUrl)
  if (!['http:', 'https:', 'ws:', 'wss:'].includes(parsed.protocol)) {
    throw new Error('Relay URL must use http(s) or ws(s)')
  }
  parsed.pathname = '/'
  parsed.search = ''
  parsed.hash = ''
  return parsed.origin
}
