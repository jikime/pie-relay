# Kroot 공통 Skills·Agents 배포 결과 — 2026-08-13

## 1. 배포 목적과 범위

Kroot ADK의 `templates/.claude/skills/**`와 `templates/.claude/agents/**`를
프로젝트마다 복사하지 않고, Manager가 각 사용자 Executor의
`/home/executor/.claude`에 공통 자산으로 설치하도록 배포했다.

이번에 추가된 자산은 다음과 같다.

- 에이전트 6개: `design-analyzer`, `design-system-applier`,
  `design-system-composer`, `design-system-reviewer`, `designmd-author`,
  `page-builder`
- 스킬 10개: `design-composition`, `design-extraction`,
  `design-fidelity-check`, `design-system-apply`,
  `design-system-orchestrator`, `designmd-spec`, `kroot-mobile-deploy`,
  `kroot-universal-glossary`, `kroot-workflow-glossary`, `page-generation`

배포된 전체 공통 자산은 스킬 디렉터리 90개, 에이전트 파일 40개를 포함한
파일 520개다. `.claude/agents`는 중앙 소스 전체를 관리하고,
`.claude/skills`는 중앙 소스에 존재하는 각 스킬 디렉터리를 관리한다.

## 2. 고정 버전

| 구분 | 버전 |
|---|---|
| Pie Relay 공통 번들 구현 | `83cc02241452` |
| Executor 교체 후 요청 재전송 보강 | `e072b90721fd` |
| Kroot ADK 및 신규 자산 | `3ba09c6f5b99` |
| Kroot Proto | `1ed117e588b2` |
| Manager 이미지 | `pie-executor-manager:20260813-kroot-common-e072b90` |
| Executor 이미지 | `pie-relay-client-kroot:20260813-kroot-common-83cc022-3ba09c6` |
| 운영 release | `/home/kaonkroot/pie-sandbox-test/releases/20260813-kroot-common-e072b90-3ba09c6` |
| 공통 bundle release | `/home/kaonkroot/pie-sandbox-test/data/kroot-common/releases/3ba09c6f5b99-34b28c62295c` |

번들 내보내기 단계의 release 지문은
`sha256:34b28c62295c6a01df01500f911ec1f8724990c88356843bc39e0295c126a77f`이고,
Manager가 설치 대상 경로·정규화된 권한·파일 본문으로 다시 계산한 설치 지문은
`sha256:34d762ee92fa39c0bdbf18d375f7926dc9176fc5a166214b149b138a5d9d7396`이다.
두 지문은 계산 목적과 직렬화 규칙이 다르며, 모든 사용자 marker는 동일한 설치 지문을
가진다.

## 3. 배포 동작

Manager는 공통 bundle의 `current` 심볼릭 링크를 실제 불변 release로 해석한 뒤,
각 사용자 HOME에 원자적으로 동기화한다. 설치가 끝나면 다음 marker를 기록한다.

```text
/home/executor/.pie-kroot-common.json
```

marker에는 schema, ADK bundle version, 설치 지문, 관리 경로, 파일 수와 적용 시간이
저장된다. 동일 지문은 다시 복사하지 않으므로 사용자 수가 늘어도 매 reconciliation마다
520개 파일을 다시 해시·복사하지 않는다.

Executor에는 `KROOT_USER_COMMON=true`가 주입된다. 따라서 이후 `kroot init`은 공통
`skills`와 `agents`를 프로젝트 안에 중복 복사하지 않고, commands·rules·hooks·settings
같은 프로젝트 전용 템플릿만 유지한다.

## 4. 배포 중 발견하고 보강한 복구 경계

실제 사용자 작업이 실행 중인 상태에서 Executor가 새 이미지로 교체되자 Relay는 같은
participant 연결에 `host:status=false`와 `host:status=true`를 차례로 전달했다. 기존
Manager는 이미 전송했다고 기억한 요청 ID를 같은 WebSocket에서 다시 보내지 않아,
내구성 저널에는 요청이 남아 있지만 화면에서는 작업 중으로 고정될 수 있었다.

`e072b90721fd`에서 호스트가 오프라인으로 바뀌면 해당 연결의 전송 상태만 초기화하도록
수정했다. 다시 온라인이 되면 내구성 저널의 같은 요청 ID를 전송한다.

- 기존 clientd가 살아 있으면 request replay cache가 중복 실행을 막고 누락 이벤트만
  재전송한다.
- Executor가 실제로 교체됐으면 새 clientd가 같은 요청을 다시 실행해 terminal event를
  돌려준다.
- 회귀 테스트는 participant WebSocket은 유지한 채 host만 교체되는 상황에서 동일 요청이
  두 번째 host에도 전달되고 `request.completed`가 기록되는지 확인한다.

서버 배포에서도 교체 중이던 실제 요청이 새 Executor에서 자동 재개되어 완료됐으며,
최종 미완료 journal request는 0건이다.

## 5. 검증 결과

| 검증 항목 | 결과 |
|---|---|
| 신규 16개 파일 frontmatter·필수 필드·중복 이름 | 통과 |
| 신규 파일 비밀값 패턴 검사 | 검출 없음 |
| Kroot ADK 전체 `go test`, `go vet` | 통과 |
| 깨끗한 ADK/Proto 커밋 조합 빌드 | 통과 |
| Manager 전체 `go test`, race, `go vet` | 통과 |
| host replacement replay 회귀 테스트 10회 | 통과 |
| Executor 이미지 UID/GID/HOME | `10001:10001`, `/home/executor` |
| 이미지 내부 공통 자산 | 520개, 신규 16개 포함 |
| 실행 중 사용자 Executor | 7개 모두 새 이미지, healthy, restart 0 |
| 사용자 marker | 7개 모두 schema/version/digest/file count 일치 |
| Executor의 `skills_list` | 사용자 스킬 90개, 신규 스킬 10개 확인 |
| 관리형 `kroot init` | 프로젝트 skills/agents 미복사, 나머지 템플릿 정상 |
| Relay·장치·런타임·세션·대화 | 각각 7개 연결/활성 상태 |
| Chat Gateway | peer 7, active/queued turn 0, pending journal 0 |
| 공개 Relay·Manager·Web Chat | 모든 상태 경로 HTTP 200 |

사용자 workspace와 HOME은 기존 `/backup/pie-sandbox-test/executor-data` bind mount를
그대로 재사용했다. 컨테이너 shell만 교체했으며 프로젝트·첨부파일·인증 저장소를
삭제하거나 이동하지 않았다.

## 6. 백업과 롤백

배포 전 백업:

```text
/home/kaonkroot/pie-sandbox-test/backups/20260813T073256Z-pre-kroot-common-83cc022
```

배포 후 백업:

```text
/home/kaonkroot/pie-sandbox-test/backups/20260813T075132Z-post-kroot-common-e072b90
```

두 백업에는 Compose와 환경 파일, release 포인터, PostgreSQL custom dump, 이미지·컨테이너
inspect 및 SHA-256 검증표가 포함된다. 비밀값이 포함될 수 있어 디렉터리와 파일 권한은
각각 `0700`, `0600`이다.

단계별 롤백 원칙은 다음과 같다.

1. 신규 turn을 잠시 막고 현재 active/queued turn이 0인지 확인한다.
2. 요청 재전송 보강만 되돌릴 때에는 `previous` release
   (`20260813-kroot-common-83cc022-3ba09c6`)의 Manager 이미지를 사용한다.
3. 공통 bundle 기능 전체를 되돌릴 때에는 배포 전 백업의 Compose·환경 파일과
   `pie-executor-manager:20260813-workspace-editor-32f42c3`,
   `pie-relay-client-kroot:20260813-workspace-editor-32f42c3`을 사용한다.
4. 사용자 `/backup` 데이터는 삭제하지 않는다. 공통 자산 내용까지 이전판으로 되돌려야
   한다면 이전 Kroot ADK template로 별도 bundle release를 만든 뒤 Manager를 통해
   다시 동기화한다.
5. 롤백 후 공개 상태, 7개 Relay 연결, pending journal 0과 실제 채팅 왕복을 다시 확인한다.

