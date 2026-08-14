# Kroot 원격 Monaco 편집기 배포·E2E 결과 — 2026-08-13

## 1. 배포 범위

Kroot 검증 서버 `221.143.48.77:2733`의 Kroot Studio 스택에 사용자 프로젝트 원격
편집 기능을 배포했다. 사용자 작업공간은 기존 `/backup/pie-sandbox-test/executor-data`
bind mount를 그대로 사용했으며 PostgreSQL, Relay, Preview Gateway와 사용자 파일은
교체하지 않았다.

| 구성 요소 | 배포 이미지 |
|---|---|
| Manager | `pie-executor-manager:20260813-workspace-editor-32f42c3` |
| Executor | `pie-relay-client-kroot:20260813-workspace-editor-32f42c3` |
| Web Chat | `pie-third-party-web-chat:20260813-workspace-editor-c5cf3b5` |
| Relay | 기존 `pie-relay-server:20260812-token-context-v3` 유지 |
| PostgreSQL | 기존 `postgres:16-alpine` 유지 |

Executor는 기존 검증 이미지 `pie-relay-client-kroot:20260812-write-bypass-v6`의 Kroot
바이너리만 가져오는 오버레이로 만들었다. 배포 전후 `/usr/local/bin/kroot` SHA-256이
동일함을 확인했으며 PAT와 Claude 자격증명은 이미지 레이어에 넣지 않았다.

## 2. 적용 기능과 보안 경계

공개 경로는 다음과 같다.

```text
Browser Monaco
  → chat-relay.cookai.dev BFF
  → api-relay-test.cookai.dev Manager
  → relay-test.cookai.dev
  → 사용자 Docker clientd
  → /workspace/projects/{server-assigned-id}
```

- 프로젝트 파일 트리 조회, UTF-8 텍스트 읽기와 저장
- SHA-256 revision 기반 낙관적 잠금과 HTTP 409 충돌 응답
- 임시 파일·`fsync`·rename을 이용한 원자적 저장
- 2MiB 파일 및 폴더당 500개 항목 제한
- `.kroot`, `.claude`, `.pie`, `.ssh`, `.env*`, `node_modules` 등 보호 경로 차단
- 절대 경로, `..`, 심볼릭 링크 탈출과 바이너리 파일 차단
- 사용자·Integration·프로젝트·대화 소유권의 BFF/Manager 이중 검증
- 소스 본문을 Manager durable journal에 저장하지 않고 요청 메타데이터만 보존

## 3. 배포 절차

1. 활성 대화 두 건에서 실행 중인 Claude turn이 없음을 확인했다.
2. 환경 설정, Compose, 소스, PostgreSQL dump, Manager 상태와 이미지 inspect를 백업했다.
3. 커밋 `32f42c3`을 불변 릴리스 디렉터리로 전송했다.
4. Executor, Manager와 Web Chat 이미지를 먼저 빌드하고 이미지 내부 파일과 Kroot 해시를 검증했다.
5. 새 Manager를 먼저 교체해 구 Web Chat과의 호환 상태를 확인했다.
6. Web Chat을 교체한 뒤 실행 중 Executor를 한 개씩 `runtime.recreate` operation으로 교체했다.
7. 각 Executor가 새 이미지와 `healthy`, 재시작 횟수 0인지 확인했다.
8. 공개 Claude 대화, 원격 파일 API와 실제 Chrome Monaco 화면을 검증했다.

`runtime.recreate`는 컨테이너만 교체한다. `/workspace`, `/home/executor`, 첨부파일 bind
mount는 유지되므로 프로젝트와 사용자 상태는 삭제되지 않는다.

## 4. 배포 중 발견한 SSR 회귀와 방지책

첫 배포 직후 `/api/health`는 200이었지만 실제 `/` 화면은 `window is not defined`로
HTTP 500을 반환했다. 원인은 `'use client'` 컴포넌트도 Next.js 초기 HTML 생성 과정에서
모듈이 서버 평가될 수 있는데, 정적 import된 `monaco-editor`가 평가 시점에 브라우저
전역을 참조한 것이었다.

커밋 `c5cf3b5`에서 Monaco 모듈 전체를 Next.js `dynamic(..., { ssr: false })` 경계
뒤로 이동했다. 또한 Web Chat 컨테이너 healthcheck가 `/api/health`와 `/`를 모두
요청하도록 바꿨다. 앞으로 API만 정상이고 화면 SSR이 500인 이미지는 `healthy`가 되지
않는다.

애플리케이션 직접 응답은 nonce, `strict-dynamic`, `worker-src 'self' blob:`을 포함한
CSP를 만든다. 공유 Traefik entrypoint의 공통 보안 헤더는 이 값을 최소 공통 CSP로
교체한다. 현재 공통 CSP에는 `default-src`/`worker-src` 제한이 없어 Monaco Worker를
차단하지 않으며 실제 Chrome 렌더링도 통과했다. 여러 공용 서비스를 함께 쓰는 Edge의
CSP 통합은 영향 범위가 크므로 별도 공통 보안 변경으로 관리한다.

## 5. 검증 결과

| 검증 | 결과 |
|---|---|
| Manager 전체 Go package | 통과 |
| Node Executor | 86개 통과 |
| Web Chat | 28개 통과, 선택적 PostgreSQL 1개 skip |
| Next.js type check·production build | 통과 |
| 공개 Relay·Manager·Web Chat 상태 경로 | 모두 HTTP 200 |
| 실제 Claude Code 왕복 | text·usage·Relay/clientd 연결 통과 |
| 원격 파일 | tree·read·write·409·restore 통과 |
| 보호 경로 | `.kroot/credential.json` 접근 차단 |
| Headless Chrome | 코드 탭, 파일 트리, Monaco 19줄 렌더링, 화면 오류 0 |
| 실행 중 사용자 Executor | 새 이미지, healthy, restart 0 |

파일 저장 E2E는 기존 `package.json`에 공백 한 글자를 추가하고, 오래된 revision 저장이
409인지 확인한 다음 원래 내용과 원래 SHA-256 revision으로 복원했다. 시험 대화는 닫고
E2E로만 기동된 Executor runtime은 중지하여 배포 전 실행 자원 수로 되돌렸다.

## 6. 백업과 롤백

배포 전 백업:

```text
/home/kaonkroot/pie-sandbox-test/backups/20260813T022804Z-pre-workspace-editor-32f42c3
```

배포 후 백업:

```text
/home/kaonkroot/pie-sandbox-test/backups/20260813T024312Z-post-workspace-editor-c5cf3b5
```

두 백업은 파일별 `SHA256SUMS` 검증을 통과했다. 사용자 프로젝트가 있는 `/backup`은
배포 중 수정·이동하지 않았다. 롤백할 때에는 신규 turn 발급을 잠시 막고 실행 중 turn이
없는지 확인한 뒤, 사전 백업의 `.env`와 Compose를 사용해 Web Chat과 Manager를 이전
이미지로 되돌린다. 실행 중 Executor는 Control Plane의 `runtime.recreate` operation으로
한 개씩 이전 이미지에 맞추며 Docker volume이나 `/backup` 디렉터리를 직접 삭제하지
않는다.
