import { describe, it, expect } from "vitest";
import { hostJoinArgs, type ConnectValues } from "./ConnectScreen";

// hostJoinArgs is the pure core of the advanced "paste a host token" path: it
// validates the required fields and decodes the token's room, producing exactly
// the (token, room) that ConnectScreen forwards to onJoined(..., asHost=true).
describe("hostJoinArgs — host-token join path", () => {
  function jwtWith(claims: Record<string, unknown>): string {
    const payload = btoa(JSON.stringify(claims))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
    return `h.${payload}.s`;
  }

  const values: ConnectValues = { wsUrl: "ws://127.0.0.1:13412", code: "", name: "alice" };

  it("decodes the token's room and trims the token", () => {
    const token = jwtWith({ room: "r-dev", sub: "host:alice" });
    const res = hostJoinArgs(values, `  ${token}  `);
    expect(res).toEqual({ token, room: "r-dev" });
  });

  it("falls back to the sub claim when the token has no room", () => {
    const token = jwtWith({ sub: "host:alice" });
    expect(hostJoinArgs(values, token)).toEqual({ token, room: "host:alice" });
  });

  it("errors when the token is missing", () => {
    const res = hostJoinArgs(values, "   ");
    expect(res).toEqual({ error: "릴레이 주소, 호스트 토큰, 이름을 모두 입력하세요." });
  });

  it("errors when the relay address or name is missing", () => {
    const token = jwtWith({ room: "r-dev" });
    expect("error" in hostJoinArgs({ ...values, wsUrl: "" }, token)).toBe(true);
    expect("error" in hostJoinArgs({ ...values, name: "" }, token)).toBe(true);
  });
});
