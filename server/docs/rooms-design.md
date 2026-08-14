# Pie Relay Rooms — 다중 사용자 대화방 설계 (Phase 1: 터미널 완결형)

2026-07-10. 현재의 "같은 JWT `sub` 페어링" 모델을 **room 기반 다자 대화**로 확장한다.
서로 다른 계정의 사용자들이 각자 신원(sub)으로 한 방에 모여, 호스트 PC의 Claude Code와
그룹 채팅하듯 대화한다(from/to 구분). vibe-canvas 없이 릴레이 + CLI만으로 완결한다.

## 용어 (기존 "browser" 명칭 폐기)

| 역할 | 설명 | ws 축 |
|---|---|---|
| **host** | claude를 실행하는 PC의 데몬 (기존 agent) | `/ws/agent` (유지) |
| **participant** | 방에 참가해 대화하는 사용자 | `/ws/participant` (신규) |

`/ws/browser`는 `/ws/participant`의 deprecated 별칭으로 당분간 유지한다.

## JWT 클레임

```
{ iss: "pie-relay", aud: "pie-relay", sub: "<신원>", room: "<roomID>",
  role: "host"|"participant", cap: [...], jti, iat, exp }
```

- `sub` = 그 사람의 신원 (from 표기에 사용). 게스트는 `guest:<name>-<rand4>`.
- `room` = 페어링 키. 구형 토큰의 `room` 누락 시 `room = sub`로 해석하는 코드는 유지하지만,
  scope 없는 토큰은 운영 기본값에서 거부한다. 제한된 전환 기간에만 별도 legacy 옵션을 켠다.
- 검증은 기존 `jwtauth.go` HS256 오프라인 검증 유지. `exp` 필수 유지.

## Registry 변경 (server/internal/relay/registry.go)

```go
// 기존: agents[userID], browsers[userID]set
// 변경: 키를 roomID로, participant에 신원 메타 부여
hosts        map[string]Sender                    // roomID → host
participants map[string]map[Sender]Participant    // roomID → sender → {UserID, Role}
```

방은 명시적 생성 없이 첫 접속으로 존재(무상태 유지). host 슬롯은 room당 1개(기존과 동일).

## 릴레이 라우팅 정책 (server.go)

릴레이는 계속 페이로드 본문을 불투명하게 다루되, **최상위 JSON의 `type`·`from`·`to`만** 취급한다
(기존 `peekMessageType`의 확장).

1. **participant → host**: 최상위 객체에 `from: <검증된 sub>`를 **강제 주입**(클라이언트가 보낸
   from은 무시·덮어쓰기 — 위조 방지). 파싱 불가 메시지는 폐기.
2. **participant chat 에코**: `type=="chat"`인 메시지는 host 외에 **다른 participant들에게도**
   `{type:"peer_chat", from, text, images생략}` 형태로 팬아웃 — 전원이 서로의 질문을 본다.
3. **host(agent) → 방**: 기본 전원 브로드캐스트 (기존 팬아웃 유지).
4. **권한 메시지 게이트**:
   - `permission_request`는 **role=host인 participant 연결에만** 전달. 게스트는 못 본다.
     (호스트 본인도 participant축으로 접속해 자기 방을 조작할 수 있다 — role=host 토큰.)
   - `permission_response`·`abort`는 role=host가 보낸 것만 host 데몬으로 전달, 그 외 폐기.
5. `agent:status`(→ `host:status` 별칭 추가)는 기존대로 프레즌스 통지.

## 초대 흐름 (릴레이가 발급자 겸임 — 터미널 완결)

릴레이에 HTTP 엔드포인트 2개 추가. 초대 코드는 **메모리 보관**(TTL 15분, 단일 인스턴스 전제 —
기존 registry와 동일한 가정)이라 무상태 철학의 실용적 절충.

```
POST /rooms/invites   Authorization: Bearer <host JWT>
  → { "code": "8자영숫자", "expiresAt": ... }        # room = 토큰의 room(없으면 sub)

POST /rooms/join      body: { "code": "...", "name": "bob" }
  → { "token": "<participant JWT>", "room": "..." }  # sub=guest:bob-x7k2, exp=12h
```

`relay -mint`는 유지하되 `-mint-room`, `-mint-role` 플래그를 추가(개발용).

## Client (Go) 확장

```
client                      # 기존: host 데몬 (변경 없음)
client room create          # 저장된 host 자격으로 POST /rooms/invites → 코드 출력
client join <code> [--name] # POST /rooms/join → 토큰 획득 → TUI 채팅 진입
```

TUI는 **Bubble Tea**: 메시지 목록(화자 라벨 = from, host의 claude 응답은 "Claude"),
스트리밍 text 델타 렌더, 하단 입력창, `peer_chat`·`host:status` 표시. 게스트 화면에는
permission_request가 오지 않으므로 단순 채팅 UI로 충분.

## Executor 변경 (최소)

`chat` 요청에 `from`이 있으면 프롬프트 앞에 화자를 표기해 claude가 다자 대화임을 알게 한다:
`[bob] 이 함수 설명해줘` 식. 그 외 변경 없음(같은 sessionId 합류 동작이 이미 그룹 대화에 적합).

## 보안 원칙 (Phase 1)

- 게스트 토큰(role=participant)으로 `/ws/agent` 접속 불가.
- permission 승인·abort는 host 전용 (위 게이트 4).
- 초대 코드 TTL 15분, participant 토큰 exp 12h.
- 게스트가 있는 방에서 데몬은 `bypassPermissions` 사용 금지(문서화; 강제는 Phase 2).

## 이후 단계

- **Phase 2 (GUI)**: Tauri 셸 + 이 Go client를 사이드카로 동봉. 코어 로직 변경 없음.
- 호스트 다중 기기(deviceId 라우팅), 발언권 세분화, vibe-canvas 웹 참가는 후순위.

## 작업 순서

1. **S1 server**: registry room화 + JWT room/role + 라우팅 정책 + invite/join 엔드포인트 + 테스트
2. **S2 client**: room create/join 서브커맨드 + Bubble Tea TUI
3. **S3 executor**: from 화자 표기

---

# Phase 3 — 도구 승인·활동 피드·권한 정책·마크다운 (2026-07-10 확정)

## P3-1 게스트 chat 새니타이즈 (relay, 보안 필수)

role=participant 발신 메시지에서 최상위 위험 필드를 **릴레이가 제거**한다
(from 강제 주입과 같은 지점): `permissionMode`, `claudePath`, `systemPrompt`,
`disallowedTools`, `cwd`, `projectPath`. role=host 발신은 보존.
근거: 게스트가 bypassPermissions/임의 cwd 를 실어 승인 우회·임의 경로 작업이 가능했다.

## P3-2 방 권한 정책 (executor)

- `CLI_RELAY_PERMISSION_MODE` env(데몬이 executor 에 전달)가 있으면 **모든 chat 의
  permissionMode 를 무시하고 이 값 사용** (방 단위 정책 · 호스트가 GUI에서 선택).
- `CLI_RELAY_DEFAULT_CWD` env: chat 에 cwd 없거나 무효일 때 homedir 대신 이 경로.

## P3-3 호스트 승인 UI + 활동 피드 + 마크다운 (desktop)

- **호스트로 참가**: 참가자 탭에 "호스트 자격으로 참가" 옵션 — 초대 코드 대신
  호스트 토큰(티켓란 값 우선, 없으면 credentials.json — Rust 커맨드로 노출)으로
  /ws/participant 접속. 릴레이가 role=host 연결에만 permission_request 를 보내므로
  이 연결에서만 승인 카드가 뜬다.
- **승인 카드**: permission_request{requestId,toolName,input} 수신 → 채팅 인라인 카드
  (도구명 + input 요약 + [허용]/[거부]) → {type:"permission_response",requestId,allow}
  발신 (릴레이가 host만 통과시킴 — 기존 게이트 그대로).
- **활동 피드**: tool_call{name,input} / tool_result 를 전 참가자 채팅에 접힌
  활동 라인("🔧 Bash: go test ./...")으로 렌더 (릴레이는 이미 브로드캐스트함).
- **마크다운**: Claude 응답 text 를 react-markdown + remark-gfm 으로 렌더
  (raw HTML 비활성 기본값 유지 = XSS 안전, 코드블록 스타일링).
- **권한 모드 선택**: 호스트 탭 셀렉트(기본: 승인 필요 default / acceptEdits /
  bypassPermissions 경고 문구) → 데몬 시작 시 CLI_RELAY_PERMISSION_MODE env.
  작업 디렉토리 필드 → CLI_RELAY_DEFAULT_CWD.

와이어 기존 사실: permission_request 는 executor 가 이미 발신, permission_response 는
stdin 으로 수신 처리 존재. tool_call/tool_result 이벤트도 이미 존재. TUI 반영은 후속.
