# Executor Manager 운영 계획

## 사용자 및 작업 흐름

외부 회원 서비스가 `user.created` signed webhook을 보내면 Manager는 사용자별
Executor profile과 workspace를 예약한다. webhook이 지연되거나 누락되면 첫
작업 요청 시 PAT introspection 후 lazy provisioning한다.

```text
user.created webhook ─┐
                      ├─ Executor profile/workspace
첫 작업 요청 ─────────┘
                         ↓
                    Docker client
                         ↓
                    job stream/result
```

## 현재 구현된 운영 기능

- 레코드별 원자 executor/job registry와 완료 작업 retention
- 사용자별·전체 동시 작업 제한
- bounded queue와 backpressure
- Docker CPU/memory/PID/network 제한
- 작업 timeout과 cancel
- 컨테이너 health check 및 orphan cleanup
- 사용자별 blob quota와 읽기 전용 mount
- 변경 알림 기반 SSE와 bounded log capture
- PAT introspection, 소유권 검사, 짧은 TTL 인증 cache
- metrics, readiness, graceful shutdown
- PostgreSQL 기반 Control Plane과 optimistic concurrency
- 사용자·장치·runtime·session·participant·grant registry
- Docker desired/observed reconciliation과 다중 PTY Session Manager
- session-scoped Relay capability와 trusted presence
- 실제 participant 연결 종료, driver 인계·회수, session 재시작 operation
- Admin RBAC, 감사 로그와 내장 Pie Admin Web

## 다중 노드 확장 단계에서 필요한 기능

- Docker node placement lease와 leader election
- Redis/NATS 등의 분산 queue
- S3 호환 object storage와 만료 lifecycle
- retry policy와 idempotency key
- 외부 회원 webhook/PAT lifecycle과 secret rotation
- OpenTelemetry tracing 및 장기 audit retention/archiving

## 포트 정책

컨테이너 포트는 고정하지 않는다. Manager 제어 포트만 고정하고 Executor와의
통신은 내부 Docker network 또는 Unix socket을 우선 사용한다. 외부 노출이
필요한 포트만 Manager가 동적으로 할당하고 registry에 기록한다.

## 확장 단계

1. 단일 Manager + 단일 Docker host
2. PostgreSQL Control Plane + 단일 Docker host (현재 구현)
3. placement lease와 여러 Docker host
4. 분산 queue/노드 RPC가 실제로 필요할 때 NATS 또는 gRPC/protobuf 도입

Claude Code 인증 정보는 이미지에 bake하지 않고 런타임 secret으로 주입하며,
작업 종료 또는 컨테이너 삭제 시 폐기한다.
