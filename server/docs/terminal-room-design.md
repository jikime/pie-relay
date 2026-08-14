# Phase 5 — 터미널 방 (PTY 직접 조작)

프로토타입(`prototypes/pty/`)을 프로덕션으로 승격. 기존 "채팅 방"(SDK claude)과
**공존하는 새 방 타입 "터미널 방"**: 호스트 PC의 셸(zsh)을 PTY로 띄우고, 참가자가
xterm.js로 그 터미널을 직접 조작한다. 결정사항: 셸(zsh) 범용 / 호스트가 드라이버 지정 /
터미널 방만 먼저(동료 채팅 패널은 다음 단계).

## 핵심 통찰 — 릴레이는 거의 안 바뀐다

릴레이는 이미 방 단위로 host↔participant 바이트를 중계하고, participant→host 메시지에
검증된 `from`(신원)을 주입한다. 터미널 프레임도 그 위를 그대로 탄다:
- `pty_output`(host→전체 브로드캐스트) = 기존 host→participant 팬아웃
- `pty_input`/`pty_resize`(participant→host) = 기존 participant→host 전달, `from` 주입됨
- **드라이버 제어는 릴레이가 아니라 호스트에서** `from`으로 강제 → 릴레이 무변경 유지

## 프로토콜 (기존 JSON 이벤트에 터미널 타입 추가)

```
host → 전체:      {"type":"room_mode","mode":"terminal"}            # 접속 시 1회, GUI가 xterm 모드로
host → 전체:      {"type":"pty_output","data":"<base64>"}           # PTY 출력
host → 전체:      {"type":"pty_exit","code":0}
host → 전체:      {"type":"driver","from":"<드라이버 sub 또는 ''>"}  # 현재 드라이버 공지
participant→host: {"type":"pty_input","data":"<utf8>"}              # 키 입력 (relay가 from 주입)
participant→host: {"type":"pty_resize","cols":120,"rows":34}
participant→host: {"type":"set_driver","target":"<sub>"}            # role=host만 유효
```
- base64로 바이너리/UTF-8/이스케이프 보존.
- 릴레이는 이 타입들을 특별 취급하지 않는다(기존 라우팅 그대로). `set_driver`는 host만
  보낼 수 있어야 하므로, 기존 permission_response/abort와 같은 "host만 통과" 게이트에
  `set_driver`를 추가한다(server.go routeFromParticipant). 이게 유일한 릴레이 변경.

## 드라이버 제어 (호스트 측, from 기반)

pty-host가 현재 드라이버 sub를 들고 있다:
- 기본값: 없음(빈 문자열) = 아무도 입력 못 함(보기 전용). 또는 호스트 오퍼레이터 본인.
- `set_driver{target}`(host 발신) 수신 → 드라이버 갱신 → `driver{from}` 브로드캐스트.
- `pty_input{from}` 수신 → `from === driver`일 때만 pty.write, 아니면 드롭.
- `pty_resize`는 드라이버만 반영(창 크기 충돌 방지). 뷰어의 로컬 fit은 각자.
- 호스트 오퍼레이터(role=host로 참가)는 항상 드라이버 될 수 있고, 언제든 회수.

## S5-1 릴레이 (server, 최소)

- `routeFromParticipant`의 "host만 통과" 목록에 `set_driver` 추가(현재 permission_response/
  abort와 동일 게이트). 그 외 무변경. 테스트 1개.

## S5-2 데몬 + PTY 호스트 (client)

- 신규 `node-executor/pty-host.mjs`: node-pty로 zsh 스폰(TERM=xterm-256color, cwd=
  CLI_RELAY_DEFAULT_CWD||homedir). **stdio JSON 프레임**으로 Go 데몬과 통신(프로토타입의
  ws 대신 stdio). 드라이버 상태 관리 + from 기반 입력 게이트 + set_driver 처리.
  접속 시 room_mode/driver 공지 프레임 방출. pty.onData→pty_output, onExit→pty_exit.
- Go 데몬: 터미널 모드 분기. `CLI_RELAY_ROOM_MODE=terminal`이면 executor.mjs 대신
  pty-host.mjs를 감독하고 relay ws ↔ pty-host stdio를 그대로 브리지(chatagent.Run과
  대칭인 최소 경로; 바이트 통과라 로직 거의 없음). 기본(env 없음)은 기존 SDK 채팅 모드.
- package.json에 node-pty 추가. 프로토타입에서 검증된 spawn-helper chmod 주의 반영.

## S5-3 데스크톱 (desktop)

- **호스트 탭**: "방 타입" 선택(채팅 / 터미널). 터미널 선택 시 데몬 시작에
  `CLI_RELAY_ROOM_MODE=terminal` env 전달(lib.rs host_daemon_start 확장). 작업 디렉토리
  필드는 CLI_RELAY_DEFAULT_CWD로 공유(PTY cwd).
- **참가자 화면**: room_mode==="terminal" 수신 시 채팅 UI 대신 **xterm.js 터미널 뷰**로
  전환. xterm.js는 npm 의존으로 추가(@xterm/xterm + @xterm/addon-fit). pty_output→
  term.write, term.onData→pty_input, resize→pty_resize(디바운스).
- **드라이버 UI**: role=host 참가자(오퍼레이터)에게 참가자 목록 + "조작권 주기" 버튼
  (set_driver 발신). 현재 드라이버는 driver 프레임으로 전원 표시. 비드라이버는 입력 시
  "보기 전용 — 조작권 없음" 힌트, 키 전송 안 함(로컬에서 막고 서버 왕복 낭비 방지).
- 멀티룸 사이드바와 공존: 터미널 방도 방 목록의 한 항목(아이콘으로 구분). 연결 유지
  불변식 동일(비활성 방 xterm도 마운트 유지, display:none).

## 성공 기준

- 데스크톱에서 호스트가 "터미널 방"으로 데몬 시작 → 초대 코드 발급.
- 참가자가 참가 → xterm.js 터미널 표시. 호스트가 조작권 주면 그 참가자가 직접 타이핑,
  그 안에서 claude 대화형 실행. 조작권 없는 참가자는 화면만 봄(입력 차단).
- 채팅 방은 그대로 동작(회귀 없음). 멀티룸에서 터미널 방·채팅 방 혼재 가능.
- 각 저장소 build/test 통과. e2e: 터미널 방 드라이버 왕복 + 비드라이버 입력 차단 실증.

## 이 Phase에서 안 하는 것 (다음 단계)

- 동료 채팅 패널(터미널 방 옆), 스크롤백 복원, 여러 PTY 탭, 파일 전송.
