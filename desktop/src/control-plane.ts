// Typed browser client for Pie Relay's user-facing Control Plane API. The PAT
// is supplied by the external service and is never put in query strings.

export interface ManagedSessionRequest {
  deviceId: string;
  deviceName: string;
  sessionName?: string;
  executionTarget: "local" | "docker";
  accessMode: "private" | "shared";
  transportMode: "auto" | "lan" | "relay";
  ownerUserId?: string;
  applicationId?: string;
  poolId?: string;
  tenantId?: string;
  resourceType?: string;
  resourceId?: string;
  agentId?: string;
  protocol?: "terminal" | "acp";
}
export interface ManagedSessionResult {
  sessionId: string;
  deviceId: string;
  ownerUserId: string;
  token: string;
  room: string;
  relayUrl?: string;
  expiresAt: number;
  executionTarget: "local" | "docker";
}

export interface ControlIdentity {
  userId: string;
  organizationId?: string;
  roles: string[];
  admin: boolean;
}

export interface ManagedDeviceSummary {
  id: string;
  ownerUserId: string;
  name: string;
  kind: "local" | "docker";
  observedState: string;
  clientConnected?: boolean;
  relayRegistered: boolean;
  activeSessions?: number;
  metadata?: Record<string, string>;
}

export interface ManagedSessionSummary {
  id: string;
  ownerUserId: string;
  deviceId: string;
  applicationId?: string;
  poolId?: string;
  tenantId?: string;
  resourceType?: string;
  resourceId?: string;
  agentId?: string;
  protocol?: "terminal" | "acp";
  name?: string;
  executionTarget: "local" | "docker";
  accessMode: "private" | "shared";
  transportMode: "auto" | "lan" | "relay";
  selectedTransport?: string;
  status: string;
  lastError?: string;
}

export interface ManagedGrantSummary {
  id: string;
  ownerUserId: string;
  subjectUserId: string;
  targetDeviceId: string;
  sessionId?: string;
  access: "view" | "control";
  expiresAt: string;
  revokedAt?: string;
}

export interface ManagedResourceList {
  identity: ControlIdentity;
  devices: ManagedDeviceSummary[];
  sessions: ManagedSessionSummary[];
  grants: ManagedGrantSummary[];
}

export interface ManagedSessionCreateRequest {
  name?: string;
  accessMode: "private" | "shared";
  applicationId?: string;
  poolId?: string;
  tenantId?: string;
  resourceType?: string;
  resourceId?: string;
  agentId?: string;
  protocol?: "terminal" | "acp";
}

interface SessionResponse {
  id: string;
  ownerUserId: string;
  deviceId: string;
}

interface CredentialResponse {
  token: string;
  protocolVersion?: number;
  room?: string;
  role?: "host" | "participant";
  access?: "view" | "control";
  expiresAt: string | number;
  relayUrl?: string;
  executionTarget?: "local" | "docker";
}

function controlOrigin(value: string): string {
  const raw = value.trim();
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    throw new Error(
      "Control Plane 주소는 http:// 또는 https:// 주소여야 합니다.",
    );
  }
  if (
    (parsed.protocol !== "http:" && parsed.protocol !== "https:") ||
    !parsed.host
  ) {
    throw new Error(
      "Control Plane 주소는 http:// 또는 https:// 주소여야 합니다.",
    );
  }
  return parsed.origin;
}

async function requestJSON<T>(
  base: string,
  token: string,
  path: string,
  init: RequestInit,
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${controlOrigin(base)}${path}`, {
      ...init,
      // Device/session state changes while this screen is polling. WebKit may
      // otherwise reuse a previous GET response and keep a newly online Host
      // OS agent stuck as "offline" until its HTTP cache expires.
      cache: "no-store",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
        ...(init.headers ?? {}),
      },
    });
  } catch {
    throw new Error(
      "Control Plane에 연결할 수 없습니다. 주소와 서버 상태를 확인하세요.",
    );
  }
  if (response.status === 401) throw new Error("PAT 인증에 실패했습니다.");
  if (response.status === 403)
    throw new Error("이 작업을 수행할 권한이 없습니다.");
  if (!response.ok) {
    const message = (await response.text().catch(() => "")).trim();
    throw new Error(
      `Control Plane 요청 실패 (HTTP ${response.status})${message ? `: ${message}` : ""}`,
    );
  }
  return (await response.json()) as T;
}

export async function createManagedHostSession(
  base: string,
  pat: string,
  value: ManagedSessionRequest,
): Promise<ManagedSessionResult> {
  if (!pat.trim()) throw new Error("외부 서비스에서 발급한 PAT를 입력하세요.");
  if (!value.deviceId.trim()) throw new Error("기기 ID가 필요합니다.");

  const wantsResourceContext = Boolean(
    value.applicationId?.trim() || value.poolId?.trim(),
  );
  if (
    wantsResourceContext &&
    (!value.applicationId?.trim() || !value.poolId?.trim())
  ) {
    throw new Error("Application ID와 Relay Pool ID를 함께 입력하세요.");
  }
  let tenantId = value.tenantId?.trim() || value.ownerUserId?.trim() || "";
  if (wantsResourceContext && !tenantId) {
    const identity = await requestJSON<ControlIdentity>(
      base,
      pat,
      "/v1/control/me",
      { method: "GET" },
    );
    tenantId = identity.organizationId || identity.userId;
  }

  await requestJSON(base, pat, "/v1/control/devices/register", {
    method: "POST",
    body: JSON.stringify({
      id: value.deviceId,
      ownerUserId: value.ownerUserId || undefined,
      name: value.deviceName || value.deviceId,
      // Keep the browser contract identical to Control Plane's canonical
      // enum.  The old desktop/container aliases were never accepted by the
      // server and made every real managed-session request fail before the
      // session was created.
      kind: value.executionTarget === "docker" ? "docker" : "local",
      desiredState: "running",
      observedState: "starting",
      clientConnected: true,
      metadata: { client: "pie-relay-desktop" },
    }),
  });

  const session = await requestJSON<SessionResponse>(
    base,
    pat,
    "/v1/control/sessions",
    {
      method: "POST",
      body: JSON.stringify({
        ownerUserId: value.ownerUserId || undefined,
        deviceId: value.deviceId,
        name: value.sessionName || undefined,
        executionTarget: value.executionTarget,
        accessMode: value.accessMode,
        transportMode: value.transportMode,
        ...(wantsResourceContext
          ? {
              applicationId: value.applicationId?.trim(),
              poolId: value.poolId?.trim(),
              tenantId,
              resourceType: value.resourceType?.trim() || "device",
              resourceId: value.resourceId?.trim() || value.deviceId,
              agentId: value.agentId?.trim() || undefined,
              protocol: value.protocol || "terminal",
            }
          : {}),
        status: "starting",
      }),
    },
  );
  if (!session.id || !session.deviceId)
    throw new Error("Control Plane 세션 응답이 올바르지 않습니다.");

  const credential = await requestJSON<CredentialResponse>(
    base,
    pat,
    `/v1/control/sessions/${encodeURIComponent(session.id)}/credential`,
    {
      method: "POST",
      body: JSON.stringify({
        subjectUserId: session.ownerUserId || value.ownerUserId || undefined,
        role: "host",
        access: "control",
        ttlSeconds: 24 * 60 * 60,
      }),
    },
  );
  if (!credential.token)
    throw new Error("Control Plane 자격 응답에 토큰이 없습니다.");
  const expiresAt =
    typeof credential.expiresAt === "number"
      ? credential.expiresAt
      : Math.floor(new Date(credential.expiresAt).getTime() / 1000);
  return {
    sessionId: session.id,
    deviceId: session.deviceId,
    ownerUserId: session.ownerUserId,
    token: credential.token,
    room: credential.room || session.ownerUserId,
    relayUrl: credential.relayUrl,
    expiresAt: Number.isFinite(expiresAt) ? expiresAt : 0,
    executionTarget:
      credential.executionTarget === "docker" ? "docker" : "local",
  };
}

export async function listManagedResources(
  base: string,
  pat: string,
): Promise<ManagedResourceList> {
  if (!pat.trim()) throw new Error("외부 서비스에서 발급한 PAT를 입력하세요.");
  const get = <T>(path: string) =>
    requestJSON<T>(base, pat, path, { method: "GET" });
  const [identity, devices, sessions, grants] = await Promise.all([
    get<ControlIdentity>("/v1/control/me"),
    get<ManagedDeviceSummary[]>("/v1/control/devices"),
    get<ManagedSessionSummary[]>("/v1/control/sessions"),
    get<ManagedGrantSummary[]>("/v1/control/grants"),
  ]);
  return { identity, devices, sessions, grants };
}

export async function createManagedDeviceSession(
  base: string,
  pat: string,
  device: ManagedDeviceSummary,
  value: ManagedSessionCreateRequest,
): Promise<ManagedSessionSummary> {
  if (!pat.trim()) throw new Error("외부 서비스에서 발급한 PAT를 입력하세요.");
  if (!device.id || (device.kind !== "local" && device.kind !== "docker")) {
    throw new Error("세션을 실행할 장치가 올바르지 않습니다.");
  }
  if (Boolean(value.applicationId?.trim()) !== Boolean(value.poolId?.trim())) {
    throw new Error("Application ID와 Relay Pool ID를 함께 입력하세요.");
  }
  return requestJSON<ManagedSessionSummary>(
    base,
    pat,
    "/v1/control/sessions",
    {
      method: "POST",
      body: JSON.stringify({
        deviceId: device.id,
        name: value.name?.trim() || undefined,
        executionTarget: device.kind,
        accessMode: value.accessMode,
        transportMode: "relay",
        ...(value.applicationId?.trim() && value.poolId?.trim()
          ? {
              applicationId: value.applicationId.trim(),
              poolId: value.poolId.trim(),
              tenantId: value.tenantId?.trim(),
              resourceType: value.resourceType?.trim() || "device",
              resourceId: value.resourceId?.trim() || device.id,
              agentId: value.agentId?.trim() || undefined,
              protocol: value.protocol || "terminal",
            }
          : {}),
        status: "starting",
      }),
    },
  );
}

export async function issueManagedParticipantCredential(
  base: string,
  pat: string,
  session: ManagedSessionSummary,
  identity: ControlIdentity,
  requestedAccess: "view" | "control",
): Promise<{
  token: string;
  room: string;
  asHost: boolean;
  access: "view" | "control";
  relayUrl?: string;
  executionTarget: "local" | "docker";
}> {
  if (!pat.trim()) throw new Error("외부 서비스에서 발급한 PAT를 입력하세요.");
  const asHost = session.ownerUserId === identity.userId;
  const access = asHost ? "control" : requestedAccess;
  const credential = await requestJSON<CredentialResponse>(
    base,
    pat,
    `/v1/control/sessions/${encodeURIComponent(session.id)}/credential`,
    {
      method: "POST",
      body: JSON.stringify({
        role: asHost ? "host" : "participant",
        access,
        ttlSeconds: asHost ? 24 * 60 * 60 : 60 * 60,
      }),
    },
  );
  if (!credential.token)
    throw new Error("Control Plane 자격 응답에 토큰이 없습니다.");
  return {
    token: credential.token,
    room: credential.room || session.ownerUserId,
    asHost,
    access: credential.access || access,
    relayUrl: credential.relayUrl,
    executionTarget:
      credential.executionTarget === "docker"
        ? "docker"
        : session.executionTarget,
  };
}

export async function createManagedGrant(
  base: string,
  pat: string,
  session: ManagedSessionSummary,
  subjectUserId: string,
  access: "view" | "control",
  ttlHours = 24,
): Promise<ManagedGrantSummary> {
  const subject = subjectUserId.trim();
  if (!subject) throw new Error("공유할 사용자 ID를 입력하세요.");
  const boundedTTL = Math.min(24 * 30, Math.max(1, Math.floor(ttlHours)));
  return requestJSON<ManagedGrantSummary>(base, pat, "/v1/control/grants", {
    method: "POST",
    body: JSON.stringify({
      subjectUserId: subject,
      targetDeviceId: session.deviceId,
      sessionId: session.id,
      access,
      expiresAt: new Date(
        Date.now() + boundedTTL * 60 * 60 * 1000,
      ).toISOString(),
    }),
  });
}

export async function revokeManagedGrant(
  base: string,
  pat: string,
  grantId: string,
): Promise<void> {
  await requestJSON<ManagedGrantSummary>(
    base,
    pat,
    `/v1/control/grants/${encodeURIComponent(grantId)}/revoke`,
    { method: "POST", body: "{}" },
  );
}
