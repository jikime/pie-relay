# Pie Relay 연결 구조와 사용 흐름

이 문서는 Pie Relay의 구성요소가 서로 어떤 관계를 가지며, 실제 연결이 어떤 순서로
이루어지는지를 설명한다. 설치 화면과 버튼별 설명을 포함한 최종 사용자 매뉴얼을 만들 때
기초 자료로 사용하는 것을 목적으로 한다.

> 이 문서는 현재 저장소에 구현된 동작을 기준으로 한다. 예시의 도메인, 포트, 토큰과
> 경로는 배포 환경에 맞게 바꿔야 한다.

실행 환경(Host OS/Docker), 접속 경로(LAN/Relay), 세션 발급 방식(단독
Relay/Control Plane)의 차이는 [세션·실행 환경·Control Plane
정의](./session-runtime-and-control-plane.md)를 먼저 참고한다.

클라이언트의 기본 Relay는 Azure staging이다. 로컬로 전환할 때는 별도 프로필 없이
`PIE_RELAY_URL` 값만 로컬 주소로 바꾼다. URL 우선순위와 Desktop/Mobile Gateway
동작은 [Relay URL 설정](./relay-url-configuration.md)을 참고한다.

## 1. 핵심 개념

Pie Relay에는 서로 구분되는 두 연결 계통이 있다.

1. **원격 방 연결**: 다른 컴퓨터에서 실행 중인 `clientd`의 Claude Code 또는 PTY를
   Desktop 앱에서 보고 조작한다.
2. **모바일 연결**: 모바일 앱이 페어링된 PC의 Desktop Host Gateway에 연결해 그 PC의
   터미널을 보고 조작한다.

두 계통은 같은 Pie Relay 서버 프로세스를 사용할 수 있지만, 연결 대상과 프로토콜이
다르다. 원격 방에 접속한 모바일 앱이 그 방의 임의 `clientd`를 자동으로 선택하는 구조는
아니다.

### 사용 목적별 필수 프로세스

| 사용 목적 | 반드시 실행되어야 하는 구성요소 | 없어도 되는 구성요소 |
|---|---|---|
| 원격 `clientd` 터미널을 Desktop에서 보기 | Relay 서버, 대상 장비의 `clientd`/PTY Host, 보는 쪽 Desktop 앱 | 대상 장비의 Desktop 앱, 모바일 앱 |
| 원격 `clientd`의 Claude Code와 대화 | Relay 서버, 대상 장비의 `clientd`/Node Executor/Claude Code, 참가 UI | 대상 장비의 Desktop 앱, 모바일 Gateway |
| 같은 Wi-Fi에서 모바일로 PC 제어 | 대상 PC의 Desktop Host Gateway, 모바일 앱 | Relay 서버, `clientd` |
| 외부 네트워크에서 모바일로 PC 제어 | Relay 서버, 대상 PC의 Desktop Host Gateway, 모바일 앱 | 일반 방용 `clientd` |

원격 방은 동일한 `room` claim을 가진 호스트와 참가자만 연결한다. 한 방의 활성
`clientd` 호스트 슬롯은 하나이며, 같은 방에 두 호스트가 연결되면 나중에 등록된 호스트가
현재 호스트가 된다.

## 2. 전체 구성요소

| 구성요소 | 실행 위치 | 역할 |
|---|---|---|
| Pie Relay 서버 | 인터넷 또는 사설망의 서버 | 인증, 방 연결, 초대 코드, WebSocket 및 모바일 E2EE 데이터 중계 |
| `clientd` | Claude Code/터미널을 제공할 PC 또는 Linux 서버 | Relay에 호스트로 접속하고 로컬 Executor 또는 PTY를 실행 |
| Node Executor | `clientd`가 실행되는 장비 | Claude Code SDK/CLI 실행 및 이벤트 변환 |
| PTY Host | `clientd`가 실행되는 장비 | 로컬 셸을 PTY로 실행하고 입력·출력을 중계 |
| Pie Relay Desktop | 사용자의 데스크톱 | 방 참가자 UI, 방 호스트 관리 UI, 모바일 Gateway 관리 UI |
| Desktop Host Gateway | Desktop 앱이 실행하는 로컬 프로세스 | 모바일 앱에 로컬 터미널을 제공하고 모바일 E2EE 세션을 종료 |
| Pie Relay 모바일 앱 | iOS/Android 기기 | QR 페어링 후 Desktop Host Gateway의 터미널을 표시·조작 |
| Executor Manager | 별도 서버/호스트 | 사용자별 Docker Executor와 작업 수명주기 관리. 기본 Relay 연결과는 독립적 |

### 2.1 Pie Relay 서버

Relay 서버는 중앙 연결 지점이다. 서버에서 Claude Code나 사용자의 셸을 직접 실행하지
않고, 인증된 연결 사이에서 메시지를 전달한다.

현재 한 Relay 서버 프로세스가 다음 엔드포인트를 제공한다.

| 엔드포인트 | 용도 |
|---|---|
| `/ws/agent` | `clientd` 호스트 연결 |
| `/ws/participant` | Desktop 또는 CLI 참가자 연결 |
| `/ws/browser` | `/ws/participant`의 호환용 별칭 |
| `/host/enroll` | 호스트 방과 호스트 JWT 발급 |
| `/rooms/invites` | 호스트가 참가 초대 코드 발급 |
| `/rooms/join` | 초대 코드를 참가자 JWT로 교환 |
| `/v1/assign`, `/v1/resolve` | 모바일 Relay 할당·해결 |
| `/v1/host/control` | 모바일 Desktop Gateway의 제어 채널 |
| `/v1/host/data/*` | 모바일 Desktop Gateway의 데이터 채널 |
| `/v1/connect/*` | 모바일 앱 연결 |
| `/v1/control/connections/{id}` | Manager가 특정 Relay 연결을 강제 종료하는 내부 API |
| `/v1/control/driver` | Manager가 실제 Relay driver를 인계·회수하는 내부 API |
| `/healthz`, `/readyz` | 운영 상태 확인 |

`/v1/control/*`는 일반 사용자용 공개 API가 아니다. `PIE_RELAY_CONTROL_TOKEN`으로
Manager만 인증하며, 외부 Traefik router에 별도로 노출하지 않는다. Relay가 Manager로
보내는 presence에는 사용자가 접속할 공개 URL과 Manager가 운영 명령에 사용할 내부
control URL이 구분되어 기록된다.

Relay 서버는 일반적으로 Linux 서버에서 `systemd`, Docker 또는 다른 프로세스 관리자를
사용해 상시 데몬으로 운영한다. 운영 환경에서는 TLS를 서버에서 직접 설정하거나
nginx/Caddy/Traefik 같은 리버스 프록시에서 HTTPS/WSS를 종단한다.

### 2.2 `clientd`

`clientd`는 단순 화면 클라이언트가 아니라 **로컬 실행 호스트 데몬**이다.

- 호스트 JWT를 `Authorization: Bearer`로 보내 `/ws/agent`에 접속한다.
- 채팅 모드에서는 `node-executor/executor.mjs`를 실행한다.
- 터미널 모드에서는 `node-executor/pty-host.mjs`를 실행한다.
- Relay에서 받은 입력을 로컬 프로세스에 전달한다.
- Claude 응답, 권한 요청, 상태 이벤트 또는 PTY 출력을 Relay로 되돌려 보낸다.
- Relay 연결이 일시적으로 끊기면 백오프로 재접속한다.
- Executor 또는 PTY Host가 일시적으로 종료되면 제한된 정책 안에서 재시작한다.

따라서 실제 파일 접근과 명령 실행은 Relay 서버가 아니라 `clientd`가 설치된 장비에서
발생한다. PTY가 가지는 OS 권한도 `clientd`를 실행한 사용자 계정의 권한과 같다.

### 2.3 Desktop 앱

Desktop 앱은 한 가지 역할만 수행하지 않는다.

- **참가자 모드**: 초대 코드로 다른 호스트의 방에 참가한다.
- **호스트 모드**: 호스트 토큰을 발급하고, 번들된 `clientd`를 사이드카로 실행한다.
- **모바일 모드**: Desktop Host Gateway를 실행하고 모바일용 QR을 표시한다.

상대방의 `clientd` 터미널을 보는 주 UI는 Desktop 앱이다. Desktop 앱의 호스트 기능과
모바일 Gateway 기능은 별도 프로세스와 별도 수명주기를 가진다.

### 2.4 모바일 앱과 Desktop Host Gateway

모바일 앱이 PC를 제어하려면 대상 PC에서 Desktop Host Gateway가 실행 중이어야 한다.
Relay 서버만 실행해서는 모바일이 PC 터미널을 제어할 수 없다.

Desktop Host Gateway는 다음 작업을 담당한다.

- 모바일 페어링 QR 생성
- 기기별 device token과 공개키 관리
- LAN WebSocket endpoint 제공
- Relay 모드에서 모바일 Relay control/data 채널 등록
- E2EE 인증과 암호화 세션 종단
- 대상 PC의 터미널 목록, PTY 입력과 출력 제공
- 등록된 모바일 기기 조회와 권한 폐기

현재는 Desktop 앱이 Gateway의 시작과 종료를 관리한다. Gateway를 독립 서비스로 분리하면
향후 Desktop UI 없이도 모바일 호스트를 운영할 수 있지만, 현재 일반 사용 흐름은 Desktop
앱에서 `Pie Relay 모바일` 화면을 여는 방식이다.

## 3. 원격 방 연결: Linux A를 Desktop B에서 제어

다음은 GUI가 없는 Linux 서버 A에서 `clientd`만 실행하고, 사용자 B가 Desktop 앱으로
A의 터미널을 보는 대표 구성이다.

```text
┌──────────────────────────────┐
│ Linux 서버 A                 │
│ clientd                      │
│   └─ pty-host.mjs ── zsh/bash│
└──────────────┬───────────────┘
               │ /ws/agent + host JWT
               ▼
        ┌───────────────┐
        │ Pie Relay     │
        │ room registry │
        └───────┬───────┘
                │ /ws/participant + participant JWT
                ▼
┌──────────────────────────────┐
│ 사용자 B의 Desktop 앱       │
│ xterm 화면 + 입력/권한 제어  │
└──────────────────────────────┘
```

### 3.1 연결 순서

1. Relay 운영자가 Relay 서버에 `RELAY_JWT_SECRET`과 `HOST_ENROLL_SECRET`을 설정한다.
2. A 또는 관리자가 `/host/enroll`을 호출해 방 ID와 호스트 JWT를 발급받는다.
3. A에서 호스트 JWT를 `RELAY_TICKET`으로 주입해 `clientd`를 실행한다.
4. `clientd`가 Relay의 `/ws/agent`에 접속해 해당 방의 호스트가 된다.
5. 호스트 JWT로 `/rooms/invites`를 호출해 `view` 또는 `control` 초대 코드를 발급한다.
6. B가 Desktop 앱에 초대 코드를 입력한다.
7. Desktop 앱이 `/rooms/join`으로 참가자 JWT를 받은 뒤 `/ws/participant`에 접속한다.
8. A의 PTY 출력이 `clientd → Relay → Desktop` 경로로 전달된다.
9. B가 조작 권한을 가지고 있으면 입력이 `Desktop → Relay → clientd → PTY`로 전달된다.

### 3.2 A에는 Desktop GUI가 필요하지 않다

A가 헤드리스 Linux 서버여도 다음 런타임만 갖추면 된다.

- `clientd` 바이너리
- Node.js 20 이상
- `node-executor` 파일과 의존성
- 터미널 방인 경우 `node-pty`를 사용하는 `pty-host.mjs`
- 채팅 방인 경우 로컬 Claude Code CLI 설치 및 인증
- Relay에 도달할 수 있는 네트워크
- 유효한 호스트 JWT

터미널 방은 다음과 같은 환경으로 실행한다.

```bash
export PIE_RELAY_URL='wss://relay.cookai.dev/ws/agent'
export RELAY_TICKET='<host-jwt>'
export CLI_RELAY_ROOM_MODE='terminal'
export PTY_HOST_PATH='/opt/pie-relay/node-executor/pty-host.mjs'

/usr/local/bin/clientd
```

채팅/Claude Code 방은 `CLI_RELAY_ROOM_MODE=terminal`을 제거하고 Executor 경로를 지정한다.

```bash
export PIE_RELAY_URL='wss://relay.cookai.dev/ws/agent'
export RELAY_TICKET='<host-jwt>'
export EXECUTOR_PATH='/opt/pie-relay/node-executor/executor.mjs'

/usr/local/bin/clientd
```

표준 ACP 어댑터를 사용하는 Claude Code 방은 다음처럼 분리한다. Relay 메시지 형식은
기존 채팅과 같아서 Desktop·Mobile 클라이언트를 동시에 업그레이드하지 않아도 된다.

```bash
export PIE_RELAY_URL='wss://relay.cookai.dev/ws/agent'
export RELAY_TICKET='<host-jwt>'
export CLI_RELAY_ROOM_MODE='acp'
export ACP_EXECUTOR_PATH='/opt/pie-relay/node-executor/acp-executor.mjs'
export PIE_ACP_AGENT_COMMAND='/opt/pie-relay/node-executor/node_modules/.bin/claude-agent-acp'

/usr/local/bin/clientd
```

Control Plane 세션에서는 `agentMode=acp`, capability의 `protocol=acp`를 함께 사용한다.
SDK 기반 기존 세션의 `agentMode=chat`은 계속 지원되며 자동 변환하지 않는다.

### 3.3 Control Plane에서 대상 장치를 골라 새 세션 만들기

PAT 기반 운영 환경에서는 호스트 JWT를 사람이 복사하지 않아도 된다.

1. 터미널을 제공할 Host OS에서 `pie-client start`를 실행해 장치를 등록한다.
   현재 PC를 선택하면 Desktop 앱이 이 agent를 자동으로 실행한다.
2. Desktop의 `새 연결 → 내 장치 · 공유받은 장치`에서 PAT로 장치 목록을 불러온다.
3. `새 작업 세션`을 선택한다.
4. `이 PC · Host OS`, 다른 `장치명 · Host OS`, `장치명 · Docker` 중 실제 실행 대상을
   선택한다.
5. Control Plane이 세션을 `starting`으로 기록한다.
6. Host OS agent 또는 Executor Manager가 해당 세션의 scoped Host JWT를 받아 PTY를
   시작한다.
7. Relay에 Host가 등록되어 세션이 `active`가 되면 Desktop이 자동으로 접속한다.

서비스별 Relay Pool을 사용할 때 Desktop 빌드에는
`VITE_PIE_RELAY_APPLICATION_ID`, `VITE_PIE_RELAY_POOL_ID`를 함께 설정한다.
Tenant를 비워 두면 인증된 organization 또는 사용자 ID를 사용한다. 관리형 연결 UI는
JWT payload를 직접 해석하지 않고 Control Plane이 반환한 `room`, `relayUrl`,
`executionTarget`을 정본으로 사용한다. 따라서 Pool이 선택한 노드 주소가 기존 기본 Relay
주소와 달라도 Host와 Participant가 같은 노드로 연결된다.

Host OS agent는 Control Plane을 outbound polling하므로 대상 서버의 Session Manager
포트를 외부에 열지 않는다. Docker 세션은 Executor Manager가 관리하며, 두 실행 환경은
같은 Relay 전송 경로를 사용하더라도 별개의 대상이다.

### 3.4 헤드리스 환경에서 호스트 토큰 발급 예시

운영 Relay에서 호스트 등록이 활성화되어 있어야 한다.

```bash
curl --fail-with-body \
  -H 'Content-Type: application/json' \
  -d '{"secret":"<host-enroll-secret>","room":"linux-a","name":"server-a"}' \
  'https://relay.cookai.dev/host/enroll'
```

응답에는 `token`, `room`, `expiresAt`이 포함된다. `token`은 A의
`RELAY_TICKET`으로 사용한다. `HOST_ENROLL_SECRET`은 장기간 저장할 호스트 토큰이 아니라
호스트 토큰을 발급할 수 있는 운영자 자격이므로 일반 참가자에게 전달하면 안 된다.

로컬 개발에서만 `--allow-loopback-enroll-without-secret`을 사용할 수 있다. 리버스
프록시 뒤에서는 외부 요청도 loopback으로 보일 수 있으므로 운영 서버에서는 활성화하지
않는다.

### 3.5 초대 코드 발급 예시

보기 전용 초대:

```bash
curl --fail-with-body \
  -H 'Authorization: Bearer <host-jwt>' \
  -H 'Content-Type: application/json' \
  -d '{"access":"view"}' \
  'https://relay.cookai.dev/rooms/invites'
```

조작 가능 초대:

```bash
curl --fail-with-body \
  -H 'Authorization: Bearer <host-jwt>' \
  -H 'Content-Type: application/json' \
  -d '{"access":"control"}' \
  'https://relay.cookai.dev/rooms/invites'
```

초대 코드는 현재 15분 동안 여러 사용자가 사용할 수 있다. 초대 코드를 교환해 발급되는
참가자 JWT는 현재 12시간 동안 유효하다.

> 현재 `clientd room create` 명령은 `RELAY_TICKET`이 아니라 레거시
> `~/.cli-relay/credentials.json`의 토큰을 읽는다. 헤드리스 운영에서
> `RELAY_TICKET`만 사용하는 경우에는 위 HTTP 호출 또는 Desktop 앱의 초대 발급 기능을
> 사용한다.

### 3.6 권한과 터미널 드라이버

| 권한 | 가능한 동작 |
|---|---|
| `view` | 터미널/응답 조회, 허용된 비입력 요청 |
| `control` | 터미널 입력 또는 채팅 요청 전송. 터미널 방 참가 시 자동 드라이버 대상 |
| `host` | 초대 발급, 권한 응답, 드라이버 지정 등 호스트 관리 |

터미널 방에서 실제 키 입력은 현재 드라이버에게만 허용한다. 여러 참가자가 동시에 화면을
볼 수는 있지만 입력 주체를 제한해 터미널 크기와 키 입력 충돌을 방지한다.

## 4. 모바일 연결

모바일 연결의 최종 대상은 항상 Desktop Host Gateway다.

### 4.1 `local-only`

```text
모바일 앱 ── 같은 Wi-Fi의 WebSocket ── Desktop Host Gateway ── 로컬 PTY
```

- Relay 서버를 거치지 않는다.
- 모바일과 PC가 서로 접근 가능한 같은 LAN에 있어야 한다.
- Desktop 화면에서 표시되는 `ws://<PC-IP>:<port>` 주소로 접속한다.
- 기본적으로 6768 포트를 우선하지만 이미 사용 중이면 사용 가능한 포트를 선택할 수 있다.
- VPN, Tailscale, 여러 NIC가 있으면 `모바일에 알릴 PC 주소`를 명시하는 편이 안전하다.

### 4.2 `relay-only`

```text
모바일 앱 ───────┐
                  ├─ Pie Relay 모바일 Director/Cell ── E2EE payload 전달
Desktop Gateway ─┘
        │
        └─ 로컬 PTY
```

- 모바일과 PC가 서로 다른 네트워크에 있어도 된다.
- Desktop Gateway와 모바일 앱이 모두 Relay 서버에 outbound로 연결한다.
- Desktop Gateway는 Relay 호스트 JWT로 assignment/control/data 채널에 등록한다.
- Desktop Gateway는 `/v1/identity`에서 Relay가 검증한 사용자·Tenant 정보를 받아 사용하며
  JWT payload의 `sub`를 직접 디코딩하지 않는다. 구형 Relay의 404 응답에서는 E2EE host
  key 기반 standalone identity로만 호환한다.
- Relay 서버는 E2EE payload를 전달하지만 복호화하거나 터미널 명령을 실행하지 않는다.

### 4.3 `automatic`

- QR에는 LAN endpoint와 Relay 연결 정보가 함께 포함된다.
- 모바일 앱은 LAN 연결을 우선 시도한다.
- LAN 연결을 사용할 수 없으면 Relay 연결로 전환한다.
- 저장된 호스트 프로필은 이후 재접속에도 선택한 연결 모드를 적용한다.

자동 전환은 Desktop Host Gateway를 생략하는 기능이 아니다. LAN과 Relay 중 어느 경로를
선택하더라도 최종 터미널 제공자는 동일한 Desktop Host Gateway다.

### 4.4 QR 페어링 흐름

1. Desktop 앱에서 `Pie Relay 모바일`을 연다.
2. `local-only`, `relay-only`, `automatic` 중 연결 방식을 선택한다.
3. Relay를 사용하는 모드라면 Relay URL과 호스트 토큰을 확인한다.
4. `모바일 호스트 시작` 또는 `새 QR로 시작`을 누른다.
5. Desktop 앱에 나타난 QR을 모바일 앱으로 스캔한다.
6. 모바일 앱에서 페어링을 승인한다.
7. 모바일 앱에 호스트와 터미널 목록이 나타난다.
8. 필요하면 Desktop 앱에서 등록 기기의 권한을 폐기한다.

QR에는 endpoint뿐 아니라 기기별 페어링 권한, 공개키, 연결 모드와 Relay invite 정보가
들어갈 수 있다. QR 화면과 페어링 링크를 외부에 공개하지 않는다.

## 5. 두 연결 계통의 차이

| 질문 | 원격 방 연결 | 모바일 연결 |
|---|---|---|
| 실제 실행 호스트 | `clientd`가 실행되는 PC/Linux 서버 | Desktop Host Gateway가 실행되는 PC |
| 화면 클라이언트 | 주로 Pie Relay Desktop | Pie Relay 모바일 앱 |
| Relay 경로 | 일반적으로 Relay 필수 | LAN이면 생략, 원격이면 Relay 사용 |
| 호스트 WebSocket | `/ws/agent` | `/v1/host/control`, `/v1/host/data/*` |
| 참가자 WebSocket | `/ws/participant` | `/v1/connect/*` |
| 로컬 실행기 | Node Executor 또는 PTY Host | Gateway가 관리하는 로컬 PTY |
| 인증 | 방의 host/participant JWT | Relay JWT + 기기별 페어링 자격 + E2EE |

### 현재 지원하지 않는 연결

다음 경로는 현재 자동으로 제공되지 않는다.

```text
모바일 앱 ── Relay 방 선택 ── 임의의 원격 Linux clientd
```

모바일 앱은 페어링한 Desktop Host Gateway의 터미널을 제어한다. 원격 Linux A의
`clientd` 터미널을 보려면 현재는 B의 Desktop 앱으로 A의 방에 참가해야 한다.

향후 모바일에 일반 Relay 방 목록, 참가자 JWT 교환, 터미널 방 프로토콜을 연결하면
모바일에서 원격 `clientd`를 직접 선택하는 기능을 추가할 수 있다. 이는 현재 모바일
Gateway 페어링과 별도의 제품 기능이다.

## 6. URL 표기 규칙

동일한 Relay라도 기능에 따라 URL 표기가 다르다.

- `clientd`의 `PIE_RELAY_URL`:
  `ws://host:port/ws/agent` 또는 `wss://relay.example.com/ws/agent`
- Relay REST API:
  `http://host:port` 또는 `https://relay.example.com`
- 모바일 Desktop 화면의 Relay URL:
  `http(s)` 또는 `ws(s)` 입력을 받을 수 있으며 Gateway가 HTTP(S) origin으로 정규화한다.
- 외부 운영 환경에서는 TLS를 사용해 HTTPS/WSS로 노출한다.
- 로컬 개발에서는 `http://127.0.0.1:<port>`와 `ws://127.0.0.1:<port>`를 사용할 수 있다.

`http(s)`는 토큰 발급·초대·모바일 assignment 같은 HTTP API에 사용하고, `ws(s)`는
지속적인 양방향 데이터 채널에 사용한다.

## 7. 인증 정보의 종류

| 자격 정보 | 소유자 | 용도 | 주의사항 |
|---|---|---|---|
| `RELAY_JWT_SECRET` | Relay 운영자 | 모든 JWT 서명·검증 | 변경하면 기존 JWT가 모두 무효화됨 |
| `HOST_ENROLL_SECRET` | Relay 운영자/신뢰 호스트 관리자 | 호스트 JWT 발급 권한 | 일반 사용자나 모바일 QR에 포함하지 않음 |
| 호스트 JWT | `clientd` 또는 Desktop Gateway | 방 호스트 등록, 초대 발급, 모바일 Relay 등록 | 로그·QR·저장소에 평문 노출 금지 |
| 참가자 JWT | Desktop/CLI 참가자 | 특정 방 참가 | 초대 코드 교환 후 발급 |
| 초대 코드 | 초대받은 참가자 | 참가자 JWT 발급 | 짧은 TTL이지만 만료 전 다중 사용 가능 |
| 모바일 device token | 페어링된 모바일 기기 | LAN Gateway 장치 인증 | 기기별 폐기 가능 |
| 모바일 invite/resume credential | 모바일 앱과 Relay | 최초 Relay 페어링 및 재연결 | 서버에는 resume credential 해시만 저장 가능 |

PAT 원문은 QR, Relay payload 또는 Docker 이미지에 포함하지 않는다. 외부 사용자 인증이
필요한 작업은 Executor Manager가 PAT introspection을 수행한 후 범위와 수명이 제한된
delegation token을 발급하는 방식으로 분리한다.

## 8. Executor Manager와의 관계

Executor Manager는 Relay 서버와 동일한 구성요소가 아니다.

- Relay는 연결과 메시지 중계를 담당한다.
- Manager는 사용자별 Docker Executor 할당, 큐, 작업 수명주기와 리소스 제한을 담당한다.
- 단일 PC의 `clientd` 또는 Desktop Gateway 연결에는 Manager가 필수는 아니다.
- 다수 사용자의 격리된 Claude Code 컨테이너를 운영할 때 Manager가 필요하다.
- Relay와 Manager를 결합 배포할 필요는 없으며 인증 경계와 장애 범위를 분리한다.

## 9. 운영 점검 항목

### Relay 서버

- `GET /healthz`와 `GET /readyz`가 2xx인지 확인한다.
- `RELAY_JWT_SECRET`을 재시작 간 동일하게 유지한다.
- 외부 호스트 등록이 필요하면 `HOST_ENROLL_SECRET`을 설정한다.
- 리버스 프록시가 WebSocket Upgrade 헤더를 전달하는지 확인한다.
- idle timeout을 장기 WebSocket에 맞게 충분히 설정한다.
- 모바일 재페어링을 줄이려면 `RELAY_MOBILE_STATE_FILE`을 영속 볼륨에 둔다.
- 초대 코드와 방 연결 registry는 단일 프로세스 메모리에 있으므로 다중 인스턴스 운영 전
  공유 상태/세션 라우팅 전략이 필요하다.
- Manager의 participant disconnect와 driver 인계 operation이 Relay 내부 control API까지
  도달하는지 staging에서 확인한다.

### 헤드리스 `clientd`

- `clientd` 서비스 사용자의 홈·프로젝트 접근 권한을 최소화한다.
- Claude Code는 같은 서비스 사용자로 미리 인증한다.
- `RELAY_TICKET`을 서비스 환경 파일이나 secret store로 주입한다.
- 로그에 토큰이 출력되지 않는지 확인한다.
- systemd의 `Restart=always`와 적절한 종료 타임아웃을 설정한다.
- `clientd`의 재연결과 자식 프로세스 재시작 로그를 모니터링한다.

### Desktop과 모바일

- Desktop에 표시된 LAN 주소가 현재 Wi-Fi 주소인지 확인한다.
- 네트워크 변경 후에는 Gateway를 다시 시작해 새 QR/endpoint를 생성한다.
- 오래되거나 분실한 모바일 기기의 권한을 폐기한다.
- 운영 Relay를 사용할 때는 유효한 호스트 JWT와 공개 HTTPS/WSS 주소를 사용한다.

## 10. 문제 진단 기준

| 증상 | 우선 확인할 항목 |
|---|---|
| Desktop에서 호스트가 끊김 | A의 `clientd` 프로세스, `/ws/agent` URL, 호스트 JWT 만료, Relay 로그 |
| 초대 코드가 유효하지 않음 | 코드 TTL, Relay 재시작 여부, 동일 Relay 인스턴스인지 확인 |
| 터미널은 보이지만 입력 불가 | 참가자의 `view/control` 등급과 현재 드라이버 확인 |
| 모바일 LAN 연결 실패 | 같은 Wi-Fi, Desktop endpoint IP, 방화벽, Gateway 리슨 포트 확인 |
| 모바일 Relay 연결 실패 | Relay 공개 URL, 호스트 JWT, `/v1/*` 프록시, control/data WebSocket 확인 |
| 모바일에서 계속 재연결 | 오래된 QR endpoint, 네트워크 변경, resume credential/기기 권한 확인 |
| Claude Code가 실행되지 않음 | A의 Claude CLI 설치·인증, Node 버전, Executor 경로와 서비스 사용자 권한 확인 |

## 11. 사용자 시나리오 요약

### 시나리오 A: 내 PC의 Claude Code를 원격 Desktop에서 사용

1. 내 PC에서 호스트 토큰으로 `clientd`를 실행한다.
2. 초대 코드를 발급한다.
3. 다른 PC의 Desktop 앱에서 초대 코드로 참가한다.
4. Claude 응답 또는 터미널을 확인하고 권한에 따라 조작한다.

### 시나리오 B: GUI 없는 Linux 서버의 터미널을 Desktop에서 사용

1. Linux 서버에 `clientd`, Node.js와 PTY Host를 설치한다.
2. `CLI_RELAY_ROOM_MODE=terminal`과 호스트 JWT로 데몬을 실행한다.
3. `control` 또는 `view` 초대 코드를 발급한다.
4. 사용자가 Desktop 앱으로 해당 방에 참가한다.

### 시나리오 C: 같은 Wi-Fi에서 모바일로 내 Desktop 제어

1. Desktop 앱에서 `local-only`로 모바일 Host Gateway를 시작한다.
2. 모바일 앱으로 QR을 스캔하고 페어링을 승인한다.
3. 모바일에서 로컬 터미널을 선택한다.

### 시나리오 D: 외부 네트워크에서 모바일로 내 Desktop 제어

1. Desktop 앱에 운영 Relay URL과 호스트 JWT를 설정한다.
2. `relay-only` 또는 `automatic`으로 Gateway를 시작한다.
3. 모바일 앱으로 QR을 스캔한다.
4. 모바일과 Desktop Gateway가 Relay를 통해 E2EE 세션을 구성한다.

## 12. 관련 문서와 구현 위치

- Relay 및 Executor Manager 개요: [`relay-and-executor-manager.md`](./relay-and-executor-manager.md)
- Relay 인증: [`relay-authentication.md`](./relay-authentication.md)
- Relay 운영: [`../server/deploy/README.md`](../server/deploy/README.md)
- Relay 서버: [`../server/cmd/relay/main.go`](../server/cmd/relay/main.go)
- `clientd`: [`../client/cmd/client/main.go`](../client/cmd/client/main.go)
- Node Executor/PTY: [`../client/node-executor/`](../client/node-executor/)
- Desktop 모바일 UI: [`../desktop/src/PieMobilePanel.tsx`](../desktop/src/PieMobilePanel.tsx)
- Desktop Gateway 실행: [`../desktop/src-tauri/src/lib.rs`](../desktop/src-tauri/src/lib.rs)
- 모바일 Host Gateway: [`../pie-mobile/adapter/host-gateway/`](../pie-mobile/adapter/host-gateway/)
- Executor Manager: [`../executor-manager/README.md`](../executor-manager/README.md)
- 단일 노드 배포와 운영: [`deployment-and-operations.md`](./deployment-and-operations.md)
