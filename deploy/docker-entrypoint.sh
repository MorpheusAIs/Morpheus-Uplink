#!/bin/sh
# Ensure DATA_DIR is writable by the uplink user. SecretVM / Docker volume
# mounts often arrive root-owned, which breaks ephemeral key persistence
# (open /data/uplink-state.json.tmp: permission denied).
# Optional bake.env (SecretVM configs mount) applies non-secret defaults
# without putting them on environment: (SecretVM scrapes every key).
#
# Railway / no-sidecar path: when COOKIE_CONTENT and UPLINK_MOR_ALLOWANCE_WEI
# are set and ROUTER_AUTH_FILE is empty/missing, mint scoped user then drop
# COOKIE_CONTENT from the environment before exec.
set -e
if [ -f /etc/uplink/bake.env ]; then
  set -a
  # shellcheck disable=SC1091
  . /etc/uplink/bake.env
  set +a
fi
DATA_DIR="${DATA_DIR:-/data}"
mkdir -p "$DATA_DIR"

AUTH_FILE="${ROUTER_AUTH_FILE:-}"
if [ -n "${COOKIE_CONTENT:-}" ] && [ -n "${UPLINK_MOR_ALLOWANCE_WEI:-}" ]; then
  if [ -z "$AUTH_FILE" ] || [ ! -s "$AUTH_FILE" ]; then
    AUTH_FILE="${AUTH_FILE:-$DATA_DIR/router.auth}"
    export ROUTER_AUTH_OUT="$AUTH_FILE"
    export ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
    echo "uplink-entrypoint: minting scoped router user (no sidecar path)"
    /usr/local/bin/router-user-init.sh
    export ROUTER_AUTH_FILE="$AUTH_FILE"
    unset COOKIE_CONTENT
  fi
fi

if [ "$(id -u)" = "0" ]; then
  chown -R uplink:uplink "$DATA_DIR" 2>/dev/null || true
  if [ -n "${ROUTER_AUTH_FILE:-}" ] && [ -f "${ROUTER_AUTH_FILE}" ]; then
    chown uplink:uplink "${ROUTER_AUTH_FILE}" 2>/dev/null || true
  fi
  exec su-exec uplink /usr/local/bin/uplink "$@"
fi
exec /usr/local/bin/uplink "$@"
