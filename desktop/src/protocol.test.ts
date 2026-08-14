import { describe, it, expect } from "vitest";
import {
  parseServerEvent,
  speakerName,
  buildChat,
  buildPermissionResponse,
  toolPreview,
  summarizeJson,
  renderToolResultContent,
  chatReducer,
  initialChatState,
  parseTerminalEvent,
  peekRoomMode,
  buildPtyInput,
  buildPtyScroll,
  isScrollSeq,
  buildPtyResize,
  buildSetDriver,
  buildRoomChat,
  buildDriverHeartbeat,
  buildDriverRequest,
  base64ToBytes,
  shouldSendInput,
  keyToSequence,
  cursorPx,
  subFromToken,
  accessFromToken,
  roomFromToken,
  executionTargetFromToken,
  deviceIdFromToken,
  relaySessionIdFromToken,
  type ChatState,
  type ServerEvent,
} from "./protocol";

// Convenience: drive a sequence of parsed events through the reducer.
function run(events: ServerEvent[], start: ChatState = initialChatState()): ChatState {
  return events.reduce((s, event) => chatReducer(s, { kind: "event", event }), start);
}

describe("parseServerEvent — wire → event mapping", () => {
  it("maps each known event type", () => {
    expect(parseServerEvent('{"type":"session_id","sessionId":"s1"}')).toEqual({
      type: "session_id",
      sessionId: "s1",
    });
    expect(parseServerEvent('{"type":"text","text":"hi"}')).toEqual({
      type: "text",
      text: "hi",
    });
    expect(parseServerEvent('{"type":"thinking","text":"hmm"}')).toEqual({
      type: "thinking",
      text: "hmm",
    });
    expect(parseServerEvent('{"type":"done","sessionId":"s2"}')).toEqual({
      type: "done",
      sessionId: "s2",
    });
    expect(parseServerEvent('{"type":"error","message":"boom"}')).toEqual({
      type: "error",
      message: "boom",
    });
    expect(parseServerEvent('{"type":"aborted"}')).toEqual({ type: "aborted" });
    expect(parseServerEvent('{"type":"peer_chat","from":"guest:bob-x7k2","text":"yo"}')).toEqual({
      type: "peer_chat",
      from: "guest:bob-x7k2",
      text: "yo",
    });
    expect(parseServerEvent('{"type":"agent:unavailable","reason":"no-agent-connected"}')).toEqual({
      type: "unavailable",
      reason: "no-agent-connected",
    });
  });

  it("folds host:status and agent:status into one host_status event", () => {
    expect(parseServerEvent('{"type":"host:status","connected":true}')).toEqual({
      type: "host_status",
      connected: true,
    });
    expect(parseServerEvent('{"type":"agent:status","connected":false}')).toEqual({
      type: "host_status",
      connected: false,
    });
  });

  it("parses tool_call and tool_result activity events", () => {
    expect(
      parseServerEvent('{"type":"tool_call","name":"Bash","input":{"command":"go test ./..."}}'),
    ).toEqual({ type: "tool_call", name: "Bash", input: { command: "go test ./..." } });
    // A tool_call with no name still parses (renders as a bare wrench line).
    expect(parseServerEvent('{"type":"tool_call"}')).toEqual({
      type: "tool_call",
      name: "",
      input: undefined,
    });
    expect(parseServerEvent('{"type":"tool_result","content":"ok\\n"}')).toEqual({
      type: "tool_result",
      content: "ok\n",
    });
  });

  it("parses permission_request but drops one without a requestId", () => {
    expect(
      parseServerEvent(
        '{"type":"permission_request","requestId":"r1","toolName":"Write","input":{"file_path":"/a"}}',
      ),
    ).toEqual({
      type: "permission_request",
      requestId: "r1",
      toolName: "Write",
      input: { file_path: "/a" },
    });
    // No requestId → unanswerable → null.
    expect(parseServerEvent('{"type":"permission_request","toolName":"Write"}')).toBeNull();
  });

  it("returns null for unknown types, non-objects, and malformed JSON", () => {
    expect(parseServerEvent('{"type":"ready"}')).toBeNull();
    expect(parseServerEvent('{"type":"session_status"}')).toBeNull();
    expect(parseServerEvent("not json")).toBeNull();
    expect(parseServerEvent("null")).toBeNull();
    expect(parseServerEvent("42")).toBeNull();
  });

  it("defaults missing fields rather than throwing", () => {
    expect(parseServerEvent('{"type":"text"}')).toEqual({ type: "text", text: "" });
    expect(parseServerEvent('{"type":"session_id"}')).toEqual({
      type: "session_id",
      sessionId: "",
    });
    expect(parseServerEvent('{"type":"host:status"}')).toEqual({
      type: "host_status",
      connected: false,
    });
  });
});

describe("speakerName — guest sub → label", () => {
  it("extracts the name from guest:<name>-<rand>", () => {
    expect(speakerName("guest:bob-x7k2")).toBe("bob");
  });
  it("keeps names that themselves contain dashes", () => {
    expect(speakerName("guest:mary-jane-9f3a")).toBe("mary-jane");
  });
  it("shows non-guest subs verbatim and handles empty", () => {
    expect(speakerName("host")).toBe("host");
    expect(speakerName("")).toBe("?");
  });
});

describe("buildChat — outbound frame", () => {
  it("never includes a `from` field and uses `prompt`", () => {
    const obj = JSON.parse(buildChat("hello", ""));
    expect(obj).toEqual({ type: "chat", prompt: "hello" });
    expect(obj).not.toHaveProperty("from");
  });
  it("includes sessionId only when known", () => {
    expect(JSON.parse(buildChat("hi", "s9"))).toEqual({
      type: "chat",
      prompt: "hi",
      sessionId: "s9",
    });
  });
});

describe("buildPtyScroll / isScrollSeq — shared scroll forwarding", () => {
  it("buildPtyScroll wraps the sequence in a pty_scroll frame (no from)", () => {
    const obj = JSON.parse(buildPtyScroll("\x1b[<64;1;1M"));
    expect(obj).toEqual({ type: "pty_scroll", data: "\x1b[<64;1;1M" });
    expect(obj).not.toHaveProperty("from");
  });

  it("recognizes SGR mouse wheel (buttons carrying the 0x40 bit)", () => {
    expect(isScrollSeq("\x1b[<64;10;5M")).toBe(true); // wheel up
    expect(isScrollSeq("\x1b[<65;10;5M")).toBe(true); // wheel down
    expect(isScrollSeq("\x1b[<69;10;5M")).toBe(true); // wheel up + shift (64|4|1)
    expect(isScrollSeq("\x1b[<0;10;5M")).toBe(false); // left click — NOT scroll
    expect(isScrollSeq("\x1b[<2;10;5M")).toBe(false); // right click — NOT scroll
    expect(isScrollSeq("\x1b[<64;10;5Mwhoami\n")).toBe(false); // valid prefix + input
  });

  it("recognizes legacy X10 mouse wheel (Cb 0x60/0x61) but not clicks", () => {
    expect(isScrollSeq("\x1b[M\x60!!")).toBe(true); // Cb=0x60 → wheel up
    expect(isScrollSeq("\x1b[M\x61!!")).toBe(true); // Cb=0x61 → wheel down
    expect(isScrollSeq("\x1b[M\x20!!")).toBe(false); // Cb=0x20 → button 0 press
  });

  it("recognizes alternate-scroll cursor keys, and rejects plain input", () => {
    expect(isScrollSeq("\x1bOA")).toBe(true);
    expect(isScrollSeq("\x1b[B")).toBe(true);
    expect(isScrollSeq("a")).toBe(false);
    expect(isScrollSeq("\x1b[C")).toBe(false); // right arrow is not vertical scroll
    expect(isScrollSeq("\x1b[M`!!whoami")).toBe(false); // oversized X10-like payload
    expect(isScrollSeq("")).toBe(false);
  });
});

describe("buildPermissionResponse — outbound approval", () => {
  it("serializes an allow decision", () => {
    expect(JSON.parse(buildPermissionResponse("r1", true))).toEqual({
      type: "permission_response",
      requestId: "r1",
      allow: true,
    });
  });
  it("serializes a deny decision", () => {
    expect(JSON.parse(buildPermissionResponse("r2", false))).toEqual({
      type: "permission_response",
      requestId: "r2",
      allow: false,
    });
  });
});

describe("tool presentation helpers", () => {
  it("toolPreview picks a sensible single-line body", () => {
    expect(toolPreview({ command: "go test ./..." })).toBe("go test ./...");
    expect(toolPreview({ file_path: "/tmp/a.txt" })).toBe("/tmp/a.txt");
    expect(toolPreview("just a string")).toBe("just a string");
    expect(toolPreview({ nothing: 1 })).toBe("");
    expect(toolPreview(undefined)).toBe("");
  });
  it("toolPreview collapses whitespace and clips long values", () => {
    expect(toolPreview({ command: "a\n  b\tc" })).toBe("a b c");
    const long = "x".repeat(200);
    expect(toolPreview({ command: long })).toHaveLength(121); // 120 + ellipsis
  });
  it("summarizeJson pretty-prints and tolerates undefined", () => {
    expect(summarizeJson({ a: 1 })).toBe('{\n  "a": 1\n}');
    expect(summarizeJson(undefined)).toBe("");
  });
  it("renderToolResultContent flattens strings and block arrays", () => {
    expect(renderToolResultContent("ok")).toBe("ok");
    expect(
      renderToolResultContent([{ type: "text", text: "line1" }, { type: "text", text: "line2" }]),
    ).toBe("line1\nline2");
    expect(renderToolResultContent(null)).toBe("");
  });
});

describe("chatReducer — streaming accumulation → done", () => {
  it("accumulates text deltas and finalizes one Claude line on done", () => {
    const s = run([
      { type: "text", text: "Hel" },
      { type: "text", text: "lo " },
      { type: "text", text: "world" },
    ]);
    expect(s.responding).toBe(true);
    expect(s.streaming).toBe("Hello world");
    expect(s.messages).toHaveLength(0); // nothing finalized yet

    const done = chatReducer(s, { kind: "event", event: { type: "done", sessionId: "" } });
    expect(done.responding).toBe(false);
    expect(done.streaming).toBe("");
    expect(done.messages).toEqual([
      expect.objectContaining({ kind: "claude", text: "Hello world" }),
    ]);
  });

  it("does not emit a blank Claude line when nothing streamed", () => {
    const s = run([{ type: "done", sessionId: "" }]);
    expect(s.messages).toHaveLength(0);
    expect(s.responding).toBe(false);
  });

  it("error and aborted finalize the stream then append a system/error line", () => {
    const err = run([
      { type: "text", text: "partial" },
      { type: "error", message: "boom" },
    ]);
    expect(err.messages).toEqual([
      expect.objectContaining({ kind: "claude", text: "partial" }),
      expect.objectContaining({ kind: "error", text: "오류: boom" }),
    ]);
    expect(err.responding).toBe(false);

    const ab = run([{ type: "aborted" }]);
    expect(ab.messages).toEqual([
      expect.objectContaining({ kind: "system", text: "응답이 중단되었습니다." }),
    ]);
  });
});

describe("chatReducer — sessionId capture", () => {
  it("captures the id from the first session_id event", () => {
    const s = run([{ type: "session_id", sessionId: "abc" }]);
    expect(s.sessionId).toBe("abc");
  });

  it("captures the id from done as well", () => {
    const s = run([{ type: "done", sessionId: "xyz" }]);
    expect(s.sessionId).toBe("xyz");
  });

  it("never overwrites a captured id with a blank value", () => {
    const s = run([
      { type: "session_id", sessionId: "abc" },
      { type: "session_id", sessionId: "" },
      { type: "done", sessionId: "" },
    ]);
    expect(s.sessionId).toBe("abc");
  });
});

describe("chatReducer — host:status idempotency", () => {
  it("returns the same reference when the connected value is unchanged", () => {
    const up = run([{ type: "host_status", connected: true }]);
    const again = chatReducer(up, {
      kind: "event",
      event: { type: "host_status", connected: true },
    });
    expect(again).toBe(up); // identical reference → React skips re-render
    expect(again.hostUp).toBe(true);
  });

  it("flips presence when the value changes", () => {
    const s = run([
      { type: "host_status", connected: true },
      { type: "host_status", connected: false },
    ]);
    expect(s.hostUp).toBe(false);
  });
});

describe("chatReducer — peer_chat name extraction", () => {
  it("labels a peer message with the extracted guest name", () => {
    const s = run([
      { type: "peer_chat", from: "guest:bob-x7k2", text: "안녕" },
    ]);
    expect(s.messages).toEqual([
      expect.objectContaining({ kind: "peer", name: "bob", text: "안녕" }),
    ]);
  });
});

describe("chatReducer — local actions", () => {
  it("send appends my line and marks responding", () => {
    const s = chatReducer(initialChatState(), {
      kind: "send",
      name: "me",
      text: "질문",
    });
    expect(s.responding).toBe(true);
    expect(s.messages).toEqual([
      expect.objectContaining({ kind: "me", name: "me", text: "질문" }),
    ]);
  });

  it("assigns unique ids across mixed actions", () => {
    let s = initialChatState();
    s = chatReducer(s, { kind: "send", name: "me", text: "a" });
    s = chatReducer(s, { kind: "system", text: "b" });
    s = chatReducer(s, {
      kind: "event",
      event: { type: "peer_chat", from: "guest:x-1", text: "c" },
    });
    const ids = s.messages.map((m) => m.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

describe("chatReducer — activity feed", () => {
  it("appends a collapsed activity line on tool_call, flushing preamble first", () => {
    const s = run([
      { type: "text", text: "먼저 테스트를 돌릴게요. " },
      { type: "tool_call", name: "Bash", input: { command: "go test ./..." } },
    ]);
    // Claude preamble flushed to its own line, then the activity line, in order.
    expect(s.messages).toEqual([
      expect.objectContaining({ kind: "claude", text: "먼저 테스트를 돌릴게요." }),
      expect.objectContaining({
        kind: "activity",
        activity: expect.objectContaining({ name: "Bash", hasResult: false }),
      }),
    ]);
    expect(s.streaming).toBe("");
    expect(s.responding).toBe(true); // still mid-turn
  });

  it("attaches a tool_result to the latest pending tool_call (order-based)", () => {
    const s = run([
      { type: "tool_call", name: "Bash", input: { command: "ls" } },
      { type: "tool_call", name: "Read", input: { file_path: "/a" } },
      { type: "tool_result", content: "read output" },
    ]);
    const acts = s.messages.filter((m) => m.kind === "activity");
    // Second (most recent pending) call gets the result; the first stays open.
    expect(acts[0].activity).toEqual(expect.objectContaining({ name: "Bash", hasResult: false }));
    expect(acts[1].activity).toEqual(
      expect.objectContaining({ name: "Read", hasResult: true, result: "read output" }),
    );
  });

  it("drops a tool_result with no pending call", () => {
    const s = run([{ type: "tool_result", content: "orphan" }]);
    expect(s.messages).toHaveLength(0);
  });

  it("keeps activity lines when the turn finalizes on done", () => {
    const s = run([
      { type: "tool_call", name: "Bash", input: { command: "ls" } },
      { type: "tool_result", content: "a\nb" },
      { type: "text", text: "완료했습니다." },
      { type: "done", sessionId: "s1" },
    ]);
    expect(s.messages.map((m) => m.kind)).toEqual(["activity", "claude"]);
    expect(s.responding).toBe(false);
  });
});

describe("chatReducer — permission cards", () => {
  it("appends a pending card on permission_request", () => {
    const s = run([
      {
        type: "permission_request",
        requestId: "r1",
        toolName: "Write",
        input: { file_path: "/a" },
      },
    ]);
    expect(s.messages).toEqual([
      expect.objectContaining({
        kind: "permission",
        permission: expect.objectContaining({ requestId: "r1", decision: "pending" }),
      }),
    ]);
  });

  it("supports several concurrent cards and decides each by requestId", () => {
    let s = run([
      { type: "permission_request", requestId: "r1", toolName: "Write", input: {} },
      { type: "permission_request", requestId: "r2", toolName: "Bash", input: {} },
    ]);
    s = chatReducer(s, { kind: "permission_decide", requestId: "r2", allow: false });
    s = chatReducer(s, { kind: "permission_decide", requestId: "r1", allow: true });
    const cards = s.messages.filter((m) => m.kind === "permission").map((m) => m.permission);
    expect(cards).toEqual([
      expect.objectContaining({ requestId: "r1", decision: "allowed" }),
      expect.objectContaining({ requestId: "r2", decision: "denied" }),
    ]);
  });

  it("ignores a decision for an unknown or already-decided card (stable reference)", () => {
    const s = run([
      { type: "permission_request", requestId: "r1", toolName: "Write", input: {} },
    ]);
    const decided = chatReducer(s, { kind: "permission_decide", requestId: "r1", allow: true });
    const again = chatReducer(decided, { kind: "permission_decide", requestId: "r1", allow: false });
    expect(again).toBe(decided); // no change → same reference
    const unknown = chatReducer(decided, { kind: "permission_decide", requestId: "zzz", allow: true });
    expect(unknown).toBe(decided);
  });
});

// ---------------------------------------------------------------------------
// Terminal room (Phase 5)
// ---------------------------------------------------------------------------

describe("parseTerminalEvent — terminal wire → event", () => {
  it("maps each terminal frame type", () => {
    expect(parseTerminalEvent('{"type":"room_mode","mode":"terminal"}')).toEqual({
      type: "room_mode",
      mode: "terminal",
    });
    expect(parseTerminalEvent('{"type":"pty_output","data":"aGk="}')).toEqual({
      type: "pty_output",
      data: "aGk=",
    });
    expect(
      parseTerminalEvent(
        '{"type":"pty_output","data":"eA==","streamId":"terminal","incarnationId":"i-1","seq":7}',
      ),
    ).toEqual({
      type: "pty_output",
      data: "eA==",
      streamId: "terminal",
      incarnationId: "i-1",
      seq: 7,
    });
    expect(
      parseTerminalEvent(
        '{"type":"pty_snapshot","data":"c25hcA==","incarnationId":"i-1","seq":7,"cols":120,"rows":34}',
      ),
    ).toEqual({
      type: "pty_snapshot",
      data: "c25hcA==",
      incarnationId: "i-1",
      seq: 7,
      cols: 120,
      rows: 34,
    });
    expect(parseTerminalEvent('{"type":"pty_exit","code":0}')).toEqual({
      type: "pty_exit",
      code: 0,
    });
    expect(parseTerminalEvent('{"type":"driver","from":"guest:bob-x7k2"}')).toEqual({
      type: "driver",
      from: "guest:bob-x7k2",
    });
  });

  it("normalizes missing fields and a driver revoke", () => {
    expect(parseTerminalEvent('{"type":"pty_output"}')).toEqual({ type: "pty_output", data: "" });
    expect(parseTerminalEvent('{"type":"pty_exit"}')).toEqual({ type: "pty_exit", code: null });
    expect(parseTerminalEvent('{"type":"driver","from":""}')).toEqual({ type: "driver", from: "" });
  });

  it("parses driver lease, request, and participant roster frames", () => {
    expect(
      parseTerminalEvent(
        '{"type":"driver_state","driver":"alice","generation":3,"expiresAt":123}',
      ),
    ).toEqual({ type: "driver_state", driver: "alice", generation: 3, expiresAt: 123 });
    expect(parseTerminalEvent('{"type":"driver_request","from":"bob"}')).toEqual({
      type: "driver_request",
      from: "bob",
    });
    expect(
      parseTerminalEvent(
        '{"type":"participant_roster","driver":"alice","generation":3,"participants":[{"userId":"alice","role":"host","access":"control","connectionId":"c1","connectedAt":99},{"bad":true}]}',
      ),
    ).toEqual({
      type: "participant_roster",
      driver: "alice",
      generation: 3,
      participants: [
        {
          userId: "alice",
          role: "host",
          access: "control",
          connectionId: "c1",
          connectedAt: 99,
        },
      ],
    });
  });

  it("rejects an unknown room_mode value", () => {
    expect(parseTerminalEvent('{"type":"room_mode","mode":"bogus"}')).toBeNull();
  });

  it("parses a valid pty_size frame (authoritative grid size)", () => {
    expect(parseTerminalEvent('{"type":"pty_size","cols":120,"rows":34}')).toEqual({
      type: "pty_size",
      cols: 120,
      rows: 34,
    });
  });

  it("drops an invalid pty_size (missing, zero, negative, or non-numeric)", () => {
    // An invalid size must never corrupt the grid — every one of these is null.
    expect(parseTerminalEvent('{"type":"pty_size","rows":34}')).toBeNull(); // cols missing
    expect(parseTerminalEvent('{"type":"pty_size","cols":120}')).toBeNull(); // rows missing
    expect(parseTerminalEvent('{"type":"pty_size"}')).toBeNull(); // both missing
    expect(parseTerminalEvent('{"type":"pty_size","cols":0,"rows":34}')).toBeNull(); // zero
    expect(parseTerminalEvent('{"type":"pty_size","cols":120,"rows":0}')).toBeNull(); // zero
    expect(parseTerminalEvent('{"type":"pty_size","cols":-5,"rows":34}')).toBeNull(); // negative
    expect(parseTerminalEvent('{"type":"pty_size","cols":120,"rows":-1}')).toBeNull(); // negative
    expect(parseTerminalEvent('{"type":"pty_size","cols":"80","rows":24}')).toBeNull(); // non-numeric
    expect(parseTerminalEvent('{"type":"pty_size","cols":null,"rows":24}')).toBeNull(); // non-numeric
  });

  it("ignores chat frames and malformed JSON", () => {
    expect(parseTerminalEvent('{"type":"text","text":"hi"}')).toBeNull();
    expect(parseTerminalEvent("not json")).toBeNull();
  });

  it("parses an inbound room_chat side-chat frame (from + text)", () => {
    expect(
      parseTerminalEvent('{"type":"room_chat","from":"guest:bob-x7k2","text":"안녕"}'),
    ).toEqual({ type: "room_chat", from: "guest:bob-x7k2", text: "안녕" });
  });

  it("defaults missing room_chat fields to empty strings", () => {
    expect(parseTerminalEvent('{"type":"room_chat"}')).toEqual({
      type: "room_chat",
      from: "",
      text: "",
    });
    expect(parseTerminalEvent('{"type":"room_chat","text":"hi"}')).toEqual({
      type: "room_chat",
      from: "",
      text: "hi",
    });
  });
});

describe("peekRoomMode — terminal detection without the chat reducer", () => {
  it("returns the mode only for a room_mode frame", () => {
    expect(peekRoomMode('{"type":"room_mode","mode":"terminal"}')).toBe("terminal");
    expect(peekRoomMode('{"type":"room_mode","mode":"chat"}')).toBe("chat");
    expect(peekRoomMode('{"type":"pty_output","data":"x"}')).toBeNull();
    expect(peekRoomMode('{"type":"text","text":"hi"}')).toBeNull();
    expect(peekRoomMode("nope")).toBeNull();
  });
});

describe("terminal outbound builders", () => {
  it("buildPtyInput sends raw utf-8 data and no from", () => {
    expect(JSON.parse(buildPtyInput("ls\r"))).toEqual({ type: "pty_input", data: "ls\r" });
  });

  it("buildPtyResize carries cols/rows", () => {
    expect(JSON.parse(buildPtyResize(120, 34))).toEqual({
      type: "pty_resize",
      cols: 120,
      rows: 34,
    });
  });

  it("buildSetDriver carries the target under both keys (design + relay-test)", () => {
    // Emits `target` (design-doc field) and `driver` (the relay's S5-1 test
    // example field) so the pty-host reads it whichever key it settled on.
    expect(JSON.parse(buildSetDriver("guest:bob-x7k2"))).toEqual({
      type: "set_driver",
      target: "guest:bob-x7k2",
      driver: "guest:bob-x7k2",
    });
    // Revoke → empty target.
    expect(JSON.parse(buildSetDriver(""))).toEqual({
      type: "set_driver",
      target: "",
      driver: "",
    });
  });

  it("buildRoomChat sends only type+text and never a from (relay injects it)", () => {
    const obj = JSON.parse(buildRoomChat("안녕하세요"));
    expect(obj).toEqual({ type: "room_chat", text: "안녕하세요" });
    expect(obj).not.toHaveProperty("from");
  });

  it("buildRoomChat preserves the exact text (whitespace/newlines)", () => {
    expect(JSON.parse(buildRoomChat("line1\nline2"))).toEqual({
      type: "room_chat",
      text: "line1\nline2",
    });
  });

  it("builds driver lease coordination frames without client-supplied identity", () => {
    expect(JSON.parse(buildDriverHeartbeat())).toEqual({ type: "driver_heartbeat" });
    expect(JSON.parse(buildDriverRequest())).toEqual({ type: "driver_request" });
  });
});

describe("base64ToBytes — binary-safe PTY decode", () => {
  it("decodes ascii", () => {
    expect(Array.from(base64ToBytes("aGk="))).toEqual([104, 105]); // "hi"
  });

  it("preserves ANSI escape bytes", () => {
    // ESC [ 3 1 m  → 0x1b 0x5b 0x33 0x31 0x6d
    const b64 = btoa("\x1b[31m");
    expect(Array.from(base64ToBytes(b64))).toEqual([0x1b, 0x5b, 0x33, 0x31, 0x6d]);
  });

  it("returns empty bytes for malformed base64 instead of throwing", () => {
    expect(base64ToBytes("!!!not base64!!!").length).toBe(0);
  });
});

describe("shouldSendInput — driver gate", () => {
  it("transmits only when I am the current driver", () => {
    expect(shouldSendInput("guest:bob-x7k2", "guest:bob-x7k2")).toBe(true);
    expect(shouldSendInput("guest:ann-99zz", "guest:bob-x7k2")).toBe(false); // someone else
    expect(shouldSendInput("", "guest:bob-x7k2")).toBe(false); // nobody driving
    expect(shouldSendInput("guest:bob-x7k2", "")).toBe(false); // unknown self
    expect(shouldSendInput("", "")).toBe(false);
  });
});

describe("subFromToken — JWT sub extraction", () => {
  function jwt(sub: string): string {
    const payload = btoa(JSON.stringify({ sub }))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
    return `h.${payload}.s`;
  }

  it("reads the sub claim from a participant token", () => {
    expect(subFromToken(jwt("guest:bob-x7k2"))).toBe("guest:bob-x7k2");
  });

  it("returns '' for a non-JWT or a token without a sub", () => {
    expect(subFromToken("not-a-jwt")).toBe("");
    expect(subFromToken(jwt(""))).toBe(""); // present but empty → treated as unknown-ish
    expect(subFromToken("")).toBe("");
  });
});

describe("accessFromToken — request-control presentation", () => {
  function jwtWith(claims: Record<string, unknown>): string {
    const payload = btoa(JSON.stringify(claims))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
    return `h.${payload}.s`;
  }

  it("reads view and control while preserving legacy-control behavior", () => {
    expect(accessFromToken(jwtWith({ access: "view" }))).toBe("view");
    expect(accessFromToken(jwtWith({ access: "control" }))).toBe("control");
    expect(accessFromToken(jwtWith({ sub: "legacy" }))).toBe("control");
    expect(accessFromToken("not-a-jwt")).toBe("");
  });
});

describe("roomFromToken — host token room claim", () => {
  function jwtWith(claims: Record<string, unknown>): string {
    const payload = btoa(JSON.stringify(claims))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
    return `h.${payload}.s`;
  }

  it("reads the room claim when present", () => {
    expect(roomFromToken(jwtWith({ room: "r-dev", sub: "host:alice" }))).toBe("r-dev");
  });

  it("falls back to sub when room is absent", () => {
    expect(roomFromToken(jwtWith({ sub: "host:alice" }))).toBe("host:alice");
  });

  it("falls back to sub when room is present but empty", () => {
    expect(roomFromToken(jwtWith({ room: "", sub: "host:alice" }))).toBe("host:alice");
  });

  it("returns '' for a non-JWT or a token with neither claim", () => {
    expect(roomFromToken("not-a-jwt")).toBe("");
    expect(roomFromToken(jwtWith({}))).toBe("");
    expect(roomFromToken("")).toBe("");
  });
});

describe("scoped runtime token metadata", () => {
  const jwtWith = (claims: Record<string, unknown>) => {
    const payload = btoa(JSON.stringify(claims))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
    return `h.${payload}.s`;
  };

  it("reads the signed session target and scope for display", () => {
    const token = jwtWith({
      execution_target: "docker",
      device_id: "executor-owner",
      session_id: "session-a",
    });
    expect(executionTargetFromToken(token)).toBe("docker");
    expect(deviceIdFromToken(token)).toBe("executor-owner");
    expect(relaySessionIdFromToken(token)).toBe("session-a");
  });

  it("fails closed for unknown targets", () => {
    expect(executionTargetFromToken(jwtWith({ execution_target: "host" }))).toBeUndefined();
  });
});

describe("keyToSequence — keydown → terminal byte sequence", () => {
  // Helper: build a KeyEventLike with modifiers defaulting off.
  const k = (
    key: string,
    mods: { ctrlKey?: boolean; altKey?: boolean; metaKey?: boolean } = {},
  ) => ({ key, ctrlKey: false, altKey: false, metaKey: false, ...mods });

  it("maps the named non-printable keys to fixed sequences", () => {
    expect(keyToSequence(k("Enter"))).toBe("\r");
    expect(keyToSequence(k("Backspace"))).toBe("\x7f");
    expect(keyToSequence(k("Tab"))).toBe("\t");
    expect(keyToSequence(k("Escape"))).toBe("\x1b");
    expect(keyToSequence(k("Delete"))).toBe("\x1b[3~");
  });

  it("maps the arrow keys to CSI cursor sequences", () => {
    expect(keyToSequence(k("ArrowUp"))).toBe("\x1b[A");
    expect(keyToSequence(k("ArrowDown"))).toBe("\x1b[B");
    expect(keyToSequence(k("ArrowRight"))).toBe("\x1b[C");
    expect(keyToSequence(k("ArrowLeft"))).toBe("\x1b[D");
  });

  it("maps Home/End/PageUp/PageDown", () => {
    expect(keyToSequence(k("Home"))).toBe("\x1b[H");
    expect(keyToSequence(k("End"))).toBe("\x1b[F");
    expect(keyToSequence(k("PageUp"))).toBe("\x1b[5~");
    expect(keyToSequence(k("PageDown"))).toBe("\x1b[6~");
  });

  it("maps Ctrl+a..z to the C0 control codes 0x01..0x1a", () => {
    expect(keyToSequence(k("a", { ctrlKey: true }))).toBe("\x01");
    expect(keyToSequence(k("c", { ctrlKey: true }))).toBe("\x03"); // SIGINT
    expect(keyToSequence(k("d", { ctrlKey: true }))).toBe("\x04"); // EOF
    expect(keyToSequence(k("z", { ctrlKey: true }))).toBe("\x1a"); // SIGTSTP
  });

  it("maps Ctrl+letter case-insensitively (Shift+Ctrl+C still 0x03)", () => {
    expect(keyToSequence(k("C", { ctrlKey: true }))).toBe("\x03");
  });

  it("maps the other common Ctrl control chords", () => {
    expect(keyToSequence(k("[", { ctrlKey: true }))).toBe("\x1b"); // ESC
    expect(keyToSequence(k("]", { ctrlKey: true }))).toBe("\x1d");
    expect(keyToSequence(k("\\", { ctrlKey: true }))).toBe("\x1c");
    expect(keyToSequence(k(" ", { ctrlKey: true }))).toBe("\x00");
  });

  it("returns null for an ordinary character (handled by the input event)", () => {
    expect(keyToSequence(k("a"))).toBeNull();
    expect(keyToSequence(k("Z"))).toBeNull();
    expect(keyToSequence(k("1"))).toBeNull();
    expect(keyToSequence(k("한"))).toBeNull();
  });

  it("does not treat Cmd (meta) as a terminal chord — returns null", () => {
    expect(keyToSequence(k("c", { metaKey: true }))).toBeNull();
    expect(keyToSequence(k("c", { ctrlKey: true, metaKey: true }))).toBeNull();
  });

  it("leaves Alt/Option keys to the input event (no ESC-meta) — returns null", () => {
    expect(keyToSequence(k("b", { altKey: true }))).toBeNull();
    expect(keyToSequence(k("f", { altKey: true }))).toBeNull();
  });
});

describe("cursorPx — cursor cell → overlay pixel offset", () => {
  it("places the origin cell at the padding offset", () => {
    expect(
      cursorPx({ cursorX: 0, cursorY: 0, cellW: 8, cellH: 17, padL: 8, padT: 6 }),
    ).toEqual({ left: 8, top: 6 });
  });

  it("adds one cell width/height per column/row", () => {
    expect(
      cursorPx({ cursorX: 3, cursorY: 2, cellW: 8, cellH: 17, padL: 8, padT: 6 }),
    ).toEqual({ left: 8 + 24, top: 6 + 34 });
  });

  it("handles fractional cell sizes (measured screen px / cols)", () => {
    expect(
      cursorPx({ cursorX: 10, cursorY: 5, cellW: 7.5, cellH: 16.4, padL: 8, padT: 6 }),
    ).toEqual({ left: 8 + 75, top: 6 + 82 });
  });

  it("works with zero padding", () => {
    expect(
      cursorPx({ cursorX: 4, cursorY: 1, cellW: 9, cellH: 18, padL: 0, padT: 0 }),
    ).toEqual({ left: 36, top: 18 });
  });
});
