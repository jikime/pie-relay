import { describe, expect, it, vi } from "vitest";
import { TerminalOutputScheduler } from "./terminal-output-scheduler";

describe("TerminalOutputScheduler", () => {
  it("chunks writes and waits for parser callbacks", () => {
    vi.useFakeTimers();
    const writes: Uint8Array[] = [];
    const callbacks: Array<() => void> = [];
    const scheduler = new TerminalOutputScheduler({
      write(data, callback) {
        writes.push(data);
        if (callback) callbacks.push(callback);
      },
    }, { chunkBytes: 4 });

    scheduler.enqueue(new Uint8Array([1, 2, 3, 4, 5, 6]));
    scheduler.enqueue(new Uint8Array([9]));
    expect(Array.from(writes[0])).toEqual([1, 2, 3, 4]);
    expect(writes).toHaveLength(1);
    callbacks.shift()?.();
    vi.runOnlyPendingTimers();
    expect(Array.from(writes[1])).toEqual([5, 6]);
    callbacks.shift()?.();
    vi.runOnlyPendingTimers();
    expect(Array.from(writes[2])).toEqual([9]);
    scheduler.dispose();
    vi.useRealTimers();
  });

  it("bounds hidden-room backlog and asks for recovery", () => {
    let overflow = 0;
    const scheduler = new TerminalOutputScheduler({ write() {} }, {
      maxBackgroundBytes: 4,
      onOverflow: () => { overflow += 1; },
    });
    expect(scheduler.enqueue(new Uint8Array(8), true)).toBe(false);
    expect(overflow).toBe(1);
    scheduler.dispose();
  });
});
