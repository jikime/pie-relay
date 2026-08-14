import { afterEach, expect, test } from 'vitest'
import { DEFAULT_RELAY_LIMITS } from './relay-runtime-deps'
import {
  joinRoom,
  nextMessage,
  send,
  startRelayHarness,
  type RelayHarness
} from './relay-integration-harness'
import type { RelayOutboundMessage } from './relay-wire-contract'

let harness: RelayHarness | undefined

afterEach(async () => {
  await harness?.close()
  harness = undefined
})

function collect(ws: import('ws').WebSocket, ms: number): Promise<RelayOutboundMessage[]> {
  const messages: RelayOutboundMessage[] = []
  ws.on('message', (data) => messages.push(JSON.parse(String(data)) as RelayOutboundMessage))
  return new Promise((resolve) => setTimeout(() => resolve(messages), ms))
}

// (c) An oversize frame (> maxFrameBytes) is rejected with an error and dropped;
// the connection and room survive and can still ferry a valid frame afterward.
test('rejects an oversize frame without crashing the connection', async () => {
  harness = await startRelayHarness({ limits: { ...DEFAULT_RELAY_LIMITS, maxFrameBytes: 1024 } })
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  const oversizeError = nextMessage(driver, (message) => message.type === 'error')
  send(driver, {
    type: 'frame',
    streamId: 'stream-1',
    seq: 1,
    dir: 'output',
    payload: Buffer.alloc(4096, 7).toString('base64') // 4096 decoded bytes > 1024
  })
  const error = await oversizeError
  if (error.type !== 'error') {
    throw new Error('expected error')
  }
  expect(error.code).toBe('frame_too_large')

  // Connection still works: a valid small frame is delivered.
  const delivered = nextMessage(viewer, (message) => message.type === 'frame')
  send(driver, {
    type: 'frame',
    streamId: 'stream-1',
    seq: 2,
    dir: 'output',
    payload: Buffer.from('ok').toString('base64')
  })
  const frame = await delivered
  expect(frame.type).toBe('frame')
  expect(driver.readyState).toBe(driver.OPEN)
})

// (d) A burst beyond the per-connection token bucket: the excess is dropped with
// rate_limited errors, the delivered count is capped at the bucket capacity, and
// the connection survives. A fixed injected clock prevents any refill mid-burst.
test('rate-limits a burst and survives', async () => {
  const fixedNow = 1_000_000
  harness = await startRelayHarness({
    clock: { now: () => fixedNow },
    limits: { ...DEFAULT_RELAY_LIMITS, maxFramesPerSecond: 5, maxBytesPerSecond: 100_000_000 }
  })
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  const driverMessages = collect(driver, 250)
  const viewerMessages = collect(viewer, 250)
  for (let seq = 0; seq < 12; seq += 1) {
    send(driver, {
      type: 'frame',
      streamId: 'stream-1',
      seq,
      dir: 'output',
      payload: Buffer.from(`f${seq}`).toString('base64')
    })
  }

  const [fromDriver, fromViewer] = await Promise.all([driverMessages, viewerMessages])
  const delivered = fromViewer.filter((m) => m.type === 'frame')
  const rateLimited = fromDriver.filter((m) => m.type === 'error' && m.code === 'rate_limited')
  // Bucket capacity == maxFramesPerSecond == 5; the remaining 7 are dropped.
  expect(delivered.length).toBe(5)
  expect(rateLimited.length).toBe(7)
  expect(driver.readyState).toBe(driver.OPEN)
})
