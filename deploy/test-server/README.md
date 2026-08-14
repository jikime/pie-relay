# 공유 테스트 서버 배포

Wildcard TLS 발급·적용, 프로젝트 실행·삭제, 장애 확인과 롤백은
[`../../docs/preview-ssl-runtime-runbook.md`](../../docs/preview-ssl-runtime-runbook.md)에
운영 Runbook으로 정리되어 있다.

이 디렉터리는 `221.143.48.77`의 기존 Kroot/Traefik과 공존하는 **Kroot Studio
검증 스택**이다. 자체 Traefik과 host port를 만들지 않으며 외부 통신은 기존
`kroot-shared-edge-network`를 통해서만 받는다. Vibe Canvas는 같은 호스트에서
`pie-relay-pie-canvas`라는 별도 Compose project로 실행한다.

사용자 Executor는 컨테이너 간 통신(ICC)이 꺼진 별도 bridge에 들어가고 Relay와
Docker network를 공유하지 않는다. clientd는 `.env`의 `PIE_EXECUTOR_RELAY_URL`로
TLS 연결하며 Kroot Studio 검증값은 `wss://relay-test.cookai.dev/ws/agent`다. 따라서 같은 Node의
다른 사용자 컨테이너를 내부 IP로 직접 호출할 수 없다.

## 안전 원칙

- Compose project는 항상 `pie-sandbox-test`를 사용한다.
- 기존 Compose project, container, network, volume을 중지하거나 정리하지 않는다.
- 공유 서버에서 Docker daemon 재시작, host 재부팅, disk-fill, OOM 시험을 하지 않는다.
- `.env`와 Claude/PAT credential은 Git과 명령 출력에 남기지 않는다.
- 현재 한계 시험용 admission 한도는 동시 활성 Executor 15개다.
- `PIE_EXECUTOR_MAX_EXECUTORS=15`를 넘는 provisioning은 `429`로 거부한다.
- 15개는 컨테이너 생성·할당 한계 시험값이다. 각 Executor의 메모리 상한은 예약량이
  아니지만, 15명이 Claude를 동시에 고부하로 사용하면 host OOM이 발생할 수 있으므로
  실사용 안전 용량으로 간주하지 않는다.
- Web Chat Integration의 `maxUsers`는 Executor 한도 이상이어야 한다. 배포 전 보정
  스크립트로 이 조건을 멱등적으로 확인한다.

## 배포 개요

```bash
ssh pie-sandbox-test
cd /home/kaonkroot/pie-sandbox-test/src/deploy/test-server
docker network create --driver bridge \
  --opt com.docker.network.bridge.enable_icc=false \
  pie-sandbox-test-executor
docker compose --env-file .env config --quiet
docker compose --env-file .env build relay manager
docker compose --env-file .env up -d
set -a; source .env; set +a
PIE_MANAGER_ADMIN_URL=https://api-relay-test.cookai.dev \
  ./reconcile-web-chat-integration.sh
docker compose --env-file .env ps
```

Executor 이미지는 저장소 root의 `Dockerfile.executor`와 Kroot named context를
사용해 `linux/amd64`로 먼저 빌드한다. 상세 명령과 검증 결과는
`docs/sandbox-test-server-plan.md`에 기록한다.

Kroot Executor image를 빌드한 뒤 Manager를 올리기 전에, 같은 이미지에 포함된 공통
skills와 agents를 versioned bundle로 추출한다. `PIE_KROOT_COMMON_BUNDLE_VERSION`에는
이미지의 `ai.pielab.kroot-adk-revision` label과 같은 값을 사용한다.

```bash
../../scripts/ops/prepare-kroot-common-bundle.sh \
  "${PIE_EXECUTOR_IMAGE}" \
  "${PIE_TEST_DATA_DIR}/kroot-common" \
  "${PIE_KROOT_COMMON_BUNDLE_VERSION}"
```

`kroot-common/current`가 없으면 기존 사용자 reconcile이 실패하도록 닫혀 있다. 빈 공통
설정으로 조용히 기동해 프로젝트마다 오래된 skills를 다시 복사하는 상태를 허용하지
않는다. 자세한 구조와 롤백은
[`../../docs/kroot-common-skills-and-agents.md`](../../docs/kroot-common-skills-and-agents.md)를
따른다.

`pie-sandbox-test-executor`는 Compose 외부 network로 한 번만 만들며, 반드시
`com.docker.network.bridge.enable_icc=false`를 확인한다. 이미 같은 이름의 network가
있다면 무조건 재생성하지 말고 연결된 컨테이너와 option을 먼저 점검한다.

## Executor 데이터 Volume

용량이 큰 사용자 데이터는 Kroot 서버의 별도 NVMe인 `/backup`에 보관한다. Manager의
레지스트리, 중앙 Claude 인증, 제어 상태와 채팅 journal은 기존
`PIE_TEST_DATA_DIR=/home/kaonkroot/pie-sandbox-test/data`에 남겨 장애 범위를 분리한다.

```dotenv
PIE_EXECUTOR_VOLUME_DIR=/backup/pie-sandbox-test/executor-data
```

| Host 경로 | Executor 경로 | 용도 |
|---|---|---|
| `${PIE_EXECUTOR_VOLUME_DIR}/workspaces/{userId}` | `/workspace` | 프로젝트와 생성 파일 |
| `${PIE_EXECUTOR_VOLUME_DIR}/executor-state/{userId}` | `/home/executor` | Claude·Kroot 설정과 세션 |
| `${PIE_EXECUTOR_VOLUME_DIR}/blobs/{userId}` | `/workspace/input` | 업로드 첨부파일, 읽기 전용 |

Manager 컨테이너에는 `PIE_TEST_DATA_DIR`와 `PIE_EXECUTOR_VOLUME_DIR`를 각각 같은 절대
경로로 bind mount한다. 그래야 Manager가 Docker daemon에 넘기는 Host bind 경로와
실제 서버 경로가 일치한다. `/backup` 마운트가 사라진 상태에서 빈 root filesystem
디렉터리에 데이터를 쓰지 않도록 서버 기동 점검에서
`findmnt -T ${PIE_EXECUTOR_VOLUME_DIR}`가 `/dev/nvme2n1p1`인지 먼저 확인한다.

2026-08-12 이전 시에는 웹채팅·Preview Gateway·Manager·Executor를 중지한 상태에서
파일 내용 해시, 상대 경로, 유형, 권한, UID/GID와 심볼릭 링크 대상을 원본과 비교했다.
검증 후 두 Executor를 새 경로로 재생성하고 공개 브라우저 E2E까지 통과했다. 이전
`/home/kaonkroot/pie-sandbox-test/data/{workspaces,executor-state,blobs}`는 즉시 삭제하지
않고 롤백 원본으로 보존한다.

## 외부 경로

| 주소 | 대상 |
|---|---|
| `https://relay-test.cookai.dev` | Kroot Studio 테스트 Relay HTTPS/WSS |
| `https://api-relay-test.cookai.dev` | Kroot Studio 테스트 Control API |
| `https://admin-relay-test.cookai.dev/admin/` | Kroot Studio 테스트 운영 콘솔 |
| `https://chat-relay.cookai.dev` | Kroot Studio 경로를 검증하는 제3자 웹채팅 BFF |
| `https://p-{무작위값}.preview.kroot.io` | 사용자 프로젝트별 임시 웹 프리뷰 |

다음 주소는 이 Compose project가 아니라 별도 Vibe Canvas 스택이 소유한다.

| 주소 | 대상 |
|---|---|
| `https://relay.cookai.dev` | Vibe Canvas Relay HTTPS/WSS |
| `https://api-relay.cookai.dev` | Vibe Canvas Control API |
| `https://admin-relay.cookai.dev/admin/` | Vibe Canvas 운영 콘솔 |

Admin과 API는 Manager bearer/service token이 있어야 데이터를 읽거나 변경할 수 있다.
Relay의 `/metrics`도 별도의 metrics bearer token으로 보호한다.

실제 서버 인벤토리, 공식 주소 전환 결과, E2E 합격 범위와 Production 전 누락 항목은
[`../../docs/sandbox-test-server-plan.md`](../../docs/sandbox-test-server-plan.md)에
기록한다. 이 공유 서버에서는 Docker daemon 재시작, host 재부팅, disk-fill과 broad
prune 시험을 수행하지 않는다.

Kroot Studio 테스트 API/Admin router는 DNS A record가 `221.143.48.77`로 해석되는
것을 확인한 뒤 Manager에 적용했다. Vibe Canvas 공식 router는 별도 Compose project와
별도 자격으로 운영한다. 자세한 분리 기준은
[`../../docs/kroot-studio-vibe-canvas-product-isolation.md`](../../docs/kroot-studio-vibe-canvas-product-isolation.md)에
기록한다.

웹채팅은 기본 stack에 자동으로 올라오지 않고 `web-chat` profile로 실행한다. 현재
production-like Staging에서는 회원가입을 켜 두었다. 초기 `users.json`은 PostgreSQL
사용자 테이블에 멱등적으로 seed되며, 이후 회원가입 계정과 provisioning 상태는
PostgreSQL에 저장된다. 사용자 credential은 애플리케이션 키로 AES-256-GCM 암호화한다.
Integration token, DB URL, credential 암호화 키와 초기 사용자 파일은 mode 0600 host
파일을 read-only mount하고 브라우저에는 노출하지 않는다.

Manager의 Executor 한도만 올리고 Integration의 `maxUsers`를 그대로 두면 회원 정보는
저장되지만 컨테이너 할당이 `429 control quota exceeded`로 실패한다. Web Chat 기동 전에
다음을 실행한다. 기존 한도를 낮추지는 않으며, token 파일이 없을 때만 token을 회전해
mode 0600으로 저장한다.

```bash
set -a
source .env
set +a
PIE_MANAGER_ADMIN_URL=https://api-relay-test.cookai.dev \
  ./reconcile-web-chat-integration.sh
```

```bash
docker compose --env-file .env --profile web-chat up -d --build web-chat
```

## 공개 Web Chat 실사용 시험

브라우저에서 다음 주소를 연다.

```text
https://chat-relay.cookai.dev
```

현재 Staging E2E 로그인 계정은 서버의 보호된 파일에만 있다. 계정 정보가 터미널 출력이나
shell history에 남지 않도록 macOS clipboard로 바로 가져온다.

```bash
ssh pie-sandbox-test \
  "jq -r '\"사용자: \(.username)\n비밀번호: \(.password)\"' /home/kaonkroot/pie-sandbox-test/web-chat/signup-login.json" \
  | pbcopy
```

클립보드에 복사된 사용자 이름과 비밀번호로 로그인한다. 기존 Project를 선택하고 새
대화를 만든 뒤 메시지를 보내면 다음 공개 경로를 왕복한다.

새 사용자는 로그인 화면의 `회원가입`을 선택한다. 가입 요청은 먼저 계정을 영속화하고,
Manager가 전용 Executor를 만들고 `ready`로 전환한 뒤 세션을 발급한다. Manager나 Docker
오류로 중간 실패해도 같은 계정으로 로그인한 뒤 `작업공간 준비`를 눌러 멱등적으로
복구할 수 있다.

```text
Browser → Web Chat BFF → api-relay-test.cookai.dev → relay-test.cookai.dev
        → 사용자 Docker clientd → Claude Code → Browser SSE
```

상태 확인은 다음처럼 한다.

```bash
curl --fail https://chat-relay.cookai.dev/api/health
ssh pie-sandbox-test \
  'cd /home/kaonkroot/pie-sandbox-test/src/deploy/test-server && \
   docker compose --env-file .env --profile web-chat ps'
```

자동 E2E는 서버의 mode 0600 로그인 파일을 read-only mount해 실행한다. 비밀번호를
명령 인수나 환경변수에 직접 넣지 않는다. 검사는 로그인, Workspace, Project,
Conversation, SSE, 실제 Claude 응답과 시험 Conversation 삭제를 포함한다.

Docker Executor의 무승인 도구 정책은 일반 응답만으로 판정하지 않는다. 다음 검사는
두 사용자에게 각각 새 Conversation을 만들고, 정답을 미리 알 수 없는 Linux UUID를
실제 Bash로 읽게 한다. 두 요청을 동시에 보낸 뒤 `tool_call`, `tool_result`, `done`을
확인하고 `permission_request`가 한 건이라도 나오면 실패한다. 시험 Conversation은
성공·실패와 관계없이 닫아 정리한다.

```bash
ssh pie-sandbox-test '
  root=/home/kaonkroot/pie-sandbox-test
  docker run --rm -i \
    -e PIE_MANAGER_URL=https://api-relay-test.cookai.dev \
    -e PIE_INTEGRATION_ID=cookai-e2e \
    -e PIE_ADMIN_ENV_FILE=/run/secrets/test.env \
    -e PIE_INTEGRATION_TOKEN_FILE=/run/secrets/integration.token \
    -e PIE_EXPECTED_USERS=2 \
    -v "$root/src/deploy/test-server/.env:/run/secrets/test.env:ro" \
    -v "$root/integration-e2e.token:/run/secrets/integration.token:ro" \
    node:22-bookworm-slim node --input-type=module -
' < scripts/e2e/remote-bypass-permissions.mjs
```

Write/Edit 회귀 검사는 실제 프로젝트 파일을 `Write → Edit → Read → Bash(rm)` 순서로
처리한다. 서로 다른 두 사용자 컨테이너에서 동시에 실행하며, 성공 결과의
`permissionRequests`는 `0`, `explicitDenyPreserved`는 `true`여야 한다. 민감 경로 검사는
파일을 남기지 않으며 모든 시험 Conversation도 성공·실패와 관계없이 정리한다.

```bash
ssh pie-sandbox-test '
  root=/home/kaonkroot/pie-sandbox-test
  docker run --rm --network kroot-shared-edge-network \
    -e PIE_MANAGER_URL=https://api-relay-test.cookai.dev \
    -e PIE_INTEGRATION_ID=cookai-e2e \
    -e PIE_ADMIN_ENV_FILE=/run/secrets/manager.env \
    -e PIE_INTEGRATION_TOKEN_FILE=/run/secrets/integration.token \
    -e PIE_EXPECTED_USERS=2 \
    -e PIE_VERIFY_DENY_RULES=true \
    -e PIE_SMOKE_TIMEOUT_MS=300000 \
    -v "$root/src/scripts/e2e/remote-write-edit-bypass.mjs:/test.mjs:ro" \
    -v "$root/src/deploy/test-server/.env:/run/secrets/manager.env:ro" \
    -v "$root/integration-e2e.token:/run/secrets/integration.token:ro" \
    node:22-bookworm-slim node /test.mjs
'
```

신규 가입부터 확인할 때는 서버의 `signup-login.json`을 로컬 파일로 복사하지 않고
표준입력으로만 전달할 수 있다.

```bash
ssh pie-sandbox-test \
  'cat /home/kaonkroot/pie-sandbox-test/web-chat/signup-login.json' \
  | PIE_WEB_CHAT_SMOKE_URL=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SMOKE_ORIGIN=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SIGNUP_LOGIN_FILE=/dev/stdin \
    node examples/third-party-web-chat/scripts/remote-signup-smoke.mjs
```

공개 Chrome UI에서는 새 대화가 이전 Docker session을 닫고 교체되는지와 함께, 실제
Claude 요청의 원본 도구 이름·입력·결과가 메시지 목록에 순서대로 나타나고 마지막 text
delta가 별도 Claude 말풍선으로 이어지는지도 검증한다. 성공 결과의
`activeConversations`는 `1`, `liveStreamReady`, `streamSendGuardObserved`,
`rawToolInputObserved`, `rawToolResultObserved`는 `true`, `toolName`은 `Bash`여야 한다.
`markdownObserved`와 `krootDoneFiltered`도 `true`여야 한다. `rawToolResult`의 작업 경로가
Markdown으로 렌더링된 `assistantText`에 포함돼야 하며 시험 종료 뒤 생성 대화는 모두
정리되어야 한다.

```bash
ssh pie-sandbox-test \
  'cat /home/kaonkroot/pie-sandbox-test/web-chat/signup-login.json' \
  | PIE_WEB_CHAT_SMOKE_URL=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SMOKE_ORIGIN=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SMOKE_LOGIN_FILE=/dev/stdin \
    node examples/third-party-web-chat/scripts/remote-browser-lifecycle-smoke.mjs
```

Claude Code 서브에이전트의 본문·도구·완료 상태가 실제 브라우저에 실시간 전달되는지는
같은 시험에 서브에이전트 전용 모드를 켜 확인한다. 성공 결과에는
`subagentStreamingObserved: true`, `observedRunning: true`가 있어야 하며, 완료된
`Explore` 카드에 `Bash` 두 건과 `pie-subagent-ok`가 나타나고 `toolErrors`는 비어 있어야
한다.

```bash
ssh pie-sandbox-test \
  'cat /home/kaonkroot/pie-sandbox-test/web-chat/signup-login.json' \
  | PIE_WEB_CHAT_SMOKE_URL=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SMOKE_ORIGIN=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SMOKE_LOGIN_FILE=/dev/stdin \
    PIE_WEB_CHAT_SMOKE_SUBAGENT_ONLY=true \
    node examples/third-party-web-chat/scripts/remote-browser-lifecycle-smoke.mjs
```

현재 기준 구성은 개인 `.credentials.json`을 Executor에 복사하지 않는다. Event Manager의
중앙 Credential Broker가 `claude setup-token`을 암호화·버전 관리하고, 실제 채팅 세션
시작 시에만 전용 FD를 거쳐 Claude Code 프로세스에 전달한다. 새 이미지 배포 뒤에는
`scripts/ops/claude-auth-login.sh`로 새 setup-token을 게시하고 canary 실제 대화까지
성공해야 인증 전환이 완료된 것으로 본다.

## 프로젝트 웹 프리뷰

웹채팅에서 준비된 프로젝트를 선택하면 `WEB PREVIEW` 영역에서 비공개 또는 공개
프리뷰를 만들 수 있다. 프리뷰는 새 컨테이너가 아니라 해당 사용자의 Executor 안에서
별도 process group으로 실행되며 Host port를 publish하지 않는다. `preview-gateway`가
사용자별 internal Docker network를 통해서만 개발 서버에 접근한다.

사용자는 컨테이너 경로나 `앱 경로`를 직접 입력하지 않는다. clientd가 선택한 작업
프로젝트 안에서 `package.json`의 `scripts.dev`가 있는 웹 앱을 제한적으로 탐지한다.
후보가 하나면 자동 선택하고, 여러 개면 package 이름과 Next.js·Vite·Node.js 프로필을
목록으로 보여준다. 선택값은 브라우저 저장소가 아니라 Manager의 Project 레코드에
저장되므로 다른 브라우저로 접속해도 유지된다. Claude Code로 앱을 새로 만든 뒤에는
`다시 찾기`를 누르면 된다.

탐지는 `node_modules`, 숨김·빌드 디렉터리와 디렉터리 심볼릭 링크를 건너뛰고 결과
수와 깊이를 제한한다. 후보가 여러 개일 때 첫 번째 폴더를 임의 실행하지 않으며,
사용자가 이름으로 고른 앱만 프리뷰에 사용한다. 브라우저에는 컨테이너 절대경로를
반환하지 않는다.

기본 실행 버튼은 같은 실행 앱의 기존 주소를 재사용한다. 실행 중이면 바로 열고,
중지·실패 상태이면 동일한 서브도메인으로 다시 시작한다. 공개·비공개는 별도 프리뷰가
아니라 같은 프리뷰의 접근 정책이며 변경 전 접근 token과 session cookie는 즉시
폐기된다. 주소 자체를 교체해야 할 때는 기존 프리뷰를 삭제하고 다시 실행한다. 삭제는
프로세스 중지와 Control 레코드 제거를 함께 수행하되 감사 기록은 보존한다. 첫 실행은
`npm ci` 또는 `npm install`로 의존성을 준비하므로 조금 더 걸릴 수 있으며, 이후에는
lockfile 지문이 같으면 설치를 건너뛴다. 유휴 회수된 사용자 Executor도 실행 버튼을
누르면 자동으로 다시 시작된다.

Kroot 서버의 기존 `kroot` 인증서 resolver는 HTTP-01이므로 wildcard 인증서를 발급할
수 없다. Preview Router에는 기존 resolver를 지정하지 않았다. 배포 전에 반드시
`preview.kroot.io`, `*.preview.kroot.io` 인증서를 DNS-01로 발급하여 공용 Traefik의
동적 TLS 설정에 등록해야 한다. 자세한 구조와 검증 명령은
[`../../docs/project-preview-platform.md`](../../docs/project-preview-platform.md)를 따른다.

최초 수동 DNS-01 시험 인증서가 준비되면 기존 공용 Compose를 수정하지 않고 다음
override를 함께 적용한다. 스크립트는 SAN·만료일·개인키 일치를 먼저 검사하고 Certbot
계정 전체가 아닌 서비스용 인증서 두 파일만 Traefik에 read-only로 제공한다.

```bash
cd /home/kaonkroot/pie-sandbox-test/current/src/deploy/test-server
./apply-preview-tls.sh
```

이 방식도 Traefik 컨테이너를 한 번 재생성하므로 기존 모든 HTTPS endpoint를 먼저
확인하고 짧은 점검 시간에 실행한다. 롤백은 원본
`docker-compose.shared-traefik.yml`만 지정해 `kroot-shared-lb`를 재생성한다.
