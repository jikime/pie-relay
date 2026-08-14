import { describe, it, expect } from "vitest";
import {
  roomsReducer,
  initialRoomsState,
  sameRoomIdentity,
  findRoom,
  type Room,
  type RoomsState,
} from "./rooms";

// A room factory — only the fields the reducer cares about need to differ.
function room(id: string, over: Partial<Room> = {}): Room {
  return {
    id,
    wsUrl: "ws://r",
    token: "t-" + id,
    room: "ROOM" + id,
    name: "alice",
    asHost: false,
    mode: "unknown",
    ...over,
  };
}

describe("sameRoomIdentity — duplicate detection", () => {
  it("matches on relay + room + name + role, ignoring id/token", () => {
    const a = room("1", { room: "A", token: "tok-a" });
    const b = room("2", { room: "A", token: "tok-b" });
    expect(sameRoomIdentity(a, b)).toBe(true);
  });

  it("differs when any identity field differs", () => {
    const a = room("1");
    expect(sameRoomIdentity(a, room("2", { name: "bob" }))).toBe(false);
    expect(sameRoomIdentity(a, room("2", { room: "OTHER" }))).toBe(false);
    expect(sameRoomIdentity(a, room("2", { wsUrl: "ws://x" }))).toBe(false);
    expect(sameRoomIdentity(a, room("2", { asHost: true }))).toBe(false);
  });

  it("findRoom locates the open room by identity", () => {
    const rooms = [room("1", { room: "A" }), room("2", { room: "B" })];
    expect(findRoom(rooms, room("9", { room: "B" }))?.id).toBe("2");
    expect(findRoom(rooms, room("9", { room: "Z" }))).toBeUndefined();
  });
});

describe("roomsReducer — add", () => {
  it("appends a new room and activates it", () => {
    const s = roomsReducer(initialRoomsState(), { kind: "add", room: room("1") });
    expect(s.rooms.map((r) => r.id)).toEqual(["1"]);
    expect(s.activeRoomId).toBe("1");
  });

  it("merges a duplicate join into the open room (no second socket)", () => {
    let s = initialRoomsState();
    s = roomsReducer(s, { kind: "add", room: room("1", { room: "A" }) });
    s = roomsReducer(s, { kind: "add", room: room("2", { room: "B" }) });
    // Re-join room A (different id/token, same identity) while B is active.
    const dup = roomsReducer(s, { kind: "add", room: room("3", { room: "A" }) });
    expect(dup.rooms.map((r) => r.id)).toEqual(["1", "2"]); // no new room
    expect(dup.activeRoomId).toBe("1"); // switched to the existing one
  });

  it("adopts the re-join's fresh token so the socket re-dials with a valid ticket", () => {
    let s = initialRoomsState();
    s = roomsReducer(s, { kind: "add", room: room("1", { room: "A", token: "stale" }) });
    // Re-join the SAME room+name with a freshly minted token (new id).
    const dup = roomsReducer(s, { kind: "add", room: room("9", { room: "A", token: "fresh" }) });
    expect(dup.rooms.map((r) => r.id)).toEqual(["1"]); // still one room, same id
    expect(dup.rooms[0].token).toBe("fresh"); // stale token replaced → no 401 loop
  });

  it("keeps the same state reference on a no-op re-add (same token/label)", () => {
    let s = initialRoomsState();
    s = roomsReducer(s, { kind: "add", room: room("1", { room: "A", token: "t-A" }) });
    const before = s.rooms;
    const dup = roomsReducer(s, { kind: "add", room: room("9", { room: "A", token: "t-A" }) });
    expect(dup.rooms).toBe(before); // unchanged reference — no React churn
  });

  it("clears unread when re-adding a room that had unread", () => {
    let s: RoomsState = initialRoomsState([room("1", { room: "A" }), room("2", { room: "B" })], "2");
    s = roomsReducer(s, { kind: "activity", id: "1" });
    expect(s.unread.has("1")).toBe(true);
    const dup = roomsReducer(s, { kind: "add", room: room("9", { room: "A" }) });
    expect(dup.unread.has("1")).toBe(false);
    expect(dup.activeRoomId).toBe("1");
  });
});

describe("roomsReducer — host room (P6 '이 방 열기')", () => {
  // The shape App.onOpenHostRoom builds from a host token.
  function hostRoom(id: string, over: Partial<Room> = {}): Room {
    return room(id, { name: "나 (호스트)", asHost: true, room: "r-dev", ...over });
  }

  it("adds the daemon's own room joined as host and activates it", () => {
    const s = roomsReducer(initialRoomsState(), { kind: "add", room: hostRoom("h1") });
    expect(s.rooms.map((r) => r.id)).toEqual(["h1"]);
    expect(s.activeRoomId).toBe("h1");
    expect(s.rooms[0].asHost).toBe(true);
  });

  it("reopening the same host room switches to it instead of a second socket", () => {
    let s = roomsReducer(initialRoomsState(), { kind: "add", room: hostRoom("h1") });
    s = roomsReducer(s, { kind: "activate", id: null }); // leave via connect view
    const again = roomsReducer(s, {
      kind: "add",
      room: hostRoom("h2", { token: "fresh-token" }), // new token, same identity
    });
    expect(again.rooms.map((r) => r.id)).toEqual(["h1"]); // no duplicate
    expect(again.activeRoomId).toBe("h1");
  });

  it("keeps a host room separate from a guest room with the same relay/room/name", () => {
    let s = roomsReducer(initialRoomsState(), { kind: "add", room: hostRoom("h1") });
    // Same wsUrl/room/name but asHost:false → different identity, its own socket.
    s = roomsReducer(s, { kind: "add", room: room("g1", { room: "r-dev", name: "나 (호스트)" }) });
    expect(s.rooms.map((r) => r.id)).toEqual(["h1", "g1"]);
  });
});

describe("roomsReducer — close", () => {
  const base = () =>
    initialRoomsState([room("1"), room("2", { room: "B" }), room("3", { room: "C" })], "2");

  it("removes the active room and falls back to the first remaining", () => {
    const s = roomsReducer(base(), { kind: "close", id: "2" });
    expect(s.rooms.map((r) => r.id)).toEqual(["1", "3"]);
    expect(s.activeRoomId).toBe("1");
  });

  it("closing the last room drops to the connect view (null)", () => {
    let s = initialRoomsState([room("1")], "1");
    s = roomsReducer(s, { kind: "close", id: "1" });
    expect(s.rooms).toEqual([]);
    expect(s.activeRoomId).toBeNull();
  });

  it("closing a background room leaves the active selection untouched", () => {
    const s = roomsReducer(base(), { kind: "close", id: "3" });
    expect(s.rooms.map((r) => r.id)).toEqual(["1", "2"]);
    expect(s.activeRoomId).toBe("2");
  });

  it("clears any unread for the closed room", () => {
    let s = base();
    s = roomsReducer(s, { kind: "activity", id: "3" });
    s = roomsReducer(s, { kind: "close", id: "3" });
    expect(s.unread.has("3")).toBe(false);
  });
});

describe("roomsReducer — activate / unread", () => {
  it("activate switches the active room and clears its unread", () => {
    let s = initialRoomsState([room("1"), room("2", { room: "B" })], "1");
    s = roomsReducer(s, { kind: "activity", id: "2" });
    expect(s.unread.has("2")).toBe(true);
    s = roomsReducer(s, { kind: "activate", id: "2" });
    expect(s.activeRoomId).toBe("2");
    expect(s.unread.has("2")).toBe(false);
  });

  it("activate(null) opens the connect view without touching unread", () => {
    let s = initialRoomsState([room("1"), room("2", { room: "B" })], "1");
    s = roomsReducer(s, { kind: "activity", id: "2" });
    s = roomsReducer(s, { kind: "activate", id: null });
    expect(s.activeRoomId).toBeNull();
    expect(s.unread.has("2")).toBe(true);
  });

  it("activity marks a background room unread but never the active one", () => {
    let s = initialRoomsState([room("1"), room("2", { room: "B" })], "1");
    s = roomsReducer(s, { kind: "activity", id: "1" }); // active — ignored
    s = roomsReducer(s, { kind: "activity", id: "2" }); // background — marked
    expect(s.unread.has("1")).toBe(false);
    expect(s.unread.has("2")).toBe(true);
  });

  it("repeated activity keeps a single unread entry (Set semantics)", () => {
    let s = initialRoomsState([room("1"), room("2", { room: "B" })], "1");
    s = roomsReducer(s, { kind: "activity", id: "2" });
    const after = roomsReducer(s, { kind: "activity", id: "2" });
    expect(after).toBe(s); // no-op → same reference
    expect(after.unread.size).toBe(1);
  });
});

describe("roomsReducer — set_mode (terminal classification)", () => {
  it("classifies an unknown room from a room_mode announcement", () => {
    let s = initialRoomsState([room("1")], "1");
    s = roomsReducer(s, { kind: "set_mode", id: "1", mode: "terminal" });
    expect(s.rooms[0].mode).toBe("terminal");
  });

  it("no-ops (same reference) when the mode is unchanged", () => {
    let s = initialRoomsState([room("1", { mode: "terminal" })], "1");
    const after = roomsReducer(s, { kind: "set_mode", id: "1", mode: "terminal" });
    expect(after).toBe(s);
  });

  it("ignores an unknown room id", () => {
    const s = initialRoomsState([room("1")], "1");
    const after = roomsReducer(s, { kind: "set_mode", id: "nope", mode: "terminal" });
    expect(after).toBe(s);
  });

  it("leaves other rooms untouched", () => {
    let s = initialRoomsState([room("1"), room("2", { room: "B" })], "1");
    s = roomsReducer(s, { kind: "set_mode", id: "2", mode: "terminal" });
    expect(s.rooms[0].mode).toBe("unknown");
    expect(s.rooms[1].mode).toBe("terminal");
  });
});

describe("roomsReducer — rename (P10 display alias)", () => {
  it("updates a room's label and name by id", () => {
    let s = initialRoomsState([room("1")], "1");
    s = roomsReducer(s, { kind: "rename", id: "1", label: "내 방", name: "지민" });
    expect(s.rooms[0].label).toBe("내 방");
    expect(s.rooms[0].name).toBe("지민");
  });

  it("targets the asHost room when no id is given", () => {
    let s = initialRoomsState(
      [room("g", { asHost: false }), room("h", { asHost: true, name: "나 (호스트)" })],
      "g",
    );
    s = roomsReducer(s, { kind: "rename", label: "작업방", name: "호스트지민" });
    expect(s.rooms[0].label).toBeUndefined(); // guest room untouched
    expect(s.rooms[0].name).toBe("alice");
    expect(s.rooms[1].label).toBe("작업방"); // asHost room renamed
    expect(s.rooms[1].name).toBe("호스트지민");
  });

  it("picks the most recently added asHost room when several exist", () => {
    let s = initialRoomsState(
      [room("h1", { asHost: true }), room("h2", { asHost: true })],
      "h1",
    );
    s = roomsReducer(s, { kind: "rename", label: "최신" });
    expect(s.rooms[0].label).toBeUndefined();
    expect(s.rooms[1].label).toBe("최신");
  });

  it("keeps the connection-invariant fields and active/unread untouched", () => {
    let s = initialRoomsState([room("1", { mode: "terminal" }), room("2", { room: "B" })], "1");
    s = roomsReducer(s, { kind: "activity", id: "2" });
    const before = s.rooms[0];
    const after = roomsReducer(s, { kind: "rename", id: "1", label: "새 이름", name: "밥" });
    const r = after.rooms[0];
    expect(r.id).toBe(before.id);
    expect(r.wsUrl).toBe(before.wsUrl);
    expect(r.token).toBe(before.token);
    expect(r.room).toBe(before.room); // relay room id unchanged
    expect(r.asHost).toBe(before.asHost);
    expect(r.mode).toBe("terminal"); // mode unchanged
    expect(after.activeRoomId).toBe("1"); // active unchanged
    expect(after.unread.has("2")).toBe(true); // unread unchanged
  });

  it("no-ops (same reference) when nothing changes", () => {
    const s = initialRoomsState([room("1", { label: "그대로", name: "alice" })], "1");
    const after = roomsReducer(s, { kind: "rename", id: "1", label: "그대로", name: "alice" });
    expect(after).toBe(s);
  });

  it("no-ops when the target room is not found", () => {
    const s = initialRoomsState([room("1", { asHost: false })], "1");
    const byId = roomsReducer(s, { kind: "rename", id: "nope", label: "x" });
    expect(byId).toBe(s);
    const noHost = roomsReducer(s, { kind: "rename", label: "x" }); // no asHost room
    expect(noHost).toBe(s);
  });

  it("updates only the field provided, leaving the other alias intact", () => {
    let s = initialRoomsState([room("1", { label: "방A", name: "alice" })], "1");
    s = roomsReducer(s, { kind: "rename", id: "1", name: "bob" }); // label omitted
    expect(s.rooms[0].label).toBe("방A"); // preserved
    expect(s.rooms[0].name).toBe("bob");
  });
});
