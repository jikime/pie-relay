# Kroot 서버 단계 배포 결과 — 2026-08-04

이 문서는 `pie-sandbox-test` 서버에 최신 Pie Relay 스택을 단계적으로 배포한 결과와
검증 증거, 복구 지점을 기록한다. 기존 Kroot 서비스와 PostgreSQL을 건드리지 않고
Pie Compose project 안에서만 작업했다.

## 1. 배포 결과

| 항목 | 배포값 | 상태 |
|---|---|---|
| 기준 소스 | `29d2d2b2098b7faf2746bc409930dcec24b38296` | 원격 저장소 반영 |
| Relay | `pie-relay-server:e9134a0f654f` | healthy |
| Manager | `pie-executor-manager:29d2d2b2098b` | healthy |
| Web Chat | `pie-third-party-web-chat:e9134a0f654f` | healthy |
| 사용자 Executor | `pie-relay-client-kroot:e9134a0f654f` 3개 | 모두 healthy |
| PostgreSQL | `postgres:16-alpine` | 재시작 없이 healthy |

현재 릴리스 링크는 다음 경로를 가리킨다.

```text
/home/kaonkroot/pie-sandbox-test/current
  → /home/kaonkroot/pie-sandbox-test/releases/20260804-29d2d2b2098b
```

## 2. 수행 순서

1. 로컬 Go race test, Node test, Next.js check/build와 전체 로컬 E2E를 통과시켰다.
2. 배포 전 DB·영속 데이터·Relay state·환경설정·기존 소스를 백업했다.
3. 커밋별 immutable image tag로 후보 이미지를 만들었다.
4. 후보 Relay에서 인증, Origin, 역할 격리, 양방향 WSS와 재연결을 검증했다.
5. 후보 Manager·Web Chat에서 가입, 프로비저닝, 사용자 격리, 프로젝트 생성,
   첨부 파일, SSE와 Claude Code 왕복을 검증했다.
6. Relay → Manager → Web Chat 순으로 교체하고 사용자 Executor를 정상 기동했다.
7. 사용자별 `.kroot`, `.claude`, workspace 해시가 교체 전후 동일한지 확인했다.
8. 공식 도메인에서 실제 Claude 응답, Relay 재시작 복구와 보안 검사를 수행했다.
9. 배포 후 백업을 서버와 별도 로컬 디스크에 각각 저장하고 체크섬을 검증했다.

## 3. 배포 중 발견하고 수정한 문제

Relay 노드는 두 주소를 가진다.

- `Address`: 외부 참가자와 격리된 사용자 Executor가 쓰는 공개 HTTPS/WSS 주소
- `ControlAddress`: Manager가 연결 종료와 Driver 제어에 쓰는 내부 HTTP 주소

첫 대화 이후 Relay presence가 내부 주소 `http://relay:13412`를 노드에 기록하면,
새 Docker 세션도 그 주소를 받아 `lookup relay ... no such host`로 실패하는 문제가
있었다. 사용자 Executor는 보안상 Relay 내부 Docker network에 연결하지 않으므로
이 동작은 잘못된 것이었다.

커밋 `29d2d2b`에서 Docker host WebSocket은 항상 노드의 공개 `Address`를 사용하도록
수정하고, 공개 주소와 내부 주소가 서로 다를 때도 공개 WSS가 선택되는 회귀 테스트를
추가했다. 수정 뒤 새 세션은 `wss://relay.cookai.dev/ws/agent`로 연결됐다.

## 4. 실제 검증 결과

### 서비스와 보안

| 검사 | 결과 |
|---|---|
| `relay.cookai.dev/healthz`, `/readyz` | HTTP 200 |
| `chat-relay.cookai.dev/api/health` | HTTP 200, PostgreSQL user store |
| Relay `/metrics` 비인증 요청 | HTTP 401 |
| Manager `/v1/admin/overview` 비인증 요청 | HTTP 401 |
| Web Chat `/api/session` 비인증 요청 | HTTP 401 |
| 허용 Origin | 정확한 `Access-Control-Allow-Origin` 반환 |
| 비허용 Origin | CORS 허용 헤더 없음 |
| 최근 로그의 panic/fatal | 0 |
| 로그의 PAT/token/password 값 노출 패턴 | 0 |

### Relay와 Claude E2E

- Relay 독립 smoke 13개 항목 통과: health, ready, 보호된 metric, enroll 인증,
  모바일 assignment, invite/join, Origin, 역할 격리, host/participant WSS,
  양방향 전송, participant/host 재연결
- 공개 Web Chat 로그인과 provisioning recovery 통과
- 프로젝트 준비 후 새 Docker `clientd` host 연결 통과
- 실제 Claude Code marker 응답과 SSE `text`, `done` 수신 통과
- 브라우저 대화 교체 lifecycle 통과
- Relay 컨테이너 재시작 후 다시 만든 대화에서 실제 Claude 응답 통과

관리자 overview의 유휴 Docker 장치는 `degraded`로 표시될 수 있다. 이는 컨테이너
고장이 아니라 현재 등록된 Relay host 세션이 없다는 뜻이다. 함께 보는 기준은
`runningRuntimes`, Docker health와 restart count이며, 배포 종료 시 3개 runtime은
모두 running/healthy이고 restart count는 0이었다.

## 5. 백업과 복구 지점

배포 전 백업:

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-3f72c85adf48
/Users/jikime/Dev/Private/cli-relay-backups/20260804-3f72c85adf48
```

배포 후 백업:

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-postdeploy-29d2d2b2098b
/Users/jikime/Dev/Private/cli-relay-backups/20260804-postdeploy-29d2d2b2098b
```

배포 후 백업에는 환경설정, Compose, 배포 소스, PostgreSQL custom dump와 restore
catalog, Pie 영속 데이터, Web Chat 상태, Relay state, image manifest가 포함된다.
서버·외부 사본 모두 `SHA256SUMS` 검증을 통과했고 백업 디렉터리는 mode `700`이다.

## 6. Manager 단독 롤백

이번 변경은 Manager만 새 이미지이므로 문제가 생기면 다음 순서로 되돌린다.
운영 중에는 먼저 신규 대화 생성을 중지하고 실행 중인 대화가 끝났는지 확인한다.

```bash
ssh pie-sandbox-test
cd /home/kaonkroot/pie-sandbox-test

# src/deploy/test-server/.env의 PIE_MANAGER_IMAGE를
# pie-executor-manager:e9134a0f654f 로 되돌린다.

docker compose -p pie-sandbox-test \
  --env-file src/deploy/test-server/.env \
  -f releases/20260804-e9134a0f654f/src/deploy/test-server/compose.yaml \
  up -d --no-deps --force-recreate manager
```

DB나 사용자 영속 데이터를 복원해야 하는 장애라면 즉시 덮어쓰지 않는다. 먼저 현재
상태를 별도 보존하고, [배포 및 운영 가이드](./deployment-and-operations.md)의 복원
절차에 따라 격리된 검증 DB에서 dump와 파일 백업의 호환성을 확인한 뒤 진행한다.

## 7. 남은 운영 승인 항목

이번 결과는 공유 Kroot 서버의 단계 배포와 기능 검증 합격이다. 대규모 고객 공개 전에
다음 운영 항목은 별도 게이트로 남겨 둔다.

- 1→2→4→8 동시 사용자 부하와 8시간 soak
- PostgreSQL PITR 및 서버 외부 사본을 이용한 정기 복원 훈련
- image registry digest 고정, SBOM·서명·Critical/High CVE 차단
- 디스크·inode·OOM·재연결·429·인증서 만료 경보
- 다중 Executor node scheduler와 drain/workspace 이전

## 8. 15명 admission 한계 시험 설정

같은 날 실제 사용량과 무관하게 사용자·컨테이너 생성 한계를 관찰하기 위해 다음 두
값을 4에서 15로 올렸다.

```text
PIE_EXECUTOR_MAX_EXECUTORS=15
PIE_WEB_CHAT_MAX_USERS=15
```

Manager를 재생성해 Executor 한도 15를 확인했고 `cookai-e2e` Integration의
`maxUsers=15`도 확인했다. 정리 시점에는 `lets.anthony.kim`의 사용자 컨테이너 하나만
남겼기 때문에 신규 할당 가능 슬롯은 14개다.

설정 반영 후 임시 계정으로 공개 Web Chat 회원가입을 다시 수행했다. 신규 사용자 row,
전용 컨테이너와 workspace가 생성되고 Relay를 거쳐 실제 Claude marker 응답까지
성공했다. 검증이 끝난 임시 계정·컨테이너·상태 디렉터리는 즉시 다시 제거했다.

기존 Alice, Bob과 가입 E2E 사용자 할당은 공식 Integration DELETE 경로로 중지했다.
이 과정에서 대화를 닫고 사용자 자격을 제거한 뒤 컨테이너 3개를 삭제했다. Web Chat
DB에서는 `lets.anthony.kim` 외 로그인 계정 3개를 삭제하고 초기 seed를 비웠다. 삭제된
세 사용자의 executor state, workspace와 blob 디렉터리도 운영 데이터 root에서
제거했다. Manager의 suspended/deleted tombstone은 감사 추적과 멱등 재시도를 위해
남으며 quota를 점유하지 않는다.

삭제 전 복구 지점은 다음 두 곳에 있고 모두 체크섬 검증을 통과했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-pre-capacity15-cleanup
/Users/jikime/Dev/Private/cli-relay-backups/20260804-pre-capacity15-cleanup
```

정리가 끝난 현재 상태도 별도 백업했으며 서버·외부 사본 모두 체크섬 검증을 통과했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-capacity15-cleanup
/Users/jikime/Dev/Private/cli-relay-backups/20260804-post-capacity15-cleanup
```

이 설정은 admission 시험용이다. Executor당 CPU·메모리 제한의 합이 host 물리 용량을
넘을 수 있으므로 15명 동시 Claude 부하의 안전성을 보장하지 않는다. 실제 한계는
단계별 가입·cold start·메모리·swap/OOM·응답 지연을 함께 관찰해 판정한다.

## 9. Docker Executor 무승인 도구 실행 배포

Web Chat의 Claude 도구 호출이 승인 대기에서 멈추지 않도록 사용자별 Docker Executor에
`bypassPermissions`를 고정했다. Desktop과 실제 PC의 Host OS 직접 연결에는 이 값을
전파하지 않아 기존 승인 경계를 유지한다.

| 항목 | 결과 |
|---|---|
| 소스/현재 릴리스 | `49f4e4127161` / `releases/20260804-49f4e4127161` |
| Manager 이미지 | `pie-executor-manager:49f4e4127161` |
| Executor 이미지 | `pie-relay-client-kroot:49f4e4127161` |
| 정책 주입 | `CLI_RELAY_PERMISSION_MODE=bypassPermissions` |
| SDK 필수 플래그 | `allowDangerouslySkipPermissions=true` |
| 동시 실사용 E2E | 사용자 2명, 7.7초 |
| 실제 도구 이벤트 | `Bash tool_call`, `tool_result`, `done` |
| 승인 요청 | 0건 |
| 배포 후 상태 | Manager 및 Executor 2개 healthy, restart 0, active turn 0 |

교체 전 승인 대기 중이던 turn 2개는 Integration cancel API로 정상 종료했다. 이어서
Manager를 중지하고 기존 Executor 컨테이너 본체 2개만 삭제한 뒤 새 이미지로 다시
만들었다. 사용자별 `/home/executor`와 `/workspace`는 host bind mount이므로 삭제하지
않았으며, 교체 전후 Claude·Kroot credential 4개 checksum이 동일함을 확인했다.

실사용 검증은 기존 대화 문맥의 영향을 피하려고 사용자마다 새 Conversation을 만들었다.
모델이 답을 미리 알 수 없는 `/proc/sys/kernel/random/uuid`를 Bash로 읽게 하고 두 계정에
동시에 요청했다. 두 경로 모두 승인 이벤트 없이 도구 실행과 응답을 끝냈고 시험용
Conversation은 삭제했다.

복구 지점은 다음과 같으며 PostgreSQL dump, Pie 영속 데이터, 설정, 컨테이너 manifest와
checksum 검증 파일을 포함한다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-pre-bypass-49f4e41
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-bypass-49f4e41
```

이 정책은 사용자별 비-root·read-only root filesystem·capability 제거·자원 제한·격리
network가 적용된 Docker sandbox만을 신뢰 경계로 삼는다. 컨테이너 안에서는 Claude가
파일과 명령을 승인 없이 사용할 수 있으므로 자격증명 최소화와 egress 제한은 별도 운영
과제로 계속 관리해야 한다.

## 10. Web Chat 실시간 작업 스트리밍 배포

> 이 절의 작업 단계 요약 UI는 이후 **12. Claude 원본 이벤트 인라인 스트리밍**에서
> 일반 채팅형 원본 이벤트 UI로 교체했다. 아래 내용은 최초 스트리밍 배포 이력이다.

컨테이너와 Chat Gateway는 원래부터 `text`, `thinking`, `task_*`, `tool_call`,
`tool_result`를 순서가 있는 SSE journal로 전달하고 있었다. Web Chat이 이 중 text와 일반
상태만 표시해, Claude가 파일·명령·하위 작업을 수행하는 시간에는 화면이 멈춘 것처럼
보였다. 다음 항목을 운영 경로에 배포했다.

- 메시지 제출 직후 사용자 메시지·작업 카드·Claude placeholder를 먼저 만드는 optimistic UI
- 내부 thinking 원문이나 도구 입출력 원문을 노출하지 않는 작업 단계 요약
- `task_started/progress/complete`, `tool_call/result` 실시간 작업 카드
- text delta의 animation-frame batching
- Relay 상태와 SSE 수신 상태의 분리 표시
- SSE heartbeat, `Last-Event-ID`와 `after` 기반 중복 없는 재접속
- 완료 이벤트 처리 시 React 비동기 state 갱신과 request ID 초기화 사이의 경합 제거

| 항목 | 결과 |
|---|---|
| 기능 커밋 | `e90d130605d3` |
| PostgreSQL 빈 seed 재시작 보강 | `cba6dce3a514` |
| 작업 카드 완료 경합 수정 | `23f4fe6bb8ac` |
| 브라우저 도구 진행 E2E | `431200a278f7` |
| Manager 이미지 | `pie-executor-manager:e90d130605d3` |
| Web Chat 이미지 | `pie-third-party-web-chat:23f4fe6bb8ac` |
| 운영 release | `releases/20260804-431200a278f7` |
| 복구 지점 | `backups/20260804-pre-streaming-e90d130` |
| 서비스 상태 | Manager/Web Chat healthy, restart 0 |
| 공개 E2E | 회원가입→Executor→Relay→Claude 및 Chrome 실시간 UI 성공 |

첫 Web Chat 재시작에서 PostgreSQL 이관 완료 후 비워 둔 `users.json`을 예전 file-store
규칙이 거부하는 문제가 발견됐다. PostgreSQL이 durable user store인 경우에는 빈 seed
배열을 허용하되, file store는 기존처럼 최소 한 명을 요구하도록 구분했다. DB dump와
배포 전 환경·container inspect는 위 복구 지점에 mode 0600으로 보관했다.

최종 Chrome E2E는 임의 UUID를 얻기 위해 컨테이너에서 실제 Bash 작업을 실행했다.
최종 답변 전에 `명령어를 실행하고 있어요` 단계가 화면에 나타났고, 이후 작업 카드가
완료 상태로 전환되며 UUID 응답이 도착했다. 시험 종료 후 시험 사용자의 활성
Conversation은 0건이며, 다른 사용자의 기존 활성 Conversation에는 손대지 않았다.

## 11. 브라우저 실시간 수신 연결 가드

실제 `lets.anthony.kim` 대화 journal을 시간순으로 확인한 결과, 컨테이너와 Manager는
요청 수락 뒤 4초 만에 첫 thinking, 6초 만에 첫 text를 전달했고 약 12초에 요청을
완료했다. 반면 문제가 보고된 브라우저를 확인한 시점의 SSE subscriber는 0이었다.
즉 Claude나 Relay가 느린 것이 아니라, 브라우저 실시간 수신이 끊긴 상태에서도 메시지
POST가 허용되어 작업 결과를 화면이 받지 못한 것이 직접 원인이었다.

다음 안전장치를 Web Chat에 추가했다.

- 대화가 `ready`여도 SSE가 `OPEN`이 아니면 전송 버튼 비활성화
- 폼을 직접 제출해도 실제 `EventSource.OPEN`을 재검사해 POST 차단
- 연결 단절 상태와 자동 재연결을 별도 상태 문구로 명확히 표시
- 작성 중인 초안을 보존하는 `실시간 재연결` 버튼 제공
- SSE 오류 시 `/api/auth/me`를 제한적으로 확인해 만료 세션을 로그인 화면으로 전환
- 로그인 뒤 발생하는 모든 API 401도 같은 만료 처리 경로로 통합
- 이전 EventSource의 늦게 도착한 callback이 새 연결 상태를 덮지 못하도록 source 식별
- 브라우저 E2E에서 네트워크 단절 시 전송 차단, 네트워크 복구 후 재연결과 전송까지 확인

| 항목 | 결과 |
|---|---|
| 커밋 | `db2f3be56710` |
| Web Chat 이미지 | `pie-third-party-web-chat:db2f3be56710` |
| 운영 release | `releases/20260804-db2f3be56710` |
| 배포 전 복구 지점 | `backups/20260804-pre-stream-guard-db2f3be` |
| 배포 후 복구 지점 | `backups/20260804-post-stream-guard-db2f3be` |
| 로컬/이미지 검사 | type check, 22 tests 통과, 선택적 PostgreSQL test 1개 skip, 운영 build 성공 |
| 공개 브라우저 E2E | `liveStreamReady=true`, `streamSendGuardObserved=true`, 실제 Bash 진행 표시, UUID 응답 성공 |
| 시험 종료 정리 | E2E 활성 Conversation 0, active turn 0, subscriber 0 |
| 서비스 상태 | Relay·Manager·Web Chat healthy, restart 0 |

첫 공개 E2E 시도는 과거 정리된 계정을 가리키던 `web-chat/login.json` 때문에 로그인
401이 발생했다. 배포 기능 장애는 아니었으며, 현재 전용 시험 계정 파일인
`web-chat/signup-login.json`으로 실행해 전체 경로를 통과했다. 운영 가이드의 수동 로그인과
브라우저 E2E 명령도 같은 파일을 사용하도록 바로잡았다.

## 12. Claude 원본 이벤트 인라인 스트리밍

작업 단계를 한 카드 안에서 `파일을 작성하고 있어요`처럼 가공해 보여주던 UI를 없애고,
Claude Code가 보내는 이벤트를 일반 AI 채팅처럼 발생 순서대로 메시지 목록에 바로
추가하도록 변경했다.

```text
사용자 메시지
→ Claude thinking 원문
→ tool_call: 정확한 도구 이름과 input 원문
→ tool_result: content 원문과 성공·오류 상태
→ 후속 Claude text delta
→ done
```

`text`는 첫 delta가 도착할 때 Claude 말풍선을 만들고 이후 delta를 같은 말풍선에
이어 붙인다. 도구나 task가 시작되면 기존 Claude 문단을 확정하고 새 인라인 항목을
추가하며, 도구 뒤에 오는 text는 별도의 다음 Claude 말풍선으로 이어진다. 따라서 한
개의 상태 카드가 계속 바뀌는 방식이 아니라 ChatGPT·Claude 웹 대화처럼 시간 순서가
메시지 목록 자체에 남는다.

Executor는 Claude의 `tool_use.id`와 `tool_result.tool_use_id`를 `toolCallId`로 보존해
동시에 여러 도구가 실행돼도 결과를 올바른 호출에 붙인다. 구버전처럼 ID가 없는 이벤트도
FIFO로 연결해 기존 journal replay와 호환한다. `is_error`도 보존하므로 실패한 도구는
정상 완료로 오인하지 않는다. Relay와 Manager는 기존부터 원본 event payload를 journal과
SSE로 전달하고 있었으므로 프로토콜을 새로 가공하지 않았다.

첫 운영 브라우저 검증에서 도구 이름·입력·결과는 정상 표시됐지만 마지막 Claude
말풍선이 비는 문제를 발견했다. placeholder를 없앤 뒤 첫 text/thinking delta가 왔을 때
message ID가 아직 없으면 `requestAnimationFrame` callback이 렌더를 건너뛰던 것이
원인이었다. 첫 delta가 메시지를 직접 만들도록 수정했고, 바로 다음 tool event가 와서
frame 실행 전에 segment를 닫더라도 flush가 내용을 보존하도록 보강했다.

| 항목 | 결과 |
|---|---|
| 원본 인라인 이벤트 커밋 | `b448434` |
| 첫 text/thinking segment 수정 | `13cf47b` |
| Kroot 완료 표식·Markdown 검증 | `9d07135` |
| Executor 이미지 | `pie-relay-client-kroot:b448434` |
| Web Chat 이미지 | `pie-third-party-web-chat:9d07135` |
| 현재 release | `releases/20260804-9d07135` |
| 배포 전 복구 지점 | `backups/20260804-pre-raw-tool-stream-b448434`, `backups/20260804-pre-first-stream-segment-13cf47b` |
| 배포 후 복구 지점 | `backups/20260804-post-raw-inline-stream-13cf47b` |
| 로컬 검증 | Web Chat type check, 22 tests 통과, 선택적 PostgreSQL test 1개 skip, 운영 build 성공; Node Executor 65 tests 통과 |
| 공개 Chrome E2E | `toolName=Bash`, `rawToolInputObserved=true`, `rawToolResultObserved=true`, UUID 도구 결과와 최종 답변 일치 |
| 연결 복구 E2E | `liveStreamReady=true`, `streamSendGuardObserved=true` |
| 종료 상태 | Relay·Manager·Web Chat·Executor 2개 healthy, restart 0, active turn 0, subscriber 0 |

Kroot가 응답 끝에 추가하는 `<kroot>DONE</kroot>`는 대화 내용이 아니라 완료 제어
표식이므로 화면 표시 단계에서만 제거한다. 원본은 Relay journal에 남겨 추적 가능하며,
태그가 여러 SSE delta로 분리돼도 완성 전 조각이 화면에 잠깐 나타나지 않도록 trailing
prefix를 보류한다. Claude 본문은 기존 `MarkdownContent`로 렌더링하며 공개 Chrome
E2E에서 굵은 글씨와 인라인 코드 DOM, `krootDoneFiltered=true`,
`markdownObserved=true`를 확인했다.

배포 중 첫 건강 검사는 기동 초기에 잠깐 나타난 `unhealthy`를 즉시 최종 실패로 판단해
자동 롤백됐다. 영속 볼륨은 건드리지 않았고 기존 서비스는 정상 복구됐다. 이후 검사를
최대 대기 시간 동안 `healthy` 전환을 기다리도록 수정해 재배포했다. Executor 컨테이너
본체만 교체했으며 교체 전후 `.claude/.credentials.json`과
`.kroot/credential.json` 네 파일의 SHA-256이 모두 일치했다.

배포 후 백업은 서버와 다음 외부 사본에 보관했고 `SHA256SUMS` 검증을 통과했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-raw-inline-stream-13cf47b
/Users/jikime/Dev/Private/cli-relay-backups/20260804-post-raw-inline-stream-13cf47b
```

## 13. 프로젝트 웹 프리뷰 운영 배포

사용자별 Executor 안에서 프로젝트 개발 서버를 실행하고
`p-{random}.preview.kroot.io`로 접근하는 웹 프리뷰 기능을 운영 경로에 배포했다.
공용 Traefik 원본은 수정하지 않았고 별도 Compose override로 공인 와일드카드
인증서만 file provider에 추가했다.

| 항목 | 결과 |
|---|---|
| 기능 커밋 | `72238e9` |
| TLS override | `de1b5d6` |
| 원격 E2E 보강/운영 소스 | `a4a7d25` |
| Manager 이미지 | `pie-executor-manager:deab4b4` |
| Executor 이미지 | `pie-relay-client-kroot:deab4b4` |
| Preview Gateway 이미지 | `pie-preview-gateway:deab4b4` |
| Web Chat 이미지 | `pie-third-party-web-chat:deab4b4` |
| 현재 release | `releases/20260804-a4a7d25` |
| 인증서 | Let's Encrypt YE1, `preview.kroot.io` + `*.preview.kroot.io` |
| 인증서 만료 | 2026-11-02 |
| 기존 서비스 영향 | Relay 재시작 없음, PostgreSQL 재시작 없음 |

배포 전에는 모든 Conversation 52개가 `closed`인지 확인하고 다음 복구 지점을 만들었다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-pre-preview-de1b5d6
```

Manager를 먼저 교체한 뒤 기존 사용자 Executor 본체 한 개를 새 이미지로 재생성했다.
사용자별 `/home/executor`와 `/workspace` bind mount는 삭제하지 않았다. 교체 전후
`.claude/.credentials.json`, `.kroot/credential.json`의 SHA-256이 각각 일치했고,
workspace도 파일 607개·6.0MB로 동일했다.

실제 운영 E2E는 임시 Integration과 사용자로 다음 항목을 검증했다.

- 사용자 프로비저닝과 프로젝트 `kroot init`
- 프리뷰 4개 동시 port 할당과 5번째 quota 거부
- 사용자 전용 internal network, Host port 미노출
- 비공개 launch token/cookie와 hostname 간 재사용 차단
- 인증 cookie, 애플리케이션 cookie와 `X-Pie-*` header 격리
- request body, chunked streaming, WebSocket upgrade
- 공개 접근, 로그, 재시작, 중지와 port 재사용
- 사용자 정지 후 route·Executor·preview network 회수

전체 20개 검사가 성공한 뒤 시험 Integration을 `revoked`로 전환했다. 활성 프리뷰와
시험 network는 0개이며 기존 사용자 Executor는 새 production 이미지로 healthy 상태다.

배포 후 복구 지점은 서버와 로컬 외부 사본에 각각 보관했고 PostgreSQL restore
catalog와 모든 파일의 `SHA256SUMS`를 검증했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-preview-a4a7d25
/Users/jikime/Dev/Private/cli-relay-backups/20260804-post-preview-a4a7d25
```

이번 공인 인증서는 수동 DNS-01로 발급했으므로 자동 갱신되지 않는다. 고객 공개
전에는 Gabia DNS 자동화 또는 `acme-dns` 위임, 만료·갱신 실패 경보를 반드시
완료해야 한다.

## 14. 하위 앱 경로 및 유휴 Executor 복구 배포

프로젝트 안에 여러 웹 앱이 있는 경우 임의의 `package.json`을 찾지 않고 사용자가
선택한 상대경로만 실행하도록 프리뷰 모델과 Web Chat을 보강했다. 최초 운영 검증에서
기존 `랜딩페이지` 프로젝트의 실제 Next.js 앱이 프로젝트 루트가 아니라
`company-landing`에 있음을 확인했다.

| 항목 | 결과 |
|---|---|
| 앱 경로 기능 커밋 | `99352b9` |
| 유휴 Executor 자동 복구 커밋 | `d4e5278` |
| Manager 이미지 | `pie-executor-manager:d4e5278` |
| Executor 이미지 | `pie-relay-client-kroot:99352b9` |
| Web Chat 이미지 | `pie-third-party-web-chat:99352b9` |
| Preview Gateway 이미지 | `pie-preview-gateway:deab4b4` (재시작 없음) |
| Relay 이미지 | `pie-relay-server:e9134a0f654f` (재시작 없음) |
| 현재 release | `releases/20260804-d4e5278` |

`appPath`는 프로젝트 루트 기준 POSIX 상대경로로 저장한다. 절대경로·상위 경로 이탈과
심볼릭 링크 이탈을 차단하고, 선택한 디렉터리의 `package.json`과 `scripts.dev`를
검증한다. lockfile이 있으면 `npm ci`, 없으면 `npm install`을 수행하며 성공 지문과
프로세스 소유 설치 잠금으로 중복 설치 및 고아 잠금을 방지한다.

첫 운영 요청에서는 유휴 회수된 Executor의 registry 상태가 `stopped`라 미리보기
네트워크 생성 전에 `503 executor runtime not found`가 발생했다. Manager의
`EnsurePreviewNetwork`와 `StartPreview`가 동일 사용자의 Executor를 멱등적으로 먼저
복구하도록 수정하고, stopped Executor에서 프리뷰를 생성하는 회귀 테스트를 추가했다.

실제 운영 검증 결과는 다음과 같다.

- 프로젝트: `랜딩페이지`, 앱 경로: `company-landing`
- 프리뷰: `preview-9169836805d6e3035f915d65025288cb`
- hostname: `p-jirpn47t6l3sit3labiloox5eu.preview.kroot.io`
- Executor 이미지·health: `pie-relay-client-kroot:99352b9`, healthy, restart 0
- 정확한 작업 경로의 `package.json`, `node_modules`, npm lockfile, 의존성 지문 확인
- 비공개 launch URL 200, 쿠키 재접속 200, 무인증 접근 401
- 재시작 후 동일 hostname·`company-landing` 유지 및 HTTPS 200
- Manager·Web Chat·Relay·PostgreSQL·Preview Gateway 모두 healthy, restart 0

배포 전·후 복구 지점은 다음과 같다. 사후 백업은 서버와 로컬 외부 사본에서 모든
`SHA256SUMS` 검증을 통과했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-pre-preview-app-path-99352b9
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-preview-app-path-d4e5278
/Users/jikime/Dev/Private/cli-relay-backups/20260804-post-preview-app-path-d4e5278
```

실사용 확인을 위해 위 프리뷰는 문서 작성 시점에도 `ready`로 유지했다. Web Chat에서
같은 프로젝트와 `company-landing` 경로를 선택하면 기존 주소를 다시 열거나 동일
hostname으로 재시작할 수 있다.

## 15. 프리뷰 재실행 버전 충돌 보강

Web Chat에서 기존 프리뷰를 다시 실행할 때 드물게
`control record version conflict`가 표시되는 문제를 보강했다. 단일 운영 Manager에서
같은 요청을 다시 수행했을 때는 재현되지 않았지만, 기존 구현의 잠금이 프리뷰 서비스
인스턴스 내부에만 있어 Manager 교체 시점이나 다중 replica에서 동일 버전을 동시에
읽을 수 있는 구조적 위험을 확인했다.

| 항목 | 결과 |
|---|---|
| 상태 전이 직렬화 커밋 | `27b52f2` |
| 조정 충돌 수렴 커밋 | `b8c2522` |
| Manager 이미지 | `pie-executor-manager:b8c2522` |
| 현재 release | `releases/20260804-b8c2522` |
| Executor·Web Chat 변경 | 없음 |
| Relay·PostgreSQL·Preview Gateway 재시작 | 없음 |

생성·중지·재시작 전체 전이를 동일 프리뷰 ID의 로컬 잠금과 PostgreSQL advisory lock으로
직렬화했다. 잠금 획득 후 공유 저장소를 갱신하고, CAS 방식의 버전 저장이 충돌하면 최신
레코드를 읽어 최대 4회 제한 재시도한다. 반면 2초마다 수행하는 상태 관찰은 긴 Docker
작업 동안 DB 트랜잭션을 점유하지 않도록 로컬 잠금만 사용한다. 비동기 조정 goroutine도
서비스 수명주기에 포함해 Manager 종료 시 취소하고 완료를 기다리도록 변경했다.

검증 결과는 다음과 같다.

- 두 프리뷰 서비스가 같은 레코드를 동시에 재시작하는 회귀 테스트 통과
- `go test ./...`와 control·preview·manager 패키지 race 검사 통과
- 로컬 전체 스택 E2E 통과: 프리뷰 재시작·로그·중지·격리·복구 포함
- 운영 서버 동시 재시작 요청 6개가 모두 HTTP 202
- 운영 서버 `중지 → 다시 실행`이 HTTP 200/202
- 최종 상태 `ready`, `company-landing`, 기존 hostname 유지, HTTPS 200
- PostgreSQL 레코드와 JSON 내부 버전이 모두 35로 일치
- 배포 후 Manager의 version conflict·panic·fatal 로그 0건
- 모든 운영 서비스 healthy, restart 0

배포 전·후 복구 지점과 로컬 외부 사본은 다음과 같으며 체크섬 검증을 통과했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-pre-preview-conflict-27b52f2
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-preview-conflict-b8c2522
/Users/jikime/Dev/Private/cli-relay-backups/20260804-post-preview-conflict-b8c2522
```

## 16. 프로젝트 앱 자동 탐지와 선택값 영속화

Web Chat의 `앱 경로` 입력란은 컨테이너 내부 구현 정보를 사용자에게 요구하고 선택값을
브라우저 `localStorage`에만 보관하는 구조였다. 이를 제거하고 clientd가 실행 가능한
웹 앱을 탐지하며 Manager의 Project 레코드에 선택값을 저장하도록 변경했다.

| 항목 | 결과 |
|---|---|
| 기능 커밋 | `c07ee03` |
| Manager 이미지 | `pie-executor-manager:c07ee03` |
| Executor 이미지 | `pie-relay-client-kroot:c07ee03` |
| Web Chat 이미지 | `pie-third-party-web-chat:c07ee03` |
| 현재 release | `releases/20260804-c07ee03` |
| Relay·Preview Gateway 변경 | 없음 |

clientd는 프로젝트 루트 아래에서 최대 깊이 5, 디렉터리 2,048개, 앱 64개 범위로
`package.json`과 `scripts.dev`를 검사한다. `node_modules`, 숨김·빌드 디렉터리와
디렉터리 심볼릭 링크는 따라가지 않는다. 외부에는 프로젝트 상대경로, package 이름,
감지된 실행 프로필만 반환하며 컨테이너 절대경로는 반환하지 않는다.

후보가 하나면 Web Chat이 자동 선택하고, 여러 개면 앱 이름과 프로필을 Select로
보여준다. 선택한 상대경로는 Project의 `previewAppPath`에 저장한다. 프리뷰 생성 요청이
`appPath`를 생략하면 이 저장값을 사용하되 기존 Integration의 명시적 `appPath` 요청은
호환성을 위해 유지한다. 탐지 API도 유휴 Executor를 멱등적으로 복구한 뒤 실행한다.

검증 결과는 다음과 같다.

- client와 Manager 전체 Go 테스트 통과, Web Chat 27개 테스트와 Next.js production build 통과
- 로컬 전체 스택 E2E 통과: `apps/web` 자동 탐지, Project 저장, 경로 생략 프리뷰 실행 포함
- 운영 `랜딩페이지`에서 `company-landing` Next.js 앱 단일 후보 자동 탐지
- Project `previewAppPath=company-landing`, version 3으로 PostgreSQL 영속화 확인
- 운영에서 `appPath` 없이 새 프리뷰 생성, `ready` 전환 후 시험 프리뷰 중지 확인
- 기존 비공개 프리뷰 launch cookie 교환과 HTTPS 200 확인
- Web Chat BFF 앱 목록 HTTP 200, 수동 경로 입력 제거와 `실행 앱` UI 확인
- 브라우저 실사용 검사 통과: 로그인, Relay, SSE, Bash 도구, Markdown, 완료 표식 필터링
- Manager·Web Chat·Executor 모두 새 이미지로 healthy, restart 0
- Manager version conflict·panic·fatal 및 Web Chat uncaught·unhandled·fatal 로그 0건

배포 전·후 복구 지점과 로컬 외부 사본은 다음과 같으며 모든 체크섬을 검증했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-pre-project-app-discovery-c07ee03
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-project-app-discovery-c07ee03
/Users/jikime/Dev/Private/cli-relay-backups/20260804-post-project-app-discovery-c07ee03
```

## 17. 프리뷰 단일화·공개 범위 전환·삭제 기능 배포

같은 프로젝트 앱에서 실행할 때마다 공개·비공개 프리뷰 레코드와 주소가 늘어나던 구조를
앱당 하나의 논리 프리뷰로 단순화했다. 공개 범위를 바꾸어도 process, port, hostname은
유지하며, 접근 세대(`accessVersion`)를 증가시켜 변경 전 launch token과 session cookie를
즉시 폐기한다. 삭제는 실행 프로세스를 먼저 중지한 뒤 Control 레코드와 hot index를
제거하고 `preview.deleted` 감사 기록은 별도로 남긴다.

| 항목 | 결과 |
|---|---|
| 기능 커밋 | `d510afe` |
| Manager 이미지 | `pie-executor-manager:d510afe` |
| Preview Gateway 이미지 | `pie-preview-gateway:d510afe` |
| Web Chat 이미지 | `pie-third-party-web-chat:d510afe` |
| 현재 release | `releases/20260804-d510afe` |
| 이전 release | `releases/20260804-c07ee03` |
| Relay·PostgreSQL·Executor 변경 | 없음 |

Preview Gateway를 먼저 배포해 기존 access generation 0 토큰을 계속 받을 수 있게 한 뒤,
새 토큰을 발급하는 Manager와 Web Chat을 순서대로 교체했다. 이 순서로 구버전 Manager와
신버전 Gateway가 잠시 공존해도 기존 비공개 프리뷰가 끊기지 않도록 했다.

검증 결과는 다음과 같다.

- Manager 전체 Go 테스트와 Web Chat 27개 테스트·Next.js production build 통과
- 로컬 전체 스택 E2E 통과: 동시 생성 단일화, 공개 범위 변경 시 identity 유지,
  기존 접근권한 폐기, 레코드 삭제, 삭제 후 새 hostname 발급과 port 재사용 포함
- 운영 Web Chat 로그인 cookie와 CSRF를 사용하는 BFF 경로에서 테스트 앱 자동 탐지
- 운영 공개 프리뷰 2회 실행과 두 hostname 모두 HTTPS 200 확인
- 첫 프리뷰 삭제 뒤 레코드 조회 및 외부 hostname 모두 HTTP 404
- 같은 프로젝트 앱 재실행 시 이전 ID·hostname을 재사용하지 않고 새 주소 발급
- 두 시험 프리뷰와 임시 앱 파일을 모두 삭제하고 시험 Project의 프리뷰 0건 확인
- `preview.deleted` 감사 기록 2건 확인
- 공식 API·Relay·Web Chat health endpoint 모두 HTTP 200
- Manager·Preview Gateway·Web Chat·Relay·PostgreSQL 모두 healthy, restart 0
- Manager version conflict·panic·fatal 및 Web Chat uncaught·unhandled·fatal 로그 0건

배포 전·후 복구 지점과 로컬 외부 사본은 다음과 같으며 PostgreSQL custom dump의
`pg_restore --list` 검사와 SHA-256 체크섬 검증을 통과했다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260804-pre-preview-delete-d510afe
/home/kaonkroot/pie-sandbox-test/backups/20260804-post-preview-delete-d510afe
/Users/jikime/Dev/Private/cli-relay-backups/20260804-pre-preview-delete-d510afe
/Users/jikime/Dev/Private/cli-relay-backups/20260804-post-preview-delete-d510afe
```
