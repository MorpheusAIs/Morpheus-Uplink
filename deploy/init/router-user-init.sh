#!/bin/sh
# Create scoped router user "uplink" with MOR allowance. Idempotent.
# Writes uplink:<password> to ROUTER_AUTH_OUT (mode 0400, owned by the init
# uid, i.e. root in compose). Never logs the password.
# Allowance: UPLINK_MOR_ALLOWANCE_WEI set (all digits, incl. 0) is an explicit
# cap; unset/empty = wallet MOR balance at boot. 0 means allowance 0
# (source=zero, warning) and the balance is NOT read.
# Writes allowance metadata beside the auth file (mode 0444, same owner).
set -eu

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

COOKIE_CONTENT="${COOKIE_CONTENT:?COOKIE_CONTENT required for admin confirm}"
ADMIN_USER=$(printf '%s' "$COOKIE_CONTENT" | cut -d: -f1)
ADMIN_PASS=$(printf '%s' "$COOKIE_CONTENT" | cut -d: -f2-)

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
# parse_mor_balance JSON -> decimal wei on stdout, or empty if not parseable.
# Requires exactly one "mor" key (string or bare integer); rejects decimals,
# exponents, signs, hex and duplicate/ambiguous keys. Leading zeros dropped.
parse_mor_balance() {
  _flat=$(printf '%s' "$1" | tr -d '\r\n')
  _m=$(printf '%s' "$_flat" | grep -oE '"mor"[[:space:]]*:[[:space:]]*("[^"]*"|[^,}[:space:]]+)' || true)
  [ -n "$_m" ] || return 0
  [ "$(printf '%s\n' "$_m" | wc -l | tr -d ' ')" = "1" ] || return 0
  _v=$(printf '%s' "$_m" | sed 's/^"mor"[[:space:]]*:[[:space:]]*//; s/^"//; s/"$//')
  case "$_v" in
    ''|*[!0-9]*) return 0 ;;
  esac
  _v=$(printf '%s' "$_v" | sed 's/^0*//')
  [ -n "$_v" ] || _v=0
  printf '%s' "$_v"
}

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
else
  BAL_JSON=""
  if BAL_JSON=$(curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" "$ROUTER_URL/blockchain/balance" 2>/dev/null); then
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

# write_owned_file DST MODE: atomic root-owned write (tmp + mv). Files stay
# owned by the init uid (root): the uplink container has no DAC_OVERRIDE, so
# a file chowned to 10001 with 0400 would be unreadable to its root
# entrypoint. The entrypoint re-copies it into DATA_DIR as the uplink user.
write_owned_file() {
  _dst="$1"; _mode="$2"
  rm -f "$_dst.tmp"
  cat > "$_dst.tmp"
  chmod "$_mode" "$_dst.tmp"
  mv -f "$_dst.tmp" "$_dst"
}

write_allowance_file() {
  umask 077
  printf '{"wei":"%s","source":"%s","warning":"%s"}\n' \
    "$ALLOWANCE_WEI" "$ALLOWANCE_SOURCE" "$(json_escape "$ALLOWANCE_WARNING")" \
    | write_owned_file "$ALLOWANCE_FILE" 0444
  echo "router-user-init: wrote allowance file $ALLOWANCE_FILE (owner uid $(id -u), mode 0444, wei=$ALLOWANCE_WEI source=$ALLOWANCE_SOURCE)"
}

PASS=$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
if [ "${#PASS}" -lt 32 ]; then
  echo "router-user-init: failed to generate password" >&2
  exit 1
fi

PERMS='["chat","embeddings","open_session","close_session","get_sessions","get_wallet","get_balance","get_supply","get_budget","get_stakes_on_hold","withdraw_user_stakes"]'

echo "router-user-init: delete existing user (ignore errors)..."
curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" -X DELETE \
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
curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" -X POST \
  -H 'Content-Type: application/json' \
  --data-binary @"$CONF_FILE" \
  "$ROUTER_URL/auth/users/confirm" >/dev/null

umask 077
# Shared dir must be traversable by the uplink entrypoint (root without
# DAC_READ_SEARCH); files inside stay 0400/0444.
chmod 0755 "$(dirname "$AUTH_OUT")" 2>/dev/null || true
printf '%s:%s\n' "$USER_NAME" "$PASS" | write_owned_file "$AUTH_OUT" 0400
write_allowance_file
echo "router-user-init: wrote ROUTER_AUTH file $AUTH_OUT (owner uid $(id -u), mode 0400); done"
