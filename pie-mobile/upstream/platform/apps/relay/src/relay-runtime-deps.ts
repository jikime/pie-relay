// Injectable runtime seams so all pure relay logic (rate limits, room routing,
// send queues) is deterministic in tests — no Date.now()/Math.random() in the
// logic paths. The entrypoint (index.ts) supplies the real implementations.

export type RelayRole = 'driver' | 'viewer'

// Monotonic-ish millisecond clock. Tests inject a hand-advanced value; the
// entrypoint uses Date.now().
export type RelayClock = {
  now: () => number
}

// Source of relay-internal connection ids. Tests inject a deterministic counter;
// the entrypoint uses randomUUID.
export type ConnectionIdSource = {
  next: () => string
}

// pino-compatible subset (matches the API/worker loggers) for structured audit
// lines. NEVER pass a token/credential/payload into these fields.
export type RelayLogger = {
  info: (fields: Record<string, unknown>, message?: string) => void
  warn: (fields: Record<string, unknown>, message?: string) => void
}

export const NOOP_RELAY_LOGGER: RelayLogger = {
  info: () => {},
  warn: () => {}
}

// Enforcement thresholds. All are overridable via env in relay-config.ts.
export type RelayLimits = {
  // Hard cap on a single forwarded frame's opaque payload, measured in decoded
  // bytes WITHOUT decoding the content (base64 length math only).
  maxFrameBytes: number
  // Per-connection token buckets: sustained frames/sec and bytes/sec plus burst.
  maxFramesPerSecond: number
  maxBytesPerSecond: number
  // Bounded per-consumer send queue. The bulk (PTY-output) lane is dropped-oldest
  // when full so a slow viewer cannot grow memory without bound; the control lane
  // is kept far larger and is NEVER starved by a PTY-output flood.
  maxQueuedPtyFrames: number
  maxQueuedControlFrames: number
  // Socket buffered-bytes high-water mark above which we stop draining and start
  // queuing (a slow consumer signal).
  sendHighWaterMarkBytes: number
}

export const DEFAULT_RELAY_LIMITS: RelayLimits = {
  maxFrameBytes: 256 * 1024,
  maxFramesPerSecond: 240,
  maxBytesPerSecond: 8 * 1024 * 1024,
  maxQueuedPtyFrames: 512,
  maxQueuedControlFrames: 4096,
  sendHighWaterMarkBytes: 1024 * 1024
}
