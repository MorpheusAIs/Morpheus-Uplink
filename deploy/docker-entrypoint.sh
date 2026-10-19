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
#
# Restart safety (sidecar path). /shared is a persistent volume, so after a
# host reboot the files from the PREVIOUS boot are still there, and the
# Docker daemon may restart this container before router-user-init has run
# again (restart policies ignore depends_on). router-user-init therefore
# writes a ready marker LAST (router.ready: boot id, random generation and
# sha256 of each file). Before copying anything we wait (bounded by
# UPLINK_INIT_WAIT_SECONDS, default 300) for a marker whose boot id matches
# this kernel boot, snapshot the files into a root-only staging dir, verify
# every hash against the marker, and confirm the marker did not change while
# we copied. No match within the timeout => exit 1 with a clear message.
# The scoped credential then stays on disk (/shared and the DATA_DIR copy);
# init keeps it while the router accepts it and re-mints it otherwise.
set -e
if [ -f /etc/uplink/bake.env ]; then
  set -a
  # shellcheck disable=SC1091
  . /etc/uplink/bake.env
  set +a
fi
DATA_DIR="${DATA_DIR:-/data}"
# The allowance manager's status file and request dir are separate volumes;
# they must never sit under DATA_DIR (which is chowned to uplink below).
_dd="${DATA_DIR%/}/"
for _p in "${UPLINK_ALLOWANCE_STATUS_FILE:-}" "${UPLINK_ALLOWANCE_CTL_DIR:-}"; do
  [ -n "$_p" ] || continue
  case "${_p%/}/" in
    "$_dd"*)
      echo "uplink-entrypoint: FATAL: $_p is under DATA_DIR ($DATA_DIR); the allowance status/ctl volumes must be separate." >&2
      exit 1 ;;
  esac
done
mkdir -p "$DATA_DIR"
IS_ROOT=0
[ "$(id -u)" = "0" ] && IS_ROOT=1

# handoff SRC DST MODE: copy SRC (readable by the current uid) to DST as
# the uplink user with MODE, atomically. Root path uses su-exec so DST is owned by
# the uplink user without needing DAC_OVERRIDE on DST's directory.
handoff() {
  _src="$1"; _dst="$2"; _mode="$3"
  if [ "$IS_ROOT" = "1" ]; then
    # shellcheck disable=SC2016
    su-exec uplink /bin/sh -c '
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
      unset UPLINK_MOR_ALLOWANCE_FILE UPLINK_ROUTER_WALLET_FILE
      # Keep the staging dir 0700 throughout (init would otherwise open it
      # to 0755 for the shared-volume case).
      export ROUTER_AUTH_DIR_MODE=0700
      export UPLINK_ROUTER_WALLET_FILE="$STAGE/router.wallet.json"
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
    export ROUTER_AUTH_FILE="$AUTH_FILE"
    unset COOKIE_CONTENT
  fi
fi

BOOT_ID_FILE="${UPLINK_BOOT_ID_FILE:-/proc/sys/kernel/random/boot_id}"

# marker_field FILE KEY -> value (empty if missing).
marker_field() {
  sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n 1
}

# wait_for_init_snapshot SRC_AUTH: wait for router-user-init's ready marker
# for this boot, then copy auth (+ allowance, wallet) into $SNAP and verify
# them against the marker. Sets SNAP_AUTH / SNAP_ALLOW / SNAP_WALLET.
wait_for_init_snapshot() {
  _auth="$1"
  _dir=$(dirname "$_auth")
  _ready="${ROUTER_AUTH_READY_FILE:-$_dir/router.ready}"
  _allow="${UPLINK_MOR_ALLOWANCE_FILE:-$_dir/router.mor-allowance.json}"
  _wallet="${UPLINK_ROUTER_WALLET_FILE:-$_dir/router.wallet.json}"
  _boot=$(tr -d ' \r\n' < "$BOOT_ID_FILE" 2>/dev/null || true)
  _limit="${UPLINK_INIT_WAIT_SECONDS:-300}"
  _waited=0
  _said=0
  if [ -z "$_boot" ]; then
    echo "uplink-entrypoint: WARNING: boot id unavailable ($BOOT_ID_FILE); accepting any consistent init marker" >&2
  fi
  SNAP=$(mktemp -d /tmp/uplink-handoff.XXXXXX)
  chmod 0700 "$SNAP"
  while :; do
    if [ -s "$_ready" ]; then
      _gen=$(marker_field "$_ready" generation)
      _mboot=$(marker_field "$_ready" boot_id)
      if [ -n "$_gen" ] && { [ -z "$_boot" ] || [ "$_mboot" = "$_boot" ]; }; then
        rm -f "$SNAP"/*
        if cat "$_auth" > "$SNAP/router.auth" 2>/dev/null \
          && cat "$_allow" > "$SNAP/router.mor-allowance.json" 2>/dev/null \
          && cat "$_wallet" > "$SNAP/router.wallet.json" 2>/dev/null \
          && [ "$(sha256sum "$SNAP/router.auth" | cut -d' ' -f1)" = "$(marker_field "$_ready" auth_sha256)" ] \
          && [ "$(sha256sum "$SNAP/router.mor-allowance.json" | cut -d' ' -f1)" = "$(marker_field "$_ready" allowance_sha256)" ] \
          && [ "$(sha256sum "$SNAP/router.wallet.json" | cut -d' ' -f1)" = "$(marker_field "$_ready" wallet_sha256)" ] \
          && [ "$(marker_field "$_ready" generation)" = "$_gen" ]; then
          SNAP_AUTH="$SNAP/router.auth"
          SNAP_ALLOW="$SNAP/router.mor-allowance.json"
          SNAP_WALLET="$SNAP/router.wallet.json"
          echo "uplink-entrypoint: router-user-init ready for this boot (generation $_gen)"
          return 0
        fi
      fi
    fi
    if [ "$_waited" -ge "$_limit" ]; then
      echo "uplink-entrypoint: FATAL: no router-user-init ready marker for this boot after ${_limit}s ($_ready)." >&2
      echo "uplink-entrypoint: refusing to use credentials left by an earlier boot. Run the full stack (docker compose up -d) so router-user-init runs first." >&2
      rm -rf "$SNAP"
      return 1
    fi
    if [ "$_said" = "0" ]; then
      echo "uplink-entrypoint: waiting for router-user-init to finish for this boot (up to ${_limit}s)..."
      _said=1
    fi
    sleep 1
    _waited=$((_waited + 1))
  done
}

if [ "$IS_ROOT" = "1" ]; then
  chown -R uplink:uplink "$DATA_DIR" 2>/dev/null || true
  if [ -n "${ROUTER_AUTH_FILE:-}" ]; then
    case "${ROUTER_AUTH_FILE}" in
      "$DATA_DIR"/*) ;;
      *)
        if [ -z "${STAGE:-}" ] && [ "${UPLINK_REQUIRE_INIT_MARKER:-true}" != "false" ]; then
          # Sidecar path: only files that match this boot's marker.
          wait_for_init_snapshot "${ROUTER_AUTH_FILE}" || exit 1
          handoff "$SNAP_AUTH" "$DATA_DIR/router.auth" 0400
          handoff "$SNAP_ALLOW" "$DATA_DIR/router.mor-allowance.json" 0444
          handoff "$SNAP_WALLET" "$DATA_DIR/router.wallet.json" 0444
          rm -rf "$SNAP"
          export ROUTER_AUTH_FILE="$DATA_DIR/router.auth"
          export UPLINK_MOR_ALLOWANCE_FILE="$DATA_DIR/router.mor-allowance.json"
          export UPLINK_ROUTER_WALLET_FILE="$DATA_DIR/router.wallet.json"
          echo "uplink-entrypoint: copied ROUTER_AUTH, allowance and wallet into DATA_DIR (owner uplink uid $(id -u uplink), auth mode 0400)"
        else
          # (test -r lies for root in busybox; let the actual read decide.)
          if ! handoff "${ROUTER_AUTH_FILE}" "$DATA_DIR/router.auth" 0400; then
            echo "uplink-entrypoint: cannot hand off ${ROUTER_AUTH_FILE} (expect root-owned 0400 from router-user-init; uplink has no DAC_OVERRIDE)" >&2
            ls -ln "${ROUTER_AUTH_FILE}" >&2 2>/dev/null || true
            exit 1
          fi
          export ROUTER_AUTH_FILE="$DATA_DIR/router.auth"
          echo "uplink-entrypoint: copied ROUTER_AUTH into DATA_DIR (owner uplink uid $(id -u uplink), mode 0400)"
        fi
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
  if [ -n "${UPLINK_ROUTER_WALLET_FILE:-}" ] && [ -f "${UPLINK_ROUTER_WALLET_FILE}" ]; then
    case "${UPLINK_ROUTER_WALLET_FILE}" in
      "$DATA_DIR"/*) ;;
      *)
        if handoff "${UPLINK_ROUTER_WALLET_FILE}" "$DATA_DIR/router.wallet.json" 0444; then
          export UPLINK_ROUTER_WALLET_FILE="$DATA_DIR/router.wallet.json"
        fi
        ;;
    esac
  fi
  if [ -n "${STAGE:-}" ]; then
    rm -rf "$STAGE"
  fi
  exec su-exec uplink /usr/local/bin/uplink "$@"
fi
exec /usr/local/bin/uplink "$@"
