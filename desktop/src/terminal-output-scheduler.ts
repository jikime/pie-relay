export interface TerminalWriter {
  write(data: Uint8Array, callback?: () => void): void;
}

interface SchedulerOptions {
  maxForegroundBytes?: number;
  maxBackgroundBytes?: number;
  chunkBytes?: number;
  stallMs?: number;
  onOverflow?: () => void;
  onStall?: () => void;
}

// Feeds xterm in bounded chunks and waits for its parser callback before the
// next write. This prevents a fast relay stream from monopolizing the renderer
// or building an unbounded xterm parse backlog. Background rooms get a smaller
// retained budget because a fresh snapshot is cheaper than hidden memory.
export class TerminalOutputScheduler {
  private readonly terminal: TerminalWriter;
  private readonly maxForegroundBytes: number;
  private readonly maxBackgroundBytes: number;
  private readonly chunkBytes: number;
  private readonly stallMs: number;
  private readonly onOverflow?: () => void;
  private readonly onStall?: () => void;
  private queue: Uint8Array[] = [];
  private queuedBytes = 0;
  private writing = false;
  private disposed = false;
  private generation = 0;
  private stallTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(terminal: TerminalWriter, options: SchedulerOptions = {}) {
    this.terminal = terminal;
    this.maxForegroundBytes = options.maxForegroundBytes ?? 4 * 1024 * 1024;
    this.maxBackgroundBytes = options.maxBackgroundBytes ?? 1024 * 1024;
    this.chunkBytes = options.chunkBytes ?? 16 * 1024;
    this.stallMs = options.stallMs ?? 5000;
    this.onOverflow = options.onOverflow;
    this.onStall = options.onStall;
  }

  enqueue(data: Uint8Array, background = false): boolean {
    if (this.disposed || data.byteLength === 0) return !this.disposed;
    const limit = background ? this.maxBackgroundBytes : this.maxForegroundBytes;
    if (this.queuedBytes + data.byteLength > limit) {
      this.recover();
      this.onOverflow?.();
      return false;
    }
    // base64ToBytes creates a fresh buffer, but copy here keeps this class safe
    // for callers passing a mutable view.
    const copy = new Uint8Array(data);
    this.queue.push(copy);
    this.queuedBytes += copy.byteLength;
    this.pump();
    return true;
  }

  recover(): void {
    this.generation += 1;
    this.queue = [];
    this.queuedBytes = 0;
    this.writing = false;
    if (this.stallTimer) clearTimeout(this.stallTimer);
    this.stallTimer = null;
  }

  dispose(): void {
    this.disposed = true;
    this.recover();
  }

  private pump(): void {
    if (this.disposed || this.writing || this.queue.length === 0) return;
    const head = this.queue[0];
    const chunk = head.byteLength <= this.chunkBytes ? head : head.subarray(0, this.chunkBytes);
    if (chunk.byteLength === head.byteLength) this.queue.shift();
    else this.queue[0] = head.subarray(chunk.byteLength);
    this.queuedBytes -= chunk.byteLength;
    this.writing = true;
    const generation = this.generation;
    this.stallTimer = setTimeout(() => {
      if (this.disposed || generation !== this.generation || !this.writing) return;
      this.recover();
      this.onStall?.();
    }, this.stallMs);
    this.terminal.write(chunk, () => {
      if (generation !== this.generation || this.disposed) return;
      if (this.stallTimer) clearTimeout(this.stallTimer);
      this.stallTimer = null;
      this.writing = false;
      setTimeout(() => this.pump(), 0);
    });
  }
}
