#!/bin/sh
# Shared helpers for router-user-init.sh and router-allowance-manager.sh.
# Sourced, never executed. POSIX sh + curl + jq. Never prints credentials or
# response bodies.
#
# Credentials reach curl only through a config on stdin (-K -), never argv,
# and no netrc/curlrc file is ever written.

# curl_with_user USERPASS ARGS...: run curl authenticated as USERPASS.
curl_with_user() {
  _cu="$1"; shift
  printf 'user = "%s"\n' "$(printf '%s' "$_cu" | sed 's/\\/\\\\/g; s/"/\\"/g')" \
    | curl -K - "$@"
}

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

# digits_or_empty VALUE: VALUE normalized when it is all digits, else empty.
digits_or_empty() {
  case "$1" in
    ''|*[!0-9]*) return 0 ;;
  esac
  _d=$(printf '%s' "$1" | sed 's/^0*//')
  [ -n "$_d" ] || _d=0
  printf '%s' "$_d"
}

# router_user_allowance ADMIN URL USER TOKEN -> current allowance (digits) or
# empty when the user/token is missing or the read failed.
router_user_allowance() {
  _users=$(curl_with_user "$1" -fsS "$2/auth/users" 2>/dev/null) || return 0
  _a=$(printf '%s' "$_users" | jq -r --arg u "$3" --arg t "$4" \
    '[.agents[]? | select(.username == $u)] | if length == 1 then (.[0].allowances[$t] // empty) else empty end' 2>/dev/null || true)
  digits_or_empty "$_a"
}

# set_allowance_inplace ADMIN URL USER TOKEN WEI: file an allowance request
# for USER and confirm it (absolute value; password unchanged), then read it
# back. Returns 0 only when the read-back equals WEI. Sets READBACK_WEI.
set_allowance_inplace() {
  READBACK_WEI=""
  _body=$(jq -cn --arg u "$3" --arg t "$4" --arg a "$5" '{username:$u, token:$t, allowance:$a}')
  # Body carries no secret (user name, token, amount); stdin is taken by -K.
  curl_with_user "$1" -fsS -X POST -H 'Content-Type: application/json' \
    --data-binary "$_body" "$2/auth/allowance/requests" >/dev/null 2>&1 || return 1
  _body=$(jq -cn --arg u "$3" --arg t "$4" '{username:$u, token:$t, confirm:true}')
  curl_with_user "$1" -fsS -X POST -H 'Content-Type: application/json' \
    --data-binary "$_body" "$2/auth/allowance/confirm" >/dev/null 2>&1 || return 1
  READBACK_WEI=$(router_user_allowance "$1" "$2" "$3" "$4")
  [ -n "$READBACK_WEI" ] && [ "$READBACK_WEI" = "$5" ]
}

# scoped_credential_works URL USERPASS: the router accepts USERPASS
# (authenticated read the scoped user is allowed to make).
scoped_credential_works() {
  curl_with_user "$2" -fsS -o /dev/null "$1/wallet" 2>/dev/null
}
