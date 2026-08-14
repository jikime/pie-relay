import type { RelayFrameDirection } from './relay-wire-contract'
import type { RelayLimits } from './relay-runtime-deps'

// Per-consumer bounded send queue with traffic-class priority.
//
// Backpressure + priority policy (security constraint #6 — a PTY-output flood
// must never starve control/audit):
//   * Two lanes: control (high priority) and pty/output (bulk, low priority).
//   * Draining ALWAYS empties the control lane before the pty lane.
//   * The pty lane is bounded (maxQueuedPtyFrames); when full we drop the OLDEST
//     pty frame (a lagged viewer loses stale output, not fresh) and count it.
//   * The control lane is bounded far higher (maxQueuedControlFrames) and is
//     drained first, so a pty flood cannot evict or delay control frames. Only a
//     genuine control-lane overflow drops (oldest), which is not reachable by
//     flooding the bulk lane.
//   * We stop draining once the socket's buffered bytes exceed the high-water
//     mark; the slow consumer's frames queue (and its pty lane drops) WITHOUT
//     blocking the room or other consumers.
// Drops are coalesced: each pump emits at most one stream_lagged per lane
// carrying the count of frames dropped since the previous signal, so the consumer
// learns it fell behind without a per-frame signal storm.

type QueuedFrame = {
  dir: RelayFrameDirection
  serialized: string
  bytes: number
}

export type ConsumerSendQueueDeps = {
  // Sends one already-serialized message to the socket.
  send: (serialized: string) => void
  // Current socket send-buffer depth in bytes (ws.bufferedAmount).
  bufferedAmount: () => number
  // Emitted once per drop run for a lane so the consumer knows it lagged.
  onLagged: (dir: RelayFrameDirection, droppedFrames: number) => void
  limits: RelayLimits
}

export class ConsumerSendQueue {
  private readonly control: QueuedFrame[] = []
  private readonly pty: QueuedFrame[] = []
  private ptyDropRun = 0
  private controlDropRun = 0

  constructor(private readonly deps: ConsumerSendQueueDeps) {}

  enqueue(dir: RelayFrameDirection, serialized: string, bytes: number): void {
    const lane = dir === 'control' ? this.control : this.pty
    const bound =
      dir === 'control'
        ? this.deps.limits.maxQueuedControlFrames
        : this.deps.limits.maxQueuedPtyFrames
    lane.push({ dir, serialized, bytes })
    while (lane.length > bound) {
      lane.shift() // drop OLDEST — a lagged consumer keeps the freshest frames.
      if (dir === 'control') {
        this.controlDropRun += 1
      } else {
        this.ptyDropRun += 1
      }
    }
    this.pump()
  }

  // Drain by priority while the socket can accept more. Public so a test (or a
  // socket 'drain' event) can resume a previously slow consumer deterministically.
  pump(): void {
    while (this.deps.bufferedAmount() < this.deps.limits.sendHighWaterMarkBytes) {
      const next = this.control.shift() ?? this.pty.shift()
      if (next === undefined) {
        break
      }
      this.deps.send(next.serialized)
    }
    this.flushLagSignals()
  }

  private flushLagSignals(): void {
    if (this.controlDropRun > 0) {
      const dropped = this.controlDropRun
      this.controlDropRun = 0
      this.deps.onLagged('control', dropped)
    }
    if (this.ptyDropRun > 0) {
      const dropped = this.ptyDropRun
      this.ptyDropRun = 0
      this.deps.onLagged('output', dropped)
    }
  }
}
