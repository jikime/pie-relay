# Pie Relay Azure 배포 계획

> 작성 기준일: 2026-07-26  
> 대상 리소스 그룹: `rg-jikime`  
> 현재 범위: Azure에는 Pie Relay만 배포하고 Manager·Executor는 외부 PC/Docker에서 운영  
> 실제 배포 상태와 명령: [`../deploy/azure/README.md`](../deploy/azure/README.md)

## 1. 결론

현재 확정된 범위에서는 Azure에 **Relay Data Plane만** 둔다. 2026-07-26
`rg-jikime`의 Korea Central에 단일 Replica Azure Container App
`pie-relay-test`를 배포했고, Azure 기본 HTTPS/WSS 주소로 standalone 인증과 양방향
WebSocket smoke test를 통과했다. 같은 주소를 사용한 relay-only iOS Simulator 페어링,
앱 재실행 복구, 20초 background/foreground 복구와 모바일 터미널 명령 왕복도
통과했다. 물리 iPhone용 Release arm64/Hermes bundle 빌드는 성공했지만 실제 설치와
Wi-Fi·셀룰러 전환 검증은 Apple 개발자 계정 재인증 및 서명 준비 후 수행해야 한다.

Manager, PostgreSQL, Executor와 사용자 Docker 컨테이너는 Azure Relay 배포에 포함하지
않는다. PC 또는 Docker의 `clientd`, Desktop과 Mobile이 Azure Relay에 outbound로
연결한다. 따라서 이 범위에는 Linux VM, Docker socket과 PostgreSQL이 필요하지 않다.

이 문서의 VM/Manager/Executor 관련 후속 절은 미래에 full stack 전체를 Azure로
옮기는 경우를 위한 별도 확장안이다. 현재 Relay-only 배포의 정본은
[`deploy/azure/README.md`](../deploy/azure/README.md)다.

## 2. 현재 Azure 상태

읽기 전용으로 확인한 `rg-jikime`의 현재 상태는 다음과 같다.

| 리소스 | 현재 값 | Pie Relay 적용 판단 |
|---|---|---|
| Resource Group | `rg-jikime`, 메타데이터 위치 `koreasouth` | 그대로 사용 |
| ACR | `vibecanvascollab20260725.azurecr.io`, Basic, Korea Central | staging에서 재사용 가능. 운영은 별도 ACR 또는 SKU·권한 검토 |
| ACR admin user | 활성화 | Pie VM에는 비밀번호 대신 Managed Identity `AcrPull` 사용. 기존 서비스 영향 검토 후 admin 비활성화 |
| Log Analytics | `workspace-rgjikime2Nxu`, Korea Central, 30일 보존 | 초기 모니터링에 재사용 가능 |
| Container Apps Environment | `vibe-collab-env`, Korea Central | Pie Manager/Executor 실행 위치로는 사용하지 않음 |
| Container App | `vibe-collaboration`, Korea Central | 기존 서비스이므로 변경하지 않음 |
| Pie Container Apps Environment | `pie-relay-test-env`, Korea Central | Relay 전용으로 생성 |
| Pie Container App | `pie-relay-test`, Korea Central, 1 Replica | 현재 Relay-only staging |
| Pie state storage | `pierelayst83b29893` / `relay-state` | 모바일 resume hash 상태 전용 |

Resource Group의 위치는 내부 메타데이터 위치이며 그 안의 모든 리소스 위치를 강제하지
않는다. 기존 ACR과 Log Analytics가 있는 **Korea Central**을 Pie Relay의 기본 지역으로
선택하면 이미지 pull과 로그 전송 경로를 단순하게 유지할 수 있다.

## 3. 1차 배포 아키텍처

```text
Desktop / clientd / Mobile Gateway
                │ HTTPS / WSS :443
                ▼
     Static Public IP + Azure NSG
                │
        Ubuntu VM (Korea Central)
        ├─ Traefik :80/:443
        │   ├─ relay.cookai.dev       → Relay :13412
        │   ├─ api-relay.cookai.dev   → Manager :19090
        │   └─ admin-relay.cookai.dev → Manager Admin
        ├─ Pie Relay
        ├─ Pie Manager
        ├─ PostgreSQL (staging 1차)
        └─ 사용자별 Executor 컨테이너
                │
                ├─ ACR: immutable image pull
                ├─ Key Vault: secret 원본
                ├─ Log Analytics / Azure Monitor
                └─ Recovery Services Vault
```

Relay는 terminal payload를 전달하는 Data Plane이고 Manager는 사용자, 장치, 세션,
권한과 Docker Executor를 관리하는 Control Plane이다. 외부에는 Traefik의 80/443만
공개하고 Relay, Manager, PostgreSQL, Prometheus와 Docker API 포트는 공개하지 않는다.

### 권장 리소스 이름

이름은 실제 생성 전에 충돌 여부를 확인한다. ACR와 Key Vault 이름은 Azure 전체에서
고유해야 하므로 suffix가 필요하다.

| 종류 | 권장 이름 |
|---|---|
| Virtual Network | `vnet-pie-relay-krc` |
| Subnet | `snet-pie-relay` |
| Network Security Group | `nsg-pie-relay` |
| Static Public IP | `pip-pie-relay` |
| Network Interface | `nic-pie-relay-01` |
| Linux VM | `vm-pie-relay-01` |
| Data Disk | `disk-pie-relay-data-01` |
| Key Vault | `kv-pie-relay-<suffix>` |
| Recovery Services Vault | `rsv-pie-relay-krc` |
| 별도 ACR 선택 시 | `pielabrelay<suffix>` |

## 4. VM과 저장소 기준

### 초기 크기

| 단계 | 제안 시작점 | 용도 |
|---|---|---|
| staging | 4 vCPU / 16 GiB (`Standard_D4s_v5` 계열) | 기능, 모바일 WAN, 소수 Executor 검증 |
| 초기 운영 | 8 vCPU / 32 GiB (`Standard_D8s_v5` 계열) | 제한된 고객 운영과 부하 측정 |

SKU 이름은 생성 시점의 Korea Central 가용성과 구독 quota를 다시 확인한다. 위 값은
보장 용량이 아니라 측정 시작점이다. 기본 Executor 제한이 사용자당 2 vCPU/2 GiB이므로
32 GiB VM도 OS, Docker, Relay, Manager, PostgreSQL 여유분을 제외하면 완전 부하 상태의
Executor를 약 10여 개 이상 안정적으로 수용한다고 단정할 수 없다. 활성 Executor 수,
평균 RSS, CPU throttling, 이미지 pull 시간과 disk IOPS를 측정해 worker 분리 시점을
결정한다.

### 디스크

- OS disk는 64~128 GiB로 시작한다.
- Premium SSD 계열의 별도 data disk를 최소 256 GiB로 시작하고
  `/var/lib/pie-relay`에 마운트한다.
- 사용자 수가 늘면 `/var/lib/docker`도 별도 data disk로 분리한다. Docker image/layer,
  container writable layer와 inode 부족은 workspace 용량과 별개의 장애 원인이 된다.
- VM 삭제 시 data disk가 함께 삭제되지 않도록 IaC의 delete option을 명시한다.
- 사용자 workspace와 Claude 인증 상태는 암호화된 관리 디스크에 저장하고 OS 계정 및
  Manager 외 접근을 제한한다.

## 5. 네트워크와 DNS

### NSG 인바운드

| 포트 | 출발지 | 정책 |
|---|---|---|
| TCP 443 | Internet | 허용. HTTPS/WSS 고객 트래픽 |
| TCP 80 | Internet | 허용. HTTPS redirect와 ACME HTTP challenge |
| TCP 22 | 운영자 고정 IP 또는 Azure Bastion | 제한 허용. Internet 전체 공개 금지 |
| 13412, 19090, 5432, 19092, Docker API | 모든 외부 | 명시적으로 공개하지 않음 |

NSG는 VNet 리소스의 inbound/outbound 트래픽을 규칙으로 필터링한다. 세부 동작은
[Microsoft의 NSG 개요](https://learn.microsoft.com/en-us/azure/virtual-network/network-security-groups-overview)를
기준으로 한다.

Executor는 Claude API와 필요한 package registry에 outbound HTTPS로 접속해야 한다.
초기에는 outbound 443을 허용하고 관찰한 뒤 FQDN 기반 egress proxy 또는 Azure
Firewall 정책으로 좁힌다. 무작정 443을 차단하면 PAT introspection, ACR pull, Azure
Monitor, 인증과 Claude 실행이 함께 실패한다.

### DNS

정적 Public IP를 만든 뒤 다음 A record를 같은 IP로 연결한다.

- `relay.cookai.dev`
- `api-relay.cookai.dev`
- `admin-relay.cookai.dev`

DNS 전환 전 낮은 TTL을 사용하고 `curl --resolve` 또는 staging 전용 도메인으로 TLS와
라우팅을 먼저 확인한다. Traefik이 Let's Encrypt HTTP challenge를 처리하므로 인증서
발급 시 80/443과 DNS가 실제 VM을 가리켜야 한다.

## 6. 이미지와 ACR

배포 이미지는 최소 다음 세 개다.

- `pie-relay-server`
- `pie-executor-manager`
- `pie-relay-client`

운영에서는 `latest` 대신 Git commit SHA 또는 release version으로 고정한다.

```text
vibecanvascollab20260725.azurecr.io/pie-relay-server:<git-sha>
vibecanvascollab20260725.azurecr.io/pie-executor-manager:<git-sha>
vibecanvascollab20260725.azurecr.io/pie-relay-client:<git-sha>
```

기존 ACR를 staging에서 재사용할 수 있지만 다른 서비스 이미지와 retention/RBAC 경계를
공유한다. 고객 운영 전에는 다음 중 하나를 결정한다.

1. 기존 ACR를 유지하고 Pie 전용 repository scope, retention, immutable tag 정책을 둔다.
2. Pie 전용 ACR를 만들고 lifecycle과 권한을 완전히 분리한다.

VM에는 System Assigned Managed Identity를 부여하고 ACR에 pull 권한만 준다. ACR
password나 admin credential을 `.env`에 넣지 않는다. Azure가 안내하는
[VM Managed Identity 기반 ACR 인증](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-authentication-managed-identity)을
사용한다. 기존 ACR의 admin user 비활성화는 `vibe-collaboration`의 배포 방식을 먼저
확인한 후 별도 변경으로 수행한다.

## 7. Secret 관리

다음 값은 각각 독립적인 난수여야 한다.

- `POSTGRES_PASSWORD`
- `RELAY_JWT_SECRET`
- `HOST_ENROLL_SECRET`
- `PIE_MANAGER_ADMIN_TOKEN`
- `PIE_RELAY_CONTROL_TOKEN`
- `PIE_RELAY_PRESENCE_TOKEN`
- `PIE_RELAY_METRICS_TOKEN`
- `PIE_AUTH_CLIENT_SECRET`
- `PIE_USER_WEBHOOK_SECRET`

Key Vault는 soft delete와 purge protection을 켜고 VM Managed Identity에 필요한 secret의
`get` 권한만 부여한다. 이는 Microsoft의
[Key Vault 보안 권장사항](https://learn.microsoft.com/en-us/azure/key-vault/general/secure-key-vault)을
따른다.

Docker Compose는 Key Vault를 직접 읽지 못한다. 배포 시 다음 중 하나를 구현해야 한다.

1. systemd `ExecStartPre`가 Managed Identity로 Key Vault 값을 읽어 root 전용
   `/run/pie-relay/runtime.env`를 만들고 Compose를 실행한다.
2. CI/CD가 Azure VM Run Command 또는 SSH를 통해 임시 secret 파일을 만들고 배포 후
   안전하게 제거한다.

1번을 권장한다. 파일 권한은 `0600`, 소유자는 `root`, 위치는 tmpfs인 `/run`으로 하고
로그와 `docker inspect`에 필요 이상의 secret이 노출되지 않는지 점검한다. 장기적으로는
Compose secret/file mount로 전환한다.

## 8. PostgreSQL 전략

### staging

현재 `deploy/compose.yaml`의 PostgreSQL 컨테이너를 그대로 사용한다. VM data disk와
Azure VM Backup을 적용하고, 배포 전후 `pg_dump` 복원 훈련을 수행한다.

### 고객 운영

Azure Database for PostgreSQL Flexible Server로 분리하는 것을 권장한다. 자동 backup과
PITR, 선택적 zone-redundant HA를 제공하며 관련 동작은
[Flexible Server backup/restore](https://learn.microsoft.com/en-us/azure/postgresql/backup-restore/concepts-backup-restore)와
[고가용성 문서](https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/concepts-high-availability)를
기준으로 설계한다.

다만 현재 운영 Compose는 `PIE_CONTROL_DATABASE_URL`을 내부 PostgreSQL과
`sslmode=disable`로 고정한다. Managed PostgreSQL 전환 전 다음 코드/배포 보완이
필요하다.

- 외부 DSN을 secret으로 주입할 수 있는 Azure Compose override
- TLS 검증을 포함한 PostgreSQL 연결 문자열
- private endpoint와 private DNS 또는 엄격한 firewall rule
- migration/backup/restore staging 검증
- DB 일시 장애 중 Manager retry와 reconciliation 검증

## 9. 모니터링과 경보

기존 `workspace-rgjikime2Nxu` Log Analytics workspace를 초기에는 재사용한다.
Azure Monitor Agent와 Data Collection Rule로 다음을 수집한다. Microsoft는 VM guest
데이터 수집에 [Azure Monitor Agent](https://learn.microsoft.com/en-us/azure/azure-monitor/agents/azure-monitor-agent-overview)를
지원 경로로 제공한다.

- VM CPU, memory, disk latency/queue, network, inode와 filesystem 사용률
- Docker daemon 및 container restart/OOM 로그
- Traefik access/error 로그. 인증 token과 query parameter는 마스킹
- Relay/Manager 구조화 로그
- 배포 및 Key Vault 접근 감사 로그

필수 경보:

- VM unavailable, CPU 지속 80% 이상, memory pressure/OOM
- `/readyz` 실패 또는 컨테이너 unhealthy/restart loop
- data disk 또는 Docker disk byte/inode 80% 이상
- `pie_relay_slow_peer_evicted_total`, rate limit, reconnect 급증
- Manager provisioning queue 적체, operation 실패, Docker API 오류
- PostgreSQL connection/storage 부족과 backup 실패
- TLS 인증서 만료 30/14/7일 전

현재 운영 `deploy/compose.yaml`에는 Prometheus 서비스가 없고 local overlay에만 있다.
Azure 고객 공개 전에는 다음 중 하나를 반드시 추가한다.

1. token 인증된 Relay/Manager `/metrics`를 수집하는 host Prometheus와 Alertmanager
2. Azure Managed Prometheus/Grafana로 안전하게 scrape하는 구성

Prometheus UI와 `/metrics` token은 public ingress에 직접 노출하지 않는다.

## 10. 백업과 복구

Recovery Services Vault로 VM Backup을 구성한다. Azure VM Backup은 VM snapshot과
Recovery Services Vault의 복구 지점을 제공한다. 세부 방식은
[Azure VM Backup 개요](https://learn.microsoft.com/en-us/azure/backup/backup-azure-vms-introduction)를
참고한다.

VM snapshot만으로 application consistency를 단정하지 않는다. 다음을 함께 수행한다.

- PostgreSQL: 매일 logical backup, 배포 직전 on-demand backup, 정기 복원 훈련
- `/var/lib/pie-relay/workspaces`: 고객 정책에 따른 암호화 backup과 보존/삭제
- `/var/lib/pie-relay/executor-state`: Claude 인증 정보로 간주해 별도 접근 통제
- Relay mobile state: 유실 시 재페어링 절차 검증
- Key Vault: soft delete, purge protection, RBAC 감사

목표값은 운영 등급 확정 시 결정한다. 초기 제안은 staging RPO 24시간/RTO 4시간,
고객 운영 RPO 15분 이하/RTO 1시간 이하이며, 이 목표를 만족하려면 managed PostgreSQL과
자동화된 data disk 복구 절차가 필요하다.

## 11. 배포 순서

### 11.1 로컬 release gate

```bash
./deploy/local/pie-local.sh test

(cd server && go test -race ./...)
(cd client && go test -race ./...)
(cd executor-manager && go test -race ./...)
(cd desktop && npm test && npm run build)
(cd pie-mobile/adapter/host-gateway && pnpm typecheck && pnpm test)
```

### 11.2 Azure 기반 구성

1. Azure CLI context와 `rg-jikime`을 재확인한다.
2. Bicep 또는 Terraform으로 VNet, subnet, NSG, Public IP, NIC, VM, data disk,
   Managed Identity, Key Vault, Backup vault, Monitor 설정을 만든다.
3. SSH public key만 사용하고 password login과 root SSH를 끈다.
4. Docker Engine/Compose plugin을 공식 repository에서 설치하고 버전을 고정한다.
5. data disk를 마운트하고 `/etc/fstab`, ownership, Docker data-root를 검증한다.
6. VM identity에 ACR `AcrPull`, Key Vault secret read 최소권한을 부여한다.

### 11.3 애플리케이션 배포

1. 세 이미지를 ACR에 immutable tag로 push한다.
2. Azure용 Compose override에서 `build:` 대신 ACR image digest/tag를 지정한다.
3. `/opt/pie-relay`에 Compose와 Traefik 구성만 배포한다.
4. Key Vault에서 `/run/pie-relay/runtime.env`를 생성한다.
5. `docker compose config`로 secret 누락과 mount/network를 검증한다.
6. PostgreSQL → Relay → Manager → Traefik 순으로 health를 확인한다.
7. DNS 전환 후 ACME 인증서 발급과 HTTPS/WSS를 확인한다.

### 11.4 staging E2E

```bash
curl --fail https://relay.cookai.dev/readyz
curl --fail https://api-relay.cookai.dev/healthz
curl --fail -H "Authorization: Bearer $PIE_MANAGER_ADMIN_TOKEN" \
  https://api-relay.cookai.dev/v1/admin/overview
```

이후 실제 구성요소로 다음 조합을 검증한다.

1. Desktop host/clientd → Azure Relay → Desktop participant
2. 헤드리스 Linux clientd → Azure Relay → Desktop terminal
3. Mobile Gateway `relay-only` → Azure Relay → 실제 iPhone의 Wi-Fi
4. Mobile Gateway `relay-only` → Azure Relay → 실제 iPhone의 셀룰러
5. Viewer 입력 차단, Controller 입력, Driver 인계와 만료
6. 사용자별 Docker Executor 생성, 작업 실행, 재시작과 격리
7. Relay/Manager/VM 순차 재시작 후 자동 재연결
8. PAT 폐기, webhook replay/서명 오류, expired invite
9. backup을 별도 임시 VM/DB로 복원하는 훈련

## 12. 배포와 롤백 운영

- 배포 단위는 immutable image digest로 기록한다.
- 배포 전 현재 digest, Compose, Traefik config와 DB backup을 보존한다.
- `docker compose pull` 후 한 서비스씩 교체하고 health를 확인한다.
- Relay 교체 전 readiness drain을 사용한다.
- schema가 하위 호환될 때만 이전 Manager image로 rollback한다.
- Executor image는 사용자별 `runtime.recreate`로 점진 교체한다.
- rollback 후에도 mobile/clientd 재연결, Driver lease와 PTY snapshot을 확인한다.

단일 VM은 VM 자체가 장애 도메인이므로 무중단 HA를 제공하지 않는다. Azure Backup은
복구 수단이지 즉시 failover가 아니다.

## 13. 확장 단계

### 단계 A — 단일 VM staging

- 현재 Compose와 동일한 topology
- 기능/WAN/보안/backup 검증
- 고객 데이터 없음

### 단계 B — 초기 운영

- Managed PostgreSQL Flexible Server
- Key Vault 자동 주입
- ACR immutable release
- Azure Monitor와 Prometheus 경보
- VM/data disk backup 및 복원 훈련

### 단계 C — 수평 확장

- Relay node 다중화와 node assignment 기반 room affinity
- Relay drain/migration과 mobile resume state 외부 저장소 검토
- Manager API와 Executor worker 역할 분리
- 사용자/session을 특정 worker VM에 배치하는 scheduler
- 여러 worker에서 Docker socket을 로컬로만 소유
- Front Door/Application Gateway 또는 L4/L7 load balancer 검토

Relay room은 현재 프로세스 메모리에 있으므로 단순 round-robin으로 Relay VM을 늘리면
같은 방의 host와 participant가 다른 node에 들어갈 수 있다. 다중 Relay는 반드시
assignment/coordinator와 sticky routing이 준비된 뒤 적용한다. Manager 역시 한 Docker
host의 socket을 제어하므로 VMSS에 복제하는 것만으로 Executor가 분산되지 않는다.

## 14. Azure 배포 전에 구현할 항목

현재 로컬 기능이 통과했더라도 아래 항목은 Azure 배포 자동화를 위해 남아 있다.

- [ ] Bicep/Terraform 기반 `rg-jikime` IaC
- [ ] ACR image tag/digest를 사용하는 Azure Compose override
- [ ] Managed Identity + Key Vault runtime env loader와 systemd unit
- [ ] 외부 PostgreSQL DSN/TLS/private endpoint 지원
- [ ] 운영 Prometheus 또는 Azure Managed Prometheus scrape 구성
- [ ] Azure VM/data disk/PostgreSQL backup 및 자동 복원 훈련 스크립트
- [ ] log redaction과 Log Analytics DCR
- [ ] NSG, SSH/Bastion, Defender for Cloud 보안 기준
- [ ] 실제 iPhone Wi-Fi/셀룰러와 Azure WSS E2E
- [ ] Azure 부하 테스트: 동시 WebSocket, room fan-out, Executor provisioning
- [ ] 운영 runbook: 장애, secret rotation, 인증서, disk full, rollback

## 15. Go/No-Go 기준

다음을 모두 만족하기 전에는 고객 트래픽을 받지 않는다.

- 로컬 full-stack test와 모든 unit/race test 통과
- Azure staging에서 LAN/Relay, Desktop/clientd/mobile 실제 연결 통과
- Relay/Manager/VM 재시작 후 세션 복구 통과
- PAT, role, invite, Driver 권한 부정 테스트 통과
- 예상 동시 사용자 2배 부하에서 p95 목표와 오류율 충족
- CPU/memory/disk/egress 비용과 Executor 수용량 측정 완료
- Key Vault 이외에 운영 secret 원문이 남지 않음
- backup 복원과 이전 image rollback 훈련 통과
- 경보가 실제 운영 채널로 전달되는지 확인
- DNS, TLS, 개인정보/인증정보 보존 정책 승인

## 16. 관련 문서

- [기존 배포 및 운영 가이드](./deployment-and-operations.md)
- [고객 배포 준비 상태와 릴리스 게이트](./release-readiness.md)
- [DNS 없는 로컬 통합 환경](../deploy/local/README.md)
- [Relay 안정화 설계](./relay-hardening.md)
- [세션 모드와 Docker 격리](./session-modes-and-mutual-access.md)
- [연결 구조와 사용 흐름](./how-to-connect.md)
