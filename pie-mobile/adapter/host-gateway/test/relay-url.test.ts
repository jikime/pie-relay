import { describe, expect, it } from 'vitest'
import { DEFAULT_RELAY_ORIGIN, resolveRelayUrl } from '../src/relay-url.ts'

describe('resolveRelayUrl', () => {
  it('defaults to the CookAI Relay', () => {
    expect(resolveRelayUrl({})).toBe(DEFAULT_RELAY_ORIGIN)
  })

  it('uses PIE_RELAY_URL for a local Relay', () => {
    expect(resolveRelayUrl({ PIE_RELAY_URL: 'http://127.0.0.1:13412' })).toBe(
      'http://127.0.0.1:13412'
    )
  })

  it('lets the CLI flag override the environment', () => {
    expect(resolveRelayUrl({ PIE_RELAY_URL: 'https://ignored.example' }, 'wss://relay.example/ws/agent')).toBe(
      'wss://relay.example'
    )
  })

  it('rejects unsupported schemes', () => {
    expect(() => resolveRelayUrl({}, 'file:///tmp/relay')).toThrow('http(s) or ws(s)')
  })
})
