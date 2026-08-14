# Phase 6 — 호스트 방 참가 모호성 제거

문제: 참가 화면의 "호스트 자격으로 참가" 체크박스는 데몬과 무관하게 토큰 하나를
막연히 가져와, (a) 어느 방인지 지정할 수 없고(초대 코드 입력란도 사라짐), (b) 여러
방을 운영할 때 구분이 안 된다. 서버는 이미 host 토큰의 room 클레임으로 방을 정하고
role=host 승격도 하므로, **릴레이 변경 없이 데스크톱 UX만** 고친다.

## 사실 정리 (설계 전제)
- 호스트 토큰(수동 티켓 또는 credentials.json accessToken)에는 `room` 클레임이 박혀
  있다(없으면 room=sub). 즉 토큰 하나 = 방 하나. 데몬은 이 토큰으로 그 방의 호스트가 된다.
- 현재 데스크톱은 데몬을 한 번에 하나만 띄운다(lib.rs DaemonHandle 단일 슬롯). 따라서
  "지금 내가 호스팅하는 방"은 항상 하나로 명확하다 — 방금 [데몬 시작]한 그 토큰의 방.
- 서버는 /ws/participant 에 host 토큰(role=host, 또는 빈 role→host 승격)으로 붙으면
  그 연결을 host 로 등록해 승인 카드/드라이버 제어를 준다. 추가 서버 작업 없음.

## 해결: 호스트 탭에서 "이 방 열기(호스트로 참가)"

데몬을 띄운 그 토큰·방으로 곧장 참가하게 한다 — 모호성 원천 제거.

1. **HostPanel**: 데몬 실행 중이면 "이 방 열기(호스트로 참가)" 버튼 노출. 클릭 시
   현재 호스트 토큰(수동 티켓 우선 → 없으면 host_access_token)을 확보하고, 그 토큰의
   `room` 클레임을 디코드해, App 으로 `onOpenHostRoom({wsUrl, token, room, asHost:true})`
   콜백을 올린다. wsUrl 은 HostPanel 의 relayUrl(= 데몬 relay-url) 기준.
2. **App**: onOpenHostRoom 수신 → rooms 에 방 추가(asHost:true, name 은 "나(호스트)"
   또는 호스트 표시), 참가자 모드로 전환하고 그 방을 active 로. 이미 같은 room 이 열려
   있으면 그 방으로 전환(중복 소켓 금지 — 기존 sameRoomIdentity 규칙 재사용).
   room 디코드는 기존 subFromToken 과 같은 JWT payload 파서를 재사용(신규 roomFromToken).
3. **ConnectScreen 정리**: "호스트 자격으로 참가" 체크박스의 역할을 **다른 기기에서
   호스트 토큰을 직접 붙여넣어 참가하는 고급 경로**로 좁힌다. 체크 시:
   - 초대 코드 입력란을 "호스트 토큰(JWT)" 입력란으로 바꿔 **명시적으로 토큰을 받는다**
     (지금처럼 credentials 를 자동으로 집어오지 않는다 — 그게 모호성의 원인이었다).
   - 붙여넣은 토큰의 room 을 디코드해 그 방으로 host 참가. 토큰 미입력 시 안내.
   - 라벨/도움말: "같은 PC에서 방금 띄운 방은 호스트 탭의 '이 방 열기'를 쓰세요."
   - localStorage 는 토큰을 저장하지 않는다(자격증명) — 방금 만든 P2 티켓 유지 원칙과
     구분: 여긴 편의보다 명확성 우선. (수동 티켓 자체는 HostPanel 에서 관리)

## 방 식별 표시 (부수 개선)
- 사이드바 방 항목·상단바에 room 식별자(짧게)를 함께 보여 어느 방인지 구분 가능하게.
  현재 room 이름이 게스트에겐 room 클레임(예 r-dev)일 수 있으니 그대로 노출하되, 호스트로
  연 방은 "호스트" 태그로 구분(이미 asHost 태그 있음).

## 테스트
- roomFromToken(JWT payload.room, 없으면 sub) 순수 함수 vitest.
- ConnectScreen host-token 경로: 토큰 입력 → room 디코드 → onJoined 호출 인자 검증.
- App onOpenHostRoom: 방 추가/중복 병합/활성 전환 리듀서 경로(기존 rooms.test 스타일).
- 기존 74 테스트 유지.

## 완료 기준
- npm run build / npx vitest run / (cd src-tauri && cargo check) 통과. GUI 실행 금지.
- README: 호스트가 자기 방 여는 두 경로(같은 PC=호스트 탭 버튼 / 다른 기기=토큰 붙여넣기) 명시.

## 이 Phase에서 안 하는 것 (후속)
- 여러 데몬 동시 운영(멀티 호스팅): lib.rs DaemonHandle 을 맵으로 확장하는 별도 작업.
  지금은 단일 데몬 전제라 "이 방 열기"로 충분. 여러 방 호스팅은 후속 Phase.
- 참가자 명단(roster) 브로드캐스트(드라이버 지정 자동 목록) — 별도.
