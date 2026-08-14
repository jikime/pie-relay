# Pie Relay 로컬 통합 환경

운영 DNS나 `api-relay.cookai.dev` 없이 Relay, Manager, PostgreSQL, 사용자별 Docker
Executor, 모의 회원 인증, 로컬 TLS와 Prometheus를 한 머신에서 검증하는 환경이다.
운영 `deploy/compose.yaml`은 그대로 두고 `deploy/local/compose.yaml`을 오버레이한다.

## 요구 사항

- Docker Desktop 또는 Docker Engine과 Compose v2
- Node.js 22 이상
- OpenSSL, curl, jq
- Docker Desktop에서는 프로젝트 디렉터리가 파일 공유 대상이어야 한다.

## 시작과 전체 검증

```bash
./deploy/local/pie-local.sh up
./deploy/local/pie-local.sh test
```

프로필을 생략하면 기존 호환성을 위해 `pie-canvas`를 사용한다. Kroot Studio는 같은
코드와 이미지를 사용하지만 Compose 프로젝트, 포트, 데이터, 인증 저장소와 Executor
network가 분리된 `kroot-studio` 프로필로 실행한다.

```bash
PIE_RELAY_PROFILE=pie-canvas ./deploy/local/pie-local.sh up
PIE_RELAY_PROFILE=kroot-studio ./deploy/local/pie-local.sh up
```

| 항목 | Pie Canvas | Kroot Studio |
|---|---|---|
| Compose 프로젝트 | `pie-relay-local` | `pie-relay-kroot-studio` |
| Manager | `127.0.0.1:19090` | `127.0.0.1:29090` |
| Relay | `127.0.0.1:13412` | `127.0.0.1:14412` |
| 로컬 TLS | `127.0.0.1:18443` | `127.0.0.1:18543` |
| PostgreSQL | `127.0.0.1:15432` | `127.0.0.1:15532` |
| Prometheus | `127.0.0.1:19092` | `127.0.0.1:19192` |
| Executor network | `pie-executor` | `pie-executor-kroot-studio` |
| 데이터 | `.local/pie-relay/state` | `.local/pie-relay/kroot-studio/state` |
| Claude 인증 기본값 | 선택 | 필수 |

Pie Canvas의 기존 `.env`, 데이터와 컨테이너 이름은 그대로 유지한다. Kroot Studio의
secret과 인증 데이터는 `deploy/local/.profiles/kroot-studio` 및
`.local/pie-relay/kroot-studio` 아래에 새로 생성한다. 프로필별 현재 설정은 다음처럼
확인한다.

```bash
PIE_RELAY_PROFILE=pie-canvas ./deploy/local/pie-local.sh profile
PIE_RELAY_PROFILE=kroot-studio ./deploy/local/pie-local.sh profile
```

기본적으로 Executor 이미지를 항상 다시 빌드한다. Docker Desktop의 임시 빌드 공간
문제를 진단하면서 이미 존재하는 이미지를 재사용해야 할 때만
`PIE_SKIP_EXECUTOR_BUILD=true ./deploy/local/pie-local.sh test`를 사용한다. 이 옵션은
기존 이미지가 없으면 즉시 실패하며 운영 이미지 빌드를 대체하지 않는다.

Docker Desktop 빌드 공간 부족으로 Node 의존성 계층을 다시 만들 수 없지만 clientd Go
코드만 바뀐 경우에는 아래 개발용 overlay로 기존 이미지를 갱신할 수 있다. 이 방식은
`executor.mjs`나 npm 의존성이 바뀐 릴리스 빌드에는 사용할 수 없다.

```sh
docker build -f executor-manager/Dockerfile.executor-client-overlay \
  -t pie-relay-client:latest .
```

첫 실행 시 다음 항목을 자동 생성한다.

- Pie Canvas의 `deploy/local/.env`, `deploy/local/.generated`, `.local/pie-relay/state`
- Kroot Studio의 `deploy/local/.profiles/kroot-studio`, `.local/pie-relay/kroot-studio/state`
- 각 프로필 전용 난수 secret, 로컬 CA, PostgreSQL, Relay, workspace와 Executor 상태
- 프로필별 복원 훈련용 백업

이 경로는 Git에서 제외된다. `bootstrap`을 다시 실행해도 secret은 회전하지 않지만 LAN
주소가 달라지면 공개 Relay 주소와 인증서 SAN을 갱신한다.

```bash
./deploy/local/pie-local.sh bootstrap
./deploy/local/pie-local.sh status
./deploy/local/pie-local.sh logs relay
./deploy/local/pie-local.sh down
```

모든 명령은 `PIE_RELAY_PROFILE`을 따른다. 따라서 `down`, `logs`, `backup`도 선택한
프로필에만 적용되고 다른 서비스의 컨테이너와 데이터는 건드리지 않는다.
현재 `test` 전체 회귀 묶음은 고정 Pie Canvas PAT와 세션 기대값을 사용하므로
`pie-canvas` 프로필에서만 실행한다. Kroot Studio는 Compose 구성 검증과 서비스별
Integration E2E를 별도로 실행한다.

Kroot Studio는 Claude 구독 OAuth가 필수이므로 첫 사용자 프로비저닝 전에 다음 명령을
실행한다. `claude setup-token`의 브라우저 안내를 마친 뒤 발급 토큰을 숨김 입력으로
붙여 넣으면 Manager가 암호화 버전을 게시한다. 토큰은 사용자 컨테이너 HOME에
복사되지 않고 채팅 세션의 Claude Code 프로세스에만 전달된다.

```bash
PIE_RELAY_PROFILE=kroot-studio ./deploy/local/pie-local.sh claude-auth-login
PIE_RELAY_PROFILE=kroot-studio ./deploy/local/pie-local.sh provision
```

Pie Canvas 로컬 프로필은 전체 프런트엔드 개발이 인증 초기화에 막히지 않도록 인증을
선택 사항으로 둔다. 실제 Claude Agent 응답까지 검증하려면 Pie Canvas 프로필에도
별도로 인증을 발행한다.

`down`은 컨테이너와 네트워크만 내리고 데이터는 보존한다. 운영 데이터와 다른 Docker
프로젝트의 컨테이너·이미지·볼륨은 조작하지 않는다.

## 로컬 주소

| 용도 | 주소 |
|---|---|
| Desktop/CLI 직접 Relay | `http://127.0.0.1:13412`, `ws://127.0.0.1:13412` |
| 같은 Wi-Fi의 모바일 Relay | `http://<PIE_LOCAL_LAN_IP>:13412` |
| Manager/Admin API | `http://127.0.0.1:19090` |
| Admin Web | `http://127.0.0.1:19090/admin/` |
| 로컬 TLS Relay | `https://relay.localhost:18443` / `wss://relay.localhost:18443` |
| 로컬 TLS API/Admin | `https://api-relay.localhost:18443`, `https://admin-relay.localhost:18443/admin/` |
| 프로젝트 웹 프리뷰 | `https://p-{무작위값}.preview.localhost:18443` |
| Prometheus | `http://127.0.0.1:19092` |

실제 LAN 주소는 다음 명령으로 확인한다.

```bash
./deploy/local/pie-local.sh bootstrap
grep '^PIE_LOCAL_RELAY_PUBLIC_URL=' deploy/local/.env
```

모바일은 우선 HTTP LAN 주소로 연결할 수 있다. 로컬 TLS를 실제 기기에서 시험하려면
`deploy/local/.generated/certs/local-ca.crt`를 기기에 설치하고 인증서 신뢰를 켠 뒤,
기기가 `relay.localhost`를 개발 머신으로 해석하도록 로컬 DNS를 제공해야 한다. 일반
Wi-Fi 테스트에서는 이 DNS 구성이 불필요한 직접 LAN 주소를 권장한다.

## 모의 PAT

모의 회원 서버는 로컬에서만 다음 고정 PAT를 제공한다. 실제 비밀이 아니며 운영에서
사용하면 안 된다.

| PAT | 결과 |
|---|---|
| `pat-local-admin` | `pie-admin` |
| `pat-local-operator` | `pie:operate` |
| `pat-local-viewer` | `pie:admin:view` |
| `pat-local-user` | 일반 사용자 `local-user` |
| `pat-pie-canvas-agent` | Pie Canvas 로컬 Agent 전용 `pie:operate` 서비스 계정 |
| `pat-kroot-studio-agent` | Kroot Studio 로컬 Agent 전용 `pie:operate` 서비스 계정 |
| `pat-local-guest` | 일반 사용자 `local-guest` |
| `pat-local-inactive` | 비활성 |
| `pat-local-slow` | Manager timeout보다 늦은 응답 |

전체 테스트는 활성·비활성·timeout뿐 아니라 `pat-local-user`를 실제로 폐기하고
`PIE_AUTH_CACHE_TTL`이 지난 뒤 401이 되는지 확인한 다음 복구한다. 회원 lifecycle
webhook도 잘못된 서명 거부와 올바른 HMAC 요청을 모두 검사한다.

## 전체 테스트가 확인하는 항목

1. 모든 서비스 readiness와 무인증 API/metric 거부
2. PAT role, 비활성, timeout, 폐기와 캐시 만료
3. HMAC 회원가입 이벤트와 `local-user` Executor 자동 생성
4. UID 10001, read-only root, capability drop, CPU 0.5, 메모리 512MiB, PID 64
5. 실제 Docker 명령 제출과 출력 왕복
6. Docker PTY host, view/control participant, 단일 Driver 인계와 강제 종료
7. 동시 20 participant 연결의 handshake p50/p95/p99
8. Relay 재시작 후 clientd의 자동 재연결과 PTY snapshot 복구
9. 네이티브 clientd 2개의 독립 PTY, view 입력 차단, control 왕복과 session 격리
10. 로컬 CA를 사용한 HTTPS와 실제 WSS participant 연결
11. Prometheus의 Relay/Manager 인증 metric 수집
12. PostgreSQL custom-format dump와 별도 임시 DB 복원
13. workspace/auth/Relay 상태 archive를 임시 디렉터리에 격리 복원
14. 프로젝트 프리뷰 동시 생성, 공개·비공개 접근, streaming, 재시작과 자원 회수

접속 수와 허용 p95는 필요하면 조정할 수 있다.

```bash
PIE_E2E_LOAD_PARTICIPANTS=32 \
PIE_E2E_LOAD_MAX_P95_MS=5000 \
./deploy/local/pie-local.sh test
```

이 수치는 개발 머신의 회귀 기준이며 운영 용량 산정 결과가 아니다. 운영 목표 동시
사용자 수는 staging과 운영과 같은 인스턴스, 네트워크, TLS, PostgreSQL 설정으로 별도
soak test를 수행해야 한다.

## Pie Canvas 채널 Agent 로컬 Runtime

Pie Canvas의 채널 Agent는 일반 `local-user` 회귀 테스트와 별도 Executor를 사용합니다.
일반 테스트의 PID 64 제한은 자원 경계 검증용이라 여러 ACP 세션을 계속 실행하기에 작기 때문입니다.

```bash
PIE_EXECUTOR_IMAGE=pie-relay-client-acp-e2e:latest \
PIE_EXECUTOR_DOCKERFILE="$PWD/executor-manager/Dockerfile.executor-acp-e2e" \
  ./deploy/local/pie-local.sh up

./deploy/local/pie-local.sh provision-pie-canvas
```

전용 `pie-canvas-agent` Executor는 CPU 1.5, 메모리 1GB, PID 256, 최대 세션 16개로 생성됩니다.
`Dockerfile.executor-acp-e2e`는 개발자의 Claude 자격증명을 복사하지 않고 고정 문자열로 응답하는
ACP v2 테스트 Agent를 추가합니다. 따라서 비용 없이 실제 clientd·Relay·Control·WebSocket 경로를
검증할 수 있지만 실제 Claude 답변 품질을 검증하는 이미지는 아닙니다.

Relay 노드는 참가자에게 반환할 `address`와 Manager가 세션 시작·종료에 사용할
`controlAddress`를 구분합니다. 로컬에서는 각각 LAN 공개 주소와 `http://relay:13412`가 되므로,
호스트 또는 다른 Docker network의 참가자에게 내부 호스트명이 잘못 반환되지 않습니다.

## 백업과 복원 훈련

```bash
backup_dir=$(./deploy/local/pie-local.sh backup)
./deploy/local/pie-local.sh restore-drill "$backup_dir"
```

백업은 PostgreSQL custom-format 논리 덤프와 Manager·Executor·Relay 상태 archive를
분리한다. 실행 중인 PostgreSQL/Prometheus 데이터 디렉터리를 archive에 중복 포함하지
않는다. 복원 훈련은 checksum과 archive 경로를 먼저 검증하고 현재 DB를 덮어쓰지
않으며, 임시 `pie_restore_drill` DB와 임시 상태 디렉터리에 복원한 뒤 자동 삭제한다.
백업에는 사용자 인증 상태가 포함될 수 있으므로 로컬 파일도 외부 공유하거나 Git에
추가하지 않는다.

## 운영과의 차이

- 로컬 회원 서버는 실제 가입·결제·조직 정책을 대신하지 않는다.
- 로컬 CA는 공인 인증서가 아니며 운영 이미지에 포함하지 않는다.
- 로컬 PostgreSQL은 단일 컨테이너이고 PITR/고가용성을 제공하지 않는다.
- bind mount의 `diskBytes`는 파일시스템 quota가 아니다.
- 물리 iPhone/Android의 셀룰러 전환과 background/resume은 실제 기기에서 승인한다.
- 여러 Relay Cell의 assignment와 sticky routing은 이 단일 Cell 환경의 범위 밖이다.
