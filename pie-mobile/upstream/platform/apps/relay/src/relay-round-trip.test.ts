import { randomBytes } from 'node:crypto'
import { afterEach, expect, test } from 'vitest'
import {
  joinRoom,
  nextMessage,
  send,
  startRelayHarness,
  type RelayHarness
} from './relay-integration-harness'

let harness: RelayHarness | undefined

afterEach(async () => {
  await harness?.close()
  harness = undefined
})

// (a) Opaque ferry round-trip identity: the exact bytes the driver sends are the
// exact bytes the viewer receives. The relay reads only streamId/seq/dir and
// forwards `payload` verbatim — it never decodes/parses/mutates the payload.
test('forwards opaque payload bytes verbatim to the viewer', async () => {
  harness = await startRelayHarness()
  const driver = await harness.connect()
  const viewer = await harness.connect()

  const driverAck = await joinRoom(driver, {
    sessionId: 's1',
    streamId: 'stream-1',
    credential: 'driver-cred'
  })
  const viewerAck = await joinRoom(viewer, {
    sessionId: 's1',
    streamId: 'stream-1',
    credential: 'viewer-cred'
  })
  expect(driverAck.type).toBe('join_ack')
  expect(viewerAck.type).toBe('join_ack')

  // Random bytes that would be corrupted by any text/parse round-trip.
  const original = randomBytes(4096)
  const payloadBase64 = original.toString('base64')

  const received = nextMessage(viewer, (message) => message.type === 'frame')
  send(driver, {
    type: 'frame',
    streamId: 'stream-1',
    seq: 1,
    dir: 'output',
    payload: payloadBase64
  })

  const frame = await received
  if (frame.type !== 'frame') {
    throw new Error('expected a frame')
  }
  expect(frame.payload).toBe(payloadBase64)
  const roundTripped = Buffer.from(frame.payload, 'base64')
  expect(roundTripped.equals(original)).toBe(true)
})

// (f, part) Sender-field enforcement: a client cannot spoof the sender. The relay
// stamps the authenticated participantId/role, ignoring any client-supplied
// sender field on the inbound frame.
test('stamps the authenticated sender and ignores client-supplied sender', async () => {
  harness = await startRelayHarness({
    decide: (_s, _st, credential) => ({
      ok: true,
      participantId: credential === 'driver-cred' ? 'real-driver' : 'real-viewer',
      role: credential === 'driver-cred' ? 'driver' : 'viewer'
    })
  })
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  const received = nextMessage(viewer, (message) => message.type === 'frame')
  // Attempt to spoof the sender via an extra field on the wire.
  driver.send(
    JSON.stringify({
      type: 'frame',
      streamId: 'stream-1',
      seq: 1,
      dir: 'output',
      payload: Buffer.from('hi').toString('base64'),
      sender: { participantId: 'attacker', role: 'driver' }
    })
  )

  const frame = await received
  if (frame.type !== 'frame') {
    throw new Error('expected a frame')
  }
  expect(frame.sender).toEqual({ participantId: 'real-driver', role: 'driver' })
})

// A viewer joining a DIFFERENT stream in the same session must not receive the
// first stream's frames — routing is keyed by (sessionId, streamId).
test('does not cross-deliver between different streams', async () => {
  harness = await startRelayHarness()
  const driver = await harness.connect()
  const otherStreamViewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-A', credential: 'driver-cred' })
  await joinRoom(otherStreamViewer, {
    sessionId: 's1',
    streamId: 'stream-B',
    credential: 'viewer-cred'
  })

  let leaked = false
  otherStreamViewer.on('message', (data) => {
    if ((JSON.parse(String(data)) as { type: string }).type === 'frame') {
      leaked = true
    }
  })
  send(driver, {
    type: 'frame',
    streamId: 'stream-A',
    seq: 1,
    dir: 'output',
    payload: Buffer.from('a').toString('base64')
  })
  await new Promise((resolve) => setTimeout(resolve, 100))
  expect(leaked).toBe(false)
})
