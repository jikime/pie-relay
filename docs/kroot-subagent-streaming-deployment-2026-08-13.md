# Kroot 서브에이전트 스트리밍 배포 기록

배포일: 2026-08-13  
대상: `chat-relay.cookai.dev`, `api-relay-test.cookai.dev`

## 반영 내용

- Claude SDK 내부 `request_id`가 Pie 채팅 요청 ID를 덮어쓰지 않도록 Executor의 요청 상관관계를 수정했다.
- Manager가 `taskId`와 `parentToolUseId`를 기준으로 백그라운드 서브에이전트 이벤트를 원래 사용자 요청에 연결하도록 보강했다.
- 웹채팅은 요청 ID가 사라진 뒤 도착한 이벤트도 같은 서브에이전트 카드에 병합한다.
- 서브에이전트의 Read, Write, Edit, Bash 등 도구 입력과 결과를 기본 펼침 상태로 표시한다. 사용자는 필요할 때 직접 접을 수 있다.
- 메인 에이전트의 도구 입력과 결과는 기존처럼 별도 카드에서 항상 보인다.

## 배포 이미지

- Manager: `pie-executor-manager:20260813-subagent-request-8d1f5e7`
- Executor: `pie-relay-client-kroot:20260813-subagent-request-8d1f5e7-3ba09c6`
- Web Chat: `pie-third-party-web-chat:20260813-subagent-request-8d1f5e7`

Kroot 바이너리는 이전 검증 이미지와 SHA-256이 같은 파일을 사용했다. Relay와 PostgreSQL은 이번 배포에서 교체하거나 재시작하지 않았다.

## 검증 결과

- Node Executor 단위·회귀 테스트: 87개 통과
- Client Go 테스트: 통과
- Manager Go 테스트: 통과
- Web Chat 테스트: 30개 통과, 선택적 PostgreSQL 테스트 1개 건너뜀
- Web Chat 정적 검사와 Next.js 운영 빌드: 통과
- 실제 원격 브라우저 E2E: 통과
  - Explore 서브에이전트의 실행 중 상태 관찰
  - 실시간 화면 갱신 12회 관찰
  - Bash 도구 2개 입력·결과 표시 확인
  - 도구 상세 2개 모두 기본 펼침 확인
  - 완료 상태와 원문 출력 확인
  - 동일 작업의 중복 카드 없음
  - 도구 오류 없음
- 실행 중 사용자 Executor 6개: 모두 새 이미지이며 `healthy`
- 각 Executor의 Claude/Kroot 인증 파일과 프로젝트 디렉터리 보존 확인
- 배포 종료 시 활성·대기 채팅 요청: 0건

상세 이벤트 규약과 장애 원인은 [Claude 서브에이전트 스트리밍](./claude-subagent-streaming.md)을 참고한다.
