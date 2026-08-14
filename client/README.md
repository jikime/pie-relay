# Pie Client (`pie-client`)

`pie-client`는 **내 PC 또는 원격 서버에서** 동작하는 Pie 전용 실행 클라이언트다. SDK 실행기
(`node-executor/executor.mjs`) 또는
ACP 실행기(`node-executor/acp-executor.mjs`)를 띄워 로컬 AI 에이전트를 구동하고,
릴레이의 `/ws/agent` 축에 붙여 중계한다.
CLI 는 여기(내 PC)에서 실행되고 — 릴레이는 내 파일을 보지 못한다.

원격 사이트 → AI 서비스 → 릴레이 → **로컬 PC(여기)** → Claude Code 또는 Codex CLI.

호스트 인증은 릴레이 **자체 발급 호스트 토큰**을 쓴다. 데스크톱 앱의 "방 만들기"가
릴레이 `POST /host/enroll` 로 토큰을 받아 데몬에 `RELAY_TICKET` 으로 전달한다.
Relay는 외부 브라우저 로그인에 직접 의존하지 않는다.

## 구성

```
cmd/client/main.go              진입점 — 데몬 실행 / `connect` · `start` · `stop` · `status` · `disconnect`
internal/credentials/credentials.go ~/.cli-relay/credentials.json 로드(레거시·수동 토큰)
internal/devicecredentials/     제품별 장치 Access/회전형 Refresh 자격의 `0600` 원자적 저장
internal/chatagent/agent.go     relay ws ⇄ executor stdio 브리지, 백오프 재접속
internal/ptyagent/ptyagent.go   터미널 모드용 pty-host 브리지
internal/rooms/                 초대 발급(room create) · 참가(join) HTTP·ws 헬퍼
internal/tui/                   게스트 채팅 Bubble Tea UI
internal/executor/              Node 실행기 감독 + NDJSON 이벤트 파싱
node-executor/
  executor.mjs                 @anthropic-ai/claude-agent-sdk stdio 어댑터
  acp-executor.mjs             Relay NDJSON ⇄ ACP v2 JSON-RPC 어댑터
  smoke.mjs                    단독 smoke 테스트
  package.json                 deps: @anthropic-ai/claude-agent-sdk
```

## 사전 준비

```bash
# SDK 모드는 Node ≥ 20, ACP/Claude Code·Codex 모드는 Node ≥ 22가 필요하다.
# 사용할 로컬 CLI 인증은 미리 완료되어 있어야 한다.
claude auth status
codex login status
cd node-executor && npm install && cd ..
node node-executor/smoke.mjs        # OK: session_id + text + done  (claude 없으면 SKIP)
```

## Pie Client 설치

일반 사용자는 GitHub Pages의 공식 설치 스크립트를 사용한다.

```bash
curl -fsSL https://jikime.github.io/pie-relay/install.sh | sh
pie-client version
```

설치 프로그램은 macOS·Linux와 Intel·ARM64를 자동으로 판별하고 GitHub Release에서
플랫폼별 Go 바이너리와 네이티브 Node Executor 런타임을 받는다. `checksums.txt`의
SHA-256과 일치하지 않으면 설치하지 않는다. 관리자 권한은 필요하지 않으며 기본 경로는
다음과 같다.

```text
실행 명령: ~/.local/bin/pie-client
버전별 본체: ~/.local/share/pie-client/versions/<버전>-<체크섬>/
현재 버전: ~/.local/share/pie-client/current
```

특정 버전을 설치하거나 별도 경로를 쓰는 방법은 다음과 같다.

```bash
curl -fsSL https://jikime.github.io/pie-relay/install.sh -o /tmp/install-pie-client.sh
sh /tmp/install-pie-client.sh --version v1.0.0
sh /tmp/install-pie-client.sh --install-dir "$HOME/bin"
```

개발 중에는 저장소 루트에서 `go -C ./client run ./cmd/client ...`로 바로 실행할 수 있다.
직접 빌드하려면 다음 명령을 사용한다.

```bash
# cli-relay 저장소 루트
cd client
npm ci --prefix node-executor
mkdir -p bin
go build -trimpath -o ./bin/pie-client ./cmd/client

# 현재 셸에서 실행
./bin/pie-client --help
```

`pie-client`를 PATH에 등록한 배포 패키지에서는 앞의 `./bin/` 없이 실행한다. 기존 Desktop
사이드카 파일명 `clientd`와 Docker 내부 명령 `pie-relay-client`는 이전 버전과의 호환을 위해 유지하지만,
사용자 문서와 새 장치 연결 화면은 `Pie Client`와 `pie-client`를 정식 이름으로 사용한다.

## 실행

### 1. 호스트 토큰 발급 (최초 1회)

데스크톱 앱의 "방 만들기"에 발급 키를 입력하면 릴레이가 호스트 토큰을 발급하고,
데몬 시작 시 `RELAY_TICKET` 으로 전달한다. 별도의 브라우저 로그인 단계는 없다.

### 2. 데몬 실행

발급받은 호스트 토큰을 `RELAY_TICKET` 으로 넣어 실행하는 것이 주 경로다.

```bash
export RELAY_TICKET=<호스트 토큰>              # 데스크톱 "방 만들기"로 발급
go run ./cmd/client
#  실행기 경로를 옮겼으면: EXECUTOR_PATH=/path/to/node-executor/executor.mjs go run ./cmd/client
#  로컬 Relay로 전환: PIE_RELAY_URL=http://127.0.0.1:13412 go run ./cmd/client
#  운영 주소 변경: PIE_RELAY_URL=https://RELAY_HOST go run ./cmd/client
```

- `RELAY_TICKET` 도 `~/.cli-relay/credentials.json` 도 없으면 즉시 종료하며 데스크톱
  앱에서 "방 만들기"로 호스트 토큰을 먼저 발급하라고 안내한다.
- ws 가 끊기면(릴레이 재시작·네트워크) 백오프로 **자동 재접속**한다.
- 접속 중 릴레이가 401 을 돌려주면(토큰 만료·거부) 즉시 종료한다 — 리프레시 경로는
  없으므로 데스크톱 앱에서 호스트 토큰을 **재발급**해야 한다.

### (레거시) credentials.json 폴백

`RELAY_TICKET` 이 없을 때 `~/.cli-relay/credentials.json` 이 있으면 그 `accessToken` 을
티켓처럼 사용한다. 과거 산출물이나 수동으로 넣은 토큰 파일용 폴백이며, 리프레시는
없으므로 만료되면 재발급해야 한다.

## 방·게스트

```bash
go run ./cmd/client room create        # 호스트 토큰으로 초대 코드 발급
go run ./cmd/client join <code> --name 이름   # 게스트가 초대 코드로 참가(채팅 UI)
```

`room create` 는 credentials.json 폴백을 쓴다. 데스크톱 앱은 발급받은 호스트 토큰으로
릴레이에 직접 HTTP 호출해 초대를 발급한다.

## 제품별 Agent 실행 장치 연결

워크스페이스 소유자 또는 관리자가 Vibe Canvas의 **새 실행 장치 연결**에서 10분짜리
코드를 발급한 뒤 화면에 표시된 명령을 실행한다. `--server`에는 Relay나
Manager가 아니라 **그 코드를 발급한 제품의 공개 주소**가 들어간다. 코드를 교환한 뒤
응답의 `controlUrl`을 통해 해당 제품 전용 Manager로 연결된다.

Kroot Studio는 이 명령을 사용하지 않는다. Kroot는 `kroot auth login`으로 PAT를
`~/.kroot/credential.json`에 저장한 뒤 `kroot chat start`로 연결하는 별도 인증
프로토콜을 사용한다.

```bash
pie-client connect \
  --server https://vibe-canvas-builder.vercel.app \
  --code ABCD-EFGH \
  --name "디자인 작업 PC"
```

`connect`는 페어링, 자격 저장과 작업 대기 시작을 한 번에 처리한다. 실행 중인 터미널을 닫으면
Agent 실행도 멈추며, 다음 실행부터는 `pie-client start`만 사용한다.

개발 소스에서 곧바로 확인할 때는 cli-relay 저장소 루트에서 다음 명령을 사용한다.

```bash
go -C ./client run ./cmd/client connect \
  --server https://vibe-canvas-builder.vercel.app \
  --code ABCD-EFGH
```

`connect`는 코드를 발급한 제품에 한 번만 교환하고 `~/.cli-relay/device-credentials.json`을 권한
`0600`으로 저장한 뒤 Session Manager를 시작한다. `start`는 해당 파일을 자동으로 찾아 15분짜리 Access token을
만료 전에 갱신하고, Control이 배정한 ACP 세션을 outbound polling한다. 401을 받으면 Refresh token을
회전해 한 번 재시도한다. 이전 Refresh token이 다시 사용되거나 관리자가 장치를 해제하면 새 코드로
다시 연결해야 한다.

연결에 성공한 코드는 이미 소비되므로 같은 `connect` 명령을 다시 실행하지 않는다. 터미널만 닫았다면
`pie-client start`를 사용한다. 자격 파일을 잃었거나 Refresh token이 만료된 경우에는 해당 제품의
등록된 실행 장치에서 **다시 연결**을 눌러 새 코드를 발급한다. 이 코드는 기존 장치 ID와 Agent 배정을
유지하고 자격만 교체한다. 실행 중인 프로세스가 있으면 먼저 `pie-client stop`을 실행해야 하며,
`connect`도 실행 상태를 확인해 자격 파일 경합을 차단한다.

### 실행 수명주기

```bash
pie-client start                 # 이미 연결된 장치 다시 시작
pie-client status                # 연결 장치와 현재 실행 상태 확인
pie-client stop                  # 로컬 Session Manager 정상 종료
pie-client disconnect            # 서버 장치 폐기 후 로컬 자격 삭제
pie-client disconnect --local-only  # 서버에 접속할 수 없을 때 로컬 자격만 삭제
```

`start`와 `connect`는 같은 자격 파일에 대해 중복 실행되지 않는다. 실행 상태 파일은 장치 자격과 같은
디렉터리에 권한 `0600`으로 저장되며, `stop`은 이 파일의 임의 토큰으로 보호된 loopback API를 통해
정상 종료한다. PID만 보고 다른 프로세스를 종료하지 않는다.

### AI 도구 준비 상태

Pie Client는 시작할 때와 약 5분 간격으로 다음 항목을 로컬에서 확인한다.

- Claude Code 설치, `claude auth status`, 버전
- Codex 설치, `codex login status`, 버전
- Codex App Server의 `model/list`가 반환한 현재 계정의 모델 ID·표시 이름·기본 모델

Control heartbeat에는 `READY`, `LOGIN_REQUIRED`, `NOT_INSTALLED`, `ERROR` 상태와 안전한 버전·모델
정보만 포함한다. CLI 인증 토큰, 계정 정보와 인증 명령의 stdout/stderr는 전송하거나 저장하지 않는다.
Builder는 컴퓨터의 온라인 상태와 도구 준비 상태를 별도로 표시하고, 선택한 도구가 로그인되지 않았거나
설치되지 않았으면 ACP 세션 생성 전에 중단한다. 구형 Pie Client처럼 상태 metadata가 없는 장치는
`상태 확인 전`으로 표시해 하위 호환 실행을 허용한다.

로그인 직후 즉시 다시 확인하려면 다음처럼 재시작한다.

```bash
pie-client stop
pie-client start
```

Claude Code를 사용하지 않고 Codex만 사용하는 컴퓨터도 Codex CLI와 번들 Codex adapter가 준비되어
있다면 Pie Client를 시작할 수 있다.

### Docker 배포

`executor-manager/Dockerfile.executor`는 `pie-client`, Node.js, `node-executor`와 고정된 npm 의존성을
한 이미지에 설치한다. Manager가 관리하는 격리 컨테이너는 장치 페어링이 필요한 `pie-client start`가 아니라
내부 세션 API만 여는 `pie-client sessions serve`를 기본 명령으로 실행하므로 컨테이너 안에서
`npm ci`나 별도의 Node 명령을 입력하지 않는다. 기존 이미지 태그 `pie-relay-client:latest`와 내부
명령 `pie-relay-client`는 배포 호환을 위해 별칭으로 유지한다.

`sessions serve`는 기본적으로 `--control-mode disabled`라서 호스트의 장치 자격 파일이나
`PIE_CONTROL_PLANE_*` 환경변수를 자동 탐색하지 않는다. 사용자 장치의 Control Plane 연결은 공개
수명주기 명령인 `pie-client connect`/`pie-client start`가 내부적으로
`--control-mode device`를 지정할 때만 활성화된다. 따라서 Executor 컨테이너가 실수로 호스트 PC의
장치 신원을 가져가거나 중복 등록하는 것을 막는다.

`deploy/local/pie-local.sh up`은 이 Executor 이미지를 먼저 빌드하고, Manager가 필요할 때 사용자별
Executor 컨테이너를 동적으로 만든다. 따라서 `deploy/compose.yaml`에 Pie Client 상시 서비스가 하나 더
나열되는 구조가 아니라, Manager가 Docker socket을 통해 격리된 Pie Client 컨테이너를 관리한다.

로컬 Wi-Fi나 VPN 전환으로 LAN 주소가 바뀐 경우 `deploy/local/pie-local.sh refresh-address`가 현재 주소와
실행 중인 Relay·Manager 설정을 비교한다. 주소가 달라졌을 때만 두 서비스를 재생성하며 이미지와 데이터는
그대로 유지한다.

기존 자동화는 `PIE_CONTROL_PLANE_URL`, `PIE_CONTROL_PLANE_TOKEN`, `PIE_DEVICE_ID`를 직접 지정해도
계속 동작한다. 세 값 중 하나라도 명시하면 장치 자격 파일보다 이 호환 경로를 우선한다.

## 환경변수

| 변수 / 플래그 | 기본 | 설명 |
|---|---|---|
| `--relay-url` / `PIE_RELAY_URL` | CookAI Relay | Relay origin 또는 `/ws/agent`. 로컬도 이 값만 변경 |
| `--ticket` / `RELAY_TICKET` | (없음) | 호스트 토큰(주 경로) — 지정 시 `credentials.json` 을 무시 |
| `--executor` / `EXECUTOR_PATH` | `./node-executor/executor.mjs` (바이너리 옆 → cwd) | 실행기 경로 |
| `--acp-executor` / `ACP_EXECUTOR_PATH` | `./node-executor/acp-executor.mjs` (바이너리 옆 → cwd) | ACP 변환 실행기 경로 |
| `--pty-host` / `PTY_HOST_PATH` | `./node-executor/pty-host.mjs` (바이너리 옆 → cwd) | 터미널 모드 pty-host 경로 |
| `CLI_RELAY_ROOM_MODE` | (SDK chat) | `terminal` 또는 `acp`; 미지정은 기존 SDK chat |
| `PIE_ACP_AGENT_COMMAND` | `claude-agent-acp` | 실행할 ACP 호환 에이전트 명령 |
| `PIE_ACP_AGENT_ARGS_JSON` | `[]` | 에이전트 인자 JSON 문자열 배열 |
| `PIE_ACP_PERMISSION_POLICY` | `interactive` | `interactive`, `allow_once`, `deny_once`; 공유 서비스는 관리자가 설정 |
| `PIE_ACP_RPC_TIMEOUT_MS` | `60000` | initialize/session 생성 RPC 제한 시간 |
| `PIE_ACP_TURN_TIMEOUT_MS` | `7200000` | 한 ACP 응답의 최대 실행 시간 |
| `PIE_DEVICE_CREDENTIALS` | `~/.cli-relay/device-credentials.json` | `pair`가 만든 장치 자격 파일의 대체 경로 |
| `PIE_PAIRING_URL` | (없음) | `connect`/`pair --server`의 기본값. 일회용 코드를 발급한 제품 주소 |
| `PIE_CANVAS_URL` | (없음) | 기존 설치 호환용 `PIE_PAIRING_URL` 대체 변수 |
| `PIE_CONTROL_PLANE_URL` | (없음) | 기존 PAT 방식의 Control 주소; 설정 시 pairing 파일보다 우선 |
| `PIE_CONTROL_PLANE_TOKEN` | (없음) | 기존 PAT 방식의 Control 자격 |
| `PIE_DEVICE_ID` | (없음) | 기존 PAT 방식의 고정 장치 ID |

ACP로 Claude Code를 실행하는 예시는 다음과 같다. 기본 이미지에는
`@agentclientprotocol/claude-agent-acp`를 고정 버전으로 포함하므로 런타임에
`npx`로 다운로드하지 않는다.

```bash
export CLI_RELAY_ROOM_MODE=acp
export ACP_EXECUTOR_PATH="$PWD/node-executor/acp-executor.mjs"
export PIE_ACP_AGENT_COMMAND="$PWD/node-executor/node_modules/.bin/claude-agent-acp"
go run ./cmd/client
```

ACP 권한 옵션의 실제 `optionId`는 에이전트마다 다를 수 있다. 실행기는 값을
하드코딩하지 않고 `kind=allow_once/reject_once`를 찾아 응답한다. 운영 채널의
자동 승인은 UI 입력이 아니라 관리자 환경설정으로만 켜야 한다.

상세한 URL 우선순위는 [`../docs/relay-url-configuration.md`](../docs/relay-url-configuration.md)를
참고한다.

## 검증

```bash
go test ./...                       # credentials / chatagent / rooms / executor 단위 테스트
node node-executor/smoke.mjs        # 실행기 단독 end-to-end (claude 필요)
```
