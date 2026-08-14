// relay.ts — HTTP join + URL helpers for reaching a relay. Kept separate from
// protocol.ts (pure/testable) and connection.ts (live socket) so the network
// surface is small and obvious.

// httpBaseFromWs converts a ws/wss relay address into its http/https ORIGIN
// (scheme://host[:port]), matching the design's "base = ws URL의 ws→http 변환"
// rule. The path is stripped — the relay-url is the agent endpoint (…/ws/agent),
// so keeping it would make /rooms/*, /host/enroll POST to
// http://host/ws/agent/host/enroll and fail. A scheme-less host defaults to ws://.
export function httpBaseFromWs(wsUrl: string): string {
  let u = wsUrl.trim();
  if (!/^wss?:\/\//i.test(u) && !/^https?:\/\//i.test(u)) {
    u = "ws://" + u;
  }
  // Keep only scheme://host[:port] — drop any /ws/agent path (and trailing slash).
  const m = u.match(/^(wss?|https?):\/\/[^/]+/i);
  if (m) u = m[0];
  else u = u.replace(/\/+$/, "");
  if (/^wss:/i.test(u)) return u.replace(/^wss:/i, "https:");
  if (/^ws:/i.test(u)) return u.replace(/^ws:/i, "http:");
  return u; // already http/https
}

// wsBaseNormalized returns the ws/wss origin (no trailing slash) for building
// the participant endpoint. A scheme-less host defaults to ws://.
export function wsBaseNormalized(wsUrl: string): string {
  let u = wsUrl.trim();
  if (!/^wss?:\/\//i.test(u) && !/^https?:\/\//i.test(u)) {
    u = "ws://" + u;
  }
  u = u.replace(/\/+$/, "");
  if (/^https:/i.test(u)) return u.replace(/^https:/i, "wss:");
  if (/^http:/i.test(u)) return u.replace(/^http:/i, "ws:");
  return u;
}

// wsOriginFromRelay strips any path (e.g. the agent endpoint's /ws/agent) from a
// relay URL, yielding the scheme://host[:port] origin that participantWsUrl
// appends /ws/participant to. Used to derive a join URL from the host panel's
// agent relay-url when opening the host's own room.
export function wsOriginFromRelay(relayUrl: string): string {
  const norm = wsBaseNormalized(relayUrl);
  const m = norm.match(/^(wss?:\/\/[^/]+)/i);
  return m ? m[1] : norm;
}

// Current Desktop clients keep the credential out of the URL so reverse
// proxies and crash reports cannot record it. The Relay negotiates this exact
// subprotocol. A server may explicitly opt into ?ticket= only for legacy migration.
export function participantWsEndpoint(wsUrl: string): string {
  return `${wsBaseNormalized(wsUrl)}/ws/participant`;
}

export function participantTicketProtocol(ticket: string): string {
  return `pie-relay.ticket.${ticket}`;
}

// isLoopbackRelay reports whether the relay address points at this machine
// (127.0.0.1 / localhost / ::1). The host panel uses it to tell the operator a
// local relay needs no enroll key, matching the relay's loopback-open enroll.
export function isLoopbackRelay(relayUrl: string): boolean {
  const origin = wsOriginFromRelay(relayUrl); // wss?://host[:port]
  const host = origin.replace(/^wss?:\/\//i, "").replace(/:\d+$/, "");
  const bare = host.replace(/^\[|\]$/g, "").toLowerCase(); // strip IPv6 brackets
  return bare === "127.0.0.1" || bare === "localhost" || bare === "::1";
}

export interface JoinResult {
  token: string;
  room: string;
  deviceId?: string;
  sessionId?: string;
  executionTarget?: "local" | "docker";
}

// InviteAccess is the grade a code grants its guest: "view" is watch-only (the
// relay drops the guest's input-bearing messages), "control" lets the guest type
// (and can acquire the terminal driver). Current Relay defaults an omitted
// invite grade to view; a legacy response that omits its granted grade is
// conservatively displayed as control so the UI never understates authority.
export type InviteAccess = "view" | "control";

export interface InviteResult {
  code: string;
  expiresAt: number; // unix seconds
  access: InviteAccess; // grade this code grants (legacy missing-field fallback: control)
}

// createInvite performs POST {httpBase}/rooms/invites with the host's manual
// ticket as a Bearer credential and returns the minted invite code. This is the
// manual-ticket path used by the host panel: `client room create` only reads
// credentials.json and has no ticket override, so when the host authenticates
// with a RELAY_TICKET we call the relay directly from the webview instead of
// via the sidecar. Mirrors rooms.CreateInvite in the Go client.
//
// When `access` is given it rides in the JSON body so the relay stamps the
// code's grade; the sidecar path (`client room create`) can't carry it, which is
// why grade selection routes through this manual-ticket call.
export async function createInvite(
  wsUrl: string,
  ticket: string,
  access?: InviteAccess,
): Promise<InviteResult> {
  const base = httpBaseFromWs(wsUrl);
  const headers: Record<string, string> = { Authorization: `Bearer ${ticket}` };
  let body: string | undefined;
  if (access) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify({ access });
  }
  let resp: Response;
  try {
    resp = await fetch(`${base}/rooms/invites`, {
      method: "POST",
      headers,
      body,
    });
  } catch {
    throw new Error(
      `릴레이에 연결할 수 없습니다 (${base}). 주소와 릴레이 실행 여부를 확인하세요.`,
    );
  }
  if (resp.status === 401 || resp.status === 403) {
    throw new Error(
      "초대 코드 발급이 거부되었습니다 — 티켓이 유효한 호스트 자격인지 확인하세요.",
    );
  }
  if (!resp.ok) {
    const errBody = (await resp.text().catch(() => "")).trim();
    throw new Error(
      `초대 코드 발급 실패 (HTTP ${resp.status})${errBody ? `: ${errBody}` : ""}`,
    );
  }
  const data = (await resp.json()) as Partial<InviteResult>;
  if (!data.code) {
    throw new Error("릴레이 응답에 초대 코드가 없습니다.");
  }
  // Prefer the grade the relay echoes; fall back to what we requested, then the
  // relay's own "control" default (unset access).
  const granted: InviteAccess =
    data.access === "view" || data.access === "control"
      ? data.access
      : (access ?? "control");
  return { code: data.code, expiresAt: data.expiresAt ?? 0, access: granted };
}

export interface EnrollResult {
  token: string;
  room: string;
  expiresAt: number; // unix seconds
  deviceId?: string;
  sessionId?: string;
  executionTarget?: "local" | "docker";
}

export interface EnrollOptions {
  // Enroll secret (HOST_ENROLL_SECRET). Optional: a blank/omitted secret is left
  // out of the body so a loopback relay can enroll keyless (docs say the relay
  // allows no key for same-PC requests). A public relay still rejects a keyless
  // request with 401/403.
  secret?: string;
  room?: string; // optional; relay generates one when empty
  name?: string; // optional; relay defaults to "host"
  deviceId?: string; // optional, but deviceId/sessionId must be supplied together
  sessionId?: string;
}

// enrollHost performs POST {httpBase}/host/enroll with {secret?, room?, name?}
// and returns the minted host token + room + expiry. This is the GUI "방 만들기"
// path: the operator's HOST_ENROLL_SECRET is exchanged for a room and a host JWT
// that the panel saves as its manual ticket (replacing the old vibe-canvas
// browser login). Only fields the caller actually set are sent — a blank secret
// is omitted entirely so a local relay can enroll without a key — and the relay
// applies its own defaults for room/name. A wrong key (secret sent) or a keyless
// request a public relay rejects both surface as 401/403; 503 means enrollment
// is disabled (no HOST_ENROLL_SECRET). Mirrors relay.Enroller.handleEnroll.
export async function enrollHost(
  relayUrl: string,
  opts: EnrollOptions,
): Promise<EnrollResult> {
  const base = httpBaseFromWs(relayUrl);
  const secret = opts.secret?.trim() ?? "";
  const body: Record<string, string> = {};
  if (secret) body.secret = secret;
  const room = opts.room?.trim();
  const name = opts.name?.trim();
  if (room) body.room = room;
  if (name) body.name = name;
  const deviceId = opts.deviceId?.trim();
  const sessionId = opts.sessionId?.trim();
  if ((deviceId && !sessionId) || (!deviceId && sessionId)) {
    throw new Error("기기 ID와 세션 ID는 함께 지정해야 합니다.");
  }
  if (deviceId && sessionId) {
    body.deviceId = deviceId;
    body.sessionId = sessionId;
  }
  let resp: Response;
  try {
    resp = await fetch(`${base}/host/enroll`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    throw new Error(
      `릴레이에 연결할 수 없습니다 (${base}). 주소와 릴레이 실행 여부를 확인하세요.`,
    );
  }
  if (resp.status === 401 || resp.status === 403) {
    // A key was sent but rejected → wrong key. No key sent → the relay isn't
    // loopback-open, so it needs the operator's key.
    throw new Error(
      secret
        ? "발급 키가 올바르지 않습니다."
        : "이 릴레이는 발급 키가 필요합니다 — 공개 릴레이면 운영자에게 발급 키를 받아 입력하세요.",
    );
  }
  if (resp.status === 503) {
    throw new Error(
      "릴레이에서 방 만들기가 비활성화되어 있습니다 (운영자가 HOST_ENROLL_SECRET 를 설정해야 합니다).",
    );
  }
  if (!resp.ok) {
    const text = (await resp.text().catch(() => "")).trim();
    throw new Error(
      `방 만들기 실패 (HTTP ${resp.status})${text ? `: ${text}` : ""}`,
    );
  }
  const data = (await resp.json()) as Partial<EnrollResult>;
  if (!data.token) {
    throw new Error("릴레이 응답에 토큰이 없습니다.");
  }
  return {
    token: data.token,
    room: data.room ?? "",
    expiresAt: data.expiresAt ?? 0,
    deviceId: data.deviceId,
    sessionId: data.sessionId,
    executionTarget: data.executionTarget,
  };
}

// join performs POST {httpBase}/rooms/join with {code, name} and returns the
// minted participant token + room. On a non-2xx response it throws an Error
// whose message is friendly for the connect screen (the relay returns 401 for
// an invalid/expired code).
export async function join(
  wsUrl: string,
  code: string,
  name: string,
  signal?: AbortSignal,
): Promise<JoinResult> {
  const base = httpBaseFromWs(wsUrl);
  let resp: Response;
  try {
    resp = await fetch(`${base}/rooms/join`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code, name }),
      signal,
    });
  } catch (e) {
    throw new Error(
      `릴레이에 연결할 수 없습니다 (${base}). 주소와 릴레이 실행 여부를 확인하세요.`,
    );
  }

  if (!resp.ok) {
    if (resp.status === 401) {
      throw new Error("초대 코드가 올바르지 않거나 만료되었습니다.");
    }
    const body = (await resp.text().catch(() => "")).trim();
    throw new Error(
      `참가 실패 (HTTP ${resp.status})${body ? `: ${body}` : ""}`,
    );
  }

  const data = (await resp.json()) as Partial<JoinResult>;
  if (!data.token) {
    throw new Error("릴레이 응답에 토큰이 없습니다.");
  }
  return {
    token: data.token,
    room: data.room ?? "",
    deviceId: data.deviceId,
    sessionId: data.sessionId,
    executionTarget: data.executionTarget,
  };
}
