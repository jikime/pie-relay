# Pie Relay 세션·실행 환경·Control Plane 정의

이 문서는 Pie Relay에서 반복해서 혼동되기 쉬운 `실행 환경`, `접속 경로`,
`세션 발급 방식`을 서로 분리해 정의한다. Desktop 화면을 해석하거나 고객용 사용
매뉴얼을 작성할 때 이 문서를 기준으로 사용한다.

> 기준: 2026-07-26 현재 저장소 구현. 이 문서에서 `Local`은 반드시 무엇이
> 로컬인지 함께 표기한다. 예를 들어 `Host OS`, `로컬 Relay`, `Direct LAN`은
> 서로 다른 의미다.

## 1. 한 문장으로 정리

Pie Relay 연결은 다음 세 질문에 각각 답해야 정확하게 설명할 수 있다.

1. **명령은 어디에서 실행되는가?** — Host OS 또는 Docker 컨테이너
2. **터미널 데이터는 어떤 길로 이동하는가?** — Pie Relay 또는 Direct LAN
3. **사용자와 권한을 확인하고 세션 JWT를 누가 발급하는가?** — 단독 Relay 또는
   Pie Control Plane

세 답은 서로 독립적이다. 예를 들어 `Docker + Pie Relay + Pie Control Plane`은
정상적인 조합이다. Relay 서버가 같은 PC에서 실행 중이어도 실행 환경이 Docker라면
화면에는 `DOCKER`로 표시해야 한다.

```text
실행 환경       Host OS | Docker
접속 경로       Pie Relay | Direct LAN
인증·세션 관리  단독 Relay | Pie Control Plane
```

## 2. 세 가지 축의 정확한 정의

### 2.1 실행 환경: 실제 명령이 실행되는 곳

| 값 | 실제 Host | 파일·프로세스 권한 | 화면 표시 |
|---|---|---|---|
| `local` | PC Host OS의 `clientd` | `clientd`를 실행한 OS 사용자 | `HOST OS` / `이 PC · Host OS` 또는 `장치명 · Host OS` |
| `docker` | 컨테이너 내부 `clientd` | 컨테이너 사용자와 마운트된 전용 volume | `DOCKER` / `Docker 컨테이너` |

`local`은 “Relay 서버가 로컬에 있다”는 뜻이 아니다. Docker가 내 PC에 설치되어
있다는 사실도 현재 세션을 `local`로 만들지 않는다. 실행 중인 `clientd`가 Host OS에
있으면 `local`, 컨테이너 안에 있으면 `docker`다.

실행 환경의 기준 정보는 Control Plane 세션의 `executionTarget`과 발급 JWT의
`execution_target` claim이다. Relay URL이나 Docker 설치 여부로 추측하지 않는다.

### 2.2 접속 경로: 데이터가 이동하는 길

| 값 | 데이터 경로 | 현재 주 사용 범위 |
|---|---|---|
| Pie Relay | UI ↔ Relay ↔ `clientd`/Gateway | Desktop/CLI 원격 터미널, 외부망 모바일 |
| Direct LAN | 모바일 ↔ Desktop Host Gateway | 같은 Wi-Fi의 모바일 제어 |

`Relay · 이 PC` 표시는 Relay 프로세스가 현재 PC에서 실행된다는 뜻이다. 실제 명령이
Host OS에서 실행되는지 Docker에서 실행되는지는 이 표시만으로 판단할 수 없다.

현재 일반 Desktop/CLI 터미널 세션은 Relay 경로를 사용한다. 모바일은 Desktop Host
Gateway를 대상으로 `LAN`, `Relay`, 또는 자동 선택을 사용할 수 있다. 모바일 Gateway
연결과 일반 `clientd` 방 연결은 프로토콜과 수명주기가 다른 별도 경로다.

### 2.3 인증·세션 관리: 자격을 발급하는 주체

| 방식 | 사용자 인증 | 세션·권한 기록 | Relay 자격 발급 | 용도 |
|---|---|---|---|---|
| 단독 Relay | `HOST_ENROLL_SECRET` 또는 개발용 loopback 예외 | Relay 메모리 중심 | Relay가 직접 Host JWT 발급 | 로컬 개발·단독 설치 |
| Pie Control Plane | 외부 서비스 PAT 또는 운영 토큰 | 사용자·장치·세션·Grant를 중앙 관리 | Control Plane이 scoped JWT 발급 | 운영·다중 사용자 |

이 선택은 실행 환경 선택이 아니다. `Pie Control Plane`을 선택했다고 Docker가
자동 생성되는 것도 아니며, `단독 Relay`라고 반드시 Host OS에서 실행되는 것도 아니다.

UI 명칭도 이 의미가 드러나도록 장기적으로는 `세션 발급 방식`보다
`인증·세션 관리 방식`을 사용하는 것이 정확하다.

## 3. 구성요소와 역할

| 구성요소 | 역할 |
|---|---|
| Pie Relay | 인증된 Host와 Participant WebSocket을 매칭하고 터미널 프레임을 중계하는 Data Plane |
| `clientd` | Host OS 또는 Docker 안에서 Executor/PTY를 실제 실행하는 Host 데몬 |
| Pie Relay Desktop | 다른 세션에 참가하는 UI이며, 필요하면 별도의 Host OS `clientd`와 모바일 Gateway도 실행 |
| Pie Relay Mobile | Desktop Host Gateway와 페어링해 PC 터미널을 표시·조작 |
| Desktop Host Gateway | 모바일 E2EE/LAN/Relay 연결을 종단하고 PC 터미널을 제공 |
| Pie Control Plane | 사용자, 장치, 세션, Grant, 정책과 scoped Relay JWT를 관리 |
| Executor Manager | 현재 Control Plane API를 제공하며 Docker Executor 생성·복구·수명주기도 관리 |

Desktop 앱은 동시에 여러 역할을 가질 수 있다. 다른 Docker Executor 세션에는
Participant로 접속하면서, 별도로 자기 PC의 Host OS `clientd`를 Host로 실행할 수 있다.
두 연결은 같은 것이 아니다.

## 4. Pie Control Plane의 세션 발급 흐름

### 4.1 인증

이 절의 PAT는 Pie Control Plane에 사용자를 인증하는 **Pie 사용자 PAT**를 뜻한다.
사용자별 Docker Executor 안에서 Kroot 외부 시스템을 호출하는 **Kroot PAT**와는
용도와 저장 위치가 다르다. 컨테이너 인증 프로비저닝 기준은
[사용자별 Executor 컨테이너·인증 프로비저닝 설계](./executor-container-auth-provisioning.md)를
따른다.

사용자는 외부 회원 서비스에서 발급받은 PAT를 Desktop에 입력한다. Desktop은 PAT를
HTTP `Authorization: Bearer` 헤더로 Control Plane에만 전달한다.

운영 환경에서 Control Plane은 `PIE_AUTH_INTROSPECTION_URL`을 통해 PAT의 활성 상태와
`sub`, 조직, 역할을 확인한다. 동일 PAT의 반복 검증은 짧게 캐시하고, 캐시 키에는
원문이 아니라 SHA-256 해시를 사용한다. 로컬 개발에서는
`PIE_EXECUTOR_MANAGER_TOKEN` 정적 토큰을 사용할 수 있다.

Pie 사용자 PAT 원문은 다음 위치로 전달하거나 저장하지 않는다.

- Pie Relay WebSocket과 Relay payload
- 초대 코드와 QR
- Docker 이미지
- Desktop `localStorage`

Pie 사용자 PAT는 Control Plane 요청을 위한 사용자 인증 수단이고, Pie Relay 접속에는 별도로 발급된
짧은 범위의 JWT를 사용한다. 현재 `이 PC · Host OS` 장치 에이전트는 Control Plane을
outbound polling하기 위해 Desktop이 전달한 PAT를 **자식 프로세스 환경 변수로만**
보유한다. 명령행, 로그, PTY 환경, Relay frame에는 노출하지 않는다. 운영에서 PAT 수명을
길게 가져갈 경우에는 후속 단계에서 장치별 폐기 가능한 agent credential로 교환하는
방식을 적용한다.

### 4.2 대상 장치를 선택하는 관리형 세션 생성

운영 사용 흐름은 Desktop의
`새 연결 → 내 장치 · 공유받은 장치 → 새 작업 세션`이다. 사용자는 세션을 만들기 전에
실제 실행 대상을 선택한다.

| 선택 항목 | Control Plane 장치 | 실행 주체 |
|---|---|---|
| `이 PC · Host OS` | `kind=local`인 현재 Desktop 장치 | Desktop이 실행한 loopback `pie-client start` |
| `다른 PC · Host OS` | 대상 PC가 등록한 `kind=local` 장치 | 대상 PC에서 이미 실행 중인 `pie-client start` |
| `장치명 · Docker` | `kind=docker`인 Executor 장치 | Executor Manager가 관리하는 컨테이너 내부 `clientd` |

Host OS 장치의 전체 흐름은 다음과 같다.

```text
Desktop + PAT
  │
  ├─ GET /v1/control/devices
  │    소유한 Host OS/Docker 장치 목록 조회
  │
  ├─ 이 PC를 선택했다면 Tauri가 pie-client start 시작
  │       ├─ POST /v1/control/devices/register
  │       └─ POST /v1/control/devices/{deviceId}/heartbeat
  │
  ├─ POST /v1/control/sessions
  │    선택한 deviceId, device.kind와 같은 executionTarget,
  │    private/shared, transportMode=relay, status=starting
  │
  ├─ 대상 Host OS clientd의 outbound reconcile (기본 2초)
  │       ├─ GET /v1/control/devices/{deviceId}/sessions
  │       ├─ POST /v1/control/sessions/{sessionId}/credential
  │       ├─ loopback Session Manager가 PTY 시작
  │       └─ POST .../sessions/{sessionId}/status (ready/error)
  │
  ├─ Relay presence가 세션을 active로 확정
  │
  └─ Desktop이 scoped Host JWT를 발급받아 Relay 참가
```

Host OS 장치 에이전트는 Control Plane으로 outbound HTTP(S) 요청만 보낸다. Session
Manager는 `127.0.0.1:19091`에만 바인딩되므로 대상 PC에 관리 포트를 공개하거나 NAT,
방화벽 inbound 규칙을 추가하지 않는다. 등록 직후 첫 heartbeat를 즉시 보내며, Desktop은
동적 장치·세션 조회에 HTTP 캐시를 사용하지 않는다.

Docker를 선택하면 같은 세션 생성 API를 사용하지만 Host OS feed 대신 Executor
Manager의 Controller가 컨테이너 수명주기와 내부 Session Manager를 조정한다. Device의
`kind`와 Session의 `executionTarget`이 다르면 Control Plane이 요청을 거절한다.

일반 사용자가 요청하면 Control Plane은 요청 body의 `ownerUserId`를 신뢰하지 않고
인증된 PAT의 `sub`를 소유자로 강제한다. Host 자격은 세션 소유자 또는 운영 권한이
있는 사용자만 발급받을 수 있다.

현재 발급되는 Host JWT에는 최소한 다음 scope 정보가 포함된다.

```text
sub, room, device_id, session_id, execution_target,
role=host, access=control, cap, jti, iat, nbf, exp
```

공유 세션의 Host에는 초대 생성 capability가 추가될 수 있다. JWT 서명 키는 Control
Plane 발급자와 Relay 검증자가 일치해야 한다. Relay는 JWT를 검증한 뒤 동일한
`room + device_id + session_id` 범위에서만 연결을 매칭한다.

### 4.3 기존 세션 참가

Desktop의 `새 연결 → 내 장치·공유받은 장치`에서는 다음 흐름을 사용한다.

1. PAT로 `/v1/control/me`, `/devices`, `/sessions`, `/grants`를 조회한다.
2. Control Plane은 소유하거나 유효한 Grant가 있는 세션만 반환한다.
3. 사용자가 세션과 `view` 또는 `control`을 선택한다.
4. `/v1/control/sessions/{sessionId}/credential`로 Participant JWT를 요청한다.
5. 현재 Desktop은 소유자에게 Host 자격을, 공유 사용자에게 Grant 범위 안의
   Participant 자격을 발급한다.
6. Desktop은 응답의 공개 Relay URL과 JWT로 Relay에 접속한다.

Desktop이 요청하는 Participant JWT의 기본 TTL은 1시간이며, Host JWT는 24시간이다.
권한을 회수한 뒤에는 기존 연결을 종료하고 후속 자격 발급을 차단해야 즉시 회수가
완성된다.

### 4.4 Control Plane과 Relay의 책임 경계

```text
Control Plane                          Relay
──────────────────────────────────    ─────────────────────────────
PAT 검증                               scoped JWT 검증
사용자·조직 확인                       Host/Participant 연결 매칭
장치·세션 소유권                       room/session 프레임 격리
view/control Grant                     단일 Driver lease 강제
scoped JWT 발급                        PTY 입력·출력 실시간 중계
수명주기·감사·운영 상태                heartbeat/roster/flow control
```

Control Plane은 터미널 내용을 중계하지 않는다. Pie Relay도 Kroot PAT를 해석하거나
사용자별 Docker 컨테이너를 생성하지 않는다. Kroot PAT introspection은 Kroot 인증
서버와 Kroot Relay의 별도 책임이다.

## 5. 토큰과 코드 구분

| 이름 | 누가 사용 | 목적 | 비고 |
|---|---|---|---|
| Pie 사용자 PAT | 사용자 → Control Plane | Pie 사용자 본인 확인 | Pie Relay/clientd의 장기 자격으로 사용하지 않음 |
| Kroot PAT | 사용자별 Executor → Kroot 인증·API·Relay | Kroot 실제 사용자 확인 | 사용자별 `~/.kroot/credential.json`에 저장하고 Kroot가 introspection |
| Host JWT | `clientd`, 소유자 Desktop | Host 연결과 관리 | 장치·세션 scope 포함 |
| 초대 코드 | 초대받은 사용자 | Participant JWT 교환 | Relay의 `/rooms/join`에 제출 |
| Participant JWT | Desktop/CLI 참가자 | `view` 또는 `control` 접속 | 해당 세션에만 유효 |
| 모바일 pairing/resume 자격 | Mobile ↔ Desktop Gateway/Relay | 모바일 기기 페어링·재접속 | 일반 방 Host JWT와 별개 |

`호스트 토큰` 입력란에는 초대 코드를 넣지 않는다. 호스트 토큰은 긴 JWT이고, 사람이
입력하는 짧은 코드는 참가자 초대 코드다.

## 6. 현재 Desktop 화면을 읽는 법

### 6.1 현재 작업 세션

화면 상단의 `현재 작업 세션`은 실제로 열려 있는 세션의 정보를 표시한다.

- `DOCKER`: 컨테이너 내부 `clientd`가 실제 Host
- `HOST OS`: PC에서 직접 실행 중인 `clientd`가 실제 Host
- `Relay · 이 PC`: Relay 서버가 이 PC에 있음
- `Relay · 원격`: Relay 서버가 다른 서버에 있음

예를 들어 다음 표시는 모순이 아니다.

```text
실행 환경  Docker 컨테이너
접속 경로  Relay · 이 PC
실제 Host  컨테이너 내부 clientd
```

명령은 Docker에서 실행되고, 그 데이터가 같은 PC에서 실행 중인 Relay를 거쳐 Desktop에
도착한다는 뜻이다.

### 6.2 이 PC 직접 호스트

이 영역은 **현재 작업 세션의 설정 화면이 아니다**. macOS Host OS에서 직접 실행할
별도의 `clientd`를 만들고 관리하는 단독 Host 화면이다. UI에는
`이 PC 직접 호스트 · 단독 Relay · Host OS`로 표시한다.

현재 구현은 이 영역에서 다음 값을 고정한다.

```text
executionTarget = local
transportMode    = relay
```

따라서 이 화면에서 세션을 만들면 기존 Docker 세션을 가져오거나 전환하지 않고 별도의
Host OS 세션을 생성한다. 다른 PC 또는 Docker를 고르는 범용 경로가 아니다.

### 6.3 새 작업 세션의 실행 대상 선택

다른 PC와 Docker를 포함한 범용 생성 경로는
`새 연결 → 내 장치 · 공유받은 장치 → 새 작업 세션`이다.

- 현재 Desktop 장치는 목록 맨 위의 `이 PC · Host OS`로 표시하고 기본 선택한다.
- 다른 Host OS 장치는 실제 장치 이름과 온라인/오프라인 상태를 표시한다.
- Docker Executor는 `장치명 · Docker`로 표시한다.
- 오프라인인 다른 Host OS에는 세션을 할당하지 않고 대상 PC에서 agent를 실행하라는
  오류를 보여준다.
- Docker가 내 PC에 설치되어 있다는 이유로 Host OS 세션을 Docker로 바꾸지 않는다.
- 생성 완료 후 Desktop은 선택된 세션의 `executionTarget`, `deviceId`, `sessionId`를
  유지하며 터미널 탭과 상태 바에 `HOST OS` 또는 `DOCKER`를 표시한다.

## 7. 대표 조합

### 내 PC에 직접 접속

```text
실행 환경       Host OS
접속 경로       Pie Relay
인증·세션 관리  단독 Relay 또는 Pie Control Plane
실제 Host       PC에서 실행 중인 clientd
```

### 내 PC의 사용자 전용 Docker에 접속

```text
실행 환경       Docker
접속 경로       Pie Relay
인증·세션 관리  Pie Control Plane
실제 Host       컨테이너 내부 clientd
프로비저닝      Executor Manager
```

### 같은 Wi-Fi에서 모바일로 PC 제어

```text
실행 대상       Desktop Host Gateway가 제공하는 PC 터미널
접속 경로       Direct LAN
페어링          모바일 QR/device/resume 자격
필수 프로세스   Desktop Host Gateway
```

### 외부망에서 모바일로 PC 제어

```text
실행 대상       Desktop Host Gateway가 제공하는 PC 터미널
접속 경로       Pie Relay
페어링          모바일 QR/device/resume 자격
필수 프로세스   Desktop Host Gateway + Relay
```

## 8. 구현 및 UI 판정 원칙

1. `HOST OS`/`DOCKER` 표시는 선택된 세션의 `executionTarget`을 사용한다.
2. Relay URL이 loopback인지 여부는 접속 경로의 위치만 설명한다.
3. Docker 설치 여부나 컨테이너 존재만으로 현재 세션을 Docker라고 표시하지 않는다.
4. Host 패널의 로컬 sidecar 상태와 현재 열린 원격/Docker 세션 상태를 합치지 않는다.
5. 연결 카드에는 `실행 환경`, `접속 경로`, `실제 Host`, `연결 상태`를 따로 표시한다.
6. 알 수 없는 실행 환경을 임의로 `Host OS`로 내리지 않고 `원격 실행 환경` 또는
   `확인 필요`로 표시한다.
7. PAT, Host JWT, 초대 코드, Participant JWT를 같은 입력값처럼 취급하지 않는다.
8. 운영에서는 Control Plane이 사용자·권한의 source of truth이고, Relay presence와
   실제 `clientd` 연결을 합성해 온라인 상태를 판단한다.

## 9. 현재 결론

- `local-user / DOCKER`처럼 표시된 세션은 Manager가 관리하는 Docker Executor
  세션이며, 컨테이너 내부 `clientd`를 Relay를 통해 사용한다.
- `별도 내 PC 데몬 설정`은 그 Docker 세션의 설정이 아니라 별도의 Host OS
  `clientd` 설정이다.
- `Pie Control Plane`은 Docker 전환 버튼이 아니라 PAT 기반 사용자 인증,
  소유권·Grant 확인과 scoped Relay JWT 발급 방식이다.
- 여러 사용자가 운영 환경에서 Local/Docker 세션을 공유하려면 Control Plane을
  권한 source of truth로 두고 Relay는 실시간 Data Plane으로 유지한다.

관련 문서:

- [Pie Relay 연결 구조와 사용 흐름](./how-to-connect.md)
- [세션 모드, Docker 격리 및 상호 접속 설계](./session-modes-and-mutual-access.md)
- [Pie Relay Control Plane 및 관리 콘솔 설계](./control-plane-and-admin-console.md)
- [Relay 인증 및 페어링](./relay-authentication.md)
