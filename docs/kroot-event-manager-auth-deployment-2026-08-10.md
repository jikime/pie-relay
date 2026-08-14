# Kroot Event Manager 인증 배포·E2E 결과 — 2026-08-10

> 이 문서는 당시의 `.credentials.json` 배포 방식을 기록한 과거 배포 보고서다. 현재
> 운영 기준은 해당 파일을 사용자 컨테이너에 복사하지 않는 중앙 `claude setup-token`
> Broker이며, 최신 절차는 [Event Manager Claude 인증](event-manager-claude-auth.md)을
> 따른다.

이 문서는 Kroot 테스트 서버에 Event Manager 기반 Claude 인증 배포 기능을 적용하고,
실제 공개 웹 채팅에서 사용자 가입부터 Claude Code 응답과 사용량 저장까지 검증한
결과를 기록한다. 토큰·비밀번호·인증 파일 원문은 이 문서와 배포 로그에 남기지 않는다.

## 1. 배포 대상

| 구분 | 값 |
|---|---|
| 서버 | Kroot 테스트 서버 `221.143.48.77:2733` |
| 릴리스 | `20260810-event-auth-91fa37d7af11` |
| Manager | `pie-executor-manager:91fa37d7af11` |
| Web Chat | `pie-third-party-web-chat:91fa37d7af11` |
| Executor | `pie-relay-client-kroot:91fa37d7af11` |
| Relay | 기존 `pie-relay-server:e9134a0f654f` 유지 |
| PostgreSQL | 기존 `postgres:16-alpine` 유지 |

이번 배포는 Manager와 Web Chat만 교체했다. Relay와 PostgreSQL은 재생성하지 않아
기존 연결과 데이터를 보존했다. Executor는 사용자 요청 시 현재 이미지로 조정된다.

## 2. 운영 설정

- `PIE_CLAUDE_AUTH_REQUIRED=true`: 활성 인증이 없거나 사용자별 배포가 실패하면
  Executor 실행을 차단한다.
- `PIE_CLAUDE_AUTH_ROLLOUT_CONCURRENCY=2`: 실행 중 Executor 재검증·재시작을 동시에
  두 개까지만 수행한다.
- `PIE_EXECUTOR_KROOT_AUTO_LINK=false`: 사용자별 Relay 연결은 채팅 세션 수명주기에서
  발급한 자격으로 수행한다.
- 인증 후보는 Event Manager 서버에서 `claude auth login`으로 생성한다. 개발자 PC의
  인증 파일을 운영 서버로 복사하지 않는다.
- 인증 파일은 이미지나 환경변수에 넣지 않고 Manager 데이터 영역의 버전 저장소와
  사용자별 HOME에 `0600`으로 보관한다.

서버 데이터 루트가 `0700/root`인 환경에서도 로그인 도구가 상위 디렉터리 권한을
완화하지 않도록 보강했다. 도구는 데이터 루트만 제한적으로 마운트한 일회성 helper로
로그인 하위 디렉터리를 만들고 Executor UID/GID에 소유권을 부여한다.

## 3. 인증 배포 결과

Event Manager 서버에서 새 로그인을 완료한 뒤 다음 버전을 발행했다.

| 항목 | 결과 |
|---|---|
| 활성 버전 | `v-20260810T063531.467761930Z-9b72738e3de6` |
| 레이블 | `kroot-server-login-20260810` |
| 직전 버전 | `v-20260810T063017.064625263Z-0911df38a11d` |
| 전체 대상 | 14 |
| 현재 버전 배포 | 14 |
| 실행 중 컨테이너 검증 | 1 |
| 누락·구버전·실패 | 0 |

중지된 사용자의 상태가 `deployed`인 것은 정상이다. 해당 Executor가 다음 요청으로
시작될 때 `claude auth status` 검증을 거쳐 `verified`로 바뀐다.

단순 `claude auth status`만으로는 실제 요청 시점의 만료 상태를 모두 발견하지 못할 수
있었다. 따라서 운영 인증 교체의 최종 통과 기준은 반드시 짧은 실제 Claude 응답 E2E로
한다.

## 4. 공개 경로 E2E 결과

다음 실제 경로를 새 테스트 사용자로 검증했다.

```text
공개 Web Chat 회원가입
  → PostgreSQL 사용자 저장
  → Manager 사용자 프로비저닝
  → 사용자 전용 Docker Executor 생성
  → 프로젝트 생성 및 작업 경로 지정
  → Relay 세션 연결
  → Claude Code 실제 실행
  → 도구·텍스트·완료·사용량 이벤트 스트리밍
  → Web Chat 응답 및 사용량 화면 반영
```

검증 결과는 다음과 같다.

- 사용자별 Executor는 `10001:10001`, 읽기 전용 root filesystem, 메모리 4 GiB,
  CPU 1 core, PID 256 제한으로 기동했고 health와 재시작 횟수가 정상이다.
- 컨테이너 안에서 `claude auth status`, `kroot --help`, Claude/Kroot/Pie 상태 파일을
  모두 확인했다. 파일 원문은 출력하지 않았다.
- 실제 Claude 텍스트, raw Bash 도구 입력·결과, Markdown, 완료 마커 필터링이
  브라우저 SSE 경로에서 통과했다.
- 스트림 중복 전송 방지와 대화 교체·재연결도 통과했다.
- 테스트 대화는 자동 정리했으며, 재검증할 수 있도록 테스트 사용자와 Executor는
  남겨 두었다.

## 5. 사용량 저장 검증

실제 E2E 두 차례의 사용량이 Manager DB에 저장되고 Web Chat의 사용자 전용 API에서
조회되었다.

| 항목 | 결과 |
|---|---:|
| 대화 턴 | 2 |
| 모델 | `claude-sonnet-4-6` |
| 총 토큰 | 145,971 |
| 저장 당시 계산 비용 | USD 0.3790524 |
| 사용량 이벤트 | 2 |

비용은 이벤트 발생 시점의 가격 스냅샷으로 저장한다. 나중에 모델 가격이 변경되어도
과거 비용을 다시 계산하여 바꾸지 않는다. 사용자 화면은 K/M 단위로 보기 좋게 표시하고,
상세 API와 DB에는 원래 정수 토큰을 유지한다.

## 6. 공개 상태 점검

다음 공개 상태 경로는 배포 후 모두 HTTP 200을 반환해야 한다.

```text
https://relay.cookai.dev/readyz
https://api-relay.cookai.dev/readyz
https://chat-relay.cookai.dev/api/health
https://admin-relay.cookai.dev/admin/
```

Manager, Web Chat, Relay, PostgreSQL과 테스트 Executor는 모두 healthy이며 이번 교체 후
Manager, Web Chat, Executor의 재시작 횟수는 0이었다. 최근 로그에서 panic, fatal,
인증 만료 및 권한 오류가 없는 것도 확인했다.

로컬 회귀 테스트도 함께 통과했다.

- Executor Manager: 전체 Go package 통과
- Web Chat: 28개 통과, PostgreSQL 선택 테스트 1개 skip
- Node Executor: 72개 통과

## 7. 백업과 복구

배포 전 백업은 서버와 로컬에 각각 보관한다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260810T061505Z-pre-event-manager-auth
/Users/jikime/Dev/Private/cli-relay-backups/20260810T061505Z-pre-event-manager-auth
```

두 복사본 모두 `SHA256SUMS` 검증을 통과했다. 백업에는 환경 설정, Compose, 소스,
PostgreSQL custom dump, Manager 데이터, 이미지·컨테이너 inspect가 들어 있다.

배포와 E2E가 모두 끝난 시점의 사후 백업도 서버와 로컬에 각각 보관한다.

```text
/home/kaonkroot/pie-sandbox-test/backups/20260810T064339Z-post-event-manager-auth
/Users/jikime/Dev/Private/cli-relay-backups/20260810T064339Z-post-event-manager-auth
```

사후 백업은 약 959MiB이며 두 위치 모두 체크섬과 민감 파일 `0600` 권한을 확인했다.

장애 시에는 다음 순서로 복구한다.

1. 현재 요청 유입을 중지하거나 신규 세션 발급을 잠시 차단한다.
2. `previous` 릴리스의 환경과 이미지 태그를 확인한다.
3. Manager와 Web Chat만 직전 이미지로 되돌린다.
4. 스키마 호환 문제가 있으면 PostgreSQL dump와 데이터 백업을 별도 복원 환경에서
   먼저 검증한다.
5. Claude 인증 자체만 문제라면 전체 릴리스 대신 관리 화면의 `이전 버전 복구`를
   사용한다.
6. 실제 Claude 응답 E2E와 사용량 저장까지 확인한 뒤 요청을 다시 받는다.

인증 파일, 관리자 토큰, 테스트 계정 비밀번호는 Git과 문서에 저장하지 않는다.

## 8. 재인증 운영 절차

운영 인증을 갱신할 때에는 [Event Manager 기반 Claude 인증 운영](./event-manager-claude-auth.md)의
절차를 따른다. 핵심 순서는 다음과 같다.

1. 서버에서 Executor와 동일한 이미지·UID로 `claude auth login`을 수행한다.
2. 후보의 파일 형식·권한과 `claude auth status`를 확인한다.
3. Manager에서 새 불변 버전을 발행하고 전체 사용자에게 제한된 동시성으로 배포한다.
4. 실행 중 Executor의 검증 상태를 확인한다.
5. 공개 Web Chat에서 짧은 실제 Claude 요청과 사용량 저장을 확인한다.

일회용 로그인 코드는 입력 직후 폐기하며 셸 히스토리, 명령 인자, CI 변수, 문서 또는
애플리케이션 로그에 남기지 않는다.

## 9. 사용자별 연결 상태 보강 배포

같은 날 후속 점검에서 Web Chat이 Relay 세션, Docker clientd, 브라우저 SSE 상태를 하나의
연결 상태처럼 표현하고 있음을 확인했다. 특히 Conversation이 15분 유휴 종료된 뒤에도
Executor 컨테이너는 1시간까지 실행될 수 있어 `Runtime 실행 중 + clientd 연결 없음`이
정상적으로 발생하지만, 화면에서는 다른 사용자 연결 때문에 끊긴 것처럼 보일 수 있었다.

Manager는 Conversation 조회 응답에 안전한 파생 `connection` 상태와 표준 사유를 제공하고,
Web Chat은 Pie Relay·Docker clientd·실시간 스트림을 각각 표시하도록 보강했다. SSE가
절전이나 네트워크 전환 중 이벤트를 놓쳐도 연결 직후, 15초 heartbeat, 오류 시점마다
Manager snapshot을 다시 받아 오래된 상태를 자동 교정한다.

후속 릴리스는 다음과 같다.

```text
/home/kaonkroot/pie-sandbox-test/releases/20260810-connection-state-714ca2f7fd9b
```

Manager와 Web Chat만 무중단 순서로 교체했고 Relay·PostgreSQL·활성 Executor는 재시작하지
않았다. 서로 다른 사용자 8명의 활성 Conversation에서 Owner·Device·Session·Routing Key가
모두 고유하고, Relay room·host·participant가 각각 8개로 일치하며 eviction·reject·presence
drop이 모두 0인 것을 확인했다. 공개 API E2E와 실제 브라우저 Claude Code 스트리밍 E2E도
통과했다.

세부 상태 정의와 장애 점검 절차는
[Relay·clientd 연결 격리와 복구 운영 가이드](./relay-client-connection-isolation-and-recovery.md)를
따른다.
