# Pie Relay Server systemd 배포

단일 Linux 호스트에서 Relay만 상주시킬 때의 배포 예시다. Manager/PostgreSQL/Executor를
함께 운영하면 저장소 루트의 `deploy/compose.yaml`을 우선 사용한다.

## 설치

```bash
cd server
go build -trimpath -ldflags="-s -w" -o /tmp/pie-relay ./cmd/relay
sudo install -Dm755 /tmp/pie-relay /usr/local/bin/pie-relay

sudo install -d -m700 /etc/pie-relay
sudo install -m600 deploy/relay.env.example /etc/pie-relay/relay.env
sudoedit /etc/pie-relay/relay.env

sudo install -Dm644 deploy/pie-relay.service /etc/systemd/system/pie-relay.service
sudo systemctl daemon-reload
sudo systemctl enable --now pie-relay
```

`RELAY_JWT_SECRET`과 `HOST_ENROLL_SECRET`은 각각 다른 32바이트 이상의 난수로 바꾼다.
두 legacy 허용 옵션은 승인된 전환 기간 외에는 `false`로 둔다.

## 확인

```bash
systemctl status pie-relay
journalctl -u pie-relay -f
curl --fail http://127.0.0.1:13412/healthz
curl --fail http://127.0.0.1:13412/readyz
```

운영 외부 주소는 `https://relay.cookai.dev`처럼 TLS reverse proxy를 거친다. Proxy는
`/ws/agent`, `/ws/participant`, `/rooms/*`, `/host/enroll`, `/v1/*`의 WebSocket upgrade와
장시간 연결 timeout을 보존해야 한다. `/metrics`와 `/v1/control/*`는 공개하지 않고
각각의 Bearer token과 내부 네트워크로 제한한다.

서버는 SIGTERM을 받으면 readiness를 먼저 내리고 최대 20초 동안 기존 연결을 drain한다.
유닛의 `TimeoutStopSec`는 이보다 길어야 한다. 모바일 resume 상태는
`/var/lib/pie-relay/mobile-state.json`에 해시 형태로 저장되며 systemd가 전용 상태
디렉터리를 만든다.

구버전 `/usr/local/bin/cli-relay`, `/etc/cli-relay`, `cli-relay.service` 설치가 있다면
새 서비스와 동시에 띄우지 않는다. 포트와 상태 파일을 확인하고 credential을 유지한 채
Pie 경로로 한 번에 전환한다.
