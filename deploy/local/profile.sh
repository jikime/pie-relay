#!/bin/sh

# 이 파일은 실행하지 않고 deploy/local의 다른 스크립트에서 source한다.
# 호출자는 script_dir와 repo_root를 먼저 설정해야 한다.

profile=${PIE_RELAY_PROFILE:-pie-canvas}

case "$profile" in
  pie-canvas)
    compose_project=pie-relay-local
    env_file="$script_dir/.env"
    generated_dir="$script_dir/.generated"
    state_dir="$repo_root/.local/pie-relay/state"
    backup_root="$repo_root/.local/pie-relay/backups"
    profile_relay_port=13412
    profile_manager_port=19090
    profile_auth_port=18080
    profile_postgres_port=15432
    profile_prometheus_port=19092
    profile_http_port=18000
    profile_https_port=18443
    profile_pool_id=pie-canvas
    profile_application_id=pie-canvas
    profile_allowed_applications=pie-control,pie-relay-desktop,pie-mobile,pie-canvas
    profile_manager_id=local-manager
    profile_executor_network=pie-executor
    profile_preview_gateway_container=pie-preview-gateway
    profile_claude_auth_required=false
    profile_kroot_auto_link=false
    profile_agent_user_id=pie-canvas-agent
    profile_agent_external_subject=pie-canvas-local-agent
    profile_organization_id=org-local
    ;;
  kroot-studio)
    compose_project=pie-relay-kroot-studio
    profile_root="$script_dir/.profiles/kroot-studio"
    env_file="$profile_root/.env"
    generated_dir="$profile_root/.generated"
    state_dir="$repo_root/.local/pie-relay/kroot-studio/state"
    backup_root="$repo_root/.local/pie-relay/kroot-studio/backups"
    profile_relay_port=14412
    profile_manager_port=29090
    profile_auth_port=18180
    profile_postgres_port=15532
    profile_prometheus_port=19192
    profile_http_port=18100
    profile_https_port=18543
    profile_pool_id=kroot-studio
    profile_application_id=kroot-studio
    profile_allowed_applications=pie-control,pie-relay-desktop,pie-mobile,kroot-studio
    profile_manager_id=kroot-studio-local-manager
    profile_executor_network=pie-executor-kroot-studio
    profile_preview_gateway_container=pie-preview-gateway-kroot-studio
    profile_claude_auth_required=true
    profile_kroot_auto_link=true
    profile_agent_user_id=kroot-studio-agent
    profile_agent_external_subject=kroot-studio-local-agent
    profile_organization_id=org-kroot-studio
    ;;
  *)
    printf 'Unsupported PIE_RELAY_PROFILE: %s (expected pie-canvas or kroot-studio)\n' "$profile" >&2
    exit 2
    ;;
esac

export PIE_RELAY_PROFILE="$profile"
