import { describe, expect, it } from "vitest";
import { TerminalStreamTracker } from "./terminal-stream";

describe("TerminalStreamTracker", () => {
  it("accepts contiguous output and drops duplicates", () => {
    const t = new TerminalStreamTracker();
    expect(t.acceptOutput({ incarnationId: "a", seq: 1 })).toBe("apply");
    expect(t.acceptOutput({ incarnationId: "a", seq: 2 })).toBe("apply");
    expect(t.acceptOutput({ incarnationId: "a", seq: 2 })).toBe("duplicate");
  });

  it("requires a snapshot after a gap or incarnation change", () => {
    const t = new TerminalStreamTracker();
    expect(t.acceptOutput({ incarnationId: "a", seq: 4 })).toBe("gap");
    expect(t.acceptSnapshot({ incarnationId: "a", seq: 4 })).toBe("snapshot");
    expect(t.acceptOutput({ incarnationId: "a", seq: 5 })).toBe("apply");
    expect(t.acceptOutput({ incarnationId: "b", seq: 1 })).toBe("gap");
  });

  it("keeps legacy unsequenced frames compatible", () => {
    expect(new TerminalStreamTracker().acceptOutput({})).toBe("apply");
  });
});
