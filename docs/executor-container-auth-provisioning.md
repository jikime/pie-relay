# 사용자별 Executor 컨테이너·인증 프로비저닝 설계

이 문서는 외부 회원 서비스의 사용자 가입을 시작점으로, 사용자별 Docker Executor를
생성하고 Claude 및 Kroot 인증 상태를 준비한 뒤 Relay 데몬을 기동하는 기준 흐름을
정의한다.

> 기준: 2026-07-26 논의 결과. `~/.calude`나 `credntial.json`이 아니라 실제 경로는
> 각각 `~/.claude`, `~/.kroot/credential.json`이다.

## 1. 확정된 설계 원칙

1. 사용자마다 독립된 Executor 컨테이너를 한 개 이상 할당한다.
2. 공용 Executor 이미지는 재사용하되 사용자 인증정보를 이미지 레이어에 넣지 않는다.
3. 컨테이너마다 독립된 Home 상태 volume과 Workspace volume을 만든다.
4. Claude는 의도적으로 하나의 공통 구독 인증을 사용한다. Event Manager의 중앙
   Broker가 setup-token을 암호화·버전 관리하고 Claude Code 프로세스 시작 시에만
   주입한다. 사용자 Home에는 인증 파일을 복제하지 않는다.
5. Kroot PAT는 사용자마다 다르다. 외부 로그인 과정에서 발급된 PAT로 각 사용자
   Home volume에 `.kroot/credential.json`을 생성한다.
6. 컨테이너가 재생성돼도 Home과 Workspace volume은 보존한다.
7. 인증 준비가 끝난 컨테이너의 상시 Relay 프로세스는 Pie
   `pie-client start` 하나다. Kroot CLI는 Hook 또는 사용자 명령에서
   외부 Kroot API가 필요할 때 실행한다.

## 2. 이미지와 volume의 경계

### 2.1 공용 이미지에 포함할 것

```text
Pie Executor 공용 이미지
├─ pie-relay-client/clientd
├─ Node Executor 및 PTY Host
├─ Claude Code 실행 파일
├─ kroot 명령어
├─ CA 인증서, git, ssh, ripgrep 등 공용 도구
├─ tini 기반 init
└─ health/readiness 검사 도구
```

이미지는 버전과 digest로 관리하며 모든 사용자 컨테이너가 같은 검증된 이미지를
재사용한다.

### 2.2 공용 이미지에 포함하지 않을 것

```text
~/.claude
~/.kroot/credential.json
Kroot PAT
Claude 로그인 세션
사용자 workspace
사용자별 MCP 및 프로젝트 설정
실행 로그와 임시 파일
```

이미지 레이어에 한 번 들어간 secret은 이후 레이어에서 삭제해도 `docker save`나
registry cache를 통해 복구될 수 있다. 따라서 인증된 컨테이너를 `docker commit`하여
템플릿으로 사용하지 않는다.

### 2.3 사용자별 volume

논리적으로 사용자마다 최소 다음 두 volume이 필요하다.

```text
사용자 A
├─ pie-home-user-a       → /home/executor
│  ├─ .claude/
│  └─ .kroot/credential.json
└─ pie-workspace-user-a  → /workspace

사용자 B
├─ pie-home-user-b       → /home/executor
│  ├─ .claude/
│  └─ .kroot/credential.json
└─ pie-workspace-user-b  → /workspace
```

Docker named volume과 사용자별 host directory bind mount 모두 가능하다. 현재
Executor Manager는 다음 사용자별 bind mount를 이미 사용한다.

```text
${PIE_EXECUTOR_STATE_DIR}/{userId} → /home/executor
${PIE_EXECUTOR_WORK_DIR}/{userId}  → /workspace
```

사용자별 Docker Executor는 Manager에
`PIE_EXECUTOR_PERMISSION_MODE=bypassPermissions`를 설정해 Claude Code의 도구 승인
대기 없이 자동 실행할 수 있다. 이 값은 컨테이너의
`CLI_RELAY_PERMISSION_MODE`로 고정되므로 채팅 요청자가 임의로 권한을 높일 수 없다.
정책을 바꾸면 Manager가 컨테이너 본체만 재생성하며, 위의 사용자 HOME과 workspace
bind mount는 그대로 보존한다.

이 모드는 비대화형 AI 작업에 필요한 대신 Claude가 컨테이너 안의 파일과 명령을
승인 없이 사용할 수 있게 한다. 따라서 비-root 사용자, read-only root filesystem,
capability 제거, PID/CPU/메모리 제한, 사용자별 HOME/workspace와 격리 network를 함께
유지해야 한다. Desktop이나 실제 PC의 Host OS에 직접 연결하는 Executor에는 적용하지
않고 기존 사용자 승인을 유지한다.

따라서 현재 구현에서는 `executor-state/{userId}`가 사용자 전용 Home volume의
실체다. 컨테이너 실행 사용자는 UID/GID `10001:10001`이고, Manager가 기존 파일까지
해당 소유권으로 정규화한다.

`PIE_EXECUTOR_STATE_SEED_DIR`을 설정하면 Manager는 컨테이너를 처음 시작하기 전에
해당 보호 디렉터리의 일반 파일을 사용자별 `executor-state/{userId}`에 최초 1회만
복제한다. 기존 사용자 파일은 덮어쓰지 않으며 symlink와 특수 파일은 거부한다. 복제
완료 표식과 파일은 `0600`, 디렉터리는 `0700`으로 만든 뒤 컨테이너 사용자
`10001:10001` 소유권으로 정규화한다. 이를 통해 공통 Claude 인증을 이미지 레이어에
넣지 않고 신규 사용자별 Home에 독립적으로 배포한다. 현재 운영 기준에서는 이 seed를
최초 마이그레이션 용도로만 사용한다. 이후 Claude 인증 교체는 Event Manager의
버전 저장소와 `Claude 인증` 관리 API가 기존·신규 사용자 모두에게 적용한다. 구체적인
절차는 [Event Manager 기반 Claude 인증 운영](./event-manager-claude-auth.md)을 따른다.

## 3. 인증 정보의 의미와 분리

| 자격 | 식별하는 대상 | 저장 위치 | 사용처 |
|---|---|---|---|
| Pie Device Credential | PC 또는 컨테이너 장치 | Pie 전용 안전 저장소 | Control Plane heartbeat, 세션 할당·상태 보고 |
| 공통 Claude 구독 OAuth | 공통 Claude 계정 | Event Manager 암호화 Broker | Claude Code child 시작 시 주입 |
| Kroot PAT | 실제 Kroot 사용자 | 사용자별 `.kroot/credential.json` | Kroot API 및 Kroot Relay 인증 |
| Pie Relay Session JWT | 특정 Pie 세션 | 메모리 | Pie Relay Host/Participant 연결 |

Pie Device Credential과 Kroot PAT는 서로 대체하지 않는다. Device Credential은
“어느 Executor인가”를 증명하고, Kroot PAT는 “외부 시스템의 어느 사용자인가”를
증명한다.

Kroot 외부 시스템은 전달받은 PAT를 인증 서버에 introspection하고 그 결과의
`userId`를 실제 사용자로 사용한다. 요청 body의 `userId`나 컨테이너 label은 인증의
근거가 아니다.

## 4. 공통 Claude 구독 OAuth 주입

모든 컨테이너가 동일한 Claude 계정을 사용하는 것은 의도된 정책이다. 갱신 가능한
`.credentials.json`을 복제하거나 공유하면 토큰 회전 경쟁이 생길 수 있으므로 현재
기준 방식은 다음과 같다.

```text
Event Manager Credential Broker
├─ setup-token AES-GCM 암호화 버전 N
├─ 세션 시작 → user-a의 Claude child 전용 env
├─ 세션 시작 → user-b의 Claude child 전용 env
└─ 세션 시작 → user-c의 Claude child 전용 env
```

공통 원본은 Docker image, Git 저장소, 일반 backup, Compose 환경변수에 포함하지
않는다. Manager 저장소는 전용 master key로 암호화하며 파일 권한을 `0600`으로
제한한다. 외부 secret manager와 연동할 때도 Manager 프로세스만 복호화할 수 있게 한다.

사용자별 `.claude`에는 프로젝트 이력, 대화 기록, skills/MCP 설정처럼 그 사용자에게
필요한 mutable 상태만 유지한다. 구독 인증 토큰은 포함하지 않는다.

Claude 구독 OAuth를 교체하면 다음 절차로 재조정한다.

1. Event Manager에서 새 `claude setup-token`을 발급한다.
2. Manager가 새 암호화 버전을 게시하고 일회성 후보를 삭제한다.
3. 영향받는 Executor를 제한된 동시성으로 재시작한다.
4. Controller가 새 채팅 세션에 활성 버전을 주입한다.
5. canary 실제 대화가 성공한 뒤 전체 사용자 흐름을 확인한다.
6. 실패하면 직전 버전으로 rollback한다.

## 5. 사용자별 Kroot credential 생성

외부 회원 서비스의 로그인/가입 과정에서 사용자별 PAT를 발급받는다. 외부 서비스는
TLS와 인증된 lifecycle/provisioning 요청을 통해 Manager 또는 Credential Provisioner에
PAT와 사용자 식별 정보를 전달한다.

운영 외부 서비스는 PAT를 발급한 인증 서버에서 사용자와 토큰의 관계를 검증한 뒤
Manager를 호출해야 한다. Manager는 제3자별 인증 체계를 추측하거나 PAT를 Pie Relay
자격으로 사용하지 않고, Integration Registry에 등록된 경로와 형식대로 다음 파일을
안전하게 생성한다.

```json
{
  "serverUrl": "grpcs://adk-server.kroot.io",
  "accessToken": "kpat_user_specific_token",
  "expiresAt": "0001-01-01T00:00:00.000Z",
  "authKind": "pat",
  "updatedAt": "2026-07-26T00:00:00Z",
  "relayUrl": "wss://adk-relay.kroot.io/ws/agent",
  "deviceId": "unique-container-device-id"
}
```

생성 규칙은 다음과 같다.

- `accessToken`은 사용자마다 별도 발급한다.
- `deviceId`는 컨테이너별로 암호학적 난수로 생성하고 Home volume에 영속화한다.
- 같은 사용자의 컨테이너를 재생성할 때는 기존 `deviceId`를 유지한다.
- 다른 컨테이너로 credential을 복제해야 한다면 PAT 관련 필드만 복제하고 새로운
  `deviceId`를 생성한다.
- `.kroot` 디렉터리는 `0700`, `credential.json`은 `0600`, 소유자는 컨테이너
  사용자 `10001:10001`로 설정한다.
- 임시 파일을 같은 filesystem에 쓴 다음 fsync와 atomic rename으로 교체한다.
- PAT 원문은 Manager 로그, DB 일반 컬럼, operation payload, Relay frame에 기록하지
  않는다. 저장이 불가피하면 KMS로 봉인하거나 secret reference만 저장한다.

Kroot 데몬은 재연결 시 `credential.json`을 다시 읽으므로 재로그인으로 PAT가 바뀌면
파일을 atomic하게 갱신하고 연결을 재수립할 수 있다. 만료 또는 폐기된 PAT는
`WAITING_FOR_KROOT_AUTH` 상태로 전환하고 사용자에게 재인증을 요구한다.

## 6. 회원가입부터 컨테이너 연결까지

```text
외부 서비스 회원가입·Kroot 로그인
  │
  ├─ 검증된 external userId와 사용자별 PAT 확보
  ▼
Pie lifecycle webhook / Provisioning API
  │
  ├─ eventId/idempotency key 검증
  ├─ 외부 인증 서비스가 검증한 externalSubject 사용
  ├─ 사용자·Quota 레코드 upsert
  ▼
Executor Manager
  │
  ├─ executor-state/{userId} 생성
  ├─ workspaces/{userId} 생성
  ├─ 공통 Claude 인증 seed 복제
  ├─ 사용자별 .kroot/credential.json atomic 생성
  ├─ 권한과 소유권 검증
  └─ 공용 이미지로 사용자 컨테이너 생성
  ▼
컨테이너 runtime
  │
  ├─ 인증·volume preflight
  ├─ pie-client start
  ├─ 필요 시 kroot 명령/Hook → Kroot API
  ├─ health/readiness 및 Pie Relay 연결 상태 보고
  └─ 종료 신호 전달·제한된 backoff 재시작
  ▼
READY / RELAY_CONNECTED
```

`docker cp`로 실행 중인 컨테이너에 임시 복사하는 방식보다, 컨테이너를 만들기 전에
마운트 대상 Home 디렉터리를 준비하는 방식을 기준으로 한다. 이 방식은 재생성,
백업·복구, 권한 검증과 실패 재시도에 유리하다.

동일한 회원가입 또는 webhook이 반복돼도 새 컨테이너와 volume을 중복 생성하지 않는다.
기존 인증 상태가 유효하다면 덮어쓰지 않고, 더 새로운 credential version일 때만
atomic update한다.

## 7. 컨테이너 프로세스와 상태 모델

컨테이너 안에서 `kroot chat start --daemon`을 함께 실행하지 않는다. `tini` 아래의
상시 프로세스는 Pie 세션 매니저 하나이며, Manager가 대화별 Pie Relay 세션을
시작·종료한다.

```text
PID 1: tini
└─ pie-client start
   └─ 대화별 Node Executor / Claude Code
```

Pie와 Kroot를 독립 운영한다는 기존 결정을 유지한다.

- `pie-relay-client`는 Pie Control Plane/Pie Relay 세션을 담당한다.
- `kroot` 명령과 Hook은 `.kroot/credential.json`의 PAT로 Kroot API를 호출할 수 있다.
- Kroot 자체 채팅 Relay를 별도로 사용할 때만 `kroot chat start`를 별도 운영한다.
  같은 컨테이너의 Pie 채팅 경로에는 필요하지 않다.

권장 상태는 다음과 같다.

```text
PROVISIONING
WAITING_FOR_CLAUDE_AUTH
WAITING_FOR_KROOT_AUTH
STARTING
READY
RELAY_CONNECTED
DEGRADED
AUTH_EXPIRED
STOPPED
FAILED
```

자격이 아직 준비되지 않았다는 이유로 컨테이너를 무한 crash loop시키지 않는다.
대기 상태를 명시적으로 보고하고 인증이 갱신되면 Controller가 시작을 재개한다.

## 8. 사용자 격리와 공유 접속의 경계

관리형 사용자 컨테이너는 가입 사용자 본인에게만 할당한다. 사용자 B는 사용자 A의
컨테이너, 대화 ID, PAT 또는 Workspace를 조회하거나 제어할 수 없다. BFF는 로그인
사용자의 `externalUserId`를 서버에서 확정하고, Manager는 Integration User와
Conversation 소유권을 매 요청마다 다시 확인한다.

Viewer/Controller 공유는 사용자가 명시적으로 공유한 Host OS 세션을 위한 별도
기능이다. 관리형 사용자 컨테이너의 소유권을 공유 권한으로 우회하지 않는다. 컨테이너
안의 외부 프로그램이 보는 정체성은 언제나 해당 컨테이너의
`.kroot/credential.json` 사용자다.

## 9. 현재 구현과 추가 구현 범위

### 현재 존재하는 기반

- 사용자 ID별 Executor 단일 프로비저닝과 중복 요청 합치기
- 사용자별 Workspace와 Home 상태 bind mount
- non-root UID/GID `10001:10001`
- read-only root filesystem, capability drop, `no-new-privileges`, 자원 제한
- `tini`, Pie `clientd`, Node Executor와 PTY Host
- Kroot ADK/Proto 소스에서 빌드한 Linux `kroot` CLI 오버레이 이미지
- `/home/executor/.claude`를 사용하는 Claude 설정 경로
- 보호된 Claude seed의 사용자별 최초 1회 복제
- Event Manager Claude 로그인 후보의 불변 버전 관리·전체 원자 배포·rollback
- Executor 생성과 채팅 세션 시작 전 활성 Claude 인증 버전 자동 조정
- 관리 UI의 버전·사용자별 배포·컨테이너 내부 검증 상태 조회
- Integration별 credential 경로와 원자적 `0600` 저장
- Kroot PAT JSON 스키마, 고유 `deviceId`, 사용자별 `.kroot/credential.json`
- 컨테이너 재생성과 기존 Home·Workspace·인증 상태 보존
- 웹 회원가입 → 사용자 할당 → Azure Pie Relay → Claude 응답 E2E

### 추가로 구현해야 할 항목

1. 실제 외부 인증 서버의 PAT 발급·폐기·introspection 연결
2. Claude 인증 버전 저장소 자체의 외부 Secret Store/Key Vault 봉인
3. 인증 만료 사전 감지와 알림 채널 연결
4. PAT 갱신·폐기와 사용자 탈퇴 수명주기 자동화
5. volume encryption, backup 제외/암호화 정책
6. 이미지와 build cache에 secret이 없음을 검사하는 release gate

현재 이미지의 기본 `CMD`는 의도대로 `pie-client start`만 실행한다.
Kroot CLI와 credential은 준비되어 있지만 Kroot Relay는 자동 연결하지 않는다.

## 10. 필수 검증 시나리오

1. 사용자 A/B의 Home과 Workspace가 서로 다른 실제 경로와 inode를 사용하는가.
2. 두 컨테이너의 Claude 인증 계정은 같지만 `.claude` 파일 쓰기가 서로 영향을
   주지 않는가.
3. Kroot Relay introspection 결과가 A/B 각각의 실제 사용자 ID와 일치하는가.
4. 각 컨테이너의 Kroot `deviceId`가 유일한가.
5. 컨테이너 삭제·재생성 후 인증, `deviceId`, workspace가 유지되는가.
6. 동일 webhook 재전송이 컨테이너나 volume을 중복 생성하지 않는가.
7. PAT 만료·폐기 시 외부 호출과 Relay 재접속이 차단되고 상태가 표시되는가.
8. PAT 갱신 중 파일을 읽어도 부분 JSON이나 빈 토큰이 관찰되지 않는가.
9. 이미지, build cache, 로그, operation payload, backup에 PAT나 Claude secret이
   포함되지 않는가.
10. Pie 세션 매니저가 네트워크 단절, Relay 재연결과 종료 신호를 올바르게 처리하는가.
