import { describe, it, expect } from "vitest";
import { parseInviteCode, ringPush } from "./sidecar";

describe("parseInviteCode", () => {
  it("extracts the code from the Go client's room-create output", () => {
    const out =
      "초대 코드: ABCD2345\n만료: 15:04:05 까지 (약 10분)\n참가: client join ABCD2345 --name <이름>\n";
    expect(parseInviteCode(out)).toBe("ABCD2345");
  });

  it("tolerates the fullwidth colon and extra spacing", () => {
    expect(parseInviteCode("초대 코드 ： WXYZ9876")).toBe("WXYZ9876");
  });

  it("finds the code even when preceded by log lines", () => {
    const out = "2026/07/10 client: something\n초대 코드: HELLO123\n";
    expect(parseInviteCode(out)).toBe("HELLO123");
  });

  it("returns null when no code line is present", () => {
    expect(parseInviteCode("초대 코드 발급 실패: HTTP 401")).toBeNull();
    expect(parseInviteCode("")).toBeNull();
  });
});

describe("ringPush", () => {
  it("appends without mutating the input", () => {
    const a = ["1", "2"];
    const b = ringPush(a, "3", 200);
    expect(b).toEqual(["1", "2", "3"]);
    expect(a).toEqual(["1", "2"]);
  });

  it("caps the buffer at max, dropping the oldest lines", () => {
    let buf: string[] = [];
    for (let i = 1; i <= 205; i++) buf = ringPush(buf, String(i), 200);
    expect(buf).toHaveLength(200);
    expect(buf[0]).toBe("6");
    expect(buf[buf.length - 1]).toBe("205");
  });

  it("handles max of 1", () => {
    let buf = ringPush(["old"], "new", 1);
    expect(buf).toEqual(["new"]);
  });
});
