import { useCallback, useEffect, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { parseInviteCode, ringPush } from "./sidecar";
import {
  createInvite,
  enrollHost,
  isLoopbackRelay,
  wsOriginFromRelay,
  type InviteAccess,
} from "./relay";
import {
  deviceIdFromToken,
  relaySessionIdFromToken,
  roomFromToken,
  type RoomMode,
} from "./protocol";
import { createManagedHostSession } from "./control-plane";
import { localDeviceIdentity } from "./device-identity";
import type { RelayConfig } from "./relay-config";

// HostPanel drives the bundled Go client (clientd sidecar) from the GUI:
// "방 만들기" (mint a host token on the relay), the relay daemon (start/stop +
// streamed log), and invite-code creation. Process work happens in Rust
// commands; token minting happens over HTTP straight from the webview. This
// component invokes them and renders their `host-*` events. See
// src-tauri/src/lib.rs.

const LS_KEY = "pie-relay.host";
const LEGACY_LS_KEY = "cli-relay.host";
// Separate key for progressive-disclosure state (advanced/log expanded). Kept
// apart from the credential/config blob so remembering "고급 설정 펼침" never
// touches the persisted ticket/relay fields.
const LS_UI_KEY = "pie-relay.host.ui";
const LEGACY_LS_UI_KEY = "cli-relay.host.ui";
const MAX_LOG_LINES = 200;

interface UiState {
  advancedOpen: boolean;
  logOpen: boolean;
}

function loadUiState(): UiState {
  const fallback: UiState = { advancedOpen: false, logOpen: false };
  try {
    const raw =
      localStorage.getItem(LS_UI_KEY) || localStorage.getItem(LEGACY_LS_UI_KEY);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as Partial<UiState>;
    return { advancedOpen: !!v.advancedOpen, logOpen: !!v.logOpen };
  } catch {
    return fallback;
  }
}

// isTauri reports whether we're running inside the Tauri webview (vs. a plain
// browser via `npm run dev`). Host mode needs the Rust sidecar commands, so we
// show a notice instead of failing invokes when it's absent.
function isTauri(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

interface LogLine {
  source: string;
  stream: string;
  line: string;
}
interface ProcExit {
  source: string;
  code: number | null;
  signal: number | null;
}
interface DaemonStatus {
  running: boolean;
}

// PermissionMode is the room-wide policy the daemon hands to the executor via
// CLI_RELAY_PERMISSION_MODE (P3-2). "default" asks the host to approve each tool;
// "acceptEdits" auto-allows edits; "bypassPermissions" allows everything.
type PermissionMode = "default" | "acceptEdits" | "bypassPermissions";

const PERMISSION_MODES: PermissionMode[] = [
  "default",
  "acceptEdits",
  "bypassPermissions",
];

function isPermissionMode(v: unknown): v is PermissionMode {
  return typeof v === "string" && (PERMISSION_MODES as string[]).includes(v);
}

// isRoomMode narrows a persisted/arbitrary value to a room type the daemon
// understands. "chat" is the legacy SDK mode; "terminal" starts the PTY host.
function isRoomMode(v: unknown): v is RoomMode {
  return v === "chat" || v === "terminal";
}

interface Persisted {
  relayEndpointKey: string;
  relayUrl: string;
  executorPath: string;
  ticket: string;
  permissionMode: PermissionMode;
  defaultCwd: string;
  roomMode: RoomMode;
  // Display aliases (방 이름 / 내 이름). Persisted so a restart restores them and
  // the auto-join can re-apply them. Not a credential (unlike the enroll secret).
  roomName: string;
  hostName: string;
  // Start the host daemon automatically on app launch (opt-in, DEFAULT ON). A
  // participant-only user turns it off so their machine isn't hosted on launch.
  autoStart: boolean;
  managedMode: boolean;
  controlUrl: string;
  deviceId: string;
  deviceName: string;
  ownerUserId: string;
  executionTarget: "local" | "docker";
  accessMode: "private" | "shared";
  transportMode: "auto" | "lan" | "relay";
}

function newScopedID(prefix: string): string {
  const id =
    globalThis.crypto?.randomUUID?.() ??
    `${Date.now()}-${Math.random().toString(16).slice(2)}`;
  return `${prefix}-${id}`;
}

function loadSaved(relayConfig: RelayConfig): Persisted {
  const localDevice = localDeviceIdentity();
  const fallback: Persisted = {
    relayEndpointKey: relayConfig.storageKey,
    relayUrl: relayConfig.agentUrl,
    executorPath: "",
    ticket: "",
    permissionMode: "default",
    defaultCwd: "",
    roomMode: "chat",
    roomName: "",
    hostName: "",
    autoStart: true,
    managedMode: false,
    controlUrl: "http://127.0.0.1:19090",
    deviceId: localDevice.id,
    deviceName: localDevice.name,
    ownerUserId: "",
    executionTarget: "local",
    accessMode: "private",
    // Desktop/clientd terminal rooms currently use the authenticated Relay
    // protocol.  LAN transport belongs to the separately paired Mobile
    // Gateway.  Persisting "auto" here used to advertise a fallback that the
    // host daemon could not execute.
    transportMode: "relay",
  };
  try {
    const raw =
      localStorage.getItem(LS_KEY) || localStorage.getItem(LEGACY_LS_KEY);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as Partial<Persisted>;
    const sameRelayEndpoint = v.relayEndpointKey === relayConfig.storageKey;
    return {
      relayEndpointKey: relayConfig.storageKey,
      relayUrl: sameRelayEndpoint
        ? v.relayUrl || relayConfig.agentUrl
        : relayConfig.agentUrl,
      executorPath: v.executorPath || "",
      // A host token belongs to one Relay. Never carry it across an environment
      // profile or endpoint change.
      ticket: sameRelayEndpoint ? v.ticket || "" : "",
      permissionMode: isPermissionMode(v.permissionMode)
        ? v.permissionMode
        : "default",
      defaultCwd: v.defaultCwd || "",
      roomMode: isRoomMode(v.roomMode) ? v.roomMode : "chat",
      roomName: v.roomName || "",
      hostName: v.hostName || "",
      // Absent (existing installs / fresh) → default ON.
      autoStart: v.autoStart !== false,
      managedMode: v.managedMode === true,
      controlUrl: v.controlUrl || "http://127.0.0.1:18080",
      deviceId: v.deviceId || fallback.deviceId,
      deviceName: v.deviceName || "Pie Relay Desktop",
      ownerUserId: v.ownerUserId || "",
      executionTarget: v.executionTarget === "docker" ? "docker" : "local",
      accessMode: v.accessMode === "shared" ? "shared" : "private",
      transportMode: "relay",
    };
  } catch {
    return fallback;
  }
}

// autoStartAttempted is a MODULE-LEVEL once-guard: it survives React StrictMode's
// dev double-mount (and any remount) so the daemon is auto-started at most once
// per app session — a component-scoped ref would reset and could double-spawn.
let autoStartAttempted = false;

// OpenHostRoom is what "이 방 열기" hands up to App: the join URL (derived from
// the daemon's agent relay-url), the host token, and the room it hosts (decoded
// from the token). App turns this into a participant room joined as host.
export interface OpenHostRoom {
  wsUrl: string;
  token: string;
  room: string;
  // Optional local display aliases carried into the joined room (방 이름 / 내 이름).
  // Cosmetic only — the room id and token are decided by the token itself.
  label?: string;
  name?: string;
  executionTarget?: "local" | "docker";
  deviceId?: string;
  relaySessionId?: string;
}

interface HostPanelProps {
  relayConfig: RelayConfig;
  // P6: open the daemon's own room as its host — App adds it as a participant
  // room joined with asHost. Omitted in contexts (e.g. non-Tauri previews) that
  // don't wire the multi-room shell.
  onOpenHostRoom?: (args: OpenHostRoom) => void;
  // P10: live-edit the already-open host room's display alias (방 이름 / 내 이름)
  // as the operator types. Rename only — no token re-mint, no re-join.
  onRenameHostRoom?: (args: { label?: string; name?: string }) => void;
  // The panel stays mounted even off the host tab (so auto-start can run on
  // launch regardless of the active view); `hidden` just display:none's it.
  hidden?: boolean;
  // The most recently selected terminal remains connected while this settings
  // page is open. Showing it separately prevents the Host OS sidecar status
  // below from being mistaken for the active Docker/remote execution target.
  activeSession?: {
    label: string;
    executionTarget?: "local" | "docker";
    deviceId?: string;
    connected: boolean;
    hostConnected: boolean;
    relayLocal: boolean;
  };
}

export function HostPanel({
  relayConfig,
  onOpenHostRoom,
  onRenameHostRoom,
  activeSession,
  hidden = false,
}: HostPanelProps) {
  const saved = loadSaved(relayConfig);
  const [relayUrl, setRelayUrl] = useState(saved.relayUrl);
  const [autoStart, setAutoStart] = useState(saved.autoStart);
  const [executorPath, setExecutorPath] = useState(saved.executorPath);
  // Manual ticket (RELAY_TICKET). Non-empty switches the panel into manual mode:
  // the daemon runs with the ticket and invites are POSTed straight from the
  // webview instead of through `client room create`. Persisted alongside the
  // other fields: dev-mode reloads (vite HMR, tauri rebuild) remount this
  // component, and a session-only ticket silently reverted the panel to
  // credentials mode — the daemon then died with a misleading 401.
  const [ticket, setTicket] = useState(saved.ticket);
  const [permissionMode, setPermissionMode] = useState<PermissionMode>(
    saved.permissionMode,
  );
  const [defaultCwd, setDefaultCwd] = useState(saved.defaultCwd);
  const [roomMode, setRoomMode] = useState<RoomMode>(saved.roomMode);
  const [managedMode, setManagedMode] = useState(saved.managedMode);
  const [controlUrl, setControlUrl] = useState(saved.controlUrl);
  const [controlPAT, setControlPAT] = useState("");
  const [deviceId, setDeviceId] = useState(saved.deviceId);
  const [deviceName, setDeviceName] = useState(saved.deviceName);
  const [ownerUserId, setOwnerUserId] = useState(saved.ownerUserId);
  // A desktop-hosted session always executes on this local device. Docker
  // sessions are provisioned by Manager/Admin and opened from 새 연결. Never
  // restore the former hidden value: it could mislabel a local sidecar as Docker.
  const executionTarget = "local" as const;
  const [accessMode, setAccessMode] = useState<"private" | "shared">(
    saved.accessMode,
  );
  const transportMode = "relay" as const;

  // "방 만들기" inputs. The enroll secret is a credential, so it lives in state
  // only — never persisted to localStorage. roomName/hostName double as the
  // room's display aliases (방 이름 / 내 이름): before a token they seed the enroll
  // request; after one they edit the open room's alias. Persisted so a restart
  // keeps them (blank → relay generates a room id / host defaults to "나 (호스트)").
  const [secret, setSecret] = useState("");
  const [roomName, setRoomName] = useState(saved.roomName);
  const [hostName, setHostName] = useState(saved.hostName);
  const [enrollInfo, setEnrollInfo] = useState<{
    room: string;
    expiresAt: number;
    deviceId?: string;
    sessionId?: string;
  } | null>(null);
  const [enrollError, setEnrollError] = useState<string | null>(null);

  const [daemonRunning, setDaemonRunning] = useState(false);
  // Set once the real daemon state has been queried on mount, so launch
  // auto-start waits for the truth instead of racing the initial `false`.
  const [daemonQueried, setDaemonQueried] = useState(false);
  const [logs, setLogs] = useState<string[]>([]);
  const [inviteCode, setInviteCode] = useState<string | null>(null);
  // Grade to stamp on the next invite. Default "view" (watch-only) is the safe
  // pick — the host opts a guest up to "control" deliberately. The grade the
  // issued code actually carries is tracked separately for the badge.
  const [inviteAccess, setInviteAccess] = useState<InviteAccess>("view");
  const [inviteCodeAccess, setInviteCodeAccess] = useState<InviteAccess | null>(
    null,
  );
  const [copied, setCopied] = useState(false);
  const [tokenCopied, setTokenCopied] = useState(false);
  const [busyEnroll, setBusyEnroll] = useState(false);
  const [busyInvite, setBusyInvite] = useState(false);
  const [busyDaemon, setBusyDaemon] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  // Progressive disclosure: 고급 설정 and 사이드카 로그 collapse by default so the
  // main flow (방 만들기 → 방 타입 → 데몬 → 이 방 참여 → 초대) reads top-to-bottom.
  const savedUi = loadUiState();
  const [advancedOpen, setAdvancedOpen] = useState(savedUi.advancedOpen);
  const [logOpen, setLogOpen] = useState(savedUi.logOpen);

  const logRef = useRef<HTMLDivElement | null>(null);
  const tauri = isTauri();
  const manualMode = ticket.trim() !== "";
  // A loopback relay enrolls keyless; used to soften the enroll-key copy.
  const localRelay = isLoopbackRelay(relayUrl);

  // Room status is derived from the existing enroll/ticket state, not new state:
  // a token exists when a manual ticket is set or "방 만들기" returned enrollInfo.
  // The room label prefers the enroll response, else decodes the manual ticket's
  // `room` claim; expiry is only known from a fresh enroll.
  const hasToken = manualMode || enrollInfo !== null;
  const roomLabel =
    enrollInfo?.room || (manualMode ? roomFromToken(ticket.trim()) : "");

  // Persist the collapse state so a reload keeps 고급/로그 where the host left them.
  useEffect(() => {
    try {
      localStorage.setItem(
        LS_UI_KEY,
        JSON.stringify({ advancedOpen, logOpen } satisfies UiState),
      );
    } catch {
      /* private mode — non-fatal */
    }
  }, [advancedOpen, logOpen]);

  // Persist the fields whenever they change (ticket included — see above).
  useEffect(() => {
    try {
      localStorage.setItem(
        LS_KEY,
        JSON.stringify({
          relayEndpointKey: relayConfig.storageKey,
          relayUrl,
          executorPath,
          ticket,
          permissionMode,
          defaultCwd,
          roomMode,
          roomName,
          hostName,
          autoStart,
          managedMode,
          controlUrl,
          deviceId,
          deviceName,
          ownerUserId,
          executionTarget,
          accessMode,
          transportMode,
        } satisfies Persisted),
      );
    } catch {
      /* private mode — non-fatal */
    }
  }, [
    relayUrl,
    relayConfig.storageKey,
    executorPath,
    ticket,
    permissionMode,
    defaultCwd,
    roomMode,
    roomName,
    hostName,
    autoStart,
    managedMode,
    controlUrl,
    deviceId,
    deviceName,
    ownerUserId,
    executionTarget,
    accessMode,
    transportMode,
  ]);

  const appendLog = useCallback((line: string) => {
    setLogs((prev) => ringPush(prev, line, MAX_LOG_LINES));
  }, []);

  // Wire the Rust event stream once. host-log lines feed the ring buffer;
  // host-daemon-status keeps the indicator honest even if the daemon exits on
  // its own; host-proc-exit annotates the log with exit codes.
  useEffect(() => {
    if (!tauri) return;
    const unlisteners: UnlistenFn[] = [];
    let cancelled = false;
    (async () => {
      const add = (u: UnlistenFn) => {
        if (cancelled) u();
        else unlisteners.push(u);
      };
      add(
        await listen<LogLine>("host-log", (e) => {
          const { source, stream, line } = e.payload;
          appendLog(`[${source}${stream === "stderr" ? "!" : ""}] ${line}`);
        }),
      );
      add(
        await listen<DaemonStatus>("host-daemon-status", (e) => {
          setDaemonRunning(e.payload.running);
        }),
      );
      add(
        await listen<ProcExit>("host-proc-exit", (e) => {
          const { source, code, signal } = e.payload;
          const how =
            signal != null ? `signal ${signal}` : `code ${code ?? "?"}`;
          appendLog(`— ${source} 종료 (${how}) —`);
        }),
      );
    })();
    return () => {
      cancelled = true;
      for (const u of unlisteners) u();
    };
  }, [tauri, appendLog]);

  // On mount, ask Rust for a suggested executor path (bundled resource → dev
  // checkout). Credential presence is no longer probed: the host authenticates
  // by minting a token via "방 만들기", tracked by the manual ticket.
  useEffect(() => {
    if (!tauri) return;
    if (executorPath.trim() === "") {
      invoke<string>("host_suggest_executor_path")
        .then((p) => {
          if (p) setExecutorPath(p);
        })
        .catch(() => {});
    }
    // Re-sync the real daemon state on mount. This panel unmounts when the user
    // switches to a room and back, so daemonRunning (event-driven) would reset to
    // false even while the daemon runs — ask Rust for the truth.
    invoke<boolean>("host_daemon_running")
      .then((running) => setDaemonRunning(running))
      .catch(() => {})
      .finally(() => setDaemonQueried(true)); // unblock launch auto-start
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tauri]);

  // Auto-scroll the log to the newest line.
  useEffect(() => {
    const el = logRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [logs]);

  // onCreateRoom exchanges the operator's enroll secret for a host token on the
  // relay (POST /host/enroll) and saves that token as the manual ticket. From
  // there the daemon, "이 방 열기", invite creation, and token copy all run off
  // this ticket via the existing wiring. The secret is dropped from the input
  // after a success so it isn't left lying in the field.
  const onCreateRoom = useCallback(async () => {
    setNotice(null);
    setEnrollError(null);
    // Secret is optional: a local (loopback) relay enrolls keyless, so an empty
    // key is a valid submission. enrollHost omits a blank secret from the body;
    // a public relay that needs a key answers 401/403 with a clear message.
    const s = secret.trim();
    setBusyEnroll(true);
    try {
      if (managedMode) {
        const managed = await createManagedHostSession(controlUrl, controlPAT, {
          deviceId,
          deviceName,
          sessionName: roomName.trim() || undefined,
          executionTarget,
          accessMode,
          transportMode,
          ownerUserId: ownerUserId.trim() || undefined,
          applicationId:
            import.meta.env.VITE_PIE_RELAY_APPLICATION_ID?.trim() || undefined,
          poolId: import.meta.env.VITE_PIE_RELAY_POOL_ID?.trim() || undefined,
          tenantId:
            import.meta.env.VITE_PIE_RELAY_TENANT_ID?.trim() || undefined,
          resourceType: "device",
          resourceId: deviceId,
          protocol: "terminal",
        });
        if (managed.relayUrl) setRelayUrl(managed.relayUrl);
        setTicket(managed.token);
        setEnrollInfo({
          room: managed.room,
          expiresAt: managed.expiresAt,
          deviceId: managed.deviceId,
          sessionId: managed.sessionId,
        });
        appendLog(
          `— 관리형 세션 생성 완료: ${managed.sessionId} · ${managed.deviceId} —`,
        );
        return;
      }
      const sessionId = newScopedID("session");
      const res = await enrollHost(relayUrl, {
        secret: s || undefined,
        room: roomName.trim() || undefined,
        name: hostName.trim() || undefined,
        deviceId,
        sessionId,
      });
      setTicket(res.token);
      setEnrollInfo({
        room: res.room,
        expiresAt: res.expiresAt,
        deviceId: res.deviceId ?? deviceId,
        sessionId: res.sessionId ?? sessionId,
      });
      setSecret("");
      appendLog(`— 방 만들기 완료: ${res.room} —`);
    } catch (err) {
      setEnrollError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyEnroll(false);
    }
  }, [
    secret,
    relayUrl,
    roomName,
    hostName,
    appendLog,
    managedMode,
    controlUrl,
    controlPAT,
    deviceId,
    deviceName,
    ownerUserId,
    executionTarget,
    accessMode,
    transportMode,
  ]);

  // resolveHostToken returns the token to join/copy: the manual ticket wins
  // (that's the token 방 만들기 stores and the daemon runs with); otherwise the
  // saved accessToken. Shared by auto-join on daemon start and "토큰 복사".
  const resolveHostToken = useCallback(async (): Promise<string> => {
    const t = ticket.trim();
    if (t) return t;
    if (!tauri) {
      throw new Error("먼저 방 만들기로 호스트 토큰을 발급하세요.");
    }
    const tok = (await invoke<string>("host_access_token")).trim();
    if (!tok) {
      throw new Error(
        "호스트 토큰을 찾지 못했습니다 — 먼저 방 만들기를 하세요.",
      );
    }
    return tok;
  }, [ticket, tauri]);

  // startDaemon starts the host daemon and background-joins its own room. Shared
  // by the "데몬 시작" button and the launch auto-start, so both take the exact
  // same path. Never switches the view (onOpenHostRoom joins in the background).
  const startDaemon = useCallback(async () => {
    setNotice(null);
    setBusyDaemon(true);
    try {
      await invoke("host_daemon_start", {
        relayUrl: relayUrl.trim() || null,
        ticket: ticket.trim() || null,
        executorPath: executorPath.trim() || null,
        permissionMode,
        defaultCwd: defaultCwd.trim() || null,
        // "terminal" makes the daemon supervise the PTY host (zsh) instead of
        // the SDK chat executor; "chat" leaves the env unset (legacy default).
        roomMode,
      });
      // Auto-join the daemon's own room in the background (asHost) so the host
      // can approve/drive/watch — the room lands in the sidebar. No view switch:
      // the operator stays put to issue invite codes etc.
      try {
        const token = await resolveHostToken();
        onOpenHostRoom?.({
          wsUrl: wsOriginFromRelay(relayUrl),
          token,
          room: roomFromToken(token),
          label: roomName.trim() || undefined,
          name: hostName.trim() || undefined,
          executionTarget: "local",
          deviceId:
            enrollInfo?.deviceId ?? deviceIdFromToken(token) ?? deviceId,
          relaySessionId:
            enrollInfo?.sessionId ?? relaySessionIdFromToken(token),
        });
      } catch {
        /* no token to auto-join with — the daemon still runs; user can join later */
      }
    } catch (err) {
      setNotice(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyDaemon(false);
    }
  }, [
    relayUrl,
    ticket,
    executorPath,
    permissionMode,
    defaultCwd,
    roomMode,
    roomName,
    hostName,
    enrollInfo,
    deviceId,
    resolveHostToken,
    onOpenHostRoom,
  ]);

  const onToggleDaemon = useCallback(async () => {
    if (daemonRunning) {
      setNotice(null);
      setBusyDaemon(true);
      try {
        await invoke("host_daemon_stop");
      } catch (err) {
        setNotice(err instanceof Error ? err.message : String(err));
      } finally {
        setBusyDaemon(false);
      }
    } else {
      await startDaemon();
    }
  }, [daemonRunning, startDaemon]);

  // Auto-start on launch (opt-in, default ON). The daemon-running query effect
  // below resolves the real state first; here we react to that. Guarded by the
  // module-level `autoStartAttempted` so StrictMode's double-mount (and any
  // remount) can't double-spawn, and skipped if a daemon is already running.
  // Runs in the background regardless of the active tab (the panel stays mounted).
  const startDaemonRef = useRef(startDaemon);
  useEffect(() => {
    startDaemonRef.current = startDaemon;
  }, [startDaemon]);
  useEffect(() => {
    if (!tauri) return;
    if (autoStartAttempted) return;
    if (!daemonQueried) return; // wait for the real daemon state (avoid double-spawn)
    if (!autoStart) return;
    if (daemonRunning) return; // already up → nothing to do
    autoStartAttempted = true;
    void startDaemonRef.current();
    // daemonQueried gates on the mount query settling; the module flag ensures
    // this still fires at most once across StrictMode remounts.
  }, [tauri, autoStart, daemonQueried, daemonRunning]);

  // resolveHostToken returns the token this daemon runs with: the manual ticket
  // wins (that's the token in manual mode); otherwise the saved login's access
  // token (Rust reads it from credentials.json). Shared by "open this room" and
  // "copy token for another device". Throws with an actionable message if none.
  // (The daemon's own room is now auto-joined on start — see onToggleDaemon —
  // so there is no separate "이 방 참여" action.)

  // Copy this daemon's host token so another device can paste it into the
  // connect screen's "다른 기기: 호스트 토큰 붙여넣기" field. This is the counterpart
  // to that paste path (P6): without it, the token — minted on the relay or saved
  // in credentials.json — has no in-GUI way to be read.
  const onCopyToken = useCallback(async () => {
    setNotice(null);
    try {
      const token = await resolveHostToken();
      await navigator.clipboard.writeText(token);
      setTokenCopied(true);
      setTimeout(() => setTokenCopied(false), 1500);
    } catch (err) {
      setNotice(err instanceof Error ? err.message : String(err));
    }
  }, [resolveHostToken]);

  const onCreateInvite = useCallback(async () => {
    setNotice(null);
    setCopied(false);
    setBusyInvite(true);
    try {
      if (manualMode) {
        // Manual-ticket path: the webview calls the relay directly, because
        // `client room create` only reads credentials.json. This is the only
        // path that can stamp a grade, so the selected access rides here.
        const res = await createInvite(relayUrl, ticket.trim(), inviteAccess);
        setInviteCode(res.code);
        setInviteCodeAccess(res.access);
      } else {
        // Sidecar path can't carry a grade, so the relay applies its "control"
        // default. The badge reflects that; grade selection needs a manual ticket.
        const out = await invoke<string>("host_invite_create", {
          relayUrl: relayUrl.trim() || null,
        });
        const code = parseInviteCode(out);
        if (!code)
          throw new Error(
            "출력에서 초대 코드를 찾지 못했습니다. 로그를 확인하세요.",
          );
        setInviteCode(code);
        setInviteCodeAccess("control");
      }
    } catch (err) {
      setNotice(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyInvite(false);
    }
  }, [manualMode, relayUrl, ticket, inviteAccess]);

  const onCopy = useCallback(async () => {
    if (!inviteCode) return;
    try {
      await navigator.clipboard.writeText(inviteCode);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      setNotice("클립보드 복사에 실패했습니다.");
    }
  }, [inviteCode]);

  return (
    <div className={"screen host" + (hidden ? " hidden" : "")}>
      <div className="host-scroll">
        {activeSession && (
          <section className="host-card active-session-summary">
            <div className="host-head">
              <div>
                <span className="active-session-eyebrow">현재 작업 세션</span>
                <h2>{activeSession.label}</h2>
              </div>
              <span
                className={
                  "active-session-target " +
                  (activeSession.executionTarget ?? "unknown")
                }
              >
                {activeSession.executionTarget === "docker"
                  ? "DOCKER"
                  : activeSession.executionTarget === "local"
                    ? "HOST OS"
                    : "REMOTE"}
              </span>
            </div>
            <div className="active-session-facts">
              <span>
                <small>실행 환경</small>
                <strong>
                  {activeSession.executionTarget === "docker"
                    ? "Docker 컨테이너"
                    : activeSession.executionTarget === "local"
                      ? activeSession.deviceId === saved.deviceId
                        ? "이 PC · Host OS"
                        : "원격 PC · Host OS"
                      : "원격 실행 환경"}
                </strong>
              </span>
              <span>
                <small>접속 경로</small>
                <strong>
                  Relay{activeSession.relayLocal ? " · 이 PC" : " · 원격"}
                </strong>
              </span>
              <span>
                <small>실제 Host</small>
                <strong>
                  {activeSession.executionTarget === "docker"
                    ? "컨테이너 내부 clientd"
                    : "Host OS clientd"}
                </strong>
              </span>
              <span>
                <small>연결 상태</small>
                <strong
                  className={
                    activeSession.connected && activeSession.hostConnected
                      ? "connected"
                      : "disconnected"
                  }
                >
                  {activeSession.connected && activeSession.hostConnected
                    ? "연결됨"
                    : "연결 끊김"}
                </strong>
              </span>
            </div>
          </section>
        )}

        {/* 1) 방 — 토큰 발급/상태 → 방 타입 → 데몬 → 이 방 참여 */}
        <section className="host-card">
          <div className="host-head">
            <div className="host-title-stack">
              <h2>이 PC 직접 호스트</h2>
              <span>단독 Relay에 이 PC의 Host OS 터미널을 연결합니다</span>
            </div>
            <div className="host-head-badges">
              <span className="host-scope-badge">HOST OS</span>
              <AuthBadge manualMode={manualMode} />
            </div>
          </div>

          {!tauri && (
            <p className="host-warn">
              이 기능은 Tauri 데스크톱 앱에서만 사용할 수 있습니다.
            </p>
          )}

          <p className="managed-session-note">
            다른 PC 또는 Docker를 선택하려면 <strong>새 연결 → 내 장치 · 공유받은 장치 → 새 작업 세션</strong>을 사용하세요.
          </p>

          <fieldset className="field invite-access session-issuer">
            <legend>인증·세션 관리</legend>
            <label className="radio">
              <input
                type="radio"
                name="session-issuer"
                checked={!managedMode}
                onChange={() => setManagedMode(false)}
                disabled={daemonRunning}
              />
              <span>단독 Relay · 로컬 개발</span>
            </label>
            <label className="radio">
              <input
                type="radio"
                name="session-issuer"
                checked={managedMode}
                onChange={() => setManagedMode(true)}
                disabled={daemonRunning}
              />
              <span>Pie Control Plane · 운영</span>
            </label>
          </fieldset>

          {managedMode && !hasToken && (
            <>
              <label className="field">
                <span>사용자 PAT</span>
                <input
                  type="password"
                  value={controlPAT}
                  onChange={(e) => setControlPAT(e.target.value)}
                  placeholder="외부 서비스에서 발급한 PAT"
                  autoComplete="off"
                  spellCheck={false}
                />
                <span className="field-help">
                  PAT는 앱에 저장하지 않습니다.
                </span>
              </label>
              <label className="field">
                <span>공유 정책</span>
                <select
                  className="select"
                  value={accessMode}
                  onChange={(e) =>
                    setAccessMode(e.target.value as "private" | "shared")
                  }
                >
                  <option value="private">비공개 · 나만</option>
                  <option value="shared">공유 · 허용된 사용자</option>
                </select>
              </label>
            </>
          )}

          {hasToken ? (
            <p className="host-notice room-status" role="status">
              방 <strong>{roomName.trim() || roomLabel || "—"}</strong>
              {enrollInfo && <> · 만료 {formatExpiry(enrollInfo.expiresAt)}</>}
              {enrollInfo?.sessionId && <> · 세션 {enrollInfo.sessionId}</>}
            </p>
          ) : null}

          {/* 방 이름/내 이름은 토큰 유무와 무관하게 항상 편집 가능. 토큰 발급 전에는
              발급 힌트, 토큰이 있으면 열린 방의 표시 별칭을 실시간으로 바꾼다
              (onRenameHostRoom — 토큰·릴레이 방 ID는 그대로). */}
          <div className="field-grid">
            <label className="field">
              <span>방 이름</span>
              <input
                type="text"
                value={roomName}
                onChange={(e) => {
                  const v = e.target.value;
                  setRoomName(v);
                  if (hasToken)
                    onRenameHostRoom?.({
                      label: v,
                      name: hostName.trim() || "나 (호스트)",
                    });
                }}
                placeholder="자동 생성"
                spellCheck={false}
                autoCapitalize="off"
              />
            </label>

            <label className="field">
              <span>표시 이름</span>
              <input
                type="text"
                value={hostName}
                onChange={(e) => {
                  const v = e.target.value;
                  setHostName(v);
                  if (hasToken)
                    onRenameHostRoom?.({
                      label: roomName,
                      name: v.trim() || "나 (호스트)",
                    });
                }}
                placeholder="나 (호스트)"
                spellCheck={false}
                autoCapitalize="off"
              />
            </label>
          </div>

          {!hasToken && (
            <div className="host-actions">
              <button
                className="primary host-create-room"
                onClick={onCreateRoom}
                disabled={busyEnroll}
              >
                {busyEnroll ? "만드는 중…" : "직접 호스트 세션 만들기"}
              </button>
            </div>
          )}

          {enrollError && (
            <p className="host-danger-note" role="alert">
              {enrollError}
            </p>
          )}

          <div className="field-grid">
            <label className="field">
              <span>방 타입</span>
              <select
                className="select"
                value={roomMode}
                onChange={(e) => setRoomMode(e.target.value as RoomMode)}
                disabled={daemonRunning}
              >
                <option value="chat">Claude 대화</option>
                <option value="terminal">터미널 직접 조작</option>
              </select>
            </label>

            {roomMode === "chat" && (
              <label className="field">
                <span>권한 정책</span>
                <select
                  className="select"
                  value={permissionMode}
                  onChange={(e) =>
                    setPermissionMode(e.target.value as PermissionMode)
                  }
                  disabled={daemonRunning}
                >
                  <option value="default">요청마다 승인</option>
                  <option value="acceptEdits">파일 편집 자동 승인</option>
                  <option value="bypassPermissions">모든 작업 자동 승인</option>
                </select>
              </label>
            )}
          </div>

          {permissionMode === "bypassPermissions" && (
            <p className="host-danger-note" role="alert">
              ⚠ 모든 도구 호출이 승인 없이 실행됩니다. 게스트가 있는 방에서는
              사용하지 마세요.
            </p>
          )}

          <label className="field">
            <span>작업 폴더</span>
            <input
              type="text"
              value={defaultCwd}
              onChange={(e) => setDefaultCwd(e.target.value)}
              placeholder="비워두면 홈 디렉토리"
              spellCheck={false}
              autoCapitalize="off"
              disabled={daemonRunning}
            />
          </label>

          <div className="host-actions">
            <div className={`daemon-state ${daemonRunning ? "on" : "off"}`}>
              <span className="dot" />
              {daemonRunning
                ? "데몬 실행 중"
                : "데몬 정지됨"}
            </div>
            <button
              className={daemonRunning ? "danger" : "primary"}
              onClick={onToggleDaemon}
              disabled={!tauri || busyDaemon}
            >
              {busyDaemon
                ? "…"
                : daemonRunning
                  ? "데몬 정지"
                  : "데몬 시작"}
            </button>
          </div>

          <label className="toggle-field">
            <input
              type="checkbox"
              checked={autoStart}
              onChange={(e) => setAutoStart(e.target.checked)}
            />
            <span className="toggle-text">
              앱 시작 시 자동 실행
            </span>
          </label>
        </section>

        {/* 2) 초대 — 등급 선택 후 코드 발급 */}
        {hasToken && <section className="host-card">
          <div className="host-head">
            <h3>초대</h3>
          </div>

          <>
              <fieldset className="field invite-access invite-tier-picker">
                <legend>초대 등급</legend>
                <label className="radio">
                  <input
                    type="radio"
                    name="invite-access"
                    value="view"
                    checked={inviteAccess === "view"}
                    onChange={() => setInviteAccess("view")}
                  />
                  <span>보기 전용</span>
                </label>
                <label className="radio">
                  <input
                    type="radio"
                    name="invite-access"
                    value="control"
                    checked={inviteAccess === "control"}
                    onChange={() => setInviteAccess("control")}
                  />
                  <span>입력 허용</span>
                </label>
              </fieldset>

              <div className="host-actions">
                <button
                  className="primary"
                  onClick={onCreateInvite}
                  disabled={busyInvite}
                >
                  {busyInvite ? "발급 중…" : "초대 코드 발급"}
                </button>
              </div>
            </>

          {inviteCode && (
            <div className="invite-box">
              <div className="invite-code" aria-label="초대 코드">
                {inviteCode}
              </div>
              {inviteCodeAccess && (
                <span
                  className={`access-badge ${inviteCodeAccess}`}
                  title={
                    inviteCodeAccess === "view"
                      ? "이 코드로 온 참가자는 보기 전용입니다."
                      : "이 코드로 온 참가자는 입력할 수 있습니다 (터미널 방은 자동 드라이버)."
                  }
                >
                  {inviteCodeAccess === "view" ? "관전" : "조작"}
                </span>
              )}
              <button className="secondary" onClick={onCopy}>
                {copied ? "복사됨 ✓" : "복사"}
              </button>
            </div>
          )}
        </section>}

        {/* Shared status line for daemon/open-room/copy-token/invite errors. */}
        {notice && (
          <p className="host-notice" role="status">
            {notice}
          </p>
        )}

        {/* 3) 고급 설정 — 릴레이·수동 티켓·발급 옵션·권한·cwd·토큰 복사 (기본 접힘) */}
        <section className="host-card host-advanced">
          <button
            className="host-collapse-toggle"
            onClick={() => setAdvancedOpen((v) => !v)}
            aria-expanded={advancedOpen}
          >
            <span className="host-collapse-caret">
              {advancedOpen ? "▾" : "▸"}
            </span>
            <h3>고급 설정</h3>
          </button>

          {advancedOpen && (
            <div className="host-collapse-body">
              <label className="field">
                <span>Relay 주소</span>
                <input
                  type="text"
                  value={relayUrl}
                  onChange={(e) => setRelayUrl(e.target.value)}
                  placeholder={relayConfig.agentUrl}
                  spellCheck={false}
                  autoCapitalize="off"
                />
                <span className="field-help">
                  설정 변수: <code>PIE_RELAY_URL</code> (미설정 시 Azure)
                </span>
              </label>

              {managedMode && (
                <>
                  <label className="field">
                    <span>Control Plane 주소</span>
                    <input
                      type="text"
                      value={controlUrl}
                      onChange={(e) => setControlUrl(e.target.value)}
                      placeholder="https://control.cookai.dev"
                      spellCheck={false}
                      autoCapitalize="off"
                      disabled={daemonRunning}
                    />
                  </label>
                  <div className="field-grid">
                    <label className="field">
                      <span>기기 ID</span>
                      <input
                        type="text"
                        value={deviceId}
                        onChange={(e) => setDeviceId(e.target.value)}
                        spellCheck={false}
                        disabled={daemonRunning}
                      />
                    </label>
                    <label className="field">
                      <span>기기 이름</span>
                      <input
                        type="text"
                        value={deviceName}
                        onChange={(e) => setDeviceName(e.target.value)}
                        disabled={daemonRunning}
                      />
                    </label>
                  </div>
                  <label className="field">
                    <span>
                      소유 사용자 ID
                    </span>
                    <input
                      type="text"
                      value={ownerUserId}
                      onChange={(e) => setOwnerUserId(e.target.value)}
                      placeholder="일반 PAT는 토큰의 사용자 ID를 자동 사용"
                      spellCheck={false}
                      disabled={daemonRunning}
                    />
                  </label>
                </>
              )}

              <label className="field">
                <span>호스트 토큰 직접 입력</span>
                <input
                  type="password"
                  value={ticket}
                  onChange={(e) => setTicket(e.target.value)}
                  placeholder="‘방 만들기’로 자동 채워집니다"
                  spellCheck={false}
                  autoCapitalize="off"
                />
              </label>

              <label className="field">
                <span>Relay 발급 키</span>
                <input
                  type="password"
                  value={secret}
                  onChange={(e) => setSecret(e.target.value)}
                  placeholder={
                    localRelay
                      ? "로컬 릴레이는 비워도 됩니다"
                      : "릴레이 운영자에게 받은 키"
                  }
                  spellCheck={false}
                  autoCapitalize="off"
                  autoComplete="off"
                />
              </label>

              <div className="host-actions host-open-room">
                <button
                  className="secondary"
                  onClick={onCopyToken}
                  disabled={!daemonRunning}
                  title={
                    daemonRunning
                      ? "다른 기기의 '호스트 토큰 붙여넣기'에 넣을 토큰을 복사합니다."
                      : "데몬을 시작하면 토큰을 복사할 수 있습니다."
                  }
                >
                  {tokenCopied ? "복사됨 ✓" : "다른 기기용 토큰 복사"}
                </button>
              </div>

              {/* EXECUTOR_PATH는 UI에 노출하지 않는다 — Rust가 자동 해석(번들 리소스 →
                  개발 체크아웃)해 데몬에 넘기므로 사용자가 볼/만질 필요가 없다. 상태와
                  자동 채움(host_suggest_executor_path) 로직은 그대로 유지된다. */}
            </div>
          )}
        </section>

        {/* 4) 사이드카 로그 (기본 접힘) */}
        <section
          className={`host-card host-advanced${logOpen ? " log-card" : ""}`}
        >
          <div className="host-collapse-head">
            <button
              className="host-collapse-toggle"
              onClick={() => setLogOpen((v) => !v)}
              aria-expanded={logOpen}
            >
              <span className="host-collapse-caret">{logOpen ? "▾" : "▸"}</span>
              <h3>사이드카 로그</h3>
            </button>
            <button
              className="link"
              onClick={() => setLogs([])}
              disabled={logs.length === 0}
            >
              지우기
            </button>
          </div>
          {logOpen && (
            <div className="log-view" ref={logRef}>
              {logs.length === 0 ? (
                <span className="log-empty">아직 출력이 없습니다.</span>
              ) : (
                logs.map((l, i) => (
                  <div
                    key={i}
                    className={l.includes("!]") ? "log-line err" : "log-line"}
                  >
                    {l}
                  </div>
                ))
              )}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}

// formatExpiry renders the enroll response's unix-seconds expiry in the local
// timezone; 0/absent (no expiry reported) shows a dash rather than the epoch.
function formatExpiry(unixSeconds: number): string {
  if (!unixSeconds) return "—";
  return new Date(unixSeconds * 1000).toLocaleString();
}

function AuthBadge({ manualMode }: { manualMode: boolean }) {
  if (manualMode) {
    return <span className="auth-badge manual">수동 티켓 있음</span>;
  }
  return <span className="auth-badge none">티켓 없음 · 방 만들기 필요</span>;
}
