// rooms.ts — the pure, React-free core of the multi-room shell (P4).
//
// App.tsx keeps every joined room's ChatScreen mounted at once (so sockets stay
// live while you switch), and this module owns the bookkeeping: which rooms are
// open, which one is active, and which hidden rooms have unread activity. Every
// function here is pure so the room lifecycle (add / close / switch / merge /
// unread) can be unit-tested (rooms.test.ts) without a DOM or a socket.

import type { Session } from "./App";
import type { RoomMode } from "./protocol";

// RoomKind is a room's classification for the shell: "unknown" until the host
// announces room_mode, then a chat room or a terminal (PTY) room. It decides
// which screen the shell mounts (ChatScreen vs TerminalScreen) and the sidebar
// icon. Persisted so a restored terminal room mounts its terminal directly
// instead of flashing the chat screen and reconnecting to reclassify.
export type RoomKind = RoomMode | "unknown";

// A Room is a Session plus a stable id used as the React key and the sidebar
// selection key. The id must survive a localStorage round-trip, so it is minted
// once on join and persisted with the rest of the session fields.
export interface Room extends Session {
  id: string;
  mode: RoomKind;
}

// Session already carries `label?` (a local display alias for the room) and
// `name` (the shown speaker name). Both are pure display values — renaming them
// never touches the relay room id, the token, the socket, or the room's mode.

export interface RoomsState {
  rooms: Room[];
  activeRoomId: string | null; // null → the "add room" (connect) view
  unread: Set<string>; // ids of hidden rooms with new activity since last seen
}

export type RoomsAction =
  | { kind: "add"; room: Room } // join succeeded (or restore)
  | { kind: "close"; id: string } // sidebar × / leave — unmounts, closes socket
  | { kind: "activate"; id: string | null } // switch active room (or connect view)
  | { kind: "activity"; id: string } // a hidden room saw new activity
  | { kind: "set_mode"; id: string; mode: RoomMode } // host announced room_mode
  | { kind: "rename"; id?: string; label?: string; name?: string }; // edit local display alias

export function initialRoomsState(
  rooms: Room[] = [],
  activeRoomId: string | null = rooms.length > 0 ? rooms[0].id : null,
): RoomsState {
  return { rooms, activeRoomId, unread: new Set() };
}

// Two joins point at the same live room when they share relay + room + name +
// role. Re-joining an already-open room must reuse the existing connection
// instead of opening a second socket, so `add` folds into the existing room.
export function sameRoomIdentity(a: Session, b: Session): boolean {
  return (
    a.wsUrl === b.wsUrl &&
    a.room === b.room &&
    a.name === b.name &&
    a.asHost === b.asHost
  );
}

export function findRoom(rooms: Room[], session: Session): Room | undefined {
  return rooms.find((r) => sameRoomIdentity(r, session));
}

// makeRoomId returns a process-unique, opaque id. It is persisted with the room
// so restored rooms keep their identity across restarts.
let roomCounter = 0;
export function makeRoomId(): string {
  return "r" + Date.now().toString(36) + (roomCounter++).toString(36);
}

function withoutUnread(unread: Set<string>, id: string): Set<string> {
  if (!unread.has(id)) return unread;
  const next = new Set(unread);
  next.delete(id);
  return next;
}

export function roomsReducer(state: RoomsState, action: RoomsAction): RoomsState {
  switch (action.kind) {
    case "add": {
      // Merge a duplicate join into the room already open (no second socket):
      // switch to it and clear its unread. CRUCIALLY, adopt the re-join's FRESH
      // token (and label): a re-join always carries newly-minted credentials, so
      // reusing the existing room's OLD token would re-dial with a stale/expired
      // ticket → the relay rejects the upgrade (401) → a perpetual "재접속 중"
      // loop, while the valid new token is silently discarded. The screen socket
      // effects key on session.token, so swapping the token here makes the live
      // socket re-dial with the valid ticket. id/mode/name/asHost are preserved
      // (React identity + classification unchanged, still one socket). Same
      // reference when nothing changed, so a no-op re-add never churns state.
      const existing = findRoom(state.rooms, action.room);
      if (existing) {
        const tokenChanged = existing.token !== action.room.token;
        const nextLabel = action.room.label ?? existing.label;
        const labelChanged = nextLabel !== existing.label;
        const rooms =
          tokenChanged || labelChanged
            ? state.rooms.map((r) =>
                r.id === existing.id
                  ? { ...r, token: action.room.token, label: nextLabel }
                  : r,
              )
            : state.rooms;
        return {
          ...state,
          rooms,
          activeRoomId: existing.id,
          unread: withoutUnread(state.unread, existing.id),
        };
      }
      return {
        rooms: [...state.rooms, action.room],
        activeRoomId: action.room.id,
        unread: withoutUnread(state.unread, action.room.id),
      };
    }

    case "close": {
      const rooms = state.rooms.filter((r) => r.id !== action.id);
      // Closing the active room falls back to the first remaining room, or the
      // connect view when none are left. Closing a background room leaves the
      // active selection untouched.
      const activeRoomId =
        state.activeRoomId === action.id
          ? rooms.length > 0
            ? rooms[0].id
            : null
          : state.activeRoomId;
      return { rooms, activeRoomId, unread: withoutUnread(state.unread, action.id) };
    }

    case "activate": {
      if (action.id === null) {
        return { ...state, activeRoomId: null };
      }
      return {
        ...state,
        activeRoomId: action.id,
        unread: withoutUnread(state.unread, action.id),
      };
    }

    case "activity": {
      // Only background (non-active) rooms accumulate unread; the room you are
      // looking at never lights its own badge.
      if (action.id === state.activeRoomId || state.unread.has(action.id)) {
        return state;
      }
      const unread = new Set(state.unread);
      unread.add(action.id);
      return { ...state, unread };
    }

    case "set_mode": {
      // Classify (or reclassify) a room from a room_mode announcement. No-op if
      // the mode is unchanged so React keeps the stable reference. The changed
      // room list re-persists via App's saveRooms effect.
      let changed = false;
      const rooms = state.rooms.map((r) => {
        if (r.id === action.id && r.mode !== action.mode) {
          changed = true;
          return { ...r, mode: action.mode };
        }
        return r;
      });
      return changed ? { ...state, rooms } : state;
    }

    case "rename": {
      // Edit a room's local display alias (label = 방 이름, name = 내 이름). This
      // is purely cosmetic — the relay room id, token, socket, mode, active, and
      // unread are all left untouched so the connection stays live. With no id
      // the target is the host's own room (asHost); if several exist, the most
      // recently added wins. No-op (same reference) when nothing actually
      // changes, so onChange can fire freely without churning React state.
      const target =
        action.id !== undefined
          ? state.rooms.find((r) => r.id === action.id)
          : [...state.rooms].reverse().find((r) => r.asHost);
      if (!target) return state;
      const nextLabel = action.label !== undefined ? action.label : target.label;
      const nextName = action.name !== undefined ? action.name : target.name;
      if (nextLabel === target.label && nextName === target.name) return state;
      const rooms = state.rooms.map((r) =>
        r.id === target.id ? { ...r, label: nextLabel, name: nextName } : r,
      );
      return { ...state, rooms };
    }

    default:
      return state;
  }
}

// ---------------------------------------------------------------------------
// localStorage persistence (design: 방 목록 localStorage 유지 → 재시작 복원)
// ---------------------------------------------------------------------------

const ROOMS_LS_KEY = "pie-relay.rooms";
const LEGACY_ROOMS_LS_KEY = "cli-relay.rooms";

// loadRooms restores the joined-room list. An expired guest token simply means
// that room's socket will not open (shown as "연결 끊김"), so nothing is
// filtered here — the user decides whether to close and re-join.
export function loadRooms(): Room[] {
  if (typeof localStorage === "undefined") return [];
  try {
    const raw = localStorage.getItem(ROOMS_LS_KEY) || localStorage.getItem(LEGACY_ROOMS_LS_KEY);
    if (!raw) return [];
    const arr = JSON.parse(raw) as unknown;
    if (!Array.isArray(arr)) return [];
    const rooms: Room[] = [];
    for (const v of arr) {
      if (v && typeof v === "object") {
        const r = v as Partial<Room>;
        if (typeof r.id === "string" && typeof r.wsUrl === "string" && typeof r.name === "string") {
          rooms.push({
            id: r.id,
            wsUrl: r.wsUrl,
            token: typeof r.token === "string" ? r.token : "",
            room: typeof r.room === "string" ? r.room : "",
            name: r.name,
            asHost: Boolean(r.asHost),
            mode: r.mode === "terminal" || r.mode === "chat" ? r.mode : "unknown",
            ...(typeof r.label === "string" ? { label: r.label } : {}),
          });
        }
      }
    }
    return rooms;
  } catch {
    return [];
  }
}

export function saveRooms(rooms: Room[]): void {
  if (typeof localStorage === "undefined") return;
  try {
    const slim = rooms.map((r) => ({
      id: r.id,
      wsUrl: r.wsUrl,
      token: r.token,
      room: r.room,
      name: r.name,
      asHost: r.asHost,
      mode: r.mode,
      ...(r.label !== undefined ? { label: r.label } : {}),
    }));
    localStorage.setItem(ROOMS_LS_KEY, JSON.stringify(slim));
  } catch {
    /* private mode / disabled storage — non-fatal */
  }
}
