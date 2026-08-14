import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RelayConnection, backoffDelay, type ConnStatus } from "./connection";

// FakeWebSocket stands in for the browser WebSocket: RelayConnection only
// touches readyState/onopen/onmessage/onerror/onclose/close/send, so that's
// all we implement. Tests drive the handshake by calling onopen/onclose
// directly instead of a real network round-trip.
class FakeWebSocket {
  static readonly OPEN = 1;
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  readyState = 0;
  url: string;
  protocols: string[];
  sent: string[] = [];
  constructor(url: string, protocols: string[] = []) {
    this.url = url;
    this.protocols = protocols;
  }
  close(): void {
    this.readyState = 3;
  }
  send(payload: string): void {
    this.sent.push(payload);
  }
}

let sockets: FakeWebSocket[] = [];

function installFakeWebSocket(): void {
  sockets = [];
  class TrackedFakeWebSocket extends FakeWebSocket {
    constructor(url: string, protocols: string[] = []) {
      super(url, protocols);
      sockets.push(this);
    }
  }
  // @ts-expect-error — test shim; RelayConnection only needs the subset of
  // the real WebSocket surface implemented above.
  globalThis.WebSocket = TrackedFakeWebSocket;
}

function lastSocket(): FakeWebSocket {
  return sockets[sockets.length - 1];
}

describe("backoffDelay (jitter)", () => {
  it("stays within [50%, 100%] of the capped exponential step", () => {
    for (const attempt of [0, 1, 2, 3, 4, 5, 10]) {
      const cap = Math.min(1000 * Math.pow(2, attempt), 30000);
      for (let i = 0; i < 200; i++) {
        const d = backoffDelay(attempt);
        expect(d).toBeGreaterThanOrEqual(cap / 2);
        expect(d).toBeLessThanOrEqual(cap);
      }
    }
  });

  it("never exceeds the 30s cap even for large attempts, and still varies", () => {
    const seen = new Set<number>();
    for (let i = 0; i < 50; i++) {
      const d = backoffDelay(20);
      expect(d).toBeLessThanOrEqual(30000);
      expect(d).toBeGreaterThanOrEqual(15000);
      seen.add(d);
    }
    // Jitter should produce more than a single repeated value across 50 draws
    // (astronomically unlikely to collide every time if Math.random() is live).
    expect(seen.size).toBeGreaterThan(1);
  });
});

describe("RelayConnection auth-expired classification", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    installFakeWebSocket();
  });

  afterEach(() => {
    vi.useRealTimers();
    // @ts-expect-error — remove the shim between tests
    delete globalThis.WebSocket;
  });

  it("classifies 3 consecutive pre-open handshake failures as auth-expired, and keeps retrying", () => {
    const statuses: ConnStatus[] = [];
    const conn = new RelayConnection(
      "wss://relay.example.com/ws?ticket=deadbeef",
      {
        onMessage: () => {},
        onStatus: (s) => statuses.push(s),
      },
    );
    conn.start();
    expect(statuses[statuses.length - 1]).toBe("connecting");
    expect(sockets).toHaveLength(1);

    // The handshake never reaches onopen — 3 dials in a row, each closing
    // cold. Advance past the (jittered, capped) backoff after each so the
    // next dial happens before the next failure is injected.
    for (let i = 0; i < 3; i++) {
      lastSocket().onclose?.();
      vi.advanceTimersByTime(31000);
    }

    expect(statuses[statuses.length - 1]).toBe("auth-expired");
    // Still retrying in the background (not stopped) — a fresh socket keeps
    // getting dialed after the classification flips.
    expect(sockets.length).toBeGreaterThanOrEqual(4);

    conn.close();
  });

  it("uses the ticket subprotocol on every reconnect without putting it in the URL", () => {
    const conn = new RelayConnection(
      "wss://relay.example.com/ws/participant",
      { onMessage: () => {}, onStatus: () => {} },
      ["pie-relay.ticket.signed.jwt"],
    );
    conn.start();
    expect(lastSocket().url).not.toContain("signed.jwt");
    expect(lastSocket().protocols).toEqual(["pie-relay.ticket.signed.jwt"]);
    lastSocket().onclose?.();
    vi.advanceTimersByTime(31000);
    expect(lastSocket().protocols).toEqual(["pie-relay.ticket.signed.jwt"]);
    conn.close();
  });

  it("a successful open resets the pre-open failure streak and status", () => {
    const statuses: ConnStatus[] = [];
    const conn = new RelayConnection(
      "wss://relay.example.com/ws?ticket=deadbeef",
      {
        onMessage: () => {},
        onStatus: (s) => statuses.push(s),
      },
    );
    conn.start();

    // Two consecutive pre-open failures — short of the 3-failure threshold.
    for (let i = 0; i < 2; i++) {
      lastSocket().onclose?.();
      vi.advanceTimersByTime(31000);
    }
    expect(statuses[statuses.length - 1]).toBe("reconnecting");

    // Now the handshake succeeds.
    const ws = lastSocket();
    ws.readyState = FakeWebSocket.OPEN;
    ws.onopen?.();
    expect(statuses[statuses.length - 1]).toBe("open");
    const join = JSON.parse(ws.sent[0]);
    expect(join).toMatchObject({
      type: "relay_join",
      protocolVersion: "2.0",
      streamId: "room",
    });

    // A later drop after a healthy connection starts counting from zero
    // again — a single failure right after "open" must NOT already read as
    // auth-expired.
    ws.onclose?.();
    expect(statuses[statuses.length - 1]).toBe("reconnecting");

    conn.close();
  });
});
