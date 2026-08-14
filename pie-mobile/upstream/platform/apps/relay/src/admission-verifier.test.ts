import { describe, expect, test } from 'vitest'
import {
  createControlPlaneAdmissionVerifier,
  mapCapabilityToRole,
  type AdmissionFetch
} from './admission-verifier'

const CREDENTIAL = JSON.stringify({
  organizationId: '3f1c2a4e-5b6d-4e8f-9a0b-1c2d3e4f5a6b',
  nonce: 'single-use-secret'
})

// A fetch stub that records the call and returns a scripted response. Keeps the verifier pure and
// offline — no running control plane.
function stubFetch(
  respond: () => { ok: boolean; status: number; body: unknown } | Promise<never>
): { fetchImpl: AdmissionFetch; calls: Array<{ url: string; body: string; auth?: string }> } {
  const calls: Array<{ url: string; body: string; auth?: string }> = []
  const fetchImpl: AdmissionFetch = async (url, init) => {
    calls.push({ url, body: init.body, auth: init.headers.authorization })
    const result = await respond()
    return { ok: result.ok, status: result.status, json: async () => result.body }
  }
  return { fetchImpl, calls }
}

function makeVerifier(fetchImpl: AdmissionFetch) {
  return createControlPlaneAdmissionVerifier({
    controlPlaneBaseUrl: 'http://control-plane.internal',
    operatorToken: 'operator-secret',
    fetchImpl
  })
}

describe('mapCapabilityToRole', () => {
  test('view is a viewer; every control kind is a driver', () => {
    expect(mapCapabilityToRole('view')).toBe('viewer')
    expect(mapCapabilityToRole('terminal_control')).toBe('driver')
    expect(mapCapabilityToRole('desktop_control')).toBe('driver')
    expect(mapCapabilityToRole('file_transfer')).toBe('driver')
  })
})

describe('createControlPlaneAdmissionVerifier', () => {
  test('200 with view → viewer, and redeems at the operator-gated endpoint with the stream audience', async () => {
    const { fetchImpl, calls } = stubFetch(() => ({
      ok: true,
      status: 200,
      body: { participantId: 'p-1', capability: 'view' }
    }))
    const decision = await makeVerifier(fetchImpl).verify({
      sessionId: 's-1',
      streamId: 'stream-1',
      credential: CREDENTIAL
    })
    expect(decision).toEqual({ ok: true, participantId: 'p-1', role: 'viewer' })
    expect(calls).toHaveLength(1)
    expect(calls[0]!.url).toBe(
      'http://control-plane.internal/internal/remote-sessions/s-1/relay-admit'
    )
    expect(calls[0]!.auth).toBe('Bearer operator-secret')
    const sent = JSON.parse(calls[0]!.body) as Record<string, unknown>
    expect(sent).toEqual({
      nonce: 'single-use-secret',
      audience: 'stream-1',
      organizationId: '3f1c2a4e-5b6d-4e8f-9a0b-1c2d3e4f5a6b'
    })
  })

  test('200 with terminal_control → driver', async () => {
    const { fetchImpl } = stubFetch(() => ({
      ok: true,
      status: 200,
      body: { participantId: 'p-2', capability: 'terminal_control' }
    }))
    const decision = await makeVerifier(fetchImpl).verify({
      sessionId: 's-1',
      streamId: 'stream-1',
      credential: CREDENTIAL
    })
    expect(decision).toEqual({ ok: true, participantId: 'p-2', role: 'driver' })
  })

  test('409 (consumed/revoked/audience) → fail closed {ok:false}', async () => {
    const { fetchImpl } = stubFetch(() => ({ ok: false, status: 409, body: {} }))
    const decision = await makeVerifier(fetchImpl).verify({
      sessionId: 's-1',
      streamId: 'stream-1',
      credential: CREDENTIAL
    })
    expect(decision.ok).toBe(false)
  })

  test('410 (expired) → fail closed {ok:false}', async () => {
    const { fetchImpl } = stubFetch(() => ({ ok: false, status: 410, body: {} }))
    const decision = await makeVerifier(fetchImpl).verify({
      sessionId: 's-1',
      streamId: 'stream-1',
      credential: CREDENTIAL
    })
    expect(decision.ok).toBe(false)
  })

  test('a network throw → fail closed {ok:false}, never surfaces the error', async () => {
    const fetchImpl: AdmissionFetch = async () => {
      throw new Error('connection refused')
    }
    const decision = await makeVerifier(fetchImpl).verify({
      sessionId: 's-1',
      streamId: 'stream-1',
      credential: CREDENTIAL
    })
    expect(decision).toEqual({ ok: false, reason: 'admission_unavailable' })
  })

  test('an unparseable credential → fail closed without any control-plane call', async () => {
    const { fetchImpl, calls } = stubFetch(() => ({ ok: true, status: 200, body: {} }))
    const decision = await makeVerifier(fetchImpl).verify({
      sessionId: 's-1',
      streamId: 'stream-1',
      credential: 'not-json'
    })
    expect(decision.ok).toBe(false)
    expect(calls).toHaveLength(0)
  })

  test('a 200 with a malformed grant body → fail closed', async () => {
    const { fetchImpl } = stubFetch(() => ({
      ok: true,
      status: 200,
      body: { participantId: 'p-3', capability: 'not-a-capability' }
    }))
    const decision = await makeVerifier(fetchImpl).verify({
      sessionId: 's-1',
      streamId: 'stream-1',
      credential: CREDENTIAL
    })
    expect(decision.ok).toBe(false)
  })
})
