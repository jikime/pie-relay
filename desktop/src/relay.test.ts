import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createInvite,
  enrollHost,
  httpBaseFromWs,
  isLoopbackRelay,
  join,
  participantTicketProtocol,
  participantWsEndpoint,
  wsBaseNormalized,
  wsOriginFromRelay,
} from "./relay";

// mockFetch installs a global.fetch that records the last call and returns the
// given response shape, so the URL/body enrollHost builds can be asserted
// without a live relay.
function mockFetch(
  responder: (
    url: string,
    init: RequestInit,
  ) => Partial<Response> & { jsonBody?: unknown },
): { calls: Array<{ url: string; init: RequestInit }> } {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const fn = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, init });
    const r = responder(url, init);
    return {
      ok: r.ok ?? true,
      status: r.status ?? 200,
      json: async () => r.jsonBody ?? {},
      text: async () => (typeof r.jsonBody === "string" ? r.jsonBody : ""),
      ...r,
    } as Response;
  });
  // @ts-expect-error — test shim for the subset of fetch enrollHost uses
  globalThis.fetch = fn;
  return { calls };
}

afterEach(() => {
  vi.restoreAllMocks();
  // @ts-expect-error — remove the shim between tests
  delete globalThis.fetch;
});

describe("URL derivation (ws→http)", () => {
  it("converts ws relay origins to http and wss to https", () => {
    // The /ws/agent path is stripped — http base is the origin only, so
    // /rooms/* and /host/enroll append cleanly (not under /ws/agent).
    expect(httpBaseFromWs("ws://127.0.0.1:13412/ws/agent")).toBe(
      "http://127.0.0.1:13412",
    );
    expect(httpBaseFromWs("wss://relay.example.com/ws/agent")).toBe(
      "https://relay.example.com",
    );
  });

  it("defaults a scheme-less host to ws→http and strips trailing slashes", () => {
    expect(httpBaseFromWs("relay.example.com//")).toBe(
      "http://relay.example.com",
    );
  });

  it("normalizes ws/http origins and strips the agent path", () => {
    expect(wsBaseNormalized("http://h:1/x")).toBe("ws://h:1/x");
    expect(wsOriginFromRelay("ws://127.0.0.1:13412/ws/agent")).toBe(
      "ws://127.0.0.1:13412",
    );
  });

  it("keeps participant credentials out of the WebSocket URL", () => {
    expect(participantWsEndpoint("https://relay.cookai.dev")).toBe(
      "wss://relay.cookai.dev/ws/participant",
    );
    expect(participantTicketProtocol("header.payload.signature")).toBe(
      "pie-relay.ticket.header.payload.signature",
    );
  });
});

describe("isLoopbackRelay", () => {
  it("recognizes loopback hosts with or without port/path", () => {
    expect(isLoopbackRelay("ws://127.0.0.1:13412/ws/agent")).toBe(true);
    expect(isLoopbackRelay("localhost")).toBe(true);
    expect(isLoopbackRelay("wss://[::1]:13412")).toBe(true);
  });

  it("treats public hosts as non-loopback", () => {
    expect(isLoopbackRelay("wss://relay.example.com/ws/agent")).toBe(false);
    expect(isLoopbackRelay("ws://10.0.0.5:13412")).toBe(false);
  });
});

describe("join — scoped runtime metadata", () => {
  it("preserves local/docker and managed scope from the Relay response", async () => {
    mockFetch(() => ({
      jsonBody: {
        token: "header.payload.signature",
        room: "owner",
        deviceId: "executor-owner",
        sessionId: "session-a",
        executionTarget: "docker",
      },
    }));
    await expect(join("ws://127.0.0.1:13412", "ABCD2345", "alice")).resolves.toEqual({
      token: "header.payload.signature",
      room: "owner",
      deviceId: "executor-owner",
      sessionId: "session-a",
      executionTarget: "docker",
    });
  });
});

describe("createInvite — grade + request construction", () => {
  it("POSTs to {httpBase}/rooms/invites with only the Bearer header when no grade is set", async () => {
    const { calls } = mockFetch(() => ({
      jsonBody: { code: "ABCD", expiresAt: 5 },
    }));
    const res = await createInvite("ws://127.0.0.1:13412/ws/agent", "tkt");
    expect(calls[0].url).toBe("http://127.0.0.1:13412/rooms/invites");
    expect(calls[0].init.method).toBe("POST");
    expect(
      (calls[0].init.headers as Record<string, string>).Authorization,
    ).toBe("Bearer tkt");
    expect(calls[0].init.body).toBeUndefined();
    // A legacy server omitted the response grade and may have granted control;
    // never label that uncertain credential as view-only.
    expect(res).toEqual({ code: "ABCD", expiresAt: 5, access: "control" });
  });

  it("sends access in the JSON body when a grade is given", async () => {
    const { calls } = mockFetch(() => ({
      jsonBody: { code: "XY", expiresAt: 1, access: "view" },
    }));
    const res = await createInvite("wss://relay.example.com", "tkt", "view");
    expect(
      (calls[0].init.headers as Record<string, string>)["Content-Type"],
    ).toBe("application/json");
    expect(JSON.parse(calls[0].init.body as string)).toEqual({
      access: "view",
    });
    expect(res.access).toBe("view");
  });

  it("falls back to the requested grade when the relay does not echo access", async () => {
    mockFetch(() => ({ jsonBody: { code: "ZZ", expiresAt: 1 } }));
    const res = await createInvite("ws://h:1", "tkt", "control");
    expect(res.access).toBe("control");
  });
});

describe("enrollHost — request construction", () => {
  it("POSTs to {httpBase}/host/enroll with the ws→http converted origin", async () => {
    const { calls } = mockFetch(() => ({
      jsonBody: { token: "t", room: "r-x", expiresAt: 1 },
    }));
    await enrollHost("ws://127.0.0.1:13412/ws/agent", { secret: "s" });
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe("http://127.0.0.1:13412/host/enroll");
    expect(calls[0].init.method).toBe("POST");
    expect(
      (calls[0].init.headers as Record<string, string>)["Content-Type"],
    ).toBe("application/json");
  });

  it("sends only the secret when room and name are omitted", async () => {
    const { calls } = mockFetch(() => ({
      jsonBody: { token: "t", room: "r-gen", expiresAt: 1 },
    }));
    await enrollHost("wss://relay.example.com", { secret: "abc" });
    expect(JSON.parse(calls[0].init.body as string)).toEqual({ secret: "abc" });
  });

  it("includes trimmed room and name when provided, dropping blank ones", async () => {
    const { calls } = mockFetch(() => ({
      jsonBody: { token: "t", room: "myroom", expiresAt: 1 },
    }));
    await enrollHost("wss://relay.example.com", {
      secret: "abc",
      room: "  myroom  ",
      name: "   ",
    });
    expect(JSON.parse(calls[0].init.body as string)).toEqual({
      secret: "abc",
      room: "myroom",
    });
  });

  it("returns the token, room, and expiry from the relay response", async () => {
    mockFetch(() => ({
      jsonBody: { token: "jwt-123", room: "r-abc", expiresAt: 9999 },
    }));
    const res = await enrollHost("ws://h:1", { secret: "s" });
    expect(res).toEqual({ token: "jwt-123", room: "r-abc", expiresAt: 9999 });
  });

  it("sends paired device and session scope", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          token: "t",
          room: "r",
          expiresAt: 123,
          deviceId: "d",
          sessionId: "s",
        }),
        {
          status: 201,
        },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const result = await enrollHost("ws://127.0.0.1:13412/ws/agent", {
      deviceId: "d",
      sessionId: "s",
    });
    expect(
      JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string),
    ).toMatchObject({
      deviceId: "d",
      sessionId: "s",
    });
    expect(result).toMatchObject({ deviceId: "d", sessionId: "s" });
  });

  it("rejects an incomplete scope before making a request", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      enrollHost("ws://127.0.0.1:13412", { deviceId: "d" }),
    ).rejects.toThrow("기기 ID와 세션 ID는 함께");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("omits the secret entirely when it is absent (local keyless enroll)", async () => {
    const { calls } = mockFetch(() => ({
      jsonBody: { token: "t", room: "r-loc", expiresAt: 1 },
    }));
    await enrollHost("ws://127.0.0.1:13412", {});
    expect(JSON.parse(calls[0].init.body as string)).toEqual({});
  });

  it("omits a blank secret but keeps room/name", async () => {
    const { calls } = mockFetch(() => ({
      jsonBody: { token: "t", room: "myroom", expiresAt: 1 },
    }));
    await enrollHost("ws://127.0.0.1:13412", {
      secret: "   ",
      room: "myroom",
      name: "me",
    });
    expect(JSON.parse(calls[0].init.body as string)).toEqual({
      room: "myroom",
      name: "me",
    });
  });
});

describe("enrollHost — error handling", () => {
  it("maps 401 to a wrong-key message when a secret was sent", async () => {
    mockFetch(() => ({ ok: false, status: 401 }));
    await expect(enrollHost("ws://h:1", { secret: "bad" })).rejects.toThrow(
      /발급 키가 올바르지 않습니다/,
    );
  });

  it("maps 401/403 without a secret to a needs-key message (public relay)", async () => {
    mockFetch(() => ({ ok: false, status: 401 }));
    await expect(enrollHost("wss://relay.example.com", {})).rejects.toThrow(
      /이 릴레이는 발급 키가 필요합니다/,
    );
    mockFetch(() => ({ ok: false, status: 403 }));
    await expect(
      enrollHost("wss://relay.example.com", { secret: "  " }),
    ).rejects.toThrow(/이 릴레이는 발급 키가 필요합니다/);
  });

  it("maps 503 to an enrollment-disabled message", async () => {
    mockFetch(() => ({ ok: false, status: 503 }));
    await expect(enrollHost("ws://h:1", { secret: "s" })).rejects.toThrow(
      /비활성화/,
    );
  });

  it("wraps a network failure with the target base URL", async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new TypeError("network down");
    }) as unknown as typeof fetch;
    await expect(
      enrollHost("ws://127.0.0.1:13412/ws/agent", { secret: "s" }),
    ).rejects.toThrow(
      /릴레이에 연결할 수 없습니다 \(http:\/\/127\.0\.0\.1:13412\)/,
    );
  });

  it("reports a missing token in an otherwise-OK response", async () => {
    mockFetch(() => ({ jsonBody: { room: "r-x", expiresAt: 1 } }));
    await expect(enrollHost("ws://h:1", { secret: "s" })).rejects.toThrow(
      /토큰이 없습니다/,
    );
  });

  it("surfaces the status and body for other non-2xx responses", async () => {
    mockFetch(() => ({ ok: false, status: 500, jsonBody: "boom" }));
    await expect(enrollHost("ws://h:1", { secret: "s" })).rejects.toThrow(
      /방 만들기 실패 \(HTTP 500\): boom/,
    );
  });
});
