# 서비스별 배포 프로필

`cli-relay` 소스와 Docker 이미지는 하나만 유지하고, Vibe Canvas와 Kroot Studio의
실행 환경은 별도 Compose 프로젝트로 배포한다. 저장소를 복제하거나 서비스별 장기
브랜치를 만들지 않는다.

기존 배포와 장치 자격의 호환을 위해 Vibe Canvas의 일부 내부 식별자에는
`pie-canvas`가 남아 있다. 이것은 제품명이 아니라 application·Compose profile용
내부 slug다.

## 격리 경계

두 서비스는 다음 항목을 공유하지 않는다.

- Compose 프로젝트와 Manager 인스턴스
- PostgreSQL 데이터베이스와 `PIE_DATA_DIR`
- Relay JWT, routing, presence, control, webhook secret
- 회원 introspection client와 Integration service token
- Claude 인증 버전 저장소
- Executor Manager ID, 외부 Executor network와 Preview Gateway 컨테이너
- Relay pool, application scope와 공개 도메인

공유하는 것은 검증된 소스 코드, 버전 태그와 Docker 이미지뿐이다.

## 제품별 인증 경계

두 제품은 장치 연결 인증 방식 자체가 다르다.

- **Vibe Canvas**: `pie-client connect --server <Vibe origin> --code <일회용 코드>`를
  사용한다. Vibe가 자신의 DB에서 코드를 원자적으로 교환하고 응답의 `controlUrl`,
  `applicationId`, `poolId`로 Vibe 전용 Manager와 Relay를 안내한다.
- **Kroot Studio**: 일회용 연결 코드를 사용하지 않는다. `kroot auth login`으로 받은
  PAT를 `~/.kroot/credential.json`에 저장하고 `kroot chat start`가 PAT Bearer 인증으로
  Kroot Relay에 연결한다. Kroot Relay는 auth server introspection 결과로 사용자를
  식별한다.

따라서 Kroot Studio를 `pie-client connect --code`에 연결하거나 Vibe Canvas의 코드
교환 API를 프록시하지 않는다. Vibe의 장치 자격과 Kroot의 PAT도 서로 변환하거나
공유하지 않는다.

## 운영 환경 파일 준비

서비스마다 `deploy/.env.example`을 별도의 비공개 경로로 복사하고 모든 placeholder
secret과 도메인을 채운다. 그런 다음 해당 서비스의 예제 파일 값을 같은 환경 파일에
적용한다.

- Vibe Canvas: `deploy/profiles/pie-canvas.env.example` (`pie-canvas`는 호환용 내부 slug)
- Kroot Studio: `deploy/profiles/kroot-studio.env.example`

두 환경 파일의 `PIE_DATA_DIR`, 데이터베이스, 공개 포트와 도메인도 반드시 달라야 한다.
같은 호스트에서 실행한다면 프로필별 외부 Executor network를 먼저 만든다.

```bash
docker network create \
  --driver bridge \
  --opt com.docker.network.bridge.enable_icc=false \
  pie-canvas-executor

docker network create \
  --driver bridge \
  --opt com.docker.network.bridge.enable_icc=false \
  kroot-studio-executor
```

배포 명령은 같은 Compose 파일과 같은 이미지 태그를 사용한다.

```bash
docker compose -p pie-relay-pie-canvas \
  --env-file /secure/pie-canvas.env \
  -f deploy/compose.yaml up -d --wait

docker compose -p pie-relay-kroot-studio \
  --env-file /secure/kroot-studio.env \
  -f deploy/compose.yaml up -d --wait
```

한 서비스의 환경 파일이나 인증 버전을 바꿀 때는 해당 Compose 프로젝트만 다시
적용한다. 공통 이미지 버전을 올릴 때도 두 프로젝트를 각각 검증하고 순차 배포한다.

## 인증 정책

Vibe Canvas의 장치 access·refresh token은 Vibe 전용 장치 인증 경계에서만 사용한다.
Kroot Studio의 사용자 인증은 PAT와 Kroot auth introspection이 담당하며, PAT는 사용자별
`~/.kroot/credential.json`에만 저장한다. 이미지·공용 환경변수·공유 volume에 PAT를
넣지 않는다.

Manager가 내부 세션 제어를 위해 발급하는 짧은 Relay JWT나 Integration service token은
전송 경로용 자격이다. Kroot 사용자 신원을 나타내는 PAT를 대체하지 않는다. Manager가
Executor에 배포하는 Claude 인증은 서비스별 `PIE_DATA_DIR/claude-auth`에 저장하며 다른
서비스로 복사하지 않는다.

Kroot Studio는 `PIE_EXECUTOR_KROOT_AUTO_LINK=true`를 사용하여 Kroot 사용자 자격과
프로젝트 연결을 적용한다. Vibe Canvas에는 이 정책을 적용하지 않는다.

## 로컬 개발

로컬 프로필은 `deploy/local/pie-local.sh`에서 선택한다.

```bash
# 기존 Vibe Canvas 데이터와 포트를 그대로 사용한다.
PIE_RELAY_PROFILE=pie-canvas ./deploy/local/pie-local.sh up

# 완전히 분리된 Kroot Studio 로컬 스택을 사용한다.
PIE_RELAY_PROFILE=kroot-studio ./deploy/local/pie-local.sh up
```

프로필별 실제 경로와 주소는 다음 명령으로 확인한다.

```bash
PIE_RELAY_PROFILE=pie-canvas ./deploy/local/pie-local.sh profile
PIE_RELAY_PROFILE=kroot-studio ./deploy/local/pie-local.sh profile
```
