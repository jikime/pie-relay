# Pie 프로젝트 웹 프리뷰 설계·운영 가이드

Kroot 서버의 실제 인증서 적용, 실행·삭제·장애 확인과 롤백 절차는
[`preview-ssl-runtime-runbook.md`](./preview-ssl-runtime-runbook.md)를 따른다.

> 기준일: 2026-08-04
>
> 구현 상태: 로컬·Kroot 운영 경로 Docker E2E 및 공인 와일드카드 TLS 검증 완료
> 운영 도메인: `preview.kroot.io`, `*.preview.kroot.io`

이 문서는 사용자 전용 Executor 안의 웹 프로젝트를 임시 HTTPS 주소로 공개하는
구조와 API, 보안 경계, 운영 절차를 설명한다. 프리뷰는 채팅·터미널 기능을 대체하지
않고, Claude Code가 만든 웹 애플리케이션의 실행 결과를 브라우저에서 확인하는 별도
기능이다.

## 1. 확정된 실행 단위

프리뷰 하나마다 Docker 컨테이너를 새로 만들지 않는다. 사용자 한 명에게 할당된 전용
Executor 컨테이너 안에서 프로젝트별 개발 서버 프로세스를 실행한다.

```text
사용자 A의 Executor 컨테이너
├─ project-1
│  └─ preview process :20000
├─ project-2
│  └─ preview process :20001
└─ clientd preview supervisor

사용자 B의 Executor 컨테이너
└─ 사용자 A와 다른 전용 Docker network
```

현재 기본 한도는 Integration 사용자 한 명당 동시 프리뷰 4개다. 컨테이너 안에서
사용하는 포트는 `20000~29999` 범위에서 Manager가 할당하며 API 사용자가 직접 지정할
수 없다. Executor의 포트는 Host에 publish하지 않는다.

프로젝트 하나에 웹 앱이 여러 개 있을 수 있으므로 프리뷰의 실행 단위는
`프로젝트 ID + 앱 경로(appPath)`다. 예를 들어 프로젝트 루트가
`/workspace/projects/project-abc`이고 `appPath`가 `company-landing`이면 실제 실행
경로는 `/workspace/projects/project-abc/company-landing`이다. 이 경로는 내부 식별값이며
사용자가 직접 입력하지 않는다. clientd가 실행 가능한 앱 후보를 제한적으로 찾아
Manager에 상대경로만 전달하고, 후보가 하나면 자동 선택하며 여러 개면 앱 이름으로
선택하게 한다. 탐지 순서로 임의의 앱을 실행하지 않는다.

## 2. 요청 경로

```text
Browser
  │ https://p-{128-bit-random}.preview.kroot.io
  ▼
공용 Traefik
  │ HostRegexp: *.preview.kroot.io
  ▼
Pie Preview Gateway
  │ ① Manager에서 hostname 소유권·상태 조회
  │ ② 비공개 프리뷰 쿠키 검증
  │ ③ 신뢰된 backend alias와 port로 proxy
  ▼
사용자별 internal Docker network
  ▼
사용자 Executor의 preview process
```

Preview Gateway만 여러 사용자 네트워크에 연결된다. Executor끼리는 네트워크를
공유하지 않으며 Gateway에는 Docker socket을 제공하지 않는다. 프록시 대상 주소는
Manager가 생성한 Docker alias만 허용하고 브라우저나 Integration이 임의 IP·URL을
전달할 수 없다.

## 3. 수명주기

1. 제3자 서비스가 사용자를 프로비저닝한다.
2. 사용자가 프로젝트를 만들면 Executor 안에서 `kroot init`이 완료된다.
3. clientd가 프로젝트 안의 실행 가능한 웹 앱을 탐지한다. 하나면 자동 선택하고 여러
   개면 사용자가 앱 이름으로 선택하며, Manager가 선택값을 Project 레코드에 저장한다.
4. 사용자가 프리뷰를 생성하면 Manager가 저장된 앱 선택과 hostname·port·TTL을 사용한다.
5. Executor가 유휴 회수된 상태라면 Manager가 먼저 같은 사용자 Executor를 다시
   생성하고, 사용자 전용 internal network에 Executor와 Preview Gateway를 연결한다.
6. clientd가 프로젝트 프로필에 맞는 개발 서버를 자식 process group으로 실행한다.
7. TCP readiness probe가 성공하면 프리뷰 상태가 `ready`가 된다.
8. TTL 만료, 명시적 중지 또는 사용자 정지 시 process group을 종료한다.
9. 사용자를 정지하면 먼저 binding을 `deleting`으로 바꿔 라우팅을 차단하고, 이후
   세션·프리뷰·컨테이너·전용 네트워크를 정리한다.

상태는 `starting → ready → stopping → stopped`이며 실제 프로세스가 오류로 종료하면
`failed`가 된다. Docker와 clientd의 일시 장애는 지수 간격으로 최대 8회 재시도한다.
Manager가 재시작되어도 PostgreSQL의 프리뷰 레코드를 읽어 실행 상태를 복구한다.

생성·중지·재시작처럼 여러 단계로 이루어진 API 상태 전이는 프리뷰 ID 단위로
직렬화한다. 단일 Manager 안에서는 공용 레코드 잠금을 사용하고, PostgreSQL 운영
환경에서는 같은 키의 advisory lock을 함께 사용한다. 잠금을 얻은 뒤 영속 레코드를
새로 읽고 낙관적 버전 갱신이 충돌하면 최신 버전으로 제한 재시도하므로, 여러 브라우저
요청이나 Manager 교체 시점이 겹쳐도 사용자에게 `control record version conflict`를
그대로 반환하지 않는다.

2초 주기의 상태 관찰은 PostgreSQL 트랜잭션을 장시간 점유하지 않도록 프로세스 내부
잠금만 사용한다. 상태 변경은 여전히 버전 비교로 보호되며 다음 조정 주기에서 수렴한다.
생성·재시작 직후 비동기로 실행하는 조정 작업은 Service 종료 시 취소·대기하므로
Manager 교체 중 백그라운드 작업이 남지 않는다.

라우팅 조회는 hostname 인덱스, UI 목록은 프로젝트 인덱스, 포트·quota 계산은 사용자
인덱스를 사용한다. 따라서 과거 중지 기록이 늘어나도 브라우저의 3초 상태 조회가 전체
사용자의 프리뷰를 반복 순회하지 않는다. 프로젝트 목록은 실행 중인 프리뷰를 먼저
보여주고 최근 종료 기록까지만 반환한다.

## 4. 실행 프로필

임의 shell 명령을 API로 받지 않는다. 다음 allowlist만 지원한다.

| 프로필 | 실행 방식 |
|---|---|
| `auto` | `package.json`을 검사해 Next.js, Vite 또는 일반 npm으로 선택 |
| `next` | `npm run dev -- --hostname 0.0.0.0 --port {port}` |
| `vite` | `npm run dev -- --host 0.0.0.0 --port {port}` |
| `npm` | `PORT`, `HOST` 환경변수를 설정하고 `npm run dev` |

공통으로 `HOST=0.0.0.0`, `PORT={allocated-port}`를 전달한다. Vite에는 임의 Host header
공격을 막으면서 발급 hostname을 허용하도록 추가 환경변수를 전달한다. 프로젝트
작업 경로는 `/workspace/projects/{opaque-project-id}` 아래로 제한한다.

### 4.1 정확한 앱 경로 선택

`appPath`는 프로젝트 루트 기준 POSIX 상대경로다. 값이 없거나 `.`이면 프로젝트
루트를 뜻한다. 절대경로, `..` 경로 이탈, 역슬래시, 제어문자, 512바이트 초과 값은
Manager와 Web Chat BFF에서 거부한다. clientd는 실행 직전에 심볼릭 링크까지 해석한
실제 경로가 선택한 프로젝트 루트 안인지 다시 검사한다.

선택한 디렉터리에는 정상적인 `package.json`과 `scripts.dev`가 있어야 한다. clientd는
프로젝트 루트에서 최대 5단계·2,048개 디렉터리·64개 앱 범위로 후보를 탐지한다.
`node_modules`, `.git`, `.next`, 빌드 산출물과 숨김 디렉터리는 건너뛰고 디렉터리
심볼릭 링크를 따라가지 않는다. 브라우저에는 절대 작업 경로를 반환하지 않는다.

후보가 하나면 Web Chat이 자동으로 Project의 `previewAppPath`에 저장한다. 후보가 여러
개면 package 이름과 감지된 실행 프로필을 선택 목록으로 보여주며, 사용자가 고른 값만
저장한다. 따라서 한 프로젝트에 `company-landing`, `admin`, `apps/web`이 함께 있어도
탐지 순서의 첫 앱이 임의 실행되지 않는다. 사용자는 컨테이너 경로를 알거나 입력할
필요가 없다. 프리뷰 레코드의 `appPath`는 생성 후 바뀌지 않으며, 다른 앱을 실행하려면
프로젝트의 실행 앱을 바꾼다.

같은 프로젝트·같은 `appPath`에는 하나의 논리적 프리뷰만 둔다. Manager는 호출자가
서로 다른 `Idempotency-Key`를 사용하거나 여러 브라우저·Manager replica가 동시에
생성을 요청해도 app slot 잠금으로 기존 레코드를 재사용한다. `ready`이면 바로 열고,
`stopped` 또는 `failed`이면 같은 hostname과 port로 재시작한다. 과거 버전에서 남은
동일 앱의 중복 실행 레코드는 controller가 최신 레코드 하나만 남기고 중지한다.

Web Chat에는 일반 사용 흐름에서 `새 주소`를 노출하지 않는다. 주소 교체는 유출 대응처럼
명시적인 보안 사유가 있을 때 기존 프리뷰를 폐기하는 별도 운영 기능으로 다룬다.

### 4.2 의존성 준비

clientd는 개발 서버를 시작하기 전에 `package.json`과 npm lockfile의 SHA-256 지문을
확인한다. `package-lock.json` 또는 `npm-shrinkwrap.json`이 있으면 `npm ci`, 없으면
`npm install --no-audit --no-fund`를 실행한다. 성공한 지문은
`node_modules/.pie-preview-deps.sha256`에 저장해 변경이 없을 때 재설치를 생략한다.

동시에 같은 앱을 시작해도 설치는 원자적 잠금 디렉터리 하나만 수행한다. 잠금 소유
프로세스가 사라진 경우에는 고아 잠금을 회수하므로 비정상 종료 뒤에도 영구 대기하지
않는다. 현재 자동 의존성 준비는 npm 프로젝트가 기준이며 pnpm·Yarn 프로젝트는 별도
프로필을 추가하기 전까지 지원 대상으로 보지 않는다.

## 5. 공개와 비공개 프리뷰

### 비공개

기본값이다. Integration API가 발급하는 launch token은 수명이 짧고 hostname과
preview ID에 묶인다. 현재 토큰은 기본 2분 동안 재교환할 수 있는 단기 토큰이며,
서버 저장소에서 사용 여부를 기록하는 엄밀한 1회용 토큰은 아니다.

1. 브라우저가 `?__pie_token=...`이 포함된 단기 교환 URL을 연다.
2. Gateway가 token을 검증한다.
3. Gateway가 token query를 제거하는 `303` redirect를 보낸다.
4. 브라우저에는 `__Host-pie_preview` 쿠키를 설정한다.

쿠키는 `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`이며 `Domain` 속성이 없는 정확한
Host 전용 쿠키다. 따라서 한 프리뷰의 token·cookie를 다른 프리뷰 hostname에서 다시
사용할 수 없다. 세션 쿠키 기본 수명은 8시간이지만, 프리뷰가 중지·만료·정지되면
Manager route가 사라져 쿠키가 남아 있어도 접근할 수 없다.

공개 범위는 별도 프리뷰 종류가 아니라 기존 프리뷰의 접근 정책이다. 공개·비공개를
바꿔도 hostname, port, 실행 프로세스는 유지한다. 정책을 바꿀 때 `accessVersion`을
증가시키고 새 launch token과 session cookie를 이 값에 묶으므로 변경 전에 발급한
비공개 자격증명은 다시 사용할 수 없다. Gateway route cache 기본값이 2초이므로 여러
Gateway replica 전체에 새 정책이 반영되는 시간의 상한은 정상 상태에서 약 2초다.

Gateway는 인증 검사가 끝난 뒤 `__Host-pie_preview`를 upstream 요청에서 제거한다.
사용자 애플리케이션의 자체 쿠키는 그대로 보존하고, 애플리케이션이 같은 이름의
`Set-Cookie`로 Gateway 인증 쿠키를 덮어쓰려는 응답은 제거한다. 클라이언트가 보낸
`X-Pie-*` 내부 헤더도 upstream에 도달하기 전에 삭제한다.

### 공개

별도 로그인 없이 URL을 아는 사용자가 접근할 수 있다. 고객 애플리케이션이 공개를
명시한 경우에만 사용한다. 무작위 hostname은 접근제어를 대신하지 않으므로 민감한
프로젝트에는 반드시 비공개 모드를 사용한다.

## 6. Integration API

모든 API는 Integration service token과 사용자·프로젝트 소유권을 검증한다.

```text
GET    /v1/integrations/{integration}/users/{user}/projects/{project}/previews
POST   /v1/integrations/{integration}/users/{user}/projects/{project}/previews
GET    /v1/integrations/{integration}/users/{user}/projects/{project}/apps
PUT    /v1/integrations/{integration}/users/{user}/projects/{project}/preview-app
GET    /v1/integrations/{integration}/users/{user}/projects/{project}/previews/{preview}
POST   /v1/integrations/{integration}/users/{user}/projects/{project}/previews/{preview}/access
PUT    /v1/integrations/{integration}/users/{user}/projects/{project}/previews/{preview}/visibility
POST   /v1/integrations/{integration}/users/{user}/projects/{project}/previews/{preview}/stop
DELETE /v1/integrations/{integration}/users/{user}/projects/{project}/previews/{preview}/record
POST   /v1/integrations/{integration}/users/{user}/projects/{project}/previews/{preview}/restart
GET    /v1/integrations/{integration}/users/{user}/projects/{project}/previews/{preview}/logs
```

`apps`는 실행 가능한 앱의 상대경로·이름·프로필만 반환하며, `preview-app`은 실제로
탐지된 후보만 Project에 저장한다. 프리뷰 생성에서 `appPath`를 생략하면 Project에
저장된 `previewAppPath`를 사용한다. 기존 Integration 호환을 위해 명시적인 `appPath`
요청도 계속 지원한다.

`stop`은 프로세스만 끝내고 hostname을 포함한 Preview 레코드를 보존하므로 나중에 같은
주소로 재시작할 수 있다. `record` 삭제는 실행 중이면 먼저 프로세스를 안전하게 종료한
뒤 Preview 레코드와 라우팅 인덱스를 제거한다. 오류·중지 상태에서도 남은 프로세스
정리를 한 번 더 확인한다. 삭제 감사 로그는 별도 Audit 레코드로 보존하며, 다음 실행은
새 hostname을 발급한다. 기존 Integration 호환을 위해 Preview 본체에 대한 `DELETE`는
당분간 기존의 중지 동작으로 유지하고 새 구현은 명시적인 `stop`과 `record`를 사용한다.

생성 요청에는 `Idempotency-Key`가 필수지만, 리소스 단일성은 이 키에만 의존하지 않는다.
Manager가 `(integration user, project, appPath)` 단위로 기존 프리뷰를 재사용한다.
공개 범위 변경은 `visibility` API에 `{"visibility":"private"}` 또는
`{"visibility":"public"}`을 전달한다. 예제 Web Chat의 BFF는 service token을
브라우저에 노출하지 않고 로그인 사용자의 external ID로만 위 API를 호출한다. 응답을
브라우저에 전달할 때 `ownerUserId`와 `backendHost`를 제거하며 URL의 protocol과 hostname도
다시 검증한다.

```json
{
  "appPath": "company-landing",
  "profile": "auto",
  "visibility": "private",
  "ttlSeconds": 14400
}
```

응답과 목록에는 정규화된 `appPath`가 포함된다. 과거 레코드처럼 필드가 없는 경우에는
호환성을 위해 프로젝트 루트인 `.`으로 해석한다.

## 7. 주요 환경변수

```dotenv
PIE_PREVIEW_DOMAIN=preview.kroot.io
PIE_PREVIEW_PUBLIC_SCHEME=https
PIE_PREVIEW_PUBLIC_PORT=0
PIE_PREVIEW_GATEWAY_CONTAINER=pie-sandbox-test-preview-gateway
PIE_PREVIEW_GATEWAY_TOKEN=<32바이트 이상 무작위 값>
PIE_PREVIEW_ACCESS_SECRET=<32바이트 이상 무작위 값>
PIE_PREVIEW_RECONCILE_INTERVAL=2s
PIE_PREVIEW_DEFAULT_TTL=4h
PIE_PREVIEW_MAX_TTL=24h
```

`PIE_PREVIEW_PUBLIC_PORT=0`은 protocol 기본 포트를 의미한다. 로컬 환경은 Traefik이
18443을 사용하므로 bootstrap이 `18443`을 설정한다. Gateway token과 access secret은
서로 다른 무작위 값을 사용하고 로그·Git·브라우저 응답에 남기지 않는다.

## 8. Traefik과 TLS

Kroot 서버의 기존 공용 Traefik은 `kroot` resolver에서 HTTP-01을 사용한다. HTTP-01은
`*.preview.kroot.io` 와일드카드 인증서를 발급할 수 없다. DNS에 wildcard A record를
추가한 것만으로 TLS 인증서가 만들어지지 않는다.

따라서 운영 배포는 다음 원칙을 따른다.

- 기존 `traefik.yaml`을 덮어쓰지 않는다.
- Preview Gateway의 Docker label로 `HostRegexp` router만 추가한다.
- `preview.kroot.io`, `*.preview.kroot.io` 인증서는 DNS-01로 발급한다.
- 인증서는 기존 Traefik의 동적 TLS 설정으로 읽거나 DNS-01 resolver를 별도로 추가한다.
- 정적 resolver를 바꿔야 한다면 설정 백업·검증·롤백 경로를 만들고 Traefik을 재시작한다.

현재 DNS provider에서 자동 DNS-01 API를 바로 사용할 수 없다면 최초 검증은 수동
DNS-01로 하고, 고객 운영 전에는 delegated `acme-dns` 또는 운영 DNS provider의 API를
사용해 자동 갱신한다. 수동 인증서를 자동 갱신 없이 운영하면 안 된다.

### 8.1 최초 Kroot 서버 확인 결과

2026-08-04 기준 `preview.kroot.io`와 임의의 `*.preview.kroot.io` 이름은 모두
`221.143.48.77`로 해석된다. 그러나 공용 Traefik이 돌려주는 인증서는
`CN=TRAEFIK DEFAULT CERT`다. 기존 `kroot` resolver는 HTTP-01이고, 서버의 Certbot에도
`standalone`, `webroot` 플러그인만 있어 Gabia DNS를 자동 변경할 수 없다.

따라서 공개 배포의 남은 외부 선행 조건은 다음 중 하나다.

1. Gabia DNS API를 안전한 secret으로 제공하고 DNS-01 자동 갱신기를 구성한다.
2. `_acme-challenge.preview.kroot.io`를 API 사용이 가능한 `acme-dns` 구역으로 위임한다.
3. 최초 시험만 수동 DNS-01로 인증서를 발급하되, 고객 운영 전에 반드시 1번 또는
   2번으로 전환한다.

인증서가 준비되기 전에는 Preview Router를 운영 완료로 판정하지 않는다. 기존 공용
Traefik Compose와 HTTP-01 resolver를 임의로 덮어쓰거나 재시작하지 않는다.

수동 시험 인증서는 `deploy/test-server/shared-traefik-preview-tls.override.yaml`로
원본 공용 Compose 위에 겹쳐 적용한다. `apply-preview-tls.sh`가 인증서의 SAN·잔여
유효기간·개인키 일치를 검증하고 별도 디렉터리에 원자적으로 설치한다. 원본 Compose를
수정하지 않으므로 override 없이 공용 LB만 재생성하면 롤백할 수 있다.

### 8.2 2026-08-04 공인 인증서 적용 결과

Gabia 권한 DNS에 두 번의 DNS-01 TXT 값을 등록해 다음 인증서를 발급하고 공유
Traefik에 적용했다.

| 항목 | 결과 |
|---|---|
| 발급자 | Let's Encrypt `YE1` |
| SAN | `preview.kroot.io`, `*.preview.kroot.io` |
| 만료일 | 2026-11-02 |
| 원본 공용 Traefik Compose 변경 | 없음 |
| 적용 방식 | file provider TLS certificate override |
| 기존 API·Relay·채팅 확인 | 모두 HTTP 200 연속 확인 |
| 임의 프리뷰 hostname | 공인 인증서 검증 통과 후 HTTP 404 |

`apply-preview-tls.sh`는 실행 중인 공유 Traefik의 ACME 명령줄에서 기존 메일 값을
자동으로 인식한다. 따라서 운영 소스의 `current` 링크가 유지되면 별도 원본 수정 없이
같은 인증서를 멱등적으로 재적용할 수 있다.

이번 인증서는 수동 DNS-01로 발급했기 때문에 **자동 갱신되지 않는다**. 만료 30일
전까지 `_acme-challenge.preview.kroot.io`를 `acme-dns`로 위임하거나 Gabia DNS를
자동 변경하는 갱신기를 운영에 추가해야 한다. 현재 수동 인증서는 기능 시험과 제한된
운영 검증용이며 자동 갱신 전환을 고객 공개 게이트로 유지한다.

## 9. 로컬 검증

```bash
./deploy/local/bootstrap.sh
docker compose --env-file deploy/local/.env \
  -f deploy/compose.yaml -f deploy/local/compose.yaml \
  up -d --build manager preview-gateway traefik
node scripts/e2e/project-preview.mjs
```

2026-08-04 로컬 E2E에서 다음 항목을 실제 Docker 경로로 검증했다.

- 사용자 프로비저닝과 `kroot init`
- `apps/web` 하위 앱 자동 탐지, Project 선택값 저장 및 경로 생략 실행
- 복수 후보의 명시적 선택과 프로젝트 루트·심볼릭 링크 이탈 차단
- lockfile 유무에 따른 의존성 준비, 지문 재사용과 고아 설치 잠금 회수
- 서로 다른 앱 프리뷰의 동시 생성 및 중복 없는 port 할당
- 같은 앱의 동시 생성 요청이 hostname·port가 같은 단일 프리뷰로 수렴
- 5번째 프리뷰 quota 거부
- 사용자별 internal network와 Host port 미노출
- 비공개 launch token의 host 전용 cookie 교환
- Gateway 인증 cookie와 내부 header의 upstream 격리
- 다른 hostname에서 token 재사용 거부
- HTTP request body 왕복, chunked streaming과 WebSocket upgrade
- 공개 프리뷰 접근
- 같은 주소를 유지한 공개·비공개 전환과 이전 access generation 거부
- 로그, 재시작, 중지
- 실행 중·오류·중지 Preview 레코드 삭제, 런타임 정리와 감사 로그 보존
- 삭제 후 새 hostname 발급
- 중지된 프리뷰의 container-local port 재사용
- 사용자 정지 시 route 폐기
- Executor와 사용자별 preview network 및 고아 network 회수

같은 시험을 Kroot 서버의 실제 `https://api-relay.cookai.dev`와
`*.preview.kroot.io:443` 경로에서도 수행했다. Docker 검사는
`DOCKER_HOST=ssh://pie-sandbox-test`로 원격 daemon을 직접 확인했다. 사용자 생성,
`kroot init`, 프리뷰 4개 동시 실행, quota, private/public 접근, cookie·header 격리,
본문·스트리밍·WebSocket, 재시작·로그·port 재사용, 사용자 정지와 network 회수까지
20개 항목을 모두 통과했다. 시험 Integration은 즉시 `revoked`로 바꾸고 시험
Executor와 network가 0개인지 확인했다.

같은 날 실제 기존 프로젝트 `랜딩페이지`도 추가 검증했다. 유휴 회수된 Executor를
미리보기 요청이 자동으로 다시 생성했고 `company-landing` 하위 앱에서 의존성을 준비해
`ready`로 전환했다. 비공개 launch URL 200, 쿠키 재접속 200, 무인증 접근 401,
재시작 후 동일 hostname 유지와 HTTPS 200을 확인했다.

## 10. 운영 전 남은 검증

- Next.js·Vite 실제 HMR 장시간 검증(WebSocket upgrade 자체는 로컬 E2E 완료)
- 대용량 업로드 상한과 느린 client timeout 검증
- 실제 사용자 동시 프리뷰 CPU·메모리·inode·IOPS 부하 측정
- Gateway 다중 replica 시 route cache와 연결 drain 검증
- 악성 프로젝트를 가정한 seccomp/AppArmor 및 egress 정책 검증
- DNS-01 인증서 자동 갱신, 갱신 실패 경보와 만료 사전 경보

로컬 기능 성공은 운영 보안과 용량 검증을 대신하지 않는다. 특히 사용자 프로젝트가
임의 코드를 실행한다면 Docker의 커널 공유 경계를 고려해 별도 Node pool, gVisor,
Kata Containers 또는 MicroVM으로 강화하는 계획을 함께 유지한다.
