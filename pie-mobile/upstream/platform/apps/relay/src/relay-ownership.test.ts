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

const roleByCredential = (_s: string, _st: string, credential: string) => ({
  ok: true as const,
  participantId: credential,
  role: credential === 'driver-cred' ? ('driver' as const) : ('viewer' as const)
})

// (b) Stream ownership: a viewer's control-direction frame is rejected (error to
// sender, NOT delivered); the driver's control frame IS delivered. Single-driver
// model — the relay trusts the role assigned at admission.
test('rejects a viewer control frame and delivers a driver control frame', async () => {
  harness = await startRelayHarness({ decide: roleByCredential })
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  // Viewer attempts a control frame -> error back to the viewer, dropped.
  const viewerError = nextMessage(viewer, (message) => message.type === 'error')
  // The driver must NOT receive the viewer's rejected control frame.
  let driverGotViewerFrame = false
  driver.on('message', (data) => {
    const message = JSON.parse(String(data)) as { type: string }
    if (message.type === 'frame') {
      driverGotViewerFrame = true
    }
  })
  send(viewer, {
    type: 'frame',
    streamId: 'stream-1',
    seq: 1,
    dir: 'control',
    payload: Buffer.from('keystroke').toString('base64')
  })
  const error = await viewerError
  if (error.type !== 'error') {
    throw new Error('expected error')
  }
  expect(error.code).toBe('forbidden_control')

  // Driver's control frame IS delivered to the other room member (the viewer).
  const delivered = nextMessage(viewer, (message) => message.type === 'frame')
  send(driver, {
    type: 'frame',
    streamId: 'stream-1',
    seq: 2,
    dir: 'control',
    payload: Buffer.from('driver-input').toString('base64')
  })
  const frame = await delivered
  if (frame.type !== 'frame') {
    throw new Error('expected frame')
  }
  expect(frame.dir).toBe('control')
  expect(frame.sender.role).toBe('driver')
  // The viewer's earlier control frame never reached the driver.
  expect(driverGotViewerFrame).toBe(false)
})
