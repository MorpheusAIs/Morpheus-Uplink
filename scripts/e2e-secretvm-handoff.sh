#!/usr/bin/env bash
# E2E: SecretVM-shaped stack with a mock router. Runs router-user-init, then
# Uplink with the real SecretVM caps (cap_drop ALL + CHOWN/SETUID/SETGID, no
# DAC_OVERRIDE) and the router_auth :ro handoff, and asserts Uplink reaches
# a healthy /healthcheck. Then re-runs init + Uplink on the same volumes
# (second boot) and asserts healthy again.
#
# Needs a Docker daemon. Image: $UPLINK_E2E_IMAGE (default uplink:e2e);
# pass --build to build it from the repo first.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

export UPLINK_E2E_IMAGE="${UPLINK_E2E_IMAGE:-uplink:e2e}"
if [ "${1:-}" = "--build" ]; then
  docker build -t "$UPLINK_E2E_IMAGE" --build-arg VERSION=e2e .
fi

export E2E_MOCK_ROUTER="$ROOT/scripts/testdata/e2e_mock_router.py"
# Throwaway fillables (no real wallet / RPC).
export ADMIN_PASSWORD="e2e-Admin-$(openssl rand -hex 12)"
export API_KEY_SEED="$(openssl rand -hex 32)"
export COOKIE_CONTENT="admin:e2e-cookie-$(openssl rand -hex 12)"
export WALLET_PRIVATE_KEY="0x$(openssl rand -hex 32)"
export ETH_NODE_ADDRESS="https://rpc.invalid"
export WEB_PUBLIC_URL="https://uplink.example"
# Allowance unset/empty => balance path (mock balance 0 => allowance 0; fine).
export UPLINK_MOR_ALLOWANCE_WEI="${UPLINK_MOR_ALLOWANCE_WEI:-}"

PROJECT="uplink-e2e-$$"
# Optional third overlay (e.g. hosts where container bridge networking is
# unavailable); CI uses the two files only.
EXTRA=()
if [ -n "${E2E_EXTRA_COMPOSE:-}" ]; then
  EXTRA=(-f "$E2E_EXTRA_COMPOSE")
fi
dc() {
  docker compose -p "$PROJECT" \
    -f deploy/secretvm/docker-compose.yml \
    -f scripts/testdata/e2e-secretvm-override.yml "${EXTRA[@]}" "$@"
}
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "---- compose ps ----"; dc ps -a || true
    echo "---- logs ----"; dc logs --no-color router-user-init uplink proxy-router 2>&1 | tail -n 200 || true
  fi
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  exit "$rc"
}
trap cleanup EXIT

wait_healthy() {
  local id status i
  id=$(dc ps -q uplink)
  [ -n "$id" ] || { echo "uplink container missing" >&2; return 1; }
  for i in $(seq 1 60); do
    status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$id")
    case "$status" in
      healthy) echo "uplink healthy after ~$((i * 2))s"; return 0 ;;
    esac
    if [ "$(docker inspect -f '{{.State.Running}}' "$id")" != "true" ]; then
      echo "uplink not running (status=$status)" >&2
      return 1
    fi
    sleep 2
  done
  echo "uplink did not become healthy (last status=$status)" >&2
  return 1
}

assert_handoff() {
  # Shared file: root-owned 0400 (written by init); DATA_DIR copy: uplink 0400.
  local shared copy body
  shared=$(dc exec -T uplink stat -c '%u %a' /shared/router.auth)
  copy=$(dc exec -T uplink sh -c 'stat -c "%u %a" /data/router.auth')
  want_uid=$(dc exec -T uplink id -u uplink)
  echo "shared router.auth: uid/mode=$shared ; data copy: uid/mode=$copy (uplink uid $want_uid)"
  [ "$shared" = "0 400" ] || { echo "shared auth must be root 0400" >&2; return 1; }
  [ "$copy" = "$want_uid 400" ] || { echo "data copy must be uplink 0400" >&2; return 1; }
  body=$(dc exec -T uplink wget -qO- http://127.0.0.1:8080/healthcheck)
  echo "healthcheck: $body"
  case "$body" in *'"status":"healthy"'*) ;; *) echo "unexpected healthcheck body" >&2; return 1 ;; esac
  # Uplink service env must not carry the admin cookie (init + router only).
  if docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(dc ps -q uplink)" | grep -q '^COOKIE_CONTENT='; then
    echo "COOKIE_CONTENT present in uplink container env" >&2
    return 1
  fi
}

echo "== boot 1 =="
dc up -d uplink
wait_healthy
assert_handoff

echo "== boot 2 (same volumes, init re-mints) =="
dc up -d --force-recreate router-user-init uplink
wait_healthy
assert_handoff

echo "OK: SecretVM auth handoff e2e (init -> uplink healthy, twice)"
