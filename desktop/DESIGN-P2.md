# Pie Relay Desktop (Phase 2) — Tauri v2 + Go 사이드카

2026-07-10. Phase 1(터미널 완결형 rooms — `../server/docs/rooms-design.md`)의 GUI 클라이언트.
**Tauri v2(Rust 셸) + React/Vite/TS 프런트 + 기존 Go client 바이너리 사이드카** 구성.
Rust는 셸·사이드카 관리 접착 코드만; 도메인 로직은 웹 프런트(참가자)와 Go(호스트)에 둔다.

## 단계

- **P2-1 참가자 모드 (사이드카 불필요)** — 코드로 방 참가 + 채팅 GUI. 웹뷰가 릴레이에 직접 접속.
- **P2-2 호스트 모드 (사이드카)** — 기존 Go client를 동봉해 login/데몬 시작·정지/room create를 GUI로.
- **P2-3 패키징** — 아이콘, dmg/msi 번들, 자동 시작 옵션.

## 와이어 계약 (Phase 1 확정분 — 재탐색 불필요)

릴레이 HTTP (base = ws URL의 ws→http 변환):

```
POST {base}/rooms/join   {"code":"8자","name":"bob"} → {"token":"<JWT>","room":"..."}
```

참가자 WebSocket: `{base ws}/ws/participant`
(브라우저 WebSocket은 Authorization 헤더 불가 → **`pie-relay.ticket.<JWT>` subprotocol 사용**,
릴레이의 `?ticket=` 호환은 운영 기본 차단이며 서버가 명시적으로 허용한 마이그레이션 기간에만 동작)

수신 이벤트(JSON 한 줄): `session_id{sessionId}` `text{text}`(스트리밍 델타) `thinking{text}`
`done{sessionId}` `error{message}` `aborted` `peer_chat{from,text}` `host:status{connected}`
(+`agent:status` 동일 의미 중복 — 멱등 처리) `agent:unavailable{reason}`

발신: `{"type":"chat","prompt":"<텍스트>","sessionId":"<있으면>"}`

- `from`은 절대 넣지 않는다 — 릴레이가 검증된 신원으로 강제 주입.
- sessionId는 첫 session_id 이벤트에서 캡처해 이후 발신에 유지(대화 연속성).
- 게스트에게 permission_request는 오지 않는다(릴레이가 host 전용 라우팅).

## P2-1 화면 (참가자 모드, 한국어 UI)

1. **접속 화면**: 릴레이 주소(기본 ws://127.0.0.1:13412), 초대 코드, 이름 → [참가]
   실패(만료/오타) 시 인라인 에러. 최근 접속값 localStorage 기억.
2. **채팅 화면**:
   - 상단 바: 방 이름·내 이름·호스트 상태(●연결/○끊김 — host:status)
   - 메시지 목록: 내 발신(우측), peer_chat(from의 guest:<name>-<rand>에서 name만 라벨),
     Claude 응답(text 델타 실시간 누적, done에서 확정), thinking은 접힌 회색 한 줄(토글),
     error/aborted 시스템 라인
   - 하단 입력창: Enter 전송, Shift+Enter 줄바꿈, 응답 중 전송 가능(그룹 대화)
   - ws 끊김: 배너 표시 + 지수 백오프 자동 재접속(TUI에 없는 개선점)

## 기술 결정

- Tauri v2, `@tauri-apps/cli`는 devDependency. 프런트: Vite + React + TypeScript.
- 외부 접속 허용: 웹뷰에서 임의 relay 주소로 fetch/ws — Tauri CSP에서 `connect-src`
  http/https/ws/wss 허용 (개발 편의; 조이면 P2-3에서).
- P2-1에서는 Rust 커스텀 커맨드 없음(순수 셸). 사이드카 배선은 P2-2에서
  `tauri.conf.json > bundle > externalBin` + shell plugin 으로.
- 상태 관리는 React 내장(useReducer 수준)으로 충분 — 외부 상태 라이브러리 금지.
- 프로토콜 파서/리듀서는 순수 TS 모듈(`src/protocol.ts`)로 분리해 vitest 단위 테스트.

## 완료 기준

- P2-1: `npm run build`(tsc+vite) 통과, `cargo check`(src-tauri) 통과, vitest 통과,
  로컬 릴레이 대상 수동 스모크 가이드 문서화. 커밋은 오케스트레이터 검토 후.
