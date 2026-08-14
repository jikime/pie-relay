# Pie Relay Control Plane 및 관리 콘솔 설계

이 문서는 Pie Relay의 Local/Docker 실행 공간, 개인/공유 세션, LAN/Relay 연결을
운영자가 중앙에서 관리할 수 있도록 Control Plane과 관리 페이지를 구성하는
방법과 현재 구현을 정리한다. 2026-07-24 기준으로 도메인 모델, Directory/PostgreSQL
저장소, RBAC, session capability, Docker reconciliation, Relay presence, Admin Web과
운영 operation이 구현됐다. 다중 Docker host scheduler와 외부 회원 서비스 연동은
별도 확장 단계다.

관련 문서:

- [세션·실행 환경·Control Plane 정의](./session-runtime-and-control-plane.md)
- [세션 모드, Docker 격리 및 상호 접속 설계](./session-modes-and-mutual-access.md)
- [Pie Relay 연결 구조와 사용 흐름](./how-to-connect.md)
- [Relay 및 Executor Manager 아키텍처](./relay-and-executor-manager.md)
- [Executor Manager 운영 계획](./executor-manager-operations.md)
- [인증 및 페어링 흐름](./relay-authentication.md)

## 결론

Traefik이나 Portainer만으로 Pie Relay의 관리 기능을 구현할 수는 없다. 권장
구성은 다음과 같다.

- **Pie Control Plane**: 사용자, 장치, 컨테이너, 세션, 권한과 할당 상태 관리
- **Pie Admin Web**: 운영자용 중앙 관리 페이지
- **Pie Relay**: 실제 터미널 입출력과 실시간 상태 전송
- **Traefik**: HTTPS/WSS, TLS 종료와 도메인 라우팅
- **Portainer**: 내부 운영자가 장애 시 사용하는 보조 도구
- **Prometheus/Grafana/OpenTelemetry**: 메트릭, 알림, 로그와 trace

Pie Manager가 상태를 변경하는 유일한 주체가 되어야 한다. Traefik, Portainer,
Grafana는 각각 네트워크, 비상 운영, 관측 역할만 담당한다.

## 현재 구현 범위

현재 `executor-manager`에는 다음 운영 기반이 있다.

- 사용자별 Executor 생성과 복구
- bounded queue와 worker
- 사용자별 동시 작업 제한
- 작업 timeout과 cancel
- Docker health check와 orphan cleanup
- PAT 검증과 소유권 확인
- 업로드 quota와 blob 보관
- SSE 상태 알림
- health, readiness, metrics와 graceful shutdown
- 외부 인증 사용자와 Admin viewer/operator/administrator RBAC
- Local/Docker 장치, runtime node/instance와 desired/observed reconciliation
- 터미널 session/participant와 viewer/controller/driver 권한
- 공유 grant와 session-scoped capability
- LAN/Relay 정책과 Relay presence
- PostgreSQL optimistic concurrency, 비동기 operation과 감사 로그
- 사용자·장치·세션·접속자·grant·컨테이너·노드 관리 Web

Directory Store는 단일 Manager 개발용이고 운영에서는 PostgreSQL을 사용한다.
현재 단일 Docker host의 operation 소유권은 한 Manager가 갖는다. 여러 Docker
host로 확장하려면 placement lease/leader election과 node scheduler가 추가로 필요하다.

## 시스템 영역 분리

```text
                         +----------------------+
                         | 외부 회원/인증 서비스 |
                         | PAT/OIDC/Webhook     |
                         +----------+-----------+
                                    |
                         +----------v-----------+
                         |       Traefik        |
                         | HTTPS/WSS/TLS/Route  |
                         +----+------------+----+
                              |            |
                    +---------v--+   +-----v------------+
                    | Pie Admin |   | Pie Control Plane |
                    | Web       |   | Manager/API       |
                    +-----------+   +----+-------+-------+
                                          |       |
                                    PostgreSQL  Docker API
                                          |
                                    Relay Director
                                          |
                                  +-------+-------+
                                  | Relay Cell(s) |
                                  +-------+-------+
                                          | WSS
                  +-----------------------+-----------------------+
                  |                       |                       |
             Local clientd        Docker Executor         Desktop/Mobile
               host 역할             host 역할           participant 역할
```

### Control Plane

Control Plane은 다음 질문에 대한 정답을 보관한다.

- 사용자가 어떤 장치를 소유하는가
- 사용자에게 어떤 컨테이너와 volume이 할당됐는가
- 어떤 세션에 누가 참가했는가
- 누가 view, control, driver 권한을 가졌는가
- runtime의 목표 상태와 실제 상태가 무엇인가
- 어떤 Relay Cell이 세션을 담당하는가

### Data Plane

Data Plane은 다음 실시간 데이터를 처리한다.

- PTY 출력과 keyboard 입력
- terminal resize
- snapshot과 sequence 복구
- heartbeat와 flow control
- participant roster와 driver lease

PTY payload를 Manager나 Admin Web이 중계해서는 안 된다. Manager는 권한과
세션 메타데이터만 관리하고 터미널 데이터는 Relay가 직접 전달한다.

### Management Plane

- 운영자 관리 페이지
- 감사 로그
- 메트릭, 로그, trace와 알림
- 컨테이너와 세션 운영 명령
- 장애 복구와 수동 개입

## Traefik 사용 범위

Traefik은 다음과 같이 고정된 Pie 서비스의 edge proxy로 사용하는 것이 좋다.

```text
admin-relay.cookai.dev -> Pie Admin Web
api-relay.cookai.dev   -> Pie Control API
relay.cookai.dev       -> Relay Director/Cell
```

적합한 역할은 다음과 같다.

- HTTPS와 WSS 인증서 종료
- HTTP와 WebSocket 라우팅
- 보안 header와 제한적인 인증 middleware
- access log와 고정 서비스 health check

Traefik Dashboard는 현재 router, service, middleware와 같은 네트워크 구성을
보여주는 화면이다. 사용자, 장치, 컨테이너 할당, 세션과 driver를 관리하는 Pie
제품 UI를 대신할 수 없다. Dashboard/API는 내부망에 두고 별도 인증으로
보호해야 한다.

Executor 컨테이너는 Relay로 outbound WebSocket 연결을 만들기 때문에 사용자별
host port를 열거나 컨테이너마다 Traefik route를 생성할 필요가 없다. 따라서
Traefik에는 Admin/API/Relay 같은 고정 upstream만 설정하고 Docker socket은
마운트하지 않는 편이 안전하다. Docker Provider를 사용해야 한다면 최소한
`exposedByDefault=false`를 적용하고 socket 접근 범위를 제한한다.

## Portainer 사용 범위

Portainer는 다음과 같은 내부 운영 작업에 유용하다.

- 컨테이너, image, volume, network 확인
- 컨테이너 로그와 resource 상태 확인
- 장애 시 수동 restart 또는 stop
- Docker 인프라 관리자 RBAC

하지만 Portainer는 Pie의 사용자, PAT, device, session, viewer/controller/driver,
공유 초대, Relay presence와 서비스 quota를 알지 못한다. 그러므로 운영자 전용
`break-glass` 도구로만 사용한다.

일반적인 상태 변경은 Pie Manager를 통해 실행한다. Portainer에서 직접 변경한
경우 Manager가 저장한 desired state와 Docker의 observed state가 달라질 수
있으므로 reconciliation과 감사 절차가 필요하다.

## 관리 페이지 정보 구조

Pie Admin Web은 Desktop 앱과 분리된 중앙 웹 애플리케이션으로 구성한다. Desktop
및 Mobile은 일반 사용자의 장치와 세션 UI를 담당하고 Admin Web은 전체 사용자,
노드, 컨테이너, Relay와 운영 정책을 담당한다.

### 대시보드

- 전체 및 활성 사용자 수
- 온라인 Local/Docker 장치 수
- 실행 중인 컨테이너 수
- 활성 세션과 participant 수
- Relay 연결 수와 Cell별 room 수
- LAN/Relay 사용 비율
- provisioning, queue, reconnect와 error 수
- Docker node별 CPU, memory, disk 사용률
- 최근 장애, OOM, health failure와 경고

### 사용자 및 할당

- 외부 사용자 ID와 내부 user ID
- 사용자 상태와 마지막 인증 시각
- PAT 등록/만료 상태와 마지막 rotation 시각
- 할당된 Local/Docker 장치
- workspace와 Claude 인증 volume
- CPU, memory, PID, disk와 session quota
- 컨테이너 생성, 중지, 재생성과 보존 정책

원본 PAT, Claude 인증 정보와 secret은 표시하지 않는다. 등록 여부, 만료 시각,
마지막 검증·회전 시각만 제공한다.

### 장치 및 Executor

- `deviceId`, owner와 장치 종류
- Local clientd 또는 Docker Executor 여부
- Docker node, container ID와 image version
- desired state와 observed state
- Docker health와 clientd 연결 상태
- Relay host 등록 여부와 마지막 heartbeat
- CPU, memory, network/block I/O와 PID
- 실행 중인 session 수

### 세션 및 접속자

- `sessionId`, owner와 실행 장치
- Local/Docker 실행 공간
- private/shared 참여 방식
- auto/LAN only/Relay only 정책
- 실제 선택된 transport와 Relay Cell
- participant 목록과 접속 시각
- view/control/driver 권한
- driver lease 만료 시각
- 연결과 재연결 횟수
- snapshot sequence와 마지막 입출력 시각

운영자는 participant 연결 종료, driver 회수·인계, 초대 취소, 세션 종료,
Relay 재연결 요청과 장치 drain을 수행할 수 있다. 브라우저에서 터미널을
확인해야 한다면 Admin API가 PTY를 직접 전달하지 않고 짧은 수명의 감사 가능한
participant capability를 발급해 브라우저가 Relay에 접속하게 한다.

### Node 및 Relay

- Docker host별 capacity와 할당 수
- 실행 컨테이너와 provisioning queue
- node ready, draining, unhealthy 상태
- Relay Cell별 connection, room과 session 수
- latency, reconnect와 slow-peer eviction 수
- heartbeat 누락과 최근 장애

### Operation 및 감사 로그

모든 상태 변경은 다음 정보를 남긴다.

- 실행 주체와 역할
- 실행 시각과 source IP
- 대상 user/device/container/session
- 명령 종류
- 이전 상태와 이후 상태
- request ID와 idempotency key
- 성공, 실패와 오류 사유

삭제, volume 폐기와 강제 종료는 별도 권한, 2단계 확인과 명시적인 보존 정책을
요구한다.

## 상태 모델과 reconciliation

`컨테이너가 실행 중`인 것만으로 장치가 온라인이라고 판단하면 안 된다. 상태는
다음 신호를 합성해 계산한다.

```text
runtimeRunning  Docker 프로세스가 실행 중인가
runtimeHealthy  Docker health check를 통과했는가
clientConnected clientd가 등록돼 있는가
relayRegistered Relay host 연결이 살아 있는가
lastHeartbeat   마지막 heartbeat는 언제인가
activeSessions  활성 세션은 몇 개인가
```

권장 상태 모델은 다음과 같다.

```text
desiredState: running | stopped | deleted

observedState:
unassigned -> provisioning -> starting -> online
                                      +-> degraded -> offline
online -> draining -> stopping -> stopped
모든 상태 -> error
```

Manager는 desired state를 PostgreSQL에 기록하고 Docker, clientd와 Relay의
observed state를 계속 비교해 실제 상태를 맞추는 reconciliation loop를 가진다.
Docker Events는 빠른 UI 갱신에 사용하지만 영구 source of truth로 사용하지
않는다. 이벤트 유실과 Manager 재시작에 대비해 주기적인 Docker inspect와
heartbeat 조회도 수행한다.

긴 작업은 HTTP 연결에서 완료될 때까지 기다리지 않는다.

```text
POST operation -> 202 Accepted + operationId
                       |
                       +-> worker/controller 실행
                       +-> SSE로 상태 갱신
                       +-> audit event 저장
```

## Relay Director와 Cell

여러 Relay Cell을 Traefik round-robin만으로 분배하면 host와 participant가 서로
다른 Cell에 들어갈 수 있다. 서로 다른 클라이언트이므로 일반적인 sticky cookie
역시 같은 Cell을 보장하지 못한다.

다음 Director/Cell 모델을 유지한다.

1. Director가 `deviceId + sessionId`를 특정 Cell에 할당한다.
2. host가 할당된 Cell에 접속해 세션을 등록한다.
3. pairing/capability에 `cellId`와 검증된 Cell endpoint를 포함한다.
4. participant도 Director가 지정한 같은 Cell에 접속한다.
5. Cell 장애 시 host가 Director를 통해 새 Cell에 다시 등록한다.
6. clientd의 snapshot/sequence로 participant 화면을 복구한다.

Traefik은 WSS를 선택된 서비스에 전달하는 역할만 맡는다. room ownership,
session assignment와 failover 정책은 Relay Director가 결정한다.

## 데이터 모델

관리 페이지와 다중 Manager를 위한 최소 엔터티는 다음과 같다.

| 엔터티 | 책임 |
|---|---|
| `users` | 외부 subject, 서비스 상태와 quota |
| `runtime_nodes` | Docker host 또는 향후 scheduler node |
| `devices` | Local clientd와 Docker Executor 장치 |
| `executor_instances` | 컨테이너와 image/runtime 상태 |
| `workspaces` | workspace/auth volume 소유권과 보존 정책 |
| `sessions` | 실행 공간, 공유 방식, transport 정책과 상태 |
| `participants` | 접속자, 역할, connection과 driver lease |
| `access_grants` | 대상 장치/세션별 view/control capability |
| `operations` | 비동기 운영 명령과 실행 결과 |
| `heartbeats` | 장치, runtime과 Relay presence |
| `audit_events` | 변경 불가능한 운영·보안 이력 |

DB 상태 변경에는 optimistic version 또는 row lock을 적용하고 외부 API의 재시도에
대비해 idempotency key를 사용한다. 상태 변경과 event 발행 사이의 유실을 막기
위해 transactional outbox도 고려한다.

## 인증과 권한

- 외부 회원 서비스가 user identity의 source of truth다.
- Manager는 PAT introspection 또는 서명된 webhook으로 외부 subject를 검증한다.
- 원본 PAT를 Executor와 Relay에 장기 전달하지 않는다.
- 검증 후 짧은 수명의 session-scoped capability를 발급한다.
- capability에는 owner, target device, session, access와 expiry를 포함한다.
- Relay는 클라이언트가 보낸 owner/from 값을 신뢰하지 않고 token claim으로
  라우팅 값을 결정한다.

Admin RBAC는 최소한 다음 역할을 분리한다.

- `viewer`: 조회만 가능
- `operator`: 일반적인 start/stop, session과 driver 운영
- `admin`: 사용자 할당, quota와 정책 변경
- `security-admin`: credential revoke, 감사와 보안 정책

## 컨테이너 보안 기준

- non-root 또는 rootless runtime
- privileged 모드 금지
- Docker socket mount 금지
- host 임의 경로 mount 금지
- 사용자별 workspace/auth volume 분리
- 가능한 경우 read-only root filesystem
- 불필요한 Linux capability 제거
- 기본 seccomp profile 유지
- CPU, memory, PID, disk와 session 수 제한
- tenant 간 network 기본 차단
- 제한된 outbound allowlist 검토

외부 불특정 사용자의 임의 명령 실행을 제공한다면 일반 Docker namespace보다
강한 gVisor, Kata Containers 또는 MicroVM 격리를 검토한다.

## 관측과 메트릭

Prometheus/Grafana는 추세, 용량과 알림에 사용하며 transactional 상태 변경 UI로
사용하지 않는다. OpenTelemetry는 API 요청부터 operation, Docker 작업, Relay
할당까지 trace를 연결하는 데 사용한다.

Prometheus metric label에 raw `userId`, `sessionId`, `containerId`를 넣으면
cardinality가 무한히 증가할 수 있다. 개별 객체의 상세 상태는 PostgreSQL과
Admin API에서 조회하고 메트릭에는 node, cell, state, operation type 같은 제한된
label만 사용한다. user/session correlation은 구조화된 log와 trace ID로 처리한다.

## 확장 전략

### 1단계: 단일 Docker host

- 단일 Go Manager
- REST command API와 SSE 상태 갱신
- Relay WebSocket data plane
- 개발 모드 Directory Store
- 운영 모드 PostgreSQL

### 2단계: 운영 안정화

- PostgreSQL registry
- reconciliation controller
- operation/outbox/audit
- object storage와 retention
- Admin Web과 RBAC
- metric, log, trace와 alert

### 3단계: 여러 Docker host

- runtime node registry와 heartbeat
- capacity 기반 placement
- node drain과 reschedule
- Manager lease 또는 leader election
- 필요 시 Redis/NATS event/queue
- 노드 간 통신이 필요할 때 gRPC/protobuf

### 4단계: Scheduler 도입 검토

사용자별 장기 실행 컨테이너가 여러 노드에 대량 배치되면 Nomad 또는 Kubernetes를
검토한다. Nomad는 상대적으로 단순한 Docker workload scheduler로 사용할 수
있다. Kubernetes는 더 폭넓은 생태계와 정책을 제공하지만 멀티테넌시를 위해
namespace, RBAC, NetworkPolicy, quota와 sandbox를 함께 설계해야 한다.

Scheduler를 도입해도 Pie Manager는 제거하지 않는다. Manager는 Pie 도메인의
Control Plane으로 남고 Docker runtime adapter만 Nomad/Kubernetes adapter로
대체한다.

## 구현 순서

1. `User -> Device -> Runtime -> Session -> Participant` 도메인 모델 확정
2. PostgreSQL registry, operation과 audit 모델 도입
3. 외부 인증 연동과 Admin RBAC 구현
4. Docker desired/observed state controller 구현
5. Device Registry와 heartbeat/presence 구현
6. `clientd`에 다중 PTY Session Manager 구현
7. Relay를 `deviceId + sessionId` 기준으로 분리
8. session-scoped view/control/driver capability 구현
9. 별도 Pie Admin Web 구현
10. Local/Docker, LAN/Relay, private/shared 조합 E2E 및 부하 테스트

## 필수 테스트 행렬

기능 테스트는 다음 축을 모두 조합해야 한다.

```text
실행 공간: Local | Docker
접근 방식: private | shared
전송 경로: LAN only | Relay only | auto fallback
권한: view | control | driver
상태: 최초 접속 | 재접속 | token 만료 | host 재시작 | Relay 재시작
```

장애 및 부하 테스트에는 다음 항목을 포함한다.

- 다수의 idle WebSocket과 active terminal 세션
- 동일 세션의 다수 viewer와 slow peer
- 중복 device/session 등록
- Manager, Relay와 Docker daemon 재시작
- LAN에서 Relay로 전환 및 네트워크 단절
- container OOM, health failure와 disk quota 초과
- expired/revoked capability와 권한 상승 시도
- node drain 중 새 세션 차단과 기존 세션 종료
- operation 재시도와 idempotency 검증
- Docker event 유실 후 reconciliation 복구

## 외부 참고 자료

- [Traefik API & Dashboard](https://doc.traefik.io/traefik/operations/dashboard/)
- [Traefik WebSocket](https://doc.traefik.io/traefik/v3.4/user-guides/websocket/)
- [Traefik Docker Provider](https://doc.traefik.io/traefik/v3.3/providers/docker/)
- [Docker Engine API](https://docs.docker.com/reference/api/engine/)
- [Docker Events](https://docs.docker.com/reference/cli/docker/system/events/)
- [Docker Container Stats](https://docs.docker.com/reference/cli/docker/container/stats/)
- [Docker Volumes](https://docs.docker.com/engine/storage/volumes/)
- [Docker Rootless Mode](https://docs.docker.com/engine/security/rootless/)
- [Docker Seccomp](https://docs.docker.com/engine/security/seccomp/)
- [Portainer Access Control](https://docs.portainer.io/advanced/access-control)
- [Portainer Roles](https://docs.portainer.io/admin/user/roles)
- [Prometheus와 Grafana](https://prometheus.io/docs/visualization/grafana/)
- [OpenTelemetry Signals](https://opentelemetry.io/docs/concepts/signals/)
- [Nomad Docker Task Driver](https://developer.hashicorp.com/nomad/docs/job-declare/task-driver/docker)
- [Kubernetes Multi-tenancy](https://kubernetes.io/docs/concepts/security/multi-tenancy/)
