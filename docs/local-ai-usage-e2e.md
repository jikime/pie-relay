# 로컬 제3자 채팅·Claude 사용량 E2E 가이드

## 목적

운영 Kroot 서비스나 Kroot Main Site를 변경하지 않고, Pie Relay 프로젝트 안에서 다음
전체 경로를 검증한다.

```text
예제 채팅 Browser :4175
  → 예제 BFF
  → Pie Manager :19190
  → Pie Relay
  → 사용자 전용 Docker clientd
  → Claude Agent SDK / Claude Code
  → usage event
  → Pie Manager PostgreSQL 사용량 원장
  → 예제 채팅의 내 사용량 화면
```

사용량의 사용자·프로젝트·대화 귀속은 Executor가 제출하지 않는다. Manager가 인증된
Integration Conversation에서 직접 결정한다. 따라서 브라우저나 컨테이너가 다른 사용자
ID를 넣어 원장을 조회할 수 없다.

## 언제 저장되는가

Claude Agent SDK가 한 사용자 턴의 최종 `result`를 반환한 직후, `done` 이벤트보다 먼저
clientd가 다음 측정값을 전송한다.

- 실제 사용 모델과 canonical model
- 입력·출력 토큰
- 캐시 읽기·캐시 생성 토큰
- 웹 검색 요청 수
- SDK가 보고한 USD 비용
- SDK result ID, Claude session ID, query run ID

Manager는 이벤트 수신 시각을 기준 시각으로 사용하고, 현재 Conversation에서
Integration User·Owner·Project·Request를 붙여 PostgreSQL에 저장한다. 고유 키
`(conversation_id, result_id, model)`로 재연결·재전송을 중복 제거한다.

정상 경로는 즉시 DB에 기록한다. DB가 잠시 실패해도 usage 이벤트를 먼저 fsync된 채팅
journal에 남기고, 백그라운드 reconciler가 다시 적재한다. 백그라운드 Claude 작업은
clientd가 원래 요청 ID를 usage 이벤트에 유지하므로 첫 응답 이후의 사용량도 같은 요청에
귀속할 수 있다.

## 비용 원칙

현재 기본값은 Claude Agent SDK가 result 시점에 보고한 모델별 `costUSD`를 그대로
불변 스냅샷으로 저장한다. `pie_llm_price_versions`에 해당 공급자·canonical model·시점의
가격을 등록하면 Manager가 입력·출력·캐시·웹 검색 단가로 계산한 비용을 대신 저장하고
그 가격 버전 ID를 함께 보존한다. 나중에 가격이 바뀌어도 과거 행은 자동으로 다시 계산하지
않는다.

이 값은 사용량 관찰과 내부 정산 자료다. OAuth/구독형 Claude 실행에서 공급자 청구서와
법적 과금 원장까지 일치해야 한다면, 별도 공급자 청구 내역 대조 절차가 필요하다.

## 데이터베이스

Manager가 다음 전용 정규화 테이블을 자동 생성한다.

| 테이블 | 역할 |
|---|---|
| `pie_llm_usage_events` | 턴·모델별 토큰, 비용, 귀속 및 원본 측정 이벤트 |
| `pie_llm_price_versions` | 모델별 기간 단가와 출처 |

`PIE_USAGE_DATABASE_URL`을 설정하면 그 PostgreSQL을 사용한다. 미설정 시
`PIE_CONTROL_DATABASE_URL`을 재사용한다. 두 값이 모두 없으면 채팅은 계속 동작하지만
사용량 원장과 조회 API는 비활성화된다.

## 로컬 실행

Manager와 Executor 이미지를 현재 소스로 만든 뒤 데모를 시작한다.

```bash
cd /Users/jikime/Dev/Private/cli-relay
docker build -f executor-manager/Dockerfile.manager -t pie-executor-manager:local .

# 로컬 Relay의 시험용 비밀값을 출력하지 않고 같은 프로세스에만 전달한다.
set -a
source deploy/local/.env
set +a
PIE_E2E_RELAY_URL=http://host.docker.internal:13412 \
PIE_E2E_RELAY_NODE_ID=relay-1 \
PIE_E2E_RELAY_POOL_ID=pie-canvas \
RELAY_JWT_SECRET="$RELAY_JWT_SECRET" \
  node scripts/dev/third-party-web-chat-demo.mjs start
unset RELAY_JWT_SECRET
```

위 명령은 `deploy/local/pie-local.sh up`으로 로컬 Relay가 이미 실행 중인 경우의 권장
경로다. 공개 `relay.cookai.dev`를 사용하려면 그 Relay의 실제 node ID, pool ID와 서명키가
모두 일치해야 한다. 운영 서명키를 개발 PC에 복사해 두지 말고, 승인된 비밀 관리 경로로
실행 프로세스에만 주입한다.

데모 시작 스크립트가 다음을 멱등적으로 준비한다.

- `pie-web-chat-demo-postgres`: 외부 포트를 열지 않는 전용 PostgreSQL
- `pie-web-chat-demo-manager`: 사용량 원장이 활성화된 Manager
- 사용자별 Executor와 clientd
- Next.js 예제 채팅 `http://127.0.0.1:4175`

상태는 비밀값을 출력하지 않고 확인한다.

```bash
node scripts/dev/third-party-web-chat-demo.mjs status
curl -fsS http://127.0.0.1:19190/readyz
curl -fsS http://127.0.0.1:4175/api/health
```

## 수동 확인

1. `http://127.0.0.1:4175`에 로그인한다.
2. 프로젝트와 새 대화를 만들고 Claude에 짧은 요청을 보낸다.
3. 응답 스트리밍이 끝난 다음 상단의 `사용량`을 누른다.
4. 최근 7일·30일·90일을 전환하며 전체 토큰, 턴, 비용, 캐시, 모델별·일별 내역을 확인한다.
5. 대시보드 아래 상세 목록에서 실행 시각, 프로젝트, 요청 참조값, 모델·상태,
   입력·출력·캐시·전체 토큰, 당시 비용과 산정 기준을 확인한다.
6. `이전 내역 더 보기`로 커서 기반 다음 페이지를 불러온다.
7. 다른 계정으로 로그인했을 때 첫 사용자의 값이 보이지 않는지 확인한다.
8. 같은 usage 이벤트를 재전송하거나 Manager를 재시작한 뒤 합계가 증가하지 않는지 확인한다.

사용량 API는 브라우저가 Manager를 직접 호출하지 않는다. 로그인된 BFF가 다음 경로를
Integration service token으로 호출한다.

```text
GET /v1/integrations/{integrationId}/users/{externalUserId}/usage/summary?days=30
GET /v1/integrations/{integrationId}/users/{externalUserId}/usage/events?days=30&limit=30&cursor=...
```

BFF는 세션의 `externalUserId`만 사용하며 브라우저가 보낸 사용자 ID를 받지 않는다.
상세 목록은 최신순 keyset pagination을 사용하므로 데이터가 계속 추가되어도 큰 offset을
건너뛰는 방식보다 일정한 조회 성능을 유지한다. 프롬프트, 응답 본문, PAT와 내부 Owner ID는
목록 응답에 포함하지 않는다.

## 자동 검증

```bash
cd /Users/jikime/Dev/Private/cli-relay/client/node-executor
node --test usage-event.test.mjs

cd /Users/jikime/Dev/Private/cli-relay/executor-manager
go test ./internal/usage ./internal/chatgateway ./cmd/manager

cd /Users/jikime/Dev/Private/cli-relay/examples/third-party-web-chat
npm test
npm run build
```

실제 PostgreSQL 테스트는 보호된 시험 DSN을 `PIE_TEST_POSTGRES_DSN`으로 지정하면
중복 제거와 사용자별 합계를 함께 검증한다.

## 운영 전 주의사항

- `PIE_USAGE_DATABASE_URL`은 secret manager 또는 mode 0600 환경 파일로 전달한다.
- DB role은 사용량 테이블의 생성·읽기·쓰기 권한만 갖도록 분리한다.
- 사용량 조회 API는 Integration user binding을 통과해야 하며 owner ID를 쿼리 인자로 받지 않는다.
- `raw_event`에는 PAT나 프롬프트가 들어가지 않는다. 모델 사용량 측정값만 저장한다.
- journal과 PostgreSQL은 디스크 암호화·백업·보존 기간·삭제 정책을 적용한다.
- Managed Executor의 SDK 이벤트만으로 고객 청구를 확정하지 않는다. 악의적 컨테이너 변조를
  과금 수준에서 방지하려면 공급자 API proxy 또는 공급자 청구서 대조가 추가로 필요하다.
