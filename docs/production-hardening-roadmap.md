# Pie 운영 보강 10개 항목과 TODO

> 기준일: 2026-08-05
> 이번 우선 작업: **2번, 4번, 7번**
> 관련 Runbook: [`preview-ssl-runtime-runbook.md`](./preview-ssl-runtime-runbook.md)

이 문서는 현재 Kroot 단일 노드 검증 환경을 고객용 서비스로 발전시키기 위한 작업 목록이다.
`완료`는 저장소 구현과 자동 테스트가 끝났다는 뜻이며, `부분 완료`는 현재 환경에서 유효한
1차 보호가 있으나 인프라 변경이나 장기 검증이 더 필요하다는 뜻이다.

| 번호 | 운영 보강 항목 | 상태 | 이번 범위와 남은 완료 조건 |
|---:|---|---|---|
| 1 | 용량 계획과 admission control | TODO | CPU·메모리 overcommit 정책, 실제 Claude workload 기준 노드 수용량, 신규 가입 차단 기준을 확정한다. |
| 2 | 사용자별 디스크 quota와 노드 공간 보호 | **부분 완료** | 20GiB 기본 감시 quota, 초과 Executor 정지·재실행 거부, 노드 headroom 차단, 사용량 metric을 구현했다. ext4/XFS project quota 기반 hard limit은 TODO다. |
| 3 | 데이터 백업·복구·탈퇴 보존 정책 | TODO | PostgreSQL, Workspace, 사용자 Home/인증정보의 암호화 백업과 사용자 단위 복원 훈련, 보존·파기 기한을 자동화한다. |
| 4 | Executor 컨테이너 격리 강화 | **부분 완료** | non-root/cap-drop/read-only에 swap 차단, private IPC, init, ulimit, 순환 로그, isolation version, 전용 bridge ICC 차단 검증을 추가했다. Docker socket broker와 VM급 sandbox는 TODO다. |
| 5 | 자격증명·secret 수명주기 | TODO | Claude 인증 seed, Integration token, Relay/Manager secret의 외부 secret store 보관·회전·폐기와 감사 절차를 구현한다. |
| 6 | Preview 공급망·런타임 안전성 | TODO | 의존성 설치 정책, 악성 package script 방어, egress 정책, 빌드 cache 상한, 장시간 process 회수 기준을 확정한다. |
| 7 | Web Chat→Manager→Relay→clientd 관측성 | **부분 완료** | `X-Request-ID`를 Web Chat BFF에서 Manager까지 전파하고, Manager HTTP latency/status, 디스크, Chat 성공·실패 metric과 경보를 추가했다. 중앙 로그·분산 trace는 TODO다. |
| 8 | 부하·장애·장시간 soak 시험 | TODO | 15명 동시 채팅·Preview, Relay 재시작, 네트워크 단절, OOM, inode·disk 고갈, 느린 Claude 응답을 자동 시험한다. |
| 9 | 고가용성·다중 노드 배치 | TODO | Manager leader/lease, Node Agent, workspace 이동, fencing, Relay/Manager replica와 장애 시 재배치를 구현한다. |
| 10 | 배포·변경·운영 대응 체계 | TODO | 무중단 release, schema 호환 rollback, SLO·on-call·장애 등급, 보안 업데이트와 고객 공지 절차를 확정한다. |

## 2번 구현 경계

사용량은 사용자별 세 경로를 합산한다.

```text
Workspace + Executor Home/상태 + 업로드 blob = 사용자 DiskUsedBytes
```

- 기본 한도는 20GiB이며 Control Plane 사용자 quota의 `diskBytes`로 덮어쓸 수 있다.
- 실행 전과 1분 주기 검사에서 한도를 넘으면 `quota_exceeded`로 만들고 컨테이너를 정지한다.
- 노드 가용 공간이 reserve 아래면 새 Executor 실행을 503으로 막는다.
- 사용량, 총 quota, 노드 가용 공간, 초과 사용자, 검사 오류를 `/metrics`에 노출한다.
- symlink 대상은 따라가지 않아 다른 사용자의 파일을 중복 계산하거나 경계를 벗어나지 않는다.

현재 Kroot host의 root ext4에는 project quota mount option이 없다. 따라서 20GiB를 넘기는
그 순간의 `write(2)`를 실패시키는 hard quota가 아니라 최대 한 검사 주기의 여유가 있는
보호 장치다. hard quota 완료 조건은 전용 XFS/ext4 project-quota volume, 사용자 project ID
할당, quota 원자 적용, 재부팅·복원 E2E다.

## 4번 구현 경계

신규 또는 정책 버전이 오래된 Executor는 `isolation_version=v3`로 재생성한다. Workspace와
Home은 bind mount이므로 컨테이너 shell을 재생성해도 사용자 데이터는 유지된다.

- 고정 non-root UID/GID, capability 전체 제거, `no-new-privileges=true`
- read-only rootfs, `nodev/noexec/nosuid` 256MiB `/tmp`
- 메모리와 memory-swap을 같은 값으로 지정해 swap 차단
- private IPC, init process, core dump 0, open file 4096, PID limit
- Docker `local` 로그 driver, 파일 크기·개수 제한
- 전용 bridge, Internet egress 허용, container-to-container ICC 차단 시작 검증
- Docker socket, privileged, host network, 임의 host mount는 Executor에 제공하지 않음

Manager 자체가 Docker socket을 직접 마운트하는 위험은 남아 있다. 고객 공개 전에는 허용된
create/start/stop/inspect 동작만 제공하는 Node Agent 또는 socket proxy로 분리하고, AppArmor/
seccomp와 egress allowlist, 필요 시 gVisor·Kata 같은 커널 격리를 평가한다.

## 7번 구현 경계

브라우저가 안전한 `X-Request-ID`를 보내면 Web Chat BFF가 그대로 사용하고, 없거나 형식이
잘못되면 새 ID를 만든다. 같은 ID가 Manager 요청·응답과 구조화 로그에 남는다. 채팅 payload의
기존 `requestId`는 Relay를 지나 clientd 응답과 journal까지 연결된다. 프롬프트와 PAT는 로그에
기록하지 않는다.

추가 metric은 다음과 같다.

- `pie_manager_http_requests_total`
- `pie_manager_http_request_duration_seconds_*`
- `pie_executor_manager_disk_*`
- `pie_chat_gateway_requests_{started,finished,failed}_total`

경보는 Manager 5xx, queue 80%, quota 초과, 노드 20GiB 미만, disk scan 오류, Chat 반복 실패를
포함한다. 다음 단계는 Loki/OpenSearch 등의 중앙 로그, OpenTelemetry trace, SLO dashboard,
개인정보 마스킹 회귀 시험이다.

## 다음 작업 권장 순서

1. 8번 부하·장애 시험으로 이번 2·4·7번 보호가 실제 압력에서 작동하는지 검증한다.
2. 3번 백업·사용자 단위 복원을 먼저 완성한다.
3. 5번 secret store와 회전을 적용한다.
4. 1번 실측 admission 기준을 만들고 9번 다중 노드로 확장한다.
5. 6번 Preview 공급망과 10번 운영 프로세스를 고객 공개 기준으로 닫는다.
