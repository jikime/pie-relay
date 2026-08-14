# Pie Relay 세션 모드, Docker 격리 및 상호 접속 설계

이 문서는 Pie Relay에서 로컬 터미널, 사용자별 Docker 작업 공간, 여러 사용자의 공동 제어, 상대방 클라이언트와의 상호 접속을 하나의 모델로 제공하는 방법을 정리한다. 멀티 세션, 장치 디렉터리와 Relay session scope는 구현됐으며, 최종 사용자용 내 장치·공유 장치 통합 UI는 계속 확장할 영역이다.

Host OS/Docker 실행 환경, LAN/Relay 접속 경로와 단독 Relay/Control Plane 발급
방식을 구분하는 기준은 [세션·실행 환경·Control Plane
정의](./session-runtime-and-control-plane.md)를 참고한다.

## 핵심 원칙

- `clientd`는 자기 장치나 컨테이너의 터미널을 외부에 제공하는 host 역할을 한다.
- Desktop/Mobile은 다른 host의 세션에 접속하는 participant 역할을 한다.
- 하나의 앱이나 사용자가 host와 participant 역할을 동시에 수행할 수 있다.
- 입력 권한은 클라이언트 단위가 아니라 **터미널 세션 단위**로 관리한다.
- 같은 터미널을 여러 명이 볼 수 있지만 raw keyboard 입력자는 항상 한 명의 driver다.
- 여러 사용자가 독립적으로 작업하려면 사용자별 또는 작업별로 다른 `sessionId`와 PTY를 사용한다.
- LAN과 Relay는 실행 모드가 아니라 세션에 도달하는 전송 경로다.

## 사용자에게 보이는 주요 모드

| 모드 | 실행 위치 | 참가 방식 | 설명 |
|---|---|---|---|
| Direct Local | 내 PC | 개인 | 내 PC의 `clientd`와 Claude Code/PTTY에 직접 접속한다. |
| Isolated Docker | 내 PC의 Docker | 개인 | 사용자 전용 컨테이너에서 호스트 환경과 분리해 작업한다. |
| Shared Local Session | 내 PC | 여러 사용자 | 여러 participant가 같은 로컬 터미널을 보고 driver를 인계한다. |
| Shared Docker Session | Docker | 여러 사용자 | 격리된 컨테이너 세션을 여러 사용자가 함께 관찰·제어한다. |

제품 UI를 네 개의 고정 모드로 구현하기보다 다음 세 축을 조합하는 것이 확장에 유리하다.

```text
실행 공간: [내 로컬] [격리 Docker]
참여 방식: [개인] [공유]
연결 방식: [자동] [LAN Only] [Relay Only]
```

세션 설정 예시는 다음과 같다.

```json
{
  "executionTarget": "local",
  "accessMode": "shared",
  "transportMode": "auto",
  "deviceId": "device-a",
  "sessionId": "session-123"
}
```

`auto`는 동일 네트워크에서 인증된 LAN 연결을 우선 시도하고, 실패하면 Pie Relay로 전환하는 정책이다. 사용자가 `LAN Only` 또는 `Relay Only`를 선택했다면 자동 전환하지 않는다.

## 사용자별 Docker 격리

Docker 이미지와 사용자 데이터는 구분해야 한다.

```text
모든 사용자가 공유하는 항목
└─ base image: OS, Node.js, Claude Code 실행 파일, clientd

사용자 A 전용
├─ container-A
├─ workspace-volume-A
└─ claude-auth-volume-A

사용자 B 전용
├─ container-B
├─ workspace-volume-B
└─ claude-auth-volume-B
```

`workspace volume`은 해당 사용자의 프로젝트, Git worktree와 작업 결과를 보존한다. `Claude 인증 volume`은 해당 사용자의 Claude 로그인 또는 인증 상태를 컨테이너 재생성 후에도 유지한다. 두 volume 모두 다른 사용자의 컨테이너에 마운트해서는 안 된다.

공통 이미지에 로그인 완료된 Claude 인증 정보, API key, Git credential을 넣어 배포하면 안 된다. 인증은 사용자별 암호화 volume 또는 실행 시 주입되는 secret으로 관리한다.

권장 마운트 예시는 다음과 같다. 실제 인증 경로는 Claude Code 실행 환경의 설정에 맞춰 확정한다.

```text
pie-workspace-user-a   → /workspace
pie-claude-auth-user-a → /home/pie/.claude
```

컨테이너는 Relay에 outbound WebSocket으로 접속하므로 사용자별 host port를 외부에 열 필요가 없다. Manager가 사용자와 `containerId/deviceId`의 대응 관계를 관리한다.

### 컨테이너 보안 기준

- non-root 사용자로 실행한다.
- privileged 모드와 Docker socket 마운트를 금지한다.
- 호스트의 임의 디렉터리를 마운트하지 않는다.
- 필요한 사용자 volume만 명시적으로 마운트한다.
- root filesystem은 가능하면 read-only로 사용한다.
- CPU, memory, PID, 디스크 사용량과 동시 세션 수를 제한한다.
- 불필요한 Linux capability를 제거하고 outbound network 정책을 적용한다.
- 외부 불특정 사용자의 임의 명령 실행까지 허용한다면 gVisor, Kata Containers 또는 MicroVM 같은 더 강한 격리를 검토한다.

## 하나의 클라이언트에서 여러 독립 세션

한 명의 driver 제약은 `clientd` 전체가 아니라 동일 PTY에만 적용된다. 하나의 사용자 컨테이너 또는 로컬 `clientd`에서 여러 PTY를 실행하면 여러 사용자가 동시에 독립적으로 작업할 수 있다.

```text
clientd / 사용자 컨테이너
├─ session-1 → PTY #1 → 사용자 Kim이 driver
├─ session-2 → PTY #2 → 사용자 Lee가 driver
└─ session-3 → PTY #3 → 사용자 Park가 driver
```

같은 세션에 여러 명이 들어오면 다음 규칙을 적용한다.

- `view`: 화면과 협업 채팅만 사용한다.
- `control`: 조작권을 요청할 수 있다.
- `driver`: 현재 세션에서 실제 keyboard/resize 입력을 보낼 수 있는 한 명이다.
- host operator는 driver를 부여·회수할 수 있다.
- 재접속은 살아 있는 driver lease를 빼앗지 않는다.

멀티 세션을 위해 `clientd`에는 세션별 `pty-host`를 생성·감시하는 Session Manager가 필요하다. Relay의 라우팅과 snapshot/sequence/driver 상태도 `roomId + sessionId` 또는 `deviceId + sessionId` 단위로 분리해야 한다.

## 상대방 클라이언트와 상호 접속

사용자 A가 B의 터미널에 접속하면서 B도 A의 터미널에 접속할 수 있다. 하나의 연결을 양방향 제어 채널로 재사용하지 않고 방향별 독립 세션을 만든다.

```text
세션 A-to-B
A Desktop ──participant──> Pie Relay ──host──> B clientd/PTTY

세션 B-to-A
B Desktop ──participant──> Pie Relay ──host──> A clientd/PTTY
```

각 장치는 동시에 두 역할을 수행할 수 있다.

```text
사용자 A 장치
├─ host: A의 Local/Docker 세션을 Relay에 등록
└─ participant: B가 공유한 세션에 접속
```

상호 접속은 자동 상호 신뢰를 의미하지 않는다. 다음 권한은 방향별로 따로 발급하고 회수해야 한다.

- A가 B에게 A 세션의 `view/control` 권한 부여
- B가 A에게 B 세션의 `view/control` 권한 부여
- 각 초대와 capability에 `ownerUserId`, `targetDeviceId`, `sessionId`, `access`, `expiresAt` 포함
- 한 방향의 권한을 취소해도 반대 방향 세션은 독립적으로 유지

예시 capability 범위는 다음과 같다.

```json
{
  "subject": "user-a",
  "ownerUserId": "user-b",
  "targetDeviceId": "device-b-linux",
  "sessionId": "session-b-17",
  "access": "control",
  "capabilities": ["terminal:view", "terminal:control"],
  "expiresAt": 1780000000
}
```

클라이언트가 보내는 `from`, 소유자, 대상 장치와 권한은 신뢰하지 않는다. Relay와 Manager가 검증된 토큰에서 해당 값을 결정하고 세션 라우팅에 적용한다.

## 장치와 세션 UI

Desktop/Mobile은 장치를 두 목록으로 나누는 것이 이해하기 쉽다.

```text
내 장치
├─ 내 MacBook Local
├─ 내 Linux Server
└─ 내 Docker Workspace

공유받은 장치
├─ 사용자 B / Linux / session-b-17 (control)
└─ 사용자 C / Docker / review-session (view)
```

사용자는 장치를 선택한 뒤 기존 세션에 참가하거나 새 세션을 생성한다. 한 Desktop에서 자기 장치의 host 상태를 유지하면서 여러 상대방 세션 탭을 동시에 열 수 있다.

## 구성요소별 책임

### Executor Manager

- 외부 회원·PAT를 내부 `userId`와 매핑한다.
- 사용자별 컨테이너와 전용 volume을 생성·복구·삭제한다.
- 자원 제한, 이미지 버전, 세션 수와 수명 주기를 관리한다.
- 사용자에게 허용된 `deviceId/sessionId`만 조회·제어하게 한다.

### clientd / Session Manager

- 장치를 Relay에 host로 등록한다.
- 세션별 `pty-host`를 생성하고 종료·재시작한다.
- 세션별 cwd, Claude session, snapshot과 sequence를 분리한다.
- 컨테이너에서는 사용자 volume 밖의 경로를 노출하지 않는다.

### Pie Relay

- host와 participant의 outbound WebSocket을 매칭한다.
- `deviceId + sessionId` 기준으로 프레임을 격리해 라우팅한다.
- view/control capability와 단일 driver lease를 강제한다.
- roster, heartbeat, rate limit, slow-peer eviction과 감사 메타데이터를 관리한다.
- PTY payload 내용과 Claude 인증 정보를 저장하지 않는다.

### Desktop/Mobile

- 내 장치와 공유받은 장치를 표시한다.
- Local/Docker, 개인/공유, LAN/Relay 정책을 선택한다.
- 여러 세션 탭과 viewer/controller/driver 상태를 표시한다.
- 조작권 요청·부여·회수와 접속 종료를 제공한다.

## 현재 구현과 후속 작업

현재 구현된 범위는 다음과 같다.

1. Manager의 사용자별 container/workspace/auth-state 영속 매핑
2. `clientd` Session Manager와 세션별 `pty-host` 생성·감시·종료
3. `room + deviceId + sessionId` 라우팅과 세션별 driver/roster 격리
4. 사용자·장치·세션·participant·grant API와 PostgreSQL 저장소
5. 방향별 session-scoped view/control capability
6. Relay presence, 재연결 복구, 실제 참가자 연결 종료와 driver 인계
7. Desktop의 Local/Docker·private/shared·LAN/Relay 세션 설정
8. Admin Web의 사용자 할당, 상태 조회, 세션/컨테이너 operation과 감사 로그

후속 제품 작업은 Desktop/Mobile의 내 장치·공유 장치·다중 세션 탭 UX, 외부 회원
서비스 webhook/PAT 연동, 사용자 삭제와 volume 보존 정책, 다중 Docker host placement다.

가장 안전한 기본값은 `Docker + private + Relay`, 공유 초대의 기본 권한은 `view`다. Local control 공유는 초대받은 사용자가 호스트 사용자 권한으로 명령을 실행할 수 있으므로 명시적 경고와 짧은 만료 시간을 적용한다.
