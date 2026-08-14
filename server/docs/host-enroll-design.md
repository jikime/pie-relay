# 호스트 자체 인증 (vibe-canvas 브라우저 로그인 제거)

## 목표
호스트 인증을 외부 브라우저 로그인(vibe-canvas PKCE)에서 **릴레이 자체 발급**으로 통일한다.
호스트는 GUI "방 만들기"로 릴레이에서 호스트 토큰을 즉석 발급받는다. 게스트는 기존 초대
코드 그대로. 외부 의존(vibe-canvas) 완전 제거.

## 권한 모델 (확정: 서버 발급 키)
릴레이에 `HOST_ENROLL_SECRET` 환경변수를 둔다. 이 키를 아는 사람만 방·호스트 토큰을 만들
수 있다(릴레이 운영자 = 호스트 발급자). 미설정이면 등록 엔드포인트 비활성(503).
직접 로컬 개발에서만 `ALLOW_LOOPBACK_ENROLL_WITHOUT_SECRET=true`로 발급 키 없는 등록을
명시적으로 열 수 있다. 리버스 프록시 뒤에서는 공개 요청도 `RemoteAddr=127.0.0.1`로 보일 수
있으므로 이 옵션을 켜면 안 된다.

## S1 릴레이 (server)
- 신규 `POST /host/enroll` (invites.go 또는 신규 파일):
  - body `{"secret":"...","room":"(선택)","name":"(선택)"}`
  - `secret != HOST_ENROLL_SECRET` → 401. secret 미설정(서버) → 503.
  - room 미지정 시 생성(예: "r-"+8자 랜덤). name 미지정 시 "host". sub = sanitize(name).
  - `auth.Mint(sub, room, RoleHost, hostEnrollTTL)` → `{"token","room","expiresAt"}` 반환.
  - hostEnrollTTL 예: 720h(30일) — 자체 발급이라 길게. CORS 적용(webview fetch).
- main.go: `HOST_ENROLL_SECRET` 읽어 Inviter(또는 신규 Enroller)에 배선. 로그.
- 기존 /rooms/invites·/rooms/join·JWTAuth·registry 불변.

## S2 클라이언트 (client) — 브라우저 로그인 제거
- `client login` 서브커맨드 + internal/loginflow + internal/pkce 제거(vibe-canvas 전용).
  internal/credentials 는 유지하되 "브라우저 로그인으로 채우는" 경로 제거 — 데몬은 이제
  RELAY_TICKET(수동 티켓=발급받은 호스트 토큰)만 쓴다. credentials.json refresh 경로도
  더는 필요 없으면 제거(EnsureFreshAccessToken/refresh). 단, 과도한 삭제로 빌드 깨지지
  않게: main.go 의 login 분기·loginflow 호출·vibe 관련만 우선 제거하고, credentials 는
  RELAY_TICKET 없을 때 "먼저 방을 만드세요" 안내로 대체.
- go.mod 에서 vibe/PKCE 전용 의존 있으면 정리(없으면 그대로).
- 데몬 실행: RELAY_TICKET(호스트 토큰)로 접속하는 경로가 주 경로. 티켓 없으면 즉시 종료+안내.

## S3 데스크톱 (desktop) — GUI "방 만들기"
- HostPanel 의 vibe-canvas "로그인" 섹션·vibeURL 필드·host_login 커맨드 사용 제거.
- 신규 "방 만들기" 섹션: 입력 [발급 키], [방 이름(선택)], [내 이름(선택)] + [방 만들기] 버튼.
  → relay `POST {httpBase}/host/enroll` 호출 → 반환 토큰을 **수동 티켓(localStorage)로 저장**
  → 이후 데몬 시작/이 방 열기/초대 발급/토큰 복사 전부 이 티켓 사용(기존 배선 그대로).
  → 방·만료 표시. 발급 키는 저장하지 않음(자격).
- host_access_token(credentials.json) 폴백은 남겨도 되나, 주 경로는 발급 토큰=수동 티켓.
- lib.rs: host_login 커맨드 제거(또는 미사용). relay httpBase 는 relayUrl 에서 유도(기존
  wsOriginFromRelay/HTTPBase 재사용).
- README: 호스트 인증을 "방 만들기(발급 키)"로 갱신, vibe-canvas 언급 제거.

## 흐름 (최종)
```
[릴레이 운영] HOST_ENROLL_SECRET 설정
[호스트] GUI 방 만들기(발급 키 입력) → 호스트 토큰 발급·저장 → 데몬 시작 → 초대 코드 발급
[게스트] 초대 코드로 참가 (변경 없음)
```

## 완료 기준
- server: go build/vet/test + /host/enroll 테스트(정상·틀린 키·미설정 503).
- client: go build/test, login 제거 후에도 데몬(RELAY_TICKET) 정상.
- desktop: npm run build/vitest/cargo check, 방 만들기→티켓 저장 흐름.
- e2e: /host/enroll 로 받은 토큰으로 데몬 접속 + 초대 발급 + 게스트 참가.
