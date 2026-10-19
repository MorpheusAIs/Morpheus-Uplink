#!/bin/sh
# Create scoped router user "uplink" with MOR allowance. Idempotent.
# Writes uplink:<password> to ROUTER_AUTH_OUT (mode 0400). Never logs the password.
# Allowance: UPLINK_MOR_ALLOWANCE_WEI override if >0, else wallet balance at boot.
# Writes allowance metadata beside the auth file (mode 0444, uid 10001).
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

# Resolve ALLOWANCE_WEI: manual override (>0) wins; else wallet balance; else "0".
ALLOWANCE_WEI=""
ALLOWANCE_SOURCE=""
ALLOWANCE_WARNING=""

OVERRIDE="${UPLINK_MOR_ALLOWANCE_WEI:-}"
OVERRIDE=$(printf '%s' "$OVERRIDE" | tr -d '[:space:]')
if [ -n "$OVERRIDE" ] && [ "$OVERRIDE" != "0" ]; then
  case "$OVERRIDE" in
    *[!0-9]*)
      echo "router-user-init: UPLINK_MOR_ALLOWANCE_WEI must be a non-negative integer (wei)" >&2
      exit 1
      ;;
  esac
  ALLOWANCE_WEI="$OVERRIDE"
  ALLOWANCE_SOURCE="override"
  echo "router-user-init: using UPLINK_MOR_ALLOWANCE_WEI override"
else
  BAL_JSON=""
  if BAL_JSON=$(curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" "$ROUTER_URL/blockchain/balance" 2>/dev/null); then
    # Parse JSON "mor" field (string or number). Keep product key lowercase.
    ALLOWANCE_WEI=$(printf '%s' "$BAL_JSON" | sed -n 's/.*"mor"[[:space:]]*:[[:space:]]*"\([0-9][0-9]*\)".*/\1/p' | head -n1)
    if [ -z "$ALLOWANCE_WEI" ]; then
      ALLOWANCE_WEI=$(printf '%s' "$BAL_JSON" | sed -n 's/.*"mor"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n1)
    fi
    if [ -z "$ALLOWANCE_WEI" ]; then
      ALLOWANCE_WEI="0"
      ALLOWANCE_SOURCE="zero"
      ALLOWANCE_WARNING="MOR balance field missing from router response; sessions will not open until funded and Uplink is restarted."
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

write_allowance_file() {
  umask 022
  printf '{"wei":"%s","source":"%s","warning":"%s"}\n' \
    "$ALLOWANCE_WEI" "$ALLOWANCE_SOURCE" "$(json_escape "$ALLOWANCE_WARNING")" > "$ALLOWANCE_FILE"
  chown 10001:10001 "$ALLOWANCE_FILE" 2>/dev/null || true
  chmod 0444 "$ALLOWANCE_FILE"
  echo "router-user-init: wrote allowance file $ALLOWANCE_FILE (source=$ALLOWANCE_SOURCE)"
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
printf '%s:%s\n' "$USER_NAME" "$PASS" > "$AUTH_OUT"
chown 10001:10001 "$AUTH_OUT" 2>/dev/null || true
chmod 0400 "$AUTH_OUT"
write_allowance_file
echo "router-user-init: wrote ROUTER_AUTH file (mode 0400, uid 10001); done"
