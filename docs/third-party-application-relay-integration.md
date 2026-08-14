# 제3의 애플리케이션 ↔ Docker Claude Code 연동 설계

## 1. 목적과 범위

이 문서의 `제3의 애플리케이션`은 Pie Relay Desktop이나 Pie Relay Mobile이 아니라,
별도의 웹서비스·브라우저·네이티브 앱·업무 시스템을 뜻한다. 목표는 제3의
애플리케이션에서 인증된 사용자의 Docker Executor를 선택하고, 그 안의 Claude Code를
실행한 뒤 입출력을 Pie Relay를 통해 실시간으로 주고받는 것이다.

결론부터 말하면 이 구조는 가능하다. 다만 제3의 애플리케이션이 Pie의 내부 Desktop
프로토콜에 직접 의존하게 만들지 않고, 공개 Control API와 버전이 명시된 WebSocket
프로토콜 또는 SDK를 제공해야 한다.

## 2. 전체 연결 구조

```text
제3의 애플리케이션
  │
  │ 1. 외부 사용자 인증(PAT 또는 서비스 세션)
  ▼
Pie Manager / Third-party Control API
  │ 2. PAT introspection, 사용자·권한·대상 컨테이너 확인
  │ 3. session-scoped Relay 자격 발급
  ▼
제3의 앱 ── WSS ── Pie Relay ── WSS ── Docker clientd / Executor
                                               │
                                               ├─ 사용자 workspace
                                               ├─ ~/.claude
                                               ├─ ~/.kroot/credential.json
                                               └─ Claude Code PTY 실행
```

각 구성요소의 책임은 다음과 같다.

| 구성요소 | 책임 |
|---|---|
| 제3의 애플리케이션 | 세션 생성·참가, 사용자 입력, 출력 렌더링, 재연결 |
| 제3의 앱 Backend/BFF | 외부 PAT 보호, Control API 호출, 브라우저에 단기 자격 전달 |
| Pie Manager | PAT 검증, 사용자→Executor 할당, 세션·권한·감사, Relay 자격 발급 |
| Pie Relay | 검증된 Host와 Participant의 양방향 프레임 중계 |
| Docker `clientd`/Executor | PTY와 Claude Code 실행, 출력 journal, 재접속 시 세션 복구 |

브라우저가 외부 PAT를 장기간 보관하는 구조는 피한다. 브라우저 기반 제3의 앱은
Backend/BFF가 PAT를 검증한 뒤 특정 `session_id`에만 유효한 Relay 자격을 브라우저에
전달하는 방식을 기본으로 한다.

## 3. 공개 API의 기준 형태

최소한 다음 인터페이스가 필요하다. 실제 URL과 필드 이름은 구현 시 OpenAPI로
고정하고 버전을 올려 변경한다.

```text
POST   /v1/integrations/sessions
GET    /v1/integrations/sessions/{sessionId}
POST   /v1/integrations/sessions/{sessionId}/credential
POST   /v1/integrations/sessions/{sessionId}/cancel
DELETE /v1/integrations/sessions/{sessionId}
WSS    /v1/integrations/sessions/{sessionId}/stream
```

세션 생성 요청은 `owner`, `targetDeviceId`, `executionTarget=docker`, workspace와 실행
정책을 포함한다. 요청 body의 사용자 ID를 인증 근거로 사용하지 않고, PAT
introspection 결과의 `sub`를 실제 사용자로 사용한다.

WebSocket 메시지는 최소 다음 범주를 지원한다.

```text
app → executor
  session_attach, start, stdin, resize, interrupt, cancel, ack

executor → app
  session_attached, stdout, stderr, status, prompt, exit, error, heartbeat
```

모든 실행 요청에는 `requestId`, 모든 출력에는 단조 증가하는 `seq`를 넣는다. 앱이
재접속할 때 마지막으로 처리한 `seq`를 cursor로 제출하면 Executor가 누락 출력을
재전송한다. 네트워크 재연결 과정에서 같은 `start`가 다시 전달되어도 `requestId`로
중복 실행을 막아야 한다.

## 4. 단기 Relay 토큰이 연결을 끊는가

### 4.1 결론

단기 Relay 토큰의 만료와 Claude 프로세스 수명을 분리하면 문제가 없다.

```text
Relay 토큰 수명       접속하거나 접속 권한을 갱신할 수 있는 기간
WebSocket 연결 수명   실제 네트워크 연결이 유지되는 기간
Pie 작업 세션 수명    사용자·컨테이너·권한 할당이 유지되는 기간
Claude 프로세스 수명  완료, 취소 또는 작업 정책에 의해 종료될 때까지
```

토큰이 만료됐다는 이유만으로 Docker 컨테이너나 Claude Code 프로세스를 종료해서는 안
된다. 토큰은 Relay 접속 권한이고, 실행 중인 작업의 생명주기 그 자체가 아니다.

### 4.2 현재 저장소의 실제 동작

현재 Go Relay는 Host와 Participant의 WebSocket upgrade 전에 JWT의 서명·`exp`·scope를
검증한다. 검증이 성공해 WebSocket이 만들어진 뒤에는 연결 중 JWT를 다시 검사하거나
`exp` 시각에 socket을 자동 종료하지 않는다. 따라서 연결된 상태에서 토큰 시간이
지났다고 즉시 Claude 통신이 끊기지는 않는다.

반대로 네트워크 단절이나 Relay 재시작 후 만료된 JWT로 다시 접속하면 handshake가
`401`로 거절된다. 이때 제3의 앱은 기존 PAT 또는 서비스 세션으로 Manager에서 새
session-scoped JWT를 발급받고 같은 `session_id`와 cursor로 재접속해야 한다.

현재 Control Plane의 기본 발급값은 Participant 1시간, Host 24시간이며 Host는 최대
24시간, Participant는 최대 12시간으로 제한한다. 현재 방식은 정상 연결의 불필요한
종료를 막지만, 연결 중 즉시 권한 회수와 무손실 output resume까지 완성한 것은 아니다.
제3의 애플리케이션에 제공하기 전에 아래 목표 수명주기를 구현해야 한다.

## 5. 권장 인증·연결 수명주기

### 5.1 정상 접속

1. 제3의 앱 Backend/BFF가 외부 PAT 또는 서비스 세션으로 Manager를 호출한다.
2. Manager가 PAT introspection과 대상 Docker 소유권·Grant를 확인한다.
3. Manager가 `room + device_id + session_id + role + access`에 한정된 Relay JWT를
   발급한다.
4. 제3의 앱이 JWT로 Relay에 접속한다.
5. Docker Executor는 별도의 Host 자격으로 같은 세션에 접속한다.
6. 앱은 `session_attach(lastAckSeq)`를 보내고 출력 stream을 이어받는다.
7. Claude Code는 WebSocket이 잠깐 끊겨도 Executor 안에서 계속 실행한다.

### 5.2 만료 전 갱신

클라이언트는 `expiresAt`을 보관하고 TTL의 약 60~70%가 지났거나 만료 2~5분 전에
Manager로 새 자격을 요청한다. 운영 구현은 다음 두 방식 중 하나를 지원해야 한다.

1. **동일 socket 재인증**: `auth_refresh` 메시지로 새 JWT를 보내고 Relay가 scope가
   동일하거나 더 좁은지 검증한 뒤 연결 lease를 갱신한다.
2. **무중단 socket 교체**: 새 JWT로 두 번째 연결을 만들고
   `session_attached(lastAckSeq)`를 받은 뒤 기존 연결을 닫는다.

동일 socket 재인증이 사용자 경험은 가장 단순하다. 초기 구현에서는 출력 journal과
cursor를 갖춘 무중단 교체도 충분히 안전하다. 어떤 방식을 택하든 새 자격 발급 실패를
Claude 프로세스 즉시 종료로 연결하지 않는다.

### 5.3 네트워크 단절과 만료가 겹친 경우

```text
네트워크 단절
  ├─ 기존 JWT가 유효함 → 즉시 재접속
  └─ 기존 JWT가 만료됨
       ├─ Manager 인증 유효 → 새 JWT 발급 → 같은 session/cursor로 재접속
       └─ Manager 인증 만료 → AUTH_REFRESH_REQUIRED 표시
```

Manager 인증까지 만료되면 사용자의 입력 권한은 중지하되, Claude 작업은 정책에 따라
일정한 detach grace 동안 보존한다. 사용자가 재인증하면 같은 세션에 다시 attach하고
누락 출력을 replay한다. grace가 끝나면 작업을 취소할지 계속 백그라운드 실행할지는
세션 생성 시 정책으로 명확히 정한다.

### 5.4 권장 초기 운영값

아래 값은 구현 계약의 시작점이며 부하·보안·UX 테스트 후 조정한다.

| 항목 | 권장 시작점 |
|---|---|
| Participant Relay JWT | 15~60분 |
| Host/Executor Relay JWT | 24시간, 만료 전에 자동 교체 |
| 갱신 시작 | TTL의 60~70% 또는 만료 2~5분 전 |
| clock skew 허용 | 30~60초, 모든 노드 NTP 동기화 |
| 앱 detach grace | 15~30분 |
| 출력 replay 보존 | 시간과 byte 상한을 함께 적용한 bounded journal |
| 완료 세션 metadata 보존 | 감사·정책에 따라 별도 설정 |

짧은 토큰을 1~2분으로 지나치게 설정하면 Control API와 인증 시스템 장애가 곧
재연결 장애로 확대된다. 초기 운영에서는 15~60분 범위와 선제 갱신, bounded grace를
함께 사용하는 것이 현실적이다.

## 6. 인증 정보의 분리

| 자격 | 용도 | Relay frame 포함 여부 |
|---|---|---|
| 외부 서비스 PAT | 제3의 사용자와 조직을 Manager가 확인 | 포함하지 않음 |
| Relay Session JWT | 특정 room/device/session/role/access 연결 | WebSocket handshake 또는 재인증에만 사용 |
| Pie Device Credential | Executor 장치의 heartbeat와 세션 할당 | 일반 터미널 frame에 포함하지 않음 |
| Kroot PAT | 컨테이너가 Kroot 외부 시스템을 호출 | 사용자별 `~/.kroot/credential.json`에서 사용 |
| Claude 인증 | 컨테이너의 Claude Code 실행 | Relay로 전달하지 않음 |

Relay JWT에는 원본 PAT를 넣지 않는다. 필요한 claim은 최소한 다음과 같다.

```text
iss, aud=pie-relay, sub, org(optional), room,
device_id, session_id, execution_target=docker,
role, access, cap, jti, iat, nbf, exp
```

제3의 앱 사용자인 `actor`와 컨테이너의 Kroot credential 소유자인 `executionOwner`가
다를 수 있다. 감사 로그에는 두 정체성을 구분해 남긴다. Kroot 외부 시스템이 실제로
인식하는 사용자는 기본적으로 컨테이너의 `~/.kroot/credential.json` 소유자다.

## 7. 장애별 기대 동작

| 상황 | 기대 동작 |
|---|---|
| 연결 중 Relay JWT 만료 | 갱신 또는 socket 교체, Claude 계속 실행 |
| 만료 후 네트워크 재연결 | Manager에서 새 JWT 발급 후 cursor attach |
| Manager 일시 장애 | 기존 연결 유지, backoff 갱신 재시도, grace 표시 |
| Relay 재시작 | Executor의 Claude 프로세스 유지, 양쪽 재연결과 출력 replay |
| 외부 PAT 만료 | 신규 JWT 발급 중지, 재인증 요구, 정책상 grace 적용 |
| Grant/PAT 강제 폐기 | 새 발급 차단 및 Control Plane이 기존 Relay 연결도 명시적으로 종료 |
| 브라우저 새로고침 | 새 JWT 발급, 같은 session과 cursor로 attach |
| 동일 start 요청 재전송 | `requestId`로 중복 Claude 실행 차단 |
| 출력 소비가 느린 앱 | bounded queue, ack/cursor, slow-consumer 정책 적용 |
| 컨테이너 재시작 | 별도 process checkpoint 정책이 없다면 실행은 실패 처리하되 상태를 명확히 보고 |

보안을 위해 “기존 socket은 토큰 만료 후 무조건 영원히 허용”해서는 안 된다. Manager가
Grant 폐기, 사용자 정지 또는 세션 취소 이벤트를 Relay에 전달해 해당 connection을
종료할 수 있어야 한다. 정상적인 짧은 만료는 선제 갱신으로 처리하고, 명시적인 폐기는
즉시 차단하는 두 경로를 구분한다.

## 8. 구현 순서

1. Third-party Control API와 OpenAPI 계약을 정의한다.
2. 외부 PAT introspection 결과로 대상 사용자·Docker·Grant를 결정한다.
3. third-party participant용 scoped JWT 발급 endpoint를 추가한다.
4. 버전이 있는 WebSocket message schema와 SDK를 만든다.
5. Executor에 `requestId` idempotency와 `seq/ack/cursor` output journal을 구현한다.
6. 클라이언트의 선제 credential 갱신과 backoff 재연결을 구현한다.
7. 동일 socket `auth_refresh` 또는 무중단 socket handover를 구현한다.
8. Control Plane의 session revoke → Relay connection close 경로를 구현한다.
9. actor/executionOwner 감사와 secret redaction을 추가한다.
10. 장시간 Claude 작업과 Relay/Manager 장애 E2E를 통과시킨다.

## 9. 필수 E2E 검증

1. Relay JWT TTL보다 오래 실행되는 Claude 작업이 중단되지 않는가.
2. 만료 전에 자격이 교체돼 사용자가 끊김을 느끼지 않는가.
3. 토큰 만료와 Wi-Fi/셀룰러 단절이 겹쳐도 새 JWT로 같은 세션에 복귀하는가.
4. 재접속 전후 stdout/stderr에 유실·중복·순서 역전이 없는가.
5. `start` 재전송이 Claude 프로세스를 두 번 만들지 않는가.
6. Manager가 잠시 중단돼도 기존 연결과 Claude 작업은 유지되는가.
7. Relay 재시작 후 양쪽이 재연결하고 journal cursor부터 복구하는가.
8. PAT 또는 Grant 폐기 시 새 연결뿐 아니라 기존 control 연결도 차단되는가.
9. Viewer 토큰으로 stdin, cancel, resize 같은 제어 메시지를 보낼 수 없는가.
10. 로그, URL, analytics, crash report에 PAT와 Relay JWT가 노출되지 않는가.

## 10. 현재 상태와 완료 조건

현재 저장소에는 scoped Relay JWT, Host/Participant 역할, Docker 실행 대상 scope,
WebSocket keepalive와 Control Plane 자격 발급 기반이 있다. 그러나 제3의 애플리케이션용
공개 API/SDK, 연결 중 credential 갱신, cursor 기반 출력 replay와 즉시 revoke 연동은
아직 별도의 제품 기능으로 완성해야 한다.

따라서 “단기 토큰 때문에 연결 자체가 불가능하다”는 문제가 있는 것은 아니다. 실제
제품 완성 조건은 토큰 수명과 작업 수명을 분리하고, 새 자격 발급·재접속·출력 replay를
하나의 수명주기로 구현하는 것이다.
