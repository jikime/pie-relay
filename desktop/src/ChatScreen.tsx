import { useEffect, useReducer, useRef, useState, useCallback } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Session } from "./App";
import {
  chatReducer,
  initialChatState,
  parseServerEvent,
  peekRoomMode,
  buildChat,
  buildPermissionResponse,
  toolPreview,
  summarizeJson,
  type ChatMessage,
  type RoomMode,
} from "./protocol";
import { RelayConnection, type ConnStatus } from "./connection";
import { participantTicketProtocol, participantWsEndpoint } from "./relay";

// RoomStatus is what a room reports up to the shell for its sidebar entry:
// host presence (the ●/○ dot) and whether the relay socket is live (a dead
// socket — e.g. an expired token on restore — renders as "연결 끊김").
export interface RoomStatus {
  hostUp: boolean;
  live: boolean;
}

interface Props {
  session: Session;
  onLeave: () => void;
  // P4 multi-room: when hidden the screen is display:none but stays mounted so
  // its socket keeps running. onActivity fires only while hidden (new activity
  // in a background room → App lights an unread badge). onStatus mirrors host
  // presence / socket liveness up to the sidebar. None of these touch the
  // socket lifecycle — the connection is owned solely by the effect below.
  hidden?: boolean;
  onActivity?: () => void;
  onStatus?: (status: RoomStatus) => void;
  // A room is mounted as a ChatScreen until classified. If the host announces a
  // room_mode (i.e. this is really a terminal room), we report it up so the
  // shell can swap in the TerminalScreen. peekRoomMode keeps this out of the
  // chat reducer entirely — terminal frames never enter the transcript.
  onMode?: (mode: RoomMode) => void;
}

// ChatScreen owns the live connection and the transcript. Event meaning is in
// protocol.ts (pure reducer); this component wires the socket to the reducer,
// renders the transcript, and drives the input box.
export function ChatScreen({
  session,
  onLeave,
  hidden = false,
  onActivity,
  onStatus,
  onMode,
}: Props) {
  const [state, dispatch] = useReducer(
    chatReducer,
    undefined,
    initialChatState,
  );
  const [status, setStatus] = useState<ConnStatus>("connecting");
  const [draft, setDraft] = useState("");
  const connRef = useRef<RelayConnection | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);

  // Open the socket once per session and tear it down on unmount / leave.
  useEffect(() => {
    const url = participantWsEndpoint(session.wsUrl);
    const conn = new RelayConnection(
      url,
      {
        onMessage: (raw) => {
          // Notice a terminal room's room_mode announcement before the chat
          // parser (which ignores it) and hand off to the shell for the swap.
          const mode = peekRoomMode(raw);
          if (mode) onMode?.(mode);
          const event = parseServerEvent(raw);
          if (event) dispatch({ kind: "event", event });
        },
        onStatus: (s) => {
          setStatus(s);
          // A dropped socket means we no longer know host presence; reflect it as
          // down until the relay pushes a fresh host:status after reconnect.
          if (
            s === "reconnecting" ||
            s === "connecting" ||
            s === "auth-expired"
          ) {
            dispatch({
              kind: "event",
              event: { type: "host_status", connected: false },
            });
          }
        },
      },
      [participantTicketProtocol(session.token)],
    );
    connRef.current = conn;
    conn.start();
    return () => conn.close();
  }, [session.wsUrl, session.token]);

  // Keep the transcript pinned to the newest content.
  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [state.messages, state.streaming, state.thinking, state.responding]);

  // Report host presence / socket liveness to the shell for the sidebar entry.
  // Runs on every change; App ignores no-op updates so this can't loop.
  useEffect(() => {
    onStatus?.({ hostUp: state.hostUp, live: status === "open" });
  }, [state.hostUp, status, onStatus]);

  // Notify the shell when a *hidden* room sees new activity (a finalized
  // message — assistant/peer/tool/permission — or the start of a response),
  // so App can raise an unread badge. Never fires for the active room.
  const prevLen = useRef(state.messages.length);
  const prevResponding = useRef(state.responding);
  useEffect(() => {
    const grew = state.messages.length > prevLen.current;
    const started = state.responding && !prevResponding.current;
    prevLen.current = state.messages.length;
    prevResponding.current = state.responding;
    if (hidden && (grew || started)) onActivity?.();
  }, [state.messages.length, state.responding, hidden, onActivity]);

  const sendDraft = useCallback(() => {
    const text = draft.trim();
    if (!text) return;
    // Group chat: sending mid-response is allowed. If the socket is momentarily
    // down, surface a system hint instead of silently dropping.
    const ok = connRef.current?.send(buildChat(text, state.sessionId)) ?? false;
    if (!ok) {
      dispatch({
        kind: "system",
        text: "연결이 끊겨 전송하지 못했습니다. 재접속을 기다리는 중…",
      });
      return;
    }
    dispatch({ kind: "send", name: session.name, text });
    setDraft("");
  }, [draft, state.sessionId, session.name]);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
      // Enter sends; Shift+Enter inserts a newline.
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        sendDraft();
      }
    },
    [sendDraft],
  );

  // Approve/deny a pending tool request. Send first; only mark the card decided
  // if the frame actually went out, so a dropped socket doesn't lock the card.
  const decidePermission = useCallback((requestId: string, allow: boolean) => {
    const ok =
      connRef.current?.send(buildPermissionResponse(requestId, allow)) ?? false;
    if (!ok) {
      dispatch({
        kind: "system",
        text: "연결이 끊겨 승인 응답을 보내지 못했습니다.",
      });
      return;
    }
    dispatch({ kind: "permission_decide", requestId, allow });
  }, []);

  const disconnected = status !== "open";

  return (
    <div className={"screen chat" + (hidden ? " hidden" : "")}>
      <header className="topbar">
        <div className="room">
          <button className="leave" onClick={onLeave} title="나가기">
            ←
          </button>
          <span className="room-name">
            {session.label || session.room || "방"}
          </span>
          <span className="me-name">나: {session.name}</span>
          {session.asHost && <span className="host-tag">호스트</span>}
        </div>
        <div className="topbar-right">
          {state.sessionId && <SessionChip sessionId={state.sessionId} />}
          <HostBadge up={state.hostUp} />
        </div>
      </header>

      {disconnected && (
        <div className="banner" role="status">
          {status === "closed"
            ? "연결이 종료되었습니다."
            : status === "auth-expired"
              ? "세션이 만료되었거나 거부되었습니다 — 방을 닫고 다시 참가하세요."
              : "릴레이 연결이 끊어졌습니다 — 자동으로 다시 연결하는 중…"}
        </div>
      )}

      <div className="transcript" ref={scrollRef}>
        {state.messages.map((m) => (
          <MessageRow key={m.id} msg={m} onDecide={decidePermission} />
        ))}
        {state.thinking && <ThinkingRow text={state.thinking} />}
        {state.responding && (
          <div className="row claude">
            <span className="who">Claude</span>
            <div className="text streaming">
              {state.streaming ? (
                <Markdown text={state.streaming} />
              ) : (
                <span className="muted">…</span>
              )}
              <span className="cursor" />
            </div>
          </div>
        )}
      </div>

      <div className="composer">
        <textarea
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="메시지를 입력하고 Enter (Shift+Enter 줄바꿈)"
          rows={2}
        />
        <button className="send" onClick={sendDraft} disabled={!draft.trim()}>
          전송
        </button>
      </div>
    </div>
  );
}

function HostBadge({ up }: { up: boolean }) {
  return (
    <span className={"host-badge " + (up ? "up" : "down")}>
      <span className="dot">{up ? "●" : "○"}</span>
      {up ? "호스트 연결" : "호스트 끊김"}
    </span>
  );
}

// SessionChip surfaces the captured Claude session id so it can be resumed from
// a terminal. Relay/SDK sessions are saved to disk and are resumable by id
// (`claude --resume <id>`), but the SDK tags them entrypoint=sdk-cli, which the
// interactive `--resume` picker hides — so the id must be carried over by hand.
// Two copy actions: the bare id, and a ready-to-paste resume command.
function SessionChip({ sessionId }: { sessionId: string }) {
  const [open, setOpen] = useState(false);
  const [copied, setCopied] = useState<"id" | "cmd" | null>(null);
  const short = sessionId.slice(0, 8);
  const cmd = `claude --resume ${sessionId}`;

  const copy = useCallback(async (what: "id" | "cmd", text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(what);
      setTimeout(() => setCopied(null), 1500);
    } catch {
      /* clipboard blocked — non-fatal */
    }
  }, []);

  return (
    <div className="session-chip">
      <button
        className="session-chip-toggle"
        onClick={() => setOpen((v) => !v)}
        title="세션 ID — 터미널에서 이어가기"
      >
        세션 {short}…
      </button>
      {open && (
        <div className="session-chip-pop" role="dialog">
          <div className="session-chip-label">
            이 대화를 터미널에서 이어가기
          </div>
          <code className="session-chip-id">{sessionId}</code>
          <div className="session-chip-actions">
            <button onClick={() => copy("id", sessionId)}>
              {copied === "id" ? "복사됨 ✓" : "ID 복사"}
            </button>
            <button onClick={() => copy("cmd", cmd)}>
              {copied === "cmd" ? "복사됨 ✓" : "명령 복사"}
            </button>
          </div>
          <div className="session-chip-hint">
            호스트 작업 디렉토리에서 <code>{cmd}</code> 실행
          </div>
        </div>
      )}
    </div>
  );
}

function MessageRow({
  msg,
  onDecide,
}: {
  msg: ChatMessage;
  onDecide: (requestId: string, allow: boolean) => void;
}) {
  switch (msg.kind) {
    case "me":
      return (
        <div className="row me">
          <span className="text">{msg.text}</span>
        </div>
      );
    case "peer":
      return (
        <div className="row peer">
          <span className="who">{msg.name}</span>
          <span className="text">{msg.text}</span>
        </div>
      );
    case "claude":
      return (
        <div className="row claude">
          <span className="who">Claude</span>
          <div className="text">
            <Markdown text={msg.text} />
          </div>
        </div>
      );
    case "activity":
      return msg.activity ? <ActivityRow activity={msg.activity} /> : null;
    case "permission":
      return msg.permission ? (
        <PermissionRow card={msg.permission} onDecide={onDecide} />
      ) : null;
    case "error":
      return (
        <div className="row system error">
          <span className="text">{msg.text}</span>
        </div>
      );
    default:
      return (
        <div className="row system">
          <span className="text">{msg.text}</span>
        </div>
      );
  }
}

// Markdown renders Claude's response with GFM (tables, task lists, strikethrough,
// autolinks). react-markdown v9 does NOT render raw HTML unless rehype-raw is
// added — we deliberately leave it out, so embedded HTML is shown as text and
// the surface is XSS-safe (design: "raw HTML 렌더링 비활성").
function Markdown({ text }: { text: string }) {
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
    </div>
  );
}

// ActivityRow is a collapsed "🔧 <tool>: <preview>" line for one tool call. It
// expands to show the full input JSON and, once matched, the tool result.
function ActivityRow({
  activity,
}: {
  activity: NonNullable<ChatMessage["activity"]>;
}) {
  const [open, setOpen] = useState(false);
  const preview = toolPreview(activity.input);
  const name = activity.name || "도구";
  return (
    <div className="row activity">
      <button className="activity-line" onClick={() => setOpen((v) => !v)}>
        <span className="chev">{open ? "▾" : "▸"}</span>
        <span className="tool-icon">🔧</span>
        <span className="tool-name">{name}</span>
        {preview && <span className="tool-preview">{preview}</span>}
        {!activity.hasResult && <span className="tool-running">…</span>}
      </button>
      {open && (
        <div className="activity-detail">
          <div className="activity-sub">입력</div>
          <pre className="activity-pre">{summarizeJson(activity.input)}</pre>
          {activity.hasResult && (
            <>
              <div className="activity-sub">결과</div>
              <pre className="activity-pre">
                {activity.result || "(빈 결과)"}
              </pre>
            </>
          )}
        </div>
      )}
    </div>
  );
}

// PermissionRow is the inline approval card. While pending it shows 허용/거부
// buttons; once answered it freezes into a decision label so the record stays.
function PermissionRow({
  card,
  onDecide,
}: {
  card: NonNullable<ChatMessage["permission"]>;
  onDecide: (requestId: string, allow: boolean) => void;
}) {
  const pending = card.decision === "pending";
  return (
    <div className={"row permission " + card.decision}>
      <div className="perm-card">
        <div className="perm-head">
          <span className="perm-badge">승인 요청</span>
          <span className="perm-tool">{card.toolName || "도구"}</span>
        </div>
        <pre className="perm-input">{summarizeJson(card.input)}</pre>
        {pending ? (
          <div className="perm-actions">
            <button
              className="perm-allow"
              onClick={() => onDecide(card.requestId, true)}
            >
              허용
            </button>
            <button
              className="perm-deny"
              onClick={() => onDecide(card.requestId, false)}
            >
              거부
            </button>
          </div>
        ) : (
          <div className={"perm-decided " + card.decision}>
            {card.decision === "allowed" ? "✓ 허용됨" : "✕ 거부됨"}
          </div>
        )}
      </div>
    </div>
  );
}

// ThinkingRow shows the latest extended-thinking summary as a collapsed grey
// line the user can expand (design: "thinking은 접힌 회색 한 줄(토글)").
function ThinkingRow({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  const oneLine = text.replace(/\s+/g, " ").trim();
  const preview = oneLine.length > 80 ? oneLine.slice(0, 80) + "…" : oneLine;
  return (
    <div className="row thinking">
      <button className="think-toggle" onClick={() => setOpen((v) => !v)}>
        {open ? "▾" : "▸"} 💭 {open ? "" : preview}
      </button>
      {open && <span className="think-full">{oneLine}</span>}
    </div>
  );
}
