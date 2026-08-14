import { once } from 'node:events'
import { afterEach, expect, test } from 'vitest'
import { joinRoom, send, startRelayHarness, type RelayHarness } from './relay-integration-harness'

let harness: RelayHarness | undefined

afterEach(async () => {
  await harness?.close()
  harness = undefined
})

async function waitFor(predicate: () => boolean, timeoutMs = 2000): Promise<void> {
  const start = Date.now()
  while (predicate() === false) {
    if (Date.now() - start > timeoutMs) {
      throw new Error('condition not met in time')
    }
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
}

// (g) Room GC: a room exists while members are present and is garbage-collected
// when the last member leaves (explicit leave and abrupt disconnect both count).
test('garbage-collects a room when its last member leaves', async () => {
  harness = await startRelayHarness()
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  expect(harness.server.registry.roomCount).toBe(1)
  expect(harness.server.registry.hasRoom('s1', 'stream-1')).toBe(true)

  // One explicit leave: room survives (viewer still present).
  const driverClosed = once(driver, 'close')
  send(driver, { type: 'leave' })
  await driverClosed
  await waitFor(() => harness!.server.registry.roomCount === 1)
  expect(harness.server.registry.hasRoom('s1', 'stream-1')).toBe(true)

  // Last member disconnects abruptly: room is GC'd.
  viewer.close()
  await waitFor(() => harness!.server.registry.roomCount === 0)
  expect(harness.server.registry.hasRoom('s1', 'stream-1')).toBe(false)
})
