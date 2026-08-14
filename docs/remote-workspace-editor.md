# 원격 프로젝트 파일 편집기

## 목적

사용자 전용 Executor 컨테이너에서 Claude Code가 만든 프로젝트 파일을 Web Chat에서
직접 확인하고 수정한다. 브라우저가 Docker 볼륨이나 Manager의 Docker socket에 직접
접근하지 않고, 기존의 인증된 연결을 다음과 같이 재사용한다.

```text
Web Chat Monaco Editor
  → Web Chat BFF (브라우저 세션·CSRF·사용자 소유권)
  → Executor Manager (Integration·사용자·프로젝트·대화 소유권)
  → Pie Relay의 해당 대화 세션
  → 사용자 전용 clientd / Node Executor
  → /workspace/projects/{opaque-project-id}
```

파일 작업은 Claude 채팅 턴과 별개의 `workspace` 제어 메시지다. 따라서 파일 목록을
읽거나 저장해도 Claude 요청 슬롯을 차지하지 않으며, 파일 작업 실패가 진행 중인 AI
대화를 종료하지도 않는다.

## 사용자 기능

- 프로젝트별 지연 로딩 파일 탐색기
- 여러 파일 탭과 Monaco Editor 문법 모드
- 수정 여부 표시와 저장하지 않은 탭 종료 경고
- `Cmd+S` 또는 `Ctrl+S` 저장
- Claude Code 작업 완료 후 파일 목록 자동 새로고침
- SHA-256 revision 기반 낙관적 잠금
- 외부 변경 충돌 시 자동 덮어쓰기 대신 최신 파일 재조회 안내

현재 편집 대상은 UTF-8 텍스트 파일이며 파일당 최대 크기는 2MiB다. 바이너리 파일과
대규모 생성물은 다운로드·전용 아티팩트 기능으로 분리하는 것이 안전하다.

## API

Web Chat BFF는 로그인 쿠키를 받고 Manager Integration token은 서버 내부에서만
사용한다.

```text
GET /api/projects/{projectId}/workspace/tree?conversationId={id}&path={relativePath}
GET /api/projects/{projectId}/workspace/file?conversationId={id}&path={relativePath}
PUT /api/projects/{projectId}/workspace/file
```

저장 요청 예시는 다음과 같다.

```json
{
  "conversationId": "conv-...",
  "path": "src/app.ts",
  "content": "export const app = true\n",
  "baseRevision": "sha256:...",
  "clientRequestId": "browser-generated-uuid"
}
```

Manager의 대응 Integration API는
`/v1/integrations/{integration}/users/{externalUser}/projects/{project}/workspace/*`다.
Manager는 브라우저가 보낸 절대 경로를 신뢰하지 않고 Control Plane에 저장된
`Project.WorkingDir`를 Executor 요청에 넣는다. 선택한 Conversation도 같은 Integration
사용자와 같은 Project에 속하고 종료되지 않은 상태여야 한다.

## 보안 경계

Node Executor에서 마지막으로 다음 항목을 검증한다.

1. `/workspace/projects` 바로 아래의 서버 발급 프로젝트만 루트로 허용한다.
2. 절대 경로, `..`, 역슬래시, 제어문자와 지나치게 긴 경로를 거부한다.
3. 실제 경로를 확인해 프로젝트 바깥으로 나가는 심볼릭 링크를 거부한다.
4. 심볼릭 링크의 실제 목적지도 다시 검사해 보호 폴더 우회를 막는다.
5. `.claude`, `.kroot`, `.pie`, `.ssh`, `.aws`, `.env*`, `secrets`는 숨기고 직접
   요청도 거부한다.
6. `.git`, `node_modules`, `.next`, `dist`, `build`, 캐시와 coverage는 탐색기에서
   제외한다.
7. 일반 파일만 `O_NOFOLLOW`로 열고 NUL이 포함된 파일과 잘못된 UTF-8을 거부한다.
8. 임시 파일 쓰기, `fsync`, 같은 폴더 내 rename, 폴더 `fsync` 순서로 저장한다.

파일 응답은 `workspace_result` 이벤트를 사용한다. 일반 `error` 이벤트는 Claude 채팅
턴의 종료 의미가 있으므로 파일 오류에 사용하지 않는다. 공개 SSE 재생에서는 파일
내용을 제거하고 작업 종류와 성공 여부만 내보낸다. Manager의 private chat journal도
파일 원문이나 저장 요청 본문은 보관하지 않고 경로·작업·revision·크기 같은 복구
메타데이터만 기록한다. 따라서 큰 파일을 반복해서 열어도 대화 journal 용량이 파일
원문 때문에 빠르게 소진되지 않는다.

## 충돌 처리

파일을 열 때 내용의 SHA-256을 revision으로 받는다. 저장할 때 같은 revision을
`baseRevision`으로 보내며, 현재 파일 해시와 다르면 HTTP 409를 반환한다. 이 경우 UI는
사용자의 내용을 그대로 보존하면서 최신 파일을 다시 불러오도록 안내한다.

이 방식은 다음 두 작업이 서로의 내용을 조용히 덮어쓰는 일을 막는다.

- 사용자가 Monaco에서 편집하는 동안 Claude Code가 같은 파일을 변경
- 같은 사용자가 여러 브라우저 탭에서 같은 파일을 편집

실시간 공동 편집이 필요해질 때에만 별도의 문서·협업 프로토콜을 추가한다. 현재는
소스 파일을 단일 진실 원본으로 유지하는 낙관적 잠금이 장애 복구와 Git 사용에 더
단순하고 안전하다.

## 검증 항목

- Node Workspace 단위 테스트: 경로 탈출, 보호 파일, 심볼릭 링크, 바이너리, 원자적
  저장, stale revision 충돌
- Gateway 테스트: Relay 왕복, requestId 귀속, 중복 요청 결과 복구
- Web Chat BFF 테스트: 로그인·CSRF, 프로젝트/Conversation 일치, 다른 사용자 대화
  차단, 경로 검증, 저장 충돌
- Next.js 운영 빌드: 로컬 Monaco 번들과 언어별 Web Worker, CSP `worker-src`
- 실제 Docker E2E: 격리된 clientd를 공개 Relay 주소로 연결한 뒤 파일 조회·수정·원복,
  중복 저장, 409 충돌, 인증 경로 및 다른 Integration 접근 차단, journal 원문 비저장

로컬 Docker E2E에서도 Manager의 `controlAddress`는 내부 제어망을 사용하지만 격리된
Executor는 Relay의 공개 주소를 사용한다. Executor network의 ICC 차단을 해제해서
테스트를 통과시키면 운영 격리 조건과 달라지므로 그렇게 해서는 안 된다.

운영 배포 후에는 별도 테스트 프로젝트의 작은 파일을 열고 수정·저장·원복한 뒤,
`.kroot/credential.json` 요청이 403이고 다른 사용자의 Conversation이 404인지 다시
확인한다.
