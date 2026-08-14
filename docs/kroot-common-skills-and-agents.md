# Kroot 공통 Skills·Agents 사용자 홈 배포

## 목적

Kroot 프로젝트를 만들 때마다 동일한 `.claude/skills`와 `.claude/agents` 전체를
프로젝트에 복제하지 않는다. Event Manager가 Kroot ADK와
같은 버전의 공통 번들을 사용자별 영속 HOME에 설치하고, 프로젝트에는 프로젝트 문맥에
해당하는 나머지 템플릿만 유지한다.

```text
Kroot ADK templates
  └─ Executor image /usr/local/share/kroot-common
       └─ 운영 서버의 versioned bundle
            └─ Manager reconcile
                 └─ /home/executor/.claude
                      ├─ skills/*
                      └─ agents/*
```

## 파일 소유 경계

| 범위 | 경로 | 관리 주체 |
|---|---|---|
| 사용자 공통 | `~/.claude/skills/<Kroot skill>` | Manager의 버전형 Kroot 번들 |
| 사용자 공통 | `~/.claude/agents/*` | Manager의 버전형 Kroot 번들 |
| 프로젝트 | `<project>/.claude/commands` | `kroot init` |
| 프로젝트 | `<project>/.claude/hooks` | `kroot init` |
| 프로젝트 | `<project>/.claude/rules`, `contexts`, `settings.json` | `kroot init` |
| 프로젝트 | `CLAUDE.md`, `.kroot`, `.mcp.json`, `.github` | `kroot init` |
| 사용자 비밀·상태 | `~/.claude/.credentials.json`, `~/.claude.json`, history 등 | 인증 Broker와 사용자 런타임 |

Manager는 번들에서 발견한 **Kroot 소유 skill 디렉터리**와 `agents` 전체를 교체한다.
다른 이름의 사용자 skill, Claude 인증, 설정, history와 프로젝트는 읽거나 덮어쓰지
않는다. 사용자 정의 agent도 공통 원본에 추가해야 하며 컨테이너의 `~/.claude/agents`를
직접 수정한 내용은 다음 공통 번들 배포 때 중앙 원본으로 교체된다.

## 번들 생성과 배포

`Dockerfile.executor-kroot`는 Kroot 바이너리와 같은 ADK BuildKit context에서 다음
번들을 함께 만든다.

```text
/usr/local/share/kroot-common/
└── .claude/
    ├── skills/
    └── agents/
```

따라서 실행 바이너리와 공통 프롬프트의 revision이 어긋나지 않는다. 이미지를 빌드한
후 운영 서버에서 다음 명령으로 불변 release와 `current` 포인터를 준비한다.

```bash
scripts/ops/prepare-kroot-common-bundle.sh \
  pie-relay-client-kroot:<image-tag> \
  /home/kaonkroot/pie-sandbox-test/data/kroot-common \
  <kroot-adk-git-revision>
```

스크립트는 이미지 안의 번들을 임시 컨테이너로 추출하고, 필수 디렉터리·심볼릭 링크·
파일 수·SHA-256을 검증한다. 결과는 다음 구조로 보존한다.

```text
kroot-common/
├── current -> releases/<revision>-<digest-prefix>
└── releases/
    └── <revision>-<digest-prefix>/
```

Manager 환경은 다음과 같다.

```dotenv
PIE_KROOT_COMMON_BUNDLE_DIR=/home/kaonkroot/pie-sandbox-test/data/kroot-common/current
PIE_KROOT_COMMON_BUNDLE_VERSION=<kroot-adk-git-revision>
```

## 동기화 방식

1. Manager는 `current`가 가리키는 불변 release별로 번들 구조와 모든 파일을 한 번
   검사하고 결정적인 SHA-256을 계산해 프로세스 캐시에 둔다. 사용자 수만큼 같은
   공통 파일 전체를 반복해서 읽지 않는다.
2. 사용자 HOME의 `.pie-kroot-common.json`과 digest가 같으면 복사를 생략한다.
3. 변경되었으면 같은 filesystem 안의 임시 디렉터리에 전체 payload를 staging한다.
4. 기존 Kroot 관리 디렉터리를 rollback 영역으로 옮긴 뒤 새 디렉터리를 rename한다.
5. 디렉터리 fsync가 성공한 뒤에만 marker를 원자적으로 교체한다.
6. 중간 오류가 나면 적용한 디렉터리를 역순으로 되돌린다.

Marker에는 schema, 운영 version, 실제 내용 digest, 관리 root 목록, 파일 수와 적용 시각이
기록된다. 심볼릭 링크와 일반 파일이 아닌 항목은 source와 target 모두 거부한다.

## `kroot init` 동작

Manager는 공통 번들이 활성화된 Executor를 다음 값과 label로 만든다.

```text
KROOT_USER_COMMON=true
pie.kroot_common_home=true
```

Kroot CLI는 이 환경에서 다음 경로만 프로젝트 복사에서 제외한다.

```text
.claude/skills/**
.claude/agents/**
```

나머지 파일은 기존과 동일하게 복사한다. 사용자가 컨테이너 터미널에서 직접
`kroot init`을 실행해도 같은 정책이 적용된다. `kroot update --sync-templates` 역시 공통
경로를 프로젝트에 다시 생성하지 않는다.

일반 PC에서는 `KROOT_USER_COMMON`이 없으므로 기존 동작이 유지된다. 수동 검증이 필요한
경우에만 `kroot init --user-common`을 사용할 수 있다.

## 기존 사용자와 기존 프로젝트

- 사용자 HOME은 컨테이너 수명과 분리된 `/backup/.../executor-state/{userId}`에 있으므로
  컨테이너 재생성만으로 지워지지 않는다.
- Manager가 기존 사용자를 reconcile할 때 공통 번들을 배포한다.
- 공통 모드 label이 없는 기존 컨테이너 shell은 영속 HOME과 workspace를 보존한 채 한 번
  재생성되어 `KROOT_USER_COMMON=true`를 받는다.
- 이미 프로젝트 안에 복사된 skills/agents는 자동 삭제하지 않는다. 사용자가 수정했을 수
  있기 때문이다. 신규 프로젝트부터 중복이 생기지 않는다.
- 기존 프로젝트 정리는 별도 migration에서 공통 번들과 SHA-256이 정확히 같은 파일만
  대상으로 해야 한다.

## 안전한 배포 순서

1. 변경된 Kroot ADK 전체 테스트
2. Kroot Executor image를 불변 tag로 빌드
3. 이미지에서 공통 번들 추출 및 digest 검증
4. `PIE_EXECUTOR_IMAGE`, `PIE_KROOT_COMMON_BUNDLE_VERSION`을 같은 revision으로 설정
5. 변경된 Manager image 빌드
6. Manager만 교체하고 기존 사용자 reconcile 확인
7. 사용자 HOME marker와 공통 파일 확인
8. 신규 프로젝트에서 project-local skills/agents가 생성되지 않는지 확인
9. 기존 프로젝트의 commands/hooks/rules/settings가 유지되는지 확인
10. 문제 시 이전 Manager/Executor image와 이전 bundle release로 `current`를 되돌림

## 검증 항목

자동 테스트는 다음 상황을 포함한다.

- 신규 사용자 HOME 설치
- 기존 사용자 bundle 업그레이드와 폐기된 Kroot skill 정리
- 사용자 자체 skill·settings·credential 보존
- source 및 target 심볼릭 링크 거부
- `current` 심볼릭 링크 release 지원
- 실행 스크립트 권한 보존
- 일반 `kroot init` 기존 동작 보존
- managed `kroot init`에서 공통 두 경로만 제외
- `kroot update --sync-templates` 재복사 방지
- 실제 Linux Executor image의 skills·agents 전체 공통 파일 추출
- 실제 이미지 내부 managed `kroot init` 실행
