import type { RelayClock } from './relay-runtime-deps'

// Per-connection token bucket. Refills continuously from an injected clock so
// rate-limit tests are deterministic (advance the clock, not a real timer).
class TokenBucket {
  private tokens: number
  private lastRefillMs: number

  constructor(
    private readonly capacity: number,
    private readonly refillPerSecond: number,
    now: number
  ) {
    this.tokens = capacity
    this.lastRefillMs = now
  }

  // Refill up to `now`, then report whether `amount` is currently available. Does
  // not spend — commit() does, so a multi-budget check can spend atomically.
  available(now: number, amount: number): boolean {
    this.refill(now)
    return this.tokens >= amount
  }

  commit(amount: number): void {
    this.tokens -= amount
  }

  private refill(now: number): void {
    const elapsedMs = now - this.lastRefillMs
    if (elapsedMs <= 0) {
      return
    }
    this.tokens = Math.min(this.capacity, this.tokens + (elapsedMs / 1000) * this.refillPerSecond)
    this.lastRefillMs = now
  }
}

// A frame must fit BOTH the frames/sec and bytes/sec budgets. Over-rate frames
// are dropped (caller errors the sender) without disconnecting — a burst degrades
// to backpressure, never a crash. Both budgets are checked before either is spent
// so a reject never partially drains one bucket.
export class FrameRateLimiter {
  private readonly frames: TokenBucket
  private readonly bytes: TokenBucket

  constructor(
    private readonly clock: RelayClock,
    maxFramesPerSecond: number,
    maxBytesPerSecond: number
  ) {
    const now = clock.now()
    this.frames = new TokenBucket(maxFramesPerSecond, maxFramesPerSecond, now)
    this.bytes = new TokenBucket(maxBytesPerSecond, maxBytesPerSecond, now)
  }

  allow(frameBytes: number): boolean {
    const now = this.clock.now()
    if (
      this.frames.available(now, 1) === false ||
      this.bytes.available(now, frameBytes) === false
    ) {
      return false
    }
    this.frames.commit(1)
    this.bytes.commit(frameBytes)
    return true
  }
}
