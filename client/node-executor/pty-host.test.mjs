// pty-host.test.mjs — 순수 로직 단위 테스트 + 실제 스폰 헤드리스 스모크.
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import readline from 'node:readline';

import os from 'node:os';

import { encodeOutput, isScrollSequence, routeFrame, resolveCwd, ensureUtf8Env, PtyOutputBatcher } from './pty-host.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const HOST = path.join(HERE, 'pty-host.mjs');

// --- 순수 로직 -------------------------------------------------------------

test('encodeOutput: base64 round-trips utf8 string', () => {
  const s = 'héllo 안녕 \x1b[0m\n';
  const b64 = encodeOutput(s);
  assert.equal(Buffer.from(b64, 'base64').toString('utf8'), s);
});

test('encodeOutput: base64 is binary-safe for Buffers', () => {
  const buf = Buffer.from([0x00, 0xff, 0x1b, 0x5b, 0x41]);
  const b64 = encodeOutput(buf);
  assert.deepEqual(Buffer.from(b64, 'base64'), buf);
});

test('PtyOutputBatcher coalesces small writes and caps chunks at 16KiB', async () => {
  const chunks = [];
  const batcher = new PtyOutputBatcher((chunk) => chunks.push(chunk), 5, 16 * 1024);
  batcher.push(Buffer.from('hello'));
  batcher.push(Buffer.from(' world'));
  await new Promise((resolve) => setTimeout(resolve, 15));
  assert.equal(chunks.length, 1);
  assert.equal(chunks[0].toString(), 'hello world');

  batcher.push(Buffer.alloc(16 * 1024 + 3, 7));
  batcher.close();
  assert.equal(chunks[1].length, 16 * 1024);
  assert.equal(chunks[2].length, 3);
});

test('routeFrame: driver input passes, non-driver input drops', () => {
  const driver = 'alice';
  assert.deepEqual(
    routeFrame({ type: 'pty_input', from: 'alice', data: 'ls\n' }, driver),
    { action: 'write', data: 'ls\n' },
  );
  assert.deepEqual(
    routeFrame({ type: 'pty_input', from: 'bob', data: 'rm -rf\n' }, driver),
    { action: 'drop', reason: 'not-driver' },
  );
});

test('routeFrame: empty driver ("" = view-only) drops everyone, even from ""', () => {
  assert.equal(routeFrame({ type: 'pty_input', from: '', data: 'x' }, '').action, 'drop');
  assert.equal(routeFrame({ type: 'pty_input', from: 'alice', data: 'x' }, '').action, 'drop');
});

test('routeFrame: resize gated to driver only', () => {
  assert.deepEqual(
    routeFrame({ type: 'pty_resize', from: 'alice', cols: 100, rows: 40 }, 'alice'),
    { action: 'resize', cols: 100, rows: 40 },
  );
  assert.equal(
    routeFrame({ type: 'pty_resize', from: 'bob', cols: 100, rows: 40 }, 'alice').action,
    'drop',
  );
});

test('routeFrame: pty_scroll writes to the PTY for ANYONE (not driver-gated)', () => {
  // A non-driver participant scrolling the shared view.
  assert.deepEqual(
    routeFrame({ type: 'pty_scroll', from: 'bob', data: '\x1b[<65;1;1M' }, 'alice'),
    { action: 'write', data: '\x1b[<65;1;1M' },
  );
  // Even with no driver assigned at all, scroll still applies.
  assert.deepEqual(
    routeFrame({ type: 'pty_scroll', from: 'eve', data: '\x1b[<64;1;1M' }, ''),
    { action: 'write', data: '\x1b[<64;1;1M' },
  );
  // Empty/absent data and a valid prefix with injected keystrokes are dropped.
  assert.equal(routeFrame({ type: 'pty_scroll', from: 'bob', data: '' }, 'alice').action, 'drop');
  assert.equal(
    routeFrame({ type: 'pty_scroll', from: 'bob', data: '\x1b[<64;1;1Mwhoami\n' }, 'alice').action,
    'drop',
  );
});

test('isScrollSequence: exact wheel/alternate-scroll only', () => {
  assert.equal(isScrollSequence('\x1b[<64;10;5M'), true);
  assert.equal(isScrollSequence('\x1b[A'), true);
  assert.equal(isScrollSequence('\x1b[<0;10;5M'), false);
  assert.equal(isScrollSequence('\x1b[<64;10;5Mprintf hacked\\n'), false);
  assert.equal(isScrollSequence('printf hacked\\n'), false);
});

test('routeFrame: set_driver adopts target (trusted — relay gates host-only)', () => {
  assert.deepEqual(
    routeFrame({ type: 'set_driver', from: 'host', target: 'bob' }, 'alice'),
    { action: 'set_driver', target: 'bob' },
  );
  // target 없으면 빈 문자열(드라이버 회수 = 보기 전용)
  assert.deepEqual(
    routeFrame({ type: 'set_driver', from: 'host' }, 'alice'),
    { action: 'set_driver', target: '' },
  );
});

test('routeFrame: unknown types ignored', () => {
  assert.equal(routeFrame({ type: 'chat', prompt: 'hi' }, 'alice').action, 'ignore');
  assert.equal(routeFrame(null, 'alice').action, 'ignore');
});

test('resolveCwd: nonexistent CLI_RELAY_DEFAULT_CWD falls back to homedir', () => {
  assert.equal(resolveCwd({ CLI_RELAY_DEFAULT_CWD: '/no/such/dir/xyz-123' }), os.homedir());
});

test('resolveCwd: existing directory is honored', () => {
  assert.equal(resolveCwd({ CLI_RELAY_DEFAULT_CWD: os.tmpdir() }), os.tmpdir());
});

// --- 실제 스폰 스모크 -------------------------------------------------------
// pty-host.mjs 를 진짜 스폰해 stdin 으로 프레임을 보내고 stdout 프레임을 관찰한다.
// 드라이버로 지정된 alice 의 입력은 셸에서 실행되어 출력에 나타나야 하고,
// 비드라이버 bob 의 입력은 pty 에 도달하지 못해 흔적조차 없어야 한다.
test('spawn smoke: driver input reaches shell, non-driver input is blocked', { timeout: 15000 }, async () => {
  const ALICE = 'PTY_ALICE_' + Math.floor(Math.random() * 1e6);
  const BOB = 'PTY_BOB_' + Math.floor(Math.random() * 1e6);

  const child = spawn('node', [HOST], {
    stdio: ['pipe', 'pipe', 'inherit'],
    env: { ...process.env, CLI_RELAY_INITIAL_DRIVER: '' },
  });

  let acc = '';
  let sawRoomMode = false;
  const drivers = [];
  const ptySizes = [];
  let snapshotRequested = false;

  const rl = readline.createInterface({ input: child.stdout });
  const send = (obj) => child.stdin.write(JSON.stringify(obj) + '\n');

  const result = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => resolve({ ok: false, note: 'timeout' }), 12000);
    let inputsSent = false;

    rl.on('line', (line) => {
      let msg;
      try { msg = JSON.parse(line); } catch { return; }

      if (msg.type === 'room_mode') sawRoomMode = msg.mode === 'terminal';
      if (msg.type === 'driver') drivers.push(msg.from);
      if (msg.type === 'pty_size') ptySizes.push({ cols: msg.cols, rows: msg.rows });
      if (msg.type === 'pty_output') {
        acc += Buffer.from(msg.data, 'base64').toString('utf8');
        // alice 의 echo 는 명령 에코 + 실행 결과로 2회 이상 나타난다.
        const aliceCount = acc.split(ALICE).length - 1;
        if (aliceCount >= 2 && !snapshotRequested) {
          snapshotRequested = true;
          send({ type: 'request_screen', from: 'viewer-1' });
        }
      }
      if (msg.type === 'pty_snapshot' && msg.to === 'viewer-1') {
        const snapshot = Buffer.from(msg.data, 'base64').toString('utf8');
        clearTimeout(timer);
        resolve({
          ok: snapshot.includes(ALICE),
          snapshotSeq: msg.seq,
          incarnationId: msg.incarnationId,
        });
      }

      // room_mode/driver 공지를 받은 뒤, 셸 프롬프트가 준비될 시간을 주고 입력 전송.
      if (!inputsSent && drivers.length > 0) {
        inputsSent = true;
        setTimeout(() => {
          // host 가 alice 에게 조작권 부여.
          send({ type: 'set_driver', from: 'host', target: 'alice' });
          // 드라이버 alice 의 리사이즈(적용되어 pty_size 로 재방출되어야 함).
          send({ type: 'pty_resize', from: 'alice', cols: 100, rows: 40 });
          // 비드라이버 bob 의 입력(차단되어야 함).
          send({ type: 'pty_input', from: 'bob', data: `echo ${BOB}\n` });
          // 드라이버 alice 의 입력(통과해야 함).
          send({ type: 'pty_input', from: 'alice', data: `echo ${ALICE}\n` });
        }, 800);
      }
    });

    child.on('error', reject);
  });

  child.kill('SIGINT');

  assert.ok(sawRoomMode, 'room_mode:terminal 공지를 받아야 한다');
  assert.equal(drivers[0], '', '초기 드라이버는 빈 문자열(보기 전용)이어야 한다');
  assert.ok(result.ok, `드라이버 입력이 셸에 도달해 ${ALICE} 출력이 관찰되어야 한다`);
  assert.ok(Number.isSafeInteger(result.snapshotSeq), '스냅샷은 출력 sequence 경계를 포함해야 한다');
  assert.ok(typeof result.incarnationId === 'string' && result.incarnationId.length > 0, '스냅샷은 PTY incarnation id를 포함해야 한다');
  assert.equal(acc.split(BOB).length - 1, 0, `비드라이버 입력 ${BOB} 은 pty 에 도달하지 않아야 한다`);
  // set_driver 후 driver 공지에 alice 가 방출되었는지.
  assert.ok(drivers.includes('alice'), 'set_driver 후 driver:alice 가 방출되어야 한다');
  // pty_size: 시작 시 초기 격자(120x34)가 방출되고, alice 의 resize 후 100x40 이 재방출되어야 한다.
  assert.ok(
    ptySizes.some((s) => s.cols === 120 && s.rows === 34),
    '시작 시 초기 pty_size 120x34 가 방출되어야 한다',
  );
  assert.ok(
    ptySizes.some((s) => s.cols === 100 && s.rows === 40),
    'driver resize 후 pty_size 100x40 이 재방출되어야 한다',
  );
});

test('ensureUtf8Env forces UTF-8 when env has no locale (GUI-launched sidecar)', () => {
  const out = ensureUtf8Env({ PATH: '/usr/bin' })
  assert.match(out.LC_CTYPE, /UTF-8/i)
  assert.match(out.LANG, /UTF-8/i)
})

test('ensureUtf8Env respects an existing UTF-8 locale', () => {
  const out = ensureUtf8Env({ LANG: 'ko_KR.UTF-8' })
  assert.equal(out.LANG, 'ko_KR.UTF-8')
  assert.equal(out.LC_CTYPE, undefined) // untouched
})

test('ensureUtf8Env drops a non-UTF-8 LC_ALL override', () => {
  const out = ensureUtf8Env({ LC_ALL: 'C', LANG: 'en_US.UTF-8' })
  assert.equal(out.LC_ALL, undefined)
})
