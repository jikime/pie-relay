# Pie Relay

Pie Relay는 여러 사용자가 로컬 PC, 헤드리스 서버 또는 사용자별 Docker Executor의
터미널과 Claude Code 세션에 안전하게 접속하도록 연결하는 원격 실행·협업 플랫폼이다.
제품명은 **Pie Relay**, 서비스 도메인은 **cookai.dev**다.

## 구성

```text
Desktop / CLI participant ─┐
                           ├─ Pie Relay(Data Plane) ─ clientd / Docker Executor
Mobile ─ Desktop Gateway ──┘               │
                                           │ presence / operation
                              Pie Manager(Control Plane)
                                  ├─ Admin Web
                                  ├─ PostgreSQL
                                  └─ Docker runtime
```

- `server/`: 방·세션 단위 WebSocket 중계, 초대, 모바일 Director/Cell, driver lease,
  presence와 운영 제어 API
- `client/`: 원격 장비에서 ACP 에이전트, Claude Agent SDK 또는 PTY를 실행하는 `clientd`와 Node Executor
- `desktop/`: 방 생성·참가, 원격 터미널, 모바일 Gateway를 제공하는 Pie Relay Desktop
- `pie-mobile/`: iOS/Android 앱과 Desktop Host Gateway 어댑터
- `executor-manager/`: 사용자·장치·Docker·세션·권한·operation을 관리하는 Control Plane
- `examples/third-party-web-chat/`: 공개 Integration API만 사용하는 Next.js 16·shadcn/ui 기반 독립 BFF·웹채팅 예제
- `deploy/`: PostgreSQL, Relay, Manager, Traefik 단일 노드 기준 배포 구성

## Pie Client 빠른 설치

macOS 또는 Linux 장치를 Pie Relay 실행 장치로 연결하려면 GitHub Pages의 공식 설치
스크립트를 사용한다. Node.js 22 이상과 사용할 Claude Code 또는 Codex CLI 로그인이
필요하다.

```bash
curl -fsSL https://jikime.github.io/pie-relay/install.sh | sh
pie-client version
```

설치 프로그램은 운영체제와 CPU를 감지해 GitHub Release의 네이티브 패키지를 받고,
SHA-256 검증을 통과한 경우에만 사용자 홈의 `~/.local` 아래에 설치한다. 버전 고정,
수동 검증 및 릴리스 절차는
[`docs/pie-client-github-distribution.md`](./docs/pie-client-github-distribution.md)에 정리되어 있다.

## 지원 연결 모드

1. 로컬 장비에 `clientd`를 실행하고 Relay를 통해 원격 접속
2. 사용자별 Docker Executor와 전용 workspace/인증 volume에 격리된 세션 제공
3. 하나의 터미널을 여러 viewer/controller가 공유하고 단일 driver를 인계하며 협업
4. 모바일에서 같은 Wi-Fi의 Desktop Gateway에 직접 연결
5. 모바일과 Desktop Gateway를 Relay로 연결해 외부 네트워크에서 E2EE 제어

Local/Docker, private/shared, LAN/Relay는 서로 독립된 정책 축이다. 상세 모델은
[`docs/session-modes-and-mutual-access.md`](./docs/session-modes-and-mutual-access.md)를
참고한다.

## 빠른 검증

각 디렉터리는 독립 모듈이다.

```bash
(cd server && go test -race ./...)
(cd client && go test -race ./...)
(cd executor-manager && go test -race ./...)
(cd client/node-executor && npm ci && npm test && npm audit)
(cd desktop && npm ci && npm test && npm run build && npm audit)
(cd desktop/src-tauri && cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings && cargo test --locked)
```

Go 빌드는 1.25.12 이상을 사용하고 각 Go 모듈에서 `govulncheck ./...`를 릴리스
게이트로 실행한다.

모바일 앱과 Gateway 검증:

```bash
(cd pie-mobile/upstream/mobile && pnpm install --frozen-lockfile && pnpm test && pnpm typecheck && pnpm lint && pnpm audit)
(cd pie-mobile/adapter/host-gateway && pnpm install --frozen-lockfile && pnpm test && pnpm typecheck && pnpm build && pnpm audit)
```

DNS 없이 PostgreSQL, 모의 PAT, 사용자별 Docker Executor, 로컬 TLS, Relay 재시작,
동시 participant와 백업/복원까지 검증하려면 다음 한 명령을 사용한다.

```bash
./deploy/local/pie-local.sh test
```

주소와 실제 모바일 LAN 연결 방법은
[`deploy/local/README.md`](./deploy/local/README.md)에 정리되어 있다.
Pie Canvas와 Kroot Studio를 같은 코드로 독립 배포하는 방법은
[`deploy/profiles/README.md`](./deploy/profiles/README.md)에 정리되어 있다.

## 배포

단일 Docker 호스트 기준 운영 구성은 다음 명령으로 검증하고 시작한다.

```bash
cp deploy/.env.example deploy/.env
# deploy/.env의 모든 replace-with-* 값을 서로 다른 강한 secret으로 교체
docker build -f executor-manager/Dockerfile.executor -t pie-relay-client:latest .
docker compose --env-file deploy/.env -f deploy/compose.yaml config
docker compose --env-file deploy/.env -f deploy/compose.yaml up -d --build
```

기본 공개 주소는 `relay.cookai.dev`, `api-relay.cookai.dev`,
`admin-relay.cookai.dev`다. 운영에서는 PostgreSQL을 정본 저장소로 사용하고 Manager의
Docker socket 접근을 신뢰 경계 안에 둬야 한다. 자세한 절차는
[`docs/deployment-and-operations.md`](./docs/deployment-and-operations.md)에 있다.

## 문서

- [연결 구조와 사용 흐름](./docs/how-to-connect.md)
- [Pie Client GitHub 설치·릴리스](./docs/pie-client-github-distribution.md)
- [Control Plane과 관리 콘솔](./docs/control-plane-and-admin-console.md)
- [Relay 안정화 설계와 운영 기준](./docs/relay-hardening.md)
- [Relay와 Executor Manager 아키텍처](./docs/relay-and-executor-manager.md)
- [제3자 웹채팅 참조 구현](./examples/third-party-web-chat/README.md)
- [배포 및 운영 가이드](./docs/deployment-and-operations.md)
- [DNS 없는 로컬 통합 환경](./deploy/local/README.md)
- [전체 문서 목록](./docs/README.md)

현재 구현은 단일 Docker 호스트에서 실제 운영 가능한 기반을 목표로 한다. 다중 호스트
scheduler, 외부 회원 서비스의 webhook/OIDC 연동, 지역별 Relay Cell 자동 배치는 다음
확장 경계이며 현재 코드가 이를 완료했다고 가정하지 않는다.
