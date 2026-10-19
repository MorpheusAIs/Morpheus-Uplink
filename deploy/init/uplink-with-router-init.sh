#!/bin/sh
# Railway / single-container variant: mint scoped router user then exec uplink.
# Expects curl available (baked into image). Prefer sidecar where available.
# Runs when COOKIE_CONTENT is set (allowance auto from balance if env unset).
set -eu
if [ -n "${COOKIE_CONTENT:-}" ]; then
  export ROUTER_AUTH_OUT="${ROUTER_AUTH_OUT:-/data/router.auth}"
  export ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
  export UPLINK_MOR_ALLOWANCE_FILE="${UPLINK_MOR_ALLOWANCE_FILE:-/data/router.mor-allowance.json}"
  /usr/local/bin/router-user-init.sh || {
    echo "uplink-with-router-init: scoped user init failed" >&2
    exit 1
  }
  export ROUTER_AUTH_FILE="$ROUTER_AUTH_OUT"
  unset COOKIE_CONTENT
fi
exec /docker-entrypoint.sh "$@"
