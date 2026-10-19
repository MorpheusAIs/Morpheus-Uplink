#!/bin/sh
# Create scoped router user "uplink" with MOR allowance. Idempotent.
# Writes uplink:<password> to ROUTER_AUTH_OUT (mode 0400, owned by the init
# uid, i.e. root in compose). Never logs the password.
# Allowance: UPLINK_MOR_ALLOWANCE_WEI set (all digits, incl. 0) is an explicit
# cap; unset/empty = wallet MOR balance at boot. 0 means allowance 0
# (source=zero, warning) and the balance is NOT read.
# Writes allowance metadata beside the auth file (mode 0444, same owner), the
# router wallet address (router.wallet.json, 0444, no secret) and, LAST, a
# ready marker (router.ready, 0400) carrying this boot's id, a fresh random
# generation and sha256 of the files above. The uplink entrypoint only copies
# files that match a marker for the current boot, so a credential left on a
# persistent volume by an earlier boot is never used.
#
# Old init-owned files are removed at start, before anything else, so a
# reader can never pair a previous boot's files with this boot's marker.
# Every file is written atomically: mktemp in the same directory (umask 077),
# chmod, then rename.
set -eu
# Core dumps off: this process holds the admin cookie.
ulimit -c 0 2>/dev/null || true
# shellcheck source=router-admin-lib.sh
. "${ROUTER_ADMIN_LIB:-$(dirname "$0")/router-admin-lib.sh}"

ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
AUTH_OUT="${ROUTER_AUTH_OUT:-/shared/router.auth}"
USER_NAME="${UPLINK_ROUTER_USER:-uplink}"
MOR_TOKEN="${MOR_TOKEN_ADDRESS:-0x7431ada8a591c955a994a21710752ef9b882b8e3}"
# Lowercase token address (router allowance lookup is case-sensitive to lower).
MOR_TOKEN=$(printf '%s' "$MOR_TOKEN" | tr 'A-F' 'a-f')

# Allowance file defaults beside auth: /shared/router.auth -> /shared/router.mor-allowance.json
case "$AUTH_OUT" in
  *.auth) ALLOWANCE_FILE_DEFAULT="${AUTH_OUT%.auth}.mor-allowance.json" ;;
  *)      ALLOWANCE_FILE_DEFAULT="${AUTH_OUT}.mor-allowance.json" ;;
esac
ALLOWANCE_FILE="${UPLINK_MOR_ALLOWANCE_FILE:-$ALLOWANCE_FILE_DEFAULT}"
AUTH_DIR=$(dirname "$AUTH_OUT")
WALLET_FILE="${UPLINK_ROUTER_WALLET_FILE:-$AUTH_DIR/router.wallet.json}"
READY_FILE="${ROUTER_AUTH_READY_FILE:-$AUTH_DIR/router.ready}"
# Boot id shared by every container on this kernel. Overridable for tests.
BOOT_ID_FILE="${UPLINK_BOOT_ID_FILE:-/proc/sys/kernel/random/boot_id}"

COOKIE_CONTENT="${COOKIE_CONTENT:?COOKIE_CONTENT required for admin confirm}"
ADMIN_CRED="$COOKIE_CONTENT"

# Allowance manager volumes (SecretVM / generic). Each is handled only when
# the directory exists (mounted), so the no-sidecar entrypoint path and the
# local tests skip them.
MGR_DIR="${ROUTER_MANAGER_CRED_DIR:-/mgr}"
STATUS_DIR="${ROUTER_ALLOWANCE_STATUS_DIR:-/status}"
CTL_DIR="${ROUTER_ALLOWANCE_CTL_DIR:-/ctl}"
MANAGER_UID="${ROUTER_MANAGER_UID:-10002}"
UPLINK_UID="${UPLINK_UID:-10001}"
CTL_GID="${ROUTER_ALLOWANCE_CTL_GID:-10003}"
MANAGER_GID="${ROUTER_MANAGER_GID:-$MANAGER_UID}"
# Owner used for root-owned dirs/files; overridable only so the unprivileged
# integration test can exercise the same code path.
ROOT_OWNER="${ROUTER_INIT_ROOT_OWNER:-0:0}"

umask 077
# Keep the current scoped password in memory (never logged): if the router
# still accepts it, it is kept and only the allowance is updated in place,
# so a running Uplink keeps working when init runs again.
KEEP_PASS=""
if [ -s "$AUTH_OUT" ]; then
  _old=$(head -n 1 "$AUTH_OUT" 2>/dev/null || true)
  case "$_old" in
    "$USER_NAME":?*) KEEP_PASS="${_old#*:}" ;;
  esac
  unset _old
fi
# Remove every file this script owns before doing anything else (marker
# first). From here until the new marker lands, readers wait.
rm -f "$READY_FILE" "$AUTH_OUT" "$ALLOWANCE_FILE" "$WALLET_FILE"
rm -f "$AUTH_DIR"/.router-init.* 2>/dev/null || true

# Admin credentials go to curl on stdin (-K -), never on argv.
admin_curl() {
  curl_with_user "$ADMIN_CRED" "$@"
}

echo "router-user-init: waiting for router healthcheck..."
i=0
while [ "$i" -lt 90 ]; do
  if curl -fsS "$ROUTER_URL/healthcheck" >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1))
  sleep 2
done
if [ "$i" -ge 90 ]; then
  echo "router-user-init: router healthcheck timeout" >&2
  exit 1
fi

# Resolve ALLOWANCE_WEI (matches Go config):
#   set + all digits -> explicit cap (leading zeros normalized; 0 = source zero)
#   unset / empty    -> wallet MOR balance at boot (else 0 + warning)
ALLOWANCE_WEI=""
ALLOWANCE_SOURCE=""
ALLOWANCE_WARNING=""

OVERRIDE="${UPLINK_MOR_ALLOWANCE_WEI:-}"
# Trim leading/trailing whitespace only. Internal whitespace ("1 000") is
# rejected below, matching Go config (which refuses boot on it) so the router
# user is never minted with a cap Uplink would reject.
OVERRIDE=$(printf '%s' "$OVERRIDE" | tr '\t\r\n' '   ' | sed 's/^ *//; s/ *$//')
if [ -n "$OVERRIDE" ]; then
  case "$OVERRIDE" in
    *[!0-9]*)
      echo "router-user-init: UPLINK_MOR_ALLOWANCE_WEI must be a non-negative integer (wei, digits only, no internal spaces)" >&2
      exit 1
      ;;
  esac
  OVERRIDE=$(printf '%s' "$OVERRIDE" | sed 's/^0*//')
  [ -z "$OVERRIDE" ] && OVERRIDE=0
  ALLOWANCE_WEI="$OVERRIDE"
  if [ "$OVERRIDE" = "0" ]; then
    ALLOWANCE_SOURCE="zero"
    ALLOWANCE_WARNING="MOR allowance is zero (UPLINK_MOR_ALLOWANCE_WEI=0); sessions will not open until the cap is raised and Uplink is restarted."
    echo "router-user-init: WARNING: UPLINK_MOR_ALLOWANCE_WEI=0; creating user with allowance 0 (balance not read)" >&2
  else
    ALLOWANCE_SOURCE="override"
    echo "router-user-init: using UPLINK_MOR_ALLOWANCE_WEI override"
  fi
elif [ "${UPLINK_MOR_ALLOWANCE_MODE:-}" = "ceiling" ]; then
  # Fallback D (explicit opt-in, no manager): a ceiling above total MOR
  # supply. No budget; the wallet balance is the only limit.
  ALLOWANCE_WEI="100000000000000000000000000"
  ALLOWANCE_SOURCE="ceiling"
  ALLOWANCE_WARNING="Allowance ceiling mode: no budget; the wallet is the only limit."
  echo "router-user-init: WARNING: UPLINK_MOR_ALLOWANCE_MODE=ceiling (no budget; the wallet is the only limit)" >&2
else
  BAL_JSON=""
  if BAL_JSON=$(admin_curl -fsS "$ROUTER_URL/blockchain/balance" 2>/dev/null); then
    ALLOWANCE_WEI=$(parse_mor_balance "$BAL_JSON")
    if [ -z "$ALLOWANCE_WEI" ]; then
      ALLOWANCE_WEI="0"
      ALLOWANCE_SOURCE="zero"
      ALLOWANCE_WARNING="MOR balance field missing or unparseable in router response; sessions will not open until funded and Uplink is restarted."
      echo "router-user-init: WARNING: could not parse mor from balance; creating user with allowance 0" >&2
    elif [ "$ALLOWANCE_WEI" = "0" ]; then
      ALLOWANCE_SOURCE="zero"
      ALLOWANCE_WARNING="MOR wallet balance is zero; sessions will not open until funded and Uplink is restarted."
      echo "router-user-init: WARNING: wallet MOR balance is zero; creating user with allowance 0 (restart after funding)" >&2
    else
      ALLOWANCE_SOURCE="balance"
      echo "router-user-init: using wallet MOR balance as allowance"
    fi
  else
    ALLOWANCE_WEI="0"
    ALLOWANCE_SOURCE="zero"
    ALLOWANCE_WARNING="Failed to read router MOR balance; sessions will not open until funded and Uplink is restarted."
    echo "router-user-init: WARNING: balance read failed; creating user with allowance 0 (restart after funding)" >&2
  fi
fi

# Escape warning for JSON (minimal: backslash and quotes).
json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

# write_owned_file DST MODE: atomic root-owned write. The temp file is made
# by mktemp in DST's own directory (so rename is atomic and never crosses a
# filesystem), starts 0600 under umask 077, gets MODE, then is renamed over
# DST. Files stay owned by the init uid (root): the uplink container has no
# DAC_OVERRIDE, so a file chowned to 10001 with 0400 would be unreadable to
# its root entrypoint. The entrypoint re-copies it into DATA_DIR as the
# uplink user.
write_owned_file() {
  _dst="$1"; _mode="$2"
  _tmp=$(mktemp "$(dirname "$_dst")/.router-init.XXXXXX")
  if ! cat > "$_tmp" || ! chmod "$_mode" "$_tmp" || ! mv -f "$_tmp" "$_dst"; then
    rm -f "$_tmp"
    return 1
  fi
}

write_allowance_file() {
  printf '{"wei":"%s","source":"%s","warning":"%s"}\n' \
    "$ALLOWANCE_WEI" "$ALLOWANCE_SOURCE" "$(json_escape "$ALLOWANCE_WARNING")" \
    | write_owned_file "$ALLOWANCE_FILE" 0444
  echo "router-user-init: wrote allowance file $ALLOWANCE_FILE (owner uid $(id -u), mode 0444, wei=$ALLOWANCE_WEI source=$ALLOWANCE_SOURCE)"
}

# Router wallet address (public, no secret). Recorded so Uplink can always
# show the funding address even when its own router calls fail.
write_wallet_file() {
  _w=$(admin_curl -fsS "$ROUTER_URL/wallet" 2>/dev/null || true)
  _addr=$(printf '%s' "$_w" | tr -d '\r\n' | grep -oE '"address"[[:space:]]*:[[:space:]]*"0x[0-9a-fA-F]{40}"' | head -n 1 | grep -oE '0x[0-9a-fA-F]{40}' || true)
  if [ -z "$_addr" ]; then
    echo "router-user-init: WARNING: could not read router wallet address; Uplink will rely on the router at runtime" >&2
    printf '{"address":""}\n' | write_owned_file "$WALLET_FILE" 0444
    return 0
  fi
  printf '{"address":"%s"}\n' "$_addr" | write_owned_file "$WALLET_FILE" 0444
  echo "router-user-init: wrote wallet file $WALLET_FILE (mode 0444)"
}

file_sha256() {
  sha256sum "$1" | cut -d' ' -f1
}

# Ready marker, written last. Root-owned 0400 (only the uplink entrypoint,
# running as root, reads it). No secret: the hash of a 256-bit random
# password does not reveal it.
write_ready_marker() {
  _boot=$(tr -d ' \r\n' < "$BOOT_ID_FILE" 2>/dev/null || true)
  _gen=$(dd if=/dev/urandom bs=16 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
  {
    printf 'boot_id=%s\n' "$_boot"
    printf 'generation=%s\n' "$_gen"
    printf 'auth_sha256=%s\n' "$(file_sha256 "$AUTH_OUT")"
    printf 'allowance_sha256=%s\n' "$(file_sha256 "$ALLOWANCE_FILE")"
    printf 'wallet_sha256=%s\n' "$(file_sha256 "$WALLET_FILE")"
  } | write_owned_file "$READY_FILE" 0400
  echo "router-user-init: wrote ready marker $READY_FILE (generation $_gen)"
}

# Scoped perms (pinned; CI check-allowance-manager-policy.sh). Never any user/allowance
# administration or transfer perm.
PERMS='["chat","embeddings","open_session","close_session","get_sessions","get_wallet","get_balance","get_supply","get_budget","get_stakes_on_hold","withdraw_user_stakes"]'

PASS=""
if [ -n "$KEEP_PASS" ] && scoped_credential_works "$ROUTER_URL" "$USER_NAME:$KEEP_PASS"; then
  if set_allowance_inplace "$ADMIN_CRED" "$ROUTER_URL" "$USER_NAME" "$MOR_TOKEN" "$ALLOWANCE_WEI"; then
    PASS="$KEEP_PASS"
    echo "router-user-init: existing scoped user still valid; password kept, allowance updated in place"
  else
    echo "router-user-init: in-place allowance update failed; re-creating the scoped user" >&2
  fi
fi
unset KEEP_PASS

if [ -z "$PASS" ]; then
  PASS=$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
  if [ "${#PASS}" -lt 32 ]; then
    echo "router-user-init: failed to generate password" >&2
    exit 1
  fi

  echo "router-user-init: delete existing user (ignore errors)..."
  admin_curl -fsS -X DELETE \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"${USER_NAME}\"}" \
    "$ROUTER_URL/auth/users" >/dev/null 2>&1 || true

  echo "router-user-init: request agent user..."
  REQ_FILE=$(mktemp)
  CONF_FILE=$(mktemp)
  trap 'rm -f "$REQ_FILE" "$CONF_FILE"' EXIT
  printf '{"username":"%s","password":"%s","perms":%s,"allowances":{"%s":"%s"}}' \
    "$USER_NAME" "$PASS" "$PERMS" "$MOR_TOKEN" "$ALLOWANCE_WEI" > "$REQ_FILE"

  curl -fsS -X POST -H 'Content-Type: application/json' \
    --data-binary @"$REQ_FILE" \
    "$ROUTER_URL/auth/users/request" >/dev/null

  echo "router-user-init: confirm agent user..."
  printf '{"username":"%s","confirm":true}' "$USER_NAME" > "$CONF_FILE"
  admin_curl -fsS -X POST \
    -H 'Content-Type: application/json' \
    --data-binary @"$CONF_FILE" \
    "$ROUTER_URL/auth/users/confirm" >/dev/null
  rm -f "$REQ_FILE" "$CONF_FILE"
fi

# --- Allowance manager volumes -------------------------------------------
# Only the mount-point directory itself is chowned/chmodded: never -R, never
# rm, never anything inside (entries in /ctl are Uplink-controlled; a planted
# symlink must not be followed). chmod runs while root owns the dir (no
# CAP_FOWNER needed), then ownership is handed over (CAP_CHOWN).
own_dir() {
  _d="$1"; _owner="$2"; _mode="$3"
  if [ -L "$_d" ] || [ ! -d "$_d" ]; then
    return 0
  fi
  chown -h "$ROOT_OWNER" "$_d" && chmod "$_mode" "$_d" && chown -h "$_owner" "$_d"
}

# write_mgr_file DST OWNER MODE: atomic (mktemp in /mgr, umask 077, chmod
# while still root-owned (no CAP_FOWNER), then chown,
# rename).
write_mgr_file() {
  _dst="$1"; _owner="$2"; _mode="$3"
  _tmp=$(mktemp "$MGR_DIR/.router-init.XXXXXX")
  if ! cat > "$_tmp" || ! chmod "$_mode" "$_tmp" || ! chown "$_owner" "$_tmp" || ! mv -f "$_tmp" "$_dst"; then
    rm -f "$_tmp"
    return 1
  fi
}

if [ -d "$MGR_DIR" ] && [ ! -L "$MGR_DIR" ]; then
  # /mgr: root:<manager gid> 0750 - only root and the manager can enter.
  chown -h "${ROOT_OWNER%%:*}:$MANAGER_GID" "$MGR_DIR"
  chmod 0750 "$MGR_DIR"
  rm -f "$MGR_DIR"/.router-init.* 2>/dev/null || true
  printf '%s' "$ADMIN_CRED" | write_mgr_file "$MGR_DIR/admin.cookie" "$MANAGER_UID:$MANAGER_GID" 0400
  # Mode marker: exactly "balance" (no newline) only for the empty-cap
  # balance mode; anything else means the manager never writes.
  if [ "$ALLOWANCE_SOURCE" = "override" ] || [ -n "$OVERRIDE" ]; then
    MODE_MARK="cap"
  elif [ "$ALLOWANCE_SOURCE" = "ceiling" ]; then
    MODE_MARK="ceiling"
  else
    MODE_MARK="balance"
  fi
  printf '%s' "$MODE_MARK" | write_mgr_file "$MGR_DIR/mode" "$ROOT_OWNER" 0444
  echo "router-user-init: wrote manager credential (0400, uid $MANAGER_UID) and mode marker ($MODE_MARK)"
fi
own_dir "$STATUS_DIR" "$MANAGER_UID:$MANAGER_GID" 0755
own_dir "$CTL_DIR" "$UPLINK_UID:$CTL_GID" 0730

# Shared dir must be traversable by the uplink entrypoint (root without
# DAC_READ_SEARCH); files inside stay 0400/0444. The no-sidecar entrypoint
# passes ROUTER_AUTH_DIR_MODE=0700 for its private staging dir.
chmod "${ROUTER_AUTH_DIR_MODE:-0755}" "$AUTH_DIR" 2>/dev/null || true
printf '%s:%s\n' "$USER_NAME" "$PASS" | write_owned_file "$AUTH_OUT" 0400
echo "router-user-init: wrote ROUTER_AUTH file $AUTH_OUT (owner uid $(id -u), mode 0400)"
write_allowance_file
write_wallet_file
write_ready_marker
unset ADMIN_CRED COOKIE_CONTENT
echo "router-user-init: done"
