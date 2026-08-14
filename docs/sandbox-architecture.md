# Pie 사용자 샌드박스 아키텍처와 검증 기록

> 최초 작성일: 2026-07-29  
> 문서 상태: 설계 기준 및 지속 갱신 문서  
> 현재 구현 기준: 단일 Docker Host의 사용자별 장기 실행 Executor 컨테이너

이 문서는 Pie를 제3자 애플리케이션에 제공할 때 사용하는 사용자 실행 공간을
`샌드박스`라는 상위 개념으로 정의하고, 현재 Docker 구현과 앞으로 확장할 구조를
구분해 기록한다. 새로운 구현이나 부하 테스트 결과가 나오면 이 문서의 상태표와
변경 이력을 함께 갱신한다.

인증정보 생성과 파일 배치의 상세 기준은
[사용자별 Executor 컨테이너·인증 프로비저닝 설계](./executor-container-auth-provisioning.md)를
따른다.
프로젝트 웹서버 공개 구조와 인증·네트워크 기준은
[Pie 프로젝트 웹 프리뷰 설계·운영 가이드](./project-preview-platform.md)를 따른다.

## 1. 먼저 구분할 용어

| 용어 | 이 프로젝트에서의 의미 |
|---|---|
| 샌드박스 | 사용자의 명령과 Claude Code를 다른 사용자 및 Host로부터 격리해 실행하는 논리적 실행 공간 |
| Docker 컨테이너 | 현재 샌드박스를 구현하는 런타임. Host 커널을 공유한다. |
| Executor | 컨테이너 안에서 명령, 프로젝트, PTY와 Claude Code 실행을 제공하는 Pie 실행 주체 |
| Workspace | 사용자 프로젝트 파일이 영속되는 전용 저장 경계 |
| Home/Auth State | `.claude`, `.kroot`, `.pie`처럼 사용자별 설정과 인증 상태가 영속되는 경계 |
| MicroVM 샌드박스 | Firecracker, Kata Containers 등 별도 커널 또는 VM 경계를 사용하는 향후 선택지 |

따라서 샌드박스와 Docker 컨테이너는 같은 말이 아니다. 샌드박스는 제품이 제공하는
격리 단위이고 Docker는 이를 구현하는 현재 기술이다. 필요해지면 Docker runtime을
gVisor, Kata Containers 또는 MicroVM 기반 adapter로 교체할 수 있어야 한다.

## 2. 현재 확정된 서비스 모델

현재 Pie의 기준 모델은 **사용자 1명당 전용 Executor 컨테이너 1개**다. 프로젝트마다
컨테이너를 새로 만드는 구조는 아니며, 한 사용자 컨테이너 안에 여러 프로젝트를
서로 다른 디렉터리로 둔다.

```text
외부 서비스 사용자 A
  └─ Pie Executor container A
       ├─ /home/executor          사용자 A 전용 Home/Auth State
       └─ /workspace/projects
            ├─ opaque-project-1   프로젝트 A-1
            └─ opaque-project-2   프로젝트 A-2

외부 서비스 사용자 B
  └─ Pie Executor container B
       ├─ /home/executor          사용자 B 전용 Home/Auth State
       └─ /workspace/projects     사용자 B 전용 Workspace
```

사용자 A가 사용자 B의 Managed Docker 컨테이너를 제어하는 것은 허용하지 않는다.
다른 사람의 PC에 직접 접속하는 공유 터미널 기능은 별도 Local/Host OS 세션이며,
Managed Docker 샌드박스의 소유자 전용 정책과 혼합하지 않는다.

## 3. 현재 요청과 실행 흐름

```text
제3자 Web/App
  │  외부 사용자 인증 및 Pie Integration Service Token
  ▼
Pie Manager / Control Plane
  │  사용자 매핑, 컨테이너 할당, Project·Session 생성
  ▼
사용자 전용 Docker Executor
  │  pie-relay-client(clientd), Claude Code, kroot
  ▼
Pie Relay ◀──────── HTTPS/WSS ──────── 제3자 Web/App
```

Relay는 컨테이너를 생성하거나 사용자를 스케줄링하지 않는다. Relay는 실시간
Data Plane이고, Manager가 사용자와 Executor의 소유 관계 및 수명주기를 관리한다.

## 4. 현재 구현과 검증 수준

상태 표시는 다음 의미로 사용한다.

- `검증`: 코드와 자동 테스트 또는 실제 Docker E2E 경로가 있다.
- `부분`: 구현 일부는 있으나 운영 요구 전체가 검증되지 않았다.
- `미구현`: 설계 항목이며 현재 고객 기능으로 간주하면 안 된다.

| 기능 | 상태 | 현재 근거와 한계 |
|---|---|---|
| 사용자별 단일 Executor 생성 | 검증 | 사용자 ID로 `executor-{userId}`를 멱등 생성한다. |
| CPU·메모리·PID 제한 | 검증 | Docker `--cpus`, `--memory`, `--pids-limit` 적용 및 inspect E2E가 있다. |
| 사용자별 Workspace/Home 분리 | 검증 | 사용자별 bind mount, `0700`, Executor UID/GID 소유권을 적용한다. |
| 기본 컨테이너 보안 강화 | 검증 | non-root, capability 전체 제거, `no-new-privileges`, read-only rootfs를 적용한다. |
| Project 생성과 `kroot init` | 검증 | 불투명 Project ID 경로와 컨테이너 내부 실제 초기화 E2E가 있다. |
| 프로젝트별 HTTPS 웹 프리뷰 | 로컬 검증 | clientd process supervisor, Preview Gateway, 사용자별 internal network와 공개·비공개 접근 E2E가 있다. 공인 wildcard TLS 운영 검증은 남아 있다. |
| Relay 기반 PTY·Claude 대화 | 검증 | host/participant, view/control, permission, 응답 왕복을 시험한다. |
| Relay·Manager 재시작 복구 | 검증 | clientd 재접속, session 복구, bounded replay 경로를 시험한다. |
| 사용자 A/B 격리 및 탈퇴 정리 | 검증 | 로컬 Docker E2E 항목에 포함된다. |
| 출력·업로드·queue backpressure | 검증 | 크기, 동시 연결 및 queue 상한이 존재한다. |
| 사용자별 감시형 디스크 쿼터 | 구현 | Workspace·Home·blob 합계를 주기적으로 검사해 초과 Executor를 정지하고 재실행을 거부한다. |
| 파일시스템 hard quota | 미구현 | 현재 host filesystem에 project quota가 없어 검사 사이의 추가 쓰기까지 즉시 막지는 못한다. |
| 유휴 컨테이너 자동 정지·재개 | 구현 | 기본 15분에 Claude/Relay 세션을 닫고 1시간에 Executor를 정지한다. 같은 Conversation 재시도 시 컨테이너와 세션을 자동 재생성한다. |
| 프로젝트별 일회성 컨테이너 | 미구현 | 현재 Project는 사용자 컨테이너 안의 디렉터리다. |
| 여러 Docker Node 간 자동 배치 | 미구현 | Node 모델은 있으나 capacity 기반 scheduler와 원격 Node Agent가 없다. |
| Node 장애 시 자동 재배치 | 미구현 | Workspace 이동, lease, fencing 및 reschedule이 필요하다. |
| 수백·수천 동시 Claude 실행 | 미검증 | 현재 접속 부하 테스트는 실제 동시 Claude workload 용량 시험이 아니다. |
| 악의적 코드에 대한 VM급 격리 | 미검증 | Docker는 Host 커널을 공유하며 침투 테스트를 대체하지 않는다. |

## 5. 현재 Docker 보안 경계

현재 적용하는 최소 기준은 다음과 같다.

- Executor는 고정된 non-root UID/GID로 실행한다.
- Linux capability를 모두 제거한다.
- `no-new-privileges`를 적용한다.
- root filesystem은 읽기 전용으로 둔다.
- `/tmp`만 크기가 제한된 `tmpfs`로 제공한다.
- 컨테이너 swap을 차단하고 private IPC, init, core dump·open file 제한을 적용한다.
- Docker 로그는 `local` driver의 순환 크기 제한을 적용한다.
- 운영 모드에서는 외부 egress는 허용하되 Executor 간 ICC를 끈 전용 bridge를 검증한다.
- 사용자 Workspace와 Home만 쓰기 가능하게 마운트한다.
- 업로드 blob은 읽기 전용으로 마운트한다.
- Executor에 Docker socket, `privileged`, Host network와 임의 Host 경로를 주지 않는다.
- 사용자·Manager·이미지 label이 다른 기존 컨테이너는 재사용하지 않는다.

이 기준은 일반적인 애플리케이션 오류와 사용자 간 파일 혼합을 막는 데 유효하지만,
신뢰할 수 없는 임의 코드를 실행하는 공개 서비스에서 VM과 동일한 경계를 보장하지는
않는다. 고객이 자유롭게 shell, 빌드 스크립트와 외부 바이너리를 실행할 수 있다면
seccomp/AppArmor, egress 정책, rootless 실행을 먼저 보강하고 위험도에 따라 gVisor,
Kata Containers 또는 MicroVM 격리를 검토한다.

Manager는 Docker daemon을 제어하므로 Docker socket이 외부 네트워크에 노출되면 안
된다. Manager 침해는 전체 Docker Host 침해로 확대될 수 있으므로 API 인증, 관리자
RBAC, 감사로그와 Host 접근통제를 별도의 보안 경계로 취급한다.

## 6. 이미지와 영속 데이터 원칙

```text
공용 불변 이미지
├─ pie-relay-client/clientd
├─ Claude Code 및 Agent SDK
├─ kroot
└─ 공용 도구

사용자별 영속 데이터
├─ Home/Auth State
│  ├─ .claude
│  ├─ .kroot/credential.json
│  └─ .pie/credential.json
└─ Workspace
   └─ projects/{opaque-project-id}
```

- PAT, Claude 로그인 상태와 사용자 파일을 공용 이미지 레이어에 넣지 않는다.
- 공용 Claude 인증을 사용하더라도 writable Home volume을 여러 사용자에게 공유하지
  않고 사용자별로 복제한다.
- 컨테이너를 재생성해도 사용자별 Home과 Workspace는 보존한다.
- 사용자 삭제는 실행 중 작업 중지, Relay session 종료, container 제거, 보존정책에
  따른 credential/Workspace 삭제 순서로 처리한다.
- Node 간 이동을 구현하기 전에는 bind mount가 특정 Host에 묶인다는 점을 운영자가
  명확히 알아야 한다.

## 7. 컨테이너 수와 서버 수의 관계

사용자 한 명당 물리 서버 한 대가 필요한 것은 아니다. 한 Docker Host에 여러 사용자
컨테이너를 배치한다. 필요한 서버 수는 가입자 수보다 **동시 활성 사용자 수와 실제
workload**에 의해 결정된다.

```text
노드 수 산정의 시작점 = max(
  동시 활성 CPU 수요 / 노드 가용 CPU,
  동시 활성 메모리 수요 / 노드 가용 메모리,
  사용자 데이터 / 노드 가용 디스크,
  실제 측정 IOPS·네트워크·API 한계
)
```

Docker의 CPU와 메모리 값은 현재 컨테이너별 상한이다. 상한을 설정했다고 물리 자원이
예약되거나 성능이 보장되는 것은 아니다. 지나친 overcommit은 Claude 작업이 동시에
몰릴 때 CPU throttling, OOM과 디스크 지연으로 나타난다.

서버별 목표 수용량은 다음 항목을 실제 대표 작업으로 측정한 뒤 정한다.

- 컨테이너 유휴 RSS와 Claude 실행 중 평균·최대 RSS
- 프로젝트 분석, 코드 생성, 빌드 및 테스트별 CPU 사용량
- CPU throttling과 OOM 횟수
- Workspace IOPS, inode와 Docker image/layer 사용량
- Relay 연결 수, 메시지 처리량과 p50/p95/p99 지연
- 컨테이너 cold start, warm start와 image pull 시간
- Claude 및 외부 API rate limit과 egress 비용

## 8. 목표 수명주기

향후 샌드박스는 다음 상태 전이를 기준으로 관리한다.

```text
requested
   ↓
provisioning → ready → active
                    ↘ idle
                       ↓ 유휴 정책
                     stopped
                       ↓ 새 요청
                    starting → active

모든 상태 → error → retry 또는 operator 조치
모든 상태 → deleting → deleted
```

2026-08-03 기준으로 `idle → stopped → starting` 자동 전이를 구현했다. 기본 정책은
Conversation 마지막 활동 15분 후 Claude/Relay 세션 종료, 활성 세션이 없는 Executor의
마지막 활동 1시간 후 컨테이너 정지다. 아래 조건을 구현 기준으로 적용한다.

1. 활성 chat, PTY, job 또는 permission 요청이 있으면 정지하지 않는다.
2. 마지막 활동 시각이 일정 시간을 넘은 컨테이너만 대상으로 한다.
3. 먼저 신규 작업 수락을 중지하고 진행 중 출력과 journal을 flush한다.
4. 프로세스를 종료하되 Home과 Workspace는 보존한다.
5. 새 요청에는 같은 사용자의 저장 경계를 다시 마운트하여 시작한다.
6. 시작 실패는 기존 Controller의 지수 backoff로 복구하고 수동 `retry` API도 제공한다.

## 9. 단계별 확장 계획

### 단계 A: 단일 Docker Host 완성

- 실제 디스크 쿼터 적용
- 유휴 정지와 요청 기반 재시작의 부하·cold-start SLA 검증
- seccomp/AppArmor, egress allowlist 및 secret rotation
- 실제 Claude workload를 사용한 동시 컨테이너 부하·장시간 시험
- 디스크 고갈, Docker daemon 재시작, OOM과 네트워크 단절 복구 시험

### 단계 B: 여러 Docker Host

```text
중앙 Pie Control Plane
  ├─ Scheduler / placement / lease
  ├─ PostgreSQL
  └─ Node Registry
       ├─ Node Agent A → Docker Host A
       ├─ Node Agent B → Docker Host B
       └─ Node Agent C → Docker Host C
```

- Node Agent가 capacity, usage와 heartbeat를 보고한다.
- Scheduler가 CPU·메모리·디스크·label을 기준으로 Node를 선택한다.
- 동일 Executor를 두 Node가 동시에 시작하지 않도록 lease와 fencing을 적용한다.
- drain 시 신규 배치를 막고 실행 중 세션과 Workspace 이전 정책을 수행한다.
- 사용자 데이터는 공유/분산 스토리지 또는 검증된 snapshot/restore 경로를 사용한다.

초기에는 직접 만든 Node Agent와 scheduler로 요구사항을 검증하고, 규모와 운영 복잡도가
커지면 Nomad 또는 Kubernetes adapter를 검토한다. Scheduler를 도입해도 사용자,
Integration, Project, Session과 권한을 관리하는 Pie Control Plane은 유지한다.

### 단계 C: 더 강한 격리

- 신뢰 수준이 낮은 workload를 별도 Node Pool로 분리한다.
- gVisor 또는 Kata Containers로 호환성과 성능을 검증한다.
- 공격면과 고객 계약 수준이 요구하면 MicroVM 기반 runtime을 검토한다.
- runtime이 달라도 사용자·Project·Session API는 유지하고 runtime adapter만 바꾼다.

## 10. 운영 전 필수 검증 행렬

| 축 | 반드시 시험할 값 |
|---|---|
| 사용자 | 최초 가입, 중복 가입, 정지, 재활성화, 탈퇴 |
| 컨테이너 | 생성, 중복 생성, 시작, 유휴 정지, 재시작, 재생성, 삭제 |
| 작업 | 짧은 명령, 장시간 Claude, 빌드, 대용량 출력, 취소, permission 대기 |
| 장애 | Relay 재시작, Manager 재시작, Docker 재시작, Node 종료, PostgreSQL 단절 |
| 자원 | CPU throttle, memory limit/OOM, PID limit, disk quota, inode 고갈 |
| 격리 | A/B 파일·credential·Project·session 교차 접근 거부 |
| 네트워크 | WSS 단절·재접속, 느린 회선, egress 차단, 외부 API timeout |
| 규모 | 동시 생성, 동시 활성 Claude, 다중 PTY 참가자, 장시간 soak |

부하 시험 결과에는 단순 성공 여부뿐 아니라 다음 값을 함께 남긴다.

- 시험일, Git revision, image digest와 Docker/Host 사양
- 활성/유휴 컨테이너 수와 대표 workload
- 성공률, p50/p95/p99 지연과 초당 처리량
- CPU, RSS, disk IOPS, network와 OOM/throttling
- 발견한 병목, 적용한 제한값과 다음 시험 조건

## 11. 현재 결정 사항과 미결정 사항

### 결정됨

- 현재 운영 단위는 사용자별 전용 Docker Executor다.
- Managed Docker Executor는 소유자만 사용한다.
- 사용자별 Home과 Workspace는 분리하고 컨테이너 재생성 시 보존한다.
- 공용 이미지는 불변으로 재사용하며 사용자 secret을 포함하지 않는다.
- Relay와 Manager의 역할을 분리한다.
- 단일 Host E2E 결과를 대규모 운영 용량으로 과장하지 않는다.

### 추가 결정 필요

- 유휴 정지 시간과 무료/유료 요금제별 보존 기간
- 사용자당 컨테이너 1개를 유지할지, 고급 요금제에 여러 Sandbox를 허용할지
- 실제 디스크 quota 구현 방식과 Workspace 백업 저장소
- Node 선택 알고리즘과 장애 시 자동 재배치 범위
- 공개 shell 실행에 필요한 최종 isolation runtime
- 사용자별 CPU·메모리 기본값과 동시 Claude 작업 상한

## 12. 변경 이력

| 날짜 | 변경 내용 | 검증 근거 |
|---|---|---|
| 2026-07-29 | 최초 문서화. 현재 Docker 구현과 목표 샌드박스 구조, 검증·미구현 경계를 분리 | `executor-manager` 전체 Go 테스트 통과, 로컬 Docker stack healthy 확인. 전체 Docker E2E의 운영 용량 검증과는 구분 |
| 2026-07-29 | 전용 테스트 서버와 기존 Traefik을 이용한 Sandbox 검증 및 Azure Relay 이전 계획을 별도 문서로 연결 | 서버 공개 80/443과 SSH 경로 읽기 전용 점검 |

## 관련 문서

- [Sandbox 테스트 서버 검증 및 Relay 이전 계획](./sandbox-test-server-plan.md)
- [사용자별 Executor 컨테이너·인증 프로비저닝 설계](./executor-container-auth-provisioning.md)
- [세션·실행 환경·Control Plane 정의](./session-runtime-and-control-plane.md)
- [Pie Relay Control Plane 및 관리 콘솔 설계](./control-plane-and-admin-console.md)
- [배포 및 운영 가이드](./deployment-and-operations.md)
- [DNS 없는 로컬 통합 환경](../deploy/local/README.md)
- [고객 배포 준비 상태와 릴리스 게이트](./release-readiness.md)
