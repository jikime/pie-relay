#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/profile.sh"

[ -f "$env_file" ] || {
  printf 'Run PIE_RELAY_PROFILE=%s %s/pie-local.sh up first.\n' "$profile" "$script_dir" >&2
  exit 1
}

# shellcheck disable=SC1090
. "$env_file"

control_url="http://127.0.0.1:${PIE_LOCAL_MANAGER_PORT:-$profile_manager_port}"
user_id=$profile_agent_user_id
device_id="executor-$user_id"

if [ "${PIE_CLAUDE_AUTH_REQUIRED:-$profile_claude_auth_required}" = true ]; then
  configured=$(curl --fail --silent --show-error \
    -H "Authorization: Bearer $PIE_MANAGER_ADMIN_TOKEN" \
    "$control_url/v1/admin/claude-auth" \
    | jq -r '.configured == true')
  if [ "$configured" != true ]; then
    printf '%s profile requires Claude authentication, but no active version is configured.\n' "$profile" >&2
    printf 'Run: PIE_RELAY_PROFILE=%s %s/pie-local.sh claude-auth-login\n' "$profile" "$script_dir" >&2
    exit 1
  fi
fi

timestamp=$(date +%s)
occurred_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
event_id="$profile-local-$timestamp"
body=$(jq -nc \
  --arg id "$event_id" \
  --arg at "$occurred_at" \
  --arg user "$user_id" \
  --arg external "$profile_agent_external_subject" \
  --arg organization "$profile_organization_id" \
  '{
    id: $id,
    type: "user.updated",
    occurredAt: $at,
    provision: true,
    user: {
      id: $user,
      externalSubject: $external,
      organizationId: $organization,
      quota: {
        cpus: "1.5",
        memoryBytes: 1073741824,
        pids: 256,
        maxSessions: 16,
        maxParticipants: 32
      }
    }
  }')
signature=$(printf '%s' "$timestamp.$body" | openssl dgst -sha256 -hmac "$PIE_USER_WEBHOOK_SECRET" | awk '{print $NF}')

curl --fail-with-body --silent --show-error \
  -X POST \
  -H 'Content-Type: application/json' \
  -H "X-Pie-Timestamp: $timestamp" \
  -H "X-Pie-Signature: v1=$signature" \
  --data "$body" \
  "$control_url/v1/hooks/users" >/dev/null

attempt=0
while [ "$attempt" -lt 90 ]; do
  ready=$(curl --fail --silent --show-error \
    -H "Authorization: Bearer $PIE_MANAGER_ADMIN_TOKEN" \
    "$control_url/v1/admin/snapshot" \
    | jq -r --arg id "$device_id" '.devices[]? | select(.id == $id) | (.runtimeHealthy == true and .observedState != "error")')
  if [ "$ready" = true ]; then
    printf '%s local Agent executor is ready: %s\n' "$profile" "$device_id"
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 1
done

printf '%s local Agent executor did not become ready: %s\n' "$profile" "$device_id" >&2
exit 1
