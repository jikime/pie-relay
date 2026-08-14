# Phase 4 — 사이드바형 멀티 룸 GUI

여러 호스트(방)에 **동시에 접속**해두고 사이드바로 전환한다. 나가지 않아도 각 방의
연결·대화·세션ID가 배경에서 유지된다. 서버·프로토콜 변경 없음(순수 desktop 프런트).

## 핵심 설계

**연결 유지가 관건**: 현재 ChatScreen은 방 하나의 소켓·리듀서·상태를 소유하고 언마운트 시
소켓을 닫는다(useEffect cleanup). 멀티 룸에서 탭 전환 시 언마운트하면 연결이 끊긴다.
→ **모든 참가 방의 ChatScreen을 항상 마운트**해두고, 비활성 방은 CSS로 숨긴다
(`hidden` prop → 컨테이너에 `hidden` 클래스/`display:none`). 소켓·리듀서는 계속 살아있다.

## App 상태 (src/App.tsx 리팩토링)

```
interface Room extends Session { id: string }   // id = 안정적 고유키(방+토큰 해시 or 카운터)
rooms: Room[]                 // 참가 중인 방들
activeRoomId: string | null   // 현재 보는 방 (null = 방 추가 화면)
unread: Set<string>           // 안 보는 방의 새 활동 표시
mode: "participant" | "host"  // 기존 유지
```

- 방 추가: ConnectScreen에서 join 성공 → rooms에 append + active로 전환. 같은 room+name
  중복 접속은 기존 방으로 전환(중복 소켓 방지).
- 방 닫기: 사이드바 항목의 × → 그 ChatScreen 언마운트(소켓 닫힘) + rooms에서 제거.
  활성 방을 닫으면 남은 첫 방으로, 없으면 방 추가 화면으로.
- 방 목록 localStorage 유지(wsUrl·token·room·name·asHost·id). 재시작 시 복원 시도,
  토큰 만료(12h)면 그 방은 ws가 열리지 않고 "연결 끊김"으로 표시 → 사용자가 닫고 재참가.

## 레이아웃 (사이드바)

```
┌──────────┬─────────────────────────────┐
│ 사이드바  │  활성 방의 ChatScreen        │
│          │  (나머지 방은 hidden으로 상주) │
│ ● 방A  2 │                             │
│ ● 방B    │                             │
│ ○ 방C    │                             │
│ ──────── │                             │
│ + 방 추가 │                             │
│ ──────── │                             │
│ 참가자    │                             │
│ 호스트    │                             │
└──────────┴─────────────────────────────┘
```

- 사이드바 항목: 호스트 연결 표시(●/○) + 방 이름 + 안읽음 뱃지(unread 카운트 또는 점).
  클릭 시 활성 전환(unread 클리어), × 로 닫기.
- "+ 방 추가": activeRoomId=null → 오른쪽에 ConnectScreen 표시(초대 코드/호스트참가 토글 등
  기존 그대로). 방이 하나도 없을 때의 기본 화면이기도 함.
- "호스트" 항목: 기존 HostPanel 표시(오른쪽 전체). 호스트 모드는 방 목록과 공존하는
  별도 뷰(사이드바 하단 섹션). 데몬 로그·초대 발급 등 기존 기능 그대로.
- 좁은 창 대응: 사이드바 최소 폭 유지, 필요 시 접기 버튼(선택, 과투자 금지).

## ChatScreen 변경 (최소)

- `Props`에 `hidden?: boolean`(비활성 시 컨테이너 숨김), `onActivity?: () => void`
  (비활성 상태에서 assistant text/done/peer_chat/tool_call 수신 시 호출 → App이 unread 표시).
- 소켓 생성/정리 useEffect의 의존성은 그대로(session 기준) — hidden 토글은 언마운트가
  아니므로 소켓 유지. **절대 hidden 때문에 소켓을 닫지 말 것.**
- 세션ID 칩·승인 카드·마크다운·재접속 등 기존 동작 전부 보존.
- onActivity: reducer 상태 변화 감지(예: messages 길이 증가/responding)로 App에 통지하되,
  자기 방이 active면 통지 안 함(App이 hidden 여부를 prop으로 알려주거나, App이 active와
  비교). 단순화: ChatScreen이 hidden일 때만 onActivity 호출.

## ConnectScreen 변경

- join 성공 콜백은 기존 유지. "취소/뒤로" 동작만 추가(방이 있을 때 방 추가를 취소하면
  직전 활성 방으로 복귀). 최근 접속값 localStorage 기억은 그대로.

## 테스트

- App/룸 관리 리듀서(방 추가/닫기/활성 전환/중복 방 병합/unread set)를 순수 함수로 뽑아
  vitest. ChatScreen의 hidden/onActivity 배선은 기존 protocol 테스트 유지 + 최소 추가.
- 기존 42 테스트 깨지지 않게.

## 완료 기준

- npm run build / npx vitest run / (cd src-tauri && cargo check) 통과.
- 수동 검증 시나리오를 README에 추가: 방 2개(다른 초대 코드) 동시 접속 → 사이드바 전환 →
  한 방에서 대화 중 다른 방 unread 뱃지 → 방 닫기. (GUI 실행은 사용자 몫, 코드 검증은 빌드/테스트)
- 커밋은 오케스트레이터가.
