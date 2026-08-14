#!/usr/bin/env bash

set -euo pipefail

# Web Chat 회원 수 한도가 Executor 총량보다 낮아 회원가입이 중간에 실패하지 않도록
# 기존 Integration을 멱등적으로 보정한다. 이 스크립트는 .env를 직접 읽지 않으므로
# 호출자가 `set -a; . ./.env; set +a`로 명시적으로 환경을 전달해야 한다.

manager_url=${PIE_MANAGER_ADMIN_URL:-https://api-relay.cookai.dev}
integration_id=${PIE_WEB_CHAT_INTEGRATION_ID:-cookai-e2e}
integration_name=${PIE_WEB_CHAT_INTEGRATION_NAME:-CookAI Web Chat}
max_users=${PIE_WEB_CHAT_MAX_USERS:-${PIE_EXECUTOR_MAX_EXECUTORS:-4}}
max_projects=${PIE_WEB_CHAT_MAX_PROJECTS_PER_USER:-4}
max_previews=${PIE_WEB_CHAT_MAX_PREVIEWS_PER_USER:-4}
max_conversations=${PIE_WEB_CHAT_MAX_CONVERSATIONS_PER_USER:-2}
token_file=${PIE_WEB_CHAT_INTEGRATION_TOKEN_FILE:?set PIE_WEB_CHAT_INTEGRATION_TOKEN_FILE}
admin_token=${PIE_MANAGER_ADMIN_TOKEN:?set PIE_MANAGER_ADMIN_TOKEN}

case "$max_users:$max_projects:$max_previews:$max_conversations" in
  *[!0-9:]*|'') echo "Integration quota must be a positive integer" >&2; exit 2 ;;
esac
if (( max_users < 1 || max_projects < 1 || max_previews < 1 || max_conversations < 1 )); then
  echo "Integration quota must be a positive integer" >&2
  exit 2
fi
if [[ -L "$token_file" ]]; then
  echo "Refusing symlink Integration token file: $token_file" >&2
  exit 2
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/pie-integration.XXXXXX")
cleanup() {
  rm -f "$work_dir/response.json" "$work_dir/token"
  rmdir "$work_dir"
}
trap cleanup EXIT

request() {
  local method=$1
  local path=$2
  local body=${3:-}
  local args=(
    --silent --show-error
    --output "$work_dir/response.json"
    --write-out '%{http_code}'
    --request "$method"
    --header "Authorization: Bearer $admin_token"
    --header 'Accept: application/json'
    --connect-timeout 10
    --max-time 30
  )
  if [[ -n "$body" ]]; then
    args+=(--header 'Content-Type: application/json' --data "$body")
  fi
  curl "${args[@]}" "${manager_url%/}$path"
}

write_token() {
  local value=$1
  local parent
  parent=$(dirname "$token_file")
  [[ -d "$parent" ]] || { echo "Integration token directory does not exist: $parent" >&2; exit 2; }
  umask 077
  printf '%s\n' "$value" > "$work_dir/token"
  install -m 600 "$work_dir/token" "$token_file"
}

integration_path="/v1/admin/integrations/$integration_id"
status=$(request GET "$integration_path")
if [[ "$status" == 404 ]]; then
  body=$(jq -cn \
    --arg id "$integration_id" \
    --arg name "$integration_name" \
    --argjson users "$max_users" \
    --argjson projects "$max_projects" \
    --argjson previews "$max_previews" \
    --argjson conversations "$max_conversations" \
    '{id:$id,displayName:$name,status:"active",maxUsers:$users,maxProjectsPerUser:$projects,maxPreviewsPerUser:$previews,maxConversationsPerUser:$conversations,credential:{targetPath:".kroot/credential.json",format:"json",maxBytes:65536}}')
  status=$(request POST /v1/admin/integrations "$body")
  [[ "$status" == 201 ]] || { echo "Integration create failed: HTTP $status" >&2; exit 1; }
  service_token=$(jq -er '.serviceToken' "$work_dir/response.json")
  write_token "$service_token"
  echo "Integration $integration_id created (maxUsers=$max_users)"
  exit 0
fi
[[ "$status" == 200 ]] || { echo "Integration read failed: HTTP $status" >&2; exit 1; }

current_status=$(jq -r '.status' "$work_dir/response.json")
credential_path=$(jq -r '.credential.targetPath' "$work_dir/response.json")
[[ "$current_status" == active ]] || { echo "Integration $integration_id is not active ($current_status)" >&2; exit 1; }
[[ "$credential_path" == .kroot/credential.json ]] || {
  echo "Integration credential path differs from .kroot/credential.json: $credential_path" >&2
  exit 1
}

current_users=$(jq -r '.maxUsers' "$work_dir/response.json")
current_projects=$(jq -r '.maxProjectsPerUser' "$work_dir/response.json")
current_previews=$(jq -r '.maxPreviewsPerUser // 4' "$work_dir/response.json")
current_conversations=$(jq -r '.maxConversationsPerUser' "$work_dir/response.json")
(( max_users < current_users )) && max_users=$current_users
(( max_projects < current_projects )) && max_projects=$current_projects
(( max_previews < current_previews )) && max_previews=$current_previews
(( max_conversations < current_conversations )) && max_conversations=$current_conversations

body=$(jq -cn \
  --argjson users "$max_users" \
  --argjson projects "$max_projects" \
  --argjson previews "$max_previews" \
  --argjson conversations "$max_conversations" \
  '{maxUsers:$users,maxProjectsPerUser:$projects,maxPreviewsPerUser:$previews,maxConversationsPerUser:$conversations}')
status=$(request PATCH "$integration_path" "$body")
[[ "$status" == 200 ]] || { echo "Integration quota update failed: HTTP $status" >&2; exit 1; }

if [[ ! -s "$token_file" ]]; then
  status=$(request POST "$integration_path/rotate-token" '{}')
  [[ "$status" == 200 ]] || { echo "Integration token rotation failed: HTTP $status" >&2; exit 1; }
  service_token=$(jq -er '.serviceToken' "$work_dir/response.json")
  write_token "$service_token"
fi

actual_users=$(jq -r '.maxUsers // empty' "$work_dir/response.json")
if [[ -z "$actual_users" ]]; then
  status=$(request GET "$integration_path")
  [[ "$status" == 200 ]] || { echo "Integration verification failed: HTTP $status" >&2; exit 1; }
  actual_users=$(jq -r '.maxUsers' "$work_dir/response.json")
fi
echo "Integration $integration_id ready (maxUsers=$actual_users)"
