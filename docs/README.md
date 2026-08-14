# Pie Relay 문서

- [Preview SSL·실행 운영 Runbook](./preview-ssl-runtime-runbook.md)
- [운영 보강 10개 항목과 TODO](./production-hardening-roadmap.md)

- [프로젝트 웹 프리뷰 설계·운영 가이드](./project-preview-platform.md)

- [세션·실행 환경·Control Plane 정의](./session-runtime-and-control-plane.md)
- [Pie Relay 연결 구조와 사용 흐름](./how-to-connect.md)
- [Pie Client GitHub Pages 설치·릴리스 운영](./pie-client-github-distribution.md)
- [Relay URL 설정](./relay-url-configuration.md)
- [Pie Relay 안정화 설계 및 운영 기준](./relay-hardening.md)
- [Relay·clientd 연결 격리와 복구 운영 가이드](./relay-client-connection-isolation-and-recovery.md)
- [Pie Relay 간헐적 연결 장애 및 Claude 구독 중앙 공유 개선 보고서 — 2026-08-12](./relay-intermittent-connection-and-shared-subscription-report-2026-08-12.md)
- [세션 모드, Docker 격리 및 상호 접속 설계](./session-modes-and-mutual-access.md)
- [사용자 샌드박스 아키텍처와 검증 기록](./sandbox-architecture.md)
- [Sandbox 테스트 서버 배포·검증 기록](./sandbox-test-server-plan.md)
- [CookAI 공식 도메인 전환 및 Pie Staging 검증 보고서](./cookai-domain-cutover-and-staging-validation-report.md)
- [Kroot 서버 단계 배포 결과 — 2026-08-04](./kroot-staged-deployment-report-2026-08-04.md)
- [Kroot Event Manager 인증 배포·E2E 결과 — 2026-08-10](./kroot-event-manager-auth-deployment-2026-08-10.md)
- [사용자별 Executor 컨테이너·인증 프로비저닝 설계](./executor-container-auth-provisioning.md)
- [Event Manager 기반 Claude 구독 OAuth 운영](./event-manager-claude-auth.md)
- [Pie Relay Control Plane 및 관리 콘솔 설계](./control-plane-and-admin-console.md)
- [제3의 애플리케이션 ↔ Docker Claude Code 연동 설계](./third-party-application-relay-integration.md)
- [제3자 애플리케이션용 사용자 전용 AI 채팅 구현 명세](./third-party-ai-chat-implementation.md)
- [Claude Code 서브에이전트 실시간 스트리밍 설계](./claude-subagent-streaming.md)
- [로컬 제3자 채팅·Claude 사용량 E2E 가이드](./local-ai-usage-e2e.md)
- [독립 제3자 웹채팅 실행 예제](../examples/third-party-web-chat/README.md)
- [Relay 및 Executor Manager 아키텍처](./relay-and-executor-manager.md)
- [인증 및 페어링 흐름](./relay-authentication.md)
- [Executor Manager 운영 계획](./executor-manager-operations.md)
- [배포 및 운영 가이드](./deployment-and-operations.md)
- [원격 프로젝트 파일 편집기](./remote-workspace-editor.md)
- [Kroot 원격 Monaco 편집기 배포·E2E 결과 — 2026-08-13](./kroot-workspace-editor-deployment-2026-08-13.md)
- [Azure 배포 계획](./azure-deployment-plan.md)
- [Azure Relay-only staging 운영 기록](../deploy/azure/README.md)
- [DNS 없는 로컬 통합 환경](../deploy/local/README.md)
- [Vibe Canvas·Kroot Studio 서비스별 배포 프로필](../deploy/profiles/README.md)
- [Kroot Studio·Vibe Canvas 제품 분리 및 연결 운영 기록](./kroot-studio-vibe-canvas-product-isolation.md)
- [고객 배포 준비 상태와 릴리스 게이트](./release-readiness.md)

현재 고객 경로의 기준 구현은 외부 PAT introspection, 최소권한 Relay presence,
사용자 lifecycle webhook, Local/Docker 세션, session-scoped view/control grant,
단일 Driver lease, 실제 drain, Docker 자동 복구와 운영 콘솔을 포함한다. 모바일
Gateway의 LAN/Relay 연결과 Generic Desktop/clientd의 Relay 연결은 서로 다른
transport 경로이므로 운영 및 테스트에서도 구분한다.
