# Pie Relay Server

Pie Relay의 WebSocket 중계 서버다. `clientd`/Executor가 소유한 터미널을 Desktop
참가자에게 전달하고, Pie Relay 모바일 앱의 Director/Cell 연결도 같은 프로세스에서
제공한다. Relay는 터미널 명령을 직접 실행하지 않으며, 일반 터미널 프레임은 방과
세션 범위 안에서 중계한다.

## 연결 경로

| 경로 | 역할 | 인증 |
|---|---|---|
| `/ws/agent` | clientd/Executor host 연결 | `Authorization: Bearer <JWT>` |
| `/ws/participant` | Desktop viewer/controller 연결 | Bearer 또는 `pie-relay.ticket.<JWT>` subprotocol |
| `/ws/browser` | 구버전 participant 별칭 | 신규 클라이언트 사용 금지 |
| `/host/enroll` | host credential 발급 | `HOST_ENROLL_SECRET` |
| `/rooms/invites`, `/rooms/join` | 참가 초대 발급·교환 | scoped host credential·초대 코드 |
| `/v1/assign`, `/v1/resolve`, `/v1/connect/*` | 모바일 Director/Cell | 모바일 페어링 credential |
| `/healthz`, `/readyz` | liveness/readiness | 없음 |
| `/metrics` | Prometheus metric | `PIE_RELAY_METRICS_TOKEN` |

일반 터미널 연결은 JWT의 `room`, `device_id`, `session_id`로 격리된다. 여러
participant가 같은 세션을 볼 수 있지만 raw terminal 입력은 한 명의 Driver만 보낸다.

## 인증 기본값

운영 기본값은 scoped HS256 JWT만 허용한다. 토큰에는 다음 claim이 필요하다.

- `iss=pie-relay`, `aud=pie-relay`, 고유한 `jti`
- `role=host|participant`
- 접속 축과 동작을 제한하는 `cap`
- 만료 시각 `exp`

Participant JWT는 URL에 넣지 않고 WebSocket subprotocol로 전달한다. 다음 두 옵션은
구버전 마이그레이션에만 사용하며 운영 기본값은 모두 `false`다.

- `RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS=true`: `iss/aud/jti/role/cap` 없는 JWT 허용
- `RELAY_ALLOW_LEGACY_QUERY_TICKET=true`: `?ticket=<JWT>` 허용

URL query credential은 access log와 방문 기록에 남을 수 있다. 전환 기간이 끝나면
두 옵션을 즉시 끄고 기존 credential을 재발급한다.

## 로컬 실행

```bash
cd server
export RELAY_JWT_SECRET="$(openssl rand -base64 48)"
export HOST_ENROLL_SECRET="$(openssl rand -base64 48)"
go run ./cmd/relay -addr :13412
```

개발용 host credential은 다음처럼 발급한다. 엄격 모드의 `-mint alice`는 room이
`alice`인 scoped host JWT를 만든다.

```bash
go run ./cmd/relay -mint alice -mint-ttl 1h
```

## 주요 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `RELAY_JWT_SECRET` | 필수 | JWT HMAC 서명·검증 secret |
| `HOST_ENROLL_SECRET` | 없음 | 비-loopback `/host/enroll` 활성화 secret |
| `RELAY_PUBLIC_URL` | 요청 Host | 외부 Desktop/모바일에 광고할 HTTPS origin |
| `RELAY_MOBILE_STATE_FILE` | 메모리 | 해시된 모바일 resume 상태 파일 |
| `RELAY_NODE_ID` | hostname | assignment에 사용하는 안정적 node ID |
| `RELAY_POOL_ID` | 없음 | 서비스·리전별 Relay Pool 식별자 |
| `RELAY_ALLOWED_APPLICATIONS` | 없음 | 이 Pool이 받을 `app_id` 쉼표 목록 |
| `RELAY_REQUIRE_POOL_SCOPE` | `false` | v3 Application/Tenant/Resource와 node 고정을 강제 |
| `RELAY_ALLOWED_ORIGINS` | Tauri/로컬 개발 origin | 허용할 WebSocket origin 목록 |
| `RELAY_MAX_PARTICIPANTS_PER_ROOM` | `64` | 방별 participant socket 상한 |
| `RELAY_FRAMES_PER_SECOND` | `240` | 연결별 inbound frame rate |
| `RELAY_BYTES_PER_SECOND` | `8388608` | 연결별 inbound byte rate |
| `PIE_CONTROL_PLANE_URL/TOKEN` | 없음 | Manager presence 보고 주소와 token |
| `PIE_RELAY_CONTROL_URL/TOKEN` | 없음 | assignment/control API의 내부 주소와 token |
| `PIE_RELAY_METRICS_TOKEN` | 없음 | `/metrics` Bearer token; 없으면 404 |
| `RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS` | `false` | 구형 JWT 임시 허용 |
| `RELAY_ALLOW_LEGACY_QUERY_TICKET` | `false` | query JWT 임시 허용 |

`RELAY_REQUIRE_POOL_SCOPE=true`는 모든 Host와 Participant가 새 context token으로
전환되고 Control Plane에 Pool 노드가 등록된 뒤 활성화한다. Relay는 15초마다
Control Plane으로 node heartbeat를 보내며, Manager는 같은 Pool에서 활성 연결이
가장 적고 한도가 남은 노드를 세션에 고정한다.

전체 운영 구성은 [`../deploy/compose.yaml`](../deploy/compose.yaml), 릴리스 게이트는
[`../docs/release-readiness.md`](../docs/release-readiness.md)를 따른다.

## Docker와 검증

```bash
docker build -f Dockerfile -t pie-relay-server:latest .
docker run --rm -p 13412:13412 \
  -e RELAY_JWT_SECRET=replace-with-a-long-random-secret \
  -e HOST_ENROLL_SECRET=replace-with-another-long-random-secret \
  pie-relay-server:latest -addr :13412

go test -race ./...
go vet ./...
golangci-lint run ./...
govulncheck ./...
```

배포된 standalone Relay의 HTTPS/WSS 인증과 양방향 중계는
`cmd/relay-smoke`로 확인한다. Azure staging 주소와 실행 방법은
[`../deploy/azure/README.md`](../deploy/azure/README.md)를 참고한다.
