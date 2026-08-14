# Claude Code 서브에이전트 실시간 스트리밍 설계

## 1. 목적

Claude Code의 메인 에이전트가 Task/Agent 도구로 서브에이전트를 실행하면, 예전 구현은
작업 시작·진행·완료 상태만 채팅에 표시했다. 서브에이전트가 실제로 보내는 텍스트와
thinking, 도구 호출은 메인 에이전트가 최종 결과를 받을 때까지 화면에 보이지 않았다.

현재 구현은 각 서브에이전트의 원본 출력을 별도 카드에서 실시간으로 보여준다. 여러
서브에이전트가 동시에 실행되어도 본문이나 도구 결과가 서로 섞이지 않는다.

## 2. 공식 SDK 기준

Claude Agent SDK의 다음 기능을 사용한다.

- [`forwardSubagentText`](https://code.claude.com/docs/en/agent-sdk/typescript): 서브에이전트의 text와 thinking 블록을 전달한다. 전달된 메시지의 `parent_tool_use_id`로 어느 Task에서 나온 출력인지 구분한다.
- [`agentProgressSummaries`](https://code.claude.com/docs/en/agent-sdk/typescript): 약 30초 간격으로 실행 중인 에이전트의 진행 요약을 `task_progress.summary`에 넣는다.
- [`task_started`, `task_progress`, `task_notification`](https://code.claude.com/docs/en/agent-sdk/typescript): 작업 ID, Task 도구 ID, 에이전트 유형, 토큰·도구·소요 시간 정보를 제공한다.
- [`SubagentStart`, `SubagentStop` hooks](https://code.claude.com/docs/en/hooks): 별도 감사나 외부 관측이 필요할 때 사용할 수 있다. 현재 채팅 스트림은 SDK 메시지를 직접 사용하므로 Hook 파일을 다시 읽지 않는다.

Executor 기본 옵션은 다음과 같다.

```js
{
  includePartialMessages: true,
  forwardSubagentText: true,
  agentProgressSummaries: true,
}
```

## 3. 이벤트 흐름

```text
Claude Agent SDK
  ├─ task_started / task_progress / task_notification
  ├─ stream_event(parent_tool_use_id)
  ├─ assistant(parent_tool_use_id)
  ├─ user tool_result(parent_tool_use_id)
  └─ tool_progress(parent_tool_use_id)
        ↓
Node Executor
  ├─ subagent_text
  ├─ subagent_thinking
  ├─ subagent_tool_call
  ├─ subagent_tool_result
  ├─ subagent_tool_progress
  └─ task_started / task_progress / task_complete
        ↓
clientd → Pie Relay → Manager journal → BFF SSE
        ↓
웹 채팅의 서브에이전트별 카드
```

Node Executor는 `task_id`와 `tool_use_id`를 양방향으로 기억한다. nested 메시지의
`parent_tool_use_id`를 이 표에서 찾고 다음 정보를 모든 서브에이전트 이벤트에 넣는다.

```json
{
  "taskId": "task-a",
  "parentToolUseId": "tool-a",
  "requestId": "request-a",
  "subagentType": "Explore",
  "taskType": "local_agent",
  "taskDescription": "인증 흐름 조사"
}
```

여기서 Pie의 `requestId`와 Claude SDK 메시지의 `request_id`는 서로 다른 식별자다.
SDK 값은 보통 `req_...` 형식이며 Pie가 수락한 채팅 요청을 가리키지 않는다. Executor는
`task_started` 시점의 Pie 요청 ID를 별도로 고정하고, 이후 SDK 내부 ID가 바뀌더라도
덮어쓰지 않는다. Manager도 `taskId`와 `parentToolUseId`를 원래 요청과 연결해 두므로
메인 응답 종료 뒤 도착한 백그라운드 이벤트와 구버전 Executor 이벤트를 같은 요청으로
복구할 수 있다.

`requestId`는 특히 중요하다. 메인 답변이 먼저 끝나고 백그라운드 에이전트가 계속
실행되면 Manager의 현재 활성 요청은 비어 있을 수 있다. Executor가 최초 요청 ID를
계속 실어 보내고, Manager는 같은 대화 journal에 이미 존재하는 `request.accepted`만
검증해 귀속한다. 따라서 임의의 요청 ID를 주입할 수 없고, 새 대화가 시작되어도 과거
백그라운드 작업이 잘못된 메시지에 붙지 않는다.

## 4. 화면 표시 규칙

서브에이전트 한 개당 카드 한 개를 사용한다.

- 헤더: 에이전트 유형, 작업 설명, 실행 상태
- 진행 요약: SDK가 생성한 `task_progress.summary` 원문
- 본문: 서브에이전트 text를 Markdown으로 누적 렌더링
- 추론: 기본적으로 접힌 상세 영역
- 도구: 호출 ID별 input/result, 성공·실패, 실시간 실행 시간
- 하단: 토큰, 도구 횟수, 소요 시간, 최근 도구, 결과 파일

화면 카드의 안정적인 키는 `parentToolUseId`를 우선하고 `taskId`를 보조로 사용한다.
메인 턴 종료나 재연결 중 `requestId`가 잠시 비어도 같은 서브에이전트가 두 카드로
갈라지지 않는다. Read, Write, Edit, Bash를 포함한 서브에이전트 도구의 입력과 결과는
기본 펼침 상태로 표시하며 사용자가 필요하면 개별적으로 접을 수 있다.

텍스트와 thinking delta는 이벤트마다 React 전체 목록을 다시 그리지 않는다. Task별
버퍼에 모은 뒤 `requestAnimationFrame`당 한 번 반영한다. 이 방식은 실시간 느낌을
유지하면서 긴 응답이나 병렬 에이전트 실행 시 브라우저 렌더링 부하를 제한한다.

메인 응답의 `done`은 메인 턴만 종료한다. 백그라운드 카드까지 임의로 완료시키지 않는다.
이후 도착하는 `task_complete`가 해당 카드만 완료로 바꾼다. 반대로 Agent SDK query가
오류나 중단으로 끝나면 Executor가 남은 task를 `failed` 또는 `stopped`로 정리해 무한
로딩 카드가 남지 않게 한다.

## 5. 호환성과 장애 처리

- Relay와 clientd는 이벤트 원문을 보존하므로 새 이벤트 타입을 위해 별도 wire protocol
  버전을 만들지 않는다.
- Manager는 알 수 없는 정상 이벤트도 journal과 SSE에 그대로 저장·전달한다.
- `task_started`보다 nested 메시지가 먼저 도착해도 `parentToolUseId`를 임시 task ID로
  사용한다. 이후 task 정보가 도착하면 같은 카드가 갱신된다.
- 병렬 에이전트의 스트리밍 여부는 `parent_tool_use_id`별로 관리한다. 한 에이전트의
  partial message 때문에 다른 에이전트의 최종 fallback text가 누락되지 않는다.
- `task_updated`의 completed/failed/killed 상태와 stream 종료 시 transcript polling을
  모두 처리한다. 한 경로가 누락돼도 가능한 범위에서 카드를 종료한다.
- Claude Code의 격리된 셸 도구는 Executor 이미지의 `bubblewrap`과 `socat`에 의존한다.
  둘 중 하나가 빠지면 서브에이전트 본문은 전달되더라도 `Bash` 도구가 샌드박스 초기화
  오류로 제한될 수 있으므로 두 패키지를 이미지 빌드 시 함께 설치한다.
- Executor는 전체 root filesystem을 계속 읽기 전용으로 유지한다. 다만 subprocess 환경
  scrub이 임시 `.mcp.json`을 준비할 수 있도록 `/home`만 16 MiB tmpfs로 제공하고,
  사용자별 영속 HOME은 그 아래 `/home/executor` bind mount로 별도 유지한다.
- rootless bubblewrap은 내부 사용자·PID namespace와 전용 `/proc`를 만든다. 따라서 해당
  기능을 켠 Executor에만 Docker의 `seccomp=unconfined`와
  `systempaths=unconfined`를 함께 적용한다. 일반 컨테이너로 예외를 확대하지 않으며,
  Executor의 비-root UID, `cap-drop ALL`, `no-new-privileges`, 읽기 전용 root filesystem,
  private PID namespace, 자원 제한과 사용자별 네트워크 경계는 그대로 유지한다.

## 6. 검증 기준

1. Node Executor 옵션과 task/tool 매핑 단위 테스트
2. 서로 다른 `parent_tool_use_id`의 병렬 스트림 격리 테스트
3. Go clientd가 새 이벤트 원문을 손실 없이 파싱·전달하는 테스트
4. 메인 `done` 뒤에 온 서브에이전트 이벤트가 기존 요청 ID에 귀속되는 Manager 테스트
5. Web Chat TypeScript 검사, 전체 서버 테스트, Next.js 운영 빌드
6. 실제 Kroot 서버에서 Claude가 서브에이전트를 실행하도록 요청한 브라우저 E2E

회귀 테스트는 Claude SDK 내부 `req_...`가 Pie 요청 ID를 덮어쓰지 않는지, 메인
`done` 뒤의 출력이 기존 카드에 합쳐지는지, 완료된 도구 상세가 열린 상태인지도
검증한다.

운영 E2E에서는 최소한 `task_started → subagent_text/tool → task_complete` 순서,
서브에이전트 카드의 실시간 증가, 메인 답변과의 분리, 새로고침 뒤 journal replay 복원을
확인한다.

## 7. 운영 적용 및 검증 결과

2026-08-12 Kroot 테스트 서버의 실제 공개 경로에서 검증했다.

- Web Chat: `pie-third-party-web-chat:20260812-subagent-stream-v2`
- Manager: `pie-executor-manager:20260812-subagent-stream-v5`
- Executor: `pie-relay-client-kroot:20260812-subagent-stream-v3`
- Executor 격리 정책: `v5`

초기 E2E에서 서브에이전트 본문 스트리밍은 정상 동작했으나, Claude의 하위 프로세스
자격증명 scrub이 `bubblewrap` 내부 `/proc`를 만들 때 Docker system-path masking에
막히는 문제가 발견됐다. Executor 범위에만 `systempaths=unconfined`를 추가하고 Docker
inspect의 `MaskedPaths`·`ReadonlyPaths`를 기준으로 정책 드리프트를 판정하도록 고쳤다.
공유 구독 OAuth를 Bash·Hook·MCP에서 제거하는 scrub 자체는 끄지 않았다.

최종 브라우저 E2E 결과는 다음과 같다.

- `Explore` 서브에이전트의 실행 중 상태를 화면에서 관찰
- 실시간 DOM 갱신 12회
- 서브에이전트 카드 완료
- 카드 안의 `Bash` 도구 2건과 `pwd`, `pie-subagent-ok` 출력 확인
- 도구 오류 0건
- 종료된 대화의 자동 교체·Relay/clientd 자동 재연결·SSE 실시간 수신 복구 확인
- 공개 Web Chat, Manager, Relay health endpoint 모두 정상
