import { once } from 'node:events'
import { afterEach, expect, test } from 'vitest'
import { joinRoom, startRelayHarness, type RelayHarness } from './relay-integration-harness'

let harness: RelayHarness | undefined

afterEach(async () => {
  await harness?.close()
  harness = undefined
})

// (f) Admission is injectable: a connection the AdmissionVerifier denies is
// rejected BEFORE joining any room (error + socket close), and no room is created.
test('rejects a connection the admission verifier denies', async () => {
  harness = await startRelayHarness({ decide: () => ({ ok: false, reason: 'no capability' }) })
  const client = await harness.connect()
  const closed = once(client, 'close')

  const result = await joinRoom(client, {
    sessionId: 's1',
    streamId: 'stream-1',
    credential: 'whatever'
  })
  expect(result.type).toBe('error')
  if (result.type === 'error') {
    expect(result.code).toBe('admission_denied')
  }
  const [code] = (await closed) as [number]
  expect(code).toBe(4403)
  // No room state was created for a denied connection.
  expect(harness.server.registry.hasRoom('s1', 'stream-1')).toBe(false)
})

// The role assigned by admission is authoritative and echoed in the join_ack; the
// relay enforces control ownership against exactly this role.
test('assigns the admission-provided role in the join ack', async () => {
  harness = await startRelayHarness({
    decide: () => ({ ok: true, participantId: 'p-driver', role: 'driver' })
  })
  const client = await harness.connect()
  const ack = await joinRoom(client, { sessionId: 's1', streamId: 'stream-1', credential: 'x' })
  expect(ack.type).toBe('join_ack')
  if (ack.type === 'join_ack') {
    expect(ack.role).toBe('driver')
    expect(ack.participantId).toBe('p-driver')
  }
})
