# Pie Relay 배포 및 운영 가이드

Azure `rg-jikime` 배포의 리소스 구성, VM/ACR/Key Vault/Monitor 선택과 단계별 전환
계획은 [Pie Relay Azure 배포 계획](./azure-deployment-plan.md)을 기준으로 한다.

## 권장 단일 노드 구성

`deploy/compose.yaml`은 다음 서비스를 한 Docker 호스트에 배치한다.

```text
Internet
  │ HTTPS/WSS
Traefik
  ├─ relay.cookai.dev       → Pie Relay
  ├─ api-relay.cookai.dev   → Pie Manager API
  └─ admin-relay.cookai.dev → Pie Admin Web
                                  │
                          PostgreSQL + Docker API
                                  │
                         사용자별 Executor 컨테이너
```

Relay가 terminal payload를 전달하는 Data Plane이고 Manager가 사용자, 장치,
세션, 권한, operation을 관리하는 Control Plane이다. Manager는 terminal byte를
중계하지 않는다.

## 최초 배포

운영 DNS를 연결하기 전에는 [`../deploy/local/README.md`](../deploy/local/README.md)의
오버레이로 동일한 Relay·Manager·Executor 흐름을 먼저 통과시킨다. 로컬 구성은 운영
Compose 파일을 복사하지 않고 오버레이하므로 운영 기본값과 로컬 HTTP/CA/PAT 모의를
명확히 분리한다.

1. 세 DNS 이름을 배포 호스트로 연결한다.
2. Executor 이미지를 호스트 Docker daemon에 빌드한다.
3. 예시 환경 파일을 복사하고 모든 secret을 서로 다른 난수로 교체한다.
4. Compose 구성을 검증한 뒤 시작한다.

```bash
docker build -f executor-manager/Dockerfile.executor \
  -t pie-relay-client:latest .

cp deploy/.env.example deploy/.env
# deploy/.env의 replace-with-* 값을 모두 교체

docker compose --env-file deploy/.env -f deploy/compose.yaml config
docker compose --env-file deploy/.env -f deploy/compose.yaml up -d --build
```

데이터 기본 경로는 `/var/lib/pie-relay`다. Manager 컨테이너와 Docker 호스트가
동일한 절대 경로를 보도록 mount하므로 `PIE_DATA_DIR`을 바꿀 때도 host와
container의 경로를 동일하게 유지한다. Relay에는 전체 경로를 노출하지 않고
전용 `relay-state` volume만 `/var/lib/pie-relay/relay`로 마운트한다. 이 volume에는
모바일 resume 상태만 두며 workspace와 Claude 인증 상태를 Relay에서 읽을 수 없게
분리한다.

## 필수 secret 경계

| 값 | 사용 주체 | 용도 |
|---|---|---|
| `RELAY_JWT_SECRET` | Manager, Relay | 짧은 수명의 session capability 발급·검증 |
| `PIE_RELAY_ROUTING_SECRET` | Manager | Application/Tenant/Resource 기반 불투명 room HMAC |
| `PIE_RELAY_CONTROL_TOKEN` | Manager, Relay | 연결 종료와 Driver 인계 내부 API |
| `PIE_MANAGER_ADMIN_TOKEN` | 운영자 | Admin/API의 비상용 정적 관리자 인증 |
| `PIE_RELAY_PRESENCE_TOKEN` | Relay, Manager | presence 전송만 가능한 최소권한 인증 |
| `PIE_RELAY_METRICS_TOKEN` | Prometheus, Relay | Relay metric 조회 전용 인증 |
| `HOST_ENROLL_SECRET` | Desktop host 등록자, Relay | standalone host token 발급 |
| `POSTGRES_PASSWORD` | Manager, PostgreSQL | Control Plane 저장소 |
| `PIE_USER_WEBHOOK_SECRET` | 외부 회원 서비스, Manager | 회원 lifecycle 요청 HMAC 검증 |

secret은 이미지, Git, QR 코드, 로그에 넣지 않는다. 운영에서는 Docker secret,
Vault 또는 클라우드 secret manager로 주입하고 주기적으로 회전한다. Relay control
API는 별도 bearer token을 요구하며 Traefik 공개 라우터에서 직접 사용할 이유가
없다.

`RELAY_JWT_SECRET`과 설정된 `HOST_ENROLL_SECRET`은 각각 32바이트 이상이어야 하며
Relay는 더 짧은 값으로 기동하지 않는다. `RELAY_ALLOW_LEGACY_QUERY_TICKET`과
`RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS`의 운영 기본값은 모두 `false`다. 초대 권한을
생략하면 `view`로 발급되며 `control`은 요청에서 명시해야 한다.

서비스별 Pool에는 `RELAY_POOL_ID`, `RELAY_NODE_ID`,
`RELAY_ALLOWED_APPLICATIONS`를 지정한다. 제공된 배포 구성은
`RELAY_REQUIRE_POOL_SCOPE=true`가 기본이며 Pie Control이 발급한 관리형 토큰은 Application,
Pool, Tenant, Resource, Relay node와 `relayGeneration`을 모두 가져야 한다.
모바일·수동 연결·초대처럼 Relay 자체가 발급하는 토큰만
`RELAY_ALLOW_DIRECT_TOKENS_WITHOUT_POOL_SCOPE=true`라는 별도 호환 경계를 사용한다. 이 예외는
issuer가 `pie-relay`인 직접 발급 토큰에만 적용되므로 scope 없는 `pie-control` 토큰은 통과하지
못한다. 직접 발급 기능을 사용하지 않는 전용 Cell에서는 이 값도 `false`로 내린다.

Manager는 node heartbeat가 기본 90초 이상 끊겼거나 `MaxConnections`에 도달한 노드를 새 세션
배정에서 제외한다. 이미 연결된 노드의 lease가 만료되면 세션을 `reconnecting`으로 바꾸고
`relayGeneration`을 올린 뒤 건강한 노드에 재할당한다. 이전 세대의 토큰, Presence와 driver
명령은 새 routing key와 일치하지 않아 폐기된다. 다중 Manager가 PostgreSQL을 공유할 때는 pool별
advisory lock 안에서 증분 상태를 다시 읽고 배정하므로 replica 사이의 용량 초과 배정을 막는다.

제공된 Traefik 구성은 `/host/enroll`, `/rooms/join`, `/rooms/invites`를 각각 IP 기반
rate-limit 라우터로 먼저 처리한다. CDN이나 다른 프록시를 Traefik 앞에 둘 때는 신뢰할
프록시 대역을 명시하기 전까지 전달된 IP 헤더를 무조건 신뢰하지 않는다. 실제 고객의 NAT
규모와 로그인 패턴에 맞춰 제한값을 조정하되 인증 경로 제한을 제거하지 않는다.

`PIE_AUTH_INTROSPECTION_URL`은 실제 회원 서비스의 PAT 검증 endpoint로 설정한다.
Manager는 짧은 TTL로 introspection 결과를 캐시하며 Desktop은 PAT를 디스크에
저장하지 않는다. Relay presence token에는 관리자 API 권한이 없다. 공개 Relay 주소는
`PIE_RELAY_PUBLIC_URL`로 Manager에 전달되어 Desktop이 세션별 credential과 함께
올바른 endpoint를 사용한다.

`RELAY_PUBLIC_URL`/`PIE_RELAY_PUBLIC_URL`은 외부 클라이언트 주소이고,
`PIE_RELAY_CONTROL_URL`은 Manager가 호출할 내부 주소다. 단독 Docker 네트워크라면
Compose 서비스 이름을 사용할 수 있지만, 여러 Compose 프로젝트가 하나의 외부
`edge` 네트워크를 공유할 때 `relay`, `manager` 같은 짧은 별칭은 충돌한다. 이 경우
`http://pie-sandbox-test-relay:13412`처럼 프로젝트 고유의 내부 network alias를
사용한다. 공개 주소의
`127.0.0.1`을 내부 제어 주소로 재사용하면 Manager 컨테이너가 자기 자신에 연결하므로
Driver 인계와 participant 강제 종료가 실패한다.

## 네트워크와 Docker socket

- `control` network는 외부 egress가 없는 Manager/Relay/PostgreSQL 내부망이다.
- `manager-egress` network에는 Manager만 참여하며 외부 PAT introspection과 secret
  provider 같은 outbound HTTPS 호출에 사용한다. host port는 게시하지 않고 공개
  ingress는 계속 Traefik→`control` 경로로만 받는다.
- `pie-executor` network는 Executor가 Relay와 Claude API에 outbound 접속하기
  위해 egress를 허용한다. Executor의 inbound host port는 열지 않으며 Manager와
  PostgreSQL은 이 network에 참여하지 않아 사용자 코드에서 직접 접근할 수 없다.
- Docker socket은 Manager만 받는다. 이것은 사실상 host-root 권한이므로 Manager
  API와 Admin Web을 강하게 인증하고 일반 사용자에게 operation 권한을 주지 않는다.
- Manager는 공개 `edge` network에 직접 참여하지 않고 Traefik이 내부 `control`
  network를 통해서만 연결한다. 별도 `manager-egress`는 outbound 전용이며 Relay와
  Manager의 root filesystem은 read-only이고
  필요한 상태·임시 경로만 별도로 쓰기 허용한다.
- Executor에는 Docker socket, privileged, Linux capability 또는 host 임의 경로를
  제공하지 않는다.

더 강한 멀티테넌시가 필요하면 Manager를 별도 Docker 호스트에 두고 gVisor, Kata
Containers 또는 MicroVM runtime adapter를 적용한다.

## 상태 확인

```bash
curl --fail https://relay.cookai.dev/readyz
curl --fail https://api-relay.cookai.dev/healthz
curl --fail -H "Authorization: Bearer $PIE_MANAGER_ADMIN_TOKEN" \
  https://api-relay.cookai.dev/v1/admin/overview
curl --fail -H "Authorization: Bearer $PIE_MANAGER_ADMIN_TOKEN" \
  'https://api-relay.cookai.dev/v1/admin/sessions?limit=100'
```

Admin Web은 `https://admin-relay.cookai.dev/admin/`에서 열고 Manager 관리자 토큰
또는 외부 인증 서비스의 운영자 PAT를 입력한다. 토큰은 브라우저 탭의
`sessionStorage`에만 저장된다. 이 도메인은 `PIE_CORS_ALLOWED_ORIGINS`에 반드시
포함해야 하며, 새 관리 도메인을 추가할 때도 wildcard 대신 정확한 origin을 등록한다.

주요 경보 기준은 다음과 같다.

- Relay `/readyz` 실패 또는 `pie_relay_slow_peer_evicted_total` 급증
- Manager `/readyz` 실패, operation `failed`, provisioning queue 장기 적체
- `clientConnected=false`, `relayRegistered=false`, Relay node heartbeat 누락
- Docker unhealthy/OOM, host disk 80% 초과, PostgreSQL connection/storage 부족
- Docker data-root의 byte/inode 부족(이미지 빌드뿐 아니라 사용자 auth volume 쓰기도 실패)
- 재연결 수와 rate limit 거부의 비정상 증가

## 백업과 복구

- PostgreSQL: 사용자, 장치, 세션, grant, operation, audit의 정본이다. 정기적인
  logical backup과 시점 복구 정책을 둔다.
- `/var/lib/pie-relay/workspaces`: 사용자 작업 결과다. 사용자별 보존·삭제 정책으로
  백업한다.
- `/var/lib/pie-relay/executor-state`: 사용자별 Claude 인증 상태를 포함할 수 있다.
  암호화 백업과 접근 감사를 적용한다.
- `relay-state` volume의 `mobile-state.json`: 모바일 resume credential의 hash다.
  유실되면 재페어링이 필요하지만 원문 credential은 복구할 수 없다.
- blob과 임시 로그는 별도 retention으로 정리한다.

Relay는 terminal payload를 저장하지 않으므로 Relay 프로세스는 재생성 가능하다.
재시작 뒤 host/clientd는 지수 backoff로 다시 연결하고 15초 presence heartbeat가
Control Plane 상태를 복구한다. 노드 자체가 제한 시간 안에 돌아오지 않으면 Control Plane이
세션 세대를 올려 다른 노드로 이동시키므로, 로드밸런서의 우연한 재접속에 의존하지 않는다.

## 안전한 배포 순서

1. 새 Manager/Relay/Executor 이미지를 빌드하고 테스트한다.
2. PostgreSQL backup과 schema 호환성을 확인한다.
   Manager 시작 시 `pie_control_changes`와 변경 trigger가 자동 생성되며, 이 테이블은
   다중 Manager의 증분 동기화 cursor다. 임의로 비우려면 모든 Manager를 중지하고
   backup을 확보한 뒤 전체 snapshot 재시작 절차로 수행한다.
3. Relay 한 노드를 readiness drain한 뒤 교체한다. 여러 Relay를 둘 때는 같은
   device/session의 host와 participant가 반드시 같은 노드로 가도록 assignment 기반
   sticky routing을 구성한다.
4. Manager를 교체하고 reconciliation 완료를 확인한다.
5. 사용자 Executor 이미지는 `runtime.recreate` operation으로 사용자별 순차 교체한다.
6. 연결, session, operation, slow-peer metric을 확인한 뒤 배포를 완료한다.

## 검증

```bash
cd server && go test -race ./...
cd ../client && go test -race ./...
cd ../executor-manager && go test -race ./...
cd ../client/node-executor && npm test
cd ../../desktop && npm test && npm run build
(cd src-tauri && cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings && cargo test --locked)
node ../scripts/e2e/relay-smoke.mjs
```

세 Go 모듈에서 `govulncheck ./...`도 실행한다. 빌드 최소 버전은 Go 1.25.12이며,
Dockerfile도 같은 patch 버전으로 고정돼 있다.

PostgreSQL 통합 테스트는 임시 데이터베이스를 지정했을 때만 실행한다.

```bash
PIE_TEST_POSTGRES_DSN='postgres://pie:pie@127.0.0.1:5432/pie?sslmode=disable' \
  go test ./internal/control -run Postgres
```

실서비스 공개 전에는 Local/Docker, private/shared, LAN/Relay, view/control/driver와
Relay/Manager/clientd 재시작을 조합한 staging E2E를 반드시 수행한다.

Generic Desktop/clientd의 원격 세션 transport는 Relay다. LAN 직접 연결은 모바일
Gateway 경로의 기능이며, 관리 콘솔에서 Docker 세션을 만들 때도 Relay로 고정된다.
로컬 장치 세션은 관리자가 빈 레코드만 만드는 것이 아니라 실제 Desktop/clientd가
호스트 프로세스를 시작하면서 등록해야 한다.
