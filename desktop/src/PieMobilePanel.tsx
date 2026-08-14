import { useCallback, useEffect, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import QRCode from "qrcode";
import type { RelayConfig } from "./relay-config";

const LS_KEY = "pie-relay.mobile";
const LEGACY_LS_KEY = "cli-relay.orca-mobile";
const HOST_LS_KEYS = ["pie-relay.host", "cli-relay.host"];
const MAX_LOG_LINES = 100;

type MobileConnectionMode = "automatic" | "local-only" | "relay-only";

interface PersistedMobileSettings {
  relayEndpointKey: string;
  advertiseHost: string;
  defaultCwd: string;
  connectionMode: MobileConnectionMode;
  relayUrl: string;
}

interface MobileGatewayReady {
  endpoint: string;
  pairingUrl: string;
  deviceId: string;
  port: number;
  connectionMode: MobileConnectionMode;
  relayEndpoint?: string;
}

interface MobileGatewayStatus {
  running: boolean;
}

interface MobileGatewayLog {
  stream: string;
  line: string;
}

interface MobileDevice {
  deviceId: string;
  name: string;
  pairedAt: number;
  lastSeenAt: number;
}

interface MobileDevicesEvent {
  devices: MobileDevice[];
}

interface MobileRelayStatus {
  status: "disabled" | "connecting" | "registered" | "offline";
  detail?: string;
}

function isConnectionMode(value: unknown): value is MobileConnectionMode {
  return value === "automatic" || value === "local-only" || value === "relay-only";
}

function loadHostRelayConfig(
  relayConfig: RelayConfig,
): { relayUrl: string; relayToken: string } {
  for (const key of HOST_LS_KEYS) {
    try {
      const value = JSON.parse(localStorage.getItem(key) || "{}") as {
        relayUrl?: unknown;
        ticket?: unknown;
        relayEndpointKey?: unknown;
      };
      if (
        value.relayEndpointKey === relayConfig.storageKey &&
        (typeof value.relayUrl === "string" || typeof value.ticket === "string")
      ) {
        return {
          relayUrl: typeof value.relayUrl === "string" ? value.relayUrl : "",
          relayToken: typeof value.ticket === "string" ? value.ticket : "",
        };
      }
    } catch {
      /* try the next compatibility key */
    }
  }
  return { relayUrl: relayConfig.httpOrigin, relayToken: "" };
}

function loadSettings(relayConfig: RelayConfig): PersistedMobileSettings {
  const host = loadHostRelayConfig(relayConfig);
  const fallback: PersistedMobileSettings = {
    relayEndpointKey: relayConfig.storageKey,
    advertiseHost: "",
    defaultCwd: "",
    connectionMode: "automatic",
    relayUrl: host.relayUrl || relayConfig.httpOrigin,
  };
  try {
    const raw = localStorage.getItem(LS_KEY) || localStorage.getItem(LEGACY_LS_KEY) || "{}";
    const value = JSON.parse(raw) as Partial<PersistedMobileSettings>;
    const sameRelayEndpoint = value.relayEndpointKey === relayConfig.storageKey;
    return {
      relayEndpointKey: relayConfig.storageKey,
      advertiseHost: value.advertiseHost || "",
      defaultCwd: value.defaultCwd || "",
      connectionMode: sameRelayEndpoint && isConnectionMode(value.connectionMode)
        ? value.connectionMode
        : fallback.connectionMode,
      relayUrl: sameRelayEndpoint
        ? value.relayUrl || fallback.relayUrl
        : fallback.relayUrl,
    };
  } catch {
    return fallback;
  }
}

function isTauri(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

export function MobilePanel({
  relayConfig,
  hidden = false,
}: {
  relayConfig: RelayConfig;
  hidden?: boolean;
}) {
  const saved = loadSettings(relayConfig);
  const [advertiseHost, setAdvertiseHost] = useState(saved.advertiseHost);
  const [defaultCwd, setDefaultCwd] = useState(saved.defaultCwd);
  const [connectionMode, setConnectionMode] = useState<MobileConnectionMode>(saved.connectionMode);
  const [relayUrl, setRelayUrl] = useState(saved.relayUrl);
  const [relayToken, setRelayToken] = useState(
    () => loadHostRelayConfig(relayConfig).relayToken,
  );
  const [relayStatus, setRelayStatus] = useState<MobileRelayStatus>({ status: "disabled" });
  const [running, setRunning] = useState(false);
  const [queried, setQueried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [ready, setReady] = useState<MobileGatewayReady | null>(null);
  const [qrDataUrl, setQrDataUrl] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [logs, setLogs] = useState<string[]>([]);
  const [devices, setDevices] = useState<MobileDevice[]>([]);
  const [revokingDeviceId, setRevokingDeviceId] = useState<string | null>(null);
  const [logOpen, setLogOpen] = useState(false);
  const logRef = useRef<HTMLDivElement | null>(null);
  const tauri = isTauri();
  const relayConfigured =
    connectionMode === "local-only" || (relayUrl.trim() !== "" && relayToken.trim() !== "");

  useEffect(() => {
    localStorage.setItem(
      LS_KEY,
      JSON.stringify({
        relayEndpointKey: relayConfig.storageKey,
        advertiseHost,
        defaultCwd,
        connectionMode,
        relayUrl,
      }),
    );
  }, [advertiseHost, defaultCwd, connectionMode, relayUrl, relayConfig.storageKey]);

  useEffect(() => {
    if (hidden || running) return;
    const host = loadHostRelayConfig(relayConfig);
    if (host.relayToken) setRelayToken(host.relayToken);
    if (host.relayUrl) setRelayUrl(host.relayUrl);
  }, [hidden, running, relayConfig]);

  useEffect(() => {
    if (!ready?.pairingUrl) {
      setQrDataUrl("");
      return;
    }
    let current = true;
    void QRCode.toDataURL(ready.pairingUrl, {
      errorCorrectionLevel: "M",
      margin: 2,
      width: 280,
    })
      .then((url) => {
        if (current) setQrDataUrl(url);
      })
      .catch((cause: unknown) => {
        if (current) setError(cause instanceof Error ? cause.message : String(cause));
      });
    return () => {
      current = false;
    };
  }, [ready]);

  useEffect(() => {
    if (!tauri) {
      setQueried(true);
      return;
    }
    let disposed = false;
    const unlisteners: UnlistenFn[] = [];
    const setup = async () => {
      unlisteners.push(
        await listen<MobileGatewayStatus>("mobile-gateway-status", (event) => {
          if (disposed) return;
          setRunning(event.payload.running);
          setBusy(false);
          if (!event.payload.running) setReady(null);
        }),
      );
      unlisteners.push(
        await listen<MobileRelayStatus>("mobile-relay-status", (event) => {
          if (disposed) return;
          setRelayStatus(event.payload);
          if (event.payload.detail) {
            setLogs((previous) =>
              [...previous, `[Relay] ${event.payload.detail}`].slice(-MAX_LOG_LINES),
            );
          }
        }),
      );
      unlisteners.push(
        await listen<MobileGatewayReady>("mobile-gateway-ready", (event) => {
          if (disposed) return;
          setReady(event.payload);
          setRunning(true);
          setBusy(false);
          setError(null);
        }),
      );
      unlisteners.push(
        await listen<MobileGatewayLog>("mobile-gateway-log", (event) => {
          if (disposed) return;
          const prefix = event.payload.stream === "stderr" ? "오류" : "호스트";
          setLogs((previous) =>
            [...previous, `[${prefix}] ${event.payload.line}`].slice(-MAX_LOG_LINES),
          );
        }),
      );
      unlisteners.push(
        await listen<MobileDevicesEvent>("mobile-gateway-devices", (event) => {
          if (disposed) return;
          setDevices(event.payload.devices);
          setRevokingDeviceId((current) =>
            current && event.payload.devices.some((device) => device.deviceId === current)
              ? current
              : null,
          );
        }),
      );
      const active = await invoke<boolean>("mobile_gateway_running");
      if (disposed) return;
      setRunning(active);
      if (active) {
        const info = await invoke<MobileGatewayReady | null>("mobile_gateway_info");
        if (!disposed && info) setReady(info);
        await invoke("mobile_gateway_list_devices");
      }
      setQueried(true);
    };
    void setup().catch((cause: unknown) => {
      if (!disposed) {
        setError(cause instanceof Error ? cause.message : String(cause));
        setQueried(true);
      }
    });
    return () => {
      disposed = true;
      for (const unlisten of unlisteners) unlisten();
    };
  }, [tauri]);

  useEffect(() => {
    const element = logRef.current;
    if (element) element.scrollTop = element.scrollHeight;
  }, [logs]);

  const start = useCallback(
    async (rotatePairing: boolean) => {
      if (!tauri || busy || running) return;
      setBusy(true);
      setError(null);
      setCopied(false);
      setReady(null);
      try {
        await invoke("mobile_gateway_start", {
          advertiseHost: advertiseHost.trim() || null,
          defaultCwd: defaultCwd.trim() || null,
          rotatePairing,
          connectionMode,
          relayUrl: connectionMode === "local-only" ? null : relayUrl.trim() || null,
          relayToken: connectionMode === "local-only" ? null : relayToken.trim() || null,
        });
      } catch (cause: unknown) {
        setBusy(false);
        setError(cause instanceof Error ? cause.message : String(cause));
      }
    },
    [advertiseHost, busy, connectionMode, defaultCwd, relayToken, relayUrl, running, tauri],
  );

  const stop = useCallback(async () => {
    if (!tauri || busy || !running) return;
    setBusy(true);
    setError(null);
    try {
      await invoke("mobile_gateway_stop");
    } catch (cause: unknown) {
      setBusy(false);
      setError(cause instanceof Error ? cause.message : String(cause));
    }
  }, [busy, running, tauri]);

  const copyPairing = useCallback(async () => {
    if (!ready) return;
    try {
      await navigator.clipboard.writeText(ready.pairingUrl);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1_500);
    } catch (cause: unknown) {
      setError(cause instanceof Error ? cause.message : String(cause));
    }
  }, [ready]);

  const revokeDevice = useCallback(
    async (deviceId: string) => {
      if (!running || revokingDeviceId) return;
      setRevokingDeviceId(deviceId);
      setError(null);
      try {
        await invoke("mobile_gateway_revoke_device", { deviceId });
      } catch (cause: unknown) {
        setRevokingDeviceId(null);
        setError(cause instanceof Error ? cause.message : String(cause));
      }
    },
    [revokingDeviceId, running],
  );

  const stateLabel = !queried ? "확인 중" : running ? (ready ? "연결 대기 중" : "시작 중") : "정지됨";

  return (
    <section className={`screen mobile-panel${hidden ? " hidden" : ""}`}>
      <div className="mobile-scroll">
        <header className="mobile-title">
          <div>
            <h2>Pie Relay 모바일</h2>
            <p>같은 Wi-Fi 또는 Pie Relay 서버를 통해 이 PC의 터미널에 E2EE로 연결합니다.</p>
          </div>
          <span className={`mobile-status${running ? " on" : ""}`}>
            <span className="dot" />
            {stateLabel}
          </span>
        </header>

        {!tauri && (
          <p className="host-warn">모바일 호스트는 Tauri 데스크톱 앱에서만 실행할 수 있습니다.</p>
        )}

        <div className="mobile-layout">
          <article className="host-card mobile-setup-card">
            <div className="host-head">
              <h3>PC 모바일 호스트</h3>
            </div>
            <p className="host-hint">
              로컬 연결은 <code>6768</code> 포트를 우선 사용합니다. 자동 모드는 같은 Wi-Fi를
              우선하고 연결할 수 없으면 Pie Relay 서버로 전환합니다.
            </p>

            <label className="field">
              연결 방식
              <select
                value={connectionMode}
                onChange={(event) => setConnectionMode(event.target.value as MobileConnectionMode)}
                disabled={running || busy}
              >
                <option value="automatic">자동 — Wi-Fi 우선, Relay 대체</option>
                <option value="local-only">로컬 Wi-Fi만</option>
                <option value="relay-only">Pie Relay 서버만</option>
              </select>
            </label>

            {connectionMode !== "local-only" && (
              <div className="mobile-relay-settings">
                <label className="field">
                  Pie Relay 서버 URL
                  <input
                    value={relayUrl}
                    onChange={(event) => setRelayUrl(event.target.value)}
                    disabled={running || busy}
                    placeholder={relayConfig.httpOrigin}
                    spellCheck={false}
                  />
                </label>
                <label className="field">
                  호스트 토큰
                  <input
                    type="password"
                    value={relayToken}
                    onChange={(event) => setRelayToken(event.target.value)}
                    disabled={running || busy}
                    placeholder="호스트 화면에서 발급한 토큰"
                    autoComplete="off"
                  />
                  <span className="field-help">
                    호스트 화면의 Relay 설정을 자동으로 불러오며, 이 화면에서는 별도로 저장하지 않습니다.
                  </span>
                </label>
                <div className={`mobile-relay-state ${relayStatus.status}`}>
                  Relay 상태: {relayStatus.status === "registered" ? "연결됨" : relayStatus.status === "connecting" ? "연결 중" : relayStatus.status === "offline" ? "연결 끊김" : "대기"}
                </div>
              </div>
            )}

            <label className="field">
              모바일에 알릴 PC 주소 (선택)
              <input
                value={advertiseHost}
                onChange={(event) => setAdvertiseHost(event.target.value)}
                disabled={running || busy}
                placeholder="자동 선택 (예: 192.168.0.10)"
                spellCheck={false}
              />
              <span className="field-help">
                Tailscale과 Wi-Fi가 함께 있으면 원하는 주소를 직접 지정하세요.
              </span>
            </label>

            <label className="field">
              터미널 시작 폴더 (선택)
              <input
                value={defaultCwd}
                onChange={(event) => setDefaultCwd(event.target.value)}
                disabled={running || busy}
                placeholder="비워 두면 홈 폴더"
                spellCheck={false}
              />
            </label>

            <div className="mobile-actions">
              {running ? (
                <button className="danger" onClick={() => void stop()} disabled={busy}>
                  모바일 호스트 중지
                </button>
              ) : (
                <>
                  <button
                    className="primary"
                    onClick={() => void start(false)}
                    disabled={!tauri || busy || !queried || !relayConfigured}
                  >
                    {busy ? "시작 중…" : "모바일 호스트 시작"}
                  </button>
                  <button
                    className="secondary"
                    onClick={() => void start(true)}
                    disabled={!tauri || busy || !queried || !relayConfigured}
                    title="노출됐을 수 있는 미사용 페어링 토큰을 폐기합니다"
                  >
                    새 QR로 시작
                  </button>
                </>
              )}
            </div>
            {error && <p className="error">{error}</p>}
          </article>

          <article className="host-card mobile-qr-card">
            <div className="host-head">
              <h3>모바일 앱 연결</h3>
              {ready && <span className="auth-badge ok">E2EE</span>}
            </div>
            {ready && qrDataUrl ? (
              <>
                <div className="mobile-qr-wrap">
                  <img src={qrDataUrl} alt="Pie Relay 모바일 페어링 QR 코드" />
                </div>
                <div className="mobile-endpoint">
                  <span>접속 주소</span>
                  <code>{ready.endpoint}</code>
                </div>
                {ready.relayEndpoint && (
                  <div className="mobile-endpoint">
                    <span>Relay 주소</span>
                    <code>{ready.relayEndpoint}</code>
                  </div>
                )}
                <ol className="mobile-steps">
                  <li>Pie Relay 모바일 앱을 실행합니다.</li>
                  <li>앱의 QR 스캐너로 이 코드를 읽습니다.</li>
                  <li>연결 후 터미널을 선택해 PC를 제어합니다.</li>
                </ol>
                <button className="secondary" onClick={() => void copyPairing()}>
                  {copied ? "복사됨" : "페어링 링크 복사"}
                </button>
                <p className="mobile-security-note">
                  QR에는 기기별 일회성 페어링 권한이 들어 있습니다. 화면 공유나 외부 노출을 피하세요.
                </p>
              </>
            ) : (
              <div className="mobile-qr-empty">
                <div className="mobile-phone-glyph">▣</div>
                <p>{running ? "보안 페어링 QR을 만드는 중입니다…" : "호스트를 시작하면 QR이 표시됩니다."}</p>
              </div>
            )}
          </article>
        </div>

        <article className="host-card mobile-devices-card">
          <div className="host-head">
            <div>
              <h3>등록된 모바일 기기</h3>
              <p className="host-hint">기기별 권한을 폐기하면 열려 있는 연결도 즉시 종료됩니다.</p>
            </div>
            <span className="auth-badge unknown">{devices.length}대</span>
          </div>
          {devices.length === 0 ? (
            <p className="mobile-device-empty">아직 연결이 완료된 모바일 기기가 없습니다.</p>
          ) : (
            <ul className="mobile-device-list">
              {devices.map((device) => (
                <li key={device.deviceId}>
                  <div className="mobile-device-copy">
                    <strong>{device.name}</strong>
                    <span>
                      마지막 연결 {new Intl.DateTimeFormat("ko-KR", {
                        dateStyle: "medium",
                        timeStyle: "short",
                      }).format(device.lastSeenAt)}
                    </span>
                  </div>
                  <button
                    className="danger"
                    onClick={() => void revokeDevice(device.deviceId)}
                    disabled={!running || revokingDeviceId !== null}
                  >
                    {revokingDeviceId === device.deviceId ? "폐기 중…" : "권한 폐기"}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </article>

        <article className="host-card mobile-log-card">
          <button className="host-collapse-toggle" onClick={() => setLogOpen((value) => !value)}>
            <span className="host-collapse-caret">{logOpen ? "▾" : "▸"}</span>
            <h3>모바일 호스트 로그</h3>
          </button>
          {logOpen && (
            <div className="log-view" ref={logRef}>
              {logs.length === 0 ? (
                <div className="log-empty">아직 로그가 없습니다.</div>
              ) : (
                logs.map((line, index) => (
                  <div className={`log-line${line.startsWith("[오류]") ? " err" : ""}`} key={index}>
                    {line}
                  </div>
                ))
              )}
            </div>
          )}
        </article>
      </div>
    </section>
  );
}
