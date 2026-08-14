# 제3자 애플리케이션용 사용자 전용 AI 채팅 구현 명세

## 1. 목표

제3자 서비스에서 사용자가 가입하면 Pie Manager가 그 사용자만 소유하는 Docker
Executor를 준비한다. 제3자 애플리케이션은 Pie의 공개 Chat API를 호출하고, Pie는
Relay를 통해 해당 컨테이너의 `clientd`로 요청을 전달한다. `clientd`는 이미 구현된
Claude Agent SDK 실행기를 사용하고 구조화된 응답 이벤트를 반환한다.

```text
제3자 Backend
  ├─ 사용자 가입 ──> Pie Provisioning API
  └─ 채팅 요청 ────> Pie Chat API/SSE
                         │
                    Pie Manager
                  (소유권·수명주기)
                         │
                      Pie Relay
                         │
              사용자 전용 Docker clientd
                         │
                  Claude Agent SDK
```

Pie는 제3자 credential로 외부 사용자를 introspection하지 않는다. 컨테이너 내부
프로그램이나 hook이 credential을 읽어 제3자 외부 API와 통신한다. Pie의 책임은
제3자 Backend 자체를 인증하고, 인증정보를 올바른 사용자 전용 volume에 안전하게
저장하며, 요청을 그 사용자의 컨테이너에만 라우팅하는 데까지다.

## 2. 강제 보안 경계

- Managed Docker Executor는 소유자 전용이다.
- 다른 사용자, 공유 grant, 관리자용 일반 채팅 세션으로 소유자가 다른 컨테이너에
  접속할 수 없다.
- 다중 사용자 viewer/controller는 명시적으로 공유된 `Host OS + Direct` 세션에만
  허용한다.
- 제3자 credential, Pie workload credential, Claude provider credential을 분리한다.
- 제3자 PAT 또는 credential을 Relay frame, Relay JWT, QR, Docker label, 명령행,
  일반 로그에 포함하지 않는다.
- 제3자 애플리케이션은 Relay 내부 프로토콜에 직접 의존하지 않고 버전이 있는 Pie
  Chat API만 사용한다.

## 3. 중앙 데이터 모델

### Integration

제3자 서비스 도입 시 Pie 운영자가 한 번 등록한다.

```json
{
  "id": "partner-a",
  "displayName": "Partner A",
  "status": "active",
  "maxUsers": 10000,
  "maxProjectsPerUser": 32,
  "maxConversationsPerUser": 4,
  "credential": {
    "targetPath": ".partner-a/credential.json",
    "format": "json",
    "maxBytes": 65536
  }
}
```

Integration에는 사용자 credential 원문을 저장하지 않는다. 대상 경로는 사용자 HOME
기준 상대 경로만 허용하고 절대 경로, `..`, symlink escape를 거부한다. 서비스 토큰은
한 번만 표시하고 중앙 저장소에는 SHA-256 digest만 보존한다.
Integration별 사용자 수, 사용자별 Project 수와 활성 Conversation 수를 Control Plane에서
원자적으로 제한한다. API의 사전 검사는 사용자 친화적 오류를 위한 것이며 최종 quota
판정은 저장 직전 Control Plane이 담당한다. 사용자별 Project 기본 상한은 32개다.

### Integration User

```text
(integration_id, external_user_id)
        -> owner_user_id
        -> executor_id
        -> container_id
```

`owner_user_id`는 외부 ID를 Docker 이름에 직접 사용하지 않고 안정적인 digest 기반
내부 ID로 생성한다. 같은 가입 요청은 같은 사용자를 반환하며, 다른 Integration의
같은 external ID와 충돌하지 않는다.

### Project

```text
project_id
  -> integration_user_id
  -> owner_user_id
  -> /workspace/projects/{project_id}
  -> kroot init 완료 상태
```

사용자 전용 컨테이너 하나 안에 여러 Project를 만들 수 있다. Project ID는 표시 이름을
경로에 넣지 않고 Integration, 사용자 바인딩, `Idempotency-Key`로부터 만든 opaque ID다.
Manager는 컨테이너 안에서 shell 없이 다음 argv를 실행한다.

```text
kroot init . "<표시 이름>" --non-interactive --locale <ko|en|ja|zh>
```

실행 위치는 `/workspace/projects/{project_id}`이며 성공 marker가 있어야 `ready`가 된다.
동일 멱등키 재호출은 이미 준비된 Project를 그대로 반환하고, 중간 실패 상태는 같은 키로
재시도할 수 있다. Project별 작업 폴더는 분리되지만 컨테이너 HOME의 `.kroot`와 `.claude`
인증은 해당 사용자 안에서 공유된다. 다른 사용자의 컨테이너·Project와는 공유하지 않는다.

### Conversation

```text
conversation_id
  -> integration_id
  -> owner_user_id
  -> project_id / project working directory
  -> docker device_id
  -> control session_id
  -> relay room
  -> clientd chat session
```

Conversation은 반드시 Integration User가 소유한 `ready` Project와 같은 Executor만
선택한다. clientd가 Claude Agent SDK를 시작할 때 Project의 작업 경로를
`CLI_RELAY_DEFAULT_CWD`로 전달하므로 파일 읽기·수정과 Kroot 문맥은 선택한 Project에
고정된다.

## 4. 자격증명 분리

| 자격증명 | 발급자·저장 위치 | 용도 |
|---|---|---|
| Integration service token | Pie, 제3자 Backend | Provisioning/Chat API 호출 |
| Pie workload credential | Pie, 사용자 컨테이너 | `clientd`의 Manager/Relay 인증 |
| Third-party user credential | 제3자, 사용자 credential volume | 컨테이너 프로그램의 외부 API 호출 |
| Claude provider credential | 별도 Claude 설정 | Claude Agent SDK 실행 |

토큰 문자열만 등록해서는 발급자와 사용자 신원을 알 수 없다. 각 토큰은 다음 절차로
발급자 문맥을 확정한다.

- Pie Admin token: Pie 운영자 IdP의 introspection endpoint가 `active`, `sub`,
  `roles/scope`를 반환해야 한다. `pie:admin:view` 이상의 권한이 있어야 Admin에 들어간다.
- Integration service token: 운영자가 Integration을 등록할 때 Pie가 발급한다.
  `/v1/integrations/{integrationId}/...`의 경로 ID와 저장된 token digest를 함께 검증한다.
- Third-party user credential/PAT: 인증된 Integration Backend가 사용자 provisioning body로
  전달한다. Pie Admin 로그인에는 사용하지 않고 해당 Integration User 전용 컨테이너에만
  materialize한다.

따라서 서로 다른 제3자 서비스의 PAT를 Pie가 자동 판별하거나 공통 Admin token으로
받지 않는다. 여러 운영자 IdP가 필요하면 issuer별 introspection 설정 또는 Pie 자체
운영자 IdP 앞단에서 통합 검증해야 한다.

제3자 credential 파일은 Docker host의 사용자별 state root 아래에 원자적으로 쓴다.
파일은 `0600`, 디렉터리는 `0700`으로 만들고 Executor UID/GID가 소유한다. Manager는
root-bounded filesystem API를 사용해 경로 이탈과 symlink 공격을 차단한다.

credential 교체는 컨테이너 재생성 없이 같은 경로를 원자 교체한다. API와 감사 로그는
값을 반환하지 않고 `configured`, `version`, `updatedAt`, `digest`만 노출한다.

## 5. 공개 API v1

### 운영자 API

```text
POST   /v1/admin/integrations
GET    /v1/admin/integrations
GET    /v1/admin/integrations/{integrationId}
PATCH  /v1/admin/integrations/{integrationId}
POST   /v1/admin/integrations/{integrationId}/rotate-token
POST   /v1/admin/integrations/{integrationId}/revoke
```

### 제3자 Provisioning API

```text
PUT    /v1/integrations/{integrationId}/users/{externalUserId}
GET    /v1/integrations/{integrationId}/users/{externalUserId}
DELETE /v1/integrations/{integrationId}/users/{externalUserId}
PUT    /v1/integrations/{integrationId}/users/{externalUserId}/credential
GET    /v1/integrations/{integrationId}/users/{externalUserId}/projects
POST   /v1/integrations/{integrationId}/users/{externalUserId}/projects
GET    /v1/integrations/{integrationId}/users/{externalUserId}/projects/{projectId}
```

가입 요청에는 `Idempotency-Key`를 요구한다. 응답은 `provisioning`, `ready`, `failed`,
`suspended`, `deleting` 중 하나의 상태를 반환한다. credential은 JSON 값 전체를 opaque
payload로 보존하며 Pie가 제3자 고유 필드를 해석하지 않는다.

Project 생성도 `Idempotency-Key`를 요구하며 body는 `name`과 선택적 `locale`을 받는다.
응답이 `ready`일 때만 Conversation을 만들 수 있다. Project 표시 이름은 경로·명령문으로
해석하지 않으며, `kroot`에는 shell을 거치지 않은 별도 argv로 전달한다.

### 제3자 Chat API

```text
POST   /v1/integrations/{integrationId}/users/{externalUserId}/conversations
GET    /v1/integrations/{integrationId}/conversations/{conversationId}
POST   /v1/integrations/{integrationId}/conversations/{conversationId}/retry
POST   /v1/integrations/{integrationId}/conversations/{conversationId}/messages
GET    /v1/integrations/{integrationId}/conversations/{conversationId}/events
POST   /v1/integrations/{integrationId}/conversations/{conversationId}/cancel
POST   /v1/integrations/{integrationId}/conversations/{conversationId}/permissions/{requestId}
DELETE /v1/integrations/{integrationId}/conversations/{conversationId}
```

Conversation 생성 body에는 소유한 `ready` Project의 `projectId`가 반드시 필요하다.
Manager와 예제 BFF가 각각 Project 소유권을 확인하며, 브라우저가 내부 owner ID나
작업 경로를 지정할 수는 없다.

`events`는 기본적으로 SSE로 제공하고, 복구·상태 확인용 bounded polling은
`?stream=false&after=<sequence>&limit=<n>`으로 제공한다. 모든 이벤트에는 단조 증가하는 `sequence`를 넣고
`Last-Event-ID`로 재접속할 수 있게 한다. 동일 message의 `Idempotency-Key` 재호출은
새 요청을 만들지 않는다. 승인된 chat frame은 `request.accepted`로 먼저 fsync하고,
terminal event를 받은 뒤 `request.completed`를 기록한다. Manager가 그 사이 재시작하면
완료되지 않은 frame을 복구해 다시 전달하므로 전달 의미는 **at-least-once**다. Claude
실행 중 컨테이너 자체까지 동시에 장애가 난 극단 상황에는 재실행될 수 있으므로 외부
API에 부수 효과를 만드는 컨테이너 프로그램/hook도 Pie `requestId` 또는 자체 작업 ID로
멱등 처리해야 한다. 느린 소비자는 bounded buffer 정책을 적용하고, 영구적인 대화
기록은 제3자 서비스 또는 별도 저장 정책이 책임진다.

clientd가 살아 있는 동안에는 Relay 브리지가 최근 64개 `requestId`의 응답을 요청당
최대 16MiB, 완료 cache 전체 32MiB까지 메모리에 보존한다. 실행 중인 ID의 재전송은 Claude를 다시 호출하지
않고 지금까지의 event만 재생하며, 완료 ID는 보존된 terminal event까지 재생한다.
따라서 Manager/Relay만 재시작되는 정상적인 장애 복구에서는 중복 실행을 막는다.
clientd/컨테이너까지 함께 재시작된 미완료 turn은 위 at-least-once 경계를 적용한다.

`permission_request`를 받은 Backend는 동일 conversation의 `permissions/{requestId}`에
허용 여부를 회신한다. Integration service token은 Backend에서만 보관하며 브라우저나
모바일 클라이언트에 전달하지 않는다.

## 6. 내부 연결

1. Conversation 생성 시 Manager가 owner가 같은 `ready` Project와 Docker device를 확인한다.
2. `executionTarget=docker`, `accessMode=private`, `agentMode=chat`, `projectId`인 Control Session을
   생성한다.
3. Controller가 Project의 서버 확정 작업 경로를 해석하고 session-scoped host capability를 발급한다.
4. Docker Session Manager가 그 경로에서 `chatagent`를 시작하고 `clientd + Claude Agent SDK`를
   Relay host leg에 연결한다.
5. Pie Chat Gateway가 owner participant capability로 Relay participant leg에 연결한다.
6. Chat API 입력은 Gateway가 `type=chat` frame으로 전송한다.
7. `clientd`의 구조화 이벤트를 Gateway가 sequence journal에 기록하고 SSE로 전달한다.

Chat Gateway는 제3자에게 Relay JWT를 노출하지 않는다. Relay 재시작이나 네트워크
단절 시 내부적으로 capability를 재발급하고 같은 conversation에 재접속한다.

## 7. 상태·복구 계약

- 가입/credential 교체/Project/Conversation 생성은 멱등해야 한다.
- Manager 재시작 후 `provisioning`과 `starting` 상태를 다시 조정한다.
- Executor 재시작·이미지 교체로 활성 Docker Session이 사라지면 `starting`으로 되돌리고,
  활성 Conversation에 속한 실패 Session은 최대 30초 backoff로 자동 재시작한다.
- clientd heartbeat가 끊기면 Conversation을 즉시 삭제하지 않고 `reconnecting`으로
  전환한다.
- Relay 재연결 전 입력은 bounded queue에 보관하며, 상한 초과 시 `429/503`으로
  backpressure를 반환한다.
- API가 승인한 미완료 chat은 append-only journal에서 복구해 Manager/Relay 재연결 뒤
  재전송한다. 완료 marker는 journal 용량에 도달해도 기록해 turn lock이 남지 않게 한다.
- message별로 활성 turn은 하나만 허용한다. 다른 Conversation은 사용자 quota 안에서
  허용하지만 동일 Project에 여러 Conversation을 열면 같은 파일을 동시에 수정할 수 있다.
  운영 BFF는 Project별 쓰기 turn 직렬화 또는 충돌 정책을 적용해야 한다.
- 사용자 정지·탈퇴 시 신규 메시지를 차단하고 기존 Conversation을 종료한다.
- 사용자 정지·탈퇴 또는 Conversation 삭제 시 컨테이너·credential뿐 아니라 해당
  Conversation event journal도 삭제한다. Control Plane tombstone과 credential 원문을
  포함하지 않는 감사 정보만 남긴다.
- container/device owner와 Integration User owner가 다르면 모든 계층에서 `403`으로
  실패한다.

## 8. 관리 페이지

Pie Admin에 다음 정보를 추가한다.

- Integration 등록·상태·서비스 토큰 교체
- Integration User와 내부 owner/container 매핑
- 사용자별 Project, 초기화 상태, 서버 확정 작업 경로와 오류 사유
- credential 값이 아닌 등록 여부·버전·갱신 시각
- Conversation 상태와 Relay/clientd 연결 상태
- 프로비저닝 실패 재시도·정지·삭제
- 감사 이벤트와 오류 사유

운영자도 일반 Chat API를 이용해 다른 사용자의 컨테이너에 접속할 수 없다. 운영 작업은
컨테이너 상태 확인·재시작·폐기만 허용하고 대화 참여 권한과 분리한다.

## 9. E2E 완료 조건

1. Integration 등록 시 service token이 한 번만 반환되고 digest만 저장된다.
2. 동일 가입 이벤트를 반복해도 컨테이너가 하나만 생성된다.
3. 두 Integration의 같은 external ID가 서로 다른 owner/container로 분리된다.
4. credential 파일이 정확한 상대 경로·내용·`0600`·Executor 소유자로 생성된다.
5. credential 교체 중 부분 파일이 노출되지 않고 PAT가 로그/API 응답에 나타나지 않는다.
6. A 사용자의 Project가 opaque 전용 경로에서 실제 `kroot init`되고, Chat 요청이 그
   경로의 clientd와 Claude Agent SDK까지 왕복한다.
7. B의 service token으로 A의 user/conversation/container에 접근하면 거부된다.
8. Relay 재시작 뒤 clientd와 Gateway가 재연결하고 후속 메시지가 성공한다.
9. Manager 재시작 뒤 Integration/User/Conversation 소유권이 보존되고, 실행 중 turn이
   복구되며 살아 있는 clientd에서 중복 실행되지 않는다.
10. 사용자 삭제 시 session/container/credential이 정리되고 재접속이 거부된다.
11. Project·Conversation·queue·SSE·컨테이너 quota 초과 시 예측 가능한 backpressure가 반환된다.
12. Go race test와 실제 Docker E2E가 모두 통과한다.

운영 지표는 `/metrics`에서 Integration/User/Conversation 상태별 수, Chat Gateway peer,
SSE subscriber, queue depth, 활성 turn을 제공한다. 경보는 `reconnecting` 장기 지속,
queue depth 상승, `429` 증가, provision 실패를 기준으로 구성한다.

## 10. 구현 순서

1. 중앙 Integration/User/Project/Conversation 모델과 저장소
2. 안전한 credential materializer
3. 운영자 Integration API와 제3자 서비스 인증
4. 사용자 Provisioning API와 기존 Executor Manager 연결
5. Project 생성 API와 컨테이너 `kroot init`
6. Docker Session Manager의 Project cwd 기반 `agentMode=chat`
7. 내부 Chat Gateway와 SSE journal
8. 관리 페이지와 웹 Project 생성·선택 UI
9. 단위·통합·실제 Docker/Relay E2E

위 순서는 현재 코드에 모두 반영되어 있으며 `scripts/e2e/third-party-chat.mjs`가 항목
1~10과 backpressure의 핵심 경로를 실제 Docker/Relay로 검증한다. 이 문서가 제3자 AI
채팅 경로의 기준 계약이다. 기존 generic job API와 Desktop/Mobile 터미널 세션은
호환성을 유지하지만, 제3자 애플리케이션은 이 API만 사용한다.

## 11. 독립 웹채팅 참조 구현

`examples/third-party-web-chat`은 Pie 내부 패키지를 import하지 않고 위 공개 API만
사용하는 독립 Next.js 16 BFF + Web UI다. App Router, React 19, TypeScript,
Tailwind CSS 4와 shadcn/ui를 사용하며 기존 HTTP/SSE 계약은 그대로 유지한다. 프로젝트와
언어 선택은 shadcn Select, 프로젝트 생성은 shadcn Dialog로 구현되어 고객사가 컴포넌트
소스를 직접 확장할 수 있다. 정확한 호출 경로는 다음과 같다.

Web UI는 `text`만 이어 붙이지 않고 `thinking`, `task_started`, `task_progress`,
`task_complete`, `tool_call`, `tool_result`를 하나의 실시간 작업 카드로 정리한다. 내부
thinking 원문과 도구 입출력 원문은 화면에 노출하지 않고, 분석·하위 작업·파일/명령 실행
상태만 사용자에게 보여 준다. 답변 text delta는 animation frame 단위로 묶어 긴 응답에서도
렌더링 횟수가 폭증하지 않게 한다. SSE heartbeat와 `Last-Event-ID`/`after` 복구 지점을
함께 사용하므로 연결이 교체되어도 이미 표시한 이벤트를 중복 렌더링하지 않는다.

```text
Browser
  -> 제3자 Web Backend/BFF
  -> Pie Manager Chat API
  -> Pie Relay(Azure 또는 로컬)
  -> 사용자 전용 Docker clientd
  -> Claude Code
```

브라우저가 Relay를 먼저 호출하는 구조가 아니다. Integration service token은 BFF에만
있고 브라우저는 HttpOnly 로그인 세션과 CSRF token만 사용한다. BFF는 로그인 계정에서
`externalUserId`를 결정하며 요청 body의 사용자/컨테이너 ID를 신뢰하지 않는다. 같은
Integration 안의 다른 사용자가 대화 ID를 알아낸 경우도 각 요청에서 Integration User
바인딩을 비교해 차단한다.

`scripts/e2e/third-party-web-chat.mjs`는 다음 경계를 실제 프로세스로 검증한다.

- Alice/Bob 비밀번호 로그인, HttpOnly 세션과 CSRF
- 서로 다른 owner, Docker 컨테이너, credential 파일
- A가 B의 Conversation을 조회하거나 입력하는 요청 거부
- Manager와 두 clientd가 선택한 Pie Relay(Azure 또는 로컬)에 outbound 연결
- Web Backend의 메시지 제출과 SSE 응답
- 이미지 첨부의 BFF/Manager 이중 검증, 공개 event의 Base64 마스킹
- PNG 원본 바이트가 Pie Relay를 거쳐 Docker clientd에 동일하게 도달하는지 확인
- 선택적으로 Headless Chrome의 실제 로그인·입력·화면 응답
- shadcn Select·Dialog, 프로젝트 선택 상태와 CSP nonce가 적용된 Next.js 화면
- 선택적으로 실제 `pie-relay-client:latest`의 Claude Agent SDK 응답

결정적인 회귀 검증은 테스트 Agent 이미지를 사용하고, Claude provider credential을
안전하게 준비할 수 있는 개발 환경에서만 실제 Claude smoke를 추가한다. macOS
Keychain credential을 사용하는 smoke는 일회성 테스트 state에만 materialize하고 이미지
layer나 Git에는 저장하지 않는다.

### 2026-07-27 실제 검증 결과

다음 세 경로를 각각 실행해 성공했다.

1. 로컬 Relay + 테스트 Agent: 로그인, A/B 컨테이너 격리, SSE 왕복
2. Azure Relay + 테스트 Agent + Headless Chrome: 실제 화면 로그인·메시지·응답
3. Azure Relay + `pie-relay-client:latest` + 실제 Claude Code + Headless Chrome:
   `PIE_AZURE_BROWSER_CLAUDE_OK` 응답의 화면 표시

2026-07-28에는 같은 Azure 경로에서 PNG 첨부를 추가 검증했다. 결정적 E2E는 테스트
Agent가 수신한 파일의 SHA-256을 응답해 브라우저 원본과 일치함을 확인했고, 실사용
E2E는 `pie-relay-client-kroot:local`의 Claude Agent SDK가 실제 PNG를 받아 응답함을
확인했다. 채팅 첨부 계약은 JPEG/PNG/GIF/WebP 최대 4개, 파일당·전체 4MiB이다.

같은 날 Project 기능은 실제 Kroot ADK 바이너리를 Linux 이미지로 다시 빌드한 뒤 로컬
Relay에서 검증했다. Alice/Bob의 Project가 서로 다른 opaque 경로에 생성되고, 실제
`kroot init` 완료 후 각 Conversation의 cwd가 선택 Project에 고정되며 이미지 첨부와 SSE
응답까지 해당 작업 경로에서 왕복하는 것을 확인했다. Azure Relay E2E를 실행할 때는
Manager의 `PIE_RELAY_JWT_SECRET`이 Azure Relay 배포값과 반드시 같아야 한다. 로컬 키와
Azure URL을 섞으면 Project는 생성되어도 Relay 세션은 준비되지 않는다.

이후 웹 예제를 Next.js 16.2.12, React 19.2, Tailwind CSS 4와 shadcn/ui로 마이그레이션했다.
기존 DOM 식별자와 API 계약을 유지한 상태에서 Headless Chrome이 회원가입, 신규 Project
생성, shadcn Select 선택, 이미지 첨부, 권한 승인·거절과 Azure Relay 응답을 모두 통과했다.
Next.js 요청별 CSP nonce도 실제 응답의 정책과 script nonce가 일치하는지 확인했으며,
E2E 빌드는 실행 중인 데모의 CSS/JS 산출물을 덮어쓰지 않도록 `.next-e2e`로 격리했다.

세 번째 실행의 실제 경로는 다음과 같았다.

```text
Headless Chrome Web UI
  -> 샘플 BFF
  -> 로컬 Pie Manager
  -> Azure Container Apps Pie Relay
  -> 로컬 사용자 전용 Docker clientd
  -> Claude Agent SDK / Claude Code
  -> 동일 경로의 SSE 응답
```

테스트 종료 후 E2E Manager, Alice/Bob Executor, Chrome profile, 복사된 Claude
credential state가 모두 제거됐으며 Azure Relay readiness는 계속 정상임을 확인했다.
