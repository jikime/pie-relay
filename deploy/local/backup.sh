#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/profile.sh"
[ -f "$env_file" ] || PIE_RELAY_PROFILE="$profile" "$script_dir/bootstrap.sh" >/dev/null
# shellcheck disable=SC1090
. "$env_file"

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$backup_root"
backup_dir=$(mktemp -d "$backup_root/$timestamp.XXXXXX")
chmod 700 "$backup_dir"

compose() {
  docker compose -p "$compose_project" --env-file "$env_file" \
    -f "$repo_root/deploy/compose.yaml" -f "$script_dir/compose.yaml" "$@"
}

compose exec -T postgres pg_dump -U pie_relay -d pie_relay -Fc >"$backup_dir/postgres.dump"

docker run --rm \
  -v "$PIE_DATA_DIR:/source:ro" \
  -v "$backup_dir:/backup" \
  alpine:3.21 tar -C /source -czf /backup/state.tar.gz \
    control registry blobs workspaces executor-state relay claude-auth chat-journal

if command -v shasum >/dev/null 2>&1; then
  (cd "$backup_dir" && shasum -a 256 postgres.dump state.tar.gz >SHA256SUMS)
else
  (cd "$backup_dir" && sha256sum postgres.dump state.tar.gz >SHA256SUMS)
fi
chmod 600 "$backup_dir"/*
printf '%s\n' "$backup_dir"
