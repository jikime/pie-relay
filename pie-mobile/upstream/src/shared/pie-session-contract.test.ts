import fs from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  PieSessionChangedSchema,
  PieSessionGetRequestSchema,
  PieSessionGetResponseSchema,
  PieSessionStateSchema
} from './pie-session-contract'

function readFixture(relativePath: string): unknown {
  const fixturePath = path.resolve(process.cwd(), 'contracts', 'fixtures', relativePath)
  return JSON.parse(fs.readFileSync(fixturePath, 'utf8'))
}

describe('Pie session contract', () => {
  it('accepts the R0 request, response, and compatibility fixtures', () => {
    expect(
      PieSessionGetRequestSchema.safeParse(readFixture('valid/ipc-session-get-request.json'))
        .success
    ).toBe(true)
    expect(
      PieSessionGetResponseSchema.safeParse(readFixture('valid/ipc-session-get-response.json'))
        .success
    ).toBe(true)
    expect(
      PieSessionGetResponseSchema.safeParse(
        readFixture('compatibility/ipc-session-response-unknown-optional.json')
      ).success
    ).toBe(true)
  })

  it('rejects unknown request fields and token-bearing responses', () => {
    expect(
      PieSessionGetRequestSchema.safeParse(
        readFixture('invalid/ipc-session-request-unknown-field.json')
      ).success
    ).toBe(false)
    expect(
      PieSessionGetResponseSchema.safeParse(
        readFixture('invalid/ipc-session-response-token-leak.json')
      ).success
    ).toBe(false)
  })

  it('accepts multi-segment permissions without exposing token fields', () => {
    const session = {
      status: 'signed_in',
      instanceId: 'local-desktop',
      userId: '10000000-0000-4000-8000-000000000001',
      displayName: 'Pie User',
      organizationId: '10000000-0000-4000-8000-000000000002',
      permissions: ['project.read', 'mcp.project.read'],
      expiresAt: '2026-07-16T01:00:00.000Z'
    }
    expect(PieSessionStateSchema.safeParse(session).success).toBe(true)
    expect(PieSessionStateSchema.safeParse({ ...session, idToken: 'secret' }).success).toBe(false)
  })

  it('rejects malformed session change events', () => {
    expect(
      PieSessionChangedSchema.safeParse({
        type: 'session.changed',
        protocolVersion: '2.0',
        sequence: 0,
        session: { status: 'signed_out', instanceId: 'local-desktop' }
      }).success
    ).toBe(false)
  })
})
