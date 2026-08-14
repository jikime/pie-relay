#!/usr/bin/env bash
set -euo pipefail

root=${PIE_TEST_ROOT:-/home/kaonkroot/pie-sandbox-test}
traefik_root=${KROOT_TRAEFIK_ROOT:-/home/kaonkroot/.edge-runtime/dev/deploy/kroot}
base_compose=${KROOT_TRAEFIK_COMPOSE:-$traefik_root/docker-compose.shared-traefik.yml}
override_compose=${PIE_PREVIEW_TLS_OVERRIDE:-$root/current/src/deploy/test-server/shared-traefik-preview-tls.override.yaml}
issued_dir=${PIE_PREVIEW_ISSUED_DIR:-$root/preview-acme/config/live/preview.kroot.io}
install_dir=${PIE_PREVIEW_TLS_DIR:-$root/preview-tls}
cert=$issued_dir/fullchain.pem
key=$issued_dir/privkey.pem

# 공유 Traefik은 ACME 메일을 Compose 보간 변수로만 사용하므로 컨테이너
# 환경에는 남기지 않는다. 운영자가 값을 명시하지 않은 재적용/복구 상황에는
# 현재 실행 중인 명령줄에서 기존 값을 찾아 원본 Compose를 그대로 재사용한다.
if [[ -z ${KROOT_ACME_EMAIL:-} ]] && docker inspect kroot-shared-lb >/dev/null 2>&1; then
  KROOT_ACME_EMAIL=$(docker inspect kroot-shared-lb --format '{{range .Config.Cmd}}{{println .}}{{end}}' \
    | sed -n 's/^--certificatesresolvers\.kroot\.acme\.email=//p' \
    | head -n 1)
  export KROOT_ACME_EMAIL
fi
: "${KROOT_ACME_EMAIL:?set KROOT_ACME_EMAIL or keep the existing kroot-shared-lb running}"

for path in "$base_compose" "$override_compose" "$cert" "$key"; do
  test -r "$path" || { printf 'required file is not readable: %s\n' "$path" >&2; exit 1; }
done

openssl x509 -in "$cert" -noout -checkend 2592000 >/dev/null || {
  printf 'preview certificate expires in less than 30 days\n' >&2
  exit 1
}
sans=$(openssl x509 -in "$cert" -noout -ext subjectAltName)
printf '%s\n' "$sans" | grep -F 'DNS:preview.kroot.io' >/dev/null
printf '%s\n' "$sans" | grep -F 'DNS:*.preview.kroot.io' >/dev/null
cert_public=$(openssl x509 -in "$cert" -pubkey -noout | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)
key_public=$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)
test "$cert_public" = "$key_public" || { printf 'preview certificate and key do not match\n' >&2; exit 1; }

umask 077
mkdir -p "$install_dir"
install -m 0644 "$cert" "$install_dir/fullchain.pem.next"
install -m 0600 "$key" "$install_dir/privkey.pem.next"
mv -f "$install_dir/fullchain.pem.next" "$install_dir/fullchain.pem"
mv -f "$install_dir/privkey.pem.next" "$install_dir/privkey.pem"

docker compose -p kroot-shared-edge -f "$base_compose" -f "$override_compose" config --quiet
docker compose -p kroot-shared-edge -f "$base_compose" -f "$override_compose" up -d --no-deps kroot-shared-lb

deadline=$((SECONDS + 60))
until docker inspect kroot-shared-lb --format '{{.State.Running}}' 2>/dev/null | grep -qx true; do
  if (( SECONDS >= deadline )); then
    docker logs --tail 100 kroot-shared-lb >&2 || true
    exit 1
  fi
  sleep 1
done

printf 'Pie Preview wildcard TLS override applied.\n'
printf 'Rollback: docker compose -p kroot-shared-edge -f %q up -d --no-deps --force-recreate kroot-shared-lb\n' "$base_compose"
