# Pie Relay 안정화 설계 및 운영 기준

이 문서는 Pie Relay의 일반 데스크탑/CLI 터미널 경로를 여러 사용자가 함께 쓰는 서비스로 운영하기 위한 기준을 정리한다. 모바일 LAN 및 모바일 Relay 프로토콜은 별도 경로이며 이번 변경으로 wire contract를 바꾸지 않는다.

## 목표 동작

- 하나의 `clientd`가 실제 PTY/Claude Code 프로세스를 소유한다.
- 여러 데스크탑 참가자가 같은 방의 출력을 동시에 본다.
- `view` 사용자는 화면과 사람 간 채팅만 사용할 수 있다.
- `control` 사용자는 조작권을 요청할 수 있지만, 실제 raw terminal 입력자는 항상 한 명이다.
- 새 연결이나 재접속은 기존 조작권을 빼앗지 않는다. 비어 있는 경우에만 첫 control 참가자가 자동 획득한다.
- 네트워크 단절·느린 뷰어·프레임 누락 이후에는 headless terminal 스냅샷으로 화면을 복구한다.

동시에 여러 사람이 raw keystroke를 섞는 방식은 의도적으로 지원하지 않는다. 셸 line editor와 TUI 상태를 비결정적으로 망가뜨리기 때문이다. 협업은 다중 관찰자 + 단일 driver lease + 명시적 handoff 방식이다.

## 구현된 10단계

1. 선택적 protocol v2 `relay_join`/`relay_join_ack`를 추가했다. 기존 프레임은 그대로 동작한다.
2. 연결별 송신 큐를 control/output 두 lane으로 분리하고 메시지 수와 총 byte를 함께 제한했다.
3. 연결별 frame/byte token bucket, 방 참가자 상한, `/metrics`를 추가했다.
4. clientd의 PTY→WebSocket 쓰기를 제한 큐와 10초 write timeout으로 분리했다.
5. PTY output에 `streamId`, `incarnationId`, `seq`를 넣고 authoritative `pty_snapshot`을 추가했다.
6. 데스크탑 xterm 입력을 16KiB 단위로 스케줄링하고 backlog 시 reset, parser stall 시 xterm 전체 remount 후 snapshot 복구한다.
7. 참가자 roster와 단일 driver lease(20초, 5초 heartbeat), 명시적 handoff를 추가했다.
8. JWT에 issuer, audience, jti, role, capability를 강제한다. 무-scope 토큰은 명시적 마이그레이션 옵션에서만 허용한다.
9. `/v1/relay/assignment`, node id, readiness drain, SIGTERM graceful shutdown 경계를 추가했다.
10. slow peer, byte overflow, sequence gap, PTY snapshot, 128-viewer fanout, race 테스트를 추가했다.

## 데이터 흐름과 복구

```text
PTY process
  │ node-pty output
  ▼
pty-host (8ms / 16KiB batching + headless xterm state)
  │ NDJSON: incarnationId + seq
  ▼
clientd (bounded writer + per-write timeout)
  │ WebSocket /ws/agent
  ▼
Pie Relay (rate limit + priority queues + fanout)
  │ WebSocket /ws/participant
  ▼
Desktop (sequence check + bounded xterm scheduler)
```

데스크탑이 sequence gap이나 incarnation 변경을 발견하면 이후 delta를 그리지 않고 `request_screen`을 보낸다. 호스트는 pending output을 먼저 flush하고 headless xterm의 직렬화 결과와 그 시점의 sequence를 `pty_snapshot`으로 응답한다. 데스크탑은 기존 상태를 reset한 뒤 snapshot부터 다시 적용한다.

## 운영 설정

| 환경 변수 | 기본값 | 설명 |
|---|---:|---|
| `RELAY_MAX_PARTICIPANTS_PER_ROOM` | `64` | 방별 participant socket 상한 |
| `RELAY_FRAMES_PER_SECOND` | `240` | 연결별 inbound frame/s, burst는 2배 |
| `RELAY_BYTES_PER_SECOND` | `8388608` | 연결별 inbound byte/s, burst는 2배 |
| `RELAY_NODE_ID` | hostname | 노드 배정 및 진단용 안정적 ID |
| `RELAY_PUBLIC_URL` | 요청 Host | assignment/mobile에 광고할 공개 HTTP(S) origin |
| `PIE_CONTROL_PLANE_URL` | 없음 | Relay presence를 받을 Manager origin |
| `PIE_CONTROL_PLANE_TOKEN` | 없음 | trusted presence bearer token |
| `PIE_RELAY_CONTROL_URL` | 공개 URL | Manager가 호출할 Relay 내부 control origin |
| `PIE_RELAY_CONTROL_TOKEN` | 없음 | 연결 종료/driver operation 전용 bearer token |
| `PIE_RELAY_METRICS_TOKEN` | 없음 | `/metrics` 전용 bearer token. 미설정 시 endpoint는 404 |
| `RELAY_ALLOWED_ORIGINS` | Tauri/local 개발 origin | cross-origin WebSocket 허용 origin pattern 목록 |
| `RELAY_ALLOW_LEGACY_QUERY_TICKET` | `false` | URL에 JWT가 남는 구버전 인증의 명시적 마이그레이션 스위치 |
| `RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS` | `false` | `iss/aud/jti/role/cap` 없는 구형 JWT의 임시 마이그레이션 스위치 |

주요 metric은 `pie_relay_connections`, `pie_relay_rooms`, `pie_relay_participants`, `pie_relay_frames_in_total`, `pie_relay_bytes_in_total`, `pie_relay_rate_limited_total`, `pie_relay_slow_peer_evicted_total`, `pie_relay_room_rejected_total`이다.

## 수평 확장 경계

방의 host와 participants는 반드시 같은 Relay 노드에 붙어야 한다. 현재 room registry, invite code, driver lease는 노드 메모리에 있으므로 임의 round-robin만 적용하면 방이 갈라진다.

운영 순서는 다음과 같다.

1. 인증된 클라이언트가 `/v1/relay/assignment`에서 node/WS URL을 받는다.
2. load balancer는 room 또는 assignment 결과에 따라 sticky routing한다.
3. 신규 scoped token을 외부 control plane이 발급할 때 선택적으로 `relay_node` claim을 넣는다.
4. 서버는 다른 node에 고정된 token을 HTTP 409로 거절한다.
5. 배포 종료 시 `/readyz`가 먼저 503이 되고 assignment/신규 WS가 차단된다. 기존 연결은 최대 20초 drain한다.

여러 Relay 노드 사이에서 방을 자유롭게 이동하려면 다음 단계로 Redis/NATS 같은 외부 presence/control bus와 snapshot 저장소가 필요하다. 현재 구현은 그 확장 경계를 API와 token에 명시했으며, 노드 사이에 terminal byte stream을 무조건 복제하지 않는다.

## 인증과 E2EE 경계

host/participant token은 `iss=pie-relay`, `aud=pie-relay`, `jti`, `role`, `cap`을 가진다. host와 participant 연결 capability가 분리되어 participant token으로 `/ws/agent`에 접속할 수 없다. scope 없는 예전 token은 기본 거부한다. 전환 기간에만 `RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS=true`로 허용하고, 발급자 전환과 credential 재발급이 끝나면 반드시 다시 끈다.

Desktop participant 연결은 `Sec-WebSocket-Protocol: pie-relay.ticket.<JWT>`로 인증한다. 따라서 JWT가 URL, reverse proxy access log, 브라우저 방문 기록에 남지 않는다. `?ticket=` 방식은 기본 차단하며, 구버전 마이그레이션 시에만 `RELAY_ALLOW_LEGACY_QUERY_TICKET=true`로 명시적으로 허용한다.

Protocol v2에는 Relay가 읽지 않는 `sealed` payload 경계가 있다. 실제 E2EE를 켜려면 외부 control plane이 endpoint public key를 확인하고 session key를 전달해야 한다. Relay가 JWT나 invite code 안의 대칭키를 직접 발급·열람하는 방식은 E2EE가 아니므로 구현하지 않았다. 현재 terminal payload 자체는 TLS 구간 암호화이며 sealed mode는 아직 기본 활성화되지 않는다.

## 검증 명령

```bash
cd server && go test ./... && go test -race ./...
cd client && go test ./... && go test -race ./internal/ptyagent
cd client/node-executor && npm ci --legacy-peer-deps=false && npm test
cd desktop && npm test -- --run && npm run build
```

부하 기준선은 다음 명령으로 확인한다.

```bash
cd server
go test ./internal/relay -run '^$' -bench 'BenchmarkFanoutTerminalOutput' -benchmem
```

실서비스에서는 뷰어 수가 늘 때 outbound bandwidth가 `PTY 출력량 × 뷰어 수`로 증가한다. CPU보다 NIC egress가 먼저 병목이 될 수 있으므로 frames/bytes/slow-peer metric과 노드 네트워크 사용량을 함께 경보해야 한다.
