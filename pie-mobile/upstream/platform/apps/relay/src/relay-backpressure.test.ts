import { afterEach, expect, test } from 'vitest'
import { DEFAULT_RELAY_LIMITS } from './relay-runtime-deps'
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

// (e, end-to-end) Under a bulk PTY-output flood, a control frame still reaches the
// consumer — control is not starved by output. The per-lane priority guarantee
// itself is proven deterministically in consumer-send-queue.test.ts.
test('a control frame reaches the consumer amid an output flood', async () => {
  harness = await startRelayHarness({
    // Generous byte/frame budget so the flood is not rate-limited; we are
    // exercising queue priority, not rate limiting.
    limits: {
      ...DEFAULT_RELAY_LIMITS,
      maxFramesPerSecond: 100_000,
      maxBytesPerSecond: 1_000_000_000
    }
  })
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  const controlReceived = nextMessage(
    viewer,
    (message) => message.type === 'frame' && message.dir === 'control',
    4000
  )

  const bulk = Buffer.alloc(2048, 9).toString('base64')
  for (let seq = 0; seq < 500; seq += 1) {
    send(driver, { type: 'frame', streamId: 'stream-1', seq, dir: 'output', payload: bulk })
  }
  send(driver, {
    type: 'frame',
    streamId: 'stream-1',
    seq: 500,
    dir: 'control',
    payload: Buffer.from('driver-input').toString('base64')
  })

  const control = await controlReceived
  if (control.type !== 'frame') {
    throw new Error('expected frame')
  }
  expect(control.dir).toBe('control')
  expect(control.sender.role).toBe('driver')
})
