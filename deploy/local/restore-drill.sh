#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
# shellcheck disable=SC1091
. "$script_dir/profile.sh"
[ -f "$env_file" ] || PIE_RELAY_PROFILE="$profile" "$script_dir/bootstrap.sh" >/dev/null
# shellcheck disable=SC1090
. "$env_file"

backup_dir=${1:-}
if [ -z "$backup_dir" ]; then
  backup_dir=$(find "$backup_root" -mindepth 1 -maxdepth 1 -type d -print | sort | tail -n 1)
fi
[ -n "$backup_dir" ] && [ -f "$backup_dir/postgres.dump" ] && \
  [ -f "$backup_dir/state.tar.gz" ] && [ -f "$backup_dir/SHA256SUMS" ] || {
  printf 'valid backup directory is required\n' >&2
  exit 1
}

if command -v shasum >/dev/null 2>&1; then
  (cd "$backup_dir" && shasum -a 256 -c SHA256SUMS >/dev/null)
else
  (cd "$backup_dir" && sha256sum -c SHA256SUMS >/dev/null)
fi

# Refuse absolute paths or parent traversal even if a backup came from an
# untrusted copy with a matching, replaced checksum manifest.
docker run --rm -v "$backup_dir:/backup:ro" alpine:3.21 tar -tzf /backup/state.tar.gz |
  while IFS= read -r entry; do
    case "$entry" in
      /*|../*|*/../*|*/..) printf 'unsafe archive path: %s\n' "$entry" >&2; exit 1 ;;
    esac
  done

compose() {
  docker compose -p "$compose_project" --env-file "$env_file" \
    -f "$repo_root/deploy/compose.yaml" -f "$script_dir/compose.yaml" "$@"
}

profile_database=$(printf '%s' "$profile" | tr '-' '_')
restore_database="pie_restore_${profile_database}_drill"
cleanup() {
  compose exec -T postgres dropdb -U pie_relay --if-exists "$restore_database" >/dev/null 2>&1 || true
  if [ -n "${restore_dir:-}" ] && [ -d "$restore_dir" ]; then
    rm -rf "$restore_dir"
  fi
}
trap cleanup EXIT INT TERM

compose exec -T postgres dropdb -U pie_relay --if-exists "$restore_database" >/dev/null
compose exec -T postgres createdb -U pie_relay "$restore_database"
compose exec -T postgres pg_restore -U pie_relay -d "$restore_database" --no-owner --no-privileges <"$backup_dir/postgres.dump"
record_count=$(compose exec -T postgres psql -U pie_relay -d "$restore_database" -Atc \
  "select count(*) from information_schema.tables where table_schema='public';")
case "$record_count" in
  ''|*[!0-9]*) printf 'restore drill returned invalid table count: %s\n' "$record_count" >&2; exit 1 ;;
esac
[ "$record_count" -gt 0 ] || { printf 'restore drill restored no tables\n' >&2; exit 1; }

restore_dir=$(mktemp -d "${TMPDIR:-/tmp}/pie-relay-${profile}-restore.XXXXXX")
docker run --rm -v "$backup_dir:/backup:ro" -v "$restore_dir:/restore" alpine:3.21 \
  tar -C /restore -xzf /backup/state.tar.gz
for expected_dir in control registry blobs workspaces executor-state relay claude-auth chat-journal; do
  [ -d "$restore_dir/$expected_dir" ] || {
    printf 'state archive is missing %s\n' "$expected_dir" >&2
    exit 1
  }
done

printf 'Restore drill passed: %s PostgreSQL tables and state archive extracted.\n' "$record_count"
