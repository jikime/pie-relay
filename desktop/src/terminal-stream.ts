export interface SequencedTerminalFrame {
  incarnationId?: string;
  seq?: number;
}

export type StreamDecision = "apply" | "snapshot" | "duplicate" | "gap";

// Tracks a terminal producer incarnation and monotonically increasing output
// sequence. Legacy frames (no metadata) remain accepted. A gap is never
// painted because missing ANSI bytes can permanently corrupt xterm state;
// callers request an authoritative snapshot instead.
export class TerminalStreamTracker {
  private incarnationId = "";
  private lastSeq = 0;
  private awaitingSnapshot = false;

  acceptOutput(frame: SequencedTerminalFrame): StreamDecision {
    if (!frame.incarnationId || !Number.isSafeInteger(frame.seq) || (frame.seq as number) <= 0) {
      return "apply";
    }
    const seq = frame.seq as number;
    if (this.incarnationId === "") {
      this.incarnationId = frame.incarnationId;
      if (seq !== 1) {
        this.awaitingSnapshot = true;
        return "gap";
      }
      this.lastSeq = seq;
      return "apply";
    }
    if (frame.incarnationId !== this.incarnationId) {
      this.incarnationId = frame.incarnationId;
      this.lastSeq = 0;
      this.awaitingSnapshot = true;
      return "gap";
    }
    if (this.awaitingSnapshot) return "duplicate";
    if (seq <= this.lastSeq) return "duplicate";
    if (seq !== this.lastSeq + 1) {
      this.awaitingSnapshot = true;
      return "gap";
    }
    this.lastSeq = seq;
    return "apply";
  }

  acceptSnapshot(frame: SequencedTerminalFrame): StreamDecision {
    if (frame.incarnationId && Number.isSafeInteger(frame.seq) && (frame.seq as number) >= 0) {
      this.incarnationId = frame.incarnationId;
      this.lastSeq = frame.seq as number;
    }
    this.awaitingSnapshot = false;
    return "snapshot";
  }

  resumeCursor(): { incarnationId?: string; seq?: number } {
    return this.incarnationId ? { incarnationId: this.incarnationId, seq: this.lastSeq } : {};
  }
}
