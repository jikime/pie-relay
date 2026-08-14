# 터미널 폰트 — D2Coding 번들 + 폰트/크기 선택

## 배경
호스트 PTY는 폰트 정보를 전달하지 않으므로(터미널 프로토콜 특성, mosaic도 동일) 화면
폰트는 참가자 xterm이 결정한다. 지금은 시스템 폰트(Menlo+CJK 폴백)라 한글·영문 정렬이
어긋난다. 한글까지 고정폭인 D2Coding(OFL)을 앱에 번들해 기본 적용하고, 사용자가 폰트·
크기를 고를 수 있게 한다.

## 이미 준비된 것
- src/fonts/D2Coding-Regular.woff2, D2Coding-Bold.woff2 (WOFF2 변환 완료)
- src/fonts/LICENSE-D2Coding.txt (SIL OFL)

## 구현

1. **@font-face (styles.css)**: D2Coding Regular(400)/Bold(700)를 위 woff2로 등록.
   vite가 CSS url()의 상대경로 폰트를 번들한다. `url("./fonts/D2Coding-Regular.woff2")`.
   font-display: swap.
2. **기본 폰트**: TerminalScreen의 new Terminal fontFamily 기본값을
   `'"D2Coding", ui-monospace, SFMono-Regular, Menlo, monospace'`로. D2Coding은 한글·
   영문 모두 고정폭이라 폴백 CJK 폰트 나열 불필요(폴백은 D2Coding 미로드시 대비만).
3. **폰트 선택 설정 (신규, 터미널 방)**:
   - 옵션 목록(순수 상수): D2Coding(번들, 기본), JetBrains Mono, Menlo, SF Mono,
     Sarasa Term K, Consolas 등 이름+fontFamily 문자열. 번들 안 된 건 로컬 설치 시만 적용
     (설명 문구). 크기: 11/12/13/14/16.
   - 상단바(topbar-right)에 작은 "Aa" 설정 버튼 → 팝오버로 폰트·크기 선택.
   - 선택값 localStorage 유지(키 예: cli-relay.term-font = {family, size}). 앱 전체 터미널
     공통(방마다 따로 아님) — 단순화.
   - 적용: term.options.fontFamily/fontSize 갱신 후 fitAndSend()로 재fit(그리드·커서
     위치 재계산). 인라인 조합 입력창(.term-input)의 font도 동일 family/size로 갱신
     (cursorPx가 셀 크기를 .xterm-screen에서 실측하므로 위치는 자동 정합, 입력창 폰트만 맞추면 됨).
4. **패키징**: woff2는 vite 번들에 포함되므로 tauri bundle 별도 설정 불필요(dist에 들어감).
   LICENSE-D2Coding.txt는 소스에 동봉(재배포 고지). README에 폰트 번들·라이선스 명시.

## 불변식
- 연결유지·드라이버 게이트·리사이즈/스크롤백·IME 인라인 조합 전부 불변. 폰트만 교체+재fit.
- 폰트 변경 시 didFirstFit 무관하게 즉시 재fit.

## 테스트/완료
- 폰트 옵션 목록·localStorage 파싱을 순수 함수로 뽑아 vitest(간단). 기존 98 유지.
- npm run build / npx vitest run / cargo check 통과. GUI 실행 금지.
- README: 터미널 폰트(D2Coding 번들·OFL, 선택 가능) 한 단락.
