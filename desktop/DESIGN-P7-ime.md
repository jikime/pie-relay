# 터미널 한글 IME 입력 우회 (WKWebView)

## 문제 (진단 완료)
Tauri WKWebView에서 xterm.js의 숨은 textarea에 IME 조합이 안 걸린다. 화면 이벤트
진단 결과 compositionstart/end가 전혀 발화하지 않고 한글 자모가 낱낱이 insertText로
들어온다(xterm.js 알려진 이슈 #3575/#5887). xterm이 매 입력마다 textarea를 비워
조합이 성립 못 하는 것이 원인. Chrome/Electron은 무관.

## 해법: 우리 소유의 IME 입력창을 오버레이
xterm 입력은 버리고, **우리가 만든 일반 textarea**(WKWebView에서 IME 정상 동작 —
앱 채팅창처럼)를 터미널 위에 투명하게 올려 모든 키보드 입력을 받아 PTY로 보낸다.
xterm은 표시 전용.

## 구현 (src/TerminalScreen.tsx)

1. **입력 오버레이**: term-wrap 안에 `<textarea class="term-input">` 을 절대배치로 깔고
   (opacity 0, 터미널 전체 덮음, caret 투명), 터미널 클릭/포커스 시 이 textarea에 focus.
   기존 xterm 자체 입력 경로(onData 송신, 내 compositionstart/end 핸들러, .xterm-helper-
   textarea CSS 이동, 진단 오버레이/ dbg)는 **전부 제거**한다.
2. **IME/문자 입력**: 이 textarea에
   - `compositionend` → e.data(완성 문자열) 전송, 그 후 textarea value 비움.
   - `input` (inputType==='insertText', isComposing===false) → e.data 전송, value 비움.
     (조합 중 input은 무시 — compositionend에서만 보냄)
   - `beforeinput` 으로 붙여넣기(insertFromPaste) → e.data 전송.
3. **특수키**: 이 textarea `keydown` 에서 제어키를 터미널 시퀀스로 매핑해 전송하고
   preventDefault. (조합 중이면 keydown 무시 — e.isComposing || keyCode===229 이면 skip)
   최소 매핑:
   - Enter→"\r", Backspace→"\x7f", Tab→"\t", Escape→"\x1b"
   - ArrowUp/Down/Right/Left→ "\x1b[A/B/C/D"
   - Home→"\x1b[H", End→"\x1b[F", PageUp→"\x1b[5~", PageDown→"\x1b[6~", Delete→"\x1b[3~"
   - Ctrl+a..z → 0x01..0x1a (String.fromCharCode(k-64)), Ctrl+C=\x03 등 포함
   - Ctrl+[ =\x1b, 필요한 흔한 것들. Alt+key → "\x1b"+key (meta) 는 선택.
   - 일반 문자 키(길이1, ctrl/meta 없음)는 keydown에서 처리하지 말고 input 이벤트에 맡김
     (조합·다국어 위해). preventDefault도 안 함.
4. **드라이버 게이트/뷰힌트 유지**: 전송은 기존 sendKey(shouldSendInput 드라이버 검사 +
   비드라이버 뷰힌트)를 그대로 통과. 즉 위 모든 전송은 sendKey(data) 경유.
5. **포커스 UX**: 터미널 영역 클릭 시 term-input.focus(). 방이 active(보이는) 상태로
   전환될 때도 focus. hidden일 때는 focus 안 함. 포커스 표시(테두리 등)는 최소.
6. **xterm은 표시 전용**: term.onData 구독 제거(입력은 오버레이가 담당). term.write(출력),
   fit, 리사이즈는 그대로. term의 자체 textarea가 키를 먹지 않도록, 오버레이가 위에서
   모든 키를 받게 z-index로 덮고 포커스를 오버레이에 둔다.
7. 리사이즈/드라이버전환/스크롤백 재생 등 기존 기능 불변. 연결유지 불변식(hidden=display:none,
   소켓/term 유지) 불변.

## 테스트
- 특수키 매핑을 순수 함수 `keyToSequence(e: {key,ctrlKey,altKey,metaKey})→string|null` 로
  분리해 vitest (Enter/Backspace/Arrow/Ctrl+C/Ctrl+A/일반문자=null 등).
- 기존 85 테스트 유지. 빌드/타입/cargo check 통과.

## 완료 기준
- npm run build / npx vitest run / cargo check 통과. GUI 실행 금지.
- 진단 오버레이·임시 dbg·isSafari/CSS 우회 흔적 제거.
- README 터미널 절에 "한글 등 IME 입력은 자체 입력 오버레이로 처리" 한 줄.
