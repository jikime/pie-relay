# CookAI 공식 도메인 전환 및 Pie Staging 검증 보고서

> 작성일: 2026-07-29  
> 대상 프로젝트: Pie Relay  
> 대상 서버: `221.143.48.77`  
> 환경: 기존 Kroot 서비스와 공존하는 공유 Staging  
> 상태: 공식 Relay·API·Admin·Web Chat 도메인 전환 및 공개 HTTPS E2E 완료

이 문서는 Pie Relay를 Azure 테스트 Relay에서 자체 서버 기반 CookAI 도메인으로
전환하면서 수행한 배포, 보안 경계, 실제 연결 시험과 남은 운영 과제를 한곳에 정리한
작업 보고서다.

비밀번호, SSH private key, PAT, Claude OAuth credential, Relay JWT secret, 관리자
token과 Integration service token은 이 문서와 Git에 기록하지 않는다.

## 1. 작업 목표

이번 작업의 목표는 다음과 같다.

1. 기존 Kroot Docker/Traefik 서비스를 중단하지 않고 Pie 전용 스택을 배포한다.
2. Azure Relay 대신 자체 서버의 `cookai.dev` 도메인을 공식 기본값으로 사용한다.
3. Relay, Manager, PostgreSQL과 사용자별 Executor를 실제로 연결한다.
4. 제3자 애플리케이션에서 사용자별 컨테이너의 Claude Code를 호출할 수 있는지
   실제 응답으로 확인한다.
5. 모바일, 데스크톱, clientd, 공유 터미널과 Docker 격리 경로를 함께 검증한다.
6. 고객 공개 전에 남은 보안·용량·복구 과제를 식별한다.

## 2. 최종 공식 주소

| 주소 | 역할 | 상태 |
|---|---|---|
| `https://relay.cookai.dev` | Desktop·Mobile·clientd의 Relay Data Plane | 정상 |
| `https://api-relay.cookai.dev` | 제3자 Backend 및 Control Plane API | 정상 |
| `https://admin-relay.cookai.dev/admin/` | 운영자 관리 화면 | 정상 |
| `https://chat-relay.cookai.dev` | 제3자 웹채팅 BFF 운영 경로 검증 | 정상 |

진단과 Staging 회귀를 위해 다음 테스트 주소도 유지한다.

| 주소 | 역할 | 상태 |
|---|---|---|
| `https://relay-test.cookai.dev` | 테스트 Relay 별칭 | 정상 |
| `https://api-relay-test.cookai.dev` | 테스트 Manager API 별칭 | 정상 |
| `https://admin-relay-test.cookai.dev/admin/` | 테스트 Admin 별칭 | 정상 |

여섯 DNS 이름은 모두 `221.143.48.77`을 가리키며 기존 Traefik의 Host router가
서비스를 구분한다.

## 3. 서버와 배포 경계

### 확인된 서버 사양

| 항목 | 값 |
|---|---|
| OS | Rocky Linux 9.7 |
| SELinux | Enforcing |
| CPU | Intel Core i9-14900T, 24 core / 32 thread |
| Memory | 약 124 GiB |
| Root NVMe | 약 1 TB |
| `/home` | 약 1 TB |
| Docker | 29.2.0 |
| Docker Compose | v5.0.2 |
| Reverse Proxy | Traefik 3.6 |

이 서버는 전용 Pie 서버가 아니다. 조사 당시 약 95개 컨테이너와 기존 Kroot 서비스가
운영 중이었으므로 Docker daemon 재시작, host 재부팅, 광범위 prune과 OOM/disk-fill
시험은 수행하지 않았다.

### Pie 전용 식별자

| 범위 | 값 |
|---|---|
| SSH alias | `pie-sandbox-test` |
| SSH 사용자/포트 | `kaonkroot` / `2733` |
| Compose project | `pie-sandbox-test` |
| Manager/Node ID | `sandbox-node-01` |
| 서버 작업 root | `/home/kaonkroot/pie-sandbox-test` |
| 영속 데이터 root | `/home/kaonkroot/pie-sandbox-test/data` |
| Executor network | `pie-sandbox-test-executor` |

기존 Compose project, container, volume과 network에는 중지·삭제·정리 작업을 하지
않았다.

## 4. 최종 구성

```text
제3자 Web/Desktop/Mobile/clientd
              │ HTTPS/WSS
              ▼
       기존 Traefik :443
        ├─ relay.cookai.dev       → Pie Relay :13412
        ├─ api-relay.cookai.dev   → Pie Manager :19090
        ├─ admin-relay.cookai.dev → Pie Manager /admin/
        └─ chat-relay.cookai.dev  → Web Chat BFF :4175
                                      │
                                      └─ Integration API → Pie Manager
                                                               │
                                                     PostgreSQL + Docker API
                                                               │
      사용자별 격리 Executor 컨테이너
                    │
       clientd → Claude Code / kroot
```

Relay는 터미널·채팅 frame을 전달하는 Data Plane이고, Manager는 사용자, Integration,
컨테이너, 프로젝트, 세션, 권한과 operation을 관리하는 Control Plane이다. Manager가
터미널 byte stream을 대신 중계하지 않는다.

## 5. 네트워크와 컨테이너 격리

사용자 Executor에는 다음 정책을 적용했다.

- 사용자마다 별도 Home, Workspace와 인증 상태 경계
- UID/GID `10001:10001`
- CPU 1개, memory 4 GiB, PID 256
- read-only root filesystem과 제한된 tmpfs
- Linux capability 전체 제거
- `no-new-privileges`
- Docker socket과 host 임의 경로 미마운트
- inbound host port 미노출
- Executor bridge의 ICC 비활성화

Relay는 Executor network에 들어가지 않는다. clientd는 내부 Docker IP가 아니라
`wss://relay.cookai.dev/ws/agent`로 TLS outbound 연결한다. 따라서 사용자 컨테이너
A가 B의 내부 IP 서비스나 Workspace에 직접 접근하지 못한다.

Manager는 현재 Docker socket을 사용한다. 이는 사실상 host-root 권한이므로
Production에서는 전용 Executor Node와 제한된 Docker API proxy 또는 runtime broker가
필요하다.

## 6. 공식 주소 전환 작업

활성 기본값과 서버 환경을 다음과 같이 변경했다.

```dotenv
RELAY_PUBLIC_URL=https://relay.cookai.dev
PIE_EXECUTOR_RELAY_URL=wss://relay.cookai.dev/ws/agent
```

기본 주소 변경 대상에는 Server, Manager, clientd, Desktop, Mobile Host Gateway와
제3자 웹 예제가 포함된다. 로컬 개발 환경은 별도 환경변수로 주소만 바꾸어 사용할 수
있으며 `relay_url`과 `azure_url`을 별도 설정으로 나누지 않는다.

전환 순서는 다음과 같았다.

1. `relay.cookai.dev` DNS와 공인 인증서 확인
2. Relay와 Manager의 공식 공개 주소 변경
3. Relay와 Manager만 재생성
4. 사용자 Executor를 Manager `runtime.recreate` operation으로 순차 재생성
5. 프로젝트, Home, Workspace와 인증 파일 보존 확인
6. 공식 Relay의 모바일·공유 터미널·Claude E2E 검증
7. `api-relay.cookai.dev`, `admin-relay.cookai.dev` DNS 추가
8. 공식 API/Admin Traefik router 적용
9. TLS, CORS, 인증과 공식 API 기반 Claude E2E 재검증

Relay/Manager 전환은 약 11초, 공식 API/Admin router 적용을 위한 Manager 재생성은
약 6초가 걸렸다. PostgreSQL과 기존 Kroot 서비스는 재시작하지 않았다.

## 7. 인증과 접근 정책

### 공식 API/Admin

- 공개 health endpoint: HTTP 200
- Admin 화면: HTTP 200
- 무인증 관리자 API: HTTP 401
- 유효한 관리자 token: HTTP 200
- 공식 Admin origin의 CORS preflight: HTTP 204
- 관리자 token은 Admin 화면의 `sessionStorage`에만 보관

### 사용자 Docker 대화

사용자별 AI 컨테이너 대화는 `private/owner-only`다. 사용자 B가 사용자 A의 Docker
컨테이너 대화에 참가하거나 제어할 수 없다. 이 세션의 host credential로 invite를
만들 때 `403 token cannot create invites`가 반환되는 것은 정상적인 보안 동작이다.

여러 사용자가 같은 터미널을 보거나 조작하는 기능은 별도의 `shared` Host OS 세션에서
view/controller와 단일 Driver 정책으로 처리한다.

## 8. 실제 E2E 결과

### Relay 및 터미널

- health/readiness와 보호된 metric
- 잘못된 host enroll secret 거부
- 모바일 Relay assignment와 공식 `cellUrl`
- invite/join
- WebSocket Origin 검증
- host/participant 역할 격리
- view/control 권한
- host ↔ participant 양방향 PTY
- host와 participant 재연결
- Driver 인계와 participant 강제 연결 종료

모두 통과했다.

### 제3자 AI 채팅

다음 실제 경로를 통과했다.

```text
공식 Manager API
  → Pie Relay
  → 사용자 전용 Docker Executor
  → clientd
  → Claude Code
  → Relay
  → 공식 Manager API 응답/Event stream
```

확인한 항목:

- Alice/Bob 사용자별 Executor 생성
- 사용자별 `.kroot/credential.json`과 Claude credential
- 컨테이너 안의 `kroot` 실행
- 실제 `kroot init` 프로젝트 생성
- 실제 Claude text 응답
- PNG 이미지 첨부 및 이미지 응답
- permission 요청 수신과 승인
- 같은 대화에서 중복 turn 409 처리
- 승인 뒤 요청 파일 생성
- Alice/Bob 실제 Claude 응답 동시 완료
- 공식 `api-relay.cookai.dev`에서 보낸 marker 응답 성공
- 공개 `https://chat-relay.cookai.dev` 로그인부터 SSE 응답까지 성공
- 공개 Web Chat BFF → 공식 Manager → 공식 Relay → 원격 Docker clientd → 실제 Claude Code
  왕복 성공

### 격리와 권한

- Alice Workspace를 Bob이 읽지 못함
- Executor A → B 직접 HTTP 차단
- 공개 Relay outbound 연결 허용
- 다른 Integration token으로 Alice conversation 접근 시 403
- Executor에 Docker socket 없음
- Admin 응답과 Relay/Manager log에서 PAT/token 패턴 누출 없음
- 활성 Executor 2개에서 세 번째 provisioning은 429

### 장애 복구

- Relay 재시작 후 약 36초 내 clientd 재연결
- Manager 재시작 후 약 10초 내 상태 복구
- PostgreSQL 중단 중 readiness 503, 재기동 후 약 1초 내 복구
- Executor 재생성 뒤 Home, Workspace, Kroot/Claude 인증 상태 보존
- 재생성한 Executor를 이용한 후속 Claude 응답 성공

## 9. 코드 품질과 회귀 검사

공식 주소 전환 후 다음 결과를 확인했다.

| 대상 | 결과 |
|---|---|
| Server, Manager, clientd | 전체 Go test 통과 |
| Go race/vet/lint/vulnerability | 세 모듈 모두 통과, 알려진 Go 취약점 0건 |
| Desktop | 177 test, production build 통과 |
| Tauri/Rust | format, Clippy warning 0, Cargo test 통과 |
| Mobile Host Gateway | 8 test, typecheck, build 통과 |
| Mobile App | 1,717 test 통과, 2 skip, typecheck 통과 |
| Next.js 예제 | 16 test, check, Next.js 16.2.12 build 통과 |
| npm/pnpm audit | 주요 패키지 0건, 모바일 제한 patch advisory 1건만 명시적 ignore |

기존 Kroot 기준 응답도 배포 전후 동일하게 유지됐다.

- `www.kroot.io`: 200
- `api.kroot.io`: 401
- `auth.kroot.io`: 302

최근 Relay/Manager log의 panic, fatal, segmentation fault는 0건이다.

## 10. 현재 용량 정책과 검증 한계

| 항목 | 현재값 |
|---|---:|
| Executor CPU | 1 CPU |
| Executor memory | 4 GiB |
| Executor PID | 256 |
| Node 활성 Executor 상한 | 2 |
| Provision 동시성 | 1 |
| Queue capacity | 16 |

124 GiB 메모리를 단순히 4 GiB로 나누어 사용자 수를 계산하면 안 된다. Claude,
Node.js, Docker cache, PostgreSQL과 기존 Kroot workload가 함께 자원을 사용한다.
현재 실제 동시 검증은 2명까지이며, 4명 이상을 고객 수용량으로 약속할 근거는 아직 없다.

공유 서버 보호를 위해 다음 시험은 수행하지 않았다.

- Docker daemon 재시작과 host 재부팅
- disk/inode fill
- Host OOM과 강제 memory pressure
- 4명 이상 실제 Claude 동시 부하
- 8시간 이상 soak
- broad Docker prune

## 11. 고객 공개 전 필수 보강

### P0

1. Claude OAuth access/refresh token의 수명과 동시 회전 구조 확정
2. Manager Docker socket의 host-root 권한 축소
3. PostgreSQL, Workspace, 사용자 auth의 암호화 off-host backup/restore
4. 사용자별 byte/inode quota 강제
5. Admin을 VPN/IP allowlist 또는 SSO/MFA 뒤로 이동
6. 실제 제3자 회원 서비스의 PAT introspection 및 lifecycle 재전송/폐기 검증
7. Alice/Bob/Charlie 시험 데이터와 Production DB/data root 분리

### P1

- Prometheus scrape와 운영 경보
- 유휴 Executor stop/reaper
- 1→2→4→8 실제 사용자 단계 부하 및 8시간 soak
- immutable image digest, registry, SBOM, 서명과 CVE gate
- PostgreSQL PITR와 정기 복원 훈련
- 인증서, disk, OOM, queue, reconnect와 429 경보

### P2

- 다중 Executor Node scheduler와 admission
- Node drain과 Workspace 이전
- 다중 Relay Cell assignment/sticky routing
- Manager/Relay HA
- 필요 시 gVisor, Kata 또는 Firecracker runtime adapter

## 12. 운영 확인 명령

```bash
ssh pie-sandbox-test
cd /home/kaonkroot/pie-sandbox-test/src/deploy/test-server
docker compose --env-file .env config --quiet
docker compose --env-file .env ps

curl --fail https://relay.cookai.dev/readyz
curl --fail https://api-relay.cookai.dev/readyz
curl --fail https://admin-relay.cookai.dev/admin/
curl --fail https://chat-relay.cookai.dev/api/health
```

관리 API와 metric에는 각 용도에 맞는 bearer token을 사용한다. secret을 shell history,
문서, Git, QR 또는 로그에 출력하지 않는다.

## 13. Rollback

Azure Relay는 중지하거나 삭제하지 않았으며 rollback 대상으로 유지한다.

공식 Relay 장애 시:

1. 신규 session 발급을 중지한다.
2. `PIE_RELAY_URL` 또는 DNS를 Azure Relay로 되돌린다.
3. short-lived session credential을 새 Relay 기준으로 재발급한다.
4. 서버 Pie 스택은 원인 조사용으로 유지한다.
5. 기존 Kroot 서비스와 Docker 자원은 변경하지 않는다.

공식 API/Admin 적용 전 Compose는 서버의
`compose.yaml.pre-official-api-admin-20260729`에 보관되어 있다. 공식 Relay 전환 전
환경 파일도 `.env.pre-cookai-cutover-20260729`에 권한을 유지해 보관했다. 두 파일 모두
secret 또는 운영 경로 정보를 포함할 수 있으므로 서버 밖으로 공개하지 않는다.

## 14. 공개 Web Chat 운영 경로 검증

`chat-relay.cookai.dev`는 host port를 공개하지 않고 기존 Traefik의 HTTPS router로만
Web Chat BFF에 연결된다. BFF는 공식 Manager API를 호출하며 Integration token은
환경변수가 아니라 mode `0600`인 host secret 파일을 read-only mount한다. 브라우저에는
Integration token, Relay JWT, 내부 owner ID와 컨테이너 ID를 반환하지 않는다.

2026-07-29 최초 공개 검증은 회원가입을 닫고 Alice 계정만 사용했다. 2026-07-30에는
실제 가입 흐름을 검증하기 위해 다음 정책으로 전환했다.

- 회원가입 활성화 및 계정·provisioning 상태 PostgreSQL 영속화
- 초기 `users.json` 계정은 PostgreSQL에 멱등적으로 seed
- 사용자 credential은 별도 mode 0600 키를 사용하는 AES-256-GCM 암호문으로 저장
- Manager 오류 뒤에도 로그인 후 같은 Integration User/Executor provisioning 재시도
- Secure·HttpOnly·SameSite=Strict 로그인 cookie
- read-only root filesystem, capability 전체 제거, `no-new-privileges`
- 0.5 CPU, memory 512 MiB, PID 256
- 공개 host port 없음
- Web Chat 요청 rate limit 적용

2026-07-29 공개 주소만 사용한 자동 E2E에서 로그인, Workspace 확인, 기존 Project 선택,
Conversation 생성, SSE 연결, 실제 Claude marker 응답과 Conversation 정리를 모두
통과했다. `ready`, `relay_join_ack`, `transport.connected`, `request.accepted`, `text`,
`done` event를 확인했다. TLS 인증서는 Let's Encrypt가 발급했으며 SAN은
`chat-relay.cookai.dev`다.

시험 도중 기존 Claude OAuth가 만료되어 macOS Keychain의 최신 credential로 기존 두
Executor와 신규 사용자 seed를 갱신했다. 전송용 임시 credential 파일은 설치 직후
로컬·서버·Manager 컨테이너에서 제거했다. 이 방식은 기능 검증용이다. 같은 개인 OAuth를
여러 고객 컨테이너에 복제하는 것은 장기 운영 인증 모델로 사용하지 않는다.

수동 로그인 정보는 서버의 다음 파일에만 보관한다.

```text
/home/kaonkroot/pie-sandbox-test/web-chat/login.json
```

파일은 mode `0600`이며 Git과 문서에 비밀번호를 기록하지 않는다.

### 14.1 신규 회원가입·전용 컨테이너 E2E

2026-07-30 신규 계정으로 공개 가입을 시도하면서 Manager의 Executor 총량은 4이지만
`cookai-e2e` Integration의 `maxUsers`가 과거 시험값 2로 남아 있어 세 번째 사용자가
`429 control quota exceeded`를 받은 것을 확인했다. 계정은 먼저 PostgreSQL에 저장되어
`failed` 상태와 원인이 보존되었다.

Integration 사용자 한도를 4로 맞추고 동일 계정으로 로그인해 작업공간 provisioning을
재시도했다. 이후 다음 경로가 모두 성공했다.

```text
공개 회원가입 → PostgreSQL 사용자 → Manager → 새 전용 Docker clientd
             → relay.cookai.dev → Claude Code → Browser SSE
```

검증된 새 Executor는 `1 CPU`, `4 GiB`, PID `256`, UID/GID `10001:10001`이며 ICC가
꺼진 `pie-sandbox-test-executor` network 하나에만 연결되었다. `kroot` 실행 파일과
`.kroot/credential.json`, `.claude/.credentials.json`, `.pie-state-seed-v1`을 확인했고
세 파일은 모두 mode `0600`, owner `10001:10001`이었다. PostgreSQL credential 열에는
평문 `kpat_demo_` 문자열이 존재하지 않았다. 실제 Claude marker 응답에서 `ready`,
`relay_join_ack`, `transport.connected`, `request.accepted`, `text`, `done` event를 확인했다.

재발 방지를 위해 `reconcile-web-chat-integration.sh`를 추가했다. 배포 시 Executor 총량보다
Integration 사용자 한도가 낮지 않도록 멱등적으로 올리고, Integration token 파일이
없을 때만 token을 회전해 mode `0600`으로 저장한다. 현재 설정은 두 값 모두 4다.

회귀 시험 중에는 과거 브라우저·수동 시험 대화가 `ready` 상태로 남아 Alice의
`maxConversationsPerUser=2`를 모두 사용한 문제도 확인했다. 시험 대화만 공개 DELETE
경로로 종료하고, Manager에 사용자별 활성 대화 조회 API를 추가했다. Web Chat은 새
브라우저에서도 자기 대화를 재개하며 `새 대화` 또는 Project 변경 시 현재 대화와 Docker
session을 먼저 닫는다. 실제 Headless Chrome에서 첫 대화 ID와 교체 대화 ID가 다르고,
교체 직후 소유 활성 대화가 정확히 1개임을 확인했다. 시험 종료 후 Alice와 신규 가입
사용자의 활성 대화 수는 0이며, 남아 있던 Bob 시험 대화도 정상 종료했다.

## 15. 현재 판정

현재 상태는 다음과 같이 판정한다.

> **공유 Staging에서 CookAI 공식 Relay/API/Admin/Web Chat 전환 및 공개 HTTPS 실제 Claude E2E 완료**

DNS, TLS, 인증, CORS, Relay, Manager, PostgreSQL, 사용자별 Executor와 실제 Claude
응답은 공개 도메인에서 동작한다. 다만 이 서버는 기존 Kroot와 함께 사용하는
production-like Staging이며 P0 항목이 남아 있으므로 아직 고객 데이터와 실사용자를
받는 Production 공개 완료로 판정하지 않는다.
