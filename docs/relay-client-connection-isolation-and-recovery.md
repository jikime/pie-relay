# Relay·clientd 연결 격리와 복구 운영 가이드

> 작성일: 2026-08-10
> 대상: Web Chat → Manager → Pie Relay → 사용자 전용 Docker clientd → Claude Code

## 1. 결론

한 사용자의 clientd가 Relay에 접속한다고 다른 사용자의 clientd가 끊기는 구조가 아니다.
각 연결은 사용자 전용 Conversation과 Device·Session으로 분리되고, Relay에서는 다음
Routing Key로 식별한다.

```text
room + deviceId + sessionId + relayGeneration
```

Relay가 기존 연결을 교체하는 경우는 **동일한 Routing Key로 새 연결이 들어온 때**뿐이다.
서로 다른 사용자·컨테이너·Conversation은 서로 다른 Key를 사용하므로 독립적으로 동시에
연결할 수 있다.

사용자 간 격리와 별개로, 같은 Conversation을 재시도하면 이전 세대의 연결은 의도적으로
종료되고 새 `relayGeneration`으로 교체된다. 이는 오래된 토큰이나 지연된 패킷이 새 세션에
개입하지 못하게 하는 fencing 동작이다.

## 2. 화면에서 구분해야 할 세 가지 상태

기존 화면은 여러 연결 상태를 하나의 `연결됨`으로 표현해 원인 파악이 어려웠다. 현재는
다음 세 상태를 각각 표시한다.

| 화면 표시 | 의미 | 정상 조건 |
|---|---|---|
| Pie Relay | Manager가 발급한 Conversation Relay 세션 | Relay Node가 준비되어 있고 Session이 연결 가능한 상태 |
| Docker clientd | 사용자 전용 Executor의 clientd 연결 | host peer가 Relay에 등록되고 heartbeat가 살아 있음 |
| 실시간 스트림 | 브라우저와 Web Chat BFF 사이의 SSE | EventSource가 열려 있고 현재 Conversation 이벤트를 수신 중 |

세 상태는 서로 독립적이다. 예를 들어 Relay와 clientd가 정상이어도 사용자의 노트북이
절전 상태가 되면 브라우저 실시간 스트림만 끊길 수 있다. 반대로 브라우저 스트림이
정상이어도 clientd가 재기동 중이면 채팅 전송은 잠시 비활성화된다.

## 3. Manager가 제공하는 연결 상태

Conversation 조회 응답의 `connection`에는 브라우저에 공개해도 안전한 최소 상태만 담는다.

```json
{
  "relayAvailable": true,
  "runtimeRunning": true,
  "runtimeHealthy": true,
  "clientConnected": true,
  "relayRegistered": true,
  "sessionStatus": "ready",
  "reason": "connected",
  "lastError": "",
  "lastHeartbeat": "2026-08-10T08:30:00Z"
}
```

Relay 내부 Node ID, Routing Key, 자격증명과 토큰은 브라우저 응답에 노출하지 않는다.
화면은 단순한 Conversation `ready` 값만 믿지 않고 이 파생 상태를 함께 확인한 뒤 전송을
허용한다.

`reason`의 대표 값은 다음과 같다.

| reason | 의미 | 기본 대응 |
|---|---|---|
| `connected` | Relay·Runtime·clientd가 모두 정상 | 전송 허용 |
| `connecting` | 초기 연결 정보가 아직 모이지 않음 | 잠시 대기하며 자동 갱신 |
| `session_starting` | 세션 생성 또는 재기동 중 | 자동 재시도 대기 |
| `client_reconnecting` | clientd가 Relay 재접속 중 | 자동 복구를 기다리고 장기화 시 로그 확인 |
| `client_offline` | Runtime은 있지만 clientd heartbeat가 없음 | 컨테이너·clientd 로그 확인 또는 다시 연결 |
| `relay_unregistered` | clientd는 보이나 Relay host 등록이 없음 | JWT 세대와 Relay join 확인 |
| `relay_unavailable` | 사용 가능한 Relay Node가 없음 | Relay `/readyz`와 presence 확인 |
| `runtime_stopped` | 사용자 Executor가 중지됨 | 요청 시 cold-start 또는 운영자 재기동 |
| `runtime_unhealthy` | 컨테이너 health check 실패 | 컨테이너 로그·자원·인증 확인 |
| `idle_timeout` | Conversation이 유휴 시간 초과로 정상 종료 | `다시 연결`로 같은 대화 복구 |
| `closed` / `deleted` | 명시적 종료 또는 삭제 | 필요하면 새 대화 생성 |

## 4. 컨테이너가 실행 중인데 clientd가 끊겨 보이는 이유

이 상태는 반드시 장애를 뜻하지 않는다. 현재 수명주기는 두 단계다.

```text
마지막 채팅 활동 후 15분
  → Conversation Relay 세션과 Claude/clientd 세션 종료

활성 세션이 없는 상태로 1시간
  → 사용자 전용 Executor 컨테이너 정지
```

따라서 15분과 1시간 사이에는 다음 상태가 정상적으로 존재한다.

```text
Docker Runtime: 실행 중
Docker clientd: Conversation 연결 없음
Conversation: idle_timeout 또는 closed
```

이 상태에서 다른 사용자의 연결 때문에 끊겼다고 판단하면 안 된다. 화면의 사유와
Conversation 종료 시각을 확인하고 `다시 연결`을 실행하면 동일 Project·Conversation
journal을 유지한 채 새 Relay 세션을 만든다.

## 5. 브라우저가 오래된 상태를 보지 않도록 하는 복구 방식

브라우저 절전, Wi-Fi 전환 또는 프록시 timeout 때문에 SSE가 끊기는 동안
`conversation.idle` 이벤트를 놓칠 수 있다. 이전 화면은 SSE 오류 때 로그인 상태만
확인하여 이미 닫힌 Conversation을 계속 `연결됨`처럼 보일 수 있었다.

현재 화면은 다음 시점에 Manager의 Conversation 상태를 다시 조회한다.

- SSE 연결이 열렸을 때
- 15초 heartbeat 주기
- SSE 오류 또는 재연결 때
- `transport.connected`, `transport.reconnecting`, `conversation.idle` 이벤트를 받았을 때

서버 snapshot이 `closed`, `idle_timeout`, `deleted`이면 오래된 EventSource를 닫고
전송을 막는다. 네트워크가 돌아오면 최신 snapshot을 반영하므로 새로고침에 의존하지 않는다.

## 6. 장애 점검 순서

사용자가 `Pie Relay는 연결되었지만 Docker clientd가 연결되지 않는다`고 보고하면 다음
순서로 확인한다.

1. Conversation의 `connection.reason`과 `sessionStatus`를 확인한다.
2. `idle_timeout`이면 정상 유휴 종료인지 마지막 활동 시각을 확인한다.
3. Executor가 `runtimeRunning`, `runtimeHealthy`인지 확인한다.
4. Relay의 host·participant 수와 해당 Session의 presence를 확인한다.
5. clientd 로그에서 인증 실패, generation 불일치, reconnect backoff를 확인한다.
6. 같은 사용자의 동일 Conversation이 중복 기동되었는지 확인한다.
7. 서로 다른 사용자의 Routing Key가 중복되지 않는지 확인한다.

운영 지표에서는 다음 항목을 함께 본다.

```text
pie_relay_connections
pie_relay_rooms
pie_relay_hosts
pie_relay_participants
pie_relay_slow_peer_evicted_total
pie_relay_room_rejected_total
pie_relay_presence_dropped_total
```

정상적인 사용자별 1:1 채팅이라면 활성 Conversation 수만큼 room·host·participant가
각각 증가한다. `slow_peer_evicted`, `room_rejected`, `presence_dropped`가 증가하지 않는데
특정 세션만 닫혔다면 Relay 전체 장애보다 그 Conversation의 유휴 종료나 clientd 상태를
먼저 확인하는 편이 정확하다.

### 배포 프로필 변경 뒤 `no relay node capacity available`가 반복될 때

2026-08-12 Kroot Studio와 Vibe Canvas의 애플리케이션·풀을 분리한 뒤, 이미 만들어진
Kroot 채팅 세션 하나가 이전 `pie-control` / `pie-relay-sandbox` 범위를 계속 가지고
있었다. 현재 Kroot Relay는 `kroot-studio` / `kroot-studio` 범위만 제공하므로 서버와
컨테이너가 모두 정상이어도 스케줄러가 호환 노드를 찾지 못했다. 이 경우 화면에는 Relay
`세션 준비 중`, Docker clientd `연결 중`이 반복되고 Manager에는 다음 오류가 남는다.

```text
no relay node capacity available
```

Manager는 이제 다음 조건을 모두 만족할 때만 대기 중 세션을 현재 기본 Relay 범위로
자동 재바인딩한다.

- Integration이 소유한 아직 종료되지 않은 Conversation이다.
- 실행 대상이 사용자 전용 Docker이고 모드는 `chat` 또는 `acp`다.
- 세션이 `active`가 아니며 기존 범위에 정상 Relay가 없다.
- 새 기본 애플리케이션·풀에는 실제로 정상 Relay가 있다.

복구 시 Relay generation을 증가시키고 이전 노드·host connection·driver lease를
비운 뒤 새 노드에 다시 배정한다. Host OS 직접 연결, 일반 공유 터미널, 활성 세션과 종료
기록은 자동 변경하지 않는다. 따라서 제품 분리 정책을 유지하면서 오래된 관리형 채팅만
복구할 수 있다.

복구 뒤 아래 값이 함께 맞아야 실제 연결 성공이다.

```text
Session: active + 현재 application/pool + 현재 relayNodeId
Conversation: ready
Relay: connections=2, rooms=1, hosts=1, participants=1
```

메시지가 `request.accepted → ... → done`까지 흐른 뒤
`Failed to authenticate: OAuth session expired...`가 반환된다면 Relay나 clientd 장애가
아니다. 요청은 Docker 안의 Claude Code까지 도착한 것이므로 Event Manager의 Claude
인증 버전을 새로 발행·검증해야 한다.

## 7. Kroot 테스트 서버 검증 결과

2026-08-10 Kroot 테스트 서버에서 Manager와 Web Chat을 다음 릴리스로 교체했다.

```text
/home/kaonkroot/pie-sandbox-test/releases/20260810-connection-state-714ca2f7fd9b
```

검증 시점에 서로 다른 소유자 8명의 활성 Conversation이 동시에 연결되었다.

| 검증 항목 | 결과 |
|---|---:|
| 활성 Conversation | 8 |
| 정상 연결 | 8 |
| 고유 Owner | 8 |
| 고유 Device | 8 |
| 고유 Session | 8 |
| 고유 Routing Key | 8 |
| Relay connection | 16 |
| Relay room / host / participant | 각 8 |
| slow peer eviction / room rejection / presence drop | 각 0 |

공개 경로 E2E도 다음 전체 경로를 통과했다.

```text
공개 Web Chat BFF
  → Manager API
  → Pie Relay
  → 원격 Docker clientd
  → 실제 Claude Code
  → 텍스트·도구 입력/결과·Markdown·사용량 SSE 반환
```

브라우저 E2E에서는 SSE를 의도적으로 차단하여 전송 버튼이 비활성화되는지 확인하고,
스트림 복구 후 실제 Claude 응답과 도구 이벤트가 다시 표시되는 것까지 검증했다.

## 8. 남은 운영 보강 항목

Web Chat의 브라우저 로그인 Session Store는 현재 프로세스 메모리 기반이다. Web Chat
컨테이너가 재시작되면 Relay나 사용자 Executor가 끊기는 것은 아니지만 브라우저 사용자는
다시 로그인해야 한다. 다중 Web Chat replica와 무중단 로그인까지 제공하려면 다음 배포
단계에서 PostgreSQL 또는 Redis 기반 공유 세션 저장소, 서명 키 rotation, 세션 폐기 정책을
추가해야 한다.

clientd의 정상 context 종료 로그도 `재접속`이 아니라 `Relay 세션 종료`로 구분하도록
소스에 반영했다. 이 문구 변경은 다음 Executor 이미지 교체 때 적용하며, 기존 사용자
Executor를 로그 문구만을 위해 강제 재시작하지 않는다.
