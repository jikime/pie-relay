# Pie Relay 및 Executor Manager 아키텍처

## 목표

Pie Relay는 Desktop/CLI participant, Local/Docker clientd와 모바일 Gateway의
실시간 연결을 담당하는 Data Plane이다. Executor Manager는 인증된 사용자의 작업과
Docker Executor 수명주기뿐 아니라 장치, 세션, 공유 권한과 Relay presence를 관리하는
Control Plane이다. 두 프로세스는 독립 배포하지만 scoped capability와 내부 control
API로 연동한다.

```text
Desktop/Mobile ─ participant ─┐
                              ├─ Pie Relay ─ host ─ Local clientd
Pie Admin ─ Pie Manager ──────┘                    └ Docker clientd 1..N
               │
           PostgreSQL
```

## 구성요소

### Pie Relay 서버

- Desktop/CLI용 session-scoped host/participant WebSocket 중계
- 모바일 Relay Director/Cell 및 Desktop Gateway control/data 중계
- 모바일 invite/resume credential 관리
- 모바일과 데스크톱 사이의 E2EE payload 전달
- 실제 Claude Code 작업을 실행하지 않음

### Desktop Host Gateway

- 사용자 PC에 설치되는 로컬 데몬
- LAN WebSocket endpoint 제공
- Relay host JWT로 Relay에 등록
- 모바일 세션을 로컬 터미널/Manager로 전달
- `automatic`, `local-only`, `relay-only` 연결 모드 지원

### Executor Manager

`executor-manager/`에 있는 Go Control Plane 서비스다.

- user → executor → job 매핑
- 사용자별 Executor 확보
- bounded job queue, worker, 프로비저닝 semaphore
- 격리된 Docker runtime과 레코드별 원자 registry
- 사용자별 blob quota, 읽기 전용 blob mount, 쓰기 가능한 workspace/state
- 정적 관리자 인증과 PAT introspection/소유권 검사
- 작업 timeout/cancel, event-driven SSE, graceful shutdown
- liveness/readiness와 Prometheus metrics
- User/Device/Runtime/Session/Participant/Grant/Operation/Audit registry
- PostgreSQL optimistic concurrency와 reconciliation
- session-scoped JWT 발급, Relay presence와 실제 연결/driver operation
- 운영자 RBAC와 내장 Pie Admin Web

구현 및 운영 설정은 [`executor-manager/README.md`](../executor-manager/README.md)를
기준으로 한다. 운영 환경은 PostgreSQL을 사용한다. 여러 Manager가 registry를 함께
읽을 수는 있지만 Docker operation의 단일 소유권은 현재 Manager ID/Docker host 단위로
유지한다. 여러 host placement에는 별도 node lease가 추가로 필요하다.

## 연결 모드

- `local-only`: LAN만 허용
- `relay-only`: Relay만 허용
- `automatic`: LAN을 우선 시도하고 실패하면 Relay fallback

연결 모드는 모바일 호스트 프로필에 보존되어 재접속 때도 동일하게 적용되어야 한다.

## 운영 경계

Pie Relay와 Kroot Relay는 계속 분리한다. Pie Relay는 interactive terminal Data Plane,
Kroot Relay는 브라우저 PAT 기반 작업 실행 경계다. 공통 인증 서버에서는 사용자 subject,
PAT 검증과 짧은 수명의 scoped delegation token 규격만 공유한다.
