import { useCallback, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import {
  createManagedDeviceSession,
  createManagedGrant,
  issueManagedParticipantCredential,
  listManagedResources,
  revokeManagedGrant,
  type ManagedResourceList,
  type ManagedDeviceSummary,
  type ManagedSessionSummary,
} from "./control-plane";
import { join } from "./relay";
import {
  deviceIdFromToken,
  executionTargetFromToken,
  relaySessionIdFromToken,
  roomFromToken,
} from "./protocol";
import pieLabMark from "./assets/pielab-mark-ondark.png";
import { UiIcon } from "./UiIcon";
import {
  isTauriDesktop,
  localDeviceIdentity,
} from "./device-identity";
import type { RelayConfig } from "./relay-config";

export interface ConnectValues {
  wsUrl: string;
  code: string;
  name: string;
  label?: string;
  executionTarget?: "local" | "docker";
  deviceId?: string;
  relaySessionId?: string;
}

const LS_KEY = "pie-relay.connect";
const LEGACY_LS_KEY = "cli-relay.connect";

interface SavedConnect extends ConnectValues {
  relayEndpointKey: string;
  asHost: boolean;
  controlUrl?: string;
}

function isConnectableSession(session: ManagedSessionSummary): boolean {
  return (
    session.status === "ready" ||
    session.status === "active" ||
    session.status === "idle"
  );
}

function wait(milliseconds: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

function deviceTitle(
  device: ManagedDeviceSummary,
  currentDeviceId: string,
): string {
  const location =
    device.id === currentDeviceId
      ? "이 PC"
      : device.name || device.id;
  return `${location} · ${device.kind === "docker" ? "Docker" : "Host OS"}`;
}

// loadSaved restores the last-used relay address / code / name / host-mode from
// localStorage (design: "최근 접속값 localStorage 기억").
function loadSaved(relayConfig: RelayConfig): SavedConnect {
  const fallback: SavedConnect = {
    relayEndpointKey: relayConfig.storageKey,
    wsUrl: relayConfig.participantUrl,
    code: "",
    name: "",
    asHost: false,
    controlUrl: "http://127.0.0.1:19090",
  };
  try {
    const raw =
      localStorage.getItem(LS_KEY) || localStorage.getItem(LEGACY_LS_KEY);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as Partial<SavedConnect>;
    const sameRelayEndpoint = v.relayEndpointKey === relayConfig.storageKey;
    return {
      relayEndpointKey: relayConfig.storageKey,
      wsUrl: sameRelayEndpoint
        ? v.wsUrl || relayConfig.participantUrl
        : relayConfig.participantUrl,
      // An invite code is Relay-local. Clear it when the profile changes.
      code: sameRelayEndpoint ? v.code || "" : "",
      name: v.name || "",
      asHost: sameRelayEndpoint && Boolean(v.asHost),
      controlUrl: v.controlUrl || fallback.controlUrl,
    };
  } catch {
    return fallback;
  }
}

function save(
  v: Omit<SavedConnect, "relayEndpointKey">,
  relayConfig: RelayConfig,
): void {
  try {
    localStorage.setItem(
      LS_KEY,
      JSON.stringify({ ...v, relayEndpointKey: relayConfig.storageKey }),
    );
  } catch {
    /* private mode / disabled storage — non-fatal */
  }
}

// hostJoinArgs computes the onJoined arguments for the advanced "paste a host
// token" path, or an error when a required field is missing. Pure (token → room
// decode) so it can be unit-tested without rendering the component.
export function hostJoinArgs(
  values: ConnectValues,
  rawToken: string,
): { token: string; room: string } | { error: string } {
  const token = rawToken.trim();
  if (!values.wsUrl || !values.name || !token) {
    return { error: "릴레이 주소, 호스트 토큰, 이름을 모두 입력하세요." };
  }
  return { token, room: roomFromToken(token) };
}

interface Props {
  relayConfig: RelayConfig;
  onJoined: (
    values: ConnectValues,
    token: string,
    room: string,
    asHost: boolean,
  ) => void;
  // P4: shown as "취소" when at least one room is already open, so adding a
  // room can be abandoned and the previously active room restored.
  onCancel?: () => void;
}

// ConnectScreen has two join paths. The default (guest) path collects the relay
// address, invite code, and name, then calls POST /rooms/join. The advanced
// "다른 기기: 호스트 토큰 붙여넣기" path takes a host JWT pasted from another
// machine and joins that token's room as host — the relay reads room/role from
// the token, so there is no round-trip and no invite code. The pasted token is a
// credential and is never persisted (unlike the same-PC host panel ticket). For
// the same PC, the host panel's "이 방 열기" button is the unambiguous route.
export function ConnectScreen({ relayConfig, onJoined, onCancel }: Props) {
  const saved = loadSaved(relayConfig);
  const [wsUrl, setWsUrl] = useState(saved.wsUrl);
  const [code, setCode] = useState(saved.code);
  const [name, setName] = useState(saved.name);
  const [asHost, setAsHost] = useState(saved.asHost);
  const [source, setSource] = useState<"invite" | "control">("invite");
  const [managedAction, setManagedAction] = useState<"connect" | "create">(
    "connect",
  );
  const [controlUrl, setControlUrl] = useState(
    saved.controlUrl || "http://127.0.0.1:19090",
  );
  const [controlPAT, setControlPAT] = useState("");
  const [managed, setManaged] = useState<ManagedResourceList | null>(null);
  const [selectedSessionId, setSelectedSessionId] = useState("");
  const [localDevice] = useState(() => localDeviceIdentity());
  const [newTargetDeviceId, setNewTargetDeviceId] = useState("");
  const [newSessionName, setNewSessionName] = useState("");
  const [newSessionAccess, setNewSessionAccess] = useState<
    "private" | "shared"
  >("private");
  const [createProgress, setCreateProgress] = useState("");
  const [requestedAccess, setRequestedAccess] = useState<"view" | "control">(
    "view",
  );
  const [shareUserId, setShareUserId] = useState("");
  const [shareAccess, setShareAccess] = useState<"view" | "control">("view");
  // Pasted host token (advanced path). Deliberately not restored from / saved to
  // localStorage — it is a credential and clarity beats convenience here.
  const [hostToken, setHostToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadManaged = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      const result = await listManagedResources(controlUrl, controlPAT);
      setManaged(result);
      const connectable = result.sessions.filter(isConnectableSession);
      setSelectedSessionId((current) =>
        connectable.some((session) => session.id === current)
          ? current
          : (connectable[0]?.id ?? ""),
      );
      if (connectable.length === 0) {
        setManagedAction("create");
      }
      const owned = result.devices.filter(
        (device) => device.ownerUserId === result.identity.userId,
      );
      setNewTargetDeviceId((current) =>
        owned.some((device) => device.id === current)
          ? current
          : (owned.find((device) => device.id === localDevice.id)?.id ??
            (isTauriDesktop() ? localDevice.id : owned[0]?.id ?? "")),
      );
    } catch (err) {
      setManaged(null);
      setSelectedSessionId("");
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [controlUrl, controlPAT, localDevice]);

  const submit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      const values: ConnectValues = {
        wsUrl: wsUrl.trim(),
        code: code.trim(),
        name: name.trim(),
      };
      if (source === "control") {
        if (managedAction === "create") return;
        if (!values.wsUrl || !values.name) {
          setError("릴레이 주소와 이름을 입력하세요.");
          return;
        }
        const selected = managed?.sessions.find(
          (session) => session.id === selectedSessionId,
        );
        if (!managed || !selected) {
          setError("먼저 Control Plane에서 세션 목록을 불러와 선택하세요.");
          return;
        }
        setBusy(true);
        setError(null);
        try {
          const credential = await issueManagedParticipantCredential(
            controlUrl,
            controlPAT,
            selected,
            managed.identity,
            requestedAccess,
          );
          const joinedValues = {
            ...values,
            wsUrl: credential.relayUrl || values.wsUrl,
            label: selected.name || selected.id,
            executionTarget: credential.executionTarget,
            deviceId: selected.deviceId,
            relaySessionId: selected.id,
          };
          save({ ...joinedValues, asHost: false, controlUrl }, relayConfig);
          onJoined(
            joinedValues,
            credential.token,
            credential.room,
            credential.asHost,
          );
        } catch (err) {
          setError(err instanceof Error ? err.message : String(err));
        } finally {
          setBusy(false);
        }
        return;
      }
      if (asHost) {
        // Advanced path: join the pasted token's room as host. Synchronous —
        // just decode the room claim; the relay authorizes the token on connect.
        const res = hostJoinArgs(values, hostToken);
        if ("error" in res) {
          setError(res.error);
          return;
        }
        setError(null);
        // Persist the relay/name/mode for convenience, but never the token.
        save({ ...values, asHost, controlUrl }, relayConfig);
        onJoined(
          {
            ...values,
            executionTarget: executionTargetFromToken(res.token),
            deviceId: deviceIdFromToken(res.token),
            relaySessionId: relaySessionIdFromToken(res.token),
          },
          res.token,
          res.room,
          true,
        );
        return;
      }
      if (!values.wsUrl || !values.name || !values.code) {
        setError("릴레이 주소, 초대 코드, 이름을 모두 입력하세요.");
        return;
      }
      setBusy(true);
      setError(null);
      try {
        const joined = await join(
          values.wsUrl,
          values.code,
          values.name,
        );
        save({ ...values, asHost, controlUrl }, relayConfig);
        onJoined(
          {
            ...values,
            executionTarget:
              joined.executionTarget ??
              executionTargetFromToken(joined.token),
            deviceId: joined.deviceId ?? deviceIdFromToken(joined.token),
            relaySessionId:
              joined.sessionId ?? relaySessionIdFromToken(joined.token),
          },
          joined.token,
          joined.room,
          false,
        );
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    },
    [
      wsUrl,
      code,
      name,
      source,
      asHost,
      hostToken,
      onJoined,
      managed,
      selectedSessionId,
      controlUrl,
      controlPAT,
      requestedAccess,
      managedAction,
      relayConfig,
    ],
  );

  const connectableManagedSessions =
    managed?.sessions.filter(isConnectableSession) ?? [];
  const selectedManagedSession = connectableManagedSessions.find(
    (session) => session.id === selectedSessionId,
  );
  const selectedOwned = Boolean(
    selectedManagedSession &&
    selectedManagedSession.ownerUserId === managed?.identity.userId,
  );
  const selectedGrant = managed?.grants.find(
    (grant) =>
      grant.subjectUserId === managed.identity.userId &&
      grant.targetDeviceId === selectedManagedSession?.deviceId &&
      (!grant.sessionId || grant.sessionId === selectedManagedSession?.id) &&
      !grant.revokedAt &&
      new Date(grant.expiresAt).getTime() > Date.now(),
  );
  const canRequestControl =
    selectedOwned || selectedGrant?.access === "control";
  const ownedSessionGrants =
    managed?.grants.filter(
      (grant) =>
        grant.ownerUserId === managed.identity.userId &&
        grant.sessionId === selectedManagedSession?.id &&
        !grant.revokedAt &&
        new Date(grant.expiresAt).getTime() > Date.now(),
    ) ?? [];

  const creatableDevices: ManagedDeviceSummary[] = managed
    ? (() => {
        const owned = managed.devices.filter(
          (device) => device.ownerUserId === managed.identity.userId,
        );
        if (
          isTauriDesktop() &&
          !owned.some((device) => device.id === localDevice.id)
        ) {
          owned.unshift({
            id: localDevice.id,
            ownerUserId: managed.identity.userId,
            name: localDevice.name,
            kind: "local",
            observedState: "offline",
            clientConnected: false,
            relayRegistered: false,
          });
        }
        return owned.sort((left, right) => {
          if (left.id === localDevice.id) return -1;
          if (right.id === localDevice.id) return 1;
          return left.name.localeCompare(right.name);
        });
      })()
    : [];

  const createSelectedSession = useCallback(async () => {
    if (!managed) return;
    let device = creatableDevices.find(
      (candidate) => candidate.id === newTargetDeviceId,
    );
    if (!device) {
      setError("세션을 실행할 장치를 선택하세요.");
      return;
    }
    setBusy(true);
    setError(null);
    setCreateProgress("대상 장치를 확인하는 중…");
    try {
      let latest = managed;
      if (device.kind === "local" && device.id === localDevice.id) {
        if (!isTauriDesktop()) {
          throw new Error("이 PC Host OS 세션은 Desktop 앱에서만 만들 수 있습니다.");
        }
        setCreateProgress("이 PC 장치 에이전트를 시작하는 중…");
        await invoke("device_agent_start", {
          controlUrl,
          controlToken: controlPAT,
          deviceId: localDevice.id,
          deviceName: localDevice.name,
        });
        for (let attempt = 0; attempt < 24; attempt++) {
          latest = await listManagedResources(controlUrl, controlPAT);
          const registered = latest.devices.find(
            (candidate) => candidate.id === localDevice.id,
          );
          if (registered?.clientConnected) {
            device = registered;
            break;
          }
          await wait(500);
        }
        if (!device.clientConnected) {
          throw new Error("이 PC 장치 에이전트가 Control Plane에 등록되지 않았습니다.");
        }
      } else if (device.kind === "local" && !device.clientConnected) {
        throw new Error(
          `${device.name || device.id}의 clientd가 오프라인입니다. 대상 PC에서 장치 에이전트를 실행하세요.`,
        );
      }

      setCreateProgress(
        device.kind === "docker"
          ? "Docker 세션을 준비하는 중…"
          : "Host OS 세션을 준비하는 중…",
      );
      const created = await createManagedDeviceSession(
        controlUrl,
        controlPAT,
        device,
        {
          name: newSessionName,
          accessMode: newSessionAccess,
          applicationId:
            import.meta.env.VITE_PIE_RELAY_APPLICATION_ID?.trim() || undefined,
          poolId: import.meta.env.VITE_PIE_RELAY_POOL_ID?.trim() || undefined,
          tenantId:
            import.meta.env.VITE_PIE_RELAY_TENANT_ID?.trim() ||
            latest.identity.organizationId ||
            latest.identity.userId,
          resourceType: "device",
          resourceId: device.id,
          protocol: "terminal",
        },
      );
      let ready: ManagedSessionSummary | undefined;
      for (let attempt = 0; attempt < 90; attempt++) {
        latest = await listManagedResources(controlUrl, controlPAT);
        const current = latest.sessions.find(
          (session) => session.id === created.id,
        );
        if (current && isConnectableSession(current)) {
          ready = current;
          break;
        }
        if (current?.status === "error" || current?.status === "closed") {
          throw new Error(
            current.lastError || `세션 시작 실패 (${current.status})`,
          );
        }
        await wait(500);
      }
      if (!ready) {
        throw new Error(
          "세션 시작 시간이 초과되었습니다. 장치 상태에서 진행 상황을 확인하세요.",
        );
      }
      setManaged(latest);
      setSelectedSessionId(ready.id);
      setCreateProgress("Relay 자격을 발급하는 중…");
      const credential = await issueManagedParticipantCredential(
        controlUrl,
        controlPAT,
        ready,
        latest.identity,
        "control",
      );
      const joinedValues: ConnectValues = {
        wsUrl: credential.relayUrl || wsUrl,
        code: "",
        name: name.trim() || "나",
        label: ready.name || ready.id,
        executionTarget: ready.executionTarget,
        deviceId: ready.deviceId,
        relaySessionId: ready.id,
      };
      save({ ...joinedValues, asHost: false, controlUrl }, relayConfig);
      onJoined(
        joinedValues,
        credential.token,
        credential.room,
        credential.asHost,
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
      setCreateProgress("");
    }
  }, [
    managed,
    creatableDevices,
    newTargetDeviceId,
    localDevice,
    controlUrl,
    controlPAT,
    newSessionName,
    newSessionAccess,
    wsUrl,
    name,
    onJoined,
    relayConfig,
  ]);

  const shareSelectedSession = useCallback(async () => {
    if (!managed || !selectedManagedSession || !selectedOwned) return;
    setBusy(true);
    setError(null);
    try {
      await createManagedGrant(
        controlUrl,
        controlPAT,
        selectedManagedSession,
        shareUserId,
        shareAccess,
      );
      setShareUserId("");
      setManaged(await listManagedResources(controlUrl, controlPAT));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }, [
    managed,
    selectedManagedSession,
    selectedOwned,
    controlUrl,
    controlPAT,
    shareUserId,
    shareAccess,
  ]);

  const revokeSelectedGrant = useCallback(
    async (grantId: string) => {
      setBusy(true);
      setError(null);
      try {
        await revokeManagedGrant(controlUrl, controlPAT, grantId);
        setManaged(await listManagedResources(controlUrl, controlPAT));
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    },
    [controlUrl, controlPAT],
  );

  return (
    <div className="screen connect">
      <div className="connect-layout">
        <section className="connect-intro" aria-labelledby="connect-title">
          <div className="connect-intro-brand">
            <span className="connect-intro-mark">
              <img src={pieLabMark} alt="" />
            </span>
            <span>PIE RELAY</span>
          </div>
          <div className="connect-intro-copy">
            <span className="connect-eyebrow">
              <span className="connect-live-dot" />
              Secure remote terminal
            </span>
            <h1 id="connect-title">
              어디서나 이어지는
              <br />
              나의 작업 공간
            </h1>
            <p>
              로컬 터미널과 원격 세션을 한 화면에서 연결하고, 필요한
              사람에게만 안전하게 공유하세요.
            </p>
          </div>
          <ul className="connect-benefits">
            <li>
              <span><UiIcon name="shield" size={16} /></span>
              <div>
                <strong>안전한 Relay 연결</strong>
                <small>초대 코드와 역할 기반 접근 제어</small>
              </div>
            </li>
            <li>
              <span><UiIcon name="users" size={16} /></span>
              <div>
                <strong>함께 보는 터미널</strong>
                <small>Viewer와 Controller 권한을 명확하게 분리</small>
              </div>
            </li>
            <li>
              <span><UiIcon name="terminal" size={16} /></span>
              <div>
                <strong>로컬부터 격리 세션까지</strong>
                <small>PC, 서버, Docker 실행 공간에 동일하게 접속</small>
              </div>
            </li>
          </ul>
        </section>

        <form className="card connect-card" onSubmit={submit}>
          <div className="connect-card-head">
            <span className="connect-card-icon">
              <UiIcon name="terminal" size={18} />
            </span>
            <div>
              <h1 className="brand">Pie Relay</h1>
              <p className="subtitle">
                연결할 작업 공간과 인증 방식을 선택하세요.
              </p>
            </div>
          </div>

          <fieldset className="connect-source" disabled={busy}>
          <legend>접속 방법</legend>
          <label className="radio">
            <input
              type="radio"
              name="connect-source"
              checked={source === "invite"}
              onChange={() => {
                setSource("invite");
                setError(null);
              }}
            />
            <span>초대 코드</span>
          </label>
          <label className="radio">
            <input
              type="radio"
              name="connect-source"
              checked={source === "control"}
              onChange={() => {
                setSource("control");
                setAsHost(false);
                setError(null);
              }}
            />
            <span>내 장치 · 공유받은 장치</span>
          </label>
          </fieldset>

          <label className="field">
          <span>릴레이 주소</span>
          <input
            type="text"
            value={wsUrl}
            onChange={(e) => setWsUrl(e.target.value)}
            placeholder={relayConfig.participantUrl}
            autoComplete="off"
            spellCheck={false}
            disabled={busy}
          />
          <span className="field-help">
            설정 변수: <code>PIE_RELAY_URL</code> (미설정 시 Azure)
          </span>
          </label>

          {source === "invite" && (
          <label className="toggle-field">
            <input
              type="checkbox"
              checked={asHost}
              onChange={(e) => setAsHost(e.target.checked)}
              disabled={busy}
            />
            <span className="toggle-text">
              다른 기기: 호스트 토큰 붙여넣기
              <span className="toggle-hint">
                다른 기기의 호스트 토큰(JWT)을 붙여넣어 그 방에 호스트로
                참가합니다.
              </span>
            </span>
          </label>
          )}

          {source === "control" ? (
          <div className="managed-connect">
            <label className="field">
              <span>Control Plane 주소</span>
              <input
                type="text"
                value={controlUrl}
                onChange={(event) => setControlUrl(event.target.value)}
                placeholder="https://control.cookai.dev"
                autoComplete="off"
                spellCheck={false}
                disabled={busy}
              />
            </label>
            <label className="field">
              <span>사용자 PAT</span>
              <input
                type="password"
                value={controlPAT}
                onChange={(event) => setControlPAT(event.target.value)}
                placeholder="외부 서비스에서 발급한 PAT"
                autoComplete="off"
                spellCheck={false}
                disabled={busy}
              />
              <span className="field-help">
                PAT는 메모리에만 보관하며 앱 설정에 저장하지 않습니다.
              </span>
            </label>
            <button
              className="secondary"
              type="button"
              onClick={loadManaged}
              disabled={busy}
            >
              {busy ? "불러오는 중…" : "세션 목록 새로고침"}
            </button>
            {managed && (
              <fieldset className="connect-source managed-action-picker" disabled={busy}>
                <legend>작업</legend>
                <label className="radio">
                  <input
                    type="radio"
                    name="managed-action"
                    checked={managedAction === "connect"}
                    onChange={() => setManagedAction("connect")}
                    disabled={connectableManagedSessions.length === 0}
                  />
                  <span>기존 세션 접속</span>
                </label>
                <label className="radio">
                  <input
                    type="radio"
                    name="managed-action"
                    checked={managedAction === "create"}
                    onChange={() => setManagedAction("create")}
                  />
                  <span>새 작업 세션</span>
                </label>
              </fieldset>
            )}
            {managed &&
              managedAction === "connect" &&
              connectableManagedSessions.length > 0 && (
              <>
                <label className="field">
                  <span>접속할 세션</span>
                  <select
                    className="select"
                    value={selectedSessionId}
                    onChange={(event) => {
                      setSelectedSessionId(event.target.value);
                      setRequestedAccess("view");
                    }}
                    disabled={busy}
                  >
                    {connectableManagedSessions.map((session) => {
                      const device = managed.devices.find(
                        (candidate) => candidate.id === session.deviceId,
                      );
                      const target = device
                        ? deviceTitle(device, localDevice.id)
                        : `${session.deviceId} · ${session.executionTarget === "docker" ? "Docker" : "Host OS"}`;
                      return (
                        <option key={session.id} value={session.id}>
                          {session.ownerUserId === managed.identity.userId
                            ? "내 장치"
                            : "공유"}
                          {" · "}{target}{" · "}{session.name || session.id}
                        </option>
                      );
                    })}
                  </select>
                </label>
                {!selectedOwned && (
                  <label className="field">
                    <span>접속 권한</span>
                    <select
                      className="select"
                      value={requestedAccess}
                      onChange={(event) =>
                        setRequestedAccess(
                          event.target.value as "view" | "control",
                        )
                      }
                      disabled={busy}
                    >
                      <option value="view">보기 전용</option>
                      {canRequestControl && (
                        <option value="control">제어 요청</option>
                      )}
                    </select>
                  </label>
                )}
                <p className="managed-session-note">
                  <strong>
                    {(() => {
                      const device = managed.devices.find(
                        (candidate) =>
                          candidate.id === selectedManagedSession?.deviceId,
                      );
                      return device
                        ? deviceTitle(device, localDevice.id)
                        : selectedManagedSession?.executionTarget === "docker"
                          ? "Docker"
                          : "Host OS";
                    })()}
                  </strong>
                  {" · "}
                  {selectedOwned
                    ? "내 세션에는 호스트 권한으로 접속합니다."
                    : `공유 권한: ${selectedGrant?.access === "control" ? "보기 및 제어" : "보기 전용"}`}
                </p>
                {managed.sessions.length > connectableManagedSessions.length && (
                  <p className="managed-session-note dim">
                    종료·오류·시작 중인 세션 {managed.sessions.length - connectableManagedSessions.length}개는 접속 목록에서 제외했습니다.
                  </p>
                )}
                {selectedOwned &&
                  selectedManagedSession?.accessMode === "shared" && (
                    <div className="managed-share">
                      <strong>이 세션 공유</strong>
                      <div className="managed-share-fields">
                        <input
                          type="text"
                          value={shareUserId}
                          onChange={(event) =>
                            setShareUserId(event.target.value)
                          }
                          placeholder="사용자 ID"
                          autoComplete="off"
                          spellCheck={false}
                          disabled={busy}
                        />
                        <select
                          className="select"
                          value={shareAccess}
                          onChange={(event) =>
                            setShareAccess(
                              event.target.value as "view" | "control",
                            )
                          }
                          disabled={busy}
                        >
                          <option value="view">보기</option>
                          <option value="control">보기 및 제어</option>
                        </select>
                        <button
                          type="button"
                          className="secondary"
                          onClick={shareSelectedSession}
                          disabled={busy || !shareUserId.trim()}
                        >
                          24시간 공유
                        </button>
                      </div>
                      {ownedSessionGrants.length > 0 && (
                        <ul className="managed-grant-list">
                          {ownedSessionGrants.map((grant) => (
                            <li key={grant.id}>
                              <span>
                                {grant.subjectUserId} ·{" "}
                                {grant.access === "control" ? "제어" : "보기"}
                              </span>
                              <button
                                type="button"
                                className="danger"
                                onClick={() => revokeSelectedGrant(grant.id)}
                                disabled={busy}
                              >
                                회수
                              </button>
                            </li>
                          ))}
                        </ul>
                      )}
                    </div>
                  )}
              </>
            )}
            {managed && managedAction === "create" && (
              <div className="managed-create-session">
                <label className="field">
                  <span>실행할 장치</span>
                  <select
                    className="select"
                    value={newTargetDeviceId}
                    onChange={(event) =>
                      setNewTargetDeviceId(event.target.value)
                    }
                    disabled={busy || creatableDevices.length === 0}
                  >
                    {creatableDevices.map((device) => (
                      <option key={device.id} value={device.id}>
                        {deviceTitle(device, localDevice.id)} · {device.clientConnected || device.kind === "docker" ? device.observedState : "오프라인"}
                      </option>
                    ))}
                  </select>
                </label>
                {creatableDevices.length === 0 && (
                  <p className="managed-session-note dim">
                    세션을 만들 수 있는 내 장치가 없습니다.
                  </p>
                )}
                <label className="field">
                  <span>세션 이름</span>
                  <input
                    type="text"
                    value={newSessionName}
                    onChange={(event) => setNewSessionName(event.target.value)}
                    placeholder="예: 프로젝트 터미널"
                    disabled={busy}
                  />
                </label>
                <label className="field">
                  <span>공유 정책</span>
                  <select
                    className="select"
                    value={newSessionAccess}
                    onChange={(event) =>
                      setNewSessionAccess(
                        event.target.value as "private" | "shared",
                      )
                    }
                    disabled={busy}
                  >
                    <option value="private">비공개 · 나만</option>
                    <option value="shared">공유 · 허용된 사용자</option>
                  </select>
                </label>
                <div className="managed-target-summary">
                  <strong>
                    {creatableDevices.find(
                      (device) => device.id === newTargetDeviceId,
                    )?.kind === "docker"
                      ? "Docker 컨테이너에서 실행"
                      : "선택한 PC의 Host OS에서 실행"}
                  </strong>
                  <span>터미널 데이터는 Pie Relay로 연결됩니다.</span>
                </div>
                <button
                  className="primary"
                  type="button"
                  onClick={createSelectedSession}
                  disabled={busy || !newTargetDeviceId}
                >
                  {createProgress || "세션 만들고 접속"}
                </button>
              </div>
            )}
          </div>
        ) : asHost ? (
          <>
            <label className="field">
              <span>호스트 토큰 (JWT)</span>
              <input
                type="text"
                value={hostToken}
                onChange={(e) => setHostToken(e.target.value)}
                placeholder="eyJ… (다른 기기에서 발급한 호스트 토큰)"
                autoComplete="off"
                spellCheck={false}
                disabled={busy}
              />
            </label>
            <p className="host-join-note">
              같은 PC에서 방금 띄운 방은 내 PC 데몬 화면의 '이 방 열기'를
              쓰세요. 이 경로는 다른 기기의 호스트 토큰을 직접 붙여넣을 때만
              씁니다. 토큰은 저장하지 않습니다.
            </p>
          </>
        ) : (
          <label className="field">
            <span>초대 코드</span>
            <input
              type="text"
              value={code}
              onChange={(e) => setCode(e.target.value.toUpperCase())}
              placeholder="예: ABCD2345"
              autoComplete="off"
              spellCheck={false}
              autoCapitalize="characters"
              disabled={busy}
            />
          </label>
          )}

          {!(source === "control" && managedAction === "create") && (
            <label className="field">
              <span>이름</span>
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="예: bob"
                autoComplete="off"
                spellCheck={false}
                disabled={busy}
              />
            </label>
          )}

          {error && (
          <p className="error" role="alert">
            {error}
          </p>
          )}

          <div className="connect-actions">
          {onCancel && (
            <button
              className="secondary"
              type="button"
              onClick={onCancel}
              disabled={busy}
            >
              취소
            </button>
          )}
          {!(source === "control" && managedAction === "create") && <button className="primary" type="submit" disabled={busy}>
            {busy
              ? "참가 중…"
              : source === "control"
                ? "선택한 세션에 접속"
                : asHost
                  ? "호스트로 참가"
                  : "참가"}
          </button>}
          </div>
        </form>
      </div>
    </div>
  );
}
