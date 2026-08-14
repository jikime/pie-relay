# Relay 인증 및 페어링

## 데스크톱 → Relay

Desktop Host Gateway는 Relay host JWT를 `Authorization: Bearer`로 전송해
assignment/control WebSocket에 등록한다. Relay는 JWT 서명, 만료, host role을
검증하고 `host-hello`의 공개키와 `relayHostId` 일치 여부를 확인한다.

## QR 최초 페어링

QR pairing offer에는 데스크톱 endpoint, 공개키, device token, 연결 모드와
Relay invite token이 포함된다. invite token은 최초 페어링에만 사용하며, 모바일
승인 후 resume credential로 교체한다.

## 모바일 → 데스크톱

- LAN: E2EE 채널에서 device token 검증
- Relay: invite token 또는 resume token으로 Relay 연결 인증 후 device token과
  공개키를 이용해 데스크톱 장치 인증

PAT 원문을 QR, Relay payload, Docker 이미지에 포함하지 않는다. 외부 인증이
필요하면 Manager가 PAT introspection 후 작업 범위가 제한된 delegation token을
발급한다.
