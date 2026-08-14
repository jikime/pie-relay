#!/usr/bin/env bash
set -euo pipefail

# Event Manager 호스트에서 Claude 구독용 장기 OAuth 토큰을 생성하고 중앙
# Broker에 게시한다. 토큰은 Docker image/Compose env/argv/API body/사용자 HOME에
# 저장하지 않는다. 게시 전 후보 파일도 Manager 데이터 영역에 0600으로만 둔다.

data_dir=${PIE_DATA_DIR:-${PIE_TEST_DATA_DIR:-/var/lib/pie-relay}}
auth_dir=${PIE_CLAUDE_AUTH_DIR:-${data_dir}/claude-auth}
login_dir=${PIE_CLAUDE_AUTH_LOGIN_DIR:-${auth_dir}/login}
executor_image=${PIE_EXECUTOR_IMAGE:-pie-relay-client-kroot:test}
manager_url=${PIE_MANAGER_URL:-}
admin_token_file=${PIE_MANAGER_ADMIN_TOKEN_FILE:-}
container_identity=${PIE_EXECUTOR_CONTAINER_USER:-10001:10001}
label=${PIE_CLAUDE_AUTH_LABEL:-subscription-oauth-$(date -u +%Y%m%dT%H%M%SZ)}
publish=${PIE_CLAUDE_AUTH_PUBLISH:-true}

case ${data_dir} in /*) ;; *) echo "PIE_DATA_DIR must be an absolute path" >&2; exit 2 ;; esac
case ${login_dir} in /*) ;; *) echo "PIE_CLAUDE_AUTH_LOGIN_DIR must be an absolute path" >&2; exit 2 ;; esac
if [[ ! -d ${data_dir} ]]; then
  echo "PIE_DATA_DIR is unavailable: ${data_dir}" >&2
  exit 2
fi
case ${login_dir} in
  "${data_dir}"/*) login_relative=${login_dir#"${data_dir}"/} ;;
  *) echo "PIE_CLAUDE_AUTH_LOGIN_DIR must stay under PIE_DATA_DIR" >&2; exit 2 ;;
esac
case ${login_relative} in ''|..|../*|*/../*) echo "PIE_CLAUDE_AUTH_LOGIN_DIR contains an unsafe path" >&2; exit 2 ;; esac
case ${container_identity} in
  *[!0-9:]*|:*|*:|*:*:*) echo "PIE_EXECUTOR_CONTAINER_USER must use numeric uid:gid form" >&2; exit 2 ;;
  *:*) ;;
  *) echo "PIE_EXECUTOR_CONTAINER_USER must use uid:gid form" >&2; exit 2 ;;
esac
container_uid=${container_identity%%:*}
container_gid=${container_identity#*:}
case ${label} in *[!A-Za-z0-9._-]*) echo "PIE_CLAUDE_AUTH_LABEL may contain only letters, numbers, dot, underscore, and dash" >&2; exit 2 ;; esac
if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required on the Event Manager host" >&2
  exit 2
fi
if ! docker image inspect "${executor_image}" >/dev/null 2>&1; then
  echo "Executor image is unavailable: ${executor_image}" >&2
  exit 2
fi

docker run --rm --user 0:0 --entrypoint /bin/sh \
  -v "${data_dir}:/pie-data" \
  "${executor_image}" -c 'target="/pie-data/$1"; mkdir -p "$target"; chown "$2:$3" "$target"; chmod 0700 "$target"' \
  _ "${login_relative}" "${container_uid}" "${container_gid}"

echo "Event Manager 전용 Claude 구독 토큰 발급을 시작합니다."
echo "브라우저 안내를 마치면 표시되는 setup-token을 복사해 주세요."
docker run --rm -it \
  --user "${container_identity}" \
  -e HOME=/home/executor \
  "${executor_image}" claude setup-token

printf '발급된 setup-token을 붙여넣고 Enter를 누르세요: ' >&2
IFS= read -r -s setup_token
printf '\n' >&2
if [[ ${#setup_token} -lt 20 || ${#setup_token} -gt 32768 || ${setup_token} =~ [[:space:]] ]]; then
  unset setup_token
  echo "setup-token 형식이 올바르지 않습니다." >&2
  exit 2
fi

# 호스트 셸의 argv나 일반 임시 파일을 거치지 않고 stdin으로만 후보를 쓴다.
printf '%s' "${setup_token}" | docker run --rm -i --user 0:0 --entrypoint /bin/sh \
  -v "${data_dir}:/pie-data" \
  "${executor_image}" -c 'umask 077; target="/pie-data/$1/setup-token"; cat >"$target"; chown "$2:$3" "$target"; chmod 0600 "$target"' \
  _ "${login_relative}" "${container_uid}" "${container_gid}"
unset setup_token
echo "암호화 게시용 후보를 안전하게 준비했습니다."

if [[ ${publish} != true ]]; then
  echo "PIE_CLAUDE_AUTH_PUBLISH=false: Manager 게시를 생략했습니다."
  exit 0
fi
if [[ -z ${manager_url} || -z ${admin_token_file} ]]; then
  echo "후보 생성은 완료됐습니다. 자동 게시에는 PIE_MANAGER_URL과 PIE_MANAGER_ADMIN_TOKEN_FILE이 필요합니다." >&2
  exit 2
fi
if [[ ! -f ${admin_token_file} ]]; then
  echo "Manager admin token file is unavailable" >&2
  exit 2
fi

curl_config=$(mktemp)
response_file=$(mktemp)
cleanup() { rm -f "${curl_config}" "${response_file}"; }
trap cleanup EXIT
chmod 0600 "${curl_config}" "${response_file}"
admin_token=$(<"${admin_token_file}")
cat >"${curl_config}" <<EOF
silent
show-error
fail-with-body
connect-timeout = 10
max-time = 600
header = "Authorization: Bearer ${admin_token}"
header = "Content-Type: application/json"
header = "Idempotency-Key: claude-auth-${label}"
EOF
unset admin_token

curl --config "${curl_config}" \
  --request POST \
  --data "{\"label\":\"${label}\",\"restart\":true}" \
  --output "${response_file}" \
  "${manager_url%/}/v1/admin/claude-auth/publish"

echo "구독 OAuth 버전 게시와 Executor 세션 재조정 요청을 완료했습니다."
