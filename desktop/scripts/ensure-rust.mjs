#!/usr/bin/env node
// Detect a system Rust/Cargo toolchain (required by Tauri) and fail fast with
// a clear, actionable message instead of letting `tauri dev`/`tauri build`
// die deep inside `cargo metadata` with a cryptic "No such file or directory".
//
// Rust/Cargo is a system toolchain installed via rustup — it is NOT an npm
// package, so `npm install` alone cannot provide it. This script mirrors the
// existing `presidecar` Go-toolchain check in package.json, but is written in
// Node (not bash) so it also works on Windows, where bash may be absent.
//
// Modes (argv):
//   (none)      GATE   — missing cargo -> print instructions, exit 1
//   --warn      SOFT   — missing cargo -> print instructions, exit 0 (used by postinstall)
//   --install   ATTEMPT — try to auto-install via rustup on macOS/Linux
//
// No external dependencies: only node:child_process / node:fs / node:os / node:path.

import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir, platform } from "node:os";
import { join } from "node:path";

const TAURI_PREREQS_URL = "https://tauri.app/start/prerequisites/";

function runCargoVersion(cargoPath) {
  try {
    const result = spawnSync(cargoPath, ["--version"], {
      stdio: ["ignore", "pipe", "ignore"],
    });
    return result.status === 0;
  } catch {
    // e.g. ENOENT if the binary doesn't exist / isn't executable.
    return false;
  }
}

// Returns { found: boolean, path: string|null, onPath: boolean }
function detectCargo() {
  // 1) Try cargo on PATH first.
  if (runCargoVersion("cargo")) {
    return { found: true, path: "cargo", onPath: true };
  }

  // 2) Probe common rustup install locations that may not be on PATH yet in
  //    the current shell (e.g. right after a fresh rustup install, before the
  //    user has restarted their shell or sourced ~/.cargo/env).
  const home = homedir();
  const isWindows = platform() === "win32";
  const probePath = isWindows
    ? join(process.env.USERPROFILE || home, ".cargo", "bin", "cargo.exe")
    : join(home, ".cargo", "bin", "cargo");

  if (existsSync(probePath) && runCargoVersion(probePath)) {
    return { found: true, path: probePath, onPath: false };
  }

  return { found: false, path: null, onPath: false };
}

function printMissingInstructions() {
  const isWindows = platform() === "win32";

  console.error("");
  console.error("✗ Rust/Cargo 가 필요합니다 (Tauri 전제조건)");
  console.error(
    "  Tauri는 시스템에 설치된 Rust 툴체인(rustup)이 필요합니다. npm 패키지로는 설치되지 않습니다."
  );
  console.error("");

  if (isWindows) {
    console.error("  설치 방법 (Windows):");
    console.error("    1) Rust:  winget install Rustlang.Rustup");
    console.error("       (또는 https://rustup.rs 에서 rustup-init.exe 실행)");
    console.error(
      "    2) C++ 빌드 도구 + Windows SDK (link.exe · Rust MSVC 링크에 필수):"
    );
    console.error(
      '       winget install --id Microsoft.VisualStudio.2022.BuildTools -e --override "--quiet --wait --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended"'
    );
    console.error(
      "       (워크로드 없이 BuildTools 만 깔면 link.exe 가 없어 컴파일이 실패합니다)"
    );
    console.error(
      "    3) WebView2: Win10/11 은 대부분 기본 설치됨 (없으면 아래 링크)"
    );
    console.error(
      "       https://developer.microsoft.com/microsoft-edge/webview2/"
    );
    console.error("    설치 후 새 터미널을 열어 다시 시도하세요.");
  } else {
    console.error("  설치 방법 (macOS/Linux):");
    console.error(
      "    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y"
    );
    console.error('    source "$HOME/.cargo/env"');
    console.error("");
    console.error("  또는: npm run setup:rust");
  }

  console.error("");
  console.error(`  참고: ${TAURI_PREREQS_URL}`);
  console.error("");
}

function printFoundButNotOnPathNote(cargoPath) {
  const isWindows = platform() === "win32";
  console.error(
    `(참고) cargo 가 PATH에는 없지만 ${cargoPath} 에서 발견했습니다.`
  );
  if (isWindows) {
    console.error(
      "  PATH에 %USERPROFILE%\\.cargo\\bin 을 추가하거나 새 터미널을 여세요."
    );
  } else {
    console.error(
      '  PATH에 ~/.cargo/bin 이 없다면 `source "$HOME/.cargo/env"` 를 실행하거나 셸을 재시작하세요.'
    );
  }
}

function attemptInstall() {
  const isWindows = platform() === "win32";

  if (isWindows) {
    console.error("Windows에서는 자동 설치를 시도하지 않습니다.");
    console.error("  winget install Rustlang.Rustup");
    console.error(
      '  winget install --id Microsoft.VisualStudio.2022.BuildTools -e --override "--quiet --wait --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended"'
    );
    console.error(
      "  (VCTools 워크로드 = link.exe + Windows SDK; WebView2 는 Win10/11 기본 설치)"
    );
    console.error(`참고: ${TAURI_PREREQS_URL}`);
    process.exit(1);
  }

  console.log("rustup 설치를 시도합니다 (https://sh.rustup.rs)...");
  const install = spawnSync(
    "sh",
    [
      "-c",
      "curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y",
    ],
    { stdio: "inherit" }
  );

  if (install.status !== 0) {
    console.error("");
    console.error("✗ rustup 설치 스크립트가 실패했습니다.");
    console.error(`  수동 설치: ${TAURI_PREREQS_URL}`);
    process.exit(1);
  }

  console.log("");
  console.log('설치가 끝나면 다음을 실행해 현재 셸에 반영하세요:');
  console.log('  source "$HOME/.cargo/env"');

  const recheck = detectCargo();
  if (recheck.found) {
    console.log("✓ cargo 설치를 확인했습니다 (새 셸/터미널에서 이용 가능).");
    process.exit(0);
  } else {
    console.error(
      '✗ 설치 후에도 cargo 를 찾지 못했습니다. `source "$HOME/.cargo/env"` 실행 후 다시 시도하세요.'
    );
    process.exit(1);
  }
}

function main() {
  const args = process.argv.slice(2);
  const mode = args.includes("--install")
    ? "install"
    : args.includes("--warn")
      ? "warn"
      : "gate";

  if (mode === "install") {
    attemptInstall();
    return;
  }

  const detection = detectCargo();

  if (detection.found) {
    if (!detection.onPath) {
      printFoundButNotOnPathNote(detection.path);
    } else {
      console.log("✓ cargo 확인됨");
    }
    process.exit(0);
  }

  printMissingInstructions();
  process.exit(mode === "warn" ? 0 : 1);
}

main();
