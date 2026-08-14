# Project 채팅 준비·복구·유휴 수명주기

> 작성일: 2026-08-03
> 대상: 제3자 웹채팅 → Pie Manager → Pie Relay → 사용자 전용 Docker clientd → Claude Code

## 1. 목적

프로젝트를 만든 직후 Relay나 Executor 준비에 시간이 걸리더라도 사용자가 화면에서
막히거나 작성 중인 질문을 잃지 않도록 한다. 사용하지 않는 Claude 세션과 Docker
컨테이너는 단계적으로 정리하되, 사용자별 Home·Workspace·인증 volume과 Conversation
기록은 보존하여 다음 요청에서 같은 작업을 이어갈 수 있게 한다.

## 2. 최종 실행 순서

```text
회원가입
  → 사용자·Integration User 저장
  → 사용자 전용 Executor 컨테이너 provision 및 ready 확인
  → Project 생성 요청
  → 컨테이너 내부 /workspace/projects/{opaque-project-id} 생성
  → kroot init 실행 및 Project ready
  → Conversation·Control Session 생성
  → Controller가 단기 Relay JWT 발급
  → 컨테이너 clientd와 Chat Gateway가 Relay에 접속
  → Conversation ready
  → 메시지 전송 및 Claude Code 응답
```

회원가입 단계에서 컨테이너를 미리 준비하므로 최초 Project 생성은 일반적으로 컨테이너
cold-start를 기다리지 않는다. Conversation을 만들기 전에는 Relay JWT나 Claude 프로세스를
상시 유지하지 않는다. 실제 대화가 생길 때 세션만 시작하여 불필요한 연결과 프로세스를
줄인다.

## 3. 준비 지연이 사용 불능으로 이어지지 않는 이유

웹 UI는 더 이상 90초 후 연결 감시를 포기하지 않는다. 처음 90회는 1초 간격, 이후에는
5초 간격으로 현재 Conversation을 계속 확인한다. 네트워크 오류나 Manager의 일시 오류가
발생해도 페이지와 Project는 그대로 유지된다.

Manager Controller는 `starting`, `reconnecting`, 시간 초과된 `provisioning`, 복구 가능한
`error` 세션을 자동 재시작한다. 오류 재시도는 1초부터 최대 30초까지 지수 backoff를
사용한다. UI의 `다시 연결` 버튼은 동일 Conversation과 journal을 보존한 채 오래된
Gateway peer와 clientd 세션만 교체한다.

```text
자동 복구: error → Controller backoff → provisioning → ready
수동 복구: error/closed → POST .../retry → starting → connecting → ready
```

따라서 준비가 늦어질 수는 있지만 90초가 지났다는 이유만으로 영구 사용 불능 상태가
되지는 않는다. 다만 Docker daemon 장애, 이미지 pull 실패, Relay 장애처럼 기반 서비스가
계속 고장 난 경우에는 준비가 완료될 수 없으므로 운영 경보와 상태 화면으로 원인을
해결해야 한다.

Relay 재시작으로 Node lease가 만료되면 Manager는 `RelayGeneration`을 올려 이전 토큰을
fencing하고 새 자격증명을 발급한다. 재시작 창에 아직 사용 가능한 Relay Node가 없으면
Session을 영구 `error`로 끝내지 않고 `reconnecting`으로 유지하며 최대 30초 간격으로
재시도한다. Node가 돌아오면 기존 clientd Session을 정리하고 새 세대 토큰으로 다시
접속한다.

주기적 자동 복구와 운영자의 수동 재시작이 동시에 실행되어도 `ready/active` Session을
다시 `provisioning`으로 내리지 않는다. 시작 함수의 상태 가드와 version CAS가 중복 PTY
기동 및 `StartAttempts` 이중 증가를 막는다.

Application/Tenant/Resource 범위를 가진 Session의 Relay room은 사용자 ID가 아니라
`r_v2_...` HMAC 가명 값이다. Driver 양도 같은 운영 제어 요청도 토큰 발급과 동일한
가명 room을 계산한다. 이 규칙을 지키지 않으면 참가자는 연결되어 있어도 제어 API가
다른 방을 조회하여 `409 driver target is not connected`를 반환한다.

## 4. 웹 UI 동작

- Relay 준비 중에도 질문과 이미지를 미리 작성·첨부할 수 있다.
- 전송 버튼만 `Conversation ready`까지 비활성화된다.
- 텍스트 초안은 `pie-demo-draft:{projectId}` 키로 Project별 `localStorage`에 보관한다.
- 서버가 메시지를 수락한 뒤에만 초안과 첨부를 지운다. 실패하면 작성 내용은 유지한다.
- SSE `transport.connected` 이벤트를 받으면 Conversation을 다시 조회해 오래된 화면 상태를
  갱신한다.
- `conversation.idle` 또는 명시적 오류에서는 원인과 `다시 연결` 버튼을 표시한다.
- Project 생성 요청은 Conversation 준비 루프를 기다리지 않고 완료되므로 생성 모달이
  장시간 멈추지 않는다.

`localStorage` 초안은 편의 기능이며 민감한 비밀이나 장기 PAT를 입력·보관하는 용도가
아니다. 고객 서비스가 더 강한 보안 정책을 요구하면 암호화된 서버 초안 또는 세션 전용
메모리 저장으로 교체한다.

## 5. 재시도 API

```http
POST /v1/integrations/{integrationId}/conversations/{conversationId}/retry
Authorization: Bearer {integration-service-token}
Idempotency-Key: {request-id}
```

동작 원칙은 다음과 같다.

1. Integration과 Integration User 소유권을 다시 확인한다.
2. `deleted` Conversation은 복구하지 않는다.
3. 이미 `ready`이며 Session도 `ready/active`이면 중복 재기동하지 않는다.
4. 오류·종료된 clientd Session을 정리하고 같은 Session ID를 `starting`으로 되돌린다.
5. Conversation ID, Project, Agent session ID, append-only event journal은 유지한다.
6. Executor가 유휴 정지 상태이면 Controller의 `EnsureWithLimits`가 기존 전용 volume을
   다시 마운트하여 컨테이너를 재생성한다.

예제 BFF는 브라우저의 CSRF와 로그인 소유권을 검사한 뒤 같은 API를 대리 호출한다.
Integration service token은 브라우저에 노출하지 않는다.

## 6. 유휴 정리 정책

두 단계로 나누어 사용자 체감과 서버 비용을 함께 관리한다.

| 단계 | 기본값 | 정리 대상 | 보존되는 것 |
|---|---:|---|---|
| Session idle | 15분 | Relay peer, clientd Claude Session | Conversation, journal, Project, Home/Workspace/auth volume |
| Executor idle | 1시간 | 사용자 전용 Docker 컨테이너 | 사용자별 Home/Workspace/auth volume과 Control Plane 기록 |

활성 Claude turn 또는 permission 흐름이 있으면 Session을 종료하지 않는다. 정리기는
Gateway의 요청 lock과 Conversation CAS(version)를 함께 사용하므로 새 요청 수락과 유휴
종료가 엇갈리지 않는다. Executor는 활성 Docker Session이나 초기화 중인 Project가 하나라도
있으면 정지하지 않는다.

관련 환경변수는 다음과 같다.

| 환경변수 | 기본값 | 설명 |
|---|---:|---|
| `PIE_CHAT_IDLE_SCAN_INTERVAL` | `1m` | 유휴 상태 검사 주기 |
| `PIE_CHAT_SESSION_IDLE_TIMEOUT` | `15m` | 마지막 채팅 활동 후 Session 종료 기준 |
| `PIE_EXECUTOR_IDLE_TIMEOUT` | `1h` | 활성 Session이 없을 때 컨테이너 정지 기준 |

무료·유료 요금제별로 시간이 달라져야 한다면 다음 단계에서 Integration/User policy로
내려야 한다. 현재 값은 Manager Node 전체의 운영 기본값이다.

## 7. 관측 지표

Manager `/metrics`에 다음 누적 지표를 추가했다.

```text
pie_chat_idle_sessions_closed_total
pie_chat_idle_executors_stopped_total
pie_chat_idle_errors_total
```

`errors_total` 증가, `connecting/error` 장기 지속, cold-start p95 증가를 함께 경보로
묶는 것이 좋다. 유휴 정지 자체는 정상 동작이므로 단독 경보 대상으로 삼지 않는다.

## 8. 검증 결과

2026-08-03 로컬 코드 기준으로 다음 검증을 통과했다.

- Go Control·Chat Gateway·Manager 단위 테스트
- 유휴 Conversation claim → lifecycle event → Session/Conversation closed 테스트
- 같은 Conversation 재시도 → Session `starting`, Conversation `connecting` 복구 테스트
- Next.js type check와 production build
- BFF 로그인·CSRF·A/B 소유권 격리·Project·채팅·SSE·이미지·permission 테스트
- BFF `retry` 전달과 `Idempotency-Key` 보존 테스트
- `./deploy/local/pie-local.sh test` 전체 Docker E2E
  - 회원 lifecycle, 사용자별 컨테이너와 credential 격리, `kroot init`
  - Relay driver 양도, 실제 PTY 입력·출력 왕복과 참가자 강제 종료
  - 동시 참가자 20명 연결: 최종 실행 기준 handshake p95 `5.8ms`, 최대 `7.6ms`
  - Relay 재시작 후 새 generation 재연결, HTTP/WS와 TLS/WSS 재접속
  - 제3자 채팅 permission, Manager 재시작 중 turn 복구, Executor 재시작 복구
  - Next.js 16 production build, 브라우저 로그인·회원가입·Project·이미지 첨부·SSE
  - Prometheus target 확인과 PostgreSQL·상태 archive 백업/복원 drill

독립 E2E Manager는 운영 Relay의 presence 보고 대상이 아니므로 시험 시작 시 해당
Manager의 Node registry에 Relay Node를 명시적으로 등록한다. 실제 운영에서는 Relay가
Manager의 `/v1/control/relay/presence`로 heartbeat를 보내 이 정보를 지속 갱신한다.

운영 전에는 실제 CookAI 시험 서버에서 의도적으로 timeout을 1~2분으로 낮춰
`ready → idle closed → executor stopped → retry → ready → Claude 응답` 전체 cold-start
E2E와 p50/p95 시간을 한 번 더 측정한다. 이 측정은 기능 구현과 별도로 서버 이미지 pull,
Docker 저장장치, Relay RTT에 영향을 받는다.
