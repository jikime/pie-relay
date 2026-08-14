# 터미널 IME 인라인 조합 미리보기 (네이티브 터미널 느낌)

## 현재 상태 / 문제
P7에서 자체 textarea 오버레이로 한글 IME 입력을 받게 했다(TerminalScreen.tsx의
.term-input). 입력은 되지만 "진짜 터미널 같지 않다":
- 오버레이가 안 보이는 전체 덮개(좌상단)라, 조합 중인 한글(ㅎ→하→한, 밑줄)이 커서
  자리에 안 보이고 확정 후 셸 에코로만 뜬다.
- IME 변환 후보창이 커서가 아니라 화면 좌상단에 뜬다(textarea가 거기 있어서).

## 목표
네이티브 터미널처럼: 조합 중인 글자가 커서 셀에 실시간 표시되고, IME 후보창도 커서
옆에 뜬다.

## 해법: 입력 textarea를 커서 셀에 배치 + 조합 중 가시화

핵심 전환 — 오버레이를 "전체 덮개 투명"에서 "커서 셀에 위치한 작은 입력창"으로 바꾼다.
포커스 기반이라 크기·위치와 무관하게 모든 키를 받으므로, 작아도 입력 처리는 그대로다.

1. **커서 픽셀 위치 계산**:
   - 셀 크기: xterm의 `.xterm-screen` 엘리먼트(term.element!.querySelector('.xterm-screen'))
     의 clientWidth/term.cols = cellW, clientHeight/term.rows = cellH.
   - 커서 셀: term.buffer.active.cursorX, cursorY (뷰포트 기준 0..cols-1 / 0..rows-1).
   - term-host 패딩(6px 8px)만큼 오프셋. left = padL + cursorX*cellW, top = padT + cursorY*cellH.
   - 위치 갱신 트리거: term.onCursorMove, onRender(또는 fit 후), 창/컨테이너 resize.
2. **textarea 스타일**:
   - position:absolute, left/top = 위 계산값, height ≈ cellH, width는 최소 몇 셀(overflow
     visible로 긴 조합/후보 대비), pointer-events:none(마우스는 xterm), z-index 위.
   - font/size/lineHeight = 터미널과 동일(term의 fontFamily/fontSize), color = 터미널
     foreground(#cdd6f4), background transparent, caret-color transparent(커서는 xterm이 그림),
     resize none, autocapitalize/autocomplete/autocorrect/spellcheck off.
   - **평상시(비조합)**: 텍스트가 안 보이게(value는 비어 있으니 자연히 안 보임 — 확정 즉시
     value를 비우므로). 셸 에코가 커서에 뜨는 걸 가리지 않는다.
   - **조합 중**: value에 조합 문자열이 들어가고 color가 foreground라 커서 자리에 실시간
     표시된다(밑줄은 브라우저 기본 IME 표시에 맡김). 이게 인라인 미리보기.
3. **입력 처리(기존 P7 유지)**: compositionend→sendKey(e.data)+value 비움, input(insertText·
   비조합)→sendKey+value 비움, paste→sendKey, keydown→keyToSequence(조합 중이면 skip).
   달라지는 건 위치/가시성뿐. 확정 시 value를 비우므로 미리보기는 사라지고 셸 에코가 그 자리에.
4. **포커스**: 방이 보일 때/터미널 클릭 시 focus, hidden이면 focus 안 함(기존 유지).
   term-wrap onMouseDown로 focus.
5. **엣지**: 커서가 화면 밖(스크롤백)일 때/계산 실패 시 안전 폴백(좌상단 근처, 숨김)로.
   cellW/H가 0이면(레이아웃 전) 위치 갱신 skip.

## 불변식 유지
- 연결유지(hidden=display:none, 소켓/term dispose는 방 닫기만), 드라이버 게이트(sendKey),
  리사이즈/fit/스크롤백 재생 전부 불변. keyToSequence·전송 경로 변경 없음.

## 테스트/완료
- 커서→픽셀 위치 계산을 순수 함수 cursorPx({cursorX,cursorY,cellW,cellH,padL,padT})로
  분리해 vitest(간단 산술 검증). 기존 94 테스트 유지.
- npm run build / npx vitest run / cargo check 통과. GUI 실행 금지(위치·조합 시각 확인은 사람 몫).
- README 한 줄: 터미널 IME는 커서 위치 인라인 조합.

## 주의(사람 확인 필요)
조합 미리보기 위치·후보창 위치는 실제 GUI에서 미세 조정이 필요할 수 있다(패딩·라인하이트
오차). 1차 구현 후 사용자 시각 피드백으로 오프셋 보정.
