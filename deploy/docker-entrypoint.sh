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
#
# Auth handoff (least privilege). The uplink container runs with cap_drop ALL
# + CHOWN/SETUID/SETGID only: no CAP_DAC_OVERRIDE / CAP_DAC_READ_SEARCH, so
# even uid 0 is bound by file modes. Therefore:
#   - router-user-init writes /shared/router.auth ROOT-owned, mode 0400
#     (root reads it here as the file owner; no DAC override needed).
#   - As root we read it and stream it into DATA_DIR *as the uplink user*
#     (su-exec), so the copy is uplink-owned 0400 in uplink's own directory.
#     Root never needs write access to the uplink-owned DATA_DIR.
#   - Same for the allowance metadata (no secrets; mode 0444).
#   - ROUTER_AUTH_FILE / UPLINK_MOR_ALLOWANCE_FILE then point at the copies.
set -e
if [ -f /etc/uplink/bake.env ]; then
  set -a
  # shellcheck disable=SC1091
  . /etc/uplink/bake.env
  set +a
fi
DATA_DIR="${DATA_DIR:-/data}"
mkdir -p "$DATA_DIR"
RUN_USER="${UPLINK_RUN_USER:-uplink}"
IS_ROOT=0
[ "$(id -u)" = "0" ] && IS_ROOT=1

# handoff SRC DST MODE: copy SRC (readable by the current uid) to DST as
# RUN_USER with MODE, atomically. Root path uses su-exec so DST is owned by
# RUN_USER without needing DAC_OVERRIDE on DST's directory.
handoff() {
  _src="$1"; _dst="$2"; _mode="$3"
  if [ "$IS_ROOT" = "1" ]; then
    # shellcheck disable=SC2016
    su-exec "$RUN_USER" /bin/sh -c '
      set -e
      umask 077
      rm -f "$1.tmp"
      cat > "$1.tmp"
      chmod "$2" "$1.tmp"
      mv -f "$1.tmp" "$1"
    ' handoff "$_dst" "$_mode" < "$_src"
  else
    umask 077
    rm -f "$_dst.tmp"
    cat "$_src" > "$_dst.tmp"
    chmod "$_mode" "$_dst.tmp"
    mv -f "$_dst.tmp" "$_dst"
  fi
}

AUTH_FILE="${ROUTER_AUTH_FILE:-}"
if [ -n "${COOKIE_CONTENT:-}" ]; then
  if [ -z "$AUTH_FILE" ] || [ ! -s "$AUTH_FILE" ]; then
    if [ "$IS_ROOT" = "1" ]; then
      # Mint into a root-only staging dir (root cannot write the
      # uplink-owned DATA_DIR without DAC_OVERRIDE), then hand off below.
      STAGE=$(mktemp -d /tmp/uplink-init.XXXXXX)
      chmod 0700 "$STAGE"
      AUTH_FILE="$STAGE/router.auth"
      ALLOW_DEFAULT="$STAGE/router.mor-allowance.json"
      unset UPLINK_MOR_ALLOWANCE_FILE
    else
      AUTH_FILE="${AUTH_FILE:-$DATA_DIR/router.auth}"
      case "$AUTH_FILE" in
        *.auth) ALLOW_DEFAULT="${AUTH_FILE%.auth}.mor-allowance.json" ;;
        *)      ALLOW_DEFAULT="${AUTH_FILE}.mor-allowance.json" ;;
      esac
    fi
    export ROUTER_AUTH_OUT="$AUTH_FILE"
    export UPLINK_MOR_ALLOWANCE_FILE="${UPLINK_MOR_ALLOWANCE_FILE:-$ALLOW_DEFAULT}"
    export ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
    echo "uplink-entrypoint: minting scoped router user (no sidecar path)"
    /usr/local/bin/router-user-init.sh
    [ -n "${STAGE:-}" ] && chmod 0700 "$STAGE"
    export ROUTER_AUTH_FILE="$AUTH_FILE"
    unset COOKIE_CONTENT
  fi
fi

if [ "$IS_ROOT" = "1" ]; then
  chown -R "$RUN_USER:$RUN_USER" "$DATA_DIR" 2>/dev/null || true
  if [ -n "${ROUTER_AUTH_FILE:-}" ]; then
    case "${ROUTER_AUTH_FILE}" in
      "$DATA_DIR"/*) ;;
      *)
        # (test -r lies for root in busybox; let the actual read decide.)
        if ! handoff "${ROUTER_AUTH_FILE}" "$DATA_DIR/router.auth" 0400; then
          echo "uplink-entrypoint: cannot hand off ${ROUTER_AUTH_FILE} (expect root-owned 0400 from router-user-init; uplink has no DAC_OVERRIDE)" >&2
          ls -ln "${ROUTER_AUTH_FILE}" >&2 2>/dev/null || true
          exit 1
        fi
        export ROUTER_AUTH_FILE="$DATA_DIR/router.auth"
        echo "uplink-entrypoint: copied ROUTER_AUTH into DATA_DIR (owner $RUN_USER uid $(id -u "$RUN_USER"), mode 0400)"
        ;;
    esac
  fi
  if [ -n "${UPLINK_MOR_ALLOWANCE_FILE:-}" ] && [ -f "${UPLINK_MOR_ALLOWANCE_FILE}" ]; then
    case "${UPLINK_MOR_ALLOWANCE_FILE}" in
      "$DATA_DIR"/*) ;;
      *)
        if ! handoff "${UPLINK_MOR_ALLOWANCE_FILE}" "$DATA_DIR/router.mor-allowance.json" 0444; then
          echo "uplink-entrypoint: cannot hand off ${UPLINK_MOR_ALLOWANCE_FILE}" >&2
          exit 1
        fi
        export UPLINK_MOR_ALLOWANCE_FILE="$DATA_DIR/router.mor-allowance.json"
        ;;
    esac
  fi
  if [ -n "${STAGE:-}" ]; then
    rm -rf "$STAGE"
  fi
  exec su-exec "$RUN_USER" /usr/local/bin/uplink "$@"
fi
exec /usr/local/bin/uplink "$@"
