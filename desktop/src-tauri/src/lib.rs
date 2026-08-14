// Host mode (P2-2) wires the bundled Go `client` binary as a Tauri sidecar.
// Participant mode (P2-1) stays a pure shell — the React frontend still talks to
// the relay directly over fetch + WebSocket. The commands below exist only for
// host mode: they spawn `clientd` (the Go client) for login, the daemon, and
// invite creation, streaming its stdout/stderr to the frontend as `host-log`
// events. Spawning happens entirely in Rust, so the frontend never touches the
// shell plugin directly; the shell capability scope is declared for defense in
// depth (see capabilities/default.json).
use std::path::PathBuf;
use std::sync::Mutex;

use serde::{Deserialize, Serialize};
use tauri::path::BaseDirectory;
use tauri::{AppHandle, Emitter, Manager, State};
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

// SIDECAR is the BASENAME of the `bundle.externalBin` entry in tauri.conf.json
// ("binaries/clientd"). The Rust shell plugin resolves a sidecar by joining this
// string verbatim onto the exe's directory (tauri-plugin-shell
// process/mod.rs::relative_command_path), and the CLI copies the binary as a
// plain basename next to the exe (target/debug/clientd in dev,
// Contents/MacOS/clientd in the bundle). Passing "binaries/clientd" here made
// every spawn look for <exe_dir>/binaries/clientd → "No such file or directory
// (os error 2)". Do NOT put the directory prefix back.
const SIDECAR: &str = "clientd";

// LOG_EVENT carries one line of sidecar output to the frontend log view.
const LOG_EVENT: &str = "host-log";
// PROC_EXIT fires once a one-shot process (login / invite) or the daemon ends.
const PROC_EXIT: &str = "host-proc-exit";
// DAEMON_STATUS reflects the daemon's running flag so the UI indicator stays honest.
const DAEMON_STATUS: &str = "host-daemon-status";
const DEVICE_AGENT_STATUS: &str = "device-agent-status";
// The Pie Relay mobile gateway has its own lifecycle and event namespace;
// it must not be confused with the existing cloud relay daemon above.
const MOBILE_LOG_EVENT: &str = "mobile-gateway-log";
const MOBILE_STATUS_EVENT: &str = "mobile-gateway-status";
const MOBILE_READY_EVENT: &str = "mobile-gateway-ready";
const MOBILE_DEVICES_EVENT: &str = "mobile-gateway-devices";
const MOBILE_RELAY_STATUS_EVENT: &str = "mobile-relay-status";

// DaemonHandle holds the running daemon child so host_daemon_stop can kill it.
// Only one daemon runs at a time (a second host_daemon_start is rejected).
#[derive(Default)]
struct DaemonHandle(Mutex<Option<CommandChild>>);

#[derive(Default)]
struct DeviceAgentHandle(Mutex<Option<CommandChild>>);

#[derive(Default)]
struct MobileGatewayHandle(Mutex<Option<CommandChild>>);

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct LogLine {
    source: String, // "login" | "daemon" | "invite"
    stream: String, // "stdout" | "stderr"
    line: String,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct ProcExit {
    source: String,
    code: Option<i32>,
    signal: Option<i32>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct DaemonStatus {
    running: bool,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct MobileGatewayLog {
    stream: String,
    line: String,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct MobileGatewayStatus {
    running: bool,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct MobileGatewayReady {
    endpoint: String,
    pairing_url: String,
    device_id: String,
    port: u64,
    connection_mode: String,
    relay_endpoint: Option<String>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct MobileGatewayDevice {
    device_id: String,
    name: String,
    paired_at: u64,
    last_seen_at: u64,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct MobileGatewayDevices {
    devices: Vec<MobileGatewayDevice>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct MobileRelayStatus {
    status: String,
    detail: Option<String>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct RelayUrlEnvironment {
    relay_url: Option<String>,
}

fn non_empty_env(name: &str) -> Option<String> {
    std::env::var(name)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
}

#[tauri::command]
fn relay_url_environment() -> RelayUrlEnvironment {
    RelayUrlEnvironment {
        relay_url: non_empty_env("PIE_RELAY_URL"),
    }
}

// emit_lines splits a chunk of sidecar output into lines and forwards each as a
// LOG_EVENT. The shell plugin already buffers to line boundaries, but we split
// defensively in case a chunk carries an embedded newline.
fn emit_lines(app: &AppHandle, source: &str, stream: &str, bytes: Vec<u8>) {
    let text = String::from_utf8_lossy(&bytes);
    for line in text.split(['\n', '\r']) {
        if line.is_empty() {
            continue;
        }
        let _ = app.emit(
            LOG_EVENT,
            LogLine {
                source: source.to_string(),
                stream: stream.to_string(),
                line: line.to_string(),
            },
        );
    }
}

fn emit_mobile_log(app: &AppHandle, stream: &str, line: &str) {
    let _ = app.emit(
        MOBILE_LOG_EVENT,
        MobileGatewayLog {
            stream: stream.to_string(),
            line: line.to_string(),
        },
    );
}

// The host gateway prints one machine-readable ready record. Consume that
// record privately (it contains the pairing capability) and forward only a
// typed Tauri event; ordinary diagnostics go to the collapsible log view.
fn emit_mobile_stdout(app: &AppHandle, bytes: Vec<u8>) {
    let text = String::from_utf8_lossy(&bytes);
    for line in text.split(['\n', '\r']).filter(|line| !line.is_empty()) {
        let parsed = serde_json::from_str::<serde_json::Value>(line).ok();
        let record_type = parsed
            .as_ref()
            .and_then(|value| value.get("type"))
            .and_then(|value| value.as_str());
        if record_type == Some("mobile-host-ready") {
            let value = parsed.expect("parsed value exists");
            let ready = MobileGatewayReady {
                endpoint: value
                    .get("endpoint")
                    .and_then(|item| item.as_str())
                    .unwrap_or_default()
                    .to_string(),
                pairing_url: value
                    .get("pairingUrl")
                    .and_then(|item| item.as_str())
                    .unwrap_or_default()
                    .to_string(),
                device_id: value
                    .get("deviceId")
                    .and_then(|item| item.as_str())
                    .unwrap_or_default()
                    .to_string(),
                port: value
                    .get("port")
                    .and_then(|item| item.as_u64())
                    .unwrap_or(0),
                connection_mode: value
                    .get("connectionMode")
                    .and_then(|item| item.as_str())
                    .unwrap_or("local-only")
                    .to_string(),
                relay_endpoint: value
                    .get("relayEndpoint")
                    .and_then(|item| item.as_str())
                    .map(str::to_string),
            };
            if !ready.endpoint.is_empty()
                && !ready.pairing_url.is_empty()
                && !ready.device_id.is_empty()
                && ready.port > 0
            {
                let _ = app.emit(MOBILE_READY_EVENT, ready);
            } else {
                emit_mobile_log(
                    app,
                    "stderr",
                    "모바일 호스트가 잘못된 준비 응답을 보냈습니다.",
                );
            }
        } else if record_type == Some("mobile-host-devices") {
            let value = parsed.expect("parsed value exists");
            match value.get("devices").cloned().and_then(|devices| {
                serde_json::from_value::<Vec<MobileGatewayDevice>>(devices).ok()
            }) {
                Some(devices) => {
                    let _ = app.emit(MOBILE_DEVICES_EVENT, MobileGatewayDevices { devices });
                }
                None => emit_mobile_log(app, "stderr", "등록 기기 목록을 읽지 못했습니다."),
            }
        } else if record_type == Some("mobile-host-device-revoked") {
            let revoked = parsed
                .as_ref()
                .and_then(|value| value.get("revoked"))
                .and_then(|value| value.as_bool())
                .unwrap_or(false);
            if !revoked {
                emit_mobile_log(app, "stderr", "요청한 모바일 기기를 폐기하지 못했습니다.");
            }
        } else if record_type == Some("mobile-relay-status") {
            let value = parsed.expect("parsed value exists");
            let status = value
                .get("status")
                .and_then(|item| item.as_str())
                .unwrap_or("offline")
                .to_string();
            let detail = value
                .get("detail")
                .and_then(|item| item.as_str())
                .map(str::to_string);
            let _ = app.emit(
                MOBILE_RELAY_STATUS_EVENT,
                MobileRelayStatus { status, detail },
            );
        } else {
            emit_mobile_log(app, "stdout", line);
        }
    }
}

// host_credentials_exist reports whether `client login` has saved
// ~/.cli-relay/credentials.json. The Go client has no `status` subcommand and
// ../client is read-only, so we probe the same path the client uses directly
// rather than shelling out.
#[tauri::command]
fn host_credentials_exist(app: AppHandle) -> bool {
    match app.path().home_dir() {
        Ok(home) => home.join(".cli-relay").join("credentials.json").exists(),
        Err(_) => false,
    }
}

// host_access_token returns the accessToken saved in
// ~/.cli-relay/credentials.json so the frontend can join its own room as the
// host over /ws/participant (P3-3). Returns "" (not an error) when the file is
// absent or has no token, so the caller can fall back to the manual ticket and
// show a single actionable message. Only this leaf value is read; the rest of
// the credential blob (refresh token, expiry) stays in Rust.
#[tauri::command]
fn host_access_token(app: AppHandle) -> String {
    let path = match app.path().home_dir() {
        Ok(home) => home.join(".cli-relay").join("credentials.json"),
        Err(_) => return String::new(),
    };
    let bytes = match std::fs::read(&path) {
        Ok(b) => b,
        Err(_) => return String::new(),
    };
    match serde_json::from_slice::<serde_json::Value>(&bytes) {
        Ok(v) => v
            .get("accessToken")
            .and_then(|t| t.as_str())
            .unwrap_or("")
            .to_string(),
        Err(_) => String::new(),
    }
}

// strip_verbatim_prefix removes a Windows extended-length ("\\?\") path prefix so
// the result is a plain drive/UNC path. Tauri's Resource path resolution and
// std::fs::canonicalize yield \\?\C:\... on Windows, but Node's main-module
// resolver (realpathSync) crashes on it (EISDIR on "C:") when we run
// `node <pty-host.mjs>`. No-op on unprefixed paths, so it's harmless on unix.
fn strip_verbatim_prefix(p: &str) -> String {
    if let Some(rest) = p.strip_prefix(r"\\?\UNC\") {
        format!(r"\\{rest}")
    } else if let Some(rest) = p.strip_prefix(r"\\?\") {
        rest.to_string()
    } else {
        p.to_string()
    }
}

// host_suggest_executor_path best-effort resolves the absolute path of the
// executor.mjs so the GUI can pre-fill the daemon's EXECUTOR_PATH field, using a
// three-tier fallback:
//   (a) a packaged app ships node-executor as a bundle resource — prefer it;
//   (b) a dev checkout resolves the sibling ../client/node-executor;
//   (c) nothing found -> "" (the GUI then asks the user to fill it in).
// An explicit EXECUTOR_PATH env override wins over all of these.
#[tauri::command]
fn host_suggest_executor_path(app: AppHandle) -> String {
    if let Ok(p) = std::env::var("EXECUTOR_PATH") {
        if !p.trim().is_empty() {
            return p;
        }
    }
    // (a) Bundled resource (see tauri.conf.json > bundle.resources). In a packaged
    // app this resolves under Contents/Resources; in dev it usually won't exist.
    if let Ok(res) = app.path().resolve(
        "resources/node-executor/executor.mjs",
        BaseDirectory::Resource,
    ) {
        if res.exists() {
            return strip_verbatim_prefix(&res.to_string_lossy());
        }
    }
    // (b) Sibling dev checkout.
    const REL: &str = "node-executor/executor.mjs";
    let mut bases: Vec<PathBuf> = Vec::new();
    if let Ok(cwd) = std::env::current_dir() {
        bases.push(cwd);
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            bases.push(dir.to_path_buf());
        }
    }
    // Reach the sibling client/ checkout from either the working dir (tauri dev
    // runs from src-tauri/) or the built binary's dir (target/<profile>/).
    let suffixes = [
        "../client",
        "../../client",
        "../../../client",
        "../../../../client",
        "client",
    ];
    for base in &bases {
        for suffix in &suffixes {
            let cand = base.join(suffix).join(REL);
            if cand.exists() {
                if let Ok(canon) = cand.canonicalize() {
                    return strip_verbatim_prefix(&canon.to_string_lossy());
                }
            }
        }
    }
    String::new()
}

// host_sidecar_preflight verifies the clientd sidecar binary is actually present
// next to the running executable before we try to spawn it. Without this, Tauri's
// shell plugin surfaces a raw "No such file or directory (os error 2)" when the
// sidecar was never built — the common dev-checkout case where `npm run sidecar`
// hasn't been run yet. We return an actionable Korean message instead. The
// bundled app ships it as `clientd`; a dev build uses `clientd-<target-triple>`,
// so we match any entry starting with `clientd`.
fn host_sidecar_preflight() -> Result<(), String> {
    let exe =
        std::env::current_exe().map_err(|e| format!("실행 파일 경로를 확인할 수 없습니다: {e}"))?;
    let dir = exe
        .parent()
        .ok_or_else(|| "실행 파일 디렉터리를 확인할 수 없습니다.".to_string())?;
    let present = std::fs::read_dir(dir)
        .map(|entries| {
            entries
                .filter_map(|e| e.ok())
                .any(|e| e.file_name().to_string_lossy().starts_with("clientd"))
        })
        .unwrap_or(false);
    if present {
        Ok(())
    } else {
        Err(
            "사이드카(clientd)를 찾을 수 없습니다 — `npm run sidecar` 실행 후 앱을 재시작하세요."
                .to_string(),
        )
    }
}

fn host_pty_path(app: &AppHandle) -> Result<String, String> {
    let executor = host_suggest_executor_path(app.clone());
    if executor.is_empty() {
        return Err("PTY 실행 파일을 찾을 수 없습니다 — 앱 리소스를 다시 빌드하세요.".to_string());
    }
    let path = std::path::Path::new(&executor).with_file_name("pty-host.mjs");
    if !path.exists() {
        return Err("pty-host.mjs를 찾을 수 없습니다 — 앱 리소스를 다시 빌드하세요.".to_string());
    }
    Ok(strip_verbatim_prefix(&path.to_string_lossy()))
}

// device_agent_start runs the multi-session Host OS agent. It registers this
// computer with Pie Control Plane and pulls desired Session rows outbound, so
// the local loopback Session Manager never has to be exposed through NAT or a
// firewall. The PAT stays in the child environment rather than process args.
#[tauri::command]
async fn device_agent_start(
    app: AppHandle,
    state: State<'_, DeviceAgentHandle>,
    control_url: String,
    control_token: String,
    device_id: String,
    device_name: String,
) -> Result<(), String> {
    if state.0.lock().unwrap().is_some() {
        return Ok(());
    }
    if !(control_url.starts_with("http://") || control_url.starts_with("https://")) {
        return Err("Control Plane 주소는 http:// 또는 https:// 주소여야 합니다.".to_string());
    }
    if control_token.trim().is_empty() || device_id.trim().is_empty() {
        return Err("PAT와 기기 ID가 필요합니다.".to_string());
    }
    host_sidecar_preflight()?;
    let pty_path = host_pty_path(&app)?;
    let mut cmd = app
        .shell()
        .sidecar(SIDECAR)
        .map_err(|error| error.to_string())?
        .args(["sessions", "serve"])
        .env("CLI_RELAY_APP_PID", std::process::id().to_string())
        .env("PIE_CONTROL_PLANE_URL", control_url)
        .env("PIE_CONTROL_PLANE_TOKEN", control_token)
        .env("PIE_DEVICE_ID", device_id)
        .env("PIE_DEVICE_NAME", device_name)
        .env("PTY_HOST_PATH", pty_path)
        .env("PIE_SESSION_MANAGER_ADDR", "127.0.0.1:19091");
    // A packaged macOS app may not inherit the interactive shell's Node path.
    // Preserve PATH explicitly when it exists; pty-host.mjs itself resolves the
    // user's shell and does not receive any Control Plane credential.
    if let Ok(path) = std::env::var("PATH") {
        cmd = cmd.env("PATH", path);
    }
    let (mut rx, child) = cmd.spawn().map_err(|error| error.to_string())?;
    *state.0.lock().unwrap() = Some(child);
    let _ = app.emit(DEVICE_AGENT_STATUS, DaemonStatus { running: true });
    let app2 = app.clone();
    tauri::async_runtime::spawn(async move {
        let mut exit = ProcExit {
            source: "device-agent".to_string(),
            code: None,
            signal: None,
        };
        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(bytes) => emit_lines(&app2, "device-agent", "stdout", bytes),
                CommandEvent::Stderr(bytes) => emit_lines(&app2, "device-agent", "stderr", bytes),
                CommandEvent::Error(error) => {
                    emit_lines(&app2, "device-agent", "stderr", error.into_bytes())
                }
                CommandEvent::Terminated(payload) => {
                    exit.code = payload.code;
                    exit.signal = payload.signal;
                }
                _ => {}
            }
        }
        if let Some(handle) = app2.try_state::<DeviceAgentHandle>() {
            *handle.0.lock().unwrap() = None;
        }
        let _ = app2.emit(PROC_EXIT, exit);
        let _ = app2.emit(DEVICE_AGENT_STATUS, DaemonStatus { running: false });
    });
    Ok(())
}

#[tauri::command]
fn device_agent_running(state: State<'_, DeviceAgentHandle>) -> bool {
    state.0.lock().unwrap().is_some()
}

#[tauri::command]
fn device_agent_stop(state: State<'_, DeviceAgentHandle>) -> Result<(), String> {
    match state.0.lock().unwrap().take() {
        Some(child) => child.kill().map_err(|error| error.to_string()),
        None => Ok(()),
    }
}

// host_daemon_start spawns `client` (no subcommand = daemon), passing the relay
// URL, an optional manual ticket, the executor path, and the room permission
// policy as environment. The child inherits the parent environment (PATH/HOME)
// so it can find node and the saved credentials; only the known keys are
// overridden. permission_mode/default_cwd realize the P3-2 room policy: the
// daemon forwards CLI_RELAY_PERMISSION_MODE / CLI_RELAY_DEFAULT_CWD to the
// executor, which overrides every chat's own permissionMode/cwd.
// room_mode realizes P5-3: "terminal" sets CLI_RELAY_ROOM_MODE=terminal so the
// daemon supervises the PTY host (zsh) instead of the SDK chat executor; "chat"
// (or absent) leaves it unset for the legacy chat path. CLI_RELAY_DEFAULT_CWD is
// shared as the PTY's start directory in terminal mode.
// Tauri exposes command parameters as individually named invoke arguments;
// grouping them would change the JavaScript IPC contract.
#[allow(clippy::too_many_arguments)]
#[tauri::command]
async fn host_daemon_start(
    app: AppHandle,
    state: State<'_, DaemonHandle>,
    relay_url: Option<String>,
    ticket: Option<String>,
    executor_path: Option<String>,
    permission_mode: Option<String>,
    default_cwd: Option<String>,
    room_mode: Option<String>,
) -> Result<(), String> {
    if state.0.lock().unwrap().is_some() {
        return Err("데몬이 이미 실행 중입니다.".to_string());
    }
    host_sidecar_preflight()?;
    let mut cmd = app.shell().sidecar(SIDECAR).map_err(|e| e.to_string())?;
    // Die-with-parent: pass our pid so the daemon shuts itself down if this app
    // is force-killed (a `tauri dev` rebuild's SIGKILL, a crash) — those paths
    // skip RunEvent::Exit, so without this the sidecar orphans (PPID=1) and keeps
    // its relay ws open, desyncing "정지됨" from a still-connected host.
    cmd = cmd.env("CLI_RELAY_APP_PID", std::process::id().to_string());
    if let Some(u) = relay_url.filter(|s| !s.trim().is_empty()) {
        cmd = cmd.env("PIE_RELAY_URL", u);
    }
    if let Some(t) = ticket.filter(|s| !s.trim().is_empty()) {
        cmd = cmd.env("RELAY_TICKET", t);
    }
    // Strip any Windows extended-length ("\\?\") prefix up front: a value stored
    // by an earlier build (localStorage) may still carry it, and running
    // `node \\?\C:\...\pty-host.mjs` crashes Node (EISDIR on "C:"). Applied once
    // here so both EXECUTOR_PATH and the derived PTY_HOST_PATH sibling are clean.
    let executor_path = executor_path
        .filter(|s| !s.trim().is_empty())
        .map(|s| strip_verbatim_prefix(&s));
    // Keep a copy — terminal mode below derives PTY_HOST_PATH from it (the sibling
    // pty-host.mjs).
    let executor_path_for_pty = executor_path.clone();
    if let Some(p) = executor_path {
        cmd = cmd.env("EXECUTOR_PATH", p);
    }
    // Only whitelisted policy values reach the executor; an unknown string is
    // dropped so a stale/garbage mode can't silently disable approvals.
    if let Some(m) = permission_mode
        .filter(|s| matches!(s.as_str(), "default" | "acceptEdits" | "bypassPermissions"))
    {
        cmd = cmd.env("CLI_RELAY_PERMISSION_MODE", m);
    }
    if let Some(c) = default_cwd.filter(|s| !s.trim().is_empty()) {
        cmd = cmd.env("CLI_RELAY_DEFAULT_CWD", c);
    }
    // Only the terminal room type sets the env; anything else (incl. "chat")
    // leaves it unset so the daemon takes its default SDK chat path. The daemon
    // then supervises pty-host.mjs instead of executor.mjs — and it can't resolve
    // that path on its own in a dev checkout (it lives next to executor.mjs, not
    // next to the sidecar binary), so we hand it PTY_HOST_PATH derived as the
    // sibling of the executor path the GUI already resolved.
    if let Some(_m) = room_mode.filter(|s| s == "terminal") {
        cmd = cmd.env("CLI_RELAY_ROOM_MODE", "terminal");
        if let Some(exec) = &executor_path_for_pty {
            let sib = std::path::Path::new(exec).with_file_name("pty-host.mjs");
            if sib.exists() {
                cmd = cmd.env("PTY_HOST_PATH", sib.to_string_lossy().to_string());
            }
        }
    }
    let (mut rx, child) = cmd.spawn().map_err(|e| e.to_string())?;
    *state.0.lock().unwrap() = Some(child);
    let _ = app.emit(DAEMON_STATUS, DaemonStatus { running: true });

    let app2 = app.clone();
    tauri::async_runtime::spawn(async move {
        let mut exit = ProcExit {
            source: "daemon".to_string(),
            code: None,
            signal: None,
        };
        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(b) => emit_lines(&app2, "daemon", "stdout", b),
                CommandEvent::Stderr(b) => emit_lines(&app2, "daemon", "stderr", b),
                CommandEvent::Error(e) => emit_lines(&app2, "daemon", "stderr", e.into_bytes()),
                CommandEvent::Terminated(p) => {
                    exit.code = p.code;
                    exit.signal = p.signal;
                }
                _ => {}
            }
        }
        // The child ended on its own (crash/exit) or via host_daemon_stop; clear
        // the handle either way so the UI and a future start stay consistent.
        if let Some(st) = app2.try_state::<DaemonHandle>() {
            *st.0.lock().unwrap() = None;
        }
        let _ = app2.emit(PROC_EXIT, exit);
        let _ = app2.emit(DAEMON_STATUS, DaemonStatus { running: false });
    });
    Ok(())
}

// host_daemon_running reports whether a daemon child is currently held. The
// GUI's daemonRunning is otherwise driven only by start/stop events, so a
// remounted HostPanel (switching to a room and back) would reset to "정지됨"
// even while the daemon runs. HostPanel queries this on mount to re-sync.
#[tauri::command]
fn host_daemon_running(state: State<'_, DaemonHandle>) -> bool {
    state.0.lock().unwrap().is_some()
}

// host_daemon_stop kills the running daemon. The reader task above observes the
// termination and clears the handle + emits the status update.
#[tauri::command]
fn host_daemon_stop(state: State<'_, DaemonHandle>) -> Result<(), String> {
    let child = state.0.lock().unwrap().take();
    match child {
        Some(c) => c.kill().map_err(|e| e.to_string()),
        None => Err("실행 중인 데몬이 없습니다.".to_string()),
    }
}

// host_invite_create runs `client room create --relay-url URL` (credentials
// mode) and returns the raw stdout for the frontend to parse. This path needs
// saved credentials; the manual-ticket path POSTs /rooms/invites straight from
// the webview instead (see the frontend), because `room create` has no ticket
// override.
#[tauri::command]
async fn host_invite_create(app: AppHandle, relay_url: Option<String>) -> Result<String, String> {
    host_sidecar_preflight()?;
    let mut args = vec!["room".to_string(), "create".to_string()];
    if let Some(u) = relay_url.filter(|s| !s.trim().is_empty()) {
        args.push("--relay-url".to_string());
        args.push(u);
    }
    let output = app
        .shell()
        .sidecar(SIDECAR)
        .map_err(|e| e.to_string())?
        .args(args)
        .output()
        .await
        .map_err(|e| e.to_string())?;

    let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).into_owned();
    for line in stdout.lines() {
        let _ = app.emit(
            LOG_EVENT,
            LogLine {
                source: "invite".to_string(),
                stream: "stdout".to_string(),
                line: line.to_string(),
            },
        );
    }
    for line in stderr.lines() {
        let _ = app.emit(
            LOG_EVENT,
            LogLine {
                source: "invite".to_string(),
                stream: "stderr".to_string(),
                line: line.to_string(),
            },
        );
    }
    if !output.status.success() {
        let msg = stderr.trim();
        return Err(if msg.is_empty() {
            format!("초대 코드 발급 실패 (종료코드 {:?})", output.status.code())
        } else {
            msg.to_string()
        });
    }
    Ok(stdout)
}

// Resolve the bundled gateway first, then the source adapter's build output in
// a development checkout. The latter keeps `cargo check` and `tauri dev`
// usable without staging resources by hand after every TypeScript edit.
fn resolve_mobile_gateway_script(app: &AppHandle) -> Result<PathBuf, String> {
    if let Ok(resource) = app.path().resolve(
        "resources/mobile-host/host-gateway.mjs",
        BaseDirectory::Resource,
    ) {
        if resource.exists() {
            return Ok(resource);
        }
    }

    let mut bases = Vec::new();
    if let Ok(cwd) = std::env::current_dir() {
        bases.push(cwd);
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(parent) = exe.parent() {
            bases.push(parent.to_path_buf());
        }
    }
    let candidates = [
        "../pie-mobile/adapter/host-gateway/dist/host-gateway.mjs",
        "../../pie-mobile/adapter/host-gateway/dist/host-gateway.mjs",
        "../../../pie-mobile/adapter/host-gateway/dist/host-gateway.mjs",
        "../../../../pie-mobile/adapter/host-gateway/dist/host-gateway.mjs",
        "pie-mobile/adapter/host-gateway/dist/host-gateway.mjs",
    ];
    for base in bases {
        for relative in candidates {
            let candidate = base.join(relative);
            if candidate.exists() {
                return candidate
                    .canonicalize()
                    .map_err(|error| format!("모바일 호스트 경로를 확인할 수 없습니다: {error}"));
            }
        }
    }
    Err(
        "Pie Relay 모바일 호스트를 찾을 수 없습니다 — `npm run prepare-mobile-host`를 실행하세요."
            .to_string(),
    )
}

// Keep the established individually named Tauri invoke arguments stable.
#[allow(clippy::too_many_arguments)]
#[tauri::command]
async fn mobile_gateway_start(
    app: AppHandle,
    state: State<'_, MobileGatewayHandle>,
    advertise_host: Option<String>,
    default_cwd: Option<String>,
    rotate_pairing: Option<bool>,
    connection_mode: Option<String>,
    relay_url: Option<String>,
    relay_token: Option<String>,
) -> Result<(), String> {
    if state.0.lock().unwrap().is_some() {
        return Err("Pie Relay 모바일 호스트가 이미 실행 중입니다.".to_string());
    }

    let script = resolve_mobile_gateway_script(&app)?;
    let home = app
        .path()
        .home_dir()
        .map_err(|error| format!("홈 디렉터리를 확인할 수 없습니다: {error}"))?;
    let cwd = default_cwd
        .filter(|value| !value.trim().is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| home.clone());
    if !cwd.is_dir() {
        return Err(format!("시작 폴더가 존재하지 않습니다: {}", cwd.display()));
    }
    let legacy_data_dir = home.join(".cli-relay").join("orca-mobile");
    let new_data_dir = home.join(".pie-relay").join("mobile");
    let data_dir = if !new_data_dir.exists() && legacy_data_dir.exists() {
        legacy_data_dir
    } else {
        new_data_dir
    };
    let connection_mode = connection_mode.unwrap_or_else(|| "local-only".to_string());
    if !matches!(
        connection_mode.as_str(),
        "automatic" | "local-only" | "relay-only"
    ) {
        return Err("지원하지 않는 모바일 연결 모드입니다.".to_string());
    }
    let mut args = vec![
        strip_verbatim_prefix(&script.to_string_lossy()),
        "--data-dir".to_string(),
        strip_verbatim_prefix(&data_dir.to_string_lossy()),
        "--cwd".to_string(),
        strip_verbatim_prefix(&cwd.to_string_lossy()),
        "--no-terminal-qr".to_string(),
        "--connection-mode".to_string(),
        connection_mode.clone(),
    ];
    if let Some(host) = advertise_host.filter(|value| !value.trim().is_empty()) {
        args.push("--advertise-host".to_string());
        args.push(host.trim().to_string());
    }
    if rotate_pairing.unwrap_or(false) {
        args.push("--rotate-pairing".to_string());
    }
    let relay_token = if connection_mode != "local-only" {
        let relay_url = relay_url
            .filter(|value| !value.trim().is_empty())
            .ok_or_else(|| "Relay 연결에는 Relay 서버 URL이 필요합니다.".to_string())?;
        args.push("--relay-url".to_string());
        args.push(relay_url.trim().to_string());
        Some(
            relay_token
                .filter(|value| !value.trim().is_empty())
                .ok_or_else(|| "Relay 연결에는 호스트 토큰이 필요합니다.".to_string())?,
        )
    } else {
        None
    };

    let mut command = app
        .shell()
        .command("node")
        .args(args)
        .env("PIE_RELAY_APP_PID", std::process::id().to_string());
    if let Some(token) = relay_token {
        command = command.env("PIE_RELAY_TOKEN", token);
    }
    let (mut rx, child) = command.spawn().map_err(|error| {
        format!(
            "Pie Relay 모바일 호스트를 시작하지 못했습니다. Node.js 20 이상을 확인하세요: {error}"
        )
    })?;
    *state.0.lock().unwrap() = Some(child);
    let _ = app.emit(MOBILE_STATUS_EVENT, MobileGatewayStatus { running: true });

    let app2 = app.clone();
    tauri::async_runtime::spawn(async move {
        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(bytes) => emit_mobile_stdout(&app2, bytes),
                CommandEvent::Stderr(bytes) => {
                    let text = String::from_utf8_lossy(&bytes);
                    for line in text.split(['\n', '\r']).filter(|line| !line.is_empty()) {
                        emit_mobile_log(&app2, "stderr", line);
                    }
                }
                CommandEvent::Error(error) => emit_mobile_log(&app2, "stderr", &error),
                _ => {}
            }
        }
        if let Some(handle) = app2.try_state::<MobileGatewayHandle>() {
            *handle.0.lock().unwrap() = None;
        }
        let _ = app2.emit(MOBILE_STATUS_EVENT, MobileGatewayStatus { running: false });
    });
    Ok(())
}

#[tauri::command]
fn mobile_gateway_running(state: State<'_, MobileGatewayHandle>) -> bool {
    state.0.lock().unwrap().is_some()
}

#[tauri::command]
fn mobile_gateway_list_devices(state: State<'_, MobileGatewayHandle>) -> Result<(), String> {
    let mut guard = state.0.lock().unwrap();
    let child = guard
        .as_mut()
        .ok_or_else(|| "Pie Relay 모바일 호스트가 실행 중이 아닙니다.".to_string())?;
    child
        .write(b"{\"type\":\"list-devices\"}\n")
        .map_err(|error| error.to_string())
}

#[tauri::command]
fn mobile_gateway_revoke_device(
    state: State<'_, MobileGatewayHandle>,
    device_id: String,
) -> Result<(), String> {
    let command = serde_json::json!({
        "type": "revoke-device",
        "deviceId": device_id,
    });
    let mut payload = serde_json::to_vec(&command).map_err(|error| error.to_string())?;
    payload.push(b'\n');
    let mut guard = state.0.lock().unwrap();
    let child = guard
        .as_mut()
        .ok_or_else(|| "Pie Relay 모바일 호스트가 실행 중이 아닙니다.".to_string())?;
    child.write(&payload).map_err(|error| error.to_string())
}

#[tauri::command]
fn mobile_gateway_info(app: AppHandle) -> Option<MobileGatewayReady> {
    let home = app.path().home_dir().ok()?;
    let new_path = home.join(".pie-relay").join("mobile").join("pairing.json");
    let legacy_path = home
        .join(".cli-relay")
        .join("orca-mobile")
        .join("pairing.json");
    let path = if new_path.exists() {
        new_path
    } else {
        legacy_path
    };
    let value = serde_json::from_slice::<serde_json::Value>(&std::fs::read(path).ok()?).ok()?;
    Some(MobileGatewayReady {
        endpoint: value.get("endpoint")?.as_str()?.to_string(),
        pairing_url: value.get("pairingUrl")?.as_str()?.to_string(),
        device_id: value.get("deviceId")?.as_str()?.to_string(),
        port: value.get("port")?.as_u64()?,
        connection_mode: value
            .get("connectionMode")
            .and_then(|item| item.as_str())
            .unwrap_or("local-only")
            .to_string(),
        relay_endpoint: value
            .get("relayEndpoint")
            .and_then(|item| item.as_str())
            .map(str::to_string),
    })
}

#[tauri::command]
fn mobile_gateway_stop(state: State<'_, MobileGatewayHandle>) -> Result<(), String> {
    match state.0.lock().unwrap().take() {
        Some(child) => child.kill().map_err(|error| error.to_string()),
        None => Err("실행 중인 Pie Relay 모바일 호스트가 없습니다.".to_string()),
    }
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .manage(DaemonHandle::default())
        .manage(DeviceAgentHandle::default())
        .manage(MobileGatewayHandle::default())
        .invoke_handler(tauri::generate_handler![
            relay_url_environment,
            host_credentials_exist,
            host_access_token,
            host_suggest_executor_path,
            host_daemon_start,
            host_daemon_stop,
            host_daemon_running,
            device_agent_start,
            device_agent_stop,
            device_agent_running,
            host_invite_create,
            mobile_gateway_start,
            mobile_gateway_stop,
            mobile_gateway_running,
            mobile_gateway_info,
            mobile_gateway_list_devices,
            mobile_gateway_revoke_device,
        ])
        .build(tauri::generate_context!())
        .expect("error while building tauri application");

    // Kill the daemon when the app exits. A Tauri sidecar is NOT killed on app
    // quit — it gets reparented to launchd (PPID=1) and keeps its relay ws open,
    // so after a restart the GUI shows "정지됨" (fresh DaemonHandle) while the
    // orphan is still registered as the room's host → participants keep seeing
    // "호스트 연결". Killing it on Exit keeps GUI state and reality in sync.
    app.run(|handle, event| {
        if let tauri::RunEvent::Exit = event {
            if let Some(state) = handle.try_state::<DaemonHandle>() {
                if let Some(child) = state.0.lock().unwrap().take() {
                    let _ = child.kill();
                }
            }
            if let Some(state) = handle.try_state::<DeviceAgentHandle>() {
                if let Some(child) = state.0.lock().unwrap().take() {
                    let _ = child.kill();
                }
            }
            if let Some(state) = handle.try_state::<MobileGatewayHandle>() {
                if let Some(child) = state.0.lock().unwrap().take() {
                    let _ = child.kill();
                }
            }
        }
    });
}
