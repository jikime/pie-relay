# Pie Relay Azure staging

이 디렉터리는 `rg-jikime`에 배포한 **Relay-only** Azure staging의 운영 기록이다.
Manager, PostgreSQL, Executor와 사용자 Docker 컨테이너는 이 Azure 배포에 포함하지
않는다. PC·Docker의 `clientd`, Desktop과 Mobile이 공개 HTTPS/WSS Relay에 outbound로
접속한다.

## 현재 배포

배포 확인일은 2026-07-26이며 지역은 Korea Central이다.

| 항목 | 값 |
|---|---|
| Resource Group | `rg-jikime` |
| Container App | `pie-relay-test` |
| Container Apps Environment | `pie-relay-test-env` |
| 공개 HTTPS origin | `https://pie-relay-test.wonderfulplant-4d48fbdb.koreacentral.azurecontainerapps.io` |
| ACR | `vibecanvascollab20260725.azurecr.io` |
| image | `pie-relay/server:staging-20260726083242-a7cf4d518272-mobile-state` |
| image digest | `sha256:e8c90db508f7ced6e1a27fe2fd16b0cde5b09e3a8d943e4478e76b7ffb5488d7` |
| active Revision | `pie-relay-test--r20260726c` (`Healthy`, traffic 100%) |
| ACR pull identity | `pie-relay-acr-pull` (`AcrPull`만 부여) |
| 상태 저장소 | `pierelayst83b29893` / Azure Files `relay-state` |
| Relay state mount | `/var/lib/pie-relay/relay` |
| Replica | 최소 1, 최대 1 |
| CPU / memory | 0.5 vCPU / 1 GiB |
| Revision mode | Single |

기존 `vibe-collab-env`와 `vibe-collaboration`은 변경하지 않았다. 기존 ACR에는
`pie-relay/server` repository만 추가했고, 앱은 사용자 할당 Managed Identity로
이미지를 pull한다. ACR admin credential은 Relay에 사용하지 않는다.

Relay room, invite와 socket registry가 현재 프로세스 메모리에 있으므로 Replica를
2개 이상으로 올리지 않는다. 다중 Relay는 node assignment와 같은 room의 host와
participant를 동일 node로 보내는 affinity가 구현된 뒤에만 허용한다.

## 클라이언트 설정

UI에 입력하는 Relay 서버 주소는 WebSocket path가 아니라 다음 **HTTPS origin**이다.

```text
https://pie-relay-test.wonderfulplant-4d48fbdb.koreacentral.azurecontainerapps.io
```

각 클라이언트가 실제로 사용하는 경로는 다음과 같다.

```text
clientd host:          wss://.../ws/agent
Desktop participant:  wss://.../ws/participant
Mobile Director:      https://.../v1/assign, /v1/resolve
Mobile Cell:          wss://.../v1/host/control, /v1/connect/{hostId}
```

Standalone 방 만들기에는 Container App secret `host-enroll-secret` 값이 필요하다.
값을 화면이나 로그에 남기지 말고 필요한 운영자만 다음 명령으로 가져온다.

```bash
az containerapp secret list \
  --resource-group rg-jikime \
  --name pie-relay-test \
  --show-values \
  --query "[?name=='host-enroll-secret'].value | [0]" \
  --output tsv
```

HTTP ingress의 `allowInsecure`는 `false`다. 평문 HTTP 요청은 HTTPS로 redirect되며,
WebSocket에는 반드시 `wss://`를 사용한다. scoped JWT, participant ticket
subprotocol, Origin allowlist가 활성화되어 있고 legacy query ticket과 unscoped token은
비활성화되어 있다.

## 상태와 Secret

- `RELAY_JWT_SECRET`, `HOST_ENROLL_SECRET`, `PIE_RELAY_METRICS_TOKEN`은 Container
  App secret으로 보관한다.
- 모바일 resume credential 원문은 저장하지 않는다. 해시 상태만 Azure Files의
  `/var/lib/pie-relay/relay/mobile-state.json`에 기록한다.
- Azure Files(SMB)는 POSIX `chmod`를 지원하지 않으므로 이 배포에는
  `RELAY_MOBILE_STATE_ALLOW_UNSUPPORTED_MODE=true`를 설정한다. 이 예외는 앱 전용
  Azure Files mount에만 사용하며 일반 POSIX 볼륨에서는 활성화하지 않는다.
- Relay는 터미널 데이터, Claude 인증, Kroot PAT와 workspace를 저장하지 않는다.
- Secret 회전은 기존 JWT와 host 등록을 무효화할 수 있으므로 영향 범위를 확인하고
  단계적으로 수행한다.
- 현재는 staging이므로 Container App secret을 사용한다. 고객 운영 전에는 Key Vault
  reference와 Secret 회전 runbook을 추가한다.

## 검증

공개 상태는 다음처럼 확인한다.

```bash
curl --fail \
  https://pie-relay-test.wonderfulplant-4d48fbdb.koreacentral.azurecontainerapps.io/healthz
curl --fail \
  https://pie-relay-test.wonderfulplant-4d48fbdb.koreacentral.azurecontainerapps.io/readyz
```

Standalone 인증·WSS smoke test는 Secret을 출력하지 않고 다음 항목을 확인한다.

- health/readiness와 인증된 metrics
- 잘못된 host enrollment key 거부
- 모바일 assignment가 HTTPS Cell URL을 광고하는지
- host enrollment, invite, join
- 허용되지 않은 WebSocket Origin 거부
- participant credential의 host 축 사용 거부
- host/participant WSS handshake와 양방향 frame relay
- participant와 host의 동일 credential 재접속

```bash
cd server
export PIE_RELAY_SMOKE_URL='https://pie-relay-test.wonderfulplant-4d48fbdb.koreacentral.azurecontainerapps.io'
export PIE_RELAY_SMOKE_ENROLL_SECRET="$(az containerapp secret list \
  --resource-group rg-jikime \
  --name pie-relay-test \
  --show-values \
  --query \"[?name=='host-enroll-secret'].value | [0]\" \
  --output tsv)"
go run ./cmd/relay-smoke
unset PIE_RELAY_SMOKE_ENROLL_SECRET
```

2026-07-26 자동 smoke test에서 위 항목은 모두 통과했다. 같은 날 iOS Simulator와
relay-only Mobile Gateway를 새 QR로 페어링하고 앱을 완전히 종료·재실행한 뒤에도
`Connected · Pie Relay` 복구를 확인했다. 모바일 터미널에서
`echo PIE_AZURE_RELAY_ONLY_OK`를 전송해 동일 문자열의 응답을 받았으며, 테스트 동안
Gateway의 로컬 `127.0.0.1:16893`에는 LISTEN socket만 있고 모바일의 직접 연결은
존재하지 않았다. 따라서 이 테스트의 명령 왕복은 Azure WSS Relay 경로를 사용했다.

같은 Simulator 앱 프로세스를 유지한 채 설정 앱을 20초 동안 전면에 띄워 Pie Relay를
background로 보낸 후 다시 foreground로 전환했다. 전환 전
`echo PIE_BACKGROUND_BEFORE_OK`, 전환 후 `echo PIE_BACKGROUND_AFTER_OK`가 각각 동일한
문자열을 반환했고, foreground 복귀 전후에도 세션 상태는 연결됨이었다. 이 과정에서도
Gateway 포트에는 LISTEN socket만 존재해 background 복구 후 명령 역시 Azure Relay를
통과했다.

이번 검증은 Simulator 기반 기능 E2E다. 실제 iPhone용 Debug/Release arm64 빌드와
Release Hermes JS bundle 포함까지는 확인했지만, 기기 설치에는 Xcode Apple 계정
재인증과 유효한 Apple Development 서명 identity/provisioning profile이 필요하다. 실제
iPhone의 Wi-Fi→셀룰러 전환, 장시간 background/foreground 복구와 통신사 NAT 환경은
서명 준비 후 별도의 물리 기기 E2E로 계속 수행한다.

## 이미지 갱신과 롤백

서버 검증 후 immutable tag로 ACR에 빌드한다. `server/Dockerfile`은 ACR 기본
builder에서도 동작하도록 BuildKit 전용 cache mount에 의존하지 않는다.

```bash
IMAGE_TAG="staging-$(date -u +%Y%m%d%H%M%S)-$(git rev-parse --short=12 HEAD)"
az acr build \
  --registry vibecanvascollab20260725 \
  --image "pie-relay/server:${IMAGE_TAG}" \
  --file server/Dockerfile \
  server

az containerapp update \
  --resource-group rg-jikime \
  --name pie-relay-test \
  --image "vibecanvascollab20260725.azurecr.io/pie-relay/server:${IMAGE_TAG}" \
  --revision-suffix "r$(date -u +%Y%m%d%H%M%S)"
```

갱신 뒤 새 Revision이 `Healthy`이고 smoke test가 통과하기 전에는 완료로 간주하지
않는다. 실패하면 같은 update 명령에 직전 정상 image tag 또는 digest를 지정한다.
Single revision 전환 중 기존 Revision은 새 Revision이 준비될 때까지 요청을 처리한다.

## 관찰과 남은 운영 작업

```bash
az containerapp revision list \
  --resource-group rg-jikime \
  --name pie-relay-test \
  --output table

az containerapp logs show \
  --resource-group rg-jikime \
  --name pie-relay-test \
  --tail 100 \
  --format text
```

고객 트래픽을 받기 전 남은 항목은 다음과 같다.

- Bicep 또는 Terraform으로 현재 리소스를 재현하고 drift를 검사한다.
- Container App secret을 Key Vault reference로 전환한다.
- Log Analytics query, WebSocket 연결 수·rate-limit·slow-peer 경보를 구성한다.
- 실제 Desktop/clientd, Docker clientd와 물리 iPhone Wi-Fi·셀룰러 E2E를 수행한다.
- 예상 동시 연결과 room fan-out의 부하·장시간 soak test를 수행한다.
- Relay Revision 교체 중 실제 클라이언트 reconnect와 모바일 상태 보존을 검증한다.
