#!/bin/sh

# Pie Client installer for macOS and Linux.
# Published at https://jikime.github.io/pie-relay/install.sh

set -eu

REPOSITORY="jikime/pie-relay"
DEFAULT_RELEASE_BASE_URL="https://github.com/${REPOSITORY}/releases"
requested_version="latest"
install_root="${PIE_CLIENT_HOME:-${XDG_DATA_HOME:-${HOME}/.local/share}/pie-client}"
bin_dir="${PIE_CLIENT_BIN_DIR:-${HOME}/.local/bin}"
release_base_url="${PIE_CLIENT_RELEASE_BASE_URL:-${DEFAULT_RELEASE_BASE_URL}}"
tmp_dir=""

say() {
  printf '%s\n' "$*"
}

die() {
  printf 'Pie Client 설치 실패: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Pie Client 설치 프로그램

사용법:
  install.sh [--version VERSION] [--install-dir PATH] [--install-root PATH]

옵션:
  --version VERSION   설치할 GitHub Release 태그. 기본값: latest
  --install-dir PATH  pie-client 실행 링크를 둘 디렉터리. 기본값: ~/.local/bin
  --bin-dir PATH      --install-dir와 같음
  --install-root PATH 버전별 런타임 저장 위치. 기본값: ~/.local/share/pie-client
  -h, --help          도움말

환경변수:
  PIE_CLIENT_HOME              --install-root 기본값
  PIE_CLIENT_BIN_DIR           --install-dir 기본값
  PIE_CLIENT_RELEASE_BASE_URL  테스트·사설 미러용 Release 기준 URL
EOF
}

cleanup() {
  if [ -n "$tmp_dir" ] && [ -d "$tmp_dir" ]; then
    rm -rf "$tmp_dir"
  fi
}
trap cleanup EXIT HUP INT TERM

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || die "--version 뒤에 버전이 필요합니다"
      requested_version=$2
      shift 2
      ;;
    --install-dir|--bin-dir)
      [ "$#" -ge 2 ] || die "$1 뒤에 경로가 필요합니다"
      bin_dir=$2
      shift 2
      ;;
    --install-root)
      [ "$#" -ge 2 ] || die "--install-root 뒤에 경로가 필요합니다"
      install_root=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "알 수 없는 옵션입니다: $1"
      ;;
  esac
done

[ -n "${HOME:-}" ] || die "HOME 환경변수가 필요합니다"
[ -n "$install_root" ] || die "설치 루트가 비어 있습니다"
[ -n "$bin_dir" ] || die "실행 파일 경로가 비어 있습니다"

case "$requested_version" in
  latest) ;;
  v[0-9]*)
    case "$requested_version" in
      *[!A-Za-z0-9._-]*) die "버전에는 영문자, 숫자, 점, 밑줄, 하이픈만 사용할 수 있습니다" ;;
    esac
    ;;
  *) die "버전은 latest 또는 v로 시작하는 Release 태그여야 합니다" ;;
esac

for command_name in curl tar awk node; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name 명령이 필요합니다"
done

node_major=$(node -p "Number(process.versions.node.split('.')[0])" 2>/dev/null || true)
case "$node_major" in
  ''|*[!0-9]*) die "Node.js 버전을 확인할 수 없습니다" ;;
esac
[ "$node_major" -ge 22 ] || die "Node.js 22 이상이 필요합니다. 현재 버전: $(node --version 2>/dev/null || printf 'unknown')"

case "$(uname -s)" in
  Darwin) target_os="darwin" ;;
  Linux) target_os="linux" ;;
  *) die "지원하지 않는 운영체제입니다: $(uname -s). macOS와 Linux를 지원합니다" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) target_arch="amd64" ;;
  arm64|aarch64) target_arch="arm64" ;;
  *) die "지원하지 않는 CPU 아키텍처입니다: $(uname -m)" ;;
esac

asset="pie-client_${target_os}_${target_arch}.tar.gz"
if [ "$requested_version" = "latest" ]; then
  download_base="${release_base_url%/}/latest/download"
else
  download_base="${release_base_url%/}/download/${requested_version}"
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/pie-client-install.XXXXXX") || die "임시 디렉터리를 만들 수 없습니다"
archive="$tmp_dir/$asset"
checksums="$tmp_dir/pie-client_checksums.txt"
payload="$tmp_dir/payload"

say "Pie Client ${requested_version} 패키지를 확인합니다 (${target_os}/${target_arch})."
curl --fail --silent --show-error --location --retry 3 --connect-timeout 15 \
  --output "$checksums" "$download_base/pie-client_checksums.txt"
curl --fail --silent --show-error --location --retry 3 --connect-timeout 15 \
  --output "$archive" "$download_base/$asset"

expected_checksum=$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset { print $1; exit }' "$checksums")
[ -n "$expected_checksum" ] || die "체크섬 목록에 $asset 항목이 없습니다"
case "$expected_checksum" in
  *[!0-9A-Fa-f]*|'') die "Release 체크섬 형식이 올바르지 않습니다" ;;
esac
[ "${#expected_checksum}" -eq 64 ] || die "Release 체크섬 길이가 올바르지 않습니다"

if command -v sha256sum >/dev/null 2>&1; then
  actual_checksum=$(sha256sum "$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual_checksum=$(shasum -a 256 "$archive" | awk '{print $1}')
else
  die "SHA-256 검증을 위한 sha256sum 또는 shasum이 필요합니다"
fi
[ "$actual_checksum" = "$expected_checksum" ] || die "다운로드한 패키지의 SHA-256이 일치하지 않습니다"

mkdir -p "$payload"
tar -xzf "$archive" -C "$payload"
for required_path in \
  "$payload/pie-client-bin" \
  "$payload/VERSION" \
  "$payload/node-executor/package.json" \
  "$payload/node-executor/package-lock.json" \
  "$payload/node-executor/executor.mjs" \
  "$payload/node-executor/acp-executor.mjs" \
  "$payload/node-executor/pty-host.mjs"; do
  [ -f "$required_path" ] || die "Release 패키지에 필수 파일이 없습니다: ${required_path#$payload/}"
done

installed_version=$(tr -d '\r\n' < "$payload/VERSION")
case "$installed_version" in
  v[0-9]*|dev-*) ;;
  *) die "Release 패키지의 VERSION 값이 올바르지 않습니다" ;;
esac
case "$installed_version" in
  *[!A-Za-z0-9._-]*) die "Release 패키지의 VERSION 값이 안전하지 않습니다" ;;
esac
if [ "$requested_version" != "latest" ] && [ "$installed_version" != "$requested_version" ]; then
  die "요청한 버전($requested_version)과 패키지 버전($installed_version)이 다릅니다"
fi

say "플랫폼별 Node Executor 런타임을 확인합니다."
runtime_ready=false
runtime_platform=""
if [ -f "$payload/RUNTIME_READY" ]; then
  runtime_platform=$(tr -d '\r\n' < "$payload/RUNTIME_READY")
fi
if [ "$runtime_platform" = "$target_os/$target_arch" ] \
  && [ -d "$payload/node-executor/node_modules" ] \
  && [ -x "$payload/node-executor/node_modules/.bin/claude-agent-acp" ] \
  && (cd "$payload/node-executor" && node -e "require('node-pty')" >/dev/null 2>&1); then
  runtime_ready=true
fi
if [ "$runtime_ready" != true ]; then
  command -v npm >/dev/null 2>&1 || die "패키지에 네이티브 런타임이 없어서 npm 명령이 필요합니다"
  say "이 패키지는 런타임 호환 설치가 필요합니다. 고정된 npm lockfile을 사용합니다."
  if ! (cd "$payload/node-executor" && npm ci --omit=dev --no-audit --no-fund); then
    die "Node Executor 의존성 설치에 실패했습니다. Node.js 22와 네이티브 모듈 빌드 환경을 확인해 주세요"
  fi
fi

checksum_prefix=$(printf '%s' "$expected_checksum" | cut -c1-12)
release_id="${installed_version}-${checksum_prefix}"
versions_dir="$install_root/versions"
target_dir="$versions_dir/$release_id"
staging_dir="$versions_dir/.${release_id}.tmp.$$"

mkdir -p "$versions_dir" "$bin_dir"
if [ ! -d "$target_dir" ]; then
  rm -rf "$staging_dir"
  mkdir -p "$staging_dir"
  cp -R "$payload/." "$staging_dir/"
  chmod 0755 "$staging_dir/pie-client-bin"
  cat > "$staging_dir/pie-client-launcher" <<'EOF'
#!/bin/sh
set -eu

self=$0
while [ -L "$self" ]; do
  self_dir=$(CDPATH= cd -P "$(dirname "$self")" && pwd)
  link_target=$(readlink "$self")
  case "$link_target" in
    /*) self=$link_target ;;
    *) self=$self_dir/$link_target ;;
  esac
done
runtime_dir=$(CDPATH= cd -P "$(dirname "$self")" && pwd)

if [ -z "${EXECUTOR_PATH:-}" ]; then EXECUTOR_PATH="$runtime_dir/node-executor/executor.mjs"; export EXECUTOR_PATH; fi
if [ -z "${ACP_EXECUTOR_PATH:-}" ]; then ACP_EXECUTOR_PATH="$runtime_dir/node-executor/acp-executor.mjs"; export ACP_EXECUTOR_PATH; fi
if [ -z "${PTY_HOST_PATH:-}" ]; then PTY_HOST_PATH="$runtime_dir/node-executor/pty-host.mjs"; export PTY_HOST_PATH; fi
if [ -z "${PIE_ACP_AGENT_COMMAND:-}" ] && [ -x "$runtime_dir/node-executor/node_modules/.bin/claude-agent-acp" ]; then
  PIE_ACP_AGENT_COMMAND="$runtime_dir/node-executor/node_modules/.bin/claude-agent-acp"
  export PIE_ACP_AGENT_COMMAND
fi
PATH="$runtime_dir/node-executor/node_modules/.bin:$PATH"
export PATH

exec "$runtime_dir/pie-client-bin" "$@"
EOF
  chmod 0755 "$staging_dir/pie-client-launcher"
  mv "$staging_dir" "$target_dir"
fi

client_link="$bin_dir/pie-client"
if [ -d "$client_link" ] && [ ! -L "$client_link" ]; then
  die "$client_link 경로가 디렉터리라서 교체할 수 없습니다"
fi
link_tmp="$bin_dir/.pie-client-link.$$"
rm -f "$link_tmp"
ln -s "$target_dir/pie-client-launcher" "$link_tmp"
mv -f "$link_tmp" "$client_link"

current_tmp="$install_root/.current.$$"
rm -f "$current_tmp"
ln -s "$target_dir" "$current_tmp"
rm -f "$install_root/current"
mv -f "$current_tmp" "$install_root/current"

installed_output=$($client_link version 2>/dev/null) || die "설치된 Pie Client 실행 검증에 실패했습니다"
say "$installed_output"
say "설치 완료: $client_link"

case ":${PATH:-}:" in
  *":$bin_dir:"*) ;;
  *)
    say "현재 PATH에 $bin_dir 경로가 없습니다. 셸 설정에 다음 줄을 추가해 주세요:"
    say "  export PATH=\"$bin_dir:\$PATH\""
    ;;
esac
