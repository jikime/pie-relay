# Pie Relay Desktop

Phase 1의 터미널 완결형 rooms(`../server/docs/rooms-design.md`)를 잇는 GUI 클라이언트.
**Tauri v2(Rust 셸) + React/Vite/TypeScript 프런트** 구성이며, 이 저장소에는 설계 문서
`DESIGN-P2.md`의 **P2-1 참가자 모드**, **P2-2 호스트 모드(Go 사이드카)**, **P2-3 패키징**
(아이콘·executor 번들 동봉·.app/.dmg 배포물)이 구현되어 있다.

앱은 상단 탭으로 두 모드를 오간다.

- **참가자 모드** — 사이드카가 필요 없다. 웹뷰가 릴레이에 직접 접속한다. 도메인
  로직(이벤트 파싱·채팅 상태)은 순수 TypeScript 모듈(`src/protocol.ts`)에 있다.
- **호스트 모드** — 기존 Go `client` 바이너리를 **사이드카(clientd)**로 동봉해 GUI에서
  방 만들기(발급 키로 호스트 토큰 발급) / 데몬 시작·정지 / 초대 코드 발급을 수행한다.
  프로세스 관리·출력 스트리밍은 Rust 커스텀 커맨드(`src-tauri/src/lib.rs`)가 담당한다.

## 요구 사항

빌드 머신:

- **Node.js 20+** / npm (프런트 빌드 + 호스트 executor 스테이징 — executor는 Node ≥20을 요구)
- Rust stable (1.77.2+) + Cargo
  (`npm install` 시 자동 감지되어 없으면 경고하고, `npm run tauri dev`/`build` 전에는
  누락 시 곧바로 실패하며 설치 안내를 출력한다. macOS/Linux는 `npm run setup:rust`로
  rustup 자동 설치를 시도할 수 있다.)
- **Go 1.25+** (호스트 모드 사이드카 빌드용 — `scripts/build-sidecar.mjs`)
- Python 3 + Pillow (아이콘 재생성 시에만 — `scripts/generate-icon.py`)
- macOS: Xcode Command Line Tools / Linux: WebKitGTK 등 Tauri 시스템 의존성
  (https://tauri.app/start/prerequisites/ 참고)
- Windows: **MSVC C++ Build Tools + Windows SDK**와 WebView2(Win10/11 대부분 기본 설치).
  Rust 기본 `x86_64-pc-windows-msvc` 툴체인이 `link.exe`로 링크하므로 반드시 필요하다.
  **워크로드를 포함해** 설치해야 한다(껍데기만 깔면 `link.exe not found`로 실패):
  ```powershell
  winget install --id Microsoft.VisualStudio.2022.BuildTools -e --override "--quiet --wait --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended"
  ```
  (이미 BuildTools가 있으면 Visual Studio Installer에서 '수정' → 'C++를 사용한 데스크톱 개발'
  체크. 설치 후 새 터미널을 열 것.) 빌드 스크립트(`scripts/*.mjs`)는 Node로 작성되어 bash 없이
  Windows에서도 동작하며, 사이드카 파일명에 `.exe`를 자동으로 붙인다.

실행 머신(배포된 앱을 쓰는 사용자):

- **호스트 모드를 쓰려면 Node.js 20+ 가 PATH에 있어야 한다.** 앱은 `executor.mjs`와
  node_modules(네이티브 `claude` 포함)를 번들에 동봉하지만, 그 executor를 구동하는
  `node` 런타임 자체는 동봉하지 않는다. 참가자 모드만 쓴다면 Node가 필요 없다.

## 설치

```bash
npm install
```

`npm install` 후 자동으로 Rust/Cargo 설치 여부를 점검한다(`postinstall` →
`scripts/ensure-rust.mjs --warn`). 없으면 경고만 출력하고 설치 자체는 계속 진행된다.
`npm run tauri dev`/`npm run tauri build`는 실행 전에 같은 점검을 다시 하는데(`pretauri`),
이때는 Rust가 없으면 곧바로 실패한다(`cargo metadata` 단계까지 가지 않고 명확한 안내를
먼저 보여준다). Rust가 없다는 안내가 나오면 macOS/Linux는 `npm run setup:rust`
(rustup 자동 설치 시도) 또는 위 rustup 원라이너를 실행한 뒤 **셸을 재시작**하고 다시
시도한다.

## 사이드카 빌드 (호스트 모드)

호스트 모드는 `../client`(Go)를 Tauri 사이드카로 동봉한다. 앱을 dev/build 하기 전에 한 번
빌드해 둔다.

```bash
npm run sidecar        # ../client 를 src-tauri/binaries/clientd-<target-triple> 로 빌드
```

`scripts/build-sidecar.mjs`는 `rustc -Vv`의 host 타깃 트리플을 파일명에 붙여
(`clientd-aarch64-apple-darwin` 등) `tauri.conf.json > bundle > externalBin`의
`"binaries/clientd"` 규약에 맞춘다. 산출물(`src-tauri/binaries/`)은 `.gitignore` 대상이라
커밋되지 않으므로, 새 체크아웃/CI에서는 이 스크립트를 먼저 실행해야 한다.

## 개발 실행

프런트 개발 서버 + Tauri 창을 함께 띄운다. 둘 중 하나를 사용한다.

```bash
npm run tauri dev          # 번들된 @tauri-apps/cli 사용 (권장)
# 또는 cargo-tauri를 전역 설치했다면:
cargo tauri dev
```

프런트만 브라우저에서 보려면:

```bash
npm run dev                # http://localhost:1420
```

## 빌드

프런트만:

```bash
npm run build              # tsc 타입체크 + vite 프로덕션 번들 (dist/)
```

배포용 데스크톱 앱(.app/.dmg) 전체 파이프라인은 아래 **패키징**을 따른다.

## 패키징 (배포물 만들기)

번들에는 두 종류의 네이티브 산출물이 함께 들어간다. 둘 다 `.gitignore` 대상이라
새 체크아웃/CI에서는 `tauri build` 전에 반드시 생성해야 한다.

1. **사이드카(clientd)** — 호스트 모드용 Go client 바이너리.
2. **executor 리소스** — 호스트 데몬이 구동하는 `executor.mjs` + node_modules.
   여기에는 `@anthropic-ai/claude-agent-sdk`의 **네이티브 `claude` 바이너리**
   (`node_modules/@anthropic-ai/claude-agent-sdk-darwin-arm64/claude`, 약 220MB)가
   포함된다. 이 바이너리가 빠지면 채팅이 전부 실패하므로,
   `scripts/prepare-executor.mjs`가 스테이징 후 존재를 단언(assert)한다.

```bash
npm run sidecar            # ../client → src-tauri/binaries/clientd-<triple>
npm run prepare-executor   # ../client/node-executor → src-tauri/resources/node-executor
                           #   (executor.mjs/package*.json 복사 + npm ci --omit=dev 로 재설치)
npm run tauri build        # .app / .dmg 번들 (release, 수 분 소요)
```

산출물:

```
src-tauri/target/release/bundle/macos/Pie Relay.app
src-tauri/target/release/bundle/dmg/Pie Relay_0.1.0_aarch64.dmg
```

`prepare-executor.mjs`는 node_modules를 **복사하지 않고** 스테이징 디렉터리에서
`npm ci --omit=dev`로 재설치한다(dev 트리 오염 방지, 재현 가능). `--omit=dev`는
devDependencies만 제외하고 플랫폼 네이티브 `claude`가 실려오는 optionalDependencies는
유지한다. 산출물(`src-tauri/resources/`)은 200MB+ 라 커밋하지 않는다.

번들에 실제로 동봉되는 경로(검증됨):

```
Pie Relay.app/Contents/MacOS/clientd                                  # 사이드카
Pie Relay.app/Contents/Resources/resources/node-executor/executor.mjs
Pie Relay.app/Contents/Resources/resources/node-executor/node_modules/
        @anthropic-ai/claude-agent-sdk-darwin-arm64/claude            # 네이티브 CLI(arm64)
```

Rust는 `EXECUTOR_PATH` 기본값을 3단 폴백으로 해석한다(`host_suggest_executor_path`):
(a) 패키징 앱이면 번들 리소스의 `resources/node-executor/executor.mjs`,
(b) 개발 체크아웃이면 형제 `../client/node-executor/executor.mjs`,
(c) 둘 다 없으면 빈 값(사용자에게 직접 입력 요구). 명시적 `EXECUTOR_PATH` env가 있으면 최우선.

### 아이콘 재생성

아이콘은 `scripts/generate-icon.py`가 코드로 그리는 1024px PNG(어두운 배경 +
릴레이 허브·두 피어 노드)를 소스로 쓴다. 바꾸려면:

```bash
npm run icon               # generate-icon.py → app-icon.png → tauri icon 세트 재생성
```

### 코드 서명 / 첫 실행

이 파이프라인은 **미서명 앱**을 만든다(서명·공증은 범위 외). 미서명 `.app`을 처음 열 때
Gatekeeper가 막으면, Finder에서 앱을 **우클릭 → 열기**로 한 번 허용하거나
`xattr -dr com.apple.quarantine "Pie Relay.app"`로 격리 속성을 제거한다.

## 테스트

프로토콜 파서·리듀서 단위 테스트(소켓 없이 순수 함수 검증):

```bash
npm test                   # vitest run
npm run test:watch
```

## 검증 명령 (완료 기준)

```bash
npm run sidecar            # ../client → src-tauri/binaries/clientd-<triple>
npm run build              # tsc + vite
(cd src-tauri && cargo check)
npx vitest run
```

## 프로젝트 구조

```
desktop/
├─ index.html
├─ package.json / vite.config.ts / tsconfig*.json
├─ app-icon.png                 # 아이콘 소스(1024px). generate-icon.py 산출물. `npm run icon`으로 재생성
├─ scripts/
│  ├─ build-sidecar.mjs          # ../client → src-tauri/binaries/clientd-<triple> (npm run sidecar)
│  ├─ prepare-executor.mjs       # ../client/node-executor → src-tauri/resources/node-executor (npm run prepare-executor)
│  └─ generate-icon.py          # 코드로 app-icon.png 생성 (npm run icon 이 tauri icon 과 함께 호출)
├─ src/
│  ├─ main.tsx                  # React 진입점
│  ├─ App.tsx                   # 참가자 ↔ 호스트 탭 + 방 타입별 채팅/터미널 라우팅(외부 상태 라이브러리 없음)
│  ├─ ConnectScreen.tsx         # (참가자) 릴레이 주소·초대 코드·이름 입력, localStorage 기억
│  ├─ ChatScreen.tsx            # (참가자) 채팅 방: 상단 바·전사·입력창, 소켓↔리듀서 배선 + room_mode 감지 핸드오프
│  ├─ TerminalScreen.tsx        # (참가자) 터미널 방: xterm.js 뷰 + 드라이버 게이트 + 오퍼레이터 조작권 제어
│  ├─ HostPanel.tsx             # (호스트) 방 만들기/데몬/초대 발급 UI + 방 타입 선택 + 사이드카 로그 뷰
│  ├─ rooms.ts / rooms.test.ts  # ★ 멀티룸 순수 코어(방 목록·활성·안읽음·방 타입 분류) (테스트 대상)
│  ├─ protocol.ts               # ★ 순수 파서 + 채팅 리듀서 + 터미널 파서/빌더/드라이버 게이트 (테스트 대상)
│  ├─ protocol.test.ts          # vitest 단위 테스트
│  ├─ sidecar.ts                # ★ 사이드카 출력 파서(초대 코드) + 로그 링버퍼 (테스트 대상)
│  ├─ sidecar.test.ts           # vitest 단위 테스트
│  ├─ relay.ts                  # join/createInvite/enrollHost(fetch POST) + ws/http URL 헬퍼
│  ├─ connection.ts             # 지수 백오프 재접속 WebSocket
│  └─ styles.css
└─ src-tauri/
   ├─ Cargo.toml / build.rs     # tauri-plugin-shell v2 추가
   ├─ tauri.conf.json           # 식별자·제품명·CSP + bundle.externalBin(clientd) + resources(node-executor) + macOS 메타
   ├─ resources/                # (gitignore) prepare-executor.mjs 산출물: node-executor 스테이징(네이티브 claude 포함)
   ├─ capabilities/default.json # core:default + shell:allow-execute(사이드차 스코프)·kill
   ├─ binaries/                 # (gitignore) build-sidecar.mjs 산출물
   ├─ icons/                    # tauri icon 산출물
   └─ src/{main.rs, lib.rs}     # 호스트 커맨드(데몬/초대/토큰) + 사이드카 이벤트 스트리밍
```

## 와이어 계약 (릴레이와의 인터페이스)

`src/protocol.ts`/`src/relay.ts`가 Phase 1 확정 계약을 그대로 따른다
(레퍼런스: `../client/internal/tui/`, `../server/internal/relay/server.go`).

- **참가(HTTP)**: `POST {base}/rooms/join` 바디 `{"code","name"}` → `{"token","room"}`.
  `base`는 릴레이 ws 주소를 http로 바꾼 값(ws→http, wss→https). 401 = 코드 만료/오타.
- **참가자 WebSocket**: `{ws base}/ws/participant`.
  브라우저 WebSocket은 Authorization 헤더를 못 보내므로
  **`Sec-WebSocket-Protocol: pie-relay.ticket.<JWT>`**로 인증한다. JWT가 URL/access log에 남지 않는다.
- **수신 이벤트(JSON 한 줄)**: `session_id` `text`(스트리밍 델타) `thinking` `done`
  `error` `aborted` `peer_chat{from,text}` `host:status{connected}`
  (`agent:status`는 동일 의미 중복 → 멱등 처리) `agent:unavailable{reason}`.
- **발신**: `{"type":"chat","prompt":"…","sessionId":"<있으면>"}`.
  `from`은 절대 넣지 않는다(릴레이가 검증된 신원을 주입). `sessionId`는 첫 `session_id`
  또는 `done`의 비어있지 않은 값에서 캡처해 이후 발신에 유지(대화 연속성).
- **터미널 방(Phase 5) 프레임**(채팅 리듀서와 별개, `parseTerminalEvent`/빌더):
  - 수신: `room_mode{mode:"terminal"}`(접속 시 1회, 방 타입 확정) ·
    `pty_output{data:<base64>}`(PTY 출력, 바이너리 안전) · `pty_exit{code}` ·
    `driver{from:<드라이버 sub 또는 "">}`(현재 조작자 공지).
  - 발신: `pty_input{data:<utf8>}`(키 입력, **내가 드라이버일 때만**) ·
    `pty_resize{cols,rows}`(fit 후, 드라이버일 때만, 디바운스) ·
    `set_driver{target:<sub>}`(role=host 오퍼레이터만 유효, 빈 값=회수).
    `set_driver`는 상호운용을 위해 `target`과 `driver` 두 키에 같은 값을 함께 싣는다
    (릴레이는 `type`만 보고 통과시키고, pty-host가 어느 키를 읽든 동작).

## 화면 동작

- **접속 화면**: 릴레이 주소(기본 CookAI Relay, 로컬은
  `PIE_RELAY_URL=http://127.0.0.1:13412`)·초대 코드·이름 입력 → [참가].
  실패 시 인라인 에러, 최근 접속값(토큰 제외)은 localStorage에 저장. **"다른 기기: 호스트
  토큰 붙여넣기"** 를 켜면 초대 코드 입력란이 호스트 토큰(JWT) 입력란으로 바뀌어, 다른 기기의
  호스트 토큰을 붙여넣어 그 토큰의 방에 호스트로 참가한다(토큰은 저장하지 않음). 같은 PC에서
  방금 띄운 방은 호스트 탭의 **[이 방 열기]** 를 쓴다.
- **방 이름·내 이름**: 방을 만든 뒤에도 호스트 패널의 방 이름/내 이름 입력란에서 언제든
  바꿀 수 있다. 이는 **표시 별칭(로컬)** 이라 릴레이 방 ID·토큰·데몬은 그대로 두고 내 화면의
  상단 바·사이드바 라벨만 실시간으로 갱신한다.
- **채팅 화면**: 상단 바에 방 이름·내 이름·호스트 상태(●연결/○끊김). 내 발신은 우측,
  peer_chat은 `guest:<name>-<rand>`에서 이름만 라벨, Claude 응답은 델타 실시간 누적 후
  `done`에서 확정, thinking은 접힌 회색 한 줄(토글). 입력창은 Enter 전송/Shift+Enter 줄바꿈,
  응답 중에도 전송 가능(그룹 대화).
- **재접속**: ws가 끊기면 배너를 띄우고 지수 백오프(1s→30s)로 자동 재접속한다.

## 멀티 룸 (사이드바)

여러 방(호스트)에 **동시 접속**해두고 왼쪽 사이드바로 전환한다. 핵심 불변식은 **연결
유지**다 — 참가 중인 모든 방의 채팅 화면을 항상 마운트해두고 비활성 방은 CSS(`display:none`)
로만 숨긴다. 탭 전환은 언마운트가 아니므로 소켓·대화·세션 ID가 배경에서 그대로 살아 있다.
소켓은 **방을 닫을 때(×/나가기)만** 닫힌다.

- **사이드바 항목**: `●/○`(호스트 연결 상태) + 방 식별자(room 클레임, 예 `r-dev`) + 내가
  호스트인 방은 `호스트` 태그 + 안읽음 점(비활성 방에 새 활동이 오면 표시, 클릭해 열면 사라짐)
  - `×`(방 닫기). 소켓이 죽은 방(예: 재시작 후 토큰 만료)은 `끊김` 태그로 흐리게 표시된다 —
    닫고 다시 참가하면 된다.
- **`+ 방 추가`**: 오른쪽에 접속 화면을 띄운다(초대 코드 또는 "다른 기기: 호스트 토큰
  붙여넣기" 토글). 방이 하나 이상 있으면 접속 화면에 **`취소`** 버튼이 생겨 직전 방으로 돌아간다.
- **참가자 / 호스트**: 사이드바 하단에서 전환. `호스트`는 기존 호스트 패널(데몬 로그·초대
  발급)을 방 목록과 공존하는 별도 뷰로 보여준다.
- **재시작 복원**: 방 목록은 localStorage에 저장되어 재시작 시 복원을 시도한다(만료 토큰은
  위처럼 `끊김`으로 표시).
- 같은 릴레이·방·이름·역할로 다시 참가하면 새 소켓을 열지 않고 **기존 방으로 전환**한다.

### 수동 검증 시나리오 (멀티 룸)

GUI 실행은 사용자 몫이며(코드 검증은 아래 "검증 명령"), 아래는 손으로 확인하는 흐름이다.

1. 릴레이와 호스트를 띄우고 초대 코드 두 개(방 A, 방 B)를 발급한다.
2. `+ 방 추가`로 방 A에 참가 → 사이드바에 방 A가 뜨고 오른쪽이 방 A 채팅으로 전환된다.
3. 다시 `+ 방 추가`로 방 B에 참가 → 방 B가 추가·활성화된다. 방 A는 사이드바에 그대로 남아
   **연결이 유지**된다(●).
4. 방 B에서 대화하는 동안 방 A에 새 메시지/도구 활동이 오면 방 A 항목에 **안읽음 점**이
   뜬다. 방 A를 클릭하면 그동안의 대화가 **끊김 없이** 남아 있고 점이 사라진다.
5. 방 A의 `×`로 닫으면 그 소켓만 닫히고 방 B는 계속 살아 있다. 마지막 방을 닫으면 접속
   화면으로 돌아간다.
6. 앱을 재시작하면 방 목록이 복원된다(토큰이 살아 있으면 재연결, 만료면 `끊김`).

## 터미널 방 (Phase 5)

**채팅 방**(SDK Claude)과 **공존**하는 새 방 타입. 호스트 PC의 셸(zsh)을 PTY로 띄우고,
참가자가 xterm.js 터미널로 그 셸을 직접 조작한다. 릴레이는 무변경에 가깝다 —
`pty_output`은 기존 host→participant 팬아웃, `pty_input`/`pty_resize`/`set_driver`는
participant→host 경로를 그대로 타며, `set_driver`만 host-only 게이트에 추가된다(S5-1).

### 호스트로 터미널 방 열기

1. 사이드바 하단 **호스트** → 방 만들기/티켓 준비(채팅 방과 동일).
2. **방 타입**을 `터미널 (셸 직접 조작)`으로 선택. 이때 데몬은 `CLI_RELAY_ROOM_MODE=terminal`
   env로 시작되어(`src-tauri/src/lib.rs host_daemon_start`) SDK 챗 실행기 대신 PTY 호스트를
   감독한다. **작업 디렉토리**가 PTY의 시작 경로(`CLI_RELAY_DEFAULT_CWD`)가 된다. 권한 모드는
   터미널 방에선 의미가 없어 비활성화된다(채팅 전용).
3. **데몬 시작** → **초대 코드 발급**. 방 타입 선택은 localStorage에 유지된다.

### 참가자 화면

- 방이 `room_mode:"terminal"`을 받으면 채팅 UI 대신 **xterm.js 터미널 뷰**로 전환된다.
  분류 전(초기 `unknown`)에는 채팅 화면으로 마운트되어 있다가 `room_mode` 수신 시
  터미널 화면으로 스왑되며(이 재마운트가 유일한 의도된 재연결), 방 타입은 저장되어 재시작 시
  바로 터미널로 뜬다. 스크롤백 복원은 이 Phase 범위 밖이라 스왑 직전 출력은 유실될 수 있다.
- 상단 바: 방 이름·내 이름·**현재 조작자**(⌨ 내가/상대 조작 중 · 👁 보기 전용)·**내 ID**(복사)·
  호스트 연결(●/○). 세션 ID 칩은 터미널 방엔 없다.
- **입력 처리**: 키 입력은 xterm이 아니라 우리 소유의 `textarea` 오버레이(`.term-input`)로
  받는다. Tauri WKWebView에서 xterm의 숨은 textarea엔 IME 조합이 안 걸려 한글이 자모로
  쪼개지므로, 조합이 정상 동작하는 일반 textarea로 모든 입력(한글 등 IME 포함)을 받아 PTY로
  보낸다. 제어키는 순수 함수 `keyToSequence`로 터미널 시퀀스에 매핑하고, 일반 문자·IME
  조합은 `input`/`compositionend`가 처리한다.
- **터미널 IME는 커서 위치 인라인 조합**: 오버레이는 전체 덮개가 아니라 커서 셀 위에
  얹힌 작은 입력창이다. 측정된 셀 크기(`.xterm-screen`)와 커서 좌표(`term.buffer.active`)로
  픽셀 위치를 순수 함수 `cursorPx`로 계산해(`.term-host` 패딩 6/8 오프셋 반영), `onCursorMove`·
  fit/resize마다 갱신한다. 텍스트 색이 터미널 전경색이라 조합 중인 한글이 커서 자리에 실시간
  미리보기로 뜨고 IME 후보창도 커서 옆에 열린다. 확정 시 value를 비워 미리보기는 사라지고
  셸 에코가 그 자리에 남는다(스크롤백으로 커서가 화면 밖이면 좌상단으로 안전 폴백).
- **터미널 폰트**: 호스트 PTY는 폰트 정보를 전달하지 않으므로 화면 폰트는 참가자 xterm이
  정한다. 한글·영문이 모두 고정폭인 **D2Coding**(SIL OFL 1.1)을 앱에 번들해(`src/fonts/`,
  Vite가 `dist/`로 함께 패키징) 기본 폰트로 쓰므로 한글·ASCII 열이 어긋나지 않는다. 상단바의
  **"Aa"** 버튼으로 폰트(D2Coding 외 JetBrains Mono·Menlo·SF Mono·Sarasa Term K·Consolas —
  번들 안 된 폰트는 시스템에 설치된 경우에만 적용)와 크기(11~16)를 고른다. 선택값은
  `localStorage`(`cli-relay.term-font`)에 저장되고 앱의 모든 터미널 방이 공유하며, 변경 즉시
  xterm 옵션 교체 후 재fit으로 그리드·커서를 다시 계산한다. 폰트·크기 파싱과 기본값은 순수
  함수 `loadTermFont`(`src/termfont.ts`)로 뽑아 테스트한다. 재배포 고지를 위해
  `src/fonts/LICENSE-D2Coding.txt`를 동봉한다.
- **조작권(driver)**: 내가 드라이버일 때만 키 입력이 `pty_input`으로 전송된다. 아니면 키를
  **로컬에서 막고**(서버 왕복 낭비 방지) "보기 전용 — 조작권 없음" 힌트를 잠깐 띄운다.
  게이트는 순수 함수 `shouldSendInput(driver, mySub)`로 뽑아 테스트한다(호스트의 `from===driver`
  게이트와 동일 규칙).

### 조작권 제어 (호스트 오퍼레이터 = role=host 참가자)

터미널 방에 **호스트로 참가**하면 상단에 **조작권 제어** 스트립이 뜬다: `나에게 조작권` /
`회수(보기 전용)` / 참가자 ID 붙여넣기 후 `조작권 주기` / 최근 관찰된 드라이버 빠른 버튼.

> **참가자 명단 한계**: 릴레이에 **참가자 로스터 브로드캐스트가 없고**, 게스트 sub는
> `guest:<name>-<rand4>`의 추측 불가한 접미사를 갖는다. 따라서 오퍼레이터가 참가자 sub를
> 자동으로 나열할 수 없다. 대신 각 참가자 화면의 **"내 ID"** 칩(복사)을 오퍼레이터에게
> 알려주면 붙여넣어 조작권을 준다. `driver` 프레임으로 관찰된 sub는 빠른 버튼으로 제공한다.
> (이 한계는 `TerminalScreen.tsx`의 `DriverControls` 주석에도 명시.)

### 멀티룸 불변식 (터미널 방 동일)

터미널 방도 방 목록의 한 항목(사이드바 아이콘 `❯_`로 채팅 `💬`과 구분)이며 **연결 유지**
불변식을 그대로 따른다 — 비활성 터미널 방도 소켓과 xterm 인스턴스를 **마운트한 채**
`display:none`으로만 숨긴다(`hidden` prop은 소켓/터미널을 절대 닫지 않는다). 방을 닫을 때만
소켓이 닫히고 터미널이 dispose된다. 숨겨진 방으로 되돌아오면 fit을 다시 수행해 크기를 맞춘다.

## 세션 이어가기 (터미널)

채팅 상단 바의 **`세션 xxxxxxxx…` 칩**을 누르면 현재 Claude 세션 ID와 복사 버튼
(ID / 명령)이 나온다. 릴레이 세션은 호스트의 작업 디렉토리(`CLI_RELAY_DEFAULT_CWD`,
없으면 홈)의 `~/.claude/projects/<경로>/<세션ID>.jsonl` 에 **정상 저장**되며, 터미널에서

```bash
claude --resume <세션ID>
```

로 그대로 이어서 대화할 수 있다(검증됨).

주의 — **인터랙티브 `claude --resume` 목록(피커)에는 이 세션들이 뜨지 않는다.** SDK가
헤드리스 세션을 `entrypoint=sdk-cli`로 태깅하고, 이 값은 외부 env·SDK `options.env`
어느 쪽으로도 바꿀 수 없다(SDK가 스트리밍 모드에서 강제). 즉 "저장이 안 되는" 게 아니라
"피커 목록에 표시되지 않을 뿐"이며, 위처럼 **ID로 직접 resume 하면 정상 동작**한다. 그래서
세션 칩으로 ID를 복사해 터미널로 넘기는 방식을 쓴다.

## 수동 스모크 테스트

로컬에서 릴레이 + 호스트 데몬을 띄우고 GUI로 참가하는 최소 절차. 터미널 4개를 쓴다.
(아래 명령은 Phase 1 e2e 스모크에서 실제 사용·검증된 형태다.)

1. **릴레이 서버** — 시크릿을 정하고, 개발용 호스트 토큰을 먼저 발급(-mint는 발급 후
   즉시 종료된다)한 뒤 서버를 기동한다.

   ```bash
   cd ../server
   export RELAY_JWT_SECRET=dev-secret-0123456789abcdef0123456789abcdef
   go run ./cmd/relay -mint alice -mint-room r-dev -mint-role host   # → 호스트 JWT 출력
   go run ./cmd/relay -addr 127.0.0.1:13412
   ```

   첫 명령이 출력한 호스트 JWT를 복사해 둔다.

2. **호스트 데몬(client)** — 위 호스트 토큰으로 릴레이에 접속하는 데몬을 띄운다.
   이 데몬이 있어야 채팅 화면 상단이 `●호스트 연결`로 바뀌고 Claude 응답이 온다.

   ```bash
   cd ../client
   RELAY_TICKET=<호스트JWT> PIE_RELAY_URL=ws://127.0.0.1:13412/ws/agent go run ./cmd/client
   ```

3. **초대 코드 발급(room create/invite)** — 호스트 토큰으로 초대 코드를 만든다.

   ```bash
   curl -s -X POST http://127.0.0.1:13412/rooms/invites \
     -H "Authorization: Bearer <호스트토큰>" | jq .
   # → {"code":"ABCD2345","expiresAt":...}
   ```

4. **GUI 참가** — 데스크톱 앱을 띄우고 접속 화면에 값을 넣는다.

   ```bash
   npm run tauri dev
   ```

   - 릴레이 주소: `ws://127.0.0.1:13412`
   - 초대 코드: 3단계에서 받은 코드
   - 이름: 예) `bob` → [참가]

   확인 항목:
   - 참가 성공 시 채팅 화면 진입, 상단에 방 이름·`나: bob`·`●호스트 연결` 표시.
   - 메시지 입력 후 Enter → 내 말풍선(우측) 표시, Claude 응답이 실시간 누적되다가 확정.
   - 다른 참가자를 하나 더 붙이면(같은 코드로 GUI 재실행 또는 TUI) 서로의 질문이
     peer 라벨로 보인다.
   - 호스트 데몬을 잠깐 내리면 상단이 `○호스트 끊김`으로, 릴레이를 내렸다 올리면 배너가
     뜨고 자동 재접속된다.

## 호스트 모드 사용법

상단 **호스트** 탭에서 내 컴퓨터의 Claude CLI를 릴레이에 연결한다. 먼저 사이드카를
빌드(`npm run sidecar`)해야 하며, 반드시 데스크톱 앱(`npm run tauri dev`)에서 실행한다
(브라우저 `npm run dev`에서는 사이드카 커맨드가 없어 안내 문구만 표시된다).

흐름은 **릴레이 운영자가 `HOST_ENROLL_SECRET` 설정 → 호스트가 방 만들기 →
데몬 시작 → 이 방 참여 → 초대 코드 발급**이다(설계: `../server/docs/host-enroll-design.md`).
외부 브라우저 로그인(vibe-canvas)은 더 이상 쓰지 않는다.

패널은 **주 흐름을 위→아래 선형으로** 배치하고, 드물게 쓰는 항목은 접이식 **고급 설정**
(기본 접힘)으로 숨긴다. 상단 배지가 **수동 티켓 있음 / 티켓 없음(방 만들기 필요)** 을
보여준다. 카드는 **방 → 초대** 순이며 그 아래 **고급 설정**과 **사이드카 로그**가 접혀 있다.
접힘/펼침 상태는 localStorage(`cli-relay.host.ui`)에 유지된다.

1. **방 만들기** — 호스트 토큰이 없으면 **방** 카드에 큰 [방 만들기] 버튼이 뜬다. 로컬
   릴레이(127.0.0.1/localhost/::1)는 서버가 로컬 전용 옵션
   `ALLOW_LOOPBACK_ENROLL_WITHOUT_SECRET=true`로 실행된 경우 발급 키 없이 바로 누르면 되고,
   공개 릴레이/이름 지정은
   **고급 설정**에서 발급 키(`HOST_ENROLL_SECRET`)·방 이름·내 이름을 채운다. [방 만들기]는
   웹뷰가 릴레이 `POST /host/enroll`을 호출해 호스트 토큰을 발급받는다. 받은 토큰은 수동
   티켓(localStorage)으로 저장되고, 토큰이 있으면 버튼 대신 **방: `r-xxxx` · 만료 …** 상태가
   표시된다. **발급 키는 저장하지 않는다**(state에만 유지, 성공 후 입력란 비움).
   - **로컬 무키 등록은 명시적 옵션이다** — 서버를
     `ALLOW_LOOPBACK_ENROLL_WITHOUT_SECRET=true`로 실행한 직접 로컬 환경에서만 키를 비울 수
     있다. 리버스 프록시 뒤에서는 이 옵션을 켜지 않는다.
   - **공개 릴레이**면 운영자가 정한 키를 입력한다. 키 없이 시도해 401/403이면
     "이 릴레이는 발급 키가 필요합니다" 안내가, 틀린 키는 "발급 키가 올바르지 않습니다",
     릴레이가 발급을 비활성화(키 미설정)했으면 503 안내가 인라인으로 뜬다.
   - 릴레이 주소·수동 티켓 붙여넣기(다른 소스의 `-mint` 토큰용)도 **고급 설정**에 있다.
2. **방 타입 / 데몬 시작·정지** — **방 카드**에서 방 타입(채팅/터미널)을 고르고(데몬 정지
   시에만 변경) [데몬 시작]으로 사이드카 `client` 데몬을 띄운다. 릴레이 주소(PIE_RELAY_URL),
   수동 티켓(RELAY_TICKET), EXECUTOR_PATH를 env로 넘긴다. 데몬 stdout/stderr는 최근 200줄
   링버퍼로 **사이드카 로그**(접이식)에 스트리밍되고, 카드 안 인디케이터가 실행 상태를
   표시한다. 권한 모드(방 정책)·작업 디렉토리는 **고급 설정**에 있다.
3. **이 방 참여(모니터링·승인)** — 데몬 실행 중이면 **방 카드**의 이 버튼으로 방금 띄운 그
   방에 **호스트로** 참가해 승인 카드·드라이버 제어·화면을 본다(아래 5번과 동일 경로).
4. **초대 코드 발급** — **초대 카드**에서 **초대 등급(관전/조작)** 을 고른 뒤 [초대 코드
   발급]으로 코드를 만들어 크게 표시하고, 발급된 코드 옆에 등급 배지(관전/조작)를 붙인다.
   - **관전(view)**: 이 코드로 온 참가자는 보기 전용(입력 드롭). **조작(control)**: 입력 가능,
     터미널 방이면 릴레이가 그 참가자를 **자동 드라이버**로 지정한다(set_driver 브로드캐스트).
   - 기본값은 안전하게 **관전**이다. 등급을 조작으로 올릴지는 호스트가 명시적으로 정한다.
   - 수동 티켓 모드(방 만들기로 발급받은 토큰): `client room create`가 티켓 override와
     등급 전달을 지원하지 않으므로, 웹뷰가 직접 `POST /rooms/invites`(Bearer=티켓, body에
     `access:"view"|"control"`)를 호출한다. **등급 선택은 이 경로에서만 적용**된다.
   - 티켓이 없으면(폴백) 사이드카 `client room create` 출력을 파싱한다. 이 경로는 등급을
     실을 수 없어 릴레이의 안전한 기본값 **관전(view)** 으로 생성되며, 등급 선택 UI는 비활성화된다.
5. **이 방 참여(모니터링·승인)** — 데몬 실행 중이면 **방 카드**의 이 버튼으로 방금 띄운 그
   방에 **호스트로** 바로 참가한다. 호스트 토큰(수동 티켓 우선 → 없으면 저장된
   `host_access_token`)의 `room` 클레임을 디코드해 그 방을 참가자 목록에 추가하고 참가자
   모드로 전환한다. 같은 방이 이미 열려 있으면 새 소켓을 열지 않고 그 방으로 전환한다.
   다른 기기에서 붙일 호스트 토큰은 **고급 설정**의 [다른 기기용 토큰 복사]로 얻는다.

### 호스트가 자기 방을 여는 두 경로

- **같은 PC** (데몬을 이 앱에서 띄운 경우): 위 5번 — 호스트 탭 **방 카드**의 **[이 방 참여]**.
  데몬의 토큰·방을 그대로 쓰므로 어느 방인지 모호하지 않다. (권장)
- **다른 기기** (데몬은 다른 컴퓨터에 있고 그 방에 호스트 자격으로 붙고 싶은 경우): 접속
  화면에서 **"다른 기기: 호스트 토큰 붙여넣기"** 를 켜고 호스트 토큰(JWT)을 직접 붙여넣는다.
  토큰의 `room` 을 디코드해 그 방에 호스트로 참가한다. 이 경로의 토큰은 자격증명이므로
  localStorage에 저장하지 않는다.

  그 붙여넣을 토큰은 **데몬을 띄운 PC의 호스트 탭 고급 설정에서 [다른 기기용 토큰 복사]** 로
  얻는다 (데몬 실행 중일 때 활성). 이 버튼은 데몬이 쓰는 토큰(수동 티켓 우선 → 없으면
  `host_access_token`)을 클립보드에 담아, 다른 기기의 붙여넣기 란과 짝을 이룬다. 토큰의
  출처는 호스트 탭의 **방 만들기**(릴레이 `POST /host/enroll`) 또는 릴레이 `-mint`(개발용)이다.

**EXECUTOR_PATH**: 데몬은 `executor.mjs` 경로가 필요하다. GUI가 필드로 받으며, 기본값은
Rust가 3단 폴백으로 채운다 — 패키징 앱이면 번들에 동봉된 executor, 개발 체크아웃이면
형제 `../client/node-executor/executor.mjs`(위 **패키징** 절 참고). 실행 머신에는
executor를 구동할 **Node ≥20**이 PATH에 있어야 한다.

## 보안

- **CSP `connect-src` 는 의도적으로 http/https/ws/wss 를 모두 허용한다.** 이 앱은 사용자가
  접속 화면에서 **임의의 릴레이 주소**를 입력해 접속하는 것이 핵심 제품 요구사항이다
  (기본 Azure 또는 `PIE_RELAY_URL`로 지정한 local/custom Relay). 접속 대상이 열려 있어야
  하므로 `connect-src`를 고정 호스트 목록으로 조이지 않는다. 나머지 지시자는 좁게 유지한다 —
  `default-src 'self'`, `script-src 'self'`(외부/인라인 스크립트 불가), `img-src 'self' data:`.
  이 근거를 tauri.conf.json에는 주석을 달 수 없어(JSON) 여기에 남긴다.
- **미서명 배포.** 코드 서명·공증은 범위 외다. 첫 실행 우회는 위 **코드 서명 / 첫 실행** 참고.
- 호스트 모드의 프로세스 spawn/kill·출력 스트리밍은 전부 Rust 커스텀 커맨드에서 한다.
  프런트는 shell 플러그인을 직접 호출하지 않으므로 capability의 `shell:allow-execute`
  스코프는 방어적 선언이다.

## 설계 메모 / 남은 작업

- **P2-3 완료.** 아이콘(코드 생성), executor 번들 동봉(네이티브 `claude` 포함), 메타데이터
  정리(버전 0.1.0·category·macOS 최소 10.15), 릴리스 빌드(.app/.dmg) 검증까지 마쳤다.
- executor 번들은 현재 **호스트 아키텍처(darwin-arm64)** 것만 스테이징한다
  (`npm ci`가 실행 플랫폼의 optional 바이너리만 설치). Intel(x64)·Linux·Windows 배포물은
  각 타깃 머신/러너에서 `prepare-executor`를 돌려 빌드해야 한다. 사이드카(clientd)도 동일하게
  타깃별 빌드가 필요하다. 크로스 배포용 CI 매트릭스는 후속 작업으로 남긴다.
- 번들 용량은 executor의 네이티브 `claude`(약 220MB) 때문에 크다(.app ≈ 245MB, .dmg ≈ 74MB).
  참가자 전용 경량 빌드를 원하면 executor 리소스를 뺀 별도 타깃을 두는 방안이 있다(미착수).
- 코드 서명·공증(notarization)은 배포 자동화 시 도입 대상이다.
