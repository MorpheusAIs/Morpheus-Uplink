#!/bin/sh
# Create scoped router user "uplink" with MOR allowance. Idempotent.
# Writes uplink:<password> to ROUTER_AUTH_OUT (mode 0400). Never logs the password.
set -eu

ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
AUTH_OUT="${ROUTER_AUTH_OUT:-/shared/router.auth}"
USER_NAME="${UPLINK_ROUTER_USER:-uplink}"
ALLOWANCE_WEI="${UPLINK_MOR_ALLOWANCE_WEI:?UPLINK_MOR_ALLOWANCE_WEI required}"
MOR_TOKEN="${MOR_TOKEN_ADDRESS:-0x7431ada8a591c955a994a21710752ef9b882b8e3}"
# Lowercase token address (router allowance lookup is case-sensitive to lower).
MOR_TOKEN=$(printf '%s' "$MOR_TOKEN" | tr 'A-F' 'a-f')

if [ -z "$ALLOWANCE_WEI" ] || [ "$ALLOWANCE_WEI" = "0" ]; then
  echo "router-user-init: UPLINK_MOR_ALLOWANCE_WEI missing or zero" >&2
  exit 1
fi

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
# Build JSON without echoing password to logs - use env for curl data via printf to file
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
echo "router-user-init: wrote ROUTER_AUTH file (mode 0400, uid 10001); done"
