import fs from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  PieRuntimeHandshakeRequestSchema,
  PieRuntimeHandshakeResponseSchema
} from './pie-runtime-handshake-contract'

function readFixture(relativePath: string): unknown {
  const fixturePath = path.resolve(process.cwd(), 'contracts', 'fixtures', relativePath)
  return JSON.parse(fs.readFileSync(fixturePath, 'utf8'))
}

describe('Pie runtime handshake contract', () => {
  it('accepts the R0 request, response, and additive compatibility fixtures', () => {
    expect(
      PieRuntimeHandshakeRequestSchema.safeParse(
        readFixture('valid/runtime-handshake-request.json')
      ).success
    ).toBe(true)
    expect(
      PieRuntimeHandshakeResponseSchema.safeParse(
        readFixture('valid/runtime-handshake-response.json')
      ).success
    ).toBe(true)
    expect(
      PieRuntimeHandshakeResponseSchema.safeParse(
        readFixture('compatibility/runtime-handshake-unknown-optional.json')
      ).success
    ).toBe(true)
  })

  it('rejects short runtime capabilities', () => {
    expect(
      PieRuntimeHandshakeRequestSchema.safeParse(
        readFixture('invalid/runtime-handshake-short-capability.json')
      ).success
    ).toBe(false)
  })
})
