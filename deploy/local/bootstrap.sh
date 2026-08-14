#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/profile.sh"
cert_dir="$generated_dir/certs"
prometheus_dir="$generated_dir/prometheus"

detect_lan_ip() {
  if command -v ipconfig >/dev/null 2>&1; then
    for interface_name in en0 en1; do
      candidate=$(ipconfig getifaddr "$interface_name" 2>/dev/null || true)
      if [ -n "$candidate" ]; then
        printf '%s' "$candidate"
        return
      fi
    done
  fi
  if command -v hostname >/dev/null 2>&1; then
    candidate=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
    if [ -n "$candidate" ]; then
      printf '%s' "$candidate"
      return
    fi
  fi
  printf '%s' '127.0.0.1'
}

random_hex() {
  openssl rand -hex 32
}

lan_ip=${PIE_LOCAL_LAN_IP:-$(detect_lan_ip)}
case "$lan_ip" in
  *[!0-9.]*|'')
    printf 'PIE_LOCAL_LAN_IP must be an IPv4 address, got %s\n' "$lan_ip" >&2
    exit 1
    ;;
esac

command -v openssl >/dev/null 2>&1 || {
  printf 'openssl is required\n' >&2
  exit 1
}

umask 077
mkdir -p "$(dirname -- "$env_file")" "$cert_dir" "$prometheus_dir/secrets" \
  "$state_dir/postgres" "$state_dir/prometheus" "$state_dir/relay" \
  "$state_dir/control" "$state_dir/registry" "$state_dir/blobs" \
  "$state_dir/workspaces" "$state_dir/executor-state" \
  "$backup_root"
# PostgreSQL initializes as root and then drops privileges; Prometheus runs as
# nobody. These development-only bind roots need to be writable from Docker
# Desktop without assuming host uid/gid mapping.
chmod 0777 "$state_dir/postgres" "$state_dir/prometheus" "$state_dir/relay"

if [ ! -f "$env_file" ]; then
  postgres_password=$(random_hex)
  relay_jwt_secret=$(random_hex)
  enroll_secret=$(random_hex)
  manager_token=$(random_hex)
  relay_control_token=$(random_hex)
  relay_presence_token=$(random_hex)
  relay_metrics_token=$(random_hex)
  relay_routing_secret=$(random_hex)
  webhook_secret=$(random_hex)
  auth_client_secret=$(random_hex)
  auth_control_token=$(random_hex)
  device_auth_secret=$(random_hex)
  preview_gateway_token=$(random_hex)
  preview_access_secret=$(random_hex)
  cat >"$env_file" <<EOF
PIE_RELAY_PROFILE=$profile
COMPOSE_PROJECT_NAME=$compose_project
POSTGRES_PASSWORD=$postgres_password
RELAY_JWT_SECRET=$relay_jwt_secret
HOST_ENROLL_SECRET=$enroll_secret
PIE_MANAGER_ADMIN_TOKEN=$manager_token
PIE_RELAY_CONTROL_TOKEN=$relay_control_token
PIE_RELAY_PRESENCE_TOKEN=$relay_presence_token
PIE_RELAY_METRICS_TOKEN=$relay_metrics_token
PIE_RELAY_ROUTING_SECRET=$relay_routing_secret
PIE_USER_WEBHOOK_SECRET=$webhook_secret
PIE_AUTH_CLIENT_ID=pie-local
PIE_AUTH_CLIENT_SECRET=$auth_client_secret
PIE_LOCAL_AUTH_CONTROL_TOKEN=$auth_control_token
PIE_DEVICE_AUTH_SECRET=$device_auth_secret
PIE_PREVIEW_GATEWAY_TOKEN=$preview_gateway_token
PIE_PREVIEW_ACCESS_SECRET=$preview_access_secret
PIE_PREVIEW_DOMAIN=preview.localhost
PIE_PREVIEW_PUBLIC_SCHEME=https
PIE_PREVIEW_PUBLIC_PORT=$profile_https_port
PIE_PREVIEW_GATEWAY_CONTAINER=$profile_preview_gateway_container
PIE_AUTH_HTTP_TIMEOUT=750ms
PIE_AUTH_CACHE_TTL=1s
PIE_AUTH_NEGATIVE_CACHE_TTL=500ms
PIE_EXECUTOR_MANAGER_ID=$profile_manager_id
PIE_EXECUTOR_IMAGE=pie-relay-client:latest
PIE_EXECUTOR_NETWORK=$profile_executor_network
PIE_EXECUTOR_KROOT_AUTO_LINK=$profile_kroot_auto_link
PIE_DATA_DIR=$state_dir
PIE_CLAUDE_AUTH_REQUIRED=$profile_claude_auth_required
PIE_RELAY_DEFAULT_APPLICATION_ID=$profile_application_id
PIE_LOCAL_LAN_IP=$lan_ip
PIE_LOCAL_RELAY_PUBLIC_URL=http://$lan_ip:$profile_relay_port
PIE_LOCAL_RELAY_ALLOWED_ORIGINS=tauri://localhost,http://tauri.localhost,https://tauri.localhost,http://localhost:1420,http://127.0.0.1:1420,http://$lan_ip:8081
PIE_LOCAL_CORS_ALLOWED_ORIGINS=tauri://localhost,http://tauri.localhost,https://tauri.localhost,http://localhost:1420,http://127.0.0.1:1420,https://admin-relay.localhost:$profile_https_port
PIE_HTTP_BIND_ADDRESS=127.0.0.1
PIE_HTTP_PORT=$profile_http_port
PIE_HTTPS_BIND_ADDRESS=127.0.0.1
PIE_HTTPS_PORT=$profile_https_port
PIE_LOCAL_RELAY_BIND_ADDRESS=0.0.0.0
PIE_LOCAL_RELAY_PORT=$profile_relay_port
PIE_LOCAL_MANAGER_BIND_ADDRESS=127.0.0.1
PIE_LOCAL_MANAGER_PORT=$profile_manager_port
PIE_LOCAL_AUTH_PORT=$profile_auth_port
PIE_LOCAL_POSTGRES_PORT=$profile_postgres_port
PIE_LOCAL_PROMETHEUS_PORT=$profile_prometheus_port
PIE_LOCAL_CERT_DIR=$cert_dir
PIE_LOCAL_PROMETHEUS_CONFIG=$prometheus_dir/prometheus.yml
PIE_LOCAL_PROMETHEUS_SECRETS_DIR=$prometheus_dir/secrets
RELAY_POOL_ID=$profile_pool_id
RELAY_ALLOWED_APPLICATIONS=$profile_allowed_applications
RELAY_ALLOW_LEGACY_QUERY_TICKET=false
RELAY_ALLOW_LEGACY_UNSCOPED_TOKENS=false
RELAY_MAX_PARTICIPANTS_PER_ROOM=64
PIE_DEFAULT_MAX_SESSIONS=8
PIE_DEFAULT_MAX_PARTICIPANTS=32
EOF
  chmod 600 "$env_file"
fi

ensure_env() {
  key=$1
  value=$2
  if ! grep -q "^$key=" "$env_file"; then
    printf '%s=%s\n' "$key" "$value" >>"$env_file"
    chmod 600 "$env_file"
  fi
}

# 기존 Pie Canvas 로컬 환경은 secret과 데이터 경로를 그대로 사용하면서
# 새 프로필 경계에 필요한 비밀이 아닌 설정만 보강한다.
ensure_env PIE_RELAY_PROFILE "$profile"
ensure_env COMPOSE_PROJECT_NAME "$compose_project"
ensure_env PIE_EXECUTOR_NETWORK "$profile_executor_network"
ensure_env PIE_EXECUTOR_KROOT_AUTO_LINK "$profile_kroot_auto_link"
ensure_env PIE_CLAUDE_AUTH_REQUIRED "$profile_claude_auth_required"
ensure_env PIE_RELAY_DEFAULT_APPLICATION_ID "$profile_application_id"
ensure_env RELAY_POOL_ID "$profile_pool_id"
ensure_env RELAY_ALLOWED_APPLICATIONS "$profile_allowed_applications"
ensure_env PIE_PREVIEW_GATEWAY_CONTAINER "$profile_preview_gateway_container"
ensure_env PIE_LOCAL_CERT_DIR "$cert_dir"
ensure_env PIE_LOCAL_PROMETHEUS_CONFIG "$prometheus_dir/prometheus.yml"
ensure_env PIE_LOCAL_PROMETHEUS_SECRETS_DIR "$prometheus_dir/secrets"

# Resource-scoped ACP 세션이 도입되기 전에 만든 로컬 .env도 기존
# secret과 데이터는 그대로 유지한 채 routing secret만 한 번 보강한다.
if ! grep -q '^PIE_RELAY_ROUTING_SECRET=' "$env_file"; then
  printf 'PIE_RELAY_ROUTING_SECRET=%s\n' "$(random_hex)" >>"$env_file"
  chmod 600 "$env_file"
fi

# Existing local installations receive the device JWT secret additively. The
# same value must be configured in Pie Canvas as PIE_DEVICE_AUTH_SECRET.
if ! grep -q '^PIE_DEVICE_AUTH_SECRET=' "$env_file"; then
  printf 'PIE_DEVICE_AUTH_SECRET=%s\n' "$(random_hex)" >>"$env_file"
  chmod 600 "$env_file"
fi

if ! grep -q '^PIE_PREVIEW_GATEWAY_TOKEN=' "$env_file"; then
  printf 'PIE_PREVIEW_GATEWAY_TOKEN=%s\n' "$(random_hex)" >>"$env_file"
  chmod 600 "$env_file"
fi
if ! grep -q '^PIE_PREVIEW_ACCESS_SECRET=' "$env_file"; then
  printf 'PIE_PREVIEW_ACCESS_SECRET=%s\n' "$(random_hex)" >>"$env_file"
  chmod 600 "$env_file"
fi
if ! grep -q '^PIE_PREVIEW_DOMAIN=' "$env_file"; then
  printf 'PIE_PREVIEW_DOMAIN=preview.localhost\n' >>"$env_file"
  printf 'PIE_PREVIEW_GATEWAY_CONTAINER=pie-preview-gateway\n' >>"$env_file"
  chmod 600 "$env_file"
fi
if ! grep -q '^PIE_PREVIEW_PUBLIC_SCHEME=' "$env_file"; then
  printf 'PIE_PREVIEW_PUBLIC_SCHEME=https\n' >>"$env_file"
  chmod 600 "$env_file"
fi
if ! grep -q '^PIE_PREVIEW_PUBLIC_PORT=' "$env_file"; then
  printf 'PIE_PREVIEW_PUBLIC_PORT=%s\n' "$profile_https_port" >>"$env_file"
  chmod 600 "$env_file"
fi

# LAN 주소 갱신 시 사용자가 프로필 환경 파일에서 조정한 포트를 보존한다.
# shellcheck disable=SC1090
. "$env_file"

# Keep the detected address current without rotating any generated secret.
if ! grep -q "^PIE_LOCAL_LAN_IP=$lan_ip$" "$env_file"; then
  temporary_env="$generated_dir/env.tmp"
  awk -v ip="$lan_ip" -v relay_port="${PIE_LOCAL_RELAY_PORT:-$profile_relay_port}" '
    /^PIE_LOCAL_LAN_IP=/ { print "PIE_LOCAL_LAN_IP=" ip; next }
    /^PIE_LOCAL_RELAY_PUBLIC_URL=/ { print "PIE_LOCAL_RELAY_PUBLIC_URL=http://" ip ":" relay_port; next }
    /^PIE_LOCAL_RELAY_ALLOWED_ORIGINS=/ {
      print "PIE_LOCAL_RELAY_ALLOWED_ORIGINS=tauri://localhost,http://tauri.localhost,https://tauri.localhost,http://localhost:1420,http://127.0.0.1:1420,http://" ip ":8081"
      next
    }
    { print }
  ' "$env_file" >"$temporary_env"
  mv "$temporary_env" "$env_file"
  chmod 600 "$env_file"
fi

# shellcheck disable=SC1090
. "$env_file"

if [ ! -f "$cert_dir/local-ca.key" ] || [ ! -f "$cert_dir/local-ca.crt" ]; then
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$cert_dir/local-ca.key" >/dev/null 2>&1
  openssl req -x509 -new -sha256 -days 825 \
    -key "$cert_dir/local-ca.key" -out "$cert_dir/local-ca.crt" \
    -subj '/CN=Pie Relay Local Development CA/O=PieLab Local' >/dev/null 2>&1
fi

certificate_config="$generated_dir/server-cert.ext"
cat >"$certificate_config" <<EOF
subjectAltName=DNS:localhost,DNS:relay.localhost,DNS:api-relay.localhost,DNS:admin-relay.localhost,DNS:*.preview.localhost,IP:127.0.0.1,IP:$PIE_LOCAL_LAN_IP
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
basicConstraints=CA:FALSE
EOF
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$cert_dir/server.key" >/dev/null 2>&1
openssl req -new -key "$cert_dir/server.key" -out "$generated_dir/server.csr" -subj '/CN=relay.localhost/O=PieLab Local' >/dev/null 2>&1
openssl x509 -req -sha256 -days 397 -in "$generated_dir/server.csr" \
  -CA "$cert_dir/local-ca.crt" -CAkey "$cert_dir/local-ca.key" -CAcreateserial \
  -extfile "$certificate_config" -out "$cert_dir/server.crt" >/dev/null 2>&1
chmod 600 "$cert_dir/local-ca.key" "$cert_dir/server.key"
chmod 644 "$cert_dir/local-ca.crt" "$cert_dir/server.crt"

printf '%s\n' "$PIE_RELAY_METRICS_TOKEN" >"$prometheus_dir/secrets/relay-token"
printf '%s\n' "$PIE_MANAGER_ADMIN_TOKEN" >"$prometheus_dir/secrets/manager-token"
chmod 644 "$prometheus_dir/secrets/relay-token" "$prometheus_dir/secrets/manager-token"
cat >"$prometheus_dir/prometheus.yml" <<'EOF'
global:
  scrape_interval: 5s
  evaluation_interval: 5s
rule_files:
  - /etc/prometheus/alerts.yaml
scrape_configs:
  - job_name: pie-relay
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /run/local-secrets/relay-token
    static_configs:
      - targets: [relay:13412]
  - job_name: pie-manager
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /run/local-secrets/manager-token
    static_configs:
      - targets: [manager:19090]
EOF
chmod 644 "$prometheus_dir/prometheus.yml"

printf 'Local environment ready: %s\n' "$profile"
printf '  Compose project: %s\n' "$compose_project"
printf '  Data:            %s\n' "$PIE_DATA_DIR"
printf '  Relay HTTP/WS:   http://%s:%s\n' "$PIE_LOCAL_LAN_IP" "$PIE_LOCAL_RELAY_PORT"
printf '  Manager:         http://127.0.0.1:%s\n' "$PIE_LOCAL_MANAGER_PORT"
printf '  Relay TLS/WSS:   https://relay.localhost:%s\n' "$PIE_HTTPS_PORT"
printf '  Admin TLS:       https://admin-relay.localhost:%s/admin/\n' "$PIE_HTTPS_PORT"
printf '  Local CA:       %s\n' "$cert_dir/local-ca.crt"
