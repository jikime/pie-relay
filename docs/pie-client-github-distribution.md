# Pie Client GitHub Pages 설치·릴리스 운영

## 공개 주소

```text
저장소: https://github.com/jikime/pie-relay
설치 안내: https://jikime.github.io/pie-relay/
설치 스크립트: https://jikime.github.io/pie-relay/install.sh
```

일반 사용자는 다음 한 줄로 설치한다.

```bash
curl -fsSL https://jikime.github.io/pie-relay/install.sh | sh
```

`install.sh`만 GitHub Pages에서 제공하며, 실제 실행 패키지와 체크섬은 GitHub Release에서
제공한다. 정적 페이지가 변해도 이미 발행된 버전 패키지는 그대로 유지된다.

## 지원 환경과 설치 내용

| 운영체제 | CPU | Release 자산 |
|---|---|---|
| Linux | x86-64 | `pie-client_linux_amd64.tar.gz` |
| Linux | ARM64 | `pie-client_linux_arm64.tar.gz` |
| macOS | Intel | `pie-client_darwin_amd64.tar.gz` |
| macOS | Apple Silicon | `pie-client_darwin_arm64.tar.gz` |

각 패키지는 다음을 포함한다.

- 버전·커밋·빌드 시각이 주입된 `pie-client` Go 바이너리
- Claude Agent SDK, ACP와 PTY 어댑터 소스
- 해당 운영체제와 CPU에서 미리 설치·검증한 `node_modules`
- 패키지 버전과 네이티브 런타임 플랫폼 표식

사용자 컴퓨터에는 Node.js 22 이상이 필요하지만, 정상 Release는 npm 컴파일을 다시
수행하지 않는다. 구형 호환 패키지에 네이티브 런타임이 없을 때만 lockfile 기반
`npm ci`를 폴백으로 사용한다.

## 설치 보안 경계

설치 프로그램은 다음 순서로 동작한다.

1. `uname`으로 운영체제와 CPU를 판별한다.
2. GitHub Release에서 패키지와 `pie-client_checksums.txt`를 받는다.
3. 로컬에서 계산한 SHA-256과 Release 값을 비교한다.
4. 패키지 내부 버전과 요청한 태그가 같은지 확인한다.
5. 네이티브 Node 런타임이 현재 플랫폼에서 로드되는지 검사한다.
6. 버전과 체크섬이 포함된 불변 디렉터리에 설치한다.
7. `~/.local/bin/pie-client` 링크를 새 버전으로 원자적으로 교체한다.
8. 설치된 바이너리의 `pie-client version` 실행이 성공해야 완료한다.

토큰, 연결 코드, Claude/Kroot 자격은 설치 파일이나 GitHub Release에 포함하지 않는다.

## 버전 고정과 경로 변경

파이프 실행보다 내용을 먼저 확인하려면 다음처럼 내려받아 실행한다.

```bash
curl -fsSL https://jikime.github.io/pie-relay/install.sh -o install-pie-client.sh
less install-pie-client.sh
sh install-pie-client.sh --version v1.0.0
```

```bash
# 실행 링크 위치 변경
sh install-pie-client.sh --install-dir "$HOME/bin"

# 버전 데이터 위치 변경
sh install-pie-client.sh --install-root "$HOME/.pie-client"
```

기본 `~/.local/bin`이 `PATH`에 없으면 설치 프로그램이 추가할 설정을 안내한다.

## 릴리스 발행

`.github/workflows/build.yml`은 `v*` 태그에서 Desktop과 Pie Client를 함께 빌드하고
하나의 GitHub Release에 게시한다. Pie Client 패키지는 GitHub 공식 네이티브 러너에서
각각 생성되므로 `node-pty`와 플랫폼별 Claude Agent SDK가 대상 환경과 일치한다.

```bash
# 모든 테스트가 통과하고 main이 원격과 일치하는지 확인한 뒤
git tag -a v1.0.0 -m "Pie Relay v1.0.0"
git push origin v1.0.0
```

릴리스가 끝나면 다음을 확인한다.

```bash
curl -fsSL https://jikime.github.io/pie-relay/install.sh -o /tmp/install-pie-client.sh
sh /tmp/install-pie-client.sh --version v1.0.0
pie-client version
```

`latest` 설치는 GitHub가 최신 정식 Release로 판단한 태그를 사용한다. 시험판은 GitHub
Release에서 prerelease로 표시하고 운영 설치 명령에는 사용하지 않는다.

## 자동화 검증

- `scripts/e2e/pie-client-installer.mjs`: 최신 버전, 고정 버전, SHA-256 변조 차단
- `.github/workflows/test.yml`: 모든 push/PR에서 설치 E2E 수행
- `.github/workflows/pages.yml`: `main`의 설치 페이지와 스크립트를 GitHub Pages에 배포
- `.github/workflows/build.yml`: 네이티브 런타임 로드와 설치 패키지 실행을 릴리스 전에 검증

