// protocol.ts — the pure, socket-free core of the participant client.
//
// It mirrors the wire contract already implemented by the Go TUI
// (../client/internal/tui/events.go + model.go) and enforced by the relay
// (../server/internal/relay/server.go): the same event names, the same
// outbound {type:"chat", prompt, sessionId?} shape, and the same
// session-continuity and host-presence rules. Everything here is a pure
// function so it can be unit-tested (protocol.test.ts) without a WebSocket.

// ---------------------------------------------------------------------------
// Inbound events
// ---------------------------------------------------------------------------

// ServerEvent is the normalized, discriminated union the UI reducer consumes.
// The relay keeps payload bodies opaque, so only these top-level fields matter.
export type ServerEvent =
  | { type: "session_id"; sessionId: string }
  | { type: "text"; text: string } // one streamed delta of the Claude response
  | { type: "thinking"; text: string } // one streamed delta of extended thinking
  | { type: "done"; sessionId: string } // end of a response turn
  | { type: "error"; message: string }
  | { type: "aborted" }
  | { type: "peer_chat"; from: string; text: string }
  | { type: "host_status"; connected: boolean } // host:status / agent:status
  | { type: "unavailable"; reason: string } // agent:unavailable
  // P3-3 activity feed: the executor emits one tool_call per tool_use block and
  // one tool_result per matching result. The relay broadcasts both to everyone.
  | { type: "tool_call"; name: string; input: unknown }
  | { type: "tool_result"; content: unknown }
  // P3-3 approval: only delivered to role=host connections by the relay.
  | { type: "permission_request"; requestId: string; toolName: string; input: unknown };

// rawEvent is the flat superset of every top-level key we read off the wire.
interface RawEvent {
  type?: string;
  text?: string;
  sessionId?: string;
  from?: string;
  message?: string;
  connected?: boolean;
  reason?: string;
  name?: string;
  input?: unknown;
  content?: unknown;
  requestId?: string;
  toolName?: string;
}

// parseServerEvent maps one raw ws JSON line to a ServerEvent, or null for
// events the chat UI ignores (ready/session_status/…) and for malformed JSON.
// A null return is a no-op for the caller — unknown events are silently
// skipped, exactly like the Go TUI.
export function parseServerEvent(raw: string): ServerEvent | null {
  let e: RawEvent;
  try {
    e = JSON.parse(raw) as RawEvent;
  } catch {
    return null;
  }
  if (!e || typeof e !== "object") return null;

  switch (e.type) {
    case "session_id":
      return { type: "session_id", sessionId: e.sessionId ?? "" };
    case "text":
      return { type: "text", text: e.text ?? "" };
    case "thinking":
      return { type: "thinking", text: e.text ?? "" };
    case "done":
      return { type: "done", sessionId: e.sessionId ?? "" };
    case "error":
      return { type: "error", message: e.message ?? "" };
    case "aborted":
      return { type: "aborted" };
    case "peer_chat":
      return { type: "peer_chat", from: e.from ?? "", text: e.text ?? "" };
    // host:status is the new name; agent:status is the legacy duplicate with
    // identical meaning. Both fold to one host_status event (idempotent apply).
    case "host:status":
    case "agent:status":
      return { type: "host_status", connected: Boolean(e.connected) };
    case "agent:unavailable":
      return { type: "unavailable", reason: e.reason ?? "" };
    case "tool_call":
      return { type: "tool_call", name: e.name ?? "", input: e.input };
    case "tool_result":
      return { type: "tool_result", content: e.content };
    case "permission_request":
      // A card we cannot answer (no requestId) is useless — drop it.
      if (!e.requestId) return null;
      return {
        type: "permission_request",
        requestId: e.requestId,
        toolName: e.toolName ?? "",
        input: e.input,
      };
    default:
      return null;
  }
}

// guestSub matches the relay's guest identity shape guest:<name>-<rand4>.
const GUEST_SUB = /^guest:(.+)-[^-]+$/;

// speakerName turns a JWT sub (peer_chat "from") into a friendly display name:
// a guest sub "guest:bob-x7k2" shows as "bob"; anything else shows verbatim.
export function speakerName(from: string): string {
  if (!from) return "?";
  const m = GUEST_SUB.exec(from);
  return m ? m[1] : from;
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// buildChat serializes one outbound chat frame. `from` is intentionally NEVER
// set — the relay injects the verified sub (design policy 4). The field is
// `prompt` because both the executor (req.prompt||req.text) and the relay's
// peer_chat echo key off it; sending only "text" would leave peers' echoes
// blank. sessionId is included only when known, to keep the shared session.
export function buildChat(prompt: string, sessionId: string): string {
  const obj: { type: "chat"; prompt: string; sessionId?: string } = {
    type: "chat",
    prompt,
  };
  if (sessionId) obj.sessionId = sessionId;
  return JSON.stringify(obj);
}

// buildPermissionResponse serializes the host's approval decision. The relay
// only forwards this to the host daemon when it arrives on a role=host
// connection (design gate 4); the executor resolves the pending canUseTool
// promise by requestId. `allow:false` maps to a deny in the executor.
export function buildPermissionResponse(requestId: string, allow: boolean): string {
  return JSON.stringify({ type: "permission_response", requestId, allow });
}

// ---------------------------------------------------------------------------
// Terminal room (Phase 5) — a wire path parallel to, and separate from, the
// chat reducer above. A terminal room streams a host PTY: xterm.js on the
// participant holds the terminal state, so there is no big reducer here — only
// pure parsers/builders and the driver-gate predicate, all unit-testable.
//
// Wire contract (server/docs/terminal-room-design.md §프로토콜):
//   host → all:   {"type":"room_mode","mode":"terminal"}   (once per connect)
//   host → all:   {"type":"pty_output","data":"<base64>"}
//   host → all:   {"type":"pty_exit","code":0}
//   host → all:   {"type":"driver","from":"<sub or ''>"}   (current driver)
//   host → all:   {"type":"pty_size","cols":120,"rows":34} (authoritative PTY grid size)
//   part → host:  {"type":"pty_input","data":"<utf8>"}      (relay injects from)
//   part → host:  {"type":"pty_resize","cols":120,"rows":34}
//   part → host:  {"type":"set_driver","target":"<sub>"}    (role=host only)
//   part ↔ part:  {"type":"room_chat","text":"<msg>"}       (out; relay injects from)
//   part ↔ part:  {"type":"room_chat","from":"<sub>","text":"<msg>"} (in, to OTHERS)
//     — a human-to-human side chat between everyone in the room (the host
//       operator is a participant too). It NEVER reaches the daemon/PTY: the
//       relay only fans it out to the other participants (no echo to sender),
//       and messages are ephemeral (no persistence/replay).
// ---------------------------------------------------------------------------

export type RoomMode = "chat" | "terminal";

export interface ParticipantPresence {
  userId: string;
  role: string;
  access: string;
  connectionId: string;
  connectedAt: number;
}

// TerminalEvent is the normalized union TerminalScreen consumes. Kept fully
// distinct from ServerEvent so the chat reducer never grows terminal cases.
export type TerminalEvent =
  | { type: "room_mode"; mode: RoomMode }
  | { type: "pty_output"; data: string; streamId?: string; incarnationId?: string; seq?: number }
  | { type: "pty_snapshot"; data: string; streamId?: string; incarnationId?: string; seq?: number; cols?: number; rows?: number }
  | { type: "pty_exit"; code: number | null }
  | { type: "driver"; from: string } // current driver sub; "" = nobody (view-only)
  | { type: "driver_state"; driver: string; generation: number; expiresAt?: number }
  | { type: "driver_request"; from: string }
  | { type: "participant_roster"; participants: ParticipantPresence[]; driver: string; generation: number }
  | { type: "pty_size"; cols: number; rows: number } // authoritative PTY grid size
  | { type: "room_chat"; from: string; text: string }; // human side-chat; never hits the PTY

interface RawTermEvent {
  type?: string;
  mode?: string;
  data?: string;
  code?: number;
  cols?: number;
  rows?: number;
  from?: string;
  text?: string;
  streamId?: string;
  incarnationId?: string;
  seq?: number;
  driver?: string;
  generation?: number;
  expiresAt?: number;
  participants?: unknown;
}

function terminalSequence(e: RawTermEvent, allowZero: boolean) {
  return {
    ...(typeof e.streamId === "string" ? { streamId: e.streamId } : {}),
    ...(typeof e.incarnationId === "string" ? { incarnationId: e.incarnationId } : {}),
    ...(Number.isSafeInteger(e.seq) && (e.seq as number) >= (allowZero ? 0 : 1)
      ? { seq: e.seq }
      : {}),
  };
}

// parseTerminalEvent maps one raw ws line to a TerminalEvent, or null for
// frames a terminal room ignores (chat events, ready, …) and malformed JSON.
export function parseTerminalEvent(raw: string): TerminalEvent | null {
  let e: RawTermEvent;
  try {
    e = JSON.parse(raw) as RawTermEvent;
  } catch {
    return null;
  }
  if (!e || typeof e !== "object") return null;
  switch (e.type) {
    case "room_mode":
      return e.mode === "terminal" || e.mode === "chat"
        ? { type: "room_mode", mode: e.mode }
        : null;
    case "pty_output":
      return {
        type: "pty_output", data: typeof e.data === "string" ? e.data : "",
        ...terminalSequence(e, false),
      };
    case "pty_snapshot":
      return {
        type: "pty_snapshot", data: typeof e.data === "string" ? e.data : "",
        ...terminalSequence(e, true),
        ...(Number.isFinite(e.cols) && (e.cols as number) > 0 ? { cols: e.cols } : {}),
        ...(Number.isFinite(e.rows) && (e.rows as number) > 0 ? { rows: e.rows } : {}),
      };
    case "pty_exit":
      return { type: "pty_exit", code: typeof e.code === "number" ? e.code : null };
    case "driver":
      return { type: "driver", from: typeof e.from === "string" ? e.from : "" };
    case "driver_state":
      return {
        type: "driver_state",
        driver: typeof e.driver === "string" ? e.driver : "",
        generation: Number.isSafeInteger(e.generation) ? (e.generation as number) : 0,
        ...(Number.isFinite(e.expiresAt) && (e.expiresAt as number) > 0
          ? { expiresAt: e.expiresAt }
          : {}),
      };
    case "driver_request":
      return { type: "driver_request", from: typeof e.from === "string" ? e.from : "" };
    case "participant_roster": {
      const participants = Array.isArray(e.participants)
        ? e.participants.flatMap((raw): ParticipantPresence[] => {
            if (!raw || typeof raw !== "object") return [];
            const p = raw as Record<string, unknown>;
            if (typeof p.userId !== "string" || typeof p.connectionId !== "string") return [];
            return [
              {
                userId: p.userId,
                role: typeof p.role === "string" ? p.role : "participant",
                access: typeof p.access === "string" ? p.access : "view",
                connectionId: p.connectionId,
                connectedAt: typeof p.connectedAt === "number" ? p.connectedAt : 0,
              },
            ];
          })
        : [];
      return {
        type: "participant_roster",
        participants,
        driver: typeof e.driver === "string" ? e.driver : "",
        generation: Number.isSafeInteger(e.generation) ? (e.generation as number) : 0,
      };
    }
    case "pty_size":
      // An invalid size must never corrupt the grid — only accept two finite,
      // positive numbers; anything else is dropped (null = no-op for the caller).
      return Number.isFinite(e.cols) && Number.isFinite(e.rows) &&
        (e.cols as number) > 0 && (e.rows as number) > 0
        ? { type: "pty_size", cols: e.cols as number, rows: e.rows as number }
        : null;
    case "room_chat":
      return { type: "room_chat", from: e.from ?? "", text: e.text ?? "" };
    default:
      return null;
  }
}

// peekRoomMode extracts only a room_mode announcement from any raw frame,
// without the full terminal parser. The chat screen (the default mount for a
// not-yet-classified room) uses it to notice it is actually a terminal room and
// hand off, so the chat reducer stays free of terminal frames. Returns null for
// every non-room_mode frame, including malformed JSON.
export function peekRoomMode(raw: string): RoomMode | null {
  const ev = parseTerminalEvent(raw);
  return ev && ev.type === "room_mode" ? ev.mode : null;
}

// buildPtyInput serializes one keystroke/paste. data is a raw UTF-8 string (not
// base64); JSON handles the encoding. `from` is NEVER set — the relay injects
// the verified sub, and the pty-host drops input whose from ≠ driver.
export function buildPtyInput(data: string): string {
  return JSON.stringify({ type: "pty_input", data });
}

// buildPtyScroll carries a wheel / alternate-scroll escape sequence that ANY
// participant (not just the driver) may send to move the shared terminal view.
// The relay does not view-gate it and the pty-host writes it to the PTY without
// the driver check — so it must ONLY ever carry scroll sequences (see
// isScrollSeq); clicks and keys stay on the driver-gated pty_input path.
export function buildPtyScroll(data: string): string {
  return JSON.stringify({ type: "pty_scroll", data });
}

// isScrollSeq reports whether an xterm-generated input sequence is a WHEEL scroll
// (or alternate-scroll cursor key), i.e. the only mouse output every participant
// is allowed to forward. It matches: SGR mouse wheel (CSI < 64/65/66/67 … , the
// 0x40 "wheel" button bit), legacy X10 mouse wheel (CSI M with Cb in 0x60–0x63),
// and alternate-scroll cursor keys (SS3/CSI A/B — up/down — emitted when an
// alt-screen app has no mouse tracking). Clicks/drags (buttons 0–2) are excluded.
export function isScrollSeq(data: string): boolean {
  // SGR mouse: \x1b[<{btn};{x};{y}{M|m}
  const sgr = /^\x1b\[<(\d+);\d+;\d+[Mm]$/.exec(data);
  if (sgr) {
    const btn = Number(sgr[1]);
    return (btn & 0x40) !== 0; // wheel buttons carry the 0x40 bit
  }
  // Legacy X10 mouse: \x1b[M Cb Cx Cy — wheel when (Cb-32) has the 0x40 bit.
  if (data.length === 6 && data.charCodeAt(0) === 0x1b && data[1] === "[" && data[2] === "M") {
    return ((data.charCodeAt(3) - 32) & 0x40) !== 0;
  }
  // Alternate-scroll cursor keys emitted on wheel: SS3 (\x1bOA/\x1bOB) or
  // CSI (\x1b[A/\x1b[B) — up/down only.
  return data === "\x1bOA" || data === "\x1bOB" || data === "\x1b[A" || data === "\x1b[B";
}

// buildPtyResize serializes a terminal resize. The pty-host only applies it for
// the current driver (avoids size fights); every viewer still fits locally.
export function buildPtyResize(cols: number, rows: number): string {
  return JSON.stringify({ type: "pty_resize", cols, rows });
}

// buildSetDriver serializes the operator's driver assignment (target = the sub
// to hand control to, or "" to revoke → view-only for all). The relay forwards
// it to the host only from a role=host connection (S5-1 gate). We emit BOTH
// `target` (design-doc field) and `driver` (the field the relay's own S5-1 test
// uses as the example payload) with the same value, so the pty-host reads the
// target correctly whichever key it settled on — the extra key is inert to the
// relay, which only inspects `type`.
export function buildSetDriver(target: string): string {
  return JSON.stringify({ type: "set_driver", target, driver: target });
}

// buildRequestScreen asks the terminal host to replay its current scrollback to
// this viewer only. Sent on (re)connect: the relay is stateless, so a viewer
// that joins an already-running (often idle) terminal missed the shell's initial
// prompt and would otherwise see only a blinking cursor. `from` is NEVER set —
// the relay injects the verified identity, which the host uses as the reply
// target so the replay reaches this viewer alone.
export function buildRequestScreen(cursor: { incarnationId?: string; seq?: number } = {}): string {
  return JSON.stringify({ type: "request_screen", ...cursor });
}

export function buildDriverHeartbeat(): string {
  return JSON.stringify({ type: "driver_heartbeat" });
}

export function buildDriverRequest(): string {
  return JSON.stringify({ type: "driver_request" });
}

// buildRoomChat serializes one outbound side-chat line — the human-to-human chat
// that rides alongside the terminal room but NEVER reaches the daemon/PTY. Like
// buildChat, `from` is intentionally never sent: the relay injects the verified
// sub before fanning the message out to the OTHER participants (the sender gets
// no echo, so this client appends its own message optimistically). Messages are
// ephemeral — the relay neither stores nor replays them.
export function buildRoomChat(text: string): string {
  return JSON.stringify({ type: "room_chat", text });
}

// base64ToBytes decodes a base64 PTY payload into raw bytes for term.write —
// binary-safe (preserves UTF-8 multibyte sequences and ANSI escapes), matching
// the prototype viewer. A malformed payload yields an empty array (dropped),
// never a throw. atob exists in the Tauri webview and in Node ≥16 (tests).
export function base64ToBytes(b64: string): Uint8Array {
  try {
    const bin = atob(b64);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    return bytes;
  } catch {
    return new Uint8Array(0);
  }
}

// shouldSendInput is the driver gate, mirrored from the pty-host's own check
// (from === driver): I may transmit keystrokes only when I am the current
// driver. Enforced locally too, so a non-driver's keys never waste a relay
// round-trip (design: "로컬에서 막고 서버 왕복 낭비 방지"). An unknown self
// (mySub === "") is never the driver.
export function shouldSendInput(driver: string, mySub: string): boolean {
  return mySub !== "" && driver === mySub;
}

// KeyEventLike is the minimal slice of a KeyboardEvent that keyToSequence reads,
// so the mapping can be unit-tested without a DOM.
export interface KeyEventLike {
  key: string;
  ctrlKey: boolean;
  altKey: boolean;
  metaKey: boolean;
}

// keyToSequence maps one keydown to the raw terminal byte string to transmit, or
// null when the key is an ordinary character that must be left to the textarea's
// `input` event instead — that path is what keeps IME composition and other
// multilingual input working (the whole reason the terminal takes input through
// our own textarea overlay rather than xterm's). Pure and total. Callers must
// already have skipped IME composition (isComposing / keyCode 229) before this.
//
// Alt/Option is intentionally NOT mapped to an ESC-prefixed meta sequence: on
// macOS WKWebView Option is how the IME composes accented/special characters, so
// swallowing it here would break the very input path this overlay exists to fix.
// Such keys return null and flow through `input` like any other character.
export function keyToSequence(e: KeyEventLike): string | null {
  // Named (non-printable) keys map to fixed sequences.
  switch (e.key) {
    case "Enter":
      return "\r";
    case "Backspace":
      return "\x7f";
    case "Tab":
      return "\t";
    case "Escape":
      return "\x1b";
    case "ArrowUp":
      return "\x1b[A";
    case "ArrowDown":
      return "\x1b[B";
    case "ArrowRight":
      return "\x1b[C";
    case "ArrowLeft":
      return "\x1b[D";
    case "Home":
      return "\x1b[H";
    case "End":
      return "\x1b[F";
    case "PageUp":
      return "\x1b[5~";
    case "PageDown":
      return "\x1b[6~";
    case "Delete":
      return "\x1b[3~";
  }

  // Ctrl+<key> control codes. Cmd (meta) is never a terminal chord — leave it to
  // the browser (copy/paste/etc). Only single-character keys form a chord.
  if (e.ctrlKey && !e.metaKey && e.key.length === 1) {
    const lower = e.key.toLowerCase();
    // Ctrl+a..z → 0x01..0x1a (Ctrl+C = \x03, Ctrl+D = \x04, …).
    if (lower >= "a" && lower <= "z") {
      return String.fromCharCode(lower.charCodeAt(0) - 96);
    }
    // The other C0 control chords a shell commonly relies on.
    switch (e.key) {
      case "[":
        return "\x1b"; // Ctrl+[ = ESC
      case "]":
        return "\x1d"; // Ctrl+] = GS
      case "\\":
        return "\x1c"; // Ctrl+\ = FS (quit)
      case " ":
        return "\x00"; // Ctrl+Space = NUL
    }
  }

  // Ordinary character (and anything else): let the `input` event deliver it.
  return null;
}

// cursorPx maps a terminal cursor cell to the pixel offset (relative to the
// terminal wrap) where the IME input overlay must sit so composing text appears
// inline at the cursor. cellW/cellH are the measured cell size (screen px / cols
// or rows); padL/padT are the .term-host padding. Pure arithmetic so it can be
// unit-tested without a DOM: left = padL + cursorX*cellW, top = padT + cursorY*cellH.
export function cursorPx(args: {
  cursorX: number;
  cursorY: number;
  cellW: number;
  cellH: number;
  padL: number;
  padT: number;
}): { left: number; top: number } {
  return {
    left: args.padL + args.cursorX * args.cellW,
    top: args.padT + args.cursorY * args.cellH,
  };
}

// jwtPayload decodes the unverified payload segment of a JWT into a plain
// object. Shared by subFromToken / roomFromToken. It only reads the payload for
// display/gating — identity and room are still enforced server-side. Returns
// null for any non-JWT or malformed payload.
function jwtPayload(token: string): Record<string, unknown> | null {
  const parts = token.split(".");
  if (parts.length < 2) return null;
  try {
    let b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    while (b64.length % 4 !== 0) b64 += "=";
    const json = JSON.parse(atob(b64)) as unknown;
    return json && typeof json === "object" ? (json as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

// subFromToken decodes a participant JWT's `sub` claim (the verified identity
// the relay injects as `from`), so a terminal room can tell whether *I* am the
// current driver and show the operator my own id to hand out. Returns "" for any
// non-JWT or missing claim.
export function subFromToken(token: string): string {
  const p = jwtPayload(token);
  return p && typeof p.sub === "string" ? p.sub : "";
}

// This unverified claim only controls whether the request button is shown; the
// relay remains authoritative. Legacy tokens omit access and are control by
// the server's compatibility contract.
export function accessFromToken(token: string): "control" | "view" | "" {
  const p = jwtPayload(token);
  if (!p) return "";
  return p.access === "view" ? "view" : "control";
}

// roomFromToken decodes a host token's `room` claim — the room the daemon hosts.
// The relay stamps `room` into host tokens (falling back to `sub` when absent),
// so "open this room" can label and dedupe the room without a round-trip. It
// reads only the unverified payload; the relay still authorizes by verifying the
// token. Returns the `room` claim, else `sub`, else "" for a non-JWT.
export function roomFromToken(token: string): string {
  const p = jwtPayload(token);
  if (!p) return "";
  if (typeof p.room === "string" && p.room) return p.room;
  return typeof p.sub === "string" ? p.sub : "";
}

// Runtime metadata is carried by scoped Pie credentials so every client can
// label a session consistently even when it joins through an invite code. It is
// presentation metadata only; the signed token and Relay remain authoritative.
export function executionTargetFromToken(
  token: string,
): "local" | "docker" | undefined {
  const value = jwtPayload(token)?.execution_target;
  return value === "local" || value === "docker" ? value : undefined;
}

export function deviceIdFromToken(token: string): string | undefined {
  const value = jwtPayload(token)?.device_id;
  return typeof value === "string" && value ? value : undefined;
}

export function relaySessionIdFromToken(token: string): string | undefined {
  const value = jwtPayload(token)?.session_id;
  return typeof value === "string" && value ? value : undefined;
}

// ---------------------------------------------------------------------------
// Tool activity presentation (pure helpers, shared by UI + tests)
// ---------------------------------------------------------------------------

// toolPreview builds the one-line activity label body, e.g. the command for a
// Bash call or the path for a file tool, falling back to a compact JSON dump.
// Kept short so the collapsed feed line stays a single row.
export function toolPreview(input: unknown): string {
  if (input && typeof input === "object") {
    const o = input as Record<string, unknown>;
    const first =
      pickString(o.command) ??
      pickString(o.file_path) ??
      pickString(o.path) ??
      pickString(o.pattern) ??
      pickString(o.url) ??
      pickString(o.description);
    if (first) return clip(first.replace(/\s+/g, " ").trim(), 120);
  }
  const s = pickString(input);
  if (s) return clip(s.replace(/\s+/g, " ").trim(), 120);
  return "";
}

function pickString(v: unknown): string | undefined {
  return typeof v === "string" && v.trim() !== "" ? v : undefined;
}

function clip(s: string, max: number): string {
  return s.length > max ? s.slice(0, max) + "…" : s;
}

// summarizeJson pretty-prints tool input/args for the expanded views. Non-JSON
// values (undefined, circular) degrade to their String() form rather than throw.
export function summarizeJson(value: unknown): string {
  if (value === undefined) return "";
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

// renderToolResultContent flattens the SDK's tool_result content — a string, or
// an array of content blocks ({type:'text',text} and friends) — into plain text
// for the expanded activity view.
export function renderToolResultContent(content: unknown): string {
  if (content == null) return "";
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((b) => {
        if (typeof b === "string") return b;
        if (b && typeof b === "object") {
          const o = b as Record<string, unknown>;
          if (typeof o.text === "string") return o.text;
        }
        return summarizeJson(b);
      })
      .filter((s) => s !== "")
      .join("\n");
  }
  return summarizeJson(content);
}

// ---------------------------------------------------------------------------
// Chat state + reducer
// ---------------------------------------------------------------------------

export type SpeakerKind =
  | "claude"
  | "me"
  | "peer"
  | "system"
  | "error"
  | "activity" // a tool_call (+ its matched tool_result), collapsed by default
  | "permission"; // an inline approval card (host connections only)

// ToolActivity is the payload of an "activity" message: one tool_call, plus the
// tool_result once it arrives (order-matched to the latest pending call).
export interface ToolActivity {
  name: string;
  input: unknown;
  result?: string; // rendered tool_result content, set once matched
  hasResult: boolean;
}

export type PermissionDecision = "pending" | "allowed" | "denied";

// PermissionCard is the payload of a "permission" message: an approval request
// and its (initially pending) decision. Fixed to allowed/denied once answered.
export interface PermissionCard {
  requestId: string;
  toolName: string;
  input: unknown;
  decision: PermissionDecision;
}

export interface ChatMessage {
  id: number; // stable key for rendering; assigned from state.nextId
  kind: SpeakerKind;
  name?: string; // display label for me/peer; ignored for claude/system/error
  text: string;
  activity?: ToolActivity; // present iff kind === "activity"
  permission?: PermissionCard; // present iff kind === "permission"
}

export interface ChatState {
  messages: ChatMessage[];
  streaming: string; // in-progress Claude response ("" between turns)
  responding: boolean; // a turn is live (awaiting done/error/aborted)
  thinking: string; // latest thinking text, shown collapsed while responding
  sessionId: string; // captured for conversation continuity
  hostUp: boolean; // host (laptop daemon) presence
  nextId: number; // monotonic id source, keeps the reducer pure
}

export function initialChatState(): ChatState {
  return {
    messages: [],
    streaming: "",
    responding: false,
    thinking: "",
    sessionId: "",
    hostUp: false,
    nextId: 1,
  };
}

// ChatAction is everything that mutates the transcript: a parsed server event,
// the local user pressing send, or a locally-injected system line (e.g. a
// reconnect banner). Keeping all three in one reducer keeps id assignment and
// streaming finalization in a single, testable place.
export type ChatAction =
  | { kind: "event"; event: ServerEvent }
  | { kind: "send"; name: string; text: string }
  | { kind: "system"; text: string }
  // The host decided a pending approval locally (after the response was sent).
  | { kind: "permission_decide"; requestId: string; allow: boolean };

// push appends a message, allocating its id from state.nextId.
function push(state: ChatState, msg: Omit<ChatMessage, "id">): ChatState {
  return {
    ...state,
    messages: [...state.messages, { id: state.nextId, ...msg }],
    nextId: state.nextId + 1,
  };
}

// flushStreaming turns the in-progress Claude buffer into a finalized transcript
// line (if non-blank) and clears it. finalizeStreaming ends the turn; the
// keep-responding variant is used mid-turn (before a tool_call) so the feed
// stays in wire order: Claude's preamble, then the activity line, then more.
function flushStreaming(state: ChatState, keepResponding: boolean): ChatState {
  const trimmed = state.streaming.trim();
  const flushed = trimmed ? push(state, { kind: "claude", text: trimmed }) : state;
  return {
    ...flushed,
    streaming: "",
    responding: keepResponding,
    thinking: keepResponding ? flushed.thinking : "",
  };
}

function finalizeStreaming(state: ChatState): ChatState {
  return flushStreaming(state, false);
}

export function chatReducer(state: ChatState, action: ChatAction): ChatState {
  switch (action.kind) {
    case "send":
      return {
        ...push(state, { kind: "me", name: action.name, text: action.text }),
        responding: true,
        thinking: "",
      };
    case "system":
      return push(state, { kind: "system", text: action.text });
    case "permission_decide":
      return decidePermission(state, action.requestId, action.allow);
    case "event":
      return applyEvent(state, action.event);
  }
}

// decidePermission fixes a pending approval card to its final decision. Matching
// by requestId keeps it correct when several cards are open at once. A card
// already decided (or an unknown id) is left untouched.
function decidePermission(state: ChatState, requestId: string, allow: boolean): ChatState {
  let changed = false;
  const messages = state.messages.map((m) => {
    if (
      m.kind === "permission" &&
      m.permission &&
      m.permission.requestId === requestId &&
      m.permission.decision === "pending"
    ) {
      changed = true;
      return {
        ...m,
        permission: { ...m.permission, decision: allow ? "allowed" : "denied" },
      } as ChatMessage;
    }
    return m;
  });
  return changed ? { ...state, messages } : state;
}

// attachToolResult binds a tool_result to the most recent activity still
// awaiting one (order-based matching, per design). If none is pending the
// result is dropped — it has no call to annotate.
function attachToolResult(state: ChatState, content: unknown): ChatState {
  for (let i = state.messages.length - 1; i >= 0; i--) {
    const m = state.messages[i];
    if (m.kind === "activity" && m.activity && !m.activity.hasResult) {
      const messages = state.messages.slice();
      messages[i] = {
        ...m,
        activity: {
          ...m.activity,
          hasResult: true,
          result: renderToolResultContent(content),
        },
      };
      return { ...state, messages };
    }
  }
  return state;
}

function applyEvent(state: ChatState, event: ServerEvent): ChatState {
  switch (event.type) {
    case "session_id":
      // Capture only a non-empty id; never overwrite with a blank.
      return event.sessionId
        ? { ...state, sessionId: event.sessionId }
        : state;

    case "text":
      return { ...state, responding: true, streaming: state.streaming + event.text };

    case "thinking":
      return { ...state, responding: true, thinking: state.thinking + event.text };

    case "done": {
      const withId = event.sessionId
        ? { ...state, sessionId: event.sessionId }
        : state;
      return finalizeStreaming(withId);
    }

    case "error": {
      const flushed = finalizeStreaming(state);
      return push(flushed, { kind: "error", text: "오류: " + event.message });
    }

    case "aborted": {
      const flushed = finalizeStreaming(state);
      return push(flushed, { kind: "system", text: "응답이 중단되었습니다." });
    }

    case "peer_chat":
      return push(state, {
        kind: "peer",
        name: speakerName(event.from),
        text: event.text,
      });

    case "host_status":
      // Idempotent: repeated status with the same value is a no-op, so React
      // sees a stable reference and skips the re-render.
      return state.hostUp === event.connected
        ? state
        : { ...state, hostUp: event.connected };

    case "unavailable":
      return push(
        { ...state, responding: false },
        {
          kind: "system",
          text: "호스트가 연결되어 있지 않습니다 — 노트북 데몬이 실행 중인지 확인하세요.",
        },
      );

    case "tool_call": {
      // Flush any Claude preamble first so the feed reads in wire order, then
      // append the collapsed activity line. Still mid-turn: keep responding.
      const flushed = flushStreaming(state, true);
      return push(flushed, {
        kind: "activity",
        text: "",
        activity: { name: event.name, input: event.input, hasResult: false },
      });
    }

    case "tool_result":
      return attachToolResult(state, event.content);

    case "permission_request":
      return push(state, {
        kind: "permission",
        text: "",
        permission: {
          requestId: event.requestId,
          toolName: event.toolName,
          input: event.input,
          decision: "pending",
        },
      });
  }
}
