# Pie Executor Manager

인증된 사용자별 Docker Executor를 만들고, 작업과 파일을 격리해 실행하는 Go
서비스이자 Pie Relay Control Plane이다. Relay와 독립 프로세스로 배포하고 scoped JWT,
presence 및 운영 control API로 연동한다.

Executor 이미지는 정식 `pie-client` 바이너리와 Node.js, `node-executor`, 고정된 npm 의존성을 모두
포함하며 컨테이너 시작 시 `pie-client start`를 자동 실행한다. `pie-relay-client:latest`는 기존 배포와
레지스트리 호환을 위한 이미지 태그다.

## 빌드 및 실행

저장소 루트에서 Executor 이미지를 먼저 빌드한다.

```sh
docker build \
  -f executor-manager/Dockerfile.executor \
  -t pie-relay-client:latest .
```

Kroot CLI까지 포함하는 실제 사용자용 이미지는 Kroot ADK와 Proto를 Linux 바이너리로
빌드하는 오버레이 스크립트를 사용한다. macOS에서 `go install`한 바이너리를 이미지에
복사하지 않는다.

```sh
KROOT_ADK_DIR=/absolute/path/to/kroot-adk \
KROOT_PROTO_DIR=/absolute/path/to/kroot-proto \
node scripts/dev/build-kroot-executor-image.mjs
```

결과 이미지는 기본적으로 `pie-relay-client-kroot:local`이다. Manager에는
`PIE_EXECUTOR_IMAGE=pie-relay-client-kroot:local`을 지정한다. Kroot 및 Proto revision은
image label에 기록되며, Kroot PAT와 Claude 인증은 여전히 이미지 레이어에 포함되지
않는다.

Kroot 자체는 바뀌지 않고 Pie Client/Node Executor만 긴급 교체하는 배포에서는 이미
검증한 Kroot 이미지의 바이너리를 새 Pie base에 결합할 수 있다. 이 방식은 원본 이미지
태그를 label에 남기며, Kroot 변경 릴리스에는 사용하지 않는다.

```sh
docker build \
  -f executor-manager/Dockerfile.executor-kroot-binary-overlay \
  --build-arg PIE_EXECUTOR_BASE_IMAGE=pie-relay-client:<release> \
  --build-arg PIE_KROOT_SOURCE_IMAGE=pie-relay-client-kroot:<validated> \
  -t pie-relay-client-kroot:<release> .
```

Manager는 `executor-manager/`에서 실행한다.

```sh
PIE_EXECUTOR_MANAGER_TOKEN=dev-secret \
PIE_EXECUTOR_IMAGE=pie-relay-client-kroot:local \
go run ./cmd/manager
```

기본 주소는 `:19090`이다. 정적 토큰은 로컬 운영용 관리자 자격증명이다. 실제
사용자 PAT는 `PIE_AUTH_INTROSPECTION_URL`을 설정한다. introspection 응답은
`{"active":true,"sub":"user-id","organizationId":"org-id"}` 형식이어야 하며,
일반 사용자는 자신의 `sub`와 같은 `userId`만 접근할 수 있다.

사용자 ID는 Docker 이름과 저장 경계로 사용되므로 영문자, 숫자, `.`, `_`, `-`만
허용한다. 외부 서비스의 `sub`가 이메일이나 `auth0|...` 형식이면 외부 인증 계층에서
안전한 내부 사용자 ID(UUID 등)로 매핑해야 한다.

## API

- `PUT /v1/users/{userId}/executor`: 사용자 Executor 확보
- `POST /v1/users/{userId}/uploads`: `multipart/form-data`의 `file` 업로드
- `DELETE /v1/users/{userId}/uploads?ref={blobRef}`: 업로드 삭제
- `POST /v1/users/{userId}/jobs`: `command`, 선택적 `blobRefs` 제출
- `GET /v1/jobs/{jobId}`: 작업 상태 조회
- `GET /v1/jobs/{jobId}/events`: 상태와 출력 delta를 SSE로 수신
- `POST /v1/jobs/{jobId}/cancel`: 대기/실행 작업 취소
- `GET /healthz`: 프로세스 liveness
- `GET /readyz`: Docker daemon readiness
- `GET /metrics`: Prometheus 형식 지표
- `GET /admin/`: 내장 Pie Admin Web
- `/v1/admin/*`: 사용자, 장치, runtime, session, participant, grant, operation, audit
- `/v1/control/*`: 장치 heartbeat, session credential, Relay trusted presence
- `POST /v1/hooks/users`: 외부 회원 서비스의 서명된 사용자 수명주기 이벤트
- `/v1/admin/integrations*`: 제3자 Integration 등록, quota, token 교체·폐기
- `/v1/integrations/{id}/users/*`: service token 기반 사용자 전용 Executor provisioning
- `/v1/integrations/{id}/users/{externalUserId}/projects*`: 멱등 Project 생성·조회와 컨테이너 `kroot init`
- `/v1/integrations/{id}/conversations/*`: 멱등 chat, SSE/polling event, permission, cancel, 동일 Conversation 복구 retry

일반 `/v1` 요청과 `/metrics`는 `Authorization: Bearer <token>`이 필요하다. 사용자
webhook은 bearer 대신 timestamp가 포함된 HMAC 서명을 검증한다. SSE는 상태 변경 때만
전송하고 15초마다 heartbeat를 보낸다. 출력의 `[]byte` 필드는 JSON에서 base64로
표현된다.

## 실행 격리

Executor는 다음 제한으로 생성된다.

- 사용자당 대기 또는 실행 작업 1개
- 기본 CPU 2, 메모리 2GiB, PID 256; 사용자 quota가 있으면 해당 값을 우선 적용
- capability 전체 제거, `no-new-privileges`, 읽기 전용 root filesystem
- `/tmp`만 256MiB `tmpfs`; 사용자 workspace와 state만 쓰기 가능
- blob은 `/workspace/input`에 읽기 전용 마운트
- Manager 인스턴스 라벨과 컨테이너 이름으로 orphan 정리 범위 격리
- 기존 컨테이너의 사용자·Manager·이미지 라벨이 다르면 재사용 거부

Manager가 root로 실행될 때 Executor의 기본 사용자는 안전한 `10001:10001`이다.
Manager가 비루트이면 그 UID/GID를 사용하며, 운영에서는
`PIE_EXECUTOR_CONTAINER_USER=uid:gid`로 명시하는 것을 권장한다. 기존 root 소유의
사용자 디렉터리는 Executor 준비 과정에서 지정 사용자 소유의 `0700` 경계로
마이그레이션된다.

Claude 인증정보는 이미지나 사용자별 HOME에 넣지 않는다. Event Manager의 중앙
Credential Broker가 `claude setup-token` 결과를 AES-GCM 암호화·버전 관리하고,
Docker 채팅 세션 시작 시 Claude Code 하위 프로세스에만 전달한다. 기존
`.claude/.credentials.json`은 활성 구독 OAuth가 준비된 뒤 제거한다.
Claude Code에는 `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`과 구독 전용 상위 설정을 함께
적용해 Bash·Hook·stdio MCP가 OAuth/API/provider 자격을 상속하지 못하게 한다.
이미지에는 Agent SDK와 동일 버전의 네이티브 Claude Code를 호출하는 `claude`
명령이 포함된다. workspace는 `/workspace`, Claude 설정은
`/home/executor/.claude`에 사용자별로 유지된다.

제3자 AI 채팅의 Project는 `/workspace/projects/{opaque-project-id}`에 생성된다. Manager가
서버에서 경로를 확정하고 컨테이너 내부의 실제 `kroot init`이 성공한 뒤에만 Project를
`ready`로 바꾼다. Conversation 생성에는 이 `projectId`가 필수이며, Controller와 clientd가
해당 경로를 Claude Agent SDK의 cwd로 끝까지 전달한다. 따라서 Project 기능을 사용하는
Manager의 `PIE_EXECUTOR_IMAGE`에는 실행 가능한 `kroot`가 반드시 포함되어야 한다.

Pie Admin의 사용자 화면에서 CPU, 메모리(MiB), PID, 최대 세션과 최대 접속자 quota를
등록·변경할 수 있다. CPU·메모리·PID 변경은 다음 Control Plane 조정 주기에 기존
Executor에도 `docker update`로 적용되고 runtime 레코드에 투영된다. 전용 Executor가
아직 없으면 최초 생성 시 적용된다. `diskBytes`는 Workspace, Home, 업로드 blob의 합계를
주기적으로 검사한다. 초과 사용자는 `quota_exceeded`로 바꾸고 컨테이너를 정지하며, 다음
실행도 HTTP 507로 거부한다. 노드의 가용 공간이 reserve보다 작으면 신규 실행은 HTTP
503으로 거부한다.

이 보호는 현재 파일시스템의 **감시형 quota**다. 검사 사이에 발생하는 추가 쓰기까지
즉시 막는 커널 hard quota는 아니다. ext4/XFS project quota가 준비된 전용 volume으로
이전하기 전에는 악의적인 대용량 쓰기를 완전히 차단하는 경계로 간주하면 안 된다.

## 저장과 backpressure

기본 registry는 `var/registry/{executors,jobs}/*.json`에 레코드별로 원자 저장한다.
작업 하나가 변경될 때 전체 상태를 다시 쓰지 않는다. 과거 단일 JSON이 필요하면
`PIE_EXECUTOR_MANAGER_STATE=/path/manager.json`을 지정할 수 있지만 신규 운영에는
권장하지 않는다. 하나의 registry와 Manager ID는 동시에 한 프로세스만 소유해야 한다.

- 명령: 최대 256KiB
- 작업 출력: 최대 4MiB
- 업로드 파일: 최대 64MiB
- 사용자별 blob: 기본 최대 1GiB
- 완료 작업: 기본 최근 1,000개만 보존
- 작업 queue: 기본 64, worker 4
- Docker 동시 프로비저닝: 기본 4
- 동시 업로드: 기본 8
- SSE 연결: 기본 256

queue, 업로드, SSE 한도를 넘으면 `503` 또는 `429`로 backpressure를 반환한다.
Docker runtime과 활성 session 상태 조회는 기본 8개 worker로 병렬화하고, 변하지 않은
runtime/device 상태는 1분 단위로 합쳐 PostgreSQL write amplification을 제한한다.
세션 시작은 1초마다 감지하지만 실행 중 session의 무거운 Docker 상태 조회는 일반
reconcile 주기에만 수행한다. 다중 Manager의 PostgreSQL 동기화는 레코드별 변경
cursor를 사용하므로 매 조정마다 전체 JSON snapshot을 다시 읽지 않는다. 변경 테이블은
같은 레코드의 최신 상태만 보관해 heartbeat가 누적 로그로 증폭되지 않는다. Pie Admin도
현재 메뉴의 최대 500개 레코드만 지연 로딩하고, 연속 SSE 변경 알림은 최소 2초 단위로
합쳐 전체 상태 재전송이 운영 경로를 압박하지 않게 한다.

## 환경변수

| 변수 | 기본값 | 설명 |
|---|---:|---|
| `PIE_EXECUTOR_MANAGER_ADDR` | `:19090` | HTTP listen 주소 |
| `PIE_EXECUTOR_MANAGER_TOKEN` | 없음 | 로컬 관리자 bearer token |
| `PIE_RELAY_PRESENCE_TOKEN` | 없음 | Relay presence 전용 최소권한 token |
| `PIE_AUTH_INTROSPECTION_URL` | 없음 | 외부 PAT introspection endpoint |
| `PIE_AUTH_CLIENT_ID`, `PIE_AUTH_CLIENT_SECRET` | 없음 | introspection Basic 인증 |
| `PIE_AUTH_HTTP_TIMEOUT` | `5s` | introspection HTTP 요청 제한시간 |
| `PIE_AUTH_CACHE_TTL` | `30s` | 활성 PAT 결과 캐시 및 최대 폐기 반영 지연 |
| `PIE_AUTH_NEGATIVE_CACHE_TTL` | `5s` | 비활성·실패 PAT 결과 캐시 |
| `PIE_CORS_ALLOWED_ORIGINS` | 없음 | Desktop/Web에서 허용할 정확한 origin 목록 |
| `PIE_USER_WEBHOOK_SECRET` | 없음 | 사용자 lifecycle HMAC secret(32바이트 이상) |
| `PIE_USER_WEBHOOK_MAX_SKEW` | `5m` | webhook timestamp 허용 오차 |
| `PIE_EXECUTOR_MANAGER_ID` | `default` | Docker 소유 범위 ID |
| `PIE_EXECUTOR_IMAGE` | `pie-relay-client:latest` | Executor 이미지 |
| `PIE_EXECUTOR_REGISTRY_DIR` | `var/registry` | Executor/job 레코드 registry |
| `PIE_EXECUTOR_MANAGER_STATE` | 없음 | 레거시 단일 JSON 경로 |
| `PIE_EXECUTOR_BLOB_DIR` | `var/blobs` | 사용자 blob root |
| `PIE_EXECUTOR_WORK_DIR` | `var/workspaces` | 사용자 workspace root |
| `PIE_EXECUTOR_STATE_DIR` | `var/executor-state` | 사용자 HOME/state root |
| `PIE_EXECUTOR_STATE_SEED_DIR` | 미설정 | 신규 사용자 HOME에 최초 1회 복제할 보호된 공통 설정 seed. `.claude/.credentials.json`은 제외 |
| `PIE_KROOT_COMMON_BUNDLE_DIR` | 미설정 | 버전형 Kroot 공통 번들의 `current` 경로. `.claude/skills/*`와 `.claude/agents/*` 전체를 모든 사용자 HOME에 동기화 |
| `PIE_KROOT_COMMON_BUNDLE_VERSION` | 번들 내용 SHA-256 | 운영자가 지정한 Kroot ADK revision/릴리스 버전. 사용자 marker와 배포 감사에 기록 |
| `PIE_CLAUDE_AUTH_DIR` | `<state root의 상위>/claude-auth` | Event Manager의 암호화 구독 OAuth 버전 저장소 |
| `PIE_CLAUDE_AUTH_LOGIN_DIR` | `<PIE_CLAUDE_AUTH_DIR>/login` | 일회성 `setup-token` 후보 파일 경로 |
| `PIE_CLAUDE_AUTH_REQUIRED` | `false` | 활성 인증이 없으면 Executor 생성·세션 시작을 차단 |
| `PIE_CLAUDE_AUTH_ROLLOUT_CONCURRENCY` | `4` | OAuth 회전 시 Executor 세션 재조정 동시성 |
| `PIE_CLAUDE_SUBSCRIPTION_MAX_CONCURRENT_TURNS` | `4` | 전체 사용자 컨테이너에서 동시에 실행할 Claude 구독 턴 수. 초과 요청은 FIFO 대기 |
| `PIE_EXECUTOR_NETWORK` | `bridge` | Docker network |
| `PIE_EXECUTOR_CONTAINER_USER` | root Manager는 `10001:10001`, 그 외 Manager UID:GID | 컨테이너 실행 사용자 |
| `PIE_EXECUTOR_PERMISSION_MODE` | 미설정 | Docker Executor에 고정할 Claude 권한 모드. 격리 컨테이너 자동 실행은 `bypassPermissions`; Host OS에는 사용 금지 |
| `PIE_EXECUTOR_CPUS` | `2` | 사용자 CPU quota가 없을 때의 컨테이너 기본 제한 |
| `PIE_EXECUTOR_MEMORY` | `2g` | 사용자 메모리 quota가 없을 때의 컨테이너 기본 제한 |
| `PIE_EXECUTOR_MEMORY_SWAP` | 메모리 제한과 동일 | 동일 값이면 사용자 컨테이너의 swap 사용 차단 |
| `PIE_EXECUTOR_PIDS_LIMIT` | `256` | 사용자 PID quota가 없을 때의 컨테이너 기본 제한 |
| `PIE_EXECUTOR_DISK_QUOTA_BYTES` | `21474836480` | 사용자 Workspace·Home·blob 합산 기본 감시 quota(20GiB) |
| `PIE_EXECUTOR_DISK_HEADROOM_BYTES` | `5368709120` | 신규 실행을 중단할 노드 최소 가용 공간(5GiB) |
| `PIE_EXECUTOR_DISK_SCAN_INTERVAL` | `1m` | 사용자별 디스크 사용량 검사 주기 |
| `PIE_EXECUTOR_REQUIRE_ISOLATED_NETWORK` | `false` | 전용 bridge·외부 egress·ICC 차단을 시작 시 강제 검증 |
| `PIE_EXECUTOR_ALLOW_USER_NAMESPACES` | `false` | Linux Claude subprocess scrub용 rootless bubblewrap 허용. 해당 Executor에만 Docker seccomp와 system-path masking을 해제하며 비-root·cap-drop·읽기 전용 rootfs 등 외부 격리는 유지 |
| `PIE_EXECUTOR_WORKERS` | `4` | 작업 worker 수 |
| `PIE_EXECUTOR_QUEUE_CAPACITY` | `64` | 대기 queue 크기 |
| `PIE_EXECUTOR_PROVISION_CONCURRENCY` | `4` | 동시 컨테이너 생성 수 |
| `PIE_EXECUTOR_MAX_EXECUTORS` | `64` | 이 Manager Node에서 용량을 점유할 수 있는 Executor 상한; stopped 상태는 슬롯을 반납 |
| `PIE_EXECUTOR_JOB_TIMEOUT` | `30m` | 작업 timeout |
| `PIE_EXECUTOR_RETAINED_JOBS` | `1000` | 완료 작업 보존 개수 |
| `PIE_EXECUTOR_USER_BLOB_QUOTA_BYTES` | `1073741824` | 사용자별 blob quota |
| `PIE_EXECUTOR_UPLOAD_CONCURRENCY` | `8` | 동시 업로드 수 |
| `PIE_EXECUTOR_SSE_CONNECTIONS` | `256` | 동시 SSE 연결 수 |
| `PIE_EXECUTOR_SSE_LIFETIME` | `30m` | SSE 최대 연결 시간 |
| `PIE_CHAT_JOURNAL_DIR` | `<registry>/chat-journal` | 제3자 AI 대화 복구용 append-only event journal |
| `PIE_CHAT_JOURNAL_MAX_BYTES` | `67108864` | Conversation별 journal 최대 크기 |
| `PIE_CHAT_EVENT_MAX_BYTES` | `8388608` | 단일 AI 응답 event 최대 크기 |
| `PIE_CHAT_IDLE_SCAN_INTERVAL` | `1m` | 유휴 Conversation·Executor 검사 주기 |
| `PIE_CHAT_SESSION_IDLE_TIMEOUT` | `15m` | 마지막 요청·응답 이후 Claude/Relay 세션 자동 종료 기준 |
| `PIE_EXECUTOR_IDLE_TIMEOUT` | `1h` | 활성 세션이 없는 사용자 Executor 컨테이너 자동 정지 기준 |
| `PIE_EXECUTOR_KROOT_AUTO_LINK` | `false` | Kroot 전용 이미지에서 프로젝트 초기화 직후 사용자 PAT로 백엔드 프로젝트를 생성·연결. 연결 성공 전에는 Project를 ready로 표시하지 않음 |
| `PIE_CONTROL_DATABASE_URL` | 없음 | 운영 PostgreSQL DSN; 미설정 시 Directory Store |
| `PIE_USAGE_DATABASE_URL` | `PIE_CONTROL_DATABASE_URL` | Claude 모델별 토큰·비용 원장 PostgreSQL DSN; 둘 다 없으면 사용량 수집 비활성화 |
| `PIE_USAGE_RECONCILE_INTERVAL` | `1m` | 채팅 journal의 미적재 usage 이벤트를 DB에 재반영하는 주기 |
| `PIE_CONTROL_REGISTRY_DIR` | `var/control` | 개발용 Control Plane Directory Store |
| `PIE_CONTROL_RECONCILE_INTERVAL` | `10s` | desired/observed 상태 조정 주기 |
| `PIE_CONTROL_RECONCILE_CONCURRENCY` | `8` | Docker 상태 조회·조정 동시 처리 수 |
| `PIE_CONTROL_HEARTBEAT_TIMEOUT` | `45s` | Local device offline 판정 |
| `PIE_RELAY_HEARTBEAT_TIMEOUT` | `90s` | Relay node lease 만료 및 세션 재할당 기준 |
| `PIE_CONTROL_OPERATION_CONCURRENCY` | `4` | 동시 운영 operation 수 |
| `PIE_CONTROL_OPERATION_RETENTION` | `168h` | 완료 operation 보존 기간 |
| `PIE_DEFAULT_MAX_SESSIONS` | `8` | 신규 사용자의 기본 활성 세션 quota |
| `PIE_DEFAULT_MAX_PARTICIPANTS` | `32` | 신규 사용자의 세션별 기본 접속자 quota |
| `PIE_RELAY_URL` | CookAI Relay | Docker session Relay origin 또는 WebSocket URL. 로컬도 이 값만 변경 |
| `PIE_RELAY_PUBLIC_URL` | 없음 | Desktop credential 응답에 포함할 공개 Relay origin |
| `PIE_RELAY_JWT_SECRET` | 없음 | session-scoped capability 서명 secret |
| `PIE_RELAY_ROUTING_SECRET` | 없음 | tenant/resource를 노출하지 않는 불투명 room HMAC secret |
| `PIE_RELAY_DEFAULT_APPLICATION_ID` | `pie-control` | 문맥을 생략한 관리형 세션에 적용할 기본 Application scope |
| `PIE_RELAY_DEFAULT_POOL_ID` | `pie-relay-default` | 문맥을 생략한 관리형 세션에 적용할 기본 Relay Pool |
| `PIE_RELAY_CONTROL_URL` | presence 주소 | Relay 내부 control origin fallback |
| `PIE_RELAY_CONTROL_TOKEN` | 없음 | Relay 연결/driver operation bearer token |

Application/Tenant/Resource 문맥이 있는 신규 세션은
`PIE_RELAY_ROUTING_SECRET`이 반드시 필요하다. 이 값은 JWT 서명 secret과 별도로
생성하며, 변경하면 같은 세션의 room이 달라지므로 계획된 유지보수 때만 회전한다.
운영 Manager는 모든 관리형 Relay 세션에 기본 문맥을 적용하므로 이 secret 없이
기동하지 않는다. 기존 무문맥 세션은 reconcile 때 한 번만 기본 문맥으로 승격된다.

Relay node heartbeat가 제한 시간을 넘기면 Control Plane은 세션을 `reconnecting`으로
전환하고 `relayGeneration`을 증가시킨다. Host OS와 Docker Session Manager는 기존 연결을
종료한 뒤 새 노드·새 capability로 다시 시작한다. Relay의 검증된 routing key와 Presence에도
같은 세대가 포함되므로 이전 토큰이나 늦게 도착한 heartbeat가 복구된 세션을 되돌릴 수 없다.
PostgreSQL 배포에서는 pool별 advisory lock 안에서 최신 상태를 다시 읽고 노드를 선택해,
여러 Manager replica가 동시에 같은 잔여 용량을 배정하는 것을 방지한다.

## 검증

```sh
go test ./...
go test -race ./...
```

Docker 통합 검증은 이미지 빌드 후 실제 API로 Executor를 생성하고, `/readyz`, blob
마운트, 명령 실행, SSE 완료 이벤트와 `docker inspect`의 보안 제한을 확인한다.
`deploy/local/pie-local.sh test`는 추가로 임시 Manager와 테스트 Agent 이미지를
구동해 Integration 등록, 사용자별 credential/컨테이너, Project별 실제 `kroot init`과 cwd,
Relay 채팅, permission,
Relay·Manager 재시작 복구, A/B 격리와 탈퇴 정리까지 실제 Docker E2E로 검증한다.
승인된 chat은 journal에 fsync한 뒤 전달하고 terminal marker 전 Manager가 재시작하면
재전송한다. 이 경로는 at-least-once이므로 외부 API 부수 효과는 request/job ID로
멱등 처리해야 한다. clientd가 살아 있는 Manager/Relay 재시작은 bounded replay cache가
같은 request ID의 Claude 중복 실행을 막고 응답을 다시 전달한다.

## 외부 회원 lifecycle webhook

본문 JSON은 `id`, `type`, RFC3339 `occurredAt`, `user.id`를 포함한다. 지원 type은 `user.created`,
`user.updated`, `user.reactivated`, `user.suspended`, `user.deleted`다. 생성 이벤트에
`"provision": true`를 넣으면 응답 전에 사용자 Executor를 확보한다. 정지·탈퇴는
Executor를 즉시 중지하고 소유 장치별 idempotent drain operation을 등록한다.
마지막 이벤트 ID와 발생 시각은 사용자 레코드에 영속화된다. 중복 ID는 다시
적용하지 않고 마지막 적용 시각보다 오래된 이벤트는 무시하므로 이벤트 전달 순서가
뒤집혀도 탈퇴 사용자가 과거 가입 이벤트로 재활성화되지 않는다.

보내는 쪽은 Unix 초를 `X-Pie-Timestamp`에 넣고 아래 값을 HMAC-SHA256으로 계산해
hex 형식의 `X-Pie-Signature: v1=<digest>`로 전송한다.

```text
signed_payload = X-Pie-Timestamp + "." + raw_request_body
```
