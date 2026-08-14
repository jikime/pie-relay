# 초대 코드 권한 등급 + 호스트 등록 로컬 개방

원칙: "발급 권한은 전부 호스트가." 릴레이는 순수 중계. 누구를 어떤 권한으로 부를지는
호스트가 초대 코드로 결정한다. 두 가지를 함께 한다.

## A. 초대 코드 권한 등급 (view / control)

호스트가 초대 코드 발급 시 등급을 고른다:
- **관전(view)**: 이 코드로 온 참가자는 보기 전용 — 채팅 프롬프트/터미널 입력 불가.
- **조작(control)**: 이 코드로 온 참가자는 입력 가능. 터미널 방이면 **자동으로 드라이버**가 됨.

### 서버 변경
1. `POST /rooms/invites` 요청 바디에 `access: "view"|"control"`(선택, 기본 "view").
   조작 권한은 명시해야 하며 잘못된 값은 400으로 거부한다. inviteEntry 에 access 저장.
2. `POST /rooms/join` → 게스트 토큰에 `access` 클레임 추가(코드의 access). JWTAuth.Mint 를
   access 받도록 확장하거나 별도 MintWithAccess. Identity 에 Access 필드 추가, verify 시 파싱
   (없으면 "control" — 레거시 호스트/게스트 토큰 하위호환). role=host 는 항상 control 취급.
3. **입력 게이팅**(routeFromParticipant): 발신자 Access=="view" 이면 host 로 전달하는
   입력성 메시지(chat, pty_input, pty_resize, set_driver, permission_response, abort)를 드롭.
   비입력(예: request_screen 화면요청)은 허용. 즉 view 는 순수 관전.
4. **터미널 드라이버 획득**(control): participant 가 접속할 때 Access=="control" 이고 role!=host
   이면, 드라이버 자리가 비어 있을 때만 그 참가자를 자동 지정한다.
   - 터미널 방: 이미 활성 lease가 있으면 뒤에 온 control이 조작권을 빼앗지 않는다.
   - 채팅 방: executor 는 set_driver 를 모르므로 무시(무해). 즉 room 타입 몰라도 안전.
   - 접속 해제·lease 만료 후에는 다음 control 참가자가 요청하거나 host가 명시적으로 인계한다.
5. 테스트: view 게이팅(입력 드롭·관전 허용), control 자동 set_driver 발신, 레거시 토큰
   (access 없음)=control, 발급 시 access 저장/전달.

## B. 호스트 등록 로컬 개방 (HOST_ENROLL_SECRET 완화)

개인 사용(릴레이=호스트=같은 PC)에서 키 입력이 불필요하다.
- `POST /host/enroll`: 요청 RemoteAddr 이 루프백(127.0.0.1/::1)이면 **secret 없이 허용**
  (같은 PC 접근이라 안전). 비루프백이면 기존대로 HOST_ENROLL_SECRET 요구(설정 안 됐으면 403).
- 즉 로컬 릴레이면 GUI "방 만들기" 가 키 없이 동작. 공개 릴레이면 여전히 키로 게이트.
- 테스트: 루프백 요청 키 없이 200, 비루프백 키 없이 401/403.

## 데스크톱 변경
- 초대 발급 UI 에 **등급 선택(관전/조작)** 추가. createInvite 에 access 전달.
  발급된 코드 옆에 등급 배지 표시.
- "방 만들기": 발급 키 입력란을 **선택(고급)** 으로 — 로컬 릴레이면 비워도 됨. 안내 문구:
  "로컬 릴레이는 키가 필요 없습니다. 공개 릴레이면 운영자가 정한 키를 입력하세요."
  enrollHost 는 secret 이 비면 안 보냄(서버가 로컬 허용).
- 터미널 방에서 control 코드로 온 게스트가 자동 드라이버가 되는지 UI 반영(driver 프레임이
  이미 브로드캐스트되므로 추가 작업 최소).

## 완료 기준
- server: go build/vet/test + 위 테스트. 기존 초대/참가/enroll 하위호환.
- desktop: build/vitest/cargo check.
- e2e: 관전 코드 게스트=입력 드롭 확인 / 조작 코드 게스트(터미널)=자동 드라이버로 타이핑 통과 /
  로컬 enroll 키 없이 200.
- client/pty-host 변경 최소(set_driver 는 이미 수용). 필요 시 주석만.
