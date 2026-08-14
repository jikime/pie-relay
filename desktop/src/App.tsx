import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import { ConnectScreen, type ConnectValues } from "./ConnectScreen";
import { ChatScreen, type RoomStatus } from "./ChatScreen";
import { TerminalScreen } from "./TerminalScreen";
import { HostPanel, type OpenHostRoom } from "./HostPanel";
import { MobilePanel } from "./PieMobilePanel";
import {
  roomsReducer,
  initialRoomsState,
  makeRoomId,
  loadRooms,
  saveRooms,
  type Room,
} from "./rooms";
import type { RoomMode } from "./protocol";
import pieLabMark from "./assets/pielab-mark-ondark.png";
import { UiIcon } from "./UiIcon";
import type { RelayConfig } from "./relay-config";

// Session is the handoff from the connect screen to the chat screen: everything
// the chat view needs to open its socket and label the user.
export interface Session {
  wsUrl: string;
  token: string;
  room: string;
  name: string;
  asHost: boolean; // joined with a host-role token → sees approval cards
  // Optional local display alias for the room (방 이름). Purely cosmetic — the
  // relay room id (`room`) and token are unchanged. Absent → fall back to `room`.
  label?: string;
  // Scoped managed sessions carry their runtime identity all the way through
  // the invite/token flow. Older standalone credentials may omit it.
  executionTarget?: "local" | "docker";
  deviceId?: string;
  relaySessionId?: string;
}

type Mode = "participant" | "host" | "mobile";

// App is the multi-room shell (P4). It keeps EVERY joined room's ChatScreen
// mounted at once and hides the inactive ones with CSS (display:none) — so each
// room's socket, reducer, and captured session id stay alive in the background
// while you switch between them. Unmounting is reserved for closing a room,
// which is the only thing that tears down a socket. State is local (useReducer +
// useState), no external store, per the design.
export function App({ relayConfig }: { relayConfig: RelayConfig }) {
  const [state, dispatch] = useReducer(
    roomsReducer,
    undefined,
    // Start on the connect screen (activeRoomId=null), NOT an auto-selected
    // persisted room — restored rooms (often stale/expired) sit in the sidebar
    // to click, but "참가자" opens to "join a host" as the user expects.
    () => initialRoomsState(loadRooms(), null),
  );
  const [mode, setMode] = useState<Mode>("participant");
  // Per-room host presence / socket liveness, mirrored up from each ChatScreen
  // for the sidebar dots. Kept separate from the room list so a status blip
  // never rewrites (or re-persists) the rooms themselves.
  const [status, setStatus] = useState<Record<string, RoomStatus>>({});
  // The room we were on before opening the "add room" view, so 취소 can return.
  const prevActiveRef = useRef<string | null>(null);

  // Persist the joined-room list so a restart can restore the rooms (expired
  // tokens just show as disconnected until the user closes and re-joins).
  useEffect(() => {
    saveRooms(state.rooms);
  }, [state.rooms]);

  // Stable per-room callbacks. ChatScreen's status/activity effects run on every
  // render, so the callbacks must be referentially stable (keyed by room id) or
  // they would retrigger those effects; setStatus also no-ops on unchanged
  // values so mirroring status can never loop.
  const cbRef = useRef(
    new Map<
      string,
      {
        onActivity: () => void;
        onStatus: (s: RoomStatus) => void;
        onMode: (m: RoomMode) => void;
      }
    >(),
  );
  const callbacksFor = useCallback((id: string) => {
    const cache = cbRef.current;
    let cb = cache.get(id);
    if (!cb) {
      cb = {
        onActivity: () => dispatch({ kind: "activity", id }),
        onStatus: (s: RoomStatus) =>
          setStatus((prev) => {
            const cur = prev[id];
            if (cur && cur.hostUp === s.hostUp && cur.live === s.live)
              return prev;
            return { ...prev, [id]: s };
          }),
        // A room_mode announcement classifies the room; the reducer no-ops when
        // the mode is unchanged, so this can safely fire on every reconnect.
        onMode: (m: RoomMode) => dispatch({ kind: "set_mode", id, mode: m }),
      };
      cache.set(id, cb);
    }
    return cb;
  }, []);

  const onJoined = useCallback(
    (values: ConnectValues, token: string, room: string, asHost: boolean) => {
      const next: Room = {
        id: makeRoomId(),
        wsUrl: values.wsUrl,
        token,
        room,
        name: values.name,
        asHost,
        label: values.label,
        executionTarget: values.executionTarget,
        deviceId: values.deviceId,
        relaySessionId: values.relaySessionId,
        mode: "unknown", // classified once the host announces room_mode
      };
      // add() merges a duplicate (same relay+room+name+role) into the open room
      // instead of spawning a second socket, and activates the result.
      setMode("participant");
      dispatch({ kind: "add", room: next });
    },
    [],
  );

  // Starting the daemon auto-joins its room as an asHost participant (so the host
  // can approve tools / hold the driver / watch), but does NOT switch away from
  // the host tab — the operator stays put to issue invite codes etc. The room
  // appears in the sidebar to open when they want it. add()'s merge rule means a
  // daemon restart re-joins the same room instead of spawning a second socket.
  const onOpenHostRoom = useCallback(
    ({
      wsUrl,
      token,
      room,
      label,
      name,
      executionTarget,
      deviceId,
      relaySessionId,
    }: OpenHostRoom) => {
      const next: Room = {
        id: makeRoomId(),
        wsUrl,
        token,
        room,
        // Host's display alias: `name` (내 이름) defaults to the host label; `label`
        // (방 이름) is optional and falls back to the relay room id in the UI.
        name: name || "나 (호스트)",
        asHost: true,
        label,
        executionTarget,
        deviceId,
        relaySessionId,
        mode: "unknown", // classified once the host announces room_mode
      };
      // No setMode("participant"): background join, no view switch (user's request).
      dispatch({ kind: "add", room: next });
    },
    [],
  );

  // Live-edit the host's own room alias from the host panel: as the operator
  // types 방 이름/내 이름, update the already-open asHost room's display values.
  // This is a rename (label/name only) — never a re-join, so the socket, token,
  // and relay room id are untouched. No-op in the reducer when nothing changed.
  const onRenameHostRoom = useCallback(
    ({ label, name }: { label?: string; name?: string }) => {
      dispatch({ kind: "rename", label, name });
    },
    [],
  );

  const closeRoom = useCallback((id: string) => {
    dispatch({ kind: "close", id });
    cbRef.current.delete(id);
    setStatus((prev) => {
      if (!(id in prev)) return prev;
      const next = { ...prev };
      delete next[id];
      return next;
    });
  }, []);

  const selectRoom = useCallback((id: string) => {
    setMode("participant");
    dispatch({ kind: "activate", id });
  }, []);

  const openAddRoom = useCallback(() => {
    setMode("participant");
    prevActiveRef.current = state.activeRoomId;
    dispatch({ kind: "activate", id: null });
  }, [state.activeRoomId]);

  const cancelAddRoom = useCallback(() => {
    const back = prevActiveRef.current;
    const target = state.rooms.find((r) => r.id === back) ?? state.rooms[0];
    if (target) dispatch({ kind: "activate", id: target.id });
  }, [state.rooms]);

  const showConnect = mode === "participant" && state.activeRoomId === null;
  const canCancel = state.rooms.length > 0;
  const activeTerminalRoom = state.rooms.find(
    (room) => room.id === state.activeRoomId,
  );
  const activeTerminalStatus = activeTerminalRoom
    ? status[activeTerminalRoom.id]
    : undefined;
  const contextTab =
    mode === "host"
      ? { icon: "host" as const, label: "이 PC 직접 호스트" }
      : mode === "mobile"
        ? { icon: "mobile" as const, label: "모바일 연결" }
        : showConnect
          ? { icon: "plus" as const, label: "새 연결" }
          : null;

  return (
    <div className="app multiroom">
      <aside className="sidebar">
        <div className="sidebar-brand">
          <span className="sidebar-brand-mark">
            <img src={pieLabMark} alt="" />
          </span>
          <div>
            <strong>Pie Relay</strong>
            <span>Remote workspace</span>
          </div>
        </div>
        <div className="sidebar-section-head">
          <span className="sidebar-section-label">세션</span>
          <span className="sidebar-count">{state.rooms.length}</span>
          <button
            className="sidebar-icon-button"
            onClick={openAddRoom}
            title="새 연결"
            aria-label="새 연결"
          >
            <UiIcon name="plus" size={14} />
          </button>
        </div>
        <ul className="room-list">
          {state.rooms.length === 0 && (
            <li className="room-empty">
              <UiIcon name="terminal" size={18} />
              <span>열린 세션이 없습니다</span>
              <small>새 연결에서 작업 세션을 만들거나 초대 코드로 참여하세요.</small>
            </li>
          )}
          {state.rooms.map((r) => (
            <RoomItem
              key={r.id}
              room={r}
              active={mode === "participant" && r.id === state.activeRoomId}
              unread={state.unread.has(r.id)}
              status={status[r.id]}
              onSelect={() => selectRoom(r.id)}
              onClose={() => closeRoom(r.id)}
            />
          ))}
        </ul>

        <button
          className={"add-room" + (showConnect ? " active" : "")}
          onClick={openAddRoom}
        >
          <UiIcon name="plus" size={15} />
          새 연결
        </button>

        <div className="sidebar-sep" />

        <nav className="sidebar-modes">
          <button
            className={"mode-item" + (mode === "participant" ? " active" : "")}
            onClick={openAddRoom}
          >
            <span className="mode-icon">
              <UiIcon name="plus" size={16} />
            </span>
            <span className="mode-copy">
              <strong>새 연결</strong>
              <small>초대 코드로 참가</small>
            </span>
          </button>
          <button
            className={"mode-item" + (mode === "host" ? " active" : "")}
            onClick={() => setMode("host")}
          >
            <span className="mode-icon">
              <UiIcon name="host" size={16} />
            </span>
            <span className="mode-copy">
              <strong>이 PC 직접 호스트</strong>
              <small>단독 Relay · Host OS</small>
            </span>
          </button>
          <button
            className={"mode-item" + (mode === "mobile" ? " active" : "")}
            onClick={() => setMode("mobile")}
          >
            <span className="mode-icon">
              <UiIcon name="mobile" size={16} />
            </span>
            <span className="mode-copy">
              <strong>모바일</strong>
              <small>QR · Wi-Fi · Relay</small>
            </span>
          </button>
        </nav>
        <div className="sidebar-footer">
          <span className="sidebar-footer-dot" />
          <span>cookai.dev</span>
          <span className="sidebar-footer-version">v0.1</span>
        </div>
      </aside>

      <main className="room-main">
        <div className="session-tabs" aria-label="열린 작업 공간">
          <div className="session-tabs-scroll">
            {state.rooms.map((r) => {
              const active =
                mode === "participant" && r.id === state.activeRoomId;
              const label =
                r.label?.trim() || r.room.trim() || r.name || "세션";
              return (
                <div
                  key={r.id}
                  className={"session-tab" + (active ? " active" : "")}
                >
                  <button
                    className="session-tab-open"
                    onClick={() => selectRoom(r.id)}
                    title={label}
                  >
                    <UiIcon
                      name={r.mode === "terminal" ? "terminal" : "chat"}
                      size={14}
                    />
                    <span>{label}</span>
                    {r.executionTarget && (
                      <span
                        className={
                          "session-target " + r.executionTarget
                        }
                      >
                        {r.executionTarget === "docker" ? "Docker" : "Host OS"}
                      </span>
                    )}
                    {state.unread.has(r.id) && (
                      <span className="session-tab-unread" aria-label="새 활동" />
                    )}
                  </button>
                  <button
                    className="session-tab-close"
                    onClick={() => closeRoom(r.id)}
                    title="세션 닫기"
                    aria-label={`${label} 닫기`}
                  >
                    <UiIcon name="close" size={13} />
                  </button>
                </div>
              );
            })}
            {contextTab && (
              <div className="session-tab context active">
                <span className="session-tab-context">
                  <UiIcon name={contextTab.icon} size={14} />
                  <span>{contextTab.label}</span>
                </span>
              </div>
            )}
          </div>
          <button
            className="session-tab-add"
            onClick={openAddRoom}
            title="새 연결"
            aria-label="새 연결"
          >
            <UiIcon name="plus" size={15} />
          </button>
        </div>
        {/* Every joined room stays mounted; inactive ones are hidden (CSS
            display:none) so their sockets keep running in the background. */}
        {state.rooms.map((r) => {
          const cb = callbacksFor(r.id);
          const roomHidden = !(
            mode === "participant" && r.id === state.activeRoomId
          );
          // A terminal room mounts TerminalScreen; everything else (chat, or a
          // not-yet-classified room) mounts ChatScreen, which detects a terminal
          // room's room_mode and hands off. Each keeps its own socket alive
          // while hidden — the mode swap is the one intentional re-mount.
          return r.mode === "terminal" ? (
            <TerminalScreen
              key={r.id}
              session={r}
              hidden={roomHidden}
              onLeave={() => closeRoom(r.id)}
              onActivity={cb.onActivity}
              onStatus={cb.onStatus}
              onMode={cb.onMode}
            />
          ) : (
            <ChatScreen
              key={r.id}
              session={r}
              hidden={roomHidden}
              onLeave={() => closeRoom(r.id)}
              onActivity={cb.onActivity}
              onStatus={cb.onStatus}
              onMode={cb.onMode}
            />
          );
        })}

        {showConnect && (
          <ConnectScreen
            relayConfig={relayConfig}
            onJoined={onJoined}
            onCancel={canCancel ? cancelAddRoom : undefined}
          />
        )}

        {/* HostPanel stays mounted even off the host tab (hidden via CSS) so its
            launch auto-start effect can run regardless of the active view. */}
        <HostPanel
          relayConfig={relayConfig}
          hidden={mode !== "host"}
          onOpenHostRoom={onOpenHostRoom}
          onRenameHostRoom={onRenameHostRoom}
          activeSession={
            activeTerminalRoom
              ? {
                  label:
                    activeTerminalRoom.label?.trim() ||
                    activeTerminalRoom.room.trim() ||
                    activeTerminalRoom.name ||
                    "터미널",
                  executionTarget: activeTerminalRoom.executionTarget,
                  deviceId: activeTerminalRoom.deviceId,
                  connected: activeTerminalStatus?.live ?? false,
                  hostConnected: activeTerminalStatus?.hostUp ?? false,
                  relayLocal: /^(?:wss?:\/\/)?(?:127\.0\.0\.1|localhost|\[::1\])(?::|\/|$)/i.test(
                    activeTerminalRoom.wsUrl,
                  ),
                }
              : undefined
          }
        />
        <MobilePanel relayConfig={relayConfig} hidden={mode !== "mobile"} />
      </main>
    </div>
  );
}

// RoomItem is one sidebar entry: host-presence dot (●/○, dimmed when the socket
// is down), room label, an unread badge, and a × to close (which unmounts the
// room and closes its socket).
function RoomItem({
  room,
  active,
  unread,
  status,
  onSelect,
  onClose,
}: {
  room: Room;
  active: boolean;
  unread: boolean;
  status: RoomStatus | undefined;
  onSelect: () => void;
  onClose: () => void;
}) {
  // Until the freshly-mounted ChatScreen reports a status it is still
  // connecting — don't flash "끊김" during that window.
  const offline = status !== undefined && !status.live;
  const hostUp = status?.hostUp ?? false;
  // Prefer the local display alias (label), then the relay room id, then name.
  const label = room.label?.trim() || room.room.trim() || room.name || "방";
  // A neutral line icon distinguishes terminal and chat rooms without relying
  // on platform-dependent emoji rendering.
  const isTerminal = room.mode === "terminal";
  const typeTitle = isTerminal ? "터미널 방" : "채팅 방";
  return (
    <li
      className={
        "room-item" + (active ? " active" : "") + (offline ? " offline" : "")
      }
    >
      <button className="room-open" onClick={onSelect} title={label}>
        <span
          className={"room-type " + (isTerminal ? "terminal" : "chat")}
          title={typeTitle}
        >
          <UiIcon name={isTerminal ? "terminal" : "chat"} size={15} />
        </span>
        <span className="room-item-copy">
          <span className="room-label">{label}</span>
          <span className="room-meta">
            <span className={"room-dot " + (hostUp ? "up" : "down")} />
            {offline ? "연결 끊김" : hostUp ? "호스트 연결됨" : "연결 중"}
          </span>
        </span>
        {room.asHost && (
          <span className="room-host-tag" title="내가 호스트인 방">
            호스트
          </span>
        )}
        {room.executionTarget && (
          <span className={"room-target-tag " + room.executionTarget}>
            {room.executionTarget === "docker" ? "DOCKER" : "HOST OS"}
          </span>
        )}
        {offline && <span className="room-offline-tag">끊김</span>}
        {unread && <span className="room-unread" aria-label="새 활동" />}
      </button>
      <button
        className="room-close"
        onClick={onClose}
        title="방 닫기"
        aria-label="방 닫기"
      >
        ×
      </button>
    </li>
  );
}
