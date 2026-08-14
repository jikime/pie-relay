# Project Preview SSL·실행 운영 Runbook

이 문서는 Pie Relay의 사용자 프로젝트를 `*.preview.kroot.io`로 실행하고, 공인 TLS로
외부에 노출하며, 중지·삭제·복구하는 현재 운영 절차를 한곳에 정리한다. 기능 설계는
[`project-preview-platform.md`](./project-preview-platform.md), Kroot 서버의 배포 이력은
[`kroot-staged-deployment-report-2026-08-04.md`](./kroot-staged-deployment-report-2026-08-04.md)를
함께 참고한다.

## 1. 현재 운영 구성

```text
Browser
  → https://chat-relay.cookai.dev
  → Web Chat BFF
  → https://api-relay.cookai.dev
  → Manager
  → 사용자 Executor의 clientd
  → 프로젝트별 npm dev process

Browser
  → https://p-{random}.preview.kroot.io
  → 공용 Traefik
  → Preview Gateway
  → 사용자 전용 internal Docker network
  → 사용자 Executor의 container-local port
```

프리뷰는 별도 사용자 컨테이너가 아니다. 한 사용자에게 할당된 Executor 안에서
프로젝트 앱별 process group으로 실행된다. Host port는 publish하지 않으며 Preview
Gateway만 해당 사용자의 internal network에 동적으로 연결된다.

## 2. DNS와 인증서

권한 DNS에는 다음 레코드가 필요하다.

```text
preview.kroot.io    A 221.143.48.77
*.preview.kroot.io  A 221.143.48.77
```

Wildcard 인증서는 HTTP-01로 발급할 수 없으므로 DNS-01이 필요하다. 2026-08-04에
발급한 현재 인증서는 다음 조건으로 검증됐다.

| 항목 | 현재 값 |
|---|---|
| 발급자 | Let's Encrypt `YE1` |
| SAN | `preview.kroot.io`, `*.preview.kroot.io` |
| 만료일 | 2026-11-02 |
| 발급 방식 | 수동 DNS-01 |
| Traefik 적용 | file provider TLS override |

현재 인증서는 **자동 갱신되지 않는다**. 고객 공개 전에는 Gabia DNS API 또는 위임된
`acme-dns`를 이용한 자동 갱신, 만료 30일 전 경보, 갱신 실패 경보를 반드시 구성한다.

인증서 원본과 Traefik 제공용 파일은 분리한다.

```text
/home/kaonkroot/pie-sandbox-test/preview-acme/config/live/preview.kroot.io/
  fullchain.pem
  privkey.pem

/home/kaonkroot/pie-sandbox-test/preview-tls/
  fullchain.pem
  privkey.pem
```

원본 공용 Traefik Compose는 수정하지 않는다. 인증서 SAN·만료일·개인키 일치를 검사한
후 다음 스크립트로 override만 겹쳐 적용한다.

```bash
ssh pie-sandbox-test
cd /home/kaonkroot/pie-sandbox-test/current/src/deploy/test-server
./apply-preview-tls.sh
```

적용 직후에는 기존 서비스와 임의 프리뷰 hostname을 함께 확인한다.

```bash
curl --fail https://api-relay.cookai.dev/readyz
curl --fail https://relay.cookai.dev/readyz
curl --fail https://chat-relay.cookai.dev/api/health
curl --head https://p-aaaaaaaaaaaaaaaaaaaaaaaaaa.preview.kroot.io
```

마지막 요청은 유효한 route가 없으므로 공인 인증서를 제시한 뒤 HTTP 404가 나와야 한다.
TLS 오류나 Traefik 기본 인증서가 나오면 정상 상태가 아니다.

## 3. 실행 절차

1. Web Chat이 선택한 Project의 실행 가능한 앱을 clientd에 요청한다.
2. clientd는 프로젝트 아래의 `package.json`과 `scripts.dev`를 제한된 깊이로 찾는다.
3. 사용자가 선택한 상대경로를 Manager의 Project 레코드에 저장한다.
4. Manager는 `(integration user, project, appPath)` 단위 잠금을 획득한다.
5. 기존 논리 프리뷰가 있으면 hostname과 container-local port를 재사용한다.
6. 사용자 Executor가 유휴 회수됐으면 같은 사용자 데이터로 다시 생성한다.
7. Manager가 사용자 전용 internal preview network를 만들고 Gateway와 Executor만 연결한다.
8. clientd가 lockfile 지문을 확인해 필요한 경우에만 `npm ci` 또는 `npm install`을 수행한다.
9. clientd가 `HOST=0.0.0.0`, 할당된 `PORT`로 dev server를 별도 process group에서 실행한다.
10. readiness 확인 후 Control 레코드를 `ready`로 바꾸고 외부 URL을 반환한다.

동일 앱에 여러 브라우저가 동시에 실행을 요청해도 하나의 프리뷰 ID·hostname·port로
수렴한다. 공개와 비공개는 서로 다른 프리뷰가 아니라 같은 프리뷰의 접근 정책이다.

## 4. 공개·비공개 접근

- `public`: hostname을 아는 사용자가 바로 접근한다.
- `private`: 짧은 launch token을 host-only, Secure, HttpOnly session cookie로 교환한다.
- 공개 범위를 바꾸면 `accessVersion`이 증가한다.
- 이전 access generation의 launch token과 session cookie는 즉시 거부된다.
- Gateway 인증 cookie는 upstream 앱에 전달하지 않는다.
- 사용자가 보낸 내부 식별 header도 Gateway에서 제거한다.

주소 유출 등으로 hostname 자체를 교체해야 할 때는 기존 프리뷰를 삭제하고 다시 실행한다.
삭제는 dev process를 먼저 중지한 뒤 Control 레코드와 route index를 제거한다.
`preview.deleted` 감사 기록은 삭제하지 않는다.

## 5. 상태 확인

```bash
ssh pie-sandbox-test
root=/home/kaonkroot/pie-sandbox-test
cd "$root/current/src/deploy/test-server"
docker compose -p pie-sandbox-test \
  --env-file "$root/src/deploy/test-server/.env" \
  --profile web-chat ps
```

다음 서비스가 모두 `healthy`여야 한다.

- `manager`
- `preview-gateway`
- `web-chat`
- `relay`
- `postgres`
- 현재 사용 중인 사용자 Executor

Manager 로그에서는 다음 문자열이 없어야 한다.

```text
panic
fatal
version conflict
```

프리뷰 장애를 확인할 때는 순서대로 살펴본다.

1. Preview Control 레코드의 `status`, `lastError`, `hostname`, `port`
2. 사용자 Executor 상태와 health
3. clientd가 반환하는 프리뷰 process 상태와 로그
4. Preview Gateway의 route 조회 결과
5. Gateway와 사용자 preview network 연결 상태
6. Traefik router와 wildcard 인증서

## 6. 자주 발생하는 오류

### `exit status 254`

프로젝트 루트나 실행 앱을 잘못 선택했거나 `scripts.dev`가 없는 경우가 많다. 사용자가
컨테이너 절대경로를 입력하게 하지 말고 앱 탐지를 다시 수행한 뒤 올바른 상대경로를
Project에 저장한다.

### `control record version conflict`

같은 프리뷰 상태를 여러 요청이 동시에 갱신할 때 발생한다. 현재 구현은 프리뷰 ID 및
프로젝트 앱 slot을 PostgreSQL advisory lock으로 직렬화하고 CAS 충돌 시 제한 재시도한다.
계속 발생하면 Manager replica가 같은 PostgreSQL을 사용하는지와 서버 시계 차이를 본다.

### TLS 기본 인증서 또는 인증서 오류

Wildcard 인증서 파일의 SAN, 만료일, 개인키 일치 여부와 공용 Traefik의 file provider
mount를 확인한다. 원본 공용 Traefik 설정을 바로 덮어쓰지 않는다.

### 삭제 후에도 페이지가 잠시 보임

Gateway route cache TTL만큼 짧게 남을 수 있다. 현재 기본값은 2초다. 그 이후에도
접근되면 삭제된 Control 레코드, Gateway cache, upstream process를 순서대로 확인한다.

## 7. 롤백

애플리케이션은 `current`와 `previous` release symlink, 이미지 tag, 배포 전 PostgreSQL
dump를 함께 복구 기준으로 사용한다. Preview TLS override만 되돌릴 때는 원본 공용
Traefik Compose만 지정해 `kroot-shared-lb`를 재생성한다.

롤백 전에는 반드시 다음을 기록한다.

- 현재 release와 이미지 tag
- 실행 중인 Preview와 hostname
- PostgreSQL dump
- 보호된 `.env`
- `docker ps --no-trunc`
- 인증서 지문과 만료일

데이터 스키마가 바뀐 배포는 이미지 tag만 되돌리지 말고 해당 release의 복원 절차와
PostgreSQL 호환성을 먼저 확인한다.

## 8. 고객 공개 전 남은 SSL 작업

- [ ] DNS-01 자동 갱신
- [ ] 인증서 만료 30일·14일·7일 경보
- [ ] 갱신 실패 경보와 운영자 Runbook 연결
- [ ] 갱신 후 SAN·개인키·HTTPS 자동 smoke test
- [ ] 공용 Traefik 재생성 시 기존 API·Relay·Web Chat 연속 확인
- [ ] 인증서와 개인키의 암호화된 외부 백업
