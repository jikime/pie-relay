import fs from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  PieRealtimeClientHelloSchema,
  PieRealtimeConnectionClosingSchema,
  PieRealtimeHeartbeatSchema,
  PieRealtimeResourceChangedSchema,
  PieRealtimeResyncRequiredSchema,
  PieRealtimeServerMessageSchema,
  PieRealtimeServerWelcomeSchema,
  PieRealtimeSessionRevokedSchema
} from './pie-realtime-contract'

function readFixture(relativePath: string): unknown {
  const fixturePath = path.resolve(process.cwd(), 'contracts', 'fixtures', relativePath)
  return JSON.parse(fs.readFileSync(fixturePath, 'utf8'))
}

describe('Pie realtime contract', () => {
  it('accepts the R0 valid fixtures', () => {
    expect(
      PieRealtimeClientHelloSchema.safeParse(readFixture('valid/realtime-client-hello.json'))
        .success
    ).toBe(true)
    expect(
      PieRealtimeResourceChangedSchema.safeParse(
        readFixture('valid/realtime-resource-changed.json')
      ).success
    ).toBe(true)
    expect(
      PieRealtimeHeartbeatSchema.safeParse(readFixture('valid/realtime-heartbeat.json')).success
    ).toBe(true)
  })

  it('accepts an additive unknown-optional field (forward compatibility)', () => {
    const parsed = PieRealtimeResourceChangedSchema.safeParse(
      readFixture('compatibility/realtime-resource-unknown-optional.json')
    )
    expect(parsed.success).toBe(true)
  })

  it('rejects an invalid resource version of zero', () => {
    expect(
      PieRealtimeResourceChangedSchema.safeParse(
        readFixture('invalid/realtime-resource-version-zero.json')
      ).success
    ).toBe(false)
  })

  it('discriminates every server message by type', () => {
    const welcome = {
      type: 'server.welcome',
      schemaVersion: 1,
      protocolVersion: '1.0',
      connectionId: '55555555-5555-4555-8555-555555555555',
      cursor: 'cursor-00000000',
      heartbeatIntervalMs: 15000
    }
    const revoked = {
      type: 'session.revoked',
      schemaVersion: 1,
      reason: 'admin_revoke',
      effectiveAt: '2026-07-15T10:45:00Z'
    }
    const resync = {
      type: 'resync.required',
      schemaVersion: 1,
      reason: 'buffer_overflow',
      cursor: null
    }
    const closing = {
      type: 'connection.closing',
      schemaVersion: 1,
      code: 'server_shutdown',
      reason: 'restart',
      reconnect: true
    }
    expect(PieRealtimeServerWelcomeSchema.safeParse(welcome).success).toBe(true)
    expect(PieRealtimeSessionRevokedSchema.safeParse(revoked).success).toBe(true)
    expect(PieRealtimeResyncRequiredSchema.safeParse(resync).success).toBe(true)
    expect(PieRealtimeConnectionClosingSchema.safeParse(closing).success).toBe(true)
    for (const message of [welcome, revoked, resync, closing]) {
      const parsed = PieRealtimeServerMessageSchema.safeParse(message)
      expect(parsed.success).toBe(true)
      if (parsed.success) {
        expect(parsed.data.type).toBe(message.type)
      }
    }
  })

  it('rejects a message with an unknown type', () => {
    expect(
      PieRealtimeServerMessageSchema.safeParse({ type: 'mystery.event', schemaVersion: 1 }).success
    ).toBe(false)
  })
})
