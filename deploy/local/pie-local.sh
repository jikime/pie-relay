#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/profile.sh"

bootstrap() {
  PIE_RELAY_PROFILE="$profile" "$script_dir/bootstrap.sh"
}

compose() {
  docker compose -p "$compose_project" --env-file "$env_file" \
    -f "$repo_root/deploy/compose.yaml" -f "$script_dir/compose.yaml" --profile observability "$@"
}

load_env() {
  # shellcheck disable=SC1090
  . "$env_file"
}

require_docker() {
  command -v docker >/dev/null 2>&1 || { printf 'docker is required\n' >&2; exit 1; }
  docker info >/dev/null 2>&1 || { printf 'Docker daemon is not running\n' >&2; exit 1; }
}

ensure_executor_network() {
  load_env
  executor_network=${PIE_EXECUTOR_NETWORK:-pie-executor}
  if docker network inspect "$executor_network" >/dev/null 2>&1; then
    network_shape=$(docker network inspect "$executor_network" \
      --format '{{.Driver}}|{{.Internal}}|{{index .Options "com.docker.network.bridge.enable_icc"}}')
    if [ "$network_shape" != "bridge|false|false" ]; then
      printf 'Executor network %s has an unsafe or outdated configuration (%s).\n' \
        "$executor_network" "$network_shape" >&2
      printf 'Remove only Manager-created Executor containers, recreate this network with ICC disabled, and retry.\n' >&2
      exit 1
    fi
    return 0
  fi

  docker network create \
    --driver bridge \
    --opt com.docker.network.bridge.enable_icc=false \
    "$executor_network" >/dev/null
}

refresh_address() {
  bootstrap >/dev/null
  require_docker

  manager_container=$(compose ps -q manager)
  relay_container=$(compose ps -q relay)
  if [ -z "$manager_container" ] || [ -z "$relay_container" ]; then
    return 0
  fi

  load_env
  running_manager_url=$(docker container inspect "$manager_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^PIE_RELAY_PUBLIC_URL=//p' \
    | head -n 1)
  running_relay_url=$(docker container inspect "$relay_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^RELAY_PUBLIC_URL=//p' \
    | head -n 1)
  running_manager_auth=$(docker container inspect "$manager_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^PIE_CLAUDE_AUTH_REQUIRED=//p' \
    | head -n 1)
  running_manager_id=$(docker container inspect "$manager_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^PIE_EXECUTOR_MANAGER_ID=//p' \
    | head -n 1)
  running_manager_network=$(docker container inspect "$manager_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^PIE_EXECUTOR_NETWORK=//p' \
    | head -n 1)
  running_manager_application=$(docker container inspect "$manager_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^PIE_RELAY_DEFAULT_APPLICATION_ID=//p' \
    | head -n 1)
  running_manager_pool=$(docker container inspect "$manager_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^PIE_RELAY_DEFAULT_POOL_ID=//p' \
    | head -n 1)
  running_relay_pool=$(docker container inspect "$relay_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^RELAY_POOL_ID=//p' \
    | head -n 1)
  running_relay_applications=$(docker container inspect "$relay_container" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | sed -n 's/^RELAY_ALLOWED_APPLICATIONS=//p' \
    | head -n 1)

  if [ "$running_manager_url" = "$PIE_LOCAL_RELAY_PUBLIC_URL" ] \
    && [ "$running_relay_url" = "$PIE_LOCAL_RELAY_PUBLIC_URL" ] \
    && [ "$running_manager_auth" = "$PIE_CLAUDE_AUTH_REQUIRED" ] \
    && [ "$running_manager_id" = "$PIE_EXECUTOR_MANAGER_ID" ] \
    && [ "$running_manager_network" = "$PIE_EXECUTOR_NETWORK" ] \
    && [ "$running_manager_application" = "$PIE_RELAY_DEFAULT_APPLICATION_ID" ] \
    && [ "$running_manager_pool" = "$RELAY_POOL_ID" ] \
    && [ "$running_relay_pool" = "$RELAY_POOL_ID" ] \
    && [ "$running_relay_applications" = "$RELAY_ALLOWED_APPLICATIONS" ]; then
    return 0
  fi

  printf 'Local Relay profile configuration changed; refreshing %s Relay and Manager.\n' "$profile"

  # Docker Compose가 여러 네트워크에 연결된 컨테이너를 force-recreate할 때
  # 일부 네트워크를 먼저 분리한 뒤 같은 endpoint를 다시 분리하려고 하면
  # `container ... is not connected to the network`로 갱신이 중단될 수 있다.
  # Relay와 Manager는 상태를 컨테이너 내부에 보관하지 않으므로 기존
  # 컨테이너만 Docker에 직접 제거하고 Compose가 새로 생성하게 한다.
  # PostgreSQL 볼륨, Relay state 볼륨, Executor 컨테이너는 건드리지 않는다.
  docker container rm -f "$manager_container" "$relay_container" >/dev/null
  ensure_executor_network
  compose up -d --no-build --wait --wait-timeout 120 relay manager
}

up() {
  bootstrap
  require_docker
  load_env
  ensure_executor_network
  # Executor containers are created through the host Docker socket, so this image
  # must exist in the host daemon before Manager provisioning is exercised.
  executor_image=${PIE_EXECUTOR_IMAGE:-pie-relay-client:latest}
  executor_dockerfile=${PIE_EXECUTOR_DOCKERFILE:-$repo_root/executor-manager/Dockerfile.executor}
  if [ "${PIE_SKIP_EXECUTOR_BUILD:-false}" != "true" ]; then
    # ACP E2E 이미지는 production Executor 위에 얹히므로 기반 이미지를
    # 먼저 갱신한다. 일반 실행은 기존 단일 build 동작을 그대로 유지한다.
    if [ "$executor_dockerfile" != "$repo_root/executor-manager/Dockerfile.executor" ]; then
      docker build -f "$repo_root/executor-manager/Dockerfile.executor" -t pie-relay-client:latest "$repo_root"
    fi
    docker build -f "$executor_dockerfile" -t "$executor_image" "$repo_root"
  else
    docker image inspect "$executor_image" >/dev/null 2>&1 || {
      printf 'PIE_SKIP_EXECUTOR_BUILD=true requires an existing %s image\n' "$executor_image" >&2
      exit 1
    }
  fi
  compose up -d --build --wait --wait-timeout 240
  compose ps
}

run_tests() {
  [ "$profile" = pie-canvas ] || {
    printf 'The full local E2E suite currently targets the pie-canvas profile.\n' >&2
    printf 'Use PIE_RELAY_PROFILE=pie-canvas %s test.\n' "$0" >&2
    exit 2
  }
  up
  load_env
  export PIE_E2E_CONTROL_URL="http://127.0.0.1:${PIE_LOCAL_MANAGER_PORT:-19090}"
  export PIE_E2E_RELAY_URL="http://127.0.0.1:${PIE_LOCAL_RELAY_PORT:-13412}"
  export PIE_E2E_MOCK_AUTH_URL="http://127.0.0.1:${PIE_LOCAL_AUTH_PORT:-18080}"
  export PIE_E2E_TLS_RELAY_URL="https://127.0.0.1:${PIE_HTTPS_PORT:-18443}"
  export PIE_E2E_LOCAL_CA="$generated_dir/certs/local-ca.crt"
  export PIE_E2E_CONTROL_TOKEN="$PIE_MANAGER_ADMIN_TOKEN"
  export PIE_E2E_WEBHOOK_SECRET="$PIE_USER_WEBHOOK_SECRET"
  export PIE_E2E_MOCK_AUTH_CONTROL_TOKEN="$PIE_LOCAL_AUTH_CONTROL_TOKEN"
  export PIE_E2E_COMPOSE_PROJECT="$compose_project"
  export PIE_E2E_EXECUTOR_NETWORK="$PIE_EXECUTOR_NETWORK"
  export PIE_EXECUTOR_MANAGER_ID
  node "$repo_root/scripts/e2e/local-stack.mjs"
  PIE_E2E_RELAY_URL="http://127.0.0.1:${PIE_LOCAL_RELAY_PORT:-13412}" \
    PIE_E2E_ENROLL_SECRET="$HOST_ENROLL_SECRET" \
    node "$repo_root/scripts/e2e/standalone-clientd.mjs"
  docker build -f "$repo_root/executor-manager/Dockerfile.executor-e2e" \
    -t pie-relay-client-e2e:latest "$repo_root"
  # Preview E2E needs the fake kroot project initializer, while the long-lived
  # local Manager intentionally keeps the production image name. Temporarily
  # retag only the host image used for newly-created test Executors; existing
  # containers are not recreated and the original tag is restored afterward.
  docker tag pie-relay-client:latest pie-relay-client:pie-local-base
  docker tag pie-relay-client-e2e:latest pie-relay-client:latest
  restore_executor_image() {
    docker tag pie-relay-client:pie-local-base pie-relay-client:latest
  }
  trap restore_executor_image EXIT INT TERM
  PIE_E2E_PREVIEW_PORT="${PIE_HTTPS_PORT:-18443}" \
    PIE_E2E_PREVIEW_APP_PATH="apps/web" \
    node "$repo_root/scripts/e2e/project-preview.mjs"
  restore_executor_image
  trap - EXIT INT TERM
  npm --prefix "$repo_root/examples/third-party-web-chat" test
  RELAY_JWT_SECRET="$RELAY_JWT_SECRET" \
    PIE_LOCAL_RELAY_PORT="${PIE_LOCAL_RELAY_PORT:-13412}" \
    PIE_E2E_RELAY_POOL_ID="${RELAY_POOL_ID:-pie-relay-default}" \
    PIE_E2E_RELAY_PUBLIC_URL="http://host.docker.internal:${PIE_LOCAL_RELAY_PORT:-13412}" \
    node "$repo_root/scripts/e2e/third-party-chat.mjs"
  RELAY_JWT_SECRET="$RELAY_JWT_SECRET" \
    PIE_E2E_RELAY_URL="ws://relay:13412/ws/agent" \
    PIE_E2E_RELAY_PUBLIC_URL="http://host.docker.internal:${PIE_LOCAL_RELAY_PORT:-13412}" \
    PIE_E2E_RELAY_POOL_ID="${RELAY_POOL_ID:-pie-relay-default}" \
    PIE_E2E_CONTROL_NETWORK="${compose_project}_control" \
    node "$repo_root/scripts/e2e/third-party-web-chat.mjs"
  curl --noproxy '*' --fail --silent --show-error \
    --cacert "$generated_dir/certs/local-ca.crt" \
    --resolve "relay.localhost:${PIE_HTTPS_PORT}:127.0.0.1" \
    "https://relay.localhost:${PIE_HTTPS_PORT}/readyz" >/dev/null
  curl --noproxy '*' --fail --silent --show-error \
    --cacert "$generated_dir/certs/local-ca.crt" \
    --resolve "api-relay.localhost:${PIE_HTTPS_PORT}:127.0.0.1" \
    "https://api-relay.localhost:${PIE_HTTPS_PORT}/readyz" >/dev/null
  curl --noproxy '*' --fail --silent --show-error \
    "http://127.0.0.1:${PIE_LOCAL_PROMETHEUS_PORT:-19092}/-/ready" >/dev/null
  scrape_ready=false
  attempt=0
  while [ "$attempt" -lt 30 ]; do
    if curl --noproxy '*' --fail --silent --show-error --get \
      --data-urlencode 'query=up{job=~"pie-relay|pie-manager"}' \
      "http://127.0.0.1:${PIE_LOCAL_PROMETHEUS_PORT:-19092}/api/v1/query" \
      | jq -e '.status == "success" and ((.data.result | length) == 2) and ([.data.result[].value[1]] | all(. == "1"))' >/dev/null; then
      scrape_ready=true
      break
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
  [ "$scrape_ready" = true ] || { printf 'Prometheus targets did not recover after Relay restart\n' >&2; exit 1; }
  backup_dir=$("$script_dir/backup.sh")
  "$script_dir/restore-drill.sh" "$backup_dir"
  printf 'Pie Relay local full-stack test passed.\n'
}

provision_profile() {
  bootstrap >/dev/null
  require_docker
  "$script_dir/provision-profile.sh"
}

claude_auth_login() {
  bootstrap >/dev/null
  require_docker
  load_env
  token_file=$(mktemp)
  cleanup_token() {
    rm -f "$token_file"
  }
  trap cleanup_token EXIT INT TERM
  chmod 600 "$token_file"
  printf '%s' "$PIE_MANAGER_ADMIN_TOKEN" >"$token_file"
  PIE_DATA_DIR="$PIE_DATA_DIR" \
  PIE_CLAUDE_AUTH_DIR="$PIE_DATA_DIR/claude-auth" \
  PIE_CLAUDE_AUTH_LOGIN_DIR="$PIE_DATA_DIR/claude-auth/login" \
  PIE_EXECUTOR_IMAGE="${PIE_EXECUTOR_IMAGE:-pie-relay-client:latest}" \
  PIE_MANAGER_URL="http://127.0.0.1:${PIE_LOCAL_MANAGER_PORT:-19090}" \
  PIE_MANAGER_ADMIN_TOKEN_FILE="$token_file" \
    "$repo_root/scripts/ops/claude-auth-login.sh"
  cleanup_token
  trap - EXIT INT TERM
}

show_profile() {
  bootstrap >/dev/null
  load_env
  printf 'profile=%s\n' "$profile"
  printf 'compose_project=%s\n' "$compose_project"
  printf 'env_file=%s\n' "$env_file"
  printf 'data_dir=%s\n' "$PIE_DATA_DIR"
  printf 'executor_network=%s\n' "$PIE_EXECUTOR_NETWORK"
  printf 'relay_url=http://127.0.0.1:%s\n' "$PIE_LOCAL_RELAY_PORT"
  printf 'manager_url=http://127.0.0.1:%s\n' "$PIE_LOCAL_MANAGER_PORT"
  printf 'claude_auth_required=%s\n' "$PIE_CLAUDE_AUTH_REQUIRED"
}

command_name=${1:-help}
case "$command_name" in
  bootstrap) bootstrap ;;
  up) up ;;
  test) run_tests ;;
  status) bootstrap >/dev/null; require_docker; compose ps ;;
  profile) show_profile ;;
  refresh-address) refresh_address ;;
  provision) provision_profile ;;
  provision-pie-canvas)
    [ "$profile" = pie-canvas ] || { printf 'Use PIE_RELAY_PROFILE=pie-canvas for this command.\n' >&2; exit 2; }
    provision_profile
    ;;
  provision-kroot-studio)
    [ "$profile" = kroot-studio ] || { printf 'Use PIE_RELAY_PROFILE=kroot-studio for this command.\n' >&2; exit 2; }
    provision_profile
    ;;
  claude-auth-login) claude_auth_login ;;
  logs) bootstrap >/dev/null; require_docker; shift; compose logs -f "$@" ;;
  backup) bootstrap >/dev/null; require_docker; "$script_dir/backup.sh" ;;
  restore-drill) bootstrap >/dev/null; require_docker; shift; "$script_dir/restore-drill.sh" "$@" ;;
  down) bootstrap >/dev/null; require_docker; compose down --remove-orphans ;;
  *)
    printf 'Usage: %s bootstrap|up|test|status|profile|refresh-address|provision|provision-pie-canvas|provision-kroot-studio|claude-auth-login|logs [service]|backup|restore-drill [backup-dir]|down\n' "$0" >&2
    exit 2
    ;;
esac
