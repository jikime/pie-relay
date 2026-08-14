# Pie 제3자 웹채팅 예제

Pie Desktop/Mobile과 독립적으로 공개 Integration API만 호출하는 참조 애플리케이션이다.
제3자 서비스가 실제로 구현해야 하는 Backend-for-Frontend(BFF) 보안 경계와 사용자별
Docker 채팅 경로를 함께 검증한다.

화면과 BFF는 Next.js 16 App Router, React 19, TypeScript와 Tailwind CSS 4로 구성한다.
프로젝트·언어 선택은 shadcn/ui의 Radix Select, 프로젝트 생성 창은 shadcn/ui Dialog를
사용한다. UI 컴포넌트 소스는 `src/components/ui`에 있으므로 고객사가 자체 디자인
시스템에 맞게 수정할 수 있다. 기존 로그인·프로젝트·채팅 API URL과 응답 계약은
프레임워크 마이그레이션 전과 동일하다.

```text
Browser
  └─ HttpOnly 로그인 세션 ──> 이 예제 Backend
                                └─ Integration service token ──> Pie Manager
                                                                      │
                                                               Pie Relay
                                                                      │
                                                         사용자 Docker clientd
                                                                      │
                                                               Claude Code
```

브라우저는 Integration service token, Relay JWT, 컨테이너 ID, 내부 owner ID를 받지
않는다. Backend는 로그인 세션의 `externalUserId`만 사용하고, 브라우저가 보낸 사용자
ID는 신뢰하지 않는다. 대화 조회·입력·SSE 연결마다 Integration User 바인딩과
Conversation 소유권을 다시 비교한다.

## 준비

1. Pie Admin의 Integration 메뉴에서 `sample-web-chat`을 등록한다.
2. Kroot 연동 예제의 credential 경로는 `.kroot/credential.json`으로 설정한다.
3. 한 번만 표시되는 `pie_int_...` 서비스 토큰을 Backend secret manager에 저장한다.
4. 샘플 사용자의 비밀번호 해시를 만든다.

```sh
cd examples/third-party-web-chat
npm install
npm run hash-password -- 'change-this-demo-password'
```

`users.example.json`을 `var/users.json`으로 복사해 해시, 외부 사용자 ID와 opaque
credential을 채운다. 이 파일은 초기 사용자 seed이자 PostgreSQL을 사용하지 않는 로컬
개발용 저장소다. 사용자 PAT가 있으므로 권한을 제한한다.

```sh
chmod 600 var/users.json
```

환경변수를 설정하고 실행한다.

```sh
export PIE_MANAGER_URL='http://127.0.0.1:19090'
export PIE_INTEGRATION_ID='sample-web-chat'
export PIE_INTEGRATION_TOKEN='pie_int_...'
export PIE_WEB_CHAT_USERS_FILE="$PWD/var/users.json"
export PIE_WEB_CHAT_REGISTRATION_ENABLED=true
export PIE_KROOT_SERVER_URL='grpcs://adk-server.kroot.io'
export PIE_KROOT_RELAY_URL='wss://adk-relay.kroot.io/ws/agent'
npm run build
npm start
```

컨테이너 운영에서는 token을 환경변수에 직접 복사하는 대신 읽기 전용 secret 파일을
마운트하고 다음 변수만 지정할 수 있다. 두 token 변수를 동시에 설정하면 기동을
거부한다.

```sh
export PIE_INTEGRATION_TOKEN_FILE='/run/secrets/pie-integration-token'
unset PIE_INTEGRATION_TOKEN
```

회원가입을 운영형으로 시험할 때는 파일 저장소 대신 PostgreSQL을 사용한다. DBA가 전용
role과 `pie_web_chat` schema를 먼저 만들고, 해당 role에는 대상 DB 연결과 그 schema
사용·객체 생성 권한만 준다. 데이터베이스 전체의 `CREATE` 권한은 필요하지 않다.
DB URL과 32바이트 Base64URL 암호화 키도 mode 0600 secret 파일로 전달한다.

```sh
export PIE_WEB_CHAT_DATABASE_URL_FILE='/run/secrets/pie-web-chat-database-url'
export PIE_WEB_CHAT_CREDENTIAL_KEY_FILE='/run/secrets/pie-web-chat-credential-key'
```

애플리케이션은 `pie_web_chat.users` 테이블을 확인하고 초기 `users.json` 사용자를
멱등적으로 seed한다. 가입 credential은 무작위 IV와 사용자 외부 ID를 AAD로 사용하는
AES-256-GCM으로 암호화되며 평문 PAT를 DB에 저장하지 않는다. 현재 암호화 키 자동
회전·재암호화 기능은 없으므로 키를 잃거나 임의 교체하면 기존 credential을 복구할 수
없다. 운영 secret manager에 백업하고 별도의 key rotation 절차를 마련해야 한다.

개발 중에는 `npm run dev`로 실행한다. `PIE_WEB_CHAT_HOST`와 `PIE_WEB_CHAT_PORT`는
개발·운영 실행 명령 모두에 적용된다. 운영은 `next start` 기반의 장기 실행 프로세스이므로
reverse proxy나 Container App이 종료 신호와 health check를 관리해야 한다.

주요 코드 경계는 다음과 같다.

- `src/app`: Next.js 페이지와 공개 API Route Handler
- `src/components/web-chat-app.tsx`: 로그인·프로젝트·SSE·채팅 React 상태
- `src/components/ui`: 프로젝트 안에서 소유하는 shadcn/ui 컴포넌트
- `src/api-handler.mjs`: 인증·CSRF·소유권·Manager BFF 계약
- `src/proxy.ts`: 요청별 CSP nonce와 브라우저 보안 정책

회원가입을 활성화하면 BFF가 비밀번호를 scrypt로 해시하고 사용자를 선택된 저장소에
먼저 기록한 다음, 서버가 생성한 `externalUserId`와 credential로 Manager provisioning
API를 호출한다. 가입 응답은 사용자 전용 Executor가 생성·할당되어 `ready`가 된 뒤에만
성공한다. 계정 저장 후 Manager 호출만 실패하면 상태를 `failed`로 보존하므로, 사용자는
같은 계정으로 로그인해 `작업공간 준비`를 다시 실행할 수 있다. 중복 요청은 안정된
idempotency key를 사용해 같은 Integration User와 Executor로 수렴한다.

현재 예제의 회원가입은 격리 경로를 검증하려고 합성 `kpat_demo_...` credential을 만든다.
고객 서비스에서는 외부 IdP가 인증한 사용자 식별자와 그 서비스가 발급한 credential을
BFF 서버에서 전달하도록 교체해야 한다. 브라우저가 제출한 external user ID나 PAT는
신뢰하면 안 된다.

로컬에서 실제 Kroot Project 연결까지 시험할 때만 보호된 파일의 PAT를 가입 fixture로
사용할 수 있다. 이 옵션은 여러 가입 사용자에게 같은 Kroot 신원을 부여하므로 운영에서
사용하면 안 된다. 파일은 `0600`이어야 하며 PAT 원문을 브라우저 응답이나 로그에 남기지 않는다.

```sh
export PIE_WEB_CHAT_SIGNUP_KROOT_PAT_FILE='/absolute/private/path/local-signup-kroot-pat'
```

로그인 뒤에는 먼저 `새 프로젝트 만들기`를 눌러 표시 이름과 언어를 선택한다. BFF는
로그인 사용자의 Project API만 호출하고, Manager가 사용자 컨테이너 안의
`/workspace/projects/{opaque-project-id}`에서 다음 명령을 실행한다.

```sh
kroot init . "프로젝트 표시 이름" --non-interactive --locale ko
```

명령은 shell 문자열 결합이 아닌 argv로 실행된다. 성공한 Project만 선택 목록에 나타나며,
새 대화는 선택 Project의 작업 경로에서 Claude Code를 시작한다. 한 사용자의 Project들은
각기 다른 작업 폴더를 쓰지만 같은 사용자 HOME의 `.kroot/credential.json`과 `.claude`
인증을 공유한다. 다른 사용자의 HOME·컨테이너·Project와는 분리된다.

브라우저를 새로 열어 localStorage에 대화 ID가 없더라도 BFF가 로그인 사용자의 활성
대화만 조회해 같은 Project의 최신 대화를 복구한다. `새 대화`를 누르거나 Project를
바꾸면 현재 대화와 Docker session을 먼저 정상 종료한 뒤 다음 대화를 만든다. 따라서
화면에서 다시 접근할 수 없는 session이 쌓여 `maxConversationsPerUser` 한도를 소모하지
않는다. Manager가 Integration User로 한 번, BFF가 현재 로그인 binding으로 다시 한 번
소유권을 제한하므로 다른 사용자의 활성 대화 목록은 반환되지 않는다.

데모가 생성하는 `kpat_demo_...` 값은 파일 형식과 격리 경계를 확인하기 위한 합성
토큰이다. 운영 BFF는 자체 인증 서버에서 발급·검증한 실제 사용자 PAT를 같은
`accessToken` 필드에 넣어야 한다. PAT는 브라우저나 Pie Relay로 보내지 않고,
Manager의 Integration provisioning API를 거쳐 해당 사용자 Home에만 기록한다.

브라우저에서 `http://127.0.0.1:4175`를 연다. Manager의 `PIE_RELAY_URL`이 Azure
Relay origin이면 Azure를 경유하고, 로컬 Relay origin이면 로컬을 경유한다. 웹채팅
설정에는 Relay URL이 없으며 제3자 앱이 내부 Relay 프로토콜에 의존하지 않는다.

로그인 후 상단의 `사용량`을 누르면 최근 7일·30일·90일의 모델별 입력·출력·캐시 토큰,
완료 턴과 SDK 보고 비용을 볼 수 있다. 브라우저는 사용자 ID를 Manager에 전달하지 않고,
BFF가 로그인 세션의 `externalUserId`로만
`/users/{externalUserId}/usage/summary`와 `/users/{externalUserId}/usage/events`를 호출한다. Manager는 다시 Integration User
binding으로 조회 범위를 제한한다. 사용량 원장의 저장 시점·중복 복구·가격 버전 규칙은
[`docs/local-ai-usage-e2e.md`](../../docs/local-ai-usage-e2e.md)에 정리되어 있다.

대시보드 아래 상세 사용 내역은 최근 항목부터 30개씩 조회한다. 실행 시각, 프로젝트,
요청 참조값, 모델·상태, 입력·출력·캐시·전체 토큰, 비용과 산정 기준을 표시하며
`이전 내역 더 보기`는 offset이 아닌 커서 기반 페이지네이션을 사용한다. 프롬프트·응답 본문과
PAT는 사용량 목록에 저장하거나 표시하지 않는다.

공개 production-like Staging은 `https://chat-relay.cookai.dev`에서 실행한다. 이 BFF는
`https://api-relay.cookai.dev`만 호출하고, Manager가 공식 Relay와 사용자 Executor를
선택한다. 따라서 브라우저나 Web Chat 설정에서 Relay 주소를 따로 지정하지 않는다.

메시지 입력창의 `＋` 버튼으로 JPEG, PNG, GIF, WebP 이미지를 최대 4개까지 첨부할
수 있다. 이미지 한 개와 한 요청의 전체 첨부 크기는 각각 4MiB 이하이다. 브라우저는
미리보기를 제공하지만 BFF와 Manager가 Base64 형식, MIME allowlist, 실제 파일
signature, 파일명, 선언 크기를 다시 검증한다. 검증된 이미지는 기존 clientd의
`images: [{data, mimeType}]` 채팅 프로토콜로 Claude Code에 전달된다. 현재 지원 범위는
이미지이며 PDF나 임의 바이너리 파일은 이 채팅 API가 받지 않는다.

재연결 복구를 위해 Manager의 private chat journal에는 진행 중 요청의 이미지가 잠시
저장될 수 있다. 브라우저에 제공하는 accepted event와 SSE에서는 Base64 원문을 제거하고
파일명·MIME·크기 요약만 반환하며, 대화 삭제 시 journal도 함께 제거한다. 운영에서는
journal 저장소의 디스크 암호화와 보존 기간 정책을 적용해야 한다.

## 인증 경계

- 데모 비밀번호는 scrypt로 검증한다.
- 로그인은 난수 HttpOnly, SameSite=Strict 세션 쿠키를 사용한다.
- 상태 변경 API는 origin과 CSRF 토큰을 검증한다.
- 반복 로그인 실패를 IP 단위로 제한한다.
- Integration 토큰과 사용자 credential은 서버 설정에만 둔다.
- 사용자 A가 사용자 B의 대화 ID를 입력해도 BFF 소유권 검사에서 `404`로 차단한다.

사용자 저장소를 PostgreSQL로 바꿔도 로그인 세션 저장소는 단일 프로세스 메모리
구현이다. 현재 한 개 BFF 인스턴스로 하는 production-like 시험에는 맞지만, 실제 제3자
서비스를 수평 확장하려면 기존 IdP/OIDC 로그인과 Redis 또는 데이터베이스 세션으로
교체해야 한다.
HTTPS reverse proxy 뒤에서는 `PIE_WEB_CHAT_SECURE_COOKIE=true`와
`PIE_WEB_CHAT_PUBLIC_ORIGIN=https://...`를 반드시 설정한다.

## 검증

```sh
npm run check
npm test
npm run build
npm audit --audit-level=high
```

테스트는 회원가입, 로그인, CSRF, 서비스 토큰 비노출, 서버 확정 사용자 ID, Project
생성·목록·선택과 A/B Project·대화 격리, 이미지 위장·크기 제한, 메시지 제출과 SSE
프록시, PostgreSQL credential 암호화와 프로세스 재시작 후 사용자 지속성을 확인한다.
PostgreSQL 통합 테스트는 `PIE_TEST_POSTGRES_URL`이 있을 때 실행한다. 실제 Docker/Relay 경로는 저장소 루트의
`scripts/e2e/third-party-web-chat.mjs`로 검증한다.

공개 Staging의 실제 Chrome에서 대화 복구·교체·정리를 확인하려면 보호된 로그인 파일을
표준입력으로만 전달한다. 시험은 첫 대화를 만든 뒤 `새 대화`를 눌러 기존 session이
종료되고 활성 대화가 정확히 한 개만 남는지 확인하며, 종료 시 생성 대화도 정리한다.

```sh
ssh pie-sandbox-test \
  'cat /home/kaonkroot/pie-sandbox-test/web-chat/login.json' \
  | PIE_WEB_CHAT_SMOKE_URL=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SMOKE_ORIGIN=https://chat-relay.cookai.dev \
    PIE_WEB_CHAT_SMOKE_LOGIN_FILE=/dev/stdin \
    npm run smoke:remote-browser-lifecycle
```

E2E는 실행 중인 데모의 `.next` 산출물을 덮어쓰지 않도록 별도의 `.next-e2e`
빌드 디렉터리를 만들고 종료 시 제거한다. `PIE_E2E_BROWSER_SMOKE=true`를
사용하면 실제 shadcn Select와 Dialog, 첨부 미리보기, 승인·거절 버튼까지 Chrome에서
검증한다.

로컬 Relay E2E는 Relay URL과 서명키를 같은 로컬 환경에서 가져와 실행한다.

```sh
set -a
source deploy/local/.env
set +a
PIE_E2E_RELAY_URL=http://host.docker.internal:13412 \
PIE_E2E_EXECUTOR_IMAGE=pie-relay-client-kroot-e2e:local \
node scripts/e2e/third-party-web-chat.mjs
```

Azure URL을 쓸 때는 `RELAY_JWT_SECRET`도 해당 Azure Relay의 실제 배포값이어야 한다.
URL만 Azure로 바꾸고 로컬 서명키를 사용하면 Project 초기화 후 Relay Conversation 연결이
실패한다.

저장소의 실사용 데모를 Azure Relay로 시작할 때는 secret을 출력하지 않고 다음처럼 바로
프로세스 환경에 전달한다.

```sh
azure_relay_signing_secret="$(az containerapp secret list \
  --resource-group rg-jikime \
  --name pie-relay-test \
  --show-values \
  --query "[?name=='relay-jwt-secret'].value | [0]" \
  --output tsv)"
RELAY_JWT_SECRET="$azure_relay_signing_secret" \
  node scripts/dev/third-party-web-chat-demo.mjs start
unset azure_relay_signing_secret
```

시작 스크립트는 현재 이미지 ID와 실행 컨테이너의 이미지 ID를 비교해 구버전
Manager/Executor만 재생성한다. 사용자별 workspace와 HOME은 호스트 볼륨에 있으므로
이미지 교체 뒤에도 유지되며, 활성 Conversation의 Docker Session은 자동 복구된다.
로컬 실사용 데모에 한해서는 시작할 때 macOS Keychain의 최신 Claude Code OAuth 파일을
기존 데모 사용자 HOME에도 다시 기록한다. Project 파일과 `.kroot/credential.json`은
변경하지 않는다. 운영 다중 사용자 서비스는 개인용 OAuth 복사를 장기 인증 배포 방식으로
사용하지 말고, 공급자 정책에 맞는 조직 계정·API credential broker와 회전 절차를 둬야 한다.

이미 실행 중인 로컬 실사용 데모에서 완전히 새로운 사용자로 가입부터 실제 Claude
응답까지 다시 검증하려면 다음 명령을 사용한다. 이 검증은 Manager operation으로
컨테이너를 재생성한 뒤 `.kroot`·`.claude` 보존과 두 번째 Azure Relay/Claude 응답까지
확인한다. 생성한 테스트 사용자와 컨테이너는 직접 로그인해 확인할 수 있도록 유지된다.

```sh
node scripts/e2e/live-web-chat-signup.mjs
```

로컬 전체 스택에서는 이 검증이 `./deploy/local/pie-local.sh test`에 포함된다. Azure
Relay 검증은 Azure의 `RELAY_JWT_SECRET`을 화면에 출력하지 않고 환경변수로 주입한 뒤
`PIE_E2E_RELAY_URL`을 Azure HTTPS origin으로 설정한다. `PIE_E2E_BROWSER_SMOKE=true`를
추가하면 Headless Chrome이 실제 회원가입, 신규 컨테이너 할당, 승인 버튼과 채팅
화면, 이미지 선택·미리보기·clientd 바이트 수신까지 검증한다.

## 프로젝트 코드 편집기

로그인 후 프로젝트와 준비된 대화를 선택하면 상단의 **코드** 화면에서 컨테이너의
프로젝트 파일을 열고 수정할 수 있다. 편집기는 Monaco를 로컬 번들로 사용하며
`Cmd/Ctrl+S`로 저장한다. Claude Code 또는 다른 브라우저가 먼저 파일을 변경한 경우
409 충돌을 표시하므로, 최신 파일을 다시 불러와 변경 내용을 합친 뒤 저장한다.

인증 파일과 비밀정보는 편집기에서 제공하지 않는다. `.claude`, `.kroot`, `.pie`,
`.env*`, `.ssh`, `.aws`는 목록과 직접 요청 모두 차단된다. 전체 구조와 보안 규칙은
[`../../docs/remote-workspace-editor.md`](../../docs/remote-workspace-editor.md)를 참고한다.
