import { expect, test } from 'vitest'
import { ConsumerSendQueue } from './consumer-send-queue'
import { DEFAULT_RELAY_LIMITS, type RelayLimits } from './relay-runtime-deps'
import type { RelayFrameDirection } from './relay-wire-contract'

function makeQueue(limits: RelayLimits) {
  const sent: string[] = []
  const lagged: Array<{ dir: RelayFrameDirection; dropped: number }> = []
  let buffered = 0
  const queue = new ConsumerSendQueue({
    send: (serialized) => sent.push(serialized),
    bufferedAmount: () => buffered,
    onLagged: (dir, droppedFrames) => lagged.push({ dir, dropped: droppedFrames }),
    limits
  })
  return { queue, sent, lagged, setBuffered: (value: number) => (buffered = value) }
}

// (e, priority) The control lane is always drained before the bulk/PTY lane, so a
// backlog of output frames cannot delay a queued control frame.
test('drains the control lane before the pty lane', () => {
  const { queue, sent, setBuffered } = makeQueue(DEFAULT_RELAY_LIMITS)
  // Block the socket so frames queue instead of sending immediately.
  setBuffered(DEFAULT_RELAY_LIMITS.sendHighWaterMarkBytes + 1)
  queue.enqueue('output', 'PTY0', 4)
  queue.enqueue('output', 'PTY1', 4)
  queue.enqueue('output', 'PTY2', 4)
  queue.enqueue('control', 'CTRL0', 4)
  expect(sent).toEqual([]) // nothing drained while blocked

  // Unblock and drain: control comes out first despite being enqueued last.
  setBuffered(0)
  queue.pump()
  expect(sent[0]).toBe('CTRL0')
  expect(sent).toEqual(['CTRL0', 'PTY0', 'PTY1', 'PTY2'])
})

// (e, drop+lag) A slow consumer's bounded PTY lane drops OLDEST frames when full
// and signals stream_lagged; the control lane is untouched by a PTY flood.
test('drops oldest pty frames and lags a slow consumer without touching control', () => {
  const limits: RelayLimits = { ...DEFAULT_RELAY_LIMITS, maxQueuedPtyFrames: 2 }
  const { queue, sent, lagged, setBuffered } = makeQueue(limits)
  setBuffered(limits.sendHighWaterMarkBytes + 1) // permanently slow consumer

  queue.enqueue('control', 'CTRL0', 4)
  for (let i = 0; i < 5; i += 1) {
    queue.enqueue('output', `PTY${i}`, 4)
  }
  // 5 pty frames into a lane bounded at 2 -> 3 oldest dropped, coalesced count.
  const totalDropped = lagged
    .filter((l) => l.dir === 'output')
    .reduce((sum, l) => sum + l.dropped, 0)
  expect(totalDropped).toBe(3)
  expect(lagged.some((l) => l.dir === 'control')).toBe(false) // control never dropped

  // Unblock: the surviving 2 newest pty frames plus the untouched control drain,
  // control first.
  setBuffered(0)
  queue.pump()
  expect(sent[0]).toBe('CTRL0')
  expect(sent.filter((s) => s.startsWith('PTY'))).toEqual(['PTY3', 'PTY4'])
})
