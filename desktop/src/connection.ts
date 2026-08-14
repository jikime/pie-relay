// connection.ts — a reconnecting participant WebSocket. This is the only
// stateful/live piece; all message meaning lives in protocol.ts. The relay does
// not renew tickets, so we re-dial with the same ticket subprotocol on drop;
// that works as long as the guest token (guestExpTTL) is still valid.
//
// "auth-expired" is a heuristic, not a certainty: browsers don't expose the
// WS handshake's HTTP status (a rejected ticket usually surfaces as a plain
// 1006 abnormal-closure, same as a network blip), so we can't tell a bad
// token from a flaky link on a single failure. What we CAN tell apart is
// repetition: a transient network issue lets the handshake succeed again as
// soon as connectivity returns, while a bad/expired ticket fails the
// handshake every single time. So we count consecutive closes that happen
// BEFORE onopen ever fires, and once that streak crosses a threshold we
// classify the connection as a likely-permanent auth failure — while still
// continuing to retry in the background in case the relay comes back with
// the same secret.

export type ConnStatus =
  "connecting" | "open" | "reconnecting" | "auth-expired" | "closed";

export interface ConnectionHandlers {
  onMessage: (raw: string) => void;
  onStatus: (status: ConnStatus, attempt: number) => void;
}

const RELAY_PROTOCOL_VERSION = "2.0";

function connectionClientId(): string {
  const c = globalThis.crypto;
  if (c && typeof c.randomUUID === "function") return c.randomUUID();
  return `pie-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

// Exponential backoff bounds from the design: 1s → 30s.
const BACKOFF_MIN_MS = 1000;
const BACKOFF_MAX_MS = 30000;

// Consecutive pre-open (handshake never reached onopen) failures before we
// classify the connection as a probable auth rejection rather than a
// transient blip.
const AUTH_FAILURE_THRESHOLD = 3;

export class RelayConnection {
  private url: string;
  private handlers: ConnectionHandlers;
  private protocols: string[];
  private ws: WebSocket | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private attempt = 0; // consecutive failed/closed connects; 0 while healthy
  private stopped = false;
  private readonly clientId = connectionClientId();
  // Consecutive closes that happened without ever reaching onopen. Reset to 0
  // the moment any attempt actually opens; see the module comment above.
  private preOpenFailures = 0;

  constructor(
    url: string,
    handlers: ConnectionHandlers,
    protocols: string[] = [],
  ) {
    this.url = url;
    this.handlers = handlers;
    this.protocols = protocols;
  }

  start(): void {
    this.stopped = false;
    this.open();
  }

  // send returns false if the socket isn't open (caller may surface a hint).
  send(payload: string): boolean {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(payload);
      return true;
    }
    return false;
  }

  isOpen(): boolean {
    return this.ws?.readyState === WebSocket.OPEN;
  }

  // close tears down permanently; no further reconnects.
  close(): void {
    this.stopped = true;
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.ws) {
      this.ws.onopen =
        this.ws.onmessage =
        this.ws.onerror =
        this.ws.onclose =
          null;
      try {
        this.ws.close();
      } catch {
        /* already closing */
      }
      this.ws = null;
    }
    this.handlers.onStatus("closed", this.attempt);
  }

  private open(): void {
    if (this.stopped) return;
    this.handlers.onStatus(this.currentStatus(), this.attempt);

    let ws: WebSocket;
    try {
      ws =
        this.protocols.length > 0
          ? new WebSocket(this.url, this.protocols)
          : new WebSocket(this.url);
    } catch {
      // Construction itself failed — that never reaches onopen either, so it
      // counts toward the same pre-open failure streak.
      this.preOpenFailures += 1;
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;
    let openedThisAttempt = false;

    ws.onopen = () => {
      openedThisAttempt = true;
      this.attempt = 0;
      this.preOpenFailures = 0;
      // Optional protocol-v2 negotiation. A legacy relay simply forwards this
      // unknown control frame to a host that ignores it; a v2 relay consumes it
      // and replies with relay_join_ack. Application frames stay compatible.
      ws.send(
        JSON.stringify({
          type: "relay_join",
          protocolVersion: RELAY_PROTOCOL_VERSION,
          streamId: "room",
          clientId: this.clientId,
        }),
      );
      this.handlers.onStatus("open", 0);
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data === "string") this.handlers.onMessage(ev.data);
    };
    ws.onerror = () => {
      // onclose will follow and drive the reconnect; nothing to do here.
    };
    ws.onclose = () => {
      if (this.stopped) return;
      this.ws = null;
      if (!openedThisAttempt) {
        this.preOpenFailures += 1;
      } else {
        this.preOpenFailures = 0;
      }
      this.scheduleReconnect();
    };
  }

  // currentStatus reflects the auth-expired heuristic on top of the normal
  // connecting/reconnecting distinction: once the pre-open failure streak
  // crosses the threshold, every status update (including the ones emitted
  // while still retrying) reports "auth-expired" instead of "reconnecting"
  // so the UI stops implying an ordinary, recoverable blip.
  private currentStatus(): ConnStatus {
    if (this.preOpenFailures >= AUTH_FAILURE_THRESHOLD) return "auth-expired";
    return this.attempt === 0 ? "connecting" : "reconnecting";
  }

  private scheduleReconnect(): void {
    if (this.stopped) return;
    const delay = backoffDelay(this.attempt);
    this.attempt += 1;
    // Retrying continues regardless of classification — only the reported
    // status changes so the UI can tell the two apart.
    this.handlers.onStatus(this.currentStatus(), this.attempt);
    this.timer = setTimeout(() => this.open(), delay);
  }
}

// backoffDelay: 1s, 2s, 4s, … capped at 30s, with jitter so many clients
// reconnecting after a shared relay blip don't all redial in lockstep
// (thundering herd). Returns a value uniformly distributed in
// [50%, 100%] of the capped exponential step. Exposed for testing/clarity.
export function backoffDelay(attempt: number): number {
  const exp = BACKOFF_MIN_MS * Math.pow(2, attempt);
  const capped = Math.min(exp, BACKOFF_MAX_MS);
  return capped / 2 + Math.random() * (capped / 2);
}
