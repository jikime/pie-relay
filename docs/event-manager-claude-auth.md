# Event Manager 기반 Claude 구독 OAuth 운영

이 문서는 하나의 Event Manager가 Claude Code 구독 인증을 중앙에서 관리하고,
여러 사용자 Executor 컨테이너의 채팅 세션에 안전하게 적용하는 구조와 운영 절차를
설명한다. Anthropic API 키나 API 과금 경로는 사용하지 않는다.

## 1. 최종 구조

```text
운영자
  └─ Event Manager 서버에서 `claude setup-token`
       └─ setup-token 후보(0600, 게시 후 즉시 삭제)
            └─ Manager Credential Broker
                 ├─ AES-256-GCM 암호화 버전 저장
                 ├─ 활성/직전 버전 및 교체 시점 관리
                 └─ Docker 채팅 세션 시작 시에만 복호화
                      └─ docker exec -i의 JSON body
                           └─ container-local clientd 메모리
                                └─ 전용 1회성 FD
                                     └─ Node Executor 메모리
                                          └─ SDK options.env
                                               └─ Claude Code 하위 프로세스
```

다음 경로에는 Claude 구독 토큰을 넣지 않는다.

- Docker image와 Compose 환경변수
- `docker inspect`로 보이는 `Config.Env`
- 프로세스 argv와 Relay JWT
- 관리자 API 요청·응답과 감사 로그
- 사용자별 `~/.claude/.credentials.json`
- Relay를 통과하는 채팅 메시지

외부 서비스의 사용자 PAT는 별개의 자격정보다. PAT는 Integration 정책에 따라
사용자별 `~/.kroot/credential.json` 또는 `~/.pie/credential.json`에 저장할 수 있지만,
Claude 구독 OAuth와 섞지 않는다.

## 2. 왜 인증 파일을 공유하지 않는가

`claude auth login`으로 생성되는 `.credentials.json`에는 갱신 가능한 OAuth 상태가
포함될 수 있다. 같은 파일을 여러 컨테이너에 복제하면 각 컨테이너가 동시에 갱신을
시도하고, 한쪽에서 회전한 토큰을 다른 쪽이 오래된 값으로 덮어쓰는 경쟁이 생긴다.

현재 구조는 자동화 환경용 `claude setup-token`을 중앙에 하나만 보관한다. 사용자
컨테이너는 로그인·로그아웃·토큰 갱신을 수행하지 않고, 실제 Claude Code 프로세스가
시작될 때 활성 토큰을 읽기 전용으로 받는다. 따라서 컨테이너 수가 늘어도 갱신 파일
경쟁이 발생하지 않는다.

## 3. 중앙 저장소

```text
${PIE_CLAUDE_AUTH_DIR}/
├─ master.key                              # 32바이트, mode 0600
├─ active.json                             # 활성·직전 버전 포인터
├─ login/
│  └─ setup-token                          # 게시 전 후보, 게시 성공 후 삭제
└─ versions/
   └─ v-<time>-<fingerprint>/
      ├─ oauth-token.enc                   # AES-256-GCM 암호문
      └─ metadata.json                     # secret 없는 버전 정보
```

디렉터리는 `0700`, 파일은 `0600` 이하를 강제한다. symlink·특수 파일·공백이나 제어
문자가 포함된 토큰·과도하게 큰 입력은 거부한다. `master.key`를 잃으면 저장된 토큰을
복호화할 수 없으므로 Manager 데이터 백업에 포함하되, 별도의 제한된 암호화 백업으로
관리한다.

버전 메타데이터에는 생성 시각, SHA-256 지문, 권장 교체일(게시 후 330일), 예상
만료일(게시 후 365일)을 기록한다. 실제 유효성은 달력만으로 판단하지 않고 정기적인
canary 채팅 요청으로 확인한다.

## 4. 토큰 발급과 게시

Event Manager 호스트에서 실행한다.

```bash
PIE_DATA_DIR=/home/kaonkroot/pie-sandbox-test/data \
PIE_EXECUTOR_IMAGE=pie-relay-client-kroot:test \
PIE_MANAGER_URL=https://admin-relay-test.cookai.dev \
PIE_MANAGER_ADMIN_TOKEN_FILE=/보호된/manager-admin-token \
./scripts/ops/claude-auth-login.sh
```

스크립트의 동작 순서는 다음과 같다.

1. 실제 Executor 이미지에서 `claude setup-token`을 실행한다.
2. 운영자가 발급 결과를 숨김 입력으로 붙여넣는다.
3. 토큰을 argv나 일반 임시 파일에 두지 않고 stdin으로 후보 파일에 기록한다.
4. `POST /v1/admin/claude-auth/publish`는 토큰 원문이 아니라 게시 명령만 받는다.
5. Manager가 후보를 읽어 AES-GCM 암호화 버전을 만든 뒤 후보를 삭제한다.
6. 실행 중 Executor를 제한된 동시성으로 재시작한다.
7. Controller가 복구한 새 채팅 세션부터 활성 버전을 사용한다.

후보만 만들고 게시를 미루려면 `PIE_CLAUDE_AUTH_PUBLISH=false`를 사용한다. 이 경우
후보 파일이 남으므로 가능한 한 빨리 관리자 화면에서 게시하거나 안전하게 제거한다.

기존 사용자별 인증에서 새 Broker 이미지로 처음 전환할 때는 발급과 Manager 교체의
순서가 중요하다. 준비된 릴리스가 있다면 다음 전환 스크립트를 Event Manager 서버의
대화형 터미널에서 실행한다.

```bash
PIE_RELEASE_DIR=/home/kaonkroot/pie-sandbox-test/releases/<준비된-릴리스> \
PIE_MANAGER_ADMIN_URL=https://admin-relay-test.cookai.dev \
PIE_MANAGER_HEALTH_URL=https://api-relay-test.cookai.dev/readyz \
./scripts/ops/claude-auth-broker-cutover.sh
```

이 스크립트는 구형 인증 저장소와 현재 릴리스를 먼저 백업한다. OAuth 승인·코드 입력
뒤 새 Manager를 시작하고, 중앙 버전을 게시하면서 실행 중인 소유 Executor를 새 이미지로
재생성한다. 사용자별 workspace와 HOME bind mount는 유지한다. 게시 전에 실패하면 기존
Manager를 자동 복구하지만, 게시 요청이 시작된 뒤에는 새 저장 형식을 구형 Manager가
읽지 못하므로 자동 코드 롤백을 수행하지 않는다.

## 5. 실제 채팅 흐름

```text
웹 사용자 로그인
  → 사용자/프로젝트/대화 소유권 확인
  → 전용 Executor Ensure
      → 활성 OAuth 버전 존재 여부 확인
      → 과거 ~/.claude/.credentials.json 제거
  → Control Session 준비
  → Controller가 Relay JWT 발급
  → Broker가 활성 setup-token을 메모리로 복호화
  → SessionSpec을 docker exec -i 표준입력으로 전달
  → clientd가 토큰을 상태 응답에서 제외하고 세션 메모리에 보관
  → Node Executor 시작 시 전용 FD로 한 번 전달하고 FD 닫기
  → Claude Agent SDK가 Claude Code child 전용 env 구성
      → CLAUDE_CODE_OAUTH_TOKEN=<활성 setup-token>
      → ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN/gateway/provider env를 빈 값으로 고정
      → 상위 SDK settings 계층에서 apiKeyHelper와 cloud credential helper 차단
      → CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1
          → Bash/Hook/stdio MCP에서 Anthropic·cloud credential 제거
          → Linux 하위 프로세스에 PID 경계 적용
  → Claude Code 구독 인증으로 응답 스트리밍
```

일반 터미널 세션과 Codex ACP 세션에는 Claude OAuth를 전달하지 않는다. Manager의
Relay 토큰과 Claude OAuth는 서로 다른 자격이며, 둘 다 세션 상태 API에 나타나지 않는다.

## 6. 관리 API와 화면

| API | 역할 |
|---|---|
| `GET /v1/admin/claude-auth` | 버전·교체일·세션 사용 가능 상태 조회 |
| `POST /v1/admin/claude-auth/publish` | setup-token 후보를 암호화 버전으로 게시 |
| `POST /v1/admin/claude-auth/deploy` | 현재 버전으로 실행 중 세션 재조정 |
| `POST /v1/admin/claude-auth/rollback` | 직전 구독 OAuth 버전 복구 |

화면과 API에는 토큰 원문, 암호문, nonce, master key를 반환하지 않는다. 지문도 앞부분만
화면에 표시한다. 변경 API는 관리자 권한과 idempotency key를 요구하고, 감사 로그에는
버전 ID·대상 수·성공/실패 수만 남긴다.

## 7. 환경변수

| 변수 | 권장값 | 설명 |
|---|---|---|
| `PIE_CLAUDE_AUTH_DIR` | `${PIE_DATA_DIR}/claude-auth` | 암호화 Broker 저장소 |
| `PIE_CLAUDE_AUTH_LOGIN_DIR` | `${PIE_CLAUDE_AUTH_DIR}/login` | 일회성 setup-token 후보 경로 |
| `PIE_CLAUDE_AUTH_REQUIRED` | `true` | 활성 OAuth가 없으면 Executor/채팅을 fail-closed |
| `PIE_CLAUDE_AUTH_ROLLOUT_CONCURRENCY` | `2~4` | 세션 재조정용 Executor 재시작 동시성 |
| `PIE_CLAUDE_SUBSCRIPTION_MAX_CONCURRENT_TURNS` | `4` | Manager 전체 동시 Claude 턴 수. 초과 요청은 FIFO 대기 |

`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`은 Compose
환경변수로 설정하지 않는다. 특히 Docker 컨테이너 전체 환경에 setup-token을 넣으면
`docker inspect`와 컨테이너 내 모든 프로세스에서 보일 수 있다.

## 8. 회전·장애 복구

- 구형 인증에서 첫 전환: 기존 사용자별 `.credentials.json` 버전이 활성 포인터에 남아
  있으면 상태 API는 `migrationPending: true`를 반환한다. 이 상태에서는 구형 버전으로
  rollback하지 않으며, 새 setup-token을 게시하면 중앙 OAuth 버전만 활성화하고 구형
  포인터를 안전하게 제거한다.
- 정기 회전: 권장 교체일 전에 새 setup-token을 발급하고 게시한다.
- 만료 차단: 게시 후 365일이 지나면 Broker는 해당 버전을 복호화해도 세션에 전달하지
  않고 fail-closed한다. 관리자 화면은 `configured`와 `available`을 구분해 만료 상태를
  표시한다.
- canary 실패: 전체 재조정 전에 테스트 사용자 대화를 하나 실행해 실제 응답을 확인한다.
- 새 버전 실패: `rollback`으로 직전 OAuth 버전을 활성화하고 세션을 재조정한다.
- Manager 재시작: `master.key`, `active.json`, 활성 암호문이 함께 있으면 복구된다.
- master key 유실: 기존 암호문은 복구할 수 없다. 새 setup-token을 발급해 새 저장소를
  구성한다.
- 실행 중 대화: OAuth 회전 시 컨테이너 재시작으로 현재 턴이 중단될 수 있으므로 사용량이
  적은 시간에 수행하고, 제3자 앱은 기존 request ID로 재조회·재시도한다.

## 9. 보안상 남는 경계

Claude Code 프로세스 자체는 인증을 위해 `CLAUDE_CODE_OAUTH_TOKEN`을 환경에서 읽는다.
현재 Executor는 Claude Code 공식 `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`을 강제해 Bash,
Hook, stdio MCP 하위 프로세스에서 Anthropic·cloud credential을 제거한다. Linux에서는
하위 프로세스가 `/proc`으로 부모 환경을 읽지 못하도록 PID 경계도 적용된다. 외부 사용자가
임의 코드를 실행할 수 있는 다중 테넌트 환경에서는 다음을 함께 유지한다.

- 전용 비-root UID, read-only root filesystem, capability drop, PID/CPU/메모리 제한
- Linux Executor 이미지의 `bubblewrap` 설치와 비권한 사용자 namespace 동작 확인
- Docker 기본 seccomp가 rootless namespace를 차단하는 노드에서는
  `PIE_EXECUTOR_ALLOW_USER_NAMESPACES=true`를 Executor에만 적용한다. 이 옵션은
  seccomp를 해제하므로 `cap-drop ALL`, 비루트 UID, `no-new-privileges`, read-only root,
  전용 네트워크와 bubblewrap 하위 프로세스 격리를 반드시 함께 유지한다.
- 사용자별 workspace/state volume과 ICC가 꺼진 전용 Docker network
- SDK 상위 설정에서 임의 `apiKeyHelper`, API base URL, gateway/provider override 차단
- 로그·오류의 OAuth/Relay token redaction
- Claude Code/Agent SDK 버전 변경 때마다 `env`, `/proc/*/environ`, Hook/Bash 상속 여부
  보안 테스트 재실행

또한 제3자 고객에게 제공하는 상용 형태는 Claude 구독 계정의 허용 범위 및 Anthropic
약관을 별도로 확인해야 한다. 이 기술 구조가 서비스 제공 권한까지 대신 보장하지는 않는다.

## 10. 완료 검증표

1. `oauth-token.enc`에 setup-token 평문이 없는가.
2. 후보 `login/setup-token`이 게시 성공 후 삭제되는가.
3. 사용자 HOME에 `.claude/.credentials.json`이 생성·복제되지 않는가.
4. `docker inspect`의 Env와 프로세스 argv에 토큰이 없는가.
5. Manager 상태 API·SSE·감사 로그·오류에 토큰이 없는가.
6. 일반 terminal/ACP 세션에는 Claude OAuth가 전달되지 않는가.
7. 웹 로그인 → 전용 컨테이너 → Relay → Claude Code → 스트리밍 E2E가 성공하는가.
8. 두 사용자 동시 요청에서 인증 오류나 토큰 갱신 경쟁이 없는가.
9. 회전 후 새 세션이 새 버전을 사용하고 rollback이 복구되는가.
10. 실제 Bash/Hook/stdio MCP에서 토큰 원문을 읽을 수 없고 `/proc` 우회도 차단됐는가.

운영 전환은 새 Manager/Executor 이미지를 별도 태그로 먼저 빌드한 뒤, setup-token을
준비하고 짧은 점검 시간에 수행한다. 새 토큰이 준비되지 않은 상태에서 Manager만 먼저
교체하면 `PIE_CLAUDE_AUTH_REQUIRED=true`에 의해 새 채팅 세션은 의도적으로 차단된다.
따라서 이미지 준비 → setup-token 후보 준비 → Manager/Executor 교체 → 즉시 게시 →
canary 대화 → 다중 사용자 동시 대화 순서를 지킨다.
