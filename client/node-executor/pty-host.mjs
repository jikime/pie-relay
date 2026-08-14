// pty-host.mjs — 터미널 방 호스트. node-pty 로 셸(zsh)을 진짜 PTY 에 띄우고,
// 프로토타입(prototypes/pty/host.mjs)과 달리 ws 가 아니라 **stdio NDJSON 프레임**으로
// Go 데몬(internal/ptyagent)과 통신한다. 한 줄 = 한 프레임.
//
//   stdin  ← Go 데몬:  {"type":"pty_input","from":"<sub>","data":"utf8"}
//                       {"type":"pty_scroll","from":"<sub>","data":"<esc seq>"} (드라이버 무관, 스크롤 전용)
//                       {"type":"pty_resize","from":"<sub>","cols":N,"rows":N}
//                       {"type":"set_driver","from":"<sub>","target":"<sub|''>"}
//   stdout → Go 데몬:  {"type":"room_mode","mode":"terminal"}   (시작 시 1회)
//                       {"type":"driver","from":"<드라이버 sub 또는 ''>"}
//                       {"type":"pty_size","cols":N,"rows":N}     (현재 PTY 격자 크기)
//                       {"type":"pty_output","data":"<base64>"}
//                       {"type":"pty_exit","code":N}
//
// 드라이버 게이트: pty_input/pty_resize 는 from === currentDriver 일 때만 pty 로 전달한다.
// set_driver 는 relay 가 role=host 만 통과시키므로(server.go routeFromParticipant),
// 여기까지 도달한 프레임은 신뢰하고 currentDriver 를 갱신한다.
//
// base64 는 Buffer 로 인코딩해 바이너리/UTF-8/이스케이프를 안전하게 보존한다.
import os from 'node:os';
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline';
import { createRequire } from 'node:module';
import { randomUUID } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import xtermHeadless from '@xterm/headless';
import xtermSerialize from '@xterm/addon-serialize';

// Desktop macOS normally has zsh, while the hardened Executor image ships
// bash. Prefer an explicit SHELL, then select an executable that actually
// exists instead of entering the supervisor's restart loop with ENOENT.
const SHELL = process.env.SHELL || (process.platform === 'win32'
  ? 'powershell.exe'
  : fs.existsSync('/bin/zsh') ? '/bin/zsh' : '/bin/bash');
const INIT_COLS = 120;
const INIT_ROWS = 34;
const OUTPUT_BATCH_MS = 8;
const OUTPUT_CHUNK_BYTES = 16 * 1024;
const STDOUT_QUEUE_BYTES = 16 * 1024 * 1024;

const log = (...a) => console.error('[pty-host]', ...a);

// --- 순수 로직 (테스트 대상) ------------------------------------------------

// node-pty 출력을 base64 로. node-pty 는 string(utf8) 을 주지만, Buffer 도 안전히 처리.
export function encodeOutput(data) {
  const buf = Buffer.isBuffer(data) ? data : Buffer.from(data, 'utf8');
  return buf.toString('base64');
}

// Batches PTY bytes into bounded 16KiB frames. Orca uses the same 8ms/16KiB
// balance: interactive echo remains effectively immediate while command
// floods no longer create one JSON/base64/WebSocket frame per node-pty event.
export class PtyOutputBatcher {
  constructor(emitChunk, delayMs = OUTPUT_BATCH_MS, chunkBytes = OUTPUT_CHUNK_BYTES) {
    this.emitChunk = emitChunk;
    this.delayMs = delayMs;
    this.chunkBytes = chunkBytes;
    this.parts = [];
    this.bytes = 0;
    this.timer = null;
  }

  push(data) {
    let buf = Buffer.isBuffer(data) ? data : Buffer.from(data, 'utf8');
    while (buf.length > 0) {
      const take = Math.min(this.chunkBytes - this.bytes, buf.length);
      this.parts.push(buf.subarray(0, take));
      this.bytes += take;
      buf = buf.subarray(take);
      if (this.bytes === this.chunkBytes) this.flush();
    }
    if (this.bytes > 0 && this.timer === null) {
      this.timer = setTimeout(() => this.flush(), this.delayMs);
    }
  }

  flush() {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.bytes === 0) return;
    const out = Buffer.concat(this.parts, this.bytes);
    this.parts = [];
    this.bytes = 0;
    this.emitChunk(out);
  }

  close() {
    this.flush();
  }
}

// process.stdout is a pipe to clientd. Respect its backpressure instead of
// letting Node retain unbounded NDJSON strings. Pausing node-pty at the high
// water mark propagates pressure to the child process/TTY; a hard byte cap is
// a final guard if a runtime lacks pause/resume.
class NDJSONWriter {
  constructor(onBlocked) {
    this.onBlocked = onBlocked;
    this.queue = [];
    this.bytes = 0;
    this.blocked = false;
  }

  write(obj) {
    const line = JSON.stringify(obj) + '\n';
    if (this.blocked || this.queue.length > 0) {
      this.queue.push(line);
      this.bytes += Buffer.byteLength(line);
      if (this.bytes > STDOUT_QUEUE_BYTES) {
        throw new Error('pty-host stdout queue overflow');
      }
      return;
    }
    if (!process.stdout.write(line)) this.setBlocked(true);
  }

  setBlocked(value) {
    if (this.blocked === value) return;
    this.blocked = value;
    this.onBlocked?.(value);
    if (value) process.stdout.once('drain', () => this.drain());
  }

  drain() {
    this.blocked = false;
    this.onBlocked?.(false);
    while (this.queue.length > 0) {
      const line = this.queue.shift();
      this.bytes -= Buffer.byteLength(line);
      if (!process.stdout.write(line)) {
        this.setBlocked(true);
        return;
      }
    }
  }
}

export function isScrollSequence(data) {
  if (typeof data !== 'string') return false;
  const sgr = /^\x1b\[<(\d+);\d+;\d+[Mm]$/.exec(data);
  if (sgr) return (Number(sgr[1]) & 0x40) !== 0;
  if (data.length === 6 && data.charCodeAt(0) === 0x1b && data[1] === '[' && data[2] === 'M') {
    return ((data.charCodeAt(3) - 32) & 0x40) !== 0;
  }
  return data === '\x1bOA' || data === '\x1bOB' || data === '\x1b[A' || data === '\x1b[B';
}

// 들어온 프레임과 현재 드라이버로부터 취할 동작을 결정한다(부수효과 없음).
// - 빈 문자열 드라이버('')는 "아무도 없음"(보기 전용) → 모든 입력 드롭.
// - pty_input/pty_resize 는 from === driver 이고 driver 가 비어있지 않을 때만 통과.
// - set_driver 는 도달 자체가 host 발신을 의미하므로 신뢰하고 target 채택.
export function routeFrame(frame, driver) {
  switch (frame && frame.type) {
    case 'pty_input':
      if (typeof frame.data === 'string' && driver !== '' && frame.from === driver) {
        return { action: 'write', data: frame.data };
      }
      return { action: 'drop', reason: 'not-driver' };
    case 'pty_scroll':
      // Scroll is a non-destructive, SHARED navigation action allowed for ANY
      // participant (not just the driver): write it straight to the PTY. The
      // client only ever sends wheel / alternate-scroll sequences under this
      // frame (clicks, keystrokes and resizes still go through the driver-gated
      // pty_input / pty_resize), so it can never be used to type, click, or
      // otherwise take control — only to move the shared view.
      if (isScrollSequence(frame.data)) {
        return { action: 'write', data: frame.data };
      }
      return { action: 'drop', reason: 'invalid-scroll' };
    case 'pty_resize':
      if (driver !== '' && frame.from === driver &&
          Number.isFinite(frame.cols) && Number.isFinite(frame.rows)) {
        return { action: 'resize', cols: frame.cols | 0, rows: frame.rows | 0 };
      }
      return { action: 'drop', reason: 'not-driver' };
    case 'set_driver':
      return { action: 'set_driver', target: typeof frame.target === 'string' ? frame.target : '' };
    case 'request_screen':
      // A (re)joining viewer asks for the current screen. Replay the scrollback
      // to that viewer ONLY (frame.from is the relay-verified requester), so it
      // does not duplicate onto everyone else. Anyone may request their own.
      if (typeof frame.from === 'string' && frame.from !== '') {
        return { action: 'replay', to: frame.from };
      }
      return { action: 'drop', reason: 'no-from' };
    default:
      return { action: 'ignore' };
  }
}

// PTY cwd: CLI_RELAY_DEFAULT_CWD 가 존재하는 디렉토리면 그걸, 아니면 홈.
export function resolveCwd(env = process.env) {
  const want = env.CLI_RELAY_DEFAULT_CWD;
  if (want) {
    try {
      if (fs.statSync(want).isDirectory()) return want;
    } catch { /* 존재하지 않으면 홈으로 폴백 */ }
  }
  return os.homedir();
}

// 셸에 UTF-8 로케일을 보장한다. macOS GUI 앱(런치드/파인더로 뜬 Tauri 사이드카)은
// 셸 로케일(LANG/LC_CTYPE)을 상속하지 못해 zsh 가 C/POSIX 로 뜨고, 그러면 한글 같은
// 전각(2칸) 문자의 폭을 1칸으로 오산해 ZLE(줄 편집기)에서 커서가 글자에 파고들어
// 겹친다. 현재 ctype 이 UTF-8 이 아니면 UTF-8 로케일을 강제한다(이미 UTF-8 이면 사용자
// 설정을 그대로 존중). LC_ALL 이 비-UTF-8 이면 다른 설정을 덮으므로 함께 교정한다.
export function ensureUtf8Env(env = process.env, want = 'en_US.UTF-8') {
  const out = { ...env };
  const ctype = out.LC_ALL || out.LC_CTYPE || out.LANG || '';
  if (/utf-?8/i.test(ctype)) return out; // 이미 UTF-8
  if (out.LC_ALL && !/utf-?8/i.test(out.LC_ALL)) delete out.LC_ALL; // 비-UTF-8 LC_ALL 제거
  if (!out.LANG || !/utf-?8/i.test(out.LANG)) out.LANG = want;
  out.LC_CTYPE = want;
  return out;
}

// --- node-pty prebuild spawn-helper 실행비트 방어 ---------------------------
// 프로토타입에서 발견: node-pty 의 prebuild spawn-helper 가 실행권한을 잃으면
// pty.spawn 이 EACCES 로 실패한다. 존재하면 chmod +x 를 시도하고, 실패해도 계속한다.
// spawn-helper 는 POSIX(macOS/Linux) 전용 보조 바이너리다 — Windows 빌드의
// node-pty 는 conpty/winpty 를 쓰고 이 파일 자체가 없으므로, Windows 에서는 이
// 파일 권한 로직 전체를 건너뛴다(무해하지만 의미 없는 chmod 를 피함).
function ensureSpawnHelperExecutable() {
  if (process.platform === 'win32') return;
  try {
    const require = createRequire(import.meta.url);
    const pkgPath = require.resolve('node-pty/package.json');
    const base = path.dirname(pkgPath);
    const candidates = [
      path.join(base, 'prebuilds', `${process.platform}-${process.arch}`, 'spawn-helper'),
      path.join(base, 'build', 'Release', 'spawn-helper'),
    ];
    for (const c of candidates) {
      if (fs.existsSync(c)) {
        try { fs.chmodSync(c, 0o755); } catch (e) { log('spawn-helper chmod 실패(무시):', e.message); }
      }
    }
  } catch (e) {
    log('spawn-helper 경로 확인 실패(무시):', e.message);
  }
}

// --- 실제 호스트 실행 -------------------------------------------------------
async function main() {
  ensureSpawnHelperExecutable();
  const pty = (await import('node-pty')).default;

  const cwd = resolveCwd();
  const term = pty.spawn(SHELL, [], {
    name: 'xterm-256color',
    cols: INIT_COLS,
    rows: INIT_ROWS,
    cwd,
    env: { ...ensureUtf8Env(process.env), TERM: 'xterm-256color' },
  });
  const { Terminal: HeadlessTerminal } = xtermHeadless;
  const { SerializeAddon } = xtermSerialize;
  const headless = new HeadlessTerminal({ cols: INIT_COLS, rows: INIT_ROWS, scrollback: 5000 });
  const serializer = new SerializeAddon();
  headless.loadAddon(serializer);
  let snapshotWriteChain = Promise.resolve();
  const incarnationId = randomUUID();
  const streamId = process.env.CLI_RELAY_STREAM_ID || 'terminal';
  let outputSeq = 0;
  log(`PTY spawned: ${SHELL} pid=${term.pid} (${INIT_COLS}x${INIT_ROWS}) cwd=${cwd}`);

  // 기본 드라이버: 없음(보기 전용). CLI_RELAY_INITIAL_DRIVER 로 초기 드라이버 지정 가능.
  let driver = process.env.CLI_RELAY_INITIAL_DRIVER || '';

  // 현재 PTY 격자 크기. 드라이버의 resize 로만 바뀐다. 비드라이버 뷰어는 자기 컨테이너
  // 폭이 아니라 이 크기에 xterm 을 맞춰(letterbox) 렌더해야 pre-wrap 된 바이트가 올바른
  // 열에서 줄바꿈된다 — 그래서 이 크기를 브로드캐스트한다.
  let curCols = INIT_COLS;
  let curRows = INIT_ROWS;

  const writer = new NDJSONWriter((blocked) => {
    if (blocked && typeof term.pause === 'function') term.pause();
    if (!blocked && typeof term.resume === 'function') term.resume();
  });
  const emit = (obj) => writer.write(obj);

  // 시작 공지: GUI 가 xterm 모드로 전환하도록 room_mode, 그리고 현재 드라이버.
  // 릴레이는 무상태라 과거 프레임을 재전송하지 않는다 — 데몬이 뜬 뒤 나중에 접속한
  // 참가자는 이 시작 공지를 놓친다. 그래서 room_mode/driver/pty_size 를 하트비트로 주기
  // 재방출해 늦게 들어온 참가자도 몇 초 안에 자기 방이 터미널 방임을(그리고 현재 드라이버·
  // 격자 크기를) 알게 한다. 프레임이 작아 비용은 무시할 만하다(pty_output 스트림 대비).
  const announce = () => {
    emit({ type: 'room_mode', mode: 'terminal' });
    emit({ type: 'driver', from: driver });
    emit({ type: 'pty_size', cols: curCols, rows: curRows });
  };
  announce();
  const heartbeat = setInterval(announce, 2000);

  const batcher = new PtyOutputBatcher((buf) => {
    outputSeq += 1;
    emit({ type: 'pty_output', streamId, incarnationId, seq: outputSeq, data: buf.toString('base64') });
  });
  term.onData((data) => {
    const buf = Buffer.isBuffer(data) ? data : Buffer.from(data, 'utf8');
    // Serialize writes so a snapshot always represents an exact sequence
    // boundary even while output is arriving rapidly.
    snapshotWriteChain = snapshotWriteChain.then(() => new Promise((resolve) => {
      headless.write(data, resolve);
    }));
    batcher.push(buf);
  });

  term.onExit(({ exitCode }) => {
    const code = exitCode ?? 0;
    log(`PTY exited code=${code}`);
    clearInterval(heartbeat);
    batcher.close();
    emit({ type: 'pty_exit', code });
    // exit 프레임이 flush 될 짧은 여유 후 정상 종료.
    setTimeout(() => process.exit(code), 50);
  });

  // stdin NDJSON: 한 줄 = 한 프레임.
  const rl = readline.createInterface({ input: process.stdin });
  rl.on('line', async (raw) => {
    const line = raw.trim();
    if (!line) return;
    let frame;
    try {
      frame = JSON.parse(line);
    } catch {
      return; // 잘못된 프레임 무시
    }
    const r = routeFrame(frame, driver);
    switch (r.action) {
      case 'write':
        term.write(r.data);
        break;
      case 'resize':
        try {
          const cols = Math.max(1, r.cols);
          const rows = Math.max(1, r.rows);
          term.resize(cols, rows);
          headless.resize(cols, rows);
          // 리사이즈가 적용됐으니 현재 크기를 갱신하고 즉시 브로드캐스트한다. 비드라이버
          // 뷰어는 이 크기에 맞춰 letterbox 렌더하므로 재랩(re-wrap) 깨짐을 피한다.
          curCols = cols;
          curRows = rows;
          emit({ type: 'pty_size', cols: curCols, rows: curRows });
        } catch (e) {
          log('resize error:', e.message);
        }
        break;
      case 'replay':
        // Flush the output batch first, then wait for the headless emulator to
        // consume every byte. The serialized snapshot is authoritative terminal
        // state and can never start halfway through UTF-8 or an ANSI sequence.
        batcher.flush();
        await snapshotWriteChain;
        emit({
          type: 'pty_snapshot', to: r.to, streamId, incarnationId, seq: outputSeq,
          cols: curCols, rows: curRows,
          data: Buffer.from(serializer.serialize(), 'utf8').toString('base64'),
        });
        break;
      case 'set_driver':
        driver = r.target;
        emit({ type: 'driver', from: driver });
        break;
      // drop/ignore: 아무것도 하지 않음
    }
  });

  // stdin 이 닫히면(데몬 clientd 가 죽음) 셸을 정리하고 이 프로세스도 종료한다.
  // 하트비트 setInterval 이 이벤트 루프를 살려두므로 명시적으로 종료하지 않으면
  // 부모가 죽어도 pty-host+셸이 고아로 남는다.
  rl.on('close', () => {
    clearInterval(heartbeat);
    batcher.close();
    try { term.kill(); } catch { /* 이미 죽었으면 무시 */ }
    setTimeout(() => process.exit(0), 50);
  });

  process.on('SIGINT', () => {
    try { term.kill(); } catch { /* 무시 */ }
    process.exit(0);
  });
}

// 직접 실행될 때만 PTY 를 스폰한다. 테스트가 순수 로직만 import 할 때는 실행 안 함.
const isMain = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (isMain) {
  main().catch((e) => {
    log('fatal:', e.stack || e.message);
    process.exit(1);
  });
}
