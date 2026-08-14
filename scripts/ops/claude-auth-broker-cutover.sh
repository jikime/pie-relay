#!/usr/bin/env bash
set -euo pipefail

# 기존 사용자별 Claude 인증에서 Event Manager 중앙 setup-token Broker로
# 전환한다. OAuth 승인/코드 입력만 운영자가 같은 터미널에서 수행하고,
# Manager 교체·암호화 게시·Executor 재조정은 한 트랜잭션 경계로 실행한다.

umask 077

release_dir=${PIE_RELEASE_DIR:?set PIE_RELEASE_DIR to the prepared release directory}
stack_root=${PIE_STACK_ROOT:-/home/kaonkroot/pie-sandbox-test}
current_link=${PIE_CURRENT_LINK:-${stack_root}/current}
previous_link=${PIE_PREVIOUS_LINK:-${stack_root}/previous}
data_dir=${PIE_DATA_DIR:-${stack_root}/data}
project=${PIE_COMPOSE_PROJECT:-pie-sandbox-test}
admin_url=${PIE_MANAGER_ADMIN_URL:?set PIE_MANAGER_ADMIN_URL, for example https://admin-relay-test.cookai.dev}
health_url=${PIE_MANAGER_HEALTH_URL:?set PIE_MANAGER_HEALTH_URL, for example https://api-relay-test.cookai.dev/readyz}
label=${PIE_CLAUDE_AUTH_LABEL:-subscription-oauth-$(date -u +%Y%m%dT%H%M%SZ)}

case ${release_dir} in /*) ;; *) echo "PIE_RELEASE_DIR must be absolute" >&2; exit 2 ;; esac
case ${stack_root} in /*) ;; *) echo "PIE_STACK_ROOT must be absolute" >&2; exit 2 ;; esac
case ${data_dir} in /*) ;; *) echo "PIE_DATA_DIR must be absolute" >&2; exit 2 ;; esac
case ${label} in *[!A-Za-z0-9._-]*) echo "PIE_CLAUDE_AUTH_LABEL contains unsafe characters" >&2; exit 2 ;; esac

deploy_dir=${release_dir}/src/deploy/test-server
compose_file=${deploy_dir}/compose.yaml
env_file=${deploy_dir}/.env
login_helper=${release_dir}/src/scripts/ops/claude-auth-login.sh
for path in "${compose_file}" "${env_file}" "${login_helper}"; do
  if [[ ! -f ${path} ]]; then
    echo "prepared release file is unavailable: ${path}" >&2
    exit 2
  fi
done
for command in docker jq curl; do
  if ! command -v "${command}" >/dev/null 2>&1; then
    echo "${command} is required" >&2
    exit 2
  fi
done

old_release=$(readlink -f "${current_link}")
old_deploy=${old_release}/src/deploy/test-server
if [[ ! -f ${old_deploy}/compose.yaml || ! -f ${old_deploy}/.env ]]; then
  echo "current release is not recoverable: ${old_release}" >&2
  exit 2
fi

compose=(docker compose --project-name "${project}" --env-file "${env_file}" -f "${compose_file}" --profile web-chat)
old_compose=(docker compose --project-name "${project}" --env-file "${old_deploy}/.env" -f "${old_deploy}/compose.yaml" --profile web-chat)
"${compose[@]}" config --quiet
config_json=$("${compose[@]}" config --format json)
manager_image=$(jq -er '.services.manager.image' <<<"${config_json}")
executor_image=$(jq -er '.services.manager.environment.PIE_EXECUTOR_IMAGE' <<<"${config_json}")
unset config_json
docker image inspect "${manager_image}" "${executor_image}" >/dev/null

if [[ ${PIE_CUTOVER_PREFLIGHT_ONLY:-false} == true ]]; then
  printf 'preflight ok\nManager: %s\nExecutor: %s\nCurrent: %s\n' "${manager_image}" "${executor_image}" "${old_release}"
  exit 0
fi

backup_root=${stack_root}/backups/$(date -u +%Y%m%dT%H%M%SZ)-claude-oauth-broker
mkdir -p "${backup_root}"
chmod 0700 "${backup_root}"
# Manager가 root 권한으로 만든 0700 인증 저장소는 SSH 배포 계정이 직접
# 읽지 못하는 것이 정상이다. Docker socket 권한을 이용한 짧은 read-only
# 백업 작업으로만 접근하며, 내용은 argv나 로그에 출력하지 않는다.
docker run --rm --user 0:0 --entrypoint /bin/sh \
  -v "${data_dir}:/pie-data:ro" \
  -v "${backup_root}:/backup" \
  alpine:3.21 -c 'if [ -d /pie-data/claude-auth ]; then cp -a /pie-data/claude-auth /backup/claude-auth; fi'
printf '%s\n' "${old_release}" >"${backup_root}/previous-release"

echo "[1/5] Event Manager 전용 Claude setup-token을 준비합니다."
PIE_DATA_DIR="${data_dir}" \
PIE_CLAUDE_AUTH_DIR="${data_dir}/claude-auth" \
PIE_CLAUDE_AUTH_LOGIN_DIR="${data_dir}/claude-auth/login" \
PIE_EXECUTOR_IMAGE="${executor_image}" \
PIE_CLAUDE_AUTH_LABEL="${label}" \
PIE_CLAUDE_AUTH_PUBLISH=false \
  "${login_helper}"

switched=false
publish_started=false
restore_before_publish() {
  exit_code=$?
  if [[ ${switched} == true && ${publish_started} == false ]]; then
    echo "새 Manager 게시 전 실패를 감지해 기존 Manager를 복구합니다." >&2
    "${old_compose[@]}" up -d --no-deps manager >/dev/null || true
  fi
  exit "${exit_code}"
}
trap restore_before_publish ERR INT TERM

echo "[2/5] 새 중앙 Broker Manager로 교체합니다."
"${compose[@]}" up -d --no-deps manager
switched=true
manager_id=$("${compose[@]}" ps -q manager)
if [[ -z ${manager_id} ]]; then
  echo "Manager container was not created" >&2
  false
fi

for _ in $(seq 1 60); do
  health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${manager_id}" 2>/dev/null || true)
  [[ ${health} == healthy || ${health} == running ]] && break
  sleep 2
done
if [[ ${health:-} != healthy && ${health:-} != running ]]; then
  echo "new Manager did not become healthy" >&2
  false
fi
for _ in $(seq 1 30); do
  code=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --connect-timeout 5 --max-time 10 "${health_url}" || true)
  [[ ${code} == 200 ]] && break
  sleep 2
done
if [[ ${code:-} != 200 ]]; then
  echo "public Manager health check failed: ${health_url}" >&2
  false
fi

admin_token=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "${manager_id}" | sed -n 's/^PIE_EXECUTOR_MANAGER_TOKEN=//p' | head -1)
if [[ -z ${admin_token} ]]; then
  echo "Manager admin token is unavailable" >&2
  false
fi
curl_config=$(mktemp "${backup_root}/curl-config.XXXXXX")
response_file=$(mktemp "${backup_root}/publish-response.XXXXXX")
chmod 0600 "${curl_config}" "${response_file}"
cat >"${curl_config}" <<EOF
silent
show-error
fail-with-body
connect-timeout = 10
max-time = 900
header = "Authorization: Bearer ${admin_token}"
header = "Content-Type: application/json"
header = "Idempotency-Key: claude-auth-${label}"
EOF
unset admin_token

echo "[3/5] setup-token을 암호화 게시하고 Executor를 재조정합니다."
publish_started=true
curl --config "${curl_config}" \
  --request POST \
  --data "{\"label\":\"${label}\",\"restart\":true}" \
  --output "${response_file}" \
  "${admin_url%/}/v1/admin/claude-auth/publish"
if ! jq -e '.version.mode == "subscription-oauth" and (.rollout.failed // 0) == 0' "${response_file}" >/dev/null; then
  echo "Claude OAuth publish or Executor reconciliation was incomplete; inspect ${response_file}" >&2
  exit 1
fi

status_file=$(mktemp "${backup_root}/status.XXXXXX")
chmod 0600 "${status_file}"
curl --config "${curl_config}" --output "${status_file}" "${admin_url%/}/v1/admin/claude-auth"
if ! jq -e '.configured == true and .available == true and .migrationPending == false' "${status_file}" >/dev/null; then
  echo "central Claude OAuth status is not available; inspect ${status_file}" >&2
  exit 1
fi
rm -f "${curl_config}"

echo "[4/5] 릴리스 포인터를 전환합니다."
ln -sfn "${old_release}" "${previous_link}"
ln -sfn "${release_dir}" "${current_link}"

echo "[5/5] 공개 상태와 실행 이미지를 확인합니다."
curl --silent --show-error --fail --connect-timeout 5 --max-time 10 "${health_url}" >/dev/null
printf 'Manager: %s\nExecutor: %s\nBackup: %s\n' "${manager_image}" "${executor_image}" "${backup_root}"
echo "중앙 Claude 구독 OAuth Broker 전환을 완료했습니다. 이제 canary 사용자 대화와 동시 사용자 대화를 확인하세요."
