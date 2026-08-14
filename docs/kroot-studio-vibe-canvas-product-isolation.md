# Kroot Studio·Vibe Canvas 제품 분리 및 연결 운영 기록

기준일: 2026-08-12

## 결론

Kroot Studio와 Vibe Canvas는 같은 `cli-relay` 코드와 검증된 Docker 이미지를 사용할
수 있지만, 실행 중인 서비스와 인증 경계는 별개다. Vibe Canvas는 일회용 장치 연결
코드를 사용하고 Kroot Studio는 PAT를 사용한다.

기존 자격과 배포 호환 때문에 Vibe Canvas 스택의 일부 application·Compose 식별자에
`pie-canvas`가 남아 있다. 이것은 내부 slug일 뿐 제품명이나 공유 인증 경계를 뜻하지
않는다.

## 소스 소유 경계

| 역할 | 기준 소스 |
|---|---|
| Vibe Canvas 제품 UI·코드 발급/교환 API | `/Users/jikime/Dev/Private/vibe-canvas` |
| Kroot Studio 제품 UI | `/Users/jikime/Dev/Business/kaonsoftlab/kroot/frontend/clients/kroot-studio` |
| Kroot PAT 발급·저장·클라이언트 연결 | `/Users/jikime/Dev/Business/kaonsoftlab/kroot-adk` |
| Kroot PAT introspection·Relay | `/Users/jikime/Dev/Business/kaonsoftlab/kroot-server` |
| 공용 Pie Client·Manager·Relay | 이 `cli-relay` 저장소 |

Kroot Studio 기능을 `kroot-main-site`나 Vibe Canvas에 대신 구현하지 않는다. 공용
프로토콜 변경은 `cli-relay`에 두고, Kroot의 PAT 인증은 `kroot-adk`와 `kroot-server`,
Vibe의 코드 발급·교환은 Vibe Canvas 저장소가 각각 소유한다.

## 연결 흐름

```text
Vibe Canvas 화면
  → Vibe Canvas가 일회용 코드 발급
  → pie-client connect --server https://vibe-canvas-builder.vercel.app
  → Vibe Canvas /api/agent-runtimes/pairings/exchange
  → 응답의 controlUrl=https://api-relay.cookai.dev
  → Vibe Canvas 전용 Manager
  → Vibe Canvas 전용 Relay=https://relay.cookai.dev

Kroot Studio 화면
  → kroot auth login 또는 외부 서비스 가입/프로비저닝
  → 사용자 PAT를 ~/.kroot/credential.json에 0600으로 저장
  → kroot chat start
  → Authorization: Bearer <PAT>로 Kroot Relay /ws/agent 연결
  → Kroot Relay가 auth server에 PAT introspection
  → introspection의 userId로 사용자·장치 식별
```

Vibe Canvas의 `pie-client connect --server`는 최종 Relay 주소가 아니라 **코드를
발급한 Vibe Canvas 주소**다. 따라서 다음 명령은 Vibe Canvas 연결에 사용할 수 없다.

```bash
pie-client connect --server https://api-relay.cookai.dev --code ABCD-EFGH
```

`api-relay.cookai.dev`는 Vibe Canvas Manager Control API이므로 코드 교환 경로가 없고
404가 정상이다. Vibe Canvas가 새로 발급한 코드에는 다음 명령을 사용한다.

```bash
go -C ./client run ./cmd/client connect \
  --server https://vibe-canvas-builder.vercel.app \
  --code <새 코드>
```

코드는 1회용이며 만료된다. 이미 실패 시험에 사용했거나 시간이 지난 코드는 새로
발급해야 한다.

## 운영 스택 소유권

| 구분 | Vibe Canvas | Kroot Studio 테스트 |
|---|---|---|
| Compose project | `pie-relay-pie-canvas` | `pie-sandbox-test` |
| application | `pie-canvas` (호환용 slug) | `kroot-studio` |
| pool | `pie-relay-default` | `kroot-studio` |
| Manager | `https://api-relay.cookai.dev` | `https://api-relay-test.cookai.dev` |
| Admin | `https://admin-relay.cookai.dev` | `https://admin-relay-test.cookai.dev` |
| Relay | `https://relay.cookai.dev` | `https://relay-test.cookai.dev` |
| Web Chat | 제품 본체에서 사용 | `https://chat-relay.cookai.dev` |
| Preview | 전용 도메인 준비 전 외부 route 비활성 | `*.preview.kroot.io` |
| Kroot 자동 연결 | 사용 안 함 | 사용 |

두 스택은 PostgreSQL, 데이터 디렉터리, Relay secret, 장치 인증 secret, Manager ID,
Executor network를 공유하지 않는다. 같은 호스트와 같은 이미지 태그를 사용하는 것은
가능하지만 런타임 자격과 데이터까지 공유해서는 안 된다.

## 2026-08-12 적용·검증 결과

- Vibe Canvas가 생성하는 명령의 `--server`를 Control API가 아니라 요청 origin으로
  수정하고 Vercel 운영 배포를 완료했다.
- 운영 교환 API에 잘못된 코드를 보내면 404 HTML이 아니라
  `AGENT_RUNTIME_PAIRING_INVALID` JSON 400을 반환한다.
- 기존 Vibe Canvas 장치 자격으로 `pie-client start`를 실행하여 Vibe 전용 Manager에
  장치가 `online`으로 등록되고 Claude Code·Codex 준비 상태가 전달되는 것을 확인했다.
- 공유 서버에서 Vibe Canvas와 Kroot Studio를 별도 Compose project로 실행했으며 모든
  Relay·Manager·PostgreSQL·Preview·Web Chat 컨테이너가 healthy 상태다.
- 두 스택의 장치 인증 secret이 서로 다르고, Traefik router 이름·Host rule이 겹치지
  않는 것을 확인했다.
- Vibe Canvas Preview router는 예약 주소 `*.preview-vibe.invalid`로 비활성화하여
  Kroot Studio의 `*.preview.kroot.io`를 가로채지 않도록 했다.
- Vibe와 Kroot의 Manager·Relay ready endpoint 및 Kroot Web Chat health endpoint가
  모두 HTTP 200을 반환했다.

## Kroot PAT와 내부 Relay 자격의 구분

Kroot Studio에는 Vibe 방식의 일회용 코드 발급·교환 기능을 추가하지 않는다.

- `kpat_...` PAT: 실제 Kroot 사용자 신원과 외부 Kroot API 권한을 나타낸다.
- Manager Integration token: 웹채팅 BFF가 Manager API를 호출하기 위한 서버 간 자격이다.
- 짧은 Relay JWT: Manager와 Executor 사이에서 특정 사용자·세션을 전달하는 수명이 짧은
  전송 자격이다.

뒤의 두 자격은 PAT를 대체하지 않는다. 사용자별 Docker Executor에는 회원가입 또는
프로비저닝 때 받은 PAT로 `~/.kroot/credential.json`을 만들고, Kroot 명령과 Claude
Hook이 필요할 때 그 파일을 사용한다. PAT 원문은 브라우저, Relay frame, 이미지 layer,
공용 환경변수에 넣지 않는다.

Kroot 로컬 PC·서버 연결은 `kroot chat start`가 담당한다. Manager가 관리하는 사용자별
Docker Executor는 `pie-client connect --code`로 장치를 페어링하지 않고, Manager가
프로비저닝한 내부 clientd/session 경로와 사용자별 Kroot PAT 파일을 사용한다.
