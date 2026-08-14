# 방 이름·내 이름 표시 별칭 편집 (로컬, 토큰 재발급 없음)

요구: 이미 만든 방에서도 호스트가 "방 이름"과 "내 이름"을 바꿀 수 있게. 단, 이 값은
토큰에 서명돼 있어 진짜 변경은 재발급이라 — **GUI 표시 별칭만** 바꾼다(릴레이 방 ID r-xxx·
토큰·데몬 그대로). 사용자 선택: 표시 이름(별칭)만 변경.

## 표시 슬롯 (현재)
- 사이드바 RoomItem: `label = room.room || (asHost ? '{name} (호스트)' : name)` → r-xxx 노출.
- 상단바(ChatScreen/TerminalScreen): `session.room`(r-xxx) + `나: {session.name}`("나 (호스트)").

## 데이터 모델
- Room(App.tsx Session)에 옵셔널 `label?: string`(방 표시 별칭) 추가. `name`은 이미 있음
  (호스트/참가자 표시 이름). `room`(실제 방 ID)·`token`은 불변.
- 별칭 저장: HostPanel localStorage 설정 blob에 roomName/hostName 유지(현재 secret처럼
  비영속이던 것을 영속으로). 이 두 값이 곧 방 별칭·내 이름.

## HostPanel
- 방 이름/내 이름 입력란을 **토큰 유무와 무관하게 항상** 표시(현재는 토큰 없을 때만).
  - 토큰 없음(방 만들기 전): enroll 힌트로 사용(기존).
  - 토큰 있음(방 존재): 표시 별칭 편집으로 사용 — 입력이 바뀌면 열린 asHost 방에 실시간
    반영(onRenameHostRoom).
- roomName/hostName을 Persisted에 추가해 localStorage 저장(재시작 유지).
- 방 상태 표시: "방: {roomName || roomLabel(r-xxx)}" 로 별칭 우선.
- 자동 참여(onOpenHostRoom) 호출 시 label=roomName(있으면), name=hostName||"나 (호스트)" 전달.
- 새 prop `onRenameHostRoom?({label,name})` 받아, 토큰 있을 때 roomName/hostName 변경 시 호출
  (디바운스 불필요, onChange마다 호출해도 무해 — 리듀서가 asHost 방 하나만 갱신).

## App
- Room에 label 추가. onOpenHostRoom({wsUrl,token,room,label?,name?})로 확장 —
  next.label=label, next.name=name||"나 (호스트)".
- 신규 onRenameHostRoom({label,name}) → dispatch rename. asHost 방(현재 열린 호스트 방)을
  찾아 label·name 갱신. (asHost 방이 여러 개일 일은 없음 — 단일 데몬. 있으면 가장 최근/활성.)
- RoomItem label 계산: `room.label?.trim() || room.room.trim() || room.name || "방"`.

## rooms.ts
- RoomsAction에 `{ kind:"rename"; id?:string; label?:string; name?:string }` 추가.
  id 없으면 asHost 방을 대상으로. 해당 room의 label/name만 갱신(소켓·mode·active 불변,
  연결유지 불변식 유지). 순수 리듀서 + 테스트.

## 상단바
- ChatScreen/TerminalScreen: room 이름 표시를 `session.label || session.room || "방"`으로.
  `나: {session.name}`는 그대로(name이 곧 별칭).

## 불변식/주의
- 소켓·토큰·mode·active 불변. label/name은 순수 표시값. 연결유지 불변식 유지.
- 별칭은 로컬 전용 — 게스트 화면엔 게스트 자신의 관점이 보이므로 호스트 별칭은 호스트
  화면에만 반영(방 ID는 공통). 이는 의도된 동작(문구로 설명 불필요, 과설명 금지).

## 완료 기준
- npm run build / npx vitest run(기존 128 + rename 테스트) / cargo check 통과.
- README 호스트 절에 "방 이름·내 이름은 표시 별칭(로컬)로 언제든 편집" 한 줄.
- GUI 실행 금지(레이아웃/실시간 반영은 사용자 확인).
