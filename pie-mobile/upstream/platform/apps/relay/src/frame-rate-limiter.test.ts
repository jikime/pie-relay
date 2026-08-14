import { expect, test } from 'vitest'
import { FrameRateLimiter } from './frame-rate-limiter'

// A mutable injected clock makes refill behavior fully deterministic.
function fakeClock(start = 0) {
  let now = start
  return { clock: { now: () => now }, advance: (ms: number) => (now += ms) }
}

test('allows up to the frame budget then drops the excess', () => {
  const { clock } = fakeClock()
  const limiter = new FrameRateLimiter(clock, 5, 1_000_000)
  const results = Array.from({ length: 8 }, () => limiter.allow(10))
  expect(results.filter(Boolean).length).toBe(5) // capacity == 5
  expect(results.slice(5).every((r) => r === false)).toBe(true)
})

test('refills over time so a later frame is allowed again', () => {
  const { clock, advance } = fakeClock()
  const limiter = new FrameRateLimiter(clock, 5, 1_000_000)
  for (let i = 0; i < 5; i += 1) {
    limiter.allow(10)
  }
  expect(limiter.allow(10)).toBe(false)
  // 1 second refills the whole 5-token bucket.
  advance(1000)
  expect(limiter.allow(10)).toBe(true)
})

test('enforces the byte budget independently of the frame budget', () => {
  const { clock } = fakeClock()
  // Plenty of frame headroom, tiny byte budget.
  const limiter = new FrameRateLimiter(clock, 1000, 100)
  expect(limiter.allow(80)).toBe(true)
  expect(limiter.allow(80)).toBe(false) // only 20 bytes left this second
})

test('a rejected frame does not spend the frame budget', () => {
  const { clock } = fakeClock()
  const limiter = new FrameRateLimiter(clock, 2, 50)
  // Byte budget blocks this frame; the frame token must not be consumed.
  expect(limiter.allow(100)).toBe(false)
  // Both small frames still fit the frame budget of 2.
  expect(limiter.allow(10)).toBe(true)
  expect(limiter.allow(10)).toBe(true)
})
