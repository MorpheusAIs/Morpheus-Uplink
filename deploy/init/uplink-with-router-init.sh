#!/bin/sh
# Railway / single-container variant: mint scoped router user then exec uplink.
# Expects curl available (bake into image or use distroless+curl). Prefer sidecar.
set -eu
if [ -n "${COOKIE_CONTENT:-}" ] && [ -n "${UPLINK_MOR_ALLOWANCE_WEI:-}" ]; then
  export ROUTER_AUTH_OUT="${ROUTER_AUTH_OUT:-/data/router.auth}"
  export ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
  /usr/local/bin/router-user-init.sh || {
    echo "uplink-with-router-init: scoped user init failed" >&2
    exit 1
  }
  export ROUTER_AUTH_FILE="$ROUTER_AUTH_OUT"
  unset COOKIE_CONTENT
fi
exec /docker-entrypoint.sh "$@"
