# Relay URL 설정

Pie Relay의 공식 Relay 선택 설정은 `PIE_RELAY_URL` 하나다. Azure용 변수와 로컬용
변수를 따로 두지 않는다. 실행 환경을 바꿀 때는 같은 변수의 값만 바꾼다.

## 기본값과 전환

`PIE_RELAY_URL`을 설정하지 않으면 다음 CookAI 공식 Relay를 사용한다.

```text
https://relay.cookai.dev
```

로컬 Relay를 사용할 때는 같은 변수에 로컬 주소를 넣는다.

```bash
PIE_RELAY_URL=http://127.0.0.1:13412 go run ./client/cmd/client
```

다른 PC나 실제 모바일 기기에서 로컬 Relay에 접속하려면 loopback 대신 Relay PC의
LAN 주소를 사용한다.

```bash
PIE_RELAY_URL=http://192.168.0.42:13412 go run ./client/cmd/client
```

운영 Relay를 바꿀 때도 변수 이름은 그대로다.

```bash
PIE_RELAY_URL=https://relay.cookai.dev go run ./client/cmd/client
```

HTTP(S) origin과 WS(S) 주소를 모두 받을 수 있다. 각 클라이언트는 입력 주소를 자신의
용도에 맞게 `/ws/agent`, participant WebSocket 또는 HTTP origin으로 정규화한다.

## 구성요소별 사용

### Desktop

패키징된 Tauri 앱과 Vite 개발 서버 모두 `PIE_RELAY_URL`을 사용한다.

```bash
# CookAI 공식 기본값
npm --prefix desktop run tauri dev

# 로컬 Relay
PIE_RELAY_URL=http://127.0.0.1:13412 npm --prefix desktop run tauri dev

# 브라우저 UI 개발도 동일한 변수 사용
PIE_RELAY_URL=http://127.0.0.1:13412 npm --prefix desktop run dev
```

Vite에도 이 변수 하나만 명시적으로 주입한다. Azure·Local 전용 변수는 두지 않는다.

### clientd

```bash
RELAY_TICKET='<host JWT>' PIE_RELAY_URL=http://127.0.0.1:13412 \
  go run ./client/cmd/client
```

일회성 실행에서는 `--relay-url`로 같은 값을 덮어쓸 수 있다. 플래그를 생략하면
`PIE_RELAY_URL`, 그마저 없으면 CookAI 공식 기본값을 사용한다.

### Executor Manager

Docker 세션을 생성하는 Manager도 같은 `PIE_RELAY_URL`을 사용한다. 값을 생략하면
CookAI Relay의 `/ws/agent`로 정규화하며, 로컬 통합 환경의 Compose는 이 변수에
`ws://relay:13412/ws/agent`를 넣는다.

### Mobile Gateway와 모바일 앱

독립 실행 Gateway도 `PIE_RELAY_URL` 하나를 읽는다.

```bash
cd pie-mobile/adapter/host-gateway
PIE_RELAY_URL=http://192.168.0.42:13412 \
PIE_RELAY_TOKEN='<host JWT>' npm start
```

모바일 앱은 별도의 전역 Relay 주소를 관리하지 않는다. Desktop/Gateway가 만든 QR의
pairing offer에 Director/Cell 주소가 포함되므로, QR을 만든 Gateway의 URL 설정을
따른다. `automatic`, `local-only`, `relay-only`는 연결 경로 정책이며 Relay URL을
선택하는 별도 프로필이 아니다.

## 우선순위와 자격증명 보호

적용 순서는 다음과 같다.

1. 화면에서 직접 입력한 주소 또는 CLI `--relay-url`
2. `PIE_RELAY_URL`
3. 코드의 CookAI 공식 기본값

Relay URL이 바뀌면 Desktop은 이전 endpoint에 묶인 host token과 사용하지 않은 invite
code를 새 Relay로 보내지 않도록 초기화한다. 주소를 전환한 뒤에는 대상 Relay에서 방과
토큰을 다시 발급해야 한다.
