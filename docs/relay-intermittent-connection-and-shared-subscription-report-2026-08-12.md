# Pie Relay 간헐적 연결 장애 및 Claude 구독 중앙 공유 개선 보고서

> 작성일: 2026-08-12
> 대상 환경: Kroot 테스트 서버의 Web Chat, Event Manager, Pie Relay, 사용자별 Docker clientd
> 문서 목적: 사용자별로 다르게 보이던 연결 장애의 원인, 조치 결과, Claude 구독 인증 중앙화 구조를 운영·보고 관점에서 정리

## 1. 요약

최근 발생한 `Pie Relay 연결 중`, `Docker clientd 연결 중`, 반복 재접속 및 대화 준비
시간 초과의 주원인은 **사용자 간 세션 충돌이 아니라 공유 Docker 네트워크의 내부 DNS
별칭 충돌**이었다.

같은 서버에서 여러 Compose 프로젝트가 하나의 외부 `edge` 네트워크를 함께 사용하면서,
각 프로젝트가 공통 이름인 `relay`와 `manager`를 등록했다. 이 때문에 Kroot Studio
Manager가 내부 주소 `relay:13412`를 조회했을 때 자기 Relay가 아닌 Vibe Canvas Relay의
IP를 받을 수 있었다. 잘못된 Relay는 Kroot Studio용 JWT를 신뢰하지 않으므로 WebSocket
연결을 HTTP 401로 거절했다.

반면 사용자 Executor의 clientd는 내부 이름이 아니라 공개 주소
`wss://relay-test.cookai.dev/ws/agent`로 접속했다. 따라서 한 연결 경로는 정상이고 다른
연결 경로만 실패하는 비대칭 상태가 만들어졌다. 이미 열려 있던 연결은 계속 살아 있고,
새로 만든 대화만 잘못된 IP로 연결될 수 있어 “나는 되는데 다른 사용자는 안 된다” 또는
“재시도할 때 가끔 된다”처럼 보였다.

현재는 Relay와 Manager에 프로젝트 고유 내부 별칭을 부여하고 모든 내부 제어 경로가 그
별칭만 사용하도록 수정했다. 종료 처리의 동시성 충돌도 보강했으며, 수정 후 서로 다른 두
사용자의 별도 컨테이너·세션에서 Claude 응답을 동시에 완료하는 E2E를 통과했다.

Claude 인증은 연결 장애와 별개의 계층이다. 과거처럼 갱신 가능한
`.claude/.credentials.json`을 사용자 컨테이너마다 복제하지 않고, Event Manager의
Credential Broker가 하나의 `claude setup-token`을 암호화 보관한 뒤 실제 Claude Code
프로세스를 시작할 때만 메모리 경로로 주입하도록 중앙화했다.

## 2. 사용자에게 나타난 증상

- 같은 시각에 사용자 A는 채팅이 되지만 사용자 B는 계속 `연결 중`으로 표시됐다.
- `connecting → reconnecting`이 반복되거나 대화 준비 단계에서 시간 초과가 발생했다.
- Relay 또는 Runtime 일부는 정상으로 보이지만, 실제 메시지를 전달할 participant 연결이
  만들어지지 않았다.
- 재접속하거나 새 대화를 만들면 일시적으로 정상화되는 것처럼 보였다.
- 별도로, 메시지가 Claude Code까지 도달한 뒤
  `OAuth session expired and could not be refreshed`가 반환되는 경우도 있었다.

마지막 OAuth 오류는 Relay 연결 실패가 아니다. 요청이 Relay와 clientd를 모두 통과해
Claude Code 실행 단계까지 도달했다는 뜻이며, 중앙 Claude 인증 상태를 점검해야 하는
별도 유형의 오류다.

## 3. 정상 연결 구조

```text
브라우저 Web Chat
  └─ Web Chat BFF
       └─ Event Manager
            ├─ participant 경로
            │    └─ 내부 Pie Relay /ws/participant
            │
            └─ 사용자 전용 Docker Executor
                 └─ clientd host 경로
                      └─ 공개 Pie Relay /ws/agent
                           └─ Claude Code 실행·스트리밍 응답
```

한 대화는 다음 값의 조합으로 다른 사용자와 격리된다.

```text
room + deviceId + sessionId + relayGeneration
```

Relay가 기존 연결을 교체하는 경우는 같은 Routing Key로 더 새로운 연결이 들어온 때뿐이다.
서로 다른 사용자, 컨테이너, 대화는 서로 다른 키를 사용하므로 한 사용자가 연결됐다는
이유로 다른 사용자가 끊기는 구조가 아니다.

## 4. 확정된 주원인

### 4.1 공유 네트워크의 짧은 DNS 별칭 충돌

Kroot Studio와 Vibe Canvas Compose 스택은 Traefik 연결을 위해 외부 Docker 네트워크
`kroot-shared-edge-network`를 공유한다. 두 스택이 모두 `relay`라는 짧은 서비스 이름을
등록하면서 Docker DNS에 같은 이름을 가진 대상이 둘 이상 생겼다.

장애 분석 당시 Kroot Studio Manager 컨테이너에서 `relay`를 조회한 결과는 의도한
Kroot Relay가 아니라 Pie Canvas Relay의 IP였다.

```text
의도한 Kroot Studio Relay: 172.27.0.8
실제 `relay` 조회 대상:      172.27.0.12
실제 대상의 정체:             pie-relay-pie-canvas-relay-1
```

Manager가 발급한 JWT의 issuer, audience, application, pool, node, generation, scope와
유효 시간은 정상이고 현재 verifier 검사도 통과했다. Manager와 원래 Kroot Relay의 JWT
secret도 일치했다. 그럼에도 `/ws/participant` handshake가 401이었던 이유는 토큰 자체가
아니라 **토큰을 전혀 다른 Relay 인스턴스에 보냈기 때문**이다.

### 4.2 왜 랜덤하게 보였는가

다음 세 조건이 겹쳐 장애가 사용자별로 달라 보였다.

1. **접속 경로가 달랐다.** clientd host 경로는 공개 WSS 주소를 사용했지만 Manager의
   participant 경로는 충돌 가능한 내부 DNS 이름을 사용했다.
2. **기존 WebSocket은 DNS를 다시 조회하지 않았다.** 이미 정상 Relay에 붙은 사용자는
   연결이 유지됐고, 신규 대화나 재연결만 잘못된 대상에 붙을 수 있었다.
3. **Docker DNS 응답·캐시와 연결 생성 시점이 달랐다.** 같은 코드여도 컨테이너 재시작,
   대화 생성 및 재연결 시점에 따라 선택된 대상이 달라질 수 있었다.

따라서 컨테이너 수가 두 개 이상이라서 생긴 용량 문제도 아니고, 한 사람의 Claude 사용이
다른 사람의 Relay 연결을 빼앗은 것도 아니다.

## 5. 함께 확인된 부수 문제

| 문제 | 증상 | 주원인과의 관계 | 조치 |
|---|---|---|---|
| 종료 시 상태 버전 충돌 | 닫힌 Conversation 뒤에 active Session이 남아 재연결·유휴 정리를 방해 | 장애 후 복구를 느리게 만든 부수 문제 | 최신 상태를 다시 읽어 최대 8회 재시도하고 종료를 멱등 처리 |
| 이전 제품 profile 잔존 | `no relay node capacity available`, 계속 준비 중 | Kroot Studio/Vibe Canvas 분리 전에 만든 세션에서 발생 가능한 별도 원인 | 관리형 대기 세션만 현재 application/pool로 안전하게 재바인딩 |
| 복제된 Claude 인증 파일의 갱신 경쟁 | 요청은 전달됐으나 OAuth 만료·refresh 실패 | Relay 연결과 무관한 Claude 실행 계층 문제 | 사용자별 인증 파일 복제를 중단하고 중앙 setup-token Broker로 전환 |
| 정상 유휴 종료 | Runtime은 살아 있지만 clientd 대화 연결은 없음 | 장애가 아니라 수명주기 정책 | 화면에 `idle_timeout`을 구분하고 같은 대화를 다시 연결 |
| 실제 네트워크·배포 중단 | Wi-Fi 전환, Relay 재시작, 초기 Executor 기동 지연 | 여전히 발생 가능한 일반 운영 원인 | 세부 상태·heartbeat·재연결 사유를 분리해 표시하고 자동 복구 |

### 5.1 종료 상태 충돌의 상세 원인

Conversation 삭제와 Relay presence heartbeat가 같은 versioned Session을 동시에 변경하면
낙관적 잠금 충돌이 날 수 있다. 기존 코드는 한 번만 저장을 시도해 Conversation은
닫혔지만 Session은 `active`로 남는 경우가 있었다. 이 고아 Session은 Executor 유휴
정리와 다음 readiness 판단을 방해했다.

현재 종료 함수는 최신 레코드를 다시 읽고 최대 8회 재시도한다. 이미 닫힌 상태도 성공으로
처리하며 `hostConnectionID`, Driver lease, 마지막 오류를 함께 정리한다.

## 6. 적용한 개선 사항

### 6.1 내부 주소를 프로젝트 고유 별칭으로 고정

테스트 서버 Compose에 다음 기본 별칭을 추가했다.

```text
PIE_RELAY_INTERNAL_ALIAS=pie-sandbox-test-relay
PIE_MANAGER_INTERNAL_ALIAS=pie-sandbox-test-manager
```

다음 내부 경로는 더 이상 `relay`, `manager` 같은 공통 이름을 사용하지 않는다.

- Manager → Relay control/participant
- Relay → Manager presence/control plane
- Preview Gateway → Manager

공개 클라이언트는 계속 TLS/WSS 공개 주소를 사용한다. 즉, 외부 접속 주소와 내부 제어
주소를 명확히 분리했다.

### 6.2 세션 종료를 멱등·수렴 방식으로 변경

- Conversation과 Session 종료를 각각 최대 8회 재시도한다.
- 이미 종료됐으면 오류가 아닌 성공으로 처리한다.
- active Session에 남을 수 있는 host connection과 Driver lease를 제거한다.
- 회귀 테스트로 반복 종료와 lease 정리를 검증한다.

### 6.3 연결 상태 진단을 세분화

화면과 API가 다음 상태를 따로 판단하도록 구성했다.

| 상태 | 의미 |
|---|---|
| Pie Relay | Manager가 대화용 Relay 세션을 준비했는지 |
| Docker clientd | 사용자 Executor host peer와 heartbeat가 살아 있는지 |
| 실시간 스트림 | 브라우저와 Web Chat BFF의 SSE가 연결됐는지 |

`connected`, `client_reconnecting`, `relay_unregistered`, `relay_unavailable`,
`runtime_unhealthy`, `idle_timeout` 같은 사유를 제공해 모든 장애를 단순 `연결 중`으로
보이지 않게 했다.

### 6.4 이전 세대 연결 차단과 자동 복구

- 같은 대화를 다시 연결할 때 `relayGeneration`을 증가시킨다.
- 이전 세대의 JWT, presence 및 Driver 명령은 새 Routing Key와 맞지 않아 폐기된다.
- 건강하지 않거나 용량이 찬 Relay node는 신규 배정에서 제외한다.
- 제품 profile 변경 뒤 남은 관리형 대기 세션은 조건을 확인한 뒤 현재 Relay 범위로만
  재바인딩한다.

## 7. Claude 구독 중앙 공유 구조

### 7.1 변경 전 문제

`claude auth login`이 만든 `.claude/.credentials.json`을 여러 컨테이너에 복사하면 각
Claude Code 인스턴스가 같은 refresh 상태를 독립적으로 갱신할 수 있다. 한 컨테이너가
토큰을 회전한 뒤 다른 컨테이너가 이전 상태로 갱신을 시도하면 간헐적인 인증 만료와
refresh 실패가 발생할 수 있다.

이 방식은 사용자별 격리와도 맞지 않고, 인증 파일이 사용자 HOME과 장기 실행 컨테이너에
남는다는 보안상 부담도 있었다.

### 7.2 변경 후 구조

```text
운영자
  └─ Event Manager 서버에서 `claude setup-token` 발급
       └─ Credential Broker
            ├─ AES-256-GCM 암호화 저장
            ├─ 활성·직전 버전과 교체 시점 관리
            ├─ deploy·rollback 지원
            └─ 채팅 세션 시작 시에만 메모리 복호화
                 └─ 신뢰된 clientd 세션 메모리
                      └─ 1회성 전용 FD
                           └─ Claude Code 프로세스 전용 환경
```

핵심 원칙은 다음과 같다.

- Anthropic API key 과금 경로는 사용하지 않는다.
- 사용자별 `.claude/.credentials.json`을 생성하거나 복제하지 않는다.
- OAuth token을 Docker image, Compose 환경, 프로세스 argv, Relay JWT, 채팅 메시지,
  관리자 응답 및 일반 로그에 넣지 않는다.
- 활성 토큰은 Claude 채팅 세션 시작 시에만 복호화한다.
- clientd가 Node Executor에 전용 FD로 한 번 전달하고, Claude Code child에만
  `CLAUDE_CODE_OAUTH_TOKEN`으로 적용한다.
- Bash, Hook, stdio MCP 하위 프로세스에는 OAuth와 provider credential이 상속되지
  않도록 환경을 제거한다.
- 활성 인증이 없거나 만료·이관 대기 상태면 인증 없이 실행하지 않고 fail-closed한다.

외부 서비스 PAT, Relay JWT, Claude OAuth는 서로 용도가 다르며 혼합하지 않는다.

| 자격정보 | 목적 | 수명·저장 위치 |
|---|---|---|
| 외부 서비스 PAT | 제3자 서비스에서 사용자를 식별하고 외부 API를 호출 | Integration 정책에 따라 사용자 전용 credential 파일에 저장 |
| Relay JWT | 특정 device·room·session·node·generation의 접속 권한 | 짧은 수명, 세션 단위 발급 |
| Claude setup-token | Claude Code 구독 실행 인증 | Manager Broker에 암호화 버전 저장, 실행 시 메모리 전달 |

### 7.3 동시 사용자 제어

Manager 전체의 Claude 동시 턴 수는
`PIE_CLAUDE_SUBSCRIPTION_MAX_CONCURRENT_TURNS`로 제한하며 현재 기본값은 4다. 제한을
넘는 요청은 다른 사용자의 턴을 덮어쓰거나 연결을 끊지 않고 FIFO로 기다린다.

따라서 부하가 높으면 Claude 응답 시작이 늦어질 수는 있지만, 이것을 Relay/clientd
오프라인으로 처리하면 안 된다. 화면에서는 `대기`, `실행 중`, `연결 실패`를 서로 다른
상태로 보여주는 것이 중요하다.

### 7.4 운영 기능

- 게시: 새 setup-token 후보를 암호화 버전으로 등록
- 배포: 실행 중인 Executor를 제한된 동시성으로 새 버전에 맞게 재조정
- 롤백: 직전 유효 버전으로 복귀
- 상태: configured, available, migrationPending, expired, rotationDue를 분리 표시
- 감사: 토큰 원문 없이 버전 ID와 대상·성공·실패 수만 기록

검증 당시 중앙 Broker는 `subscription-oauth` 모드로 사용 가능했고 만료 또는 이관 대기
상태가 아니었다. 메타데이터상 예상 만료일은 2027-08-12이지만, 이는 공급자 측의 실제
유효성을 보장하는 값이 아니므로 정기 canary 대화와 만료 전 회전을 함께 운영해야 한다.

## 8. 검증 결과

수정 배포 뒤 다음 항목을 확인했다.

| 검증 항목 | 결과 |
|---|---|
| 단일 사용자 공개 Web Chat E2E | 성공 |
| 서로 다른 두 사용자의 동시 대화 | 모두 성공 |
| 사용자별 프로젝트·컨테이너·Session 분리 | 서로 다른 값 확인 |
| Relay/clientd 상태 | 두 사용자 모두 `connected` |
| Relay 이벤트 | join ack, request accepted, text, usage, done 수신 |
| Claude 스트리밍 완료 시간 | 각 요청 약 4~5초 |
| 테스트 종료 후 active Conversation | 0 |
| 테스트 종료 후 active Session | 0 |
| Relay·Manager·Web Chat 공개 health | 모두 HTTP 200 |
| Relay·Manager·client Go 테스트 | 모두 통과 |
| Node Executor 테스트 | 75/75 통과 |
| Web Chat 테스트 | 28 통과, 선택 PostgreSQL 테스트 1건 skip |
| Next.js production build | 성공 |

이번 검증으로 최소한 다음 두 가지를 확인했다.

1. 사용자 두 명이 동시에 붙어도 서로의 Relay session이나 Executor container를
   대체하지 않는다.
2. 중앙 Claude 인증을 함께 사용하면서도 사용자별 대화·작업공간·Relay Routing Key는
   독립적으로 유지된다.

## 9. 남은 위험과 운영 권고

이번 수정은 재현된 내부 DNS 충돌과 종료 상태 경쟁을 제거한 것이다. 네트워크 단절,
배포 중 재시작, 실제 토큰 만료, 자원 부족과 같은 모든 장애 가능성을 없앤 것은 아니다.
다음 지표와 경보를 운영에 추가하거나 유지해야 한다.

- WebSocket handshake 401을 endpoint·Relay node별로 집계
- `transport.reconnecting` 지속 시간과 횟수
- Relay presence 전송 실패와 drop 수
- Conversation 생성부터 ready까지 걸린 시간
- 닫힌 Conversation에 연결된 active Session 수
- unique internal alias가 예상 컨테이너 IP를 가리키는지 확인하는 배포 점검
- 중앙 인증의 `available`, `expired`, `rotationDue`, canary 성공 여부
- Claude active turn과 FIFO queue 길이·대기 시간
- Executor cold-start, CPU, 메모리, PID 및 디스크 quota 사용량

장애 판단은 다음 순서가 가장 빠르다.

```text
1. 공개 /readyz 확인
2. Conversation connection.reason 확인
3. Runtime health와 clientd heartbeat 확인
4. Relay host/participant presence 확인
5. WebSocket 401과 실제 접속 대상 IP 확인
6. 요청이 accepted 이후 실패했다면 Claude 인증 상태 확인
```

공유 구독 인증의 기술적 격리와 회전 구조는 마련됐지만, 제3자 고객에게 제공하는 실제
서비스 범위는 공급자 정책·계약 조건과 별도로 검토해야 한다. 이는 연결 안정성이나
사용자 격리의 기술 검증과는 별개의 운영 의사결정이다.

## 10. 관련 구현 및 운영 문서

- 내부 별칭과 공개·내부 URL 분리:
  [`deploy/test-server/compose.yaml`](../deploy/test-server/compose.yaml)
- 배포 환경변수 예시:
  [`deploy/test-server/.env.example`](../deploy/test-server/.env.example)
- 멱등 Conversation·Session 종료:
  [`executor-manager/cmd/manager/integration_api.go`](../executor-manager/cmd/manager/integration_api.go)
- 종료 경쟁 회귀 테스트:
  [`executor-manager/cmd/manager/chat_lifecycle_test.go`](../executor-manager/cmd/manager/chat_lifecycle_test.go)
- 연결 격리·상태·복구 운영 가이드:
  [`relay-client-connection-isolation-and-recovery.md`](./relay-client-connection-isolation-and-recovery.md)
- 중앙 Claude 인증 발급·회전·복구 절차:
  [`event-manager-claude-auth.md`](./event-manager-claude-auth.md)
- 배포 네트워크 운영 기준:
  [`deployment-and-operations.md`](./deployment-and-operations.md)

## 11. 결론

이번 간헐 장애의 확정 원인은 사용자 수나 사용자 간 세션 간섭이 아니라 **여러 Compose
프로젝트가 공유 네트워크에서 같은 내부 DNS 이름을 사용한 구성 오류**였다. 공개 host
경로와 내부 participant 경로가 서로 다른 Relay를 바라볼 수 있었기 때문에 일부 상태만
정상으로 보였고, 연결 생성 시점에 따라 사용자별로 다르게 나타났다.

프로젝트 고유 내부 별칭, 멱등 종료, generation fencing, 세부 연결 상태와 중앙 Claude
Credential Broker를 적용함으로써 연결과 인증 문제를 각각 독립적으로 진단·복구할 수
있게 됐다. 수정 후 두 사용자 동시 E2E와 전체 회귀 테스트를 통과했으므로 현재 확인된
랜덤 연결 장애 경로는 해소된 상태다.
