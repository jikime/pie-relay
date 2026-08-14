# Pie Sandbox 테스트 서버 배포·검증 기록

> 최초 작성 및 최근 검증: 2026-07-29  
> 대상 서버: `221.143.48.77`  
> SSH alias: `pie-sandbox-test`  
> 문서 상태: 공유 서버 Staging 배포 및 공식 Relay 주소 전환 완료  
> 공식 Relay: `https://relay.cookai.dev`

이 문서는 첨부 사양의 5번 서버에 Pie Relay, Manager와 사용자별 Docker Executor를
실제로 배포해 검증한 결과를 기록한다. 이 서버에는 기존 Kroot 서비스가 함께 실행
중이므로, 아래의 합격은 **제한된 Staging 합격**을 뜻하며 고객용 Production 합격을
뜻하지 않는다.

실제 비밀번호, private key, PAT, Claude OAuth, Relay secret과 Integration token은
문서·Git·명령 출력에 기록하지 않는다. 상위 구조는
[Pie 사용자 샌드박스 아키텍처와 검증 기록](./sandbox-architecture.md)을 함께 참고한다.

## 1. 이번 검증의 결론

- DNS, 공인 TLS와 WSS를 포함한 `relay.cookai.dev` 공식 주소 전환을 완료했다.
- 제3자 API → Manager → Relay → 사용자 컨테이너 clientd → 실제 Claude Code → 제3자
  API 응답 경로가 동작한다.
- 사용자 두 명에게 서로 다른 Executor, Home, Workspace와 인증 상태를 할당했고 두
  사용자의 실제 Claude 요청을 동시에 처리했다.
- Relay·Manager·PostgreSQL 중단 및 Executor 재생성 뒤 데이터와 연결 복구를 확인했다.
- Executor 간 직접 통신과 다른 사용자 Workspace 접근을 차단했다.
- 현재 활성 Executor 상한은 공유 서버 보호를 위해 **2개**다. 2명을 넘는 실제 용량은
  아직 검증하지 않았으며 고객 수용량으로 약속하면 안 된다.
- Azure Relay는 중지하거나 삭제하지 않았다. 공식 주소 장애 시 되돌릴 수 있는
  rollback 대상이다.

## 2. 실제 서버 인벤토리

| 항목 | 확인값 | 운영 판단 |
|---|---|---|
| OS | Rocky Linux 9.7 | 지원 가능한 RHEL 계열 |
| SELinux | Enforcing | 유지해야 함 |
| CPU | Intel Core i9-14900T, 24 core / 32 thread | 초기 Staging에는 충분, 실제 Claude 동시 부하 측정 필요 |
| Memory | 약 124 GiB | 물리 용량만으로 동시 사용자 수를 확정할 수 없음 |
| Root NVMe | 약 1 TB | Docker image/cache와 기존 서비스가 함께 사용 |
| `/home` | 약 1 TB | Pie 영속 데이터의 현재 위치 |
| `/backup` | 약 2 TB | 현재 Pie 계정 쓰기 불가, 같은 장비라 off-host backup이 아님 |
| Docker | 29.2.0 | 정상 |
| Docker Compose | v5.0.2 | 정상 |
| 기존 컨테이너 | 조사 시 95개, 그중 92개 실행 | 전용 호스트가 아니므로 비간섭 원칙 필수 |
| 기존 Proxy | Traefik 3.6 `kroot-shared-lb` | 재기동 없이 Docker label로 Pie router 추가 |
| 공개 ingress | 기존 80/443 | Pie 내부 포트를 host에 publish하지 않음 |

SSH는 운영자 로컬의 다음 alias를 사용한다. 실제 key 경로는 개인 설정이므로 저장소에
기록하지 않는다.

```sshconfig
Host pie-sandbox-test
  HostName 221.143.48.77
  User kaonkroot
  Port 2733
  ServerAliveInterval 30
  ServerAliveCountMax 3
```

## 3. 식별자와 실제 배치 경계

| 범위 | 기준값 |
|---|---|
| Compose project | `pie-sandbox-test` |
| Executor Node ID | `sandbox-node-01` |
| Manager ID | `sandbox-node-01` |
| 서버 작업 root | `/home/kaonkroot/pie-sandbox-test` |
| 배포 파일 | `/home/kaonkroot/pie-sandbox-test/src/deploy/test-server` |
| 영속 데이터 | `/home/kaonkroot/pie-sandbox-test/data` |
| 공유 edge network | `kroot-shared-edge-network` |
| Executor network | `pie-sandbox-test-executor` |
| 테스트 Relay | `https://relay-test.cookai.dev` |
| 공식 Relay | `https://relay.cookai.dev` |
| Manager API | `https://api-relay-test.cookai.dev` |
| Admin | `https://admin-relay-test.cookai.dev/admin/` |
| 공식 Manager API | `https://api-relay.cookai.dev` |
| 공식 Admin | `https://admin-relay.cookai.dev/admin/` |

DNS 여섯 이름은 `221.143.48.77`로 해석되며 Traefik의 Host router가 서비스를 구분한다.
`relay-test.cookai.dev`는 진단용 별칭으로 남겨 두지만, 신규 세션과 앱의 기본값은
`relay.cookai.dev`다.

## 4. 배포 구조

```text
Internet HTTPS/WSS
        │
기존 Traefik :80/:443
  ├─ 기존 Kroot routers                       변경하지 않음
  ├─ relay.cookai.dev / relay-test.cookai.dev → Pie Relay :13412
  ├─ api-relay-test.cookai.dev                → Pie Manager :19090
  └─ admin-relay-test.cookai.dev              → Pie Manager /admin/

Pie Compose project: pie-sandbox-test
  ├─ Relay       read-only root, 0.5 CPU, 256 MiB
  ├─ Manager     read-only root, 1 CPU, 1 GiB, Docker socket
  └─ PostgreSQL  1 CPU, 2 GiB, named volume

사용자별 Executor
  ├─ UID/GID 10001:10001, capability ALL drop, no-new-privileges
  ├─ read-only root + 제한된 tmpfs
  ├─ 1 CPU, 4 GiB, PID 256
  ├─ 전용 Home/Auth/Workspace 경계
  └─ 공개 `wss://relay.cookai.dev/ws/agent`로 outbound 연결
```

Relay는 Executor network에 참여하지 않는다. Executor network는 Docker option
`com.docker.network.bridge.enable_icc=false`로 만들었으므로 사용자 컨테이너끼리
내부 IP로 직접 통신할 수 없다. Manager와 Relay의 내부 제어 통신만 `control`
network를 사용한다. Relay `13412`, Manager `19090`, PostgreSQL `5432`와 Executor
포트는 host에 publish하지 않았다.

Manager가 받는 Docker socket은 사실상 host-root 권한이다. 현재는 강하게 인증된
Staging Manager만 접근하지만, Production에서는 전용 Executor Node 또는 제한된
Docker API proxy/runtime broker로 이 권한을 줄여야 한다.

## 5. 공식 도메인 전환 결과

2026-07-29에 다음 값을 공식 주소로 변경했다.

```dotenv
RELAY_PUBLIC_URL=https://relay.cookai.dev
PIE_EXECUTOR_RELAY_URL=wss://relay.cookai.dev/ws/agent
```

Relay와 Manager만 재생성했으며 전환은 약 11초가 걸렸다. PostgreSQL과 사용자
영속 데이터는 중단하지 않았다. 두 사용자 Executor는 Manager의
`runtime.recreate` operation으로 순차 재생성했고 Home, Workspace, Kroot/Claude
credential digest와 프로젝트가 보존됐다.

확인 결과:

- `https://relay.cookai.dev/readyz`: HTTP 200
- Relay의 광고 주소: `https://relay.cookai.dev`
- Manager의 Executor 주소: `wss://relay.cookai.dev/ws/agent`
- 모바일 `/v1/assign`의 `cellUrl`: `https://relay.cookai.dev`
- 공인 인증서 만료일: 2026-10-27
- 실제 Claude marker 응답: 공식 Relay를 통해 성공
- 공식 Manager API의 무인증 Admin 요청 401, 관리자 인증 요청 200
- 공식 Admin 페이지 200 및 공식 Admin origin CORS preflight 204
- 공식 Manager API → 공식 Relay → 실제 Claude marker 응답 성공

전환 전 환경 파일은 서버의
`.env.pre-cookai-cutover-20260729`에 mode를 유지해 보관했다. 이 파일도 secret이므로
외부로 복사하거나 Git에 넣지 않는다.

## 6. 실제 E2E 검증 결과

| 범주 | 실제 확인 내용 | 결과 |
|---|---|---|
| HTTPS/WSS | health, ready, 공인 TLS, WebSocket upgrade | 통과 |
| Relay 인증 | 잘못된 enroll secret, 보호된 metric, Origin 차단 | 통과 |
| 공유 터미널 | invite/join, view/control 역할 격리, 양방향 PTY | 통과 |
| 재연결 | host와 participant WSS 재연결 | 통과 |
| 모바일 Relay | session credential, `/v1/assign`, 공식 `cellUrl` | 통과 |
| 사용자 생성 | Alice/Bob Integration User와 전용 Executor 생성 | 통과 |
| 컨테이너 자격 | `kroot`, `.kroot/credential.json`, Claude credential, UID 10001 | 통과 |
| Project | 컨테이너 안에서 실제 `kroot init`, 전용 작업 경로 | 통과 |
| Claude text | 제3자 API부터 실제 Claude Code 응답 왕복 | 통과 |
| 이미지 | PNG 첨부 후 실제 Claude 이미지 응답 | 통과 |
| Permission | 요청 수신, 중복 turn 409, 승인 후 파일 생성 | 통과 |
| 동시 사용자 | Alice/Bob 실제 Claude 응답 동시 완료 | 2명 통과 |
| 데이터 격리 | Alice Workspace를 Bob이 볼 수 없음 | 통과 |
| 네트워크 격리 | Executor A→B 직접 HTTP 차단, 공개 Relay egress 허용 | 통과 |
| 권한 격리 | 다른 Integration token으로 Alice 대화 접근 시 403 | 통과 |
| Secret 누출 | Admin 응답·Relay/Manager log에서 token/PAT 패턴 없음 | 통과 |
| Capacity | 활성 2개 상태에서 세 번째 provisioning 429 | 통과 |
| Relay 재시작 | clientd 자동 재연결, 약 36초 내 복구 | 통과 |
| Manager 재시작 | API와 상태 약 10초 내 복구 | 통과 |
| PostgreSQL 단절 | ready 503 후 재기동 약 1초 내 상태 복구 | 통과 |
| Runtime 재생성 | container ID 변경, Home/Workspace/Auth 보존, 후속 chat | 통과 |
| 기존 서비스 비간섭 | `www.kroot.io` 200, API 401, Auth 302 기준 유지 | 통과 |

AI 컨테이너 대화는 `private/owner-only`이므로 그 호스트 자격으로 invite 생성 시 403이
정상이다. 사용자 B가 사용자 A의 컨테이너를 제어하지 못하게 하기 위한 경계다. 여러
사람이 같은 터미널을 보는 기능은 별도의 `shared` Host OS 세션에서만 invite와
Driver 정책을 적용한다.

## 7. 코드 회귀 검사 결과

공식 주소 전환 후 다음 검사를 다시 실행했다.

- Server, Manager, clientd: 전체 Go test 통과
- 세 Go 모듈: race test, `go vet`, `golangci-lint`, `govulncheck` 통과
- Desktop: 177 test 및 production build 통과
- Desktop Tauri: Rust format, Clippy warning 0, Cargo test 통과
- Mobile Host Gateway: 8 test, typecheck, build 통과
- Mobile App: 1,717 test 통과, 2 skip, typecheck 통과
- 제3자 Next.js 예제: 14 test, typecheck/check, Next.js 16.2.12 production build 통과
- Node Executor, Desktop, Mobile Gateway, Next.js 예제 package audit: 알려진 취약점 0건
- Mobile App audit: 저장소의 제한 patch와 선행 검증을 적용한 고위험 advisory 1건만
  명시적으로 ignore

`pielab.ai`와 Azure 기본 Relay를 가리키던 활성 기본값은 `cookai.dev`로 교체했다.
Azure 배포 기록의 Azure 언급과 GitHub module 경로 `github.com/pielab-ai/...`는
도메인 기본값이 아니므로 그대로 유지한다.

## 8. 현재 자원 정책

| 항목 | Staging 값 |
|---|---:|
| 사용자 CPU hard limit | 1 CPU |
| 사용자 memory hard limit | 4 GiB |
| 사용자 PID limit | 256 |
| 사용자당 활성 chat turn | 1 |
| Executor queue | 16 |
| Worker | 2 |
| Provision 동시성 | 1 |
| Node 활성 Executor 상한 | 2 |

124 GiB 메모리가 있다고 해서 `124 / 4 = 31명`으로 계산하면 안 된다. Claude 프로세스,
Node.js, 파일 처리, Docker cache, PostgreSQL과 기존 Kroot workload가 함께 자원을
사용한다. 4명 이상의 실제 Claude 동시 부하, p95/p99, OOM/PSI와 8시간 soak 결과가
나오기 전에는 상한 2를 올리지 않는다.

## 9. 공유 서버라 수행하지 않은 시험

다음 시험은 기존 Kroot 서비스 장애를 만들 수 있어 의도적으로 수행하지 않았다.

- Docker daemon 재시작
- 물리 서버 재부팅
- filesystem disk-fill/inode-fill
- Host OOM과 강제 memory pressure
- 광범위한 image/container/network/volume prune
- 4명 이상 실제 Claude 동시 부하와 8시간 soak

이 항목은 전용 Staging/Executor Node에서만 수행한다. “하지 않은 시험”을 합격으로
표시해서는 안 된다.

## 10. Production 전 필수 보강 항목

### P0: 고객 공개를 막는 항목

1. **Claude 인증 수명과 회전**  
   이번 시험은 한 Claude 계정의 OAuth credential을 사용자별 상태에 복제했다. access
   token 만료와 refresh token 회전이 여러 컨테이너에서 경쟁하면 일부 사용자가 갑자기
   인증 실패할 수 있다. 서비스가 허용하는 사용자별 인증 또는 중앙 credential broker와
   원자적 회전 절차를 확정해야 한다.
2. **Docker socket 경계**  
   Manager 침해가 곧 host 전체 침해가 될 수 있다. 고객 환경은 기존 Kroot와 분리한
   전용 Node를 쓰고 socket proxy/runtime broker, 최소 API와 네트워크 정책을 적용한다.
3. **암호화 off-host backup/restore**  
   PostgreSQL, Workspace, 사용자 auth state와 Relay mobile state를 다른 장비/object
   storage에 암호화 백업하고 실제 복원 시험을 통과해야 한다.
4. **사용자별 byte/inode quota**  
   현재 20 GiB 정책은 문서값이며 filesystem에서 강제되지 않는다. 한 사용자가 공유
   Docker data-root를 고갈시키지 못하게 project quota 또는 별도 volume backend가 필요하다.
5. **Admin 외부 노출 축소**  
   현재 bearer 인증은 동작하지만 Production Admin은 VPN/IP allowlist 또는 SSO/MFA 뒤에
   두어야 한다.
6. **실제 회원 서비스 인증 연결**  
   현재 Staging E2E는 시험용 Integration과 credential로 검증했다. 고객 공개 전에는
   실제 제3자 Backend가 service token을 secret manager에 보관하고, 회원 lifecycle
   재전송·폐기와 PAT active/inactive/timeout/revocation을 별도 Staging에서 통과해야 한다.
7. **깨끗한 Production 데이터 경계**  
   현재 DB에는 Alice/Bob과 capacity 거부용 Charlie 시험 레코드가 남아 있다. 이 volume을
   Production 정본으로 승격하지 말고, 배포 이미지 digest를 고정한 새 DB/데이터 root에서
   시작한다.

### P1: 운영 안정성

- Prometheus에 bearer-auth Relay/Manager metric scrape와 경보 규칙 추가
- 구현된 유휴 Executor stop/reaper의 운영 재기동 SLA와 cold-start 측정
- 실제 1→2→4→8 사용자 단계 부하와 8시간 soak
- image registry, immutable digest, SBOM, 서명 및 Critical/High CVE gate
- PostgreSQL PITR, migration rollback과 정기 복원 훈련
- 인증서 만료, disk 70/80/90%, OOM, 재연결, queue와 429 경보
- 감사 로그 보존 기간과 개인정보/credential 삭제 정책

### P2: 확장 단계

- 다중 Executor Node scheduler와 admission/capacity heartbeat
- Node drain 및 사용자 Workspace 이전 정책
- 다중 Relay Cell assignment/sticky routing
- Manager/Relay HA와 공유 상태 저장소
- Docker보다 강한 격리가 필요할 때 gVisor/Kata/Firecracker adapter

## 11. 안전한 운영 명령

```bash
ssh pie-sandbox-test
cd /home/kaonkroot/pie-sandbox-test/src/deploy/test-server
docker compose --env-file .env config --quiet
docker compose --env-file .env ps
curl --fail https://relay.cookai.dev/readyz
curl --fail https://api-relay-test.cookai.dev/readyz
```

기존 서비스와 공존하므로 `docker system prune`, broad `docker rm`, Docker daemon
재시작과 host 재부팅을 이 서버에서 실행하지 않는다. 사용자 컨테이너 변경은 Docker
CLI 직접 조작보다 Manager `runtime.*` operation을 사용한다.

## 12. Rollback

공식 Relay 장애 시 순서는 다음과 같다.

1. 신규 session 발급을 중지한다.
2. `relay.cookai.dev` DNS 또는 앱의 단일 `PIE_RELAY_URL` 값을 Azure Relay로 되돌린다.
3. 기존 short-lived session credential은 새 Relay 기준으로 재발급한다.
4. 서버의 Pie 스택은 조사용으로 유지하고 기존 Kroot 프로젝트는 변경하지 않는다.
5. 원인과 데이터 호환성을 확인한 뒤에만 재전환한다.

Azure Relay는 관찰 기간과 rollback 훈련이 끝나기 전에 삭제하거나 scale-to-zero하지
않는다.

## 13. 변경 이력

| 날짜 | 변경 내용 | 근거 |
|---|---|---|
| 2026-07-29 | 공유 서버 비간섭 배포 계획 수립 | 기존 Traefik/Docker 조사 |
| 2026-07-29 | 테스트·API·Admin·공식 Relay 도메인을 `cookai.dev`로 결정 | 사용자 결정 |
| 2026-07-29 | 실제 서버 인벤토리, 격리 배포와 2사용자 E2E 완료 | 실행 결과 |
| 2026-07-29 | `relay.cookai.dev` 공식 주소 전환 및 전체 회귀 통과 | DNS/TLS/WSS/Claude 실검증 |
| 2026-07-29 | 공식 API/Admin DNS, TLS, 인증·CORS와 실제 Claude E2E 통과 | 운영 도메인 실검증 |
