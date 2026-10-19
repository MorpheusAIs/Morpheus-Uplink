#!/bin/sh
# Ensure DATA_DIR is writable by the uplink user. SecretVM / Docker volume
# mounts often arrive root-owned, which breaks ephemeral key persistence
# (open /data/uplink-state.json.tmp: permission denied).
# Optional bake.env (SecretVM configs mount) applies non-secret defaults
# without putting them on environment: (SecretVM scrapes every key).
#
# Railway / no-sidecar path: when COOKIE_CONTENT is set and ROUTER_AUTH_FILE
# is empty/missing, mint scoped user then drop COOKIE_CONTENT from the
# environment before exec. UPLINK_MOR_ALLOWANCE_WEI is an optional cap:
# unset/empty = wallet MOR balance at boot; set (incl. 0) = explicit cap.
# Allowance metadata is written beside the auth file
# (router.auth -> router.mor-allowance.json) unless UPLINK_MOR_ALLOWANCE_FILE.
#
# Sidecar handoff: router-user-init may write /shared/router.auth as uid 100
# mode 0400 on a root-owned volume. Uplink runs as uid 10001 and mounts
# :ro, so chown on the shared path fails. As root, copy into DATA_DIR and
# point ROUTER_AUTH_FILE at the owned copy before dropping privileges.
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
if [ -n "${COOKIE_CONTENT:-}" ]; then
  if [ -z "$AUTH_FILE" ] || [ ! -s "$AUTH_FILE" ]; then
    AUTH_FILE="${AUTH_FILE:-$DATA_DIR/router.auth}"
    export ROUTER_AUTH_OUT="$AUTH_FILE"
    case "$AUTH_FILE" in
      *.auth) ALLOW_DEFAULT="${AUTH_FILE%.auth}.mor-allowance.json" ;;
      *)      ALLOW_DEFAULT="${AUTH_FILE}.mor-allowance.json" ;;
    esac
    export UPLINK_MOR_ALLOWANCE_FILE="${UPLINK_MOR_ALLOWANCE_FILE:-$ALLOW_DEFAULT}"
    export ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
    echo "uplink-entrypoint: minting scoped router user (no sidecar path)"
    /usr/local/bin/router-user-init.sh
    export ROUTER_AUTH_FILE="$AUTH_FILE"
    unset COOKIE_CONTENT
  fi
fi

if [ "$(id -u)" = "0" ]; then
  chown -R uplink:uplink "$DATA_DIR" 2>/dev/null || true
  # Copy shared auth into DATA_DIR so uid 10001 can read it after drop.
  if [ -n "${ROUTER_AUTH_FILE:-}" ] && [ -f "${ROUTER_AUTH_FILE}" ]; then
    case "${ROUTER_AUTH_FILE}" in
      "$DATA_DIR"/*) ;;
      *)
        cp -f "${ROUTER_AUTH_FILE}" "$DATA_DIR/router.auth"
        chown uplink:uplink "$DATA_DIR/router.auth"
        chmod 0400 "$DATA_DIR/router.auth"
        export ROUTER_AUTH_FILE="$DATA_DIR/router.auth"
        echo "uplink-entrypoint: copied ROUTER_AUTH into DATA_DIR (uid 10001)"
        ;;
    esac
  fi
  if [ -n "${ROUTER_AUTH_FILE:-}" ] && [ -f "${ROUTER_AUTH_FILE}" ]; then
    chown uplink:uplink "${ROUTER_AUTH_FILE}" 2>/dev/null || true
    chmod 0400 "${ROUTER_AUTH_FILE}" 2>/dev/null || true
  fi
  exec su-exec uplink /usr/local/bin/uplink "$@"
fi
exec /usr/local/bin/uplink "$@"
