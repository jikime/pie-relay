import fs from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  evaluateClientCompatibility,
  PieInstanceDiscoverySchema,
  type PieInstanceDiscovery
} from './pie-instance-discovery'

function readFixture(relativePath: string): unknown {
  return JSON.parse(
    fs.readFileSync(path.resolve(process.cwd(), 'contracts', 'fixtures', relativePath), 'utf8')
  )
}

const baseDiscovery = PieInstanceDiscoverySchema.parse(
  readFixture('valid/discovery-self-hosted.json')
)

function discoveryWith(overrides: Partial<PieInstanceDiscovery>): PieInstanceDiscovery {
  return { ...baseDiscovery, ...overrides }
}

describe('Pie instance discovery contract', () => {
  it('accepts the valid + additive-optional fixtures', () => {
    expect(
      PieInstanceDiscoverySchema.safeParse(readFixture('valid/discovery-self-hosted.json')).success
    ).toBe(true)
    expect(
      PieInstanceDiscoverySchema.safeParse(
        readFixture('compatibility/discovery-unknown-optional.json')
      ).success
    ).toBe(true)
  })

  it('rejects a non-loopback LAN http endpoint', () => {
    expect(
      PieInstanceDiscoverySchema.safeParse(readFixture('invalid/discovery-lan-http.json')).success
    ).toBe(false)
  })

  it('rejects a non-OIDC auth protocol', () => {
    expect(
      PieInstanceDiscoverySchema.safeParse(readFixture('invalid/discovery-direct-password.json'))
        .success
    ).toBe(false)
  })
})

describe('client version/capability gating', () => {
  const client = {
    appVersion: '1.0.0',
    supportedApiProtocol: '1.0',
    supportedRealtimeProtocol: '1.0'
  }

  it('needs-update when the app is below the minimum version', () => {
    const discovery = discoveryWith({ minimumClientVersion: '2.0.0' })
    expect(evaluateClientCompatibility(client, discovery).state).toBe('needs-update')
  })

  it('supported when version and protocols match', () => {
    const discovery = discoveryWith({ minimumClientVersion: '0.1.0' })
    expect(evaluateClientCompatibility(client, discovery).state).toBe('supported')
  })

  it('limited when the server protocol minor is ahead', () => {
    const discovery = discoveryWith({
      minimumClientVersion: '0.1.0',
      protocol: { api: '1.1', realtime: '1.0' }
    })
    const result = evaluateClientCompatibility(client, discovery)
    expect(result.state).toBe('limited')
    expect(result.reasons).toContain('api-protocol-minor-ahead')
  })

  it('needs-update when the server protocol major is ahead', () => {
    const discovery = discoveryWith({
      minimumClientVersion: '0.1.0',
      protocol: { api: '2.0', realtime: '1.0' }
    })
    expect(evaluateClientCompatibility(client, discovery).state).toBe('needs-update')
  })
})
