# 고객 배포 준비 상태와 릴리스 게이트

이 문서는 Pie Relay를 고객 환경에 배포할 때 코드가 보장하는 범위와 배포 전에
환경별로 반드시 확인할 항목을 구분한다. 단위 테스트 통과만으로 실제 인증 서비스,
TLS, 모바일 네트워크와 Docker 용량까지 검증됐다고 간주하지 않는다.

## 현재 기준 구현

- Desktop/clientd의 session-scoped Relay 연결과 viewer/controller 권한
- 동일 세션의 단일 Driver 인계, 강제 연결 종료와 presence 반영
- Relay 재시작 후 clientd 지수 backoff 재연결과 PTY snapshot 복구
- 사용자별 Docker Executor, workspace/auth 경계, CPU·메모리·PID 제한
- PAT introspection, 사용자 lifecycle webhook, quota와 operation 감사 기록
- PostgreSQL optimistic concurrency와 다중 Manager refresh
- PostgreSQL 레코드별 변경 cursor 기반 증분 동기화와 삭제 tombstone 반영
- Relay WebSocket Origin 검증, ticket subprotocol, 인증된 운영 metric
- URL query JWT와 scope 없는 JWT 기본 차단, 각각 분리된 legacy migration switch
- Manager/Relay/Executor 네트워크 분리와 Docker 자동 복구

현재 Compose는 단일 Docker 호스트 기준이다. Relay를 여러 Cell로 수평 확장하려면
같은 session의 host와 participant를 같은 Cell로 보내는 assignment/sticky routing이
필수다. 이 배치 계층 없이 임의 round-robin으로 여러 Relay를 운영하면 안 된다.

## 2026-07-29 공유 Staging 검증 상태

`221.143.48.77`의 기존 Traefik 아래에 Pie 전용 Compose project를 배포하고
`relay.cookai.dev` 공식 Relay 주소를 전환했다. 실제 Claude Code text/image,
permission, 사용자 2명 동시 실행, Executor 격리, Relay·Manager·PostgreSQL 복구와
전체 클라이언트 회귀 테스트는 통과했다. 상세 증거와 수행하지 않은 시험은
[Sandbox 테스트 서버 배포·검증 기록](./sandbox-test-server-plan.md)에 있다.

현재 DNS 상태는 다음과 같다.

| 주소 | 상태 |
|---|---|
| `relay.cookai.dev` | A record, TLS, HTTPS/WSS 정상 |
| `relay-test.cookai.dev` | 진단용 별칭 정상 |
| `api-relay-test.cookai.dev` | Staging Manager API 정상 |
| `admin-relay-test.cookai.dev` | Staging Admin 정상 |
| `api-relay.cookai.dev` | A record, TLS, 보호된 Manager API 정상 |
| `admin-relay.cookai.dev` | A record, TLS, Admin 및 보호된 API 정상 |

공식 DNS와 router까지 검증했지만 현재 결과는 여전히 Staging 합격이며 Production 공개
승인이 아니다. Claude credential 회전, off-host backup/restore, byte/inode quota,
전용 Executor Node, 4명 이상 부하와 8시간 soak가 고객 공개의 차단 항목이다.

## 주소 구분

Relay에는 서로 다른 두 주소가 있다.

| 설정 | 사용 주체 | 예시 |
|---|---|---|
| `RELAY_PUBLIC_URL` / `PIE_RELAY_PUBLIC_URL` | Desktop·모바일·외부 participant | `https://relay.cookai.dev` |
| `PIE_RELAY_CONTROL_URL` | Manager가 Driver 인계·연결 종료에 사용하는 내부 주소 | `http://relay:13412` |

컨테이너 안에서 공개 주소가 `127.0.0.1`이면 자기 컨테이너를 가리킨다. 공개 주소와
내부 제어 주소를 혼용하면 화면 연결은 되더라도 Driver 인계나 강제 종료가 실패한다.
`deploy/compose.yaml`은 두 주소를 분리해 설정한다.

## 자동 검증 게이트

릴리스 후보마다 다음 항목이 모두 통과해야 한다.

```bash
(cd server && go test -race ./... && go vet ./... && golangci-lint run ./... && govulncheck ./...)
(cd client && go test -race ./... && go vet ./... && golangci-lint run ./... && govulncheck ./...)
(cd executor-manager && go test -race ./... && go vet ./... && golangci-lint run ./... && govulncheck ./...)
(cd client/node-executor && npm test && npm audit)
(cd desktop && npm test && npm run build && npm audit)
(cd desktop/src-tauri && cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings && cargo test --locked)
(cd pie-mobile/adapter/host-gateway && pnpm test && pnpm typecheck && pnpm build && pnpm audit)
(cd pie-mobile/upstream/mobile && pnpm test && pnpm typecheck && pnpm lint && pnpm audit)
docker compose --env-file deploy/.env.example -f deploy/compose.yaml config --quiet
```

Go 모듈과 빌드 이미지는 표준 라이브러리 보안 수정이 포함된 Go 1.25.12 이상을
요구한다. patch 버전을 낮추면 `govulncheck`가 실패하므로 production builder에서
임의로 이전 toolchain을 강제하지 않는다.

PostgreSQL은 폐기 가능한 별도 데이터베이스로 충돌 테스트를 실행한다.

```bash
(cd executor-manager && \
  PIE_TEST_POSTGRES_DSN='postgres://pie:pie@127.0.0.1:5432/pie?sslmode=disable' \
  go test ./internal/control -run Postgres -count=1)
```

`brace-expansion` 1.x를 요구하는 React Native 도구 체인에는 결과 수·입력 길이·중첩
수 제한을 backport한 pnpm patch가 적용돼 있다. 감사 도구의 해당 GHSA ignore는
취약 코드를 그대로 허용하는 예외가 아니다. `pnpm test`가 실제 설치 모듈에서 patch와
제한 동작을 먼저 검증하므로 이 검사 없이 모바일 빌드를 배포하지 않는다.

## 실제 연결 검증 게이트

DNS 없는 개발 환경에서는 아래 명령으로 운영 전 회귀를 수행한다.

```bash
./deploy/local/pie-local.sh test
```

이 테스트는 모의 PAT의 active/inactive/timeout/revocation, HMAC lifecycle,
PostgreSQL, 실제 Docker quota와 작업, PTY Relay, view/control/driver, 동시 20명,
Relay 재시작 복구, 로컬 TLS, 인증 metric, 백업·격리 복원을 한 번에 확인한다. 로컬
통과 결과를 공인 DNS/TLS, 실제 회원 서비스나 물리 모바일 승인으로 대체해서는 안 된다.

`scripts/e2e/relay-smoke.mjs`는 준비된 host session에 대해 다음을 검사한다.

1. session-scoped host credential과 view/control invite 발급
2. JWT를 URL에 넣지 않는 WebSocket ticket subprotocol 연결
3. `relay_join_ack`, host 상태와 PTY snapshot 수신
4. Relay presence의 Manager 투영
5. controller에게 Driver 인계 후 Control Plane 반영
6. Manager operation을 통한 controller/viewer 연결 종료

릴리스 전 staging에서는 여기에 Relay 재시작과 clientd 자동 재연결도 포함한다.
Staging/production 설정에서 `RELAY_ALLOW_LEGACY_QUERY_TICKET=false`와
`RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS=false`도 자동 검사한다. 전환 승인을 받은 제한된
기간 외에는 어느 쪽도 배포 설정에서 켜지 않는다.
Docker 이미지는 별도로 다음을 확인한다.

- Executor가 UID/GID 10001로 실행되고 capability·쓰기 경계 제한을 유지하는가
- 사용자 quota로 생성한 Executor의 CPU·메모리·PID 제한이 `docker inspect`에
  반영되고 quota 변경 후 실행 중 컨테이너에도 갱신되는가
- 번들 `claude --version`과 `pie-relay-client --help`가 실행되는가
- Manager가 Docker socket으로 실제 daemon을 조회하고 Executor를 생성할 수 있는가
- Relay/Manager `/metrics`가 무인증 요청을 거부하는가
- CI image scanner에서 fix 가능한 Critical/High OS·application CVE가 0개인가

## 환경별 최종 승인 항목

다음은 저장소만으로 완료할 수 없으므로 고객 또는 staging 환경에서 승인해야 한다.

- `cookai.dev` DNS, 공인 인증서와 WSS reverse proxy
- 실제 회원 서비스 PAT introspection의 active/inactive/timeout/revocation
- 회원 lifecycle webhook의 secret 회전과 재전송 순서
- 실제 iPhone/Android에서 LAN, 외부 Relay, Wi-Fi↔셀룰러 전환과 백그라운드 복귀
- Claude credential 공급·회수와 사용자별 auth volume 삭제 정책
- Docker filesystem/storage driver에 맞는 사용자별 디스크 quota 강제 정책
- 예상 동시 사용자 수의 soak/load test, 장애 경보와 on-call 절차
- PostgreSQL backup/restore와 workspace/auth volume 복구 훈련

Docker data-root는 80%부터 경고하고 이미지 빌드와 DB 초기화에 필요한 여유 공간을
확보한다. 디스크가 가득 차면 실행 중인 연결보다 신규 Executor와 PostgreSQL 복구가
먼저 실패할 수 있다. 사용자 volume을 자동 삭제해 복구하지 말고 보존 정책에 따라
운영자가 명시적으로 정리한다.
