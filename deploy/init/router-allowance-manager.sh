#!/bin/sh
# router-allowance-manager: keeps the scoped router user's MOR allowance equal
# to the live wallet MOR balance in balance mode (UPLINK_MOR_ALLOWANCE_WEI
# empty). Long-lived, non-root (10002), no capabilities, read-only rootfs,
# internal router network only. It only ever changes the allowance (in place,
# absolute value, read back); it never touches the password.
#
# Inputs:
#   /mgr/admin.cookie   admin credential (0400, read once; re-read once on 401)
#   /mgr/mode           written by router-user-init; must be exactly "balance"
#   /ctl/resync.request presence-only flag (never opened/read/listed)
# Output:
#   /status/allowance-status.json (0444, no secrets)
#
# Fail closed (A1): any non-empty UPLINK_MOR_ALLOWANCE_WEI, or a mode marker
# that is missing/unreadable/not exactly "balance", means no write, ever.
# Never logs credentials or response bodies.
set -u
ulimit -c 0 2>/dev/null || true
umask 077
# shellcheck source=router-admin-lib.sh
. "${ROUTER_ADMIN_LIB:-$(dirname "$0")/router-admin-lib.sh}"

ROUTER_URL="${ROUTER_URL:-http://proxy-router:8082}"
USER_NAME="${UPLINK_ROUTER_USER:-uplink}"
MOR_TOKEN=$(printf '%s' "${MOR_TOKEN_ADDRESS:-0x7431ada8a591c955a994a21710752ef9b882b8e3}" | tr 'A-F' 'a-f')
MGR_DIR="${ROUTER_MANAGER_CRED_DIR:-/mgr}"
STATUS_DIR="${ROUTER_ALLOWANCE_STATUS_DIR:-/status}"
CTL_DIR="${ROUTER_ALLOWANCE_CTL_DIR:-/ctl}"
STATUS_FILE="$STATUS_DIR/allowance-status.json"
FLAG="$CTL_DIR/resync.request"
POLL="${ALLOWANCE_POLL_SECONDS:-60}"
FLAG_MIN="${ALLOWANCE_FLAG_MIN_SECONDS:-60}"
WRITE_MIN="${ALLOWANCE_WRITE_MIN_SECONDS:-30}"
BACKOFF_MAX="${ALLOWANCE_BACKOFF_MAX_SECONDS:-600}"
MAX_TICKS="${ALLOWANCE_MAX_TICKS:-0}" # tests only; 0 = forever

if [ "${1:-}" = "--health" ]; then
  # Healthy while the heartbeat (status file) is fresher than 3 polls.
  [ -f "$STATUS_FILE" ] || exit 1
  now=$(date +%s)
  mt=$(stat -c %Y "$STATUS_FILE" 2>/dev/null || echo 0)
  [ $((now - mt)) -lt $((POLL * 3)) ]
  exit $?
fi

now_s() { date +%s; }
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ; }

audit() {
  # audit EVENT JSON-FIELDS (fields are numbers/fixed words only)
  printf '{"ts":"%s","event":"%s",%s}\n' "$(iso "$(now_s)")" "$1" "$2"
}

MODE="balance"
SOURCE="balance"
WEI=""
BAL=""
LAST_RESYNC_AT=""
LAST_RESULT="none"
LAST_REASON=""

write_status() {
  _tmp=$(mktemp "$STATUS_DIR/.allowance-status.XXXXXX") || return 0
  jq -cn --arg mode "$MODE" --arg source "$SOURCE" --arg wei "$WEI" --arg bal "$BAL" \
    --arg lra "$LAST_RESYNC_AT" --arg lr "$LAST_RESULT" --arg why "$LAST_REASON" \
    --arg hb "$(iso "$(now_s)")" \
    '{mode:$mode, source:$source, wei:$wei, balanceWei:$bal, lastResyncAt:$lra, lastResult:$lr, lastReason:$why, heartbeatAt:$hb}' \
    > "$_tmp" && chmod 0444 "$_tmp" && mv -f "$_tmp" "$STATUS_FILE" || rm -f "$_tmp"
}

skip() {
  LAST_REASON="$1"
  [ "$1" = "unchanged" ] || audit allowance_resync_skip "\"reason\":\"$1\""
}

# Gate 1: env. Any non-empty value (even "0", spaces, garbage) is cap mode.
if [ -n "${UPLINK_MOR_ALLOWANCE_WEI+x}" ] && [ -n "${UPLINK_MOR_ALLOWANCE_WEI}" ]; then
  GATE="cap_mode"
else
  GATE=""
fi

COOKIE=""
load_cookie() {
  COOKIE=""
  [ -f "$MGR_DIR/admin.cookie" ] && [ ! -L "$MGR_DIR/admin.cookie" ] || return 1
  COOKIE=$(head -c 4096 "$MGR_DIR/admin.cookie" 2>/dev/null) || { COOKIE=""; return 1; }
  [ -n "$COOKIE" ]
}

# get_json PATH -> sets RESP_CODE and RESP_BODY (body never logged).
get_json() {
  _o=$(curl_with_user "$COOKIE" -sS -m 20 -w '\n%{http_code}' "$ROUTER_URL$1" 2>/dev/null) || _o="
000"
  RESP_CODE=$(printf '%s' "$_o" | tail -n 1)
  RESP_BODY=$(printf '%s' "$_o" | sed '$d')
}

read_balance() {
  BAL_READ=""
  get_json /blockchain/balance
  [ "$RESP_CODE" = "200" ] || return 1
  BAL_READ=$(parse_mor_balance "$RESP_BODY")
  [ -n "$BAL_READ" ]
}

backoff=0
backoff_until=0
diffs=0
last_write=0
last_flag=0
cookie_retry=0
ticks=0
dir_flag_logged=0
load_cookie || true

while :; do
  t=$(now_s)
  ticks=$((ticks + 1))
  flag=0
  if [ -e "$FLAG" ] || [ -L "$FLAG" ]; then flag=1; fi

  marker=""
  if [ -f "$MGR_DIR/mode" ] && [ ! -L "$MGR_DIR/mode" ]; then
    marker=$(head -c 16 "$MGR_DIR/mode" 2>/dev/null || true)
  fi

  if [ -n "$GATE" ]; then
    MODE="cap"; SOURCE="override"; skip cap_mode; LAST_RESULT="skipped"
    # A request in cap mode is acknowledged (removed) and ignored.
    [ "$flag" = 1 ] && rm -f "$FLAG" 2>/dev/null
  elif [ "$marker" != "balance" ]; then
    MODE="${marker:-unknown}"; [ "$MODE" = "balance" ] || MODE="cap"
    [ "$marker" = "ceiling" ] && MODE="ceiling"
    SOURCE="$MODE"; skip marker; LAST_RESULT="skipped"
    [ "$flag" = 1 ] && rm -f "$FLAG" 2>/dev/null
  elif [ "$t" -lt "$backoff_until" ]; then
    MODE="balance"; skip backoff
  elif [ -z "$COOKIE" ] && ! load_cookie; then
    MODE="balance"; skip read_error; LAST_RESULT="error"
  else
    MODE="balance"; SOURCE="balance"
    trigger="poll"
    use_flag=0
    if [ "$flag" = 1 ]; then
      if [ $((t - last_flag)) -ge "$FLAG_MIN" ] || [ "$last_flag" = 0 ]; then
        if rm -f "$FLAG" 2>/dev/null && ! { [ -e "$FLAG" ] || [ -L "$FLAG" ]; }; then
          use_flag=1; trigger="flag"; last_flag=$t
        elif [ "$dir_flag_logged" = 0 ]; then
          audit allowance_resync_skip '"reason":"flag_unremovable"'
          dir_flag_logged=1
        fi
      fi
    fi
    if ! read_balance; then
      if [ "$RESP_CODE" = "401" ] && [ "$cookie_retry" = 0 ]; then
        cookie_retry=1; load_cookie || true
      fi
      skip read_error; LAST_RESULT="error"
      backoff=$(( backoff == 0 ? POLL : backoff * 2 )); [ "$backoff" -gt "$BACKOFF_MAX" ] && backoff=$BACKOFF_MAX
      backoff_until=$((t + backoff))
    else
      cookie_retry=0
      BAL="$BAL_READ"
      cur=$(router_user_allowance "$COOKIE" "$ROUTER_URL" "$USER_NAME" "$MOR_TOKEN")
      if [ -z "$cur" ]; then
        skip read_error; LAST_RESULT="error"
        backoff=$(( backoff == 0 ? POLL : backoff * 2 )); [ "$backoff" -gt "$BACKOFF_MAX" ] && backoff=$BACKOFF_MAX
        backoff_until=$((t + backoff))
      elif [ "$BAL" = "0" ]; then
        WEI="$cur"; skip zero_balance
      else
        WEI="$cur"
        if [ "$cur" != "$BAL" ]; then diffs=$((diffs + 1)); else diffs=0; fi
        if [ "$use_flag" = 0 ] && [ "$diffs" -lt 2 ]; then
          skip unchanged
        elif [ "$cur" = "$BAL" ]; then
          skip unchanged; LAST_RESULT="ok"
        elif [ $((t - last_write)) -lt "$WRITE_MIN" ] && [ "$last_write" != 0 ]; then
          skip debounce
        elif ! read_balance || [ "$BAL_READ" != "$BAL" ]; then
          skip balance_moved
        else
          last_write=$t
          if set_allowance_inplace "$COOKIE" "$ROUTER_URL" "$USER_NAME" "$MOR_TOKEN" "$BAL"; then
            audit allowance_resync "\"trigger\":\"$trigger\",\"from_wei\":\"$cur\",\"to_wei\":\"$BAL\",\"balance_wei\":\"$BAL\",\"result\":\"ok\",\"readback\":\"$READBACK_WEI\""
            WEI="$READBACK_WEI"; LAST_RESULT="ok"; LAST_REASON="$trigger"; LAST_RESYNC_AT=$(iso "$t")
            diffs=0; backoff=0
          else
            if [ -n "$READBACK_WEI" ]; then
              audit allowance_resync_mismatch "\"expected_wei\":\"$BAL\",\"actual_wei\":\"$READBACK_WEI\""
            fi
            audit allowance_resync "\"trigger\":\"$trigger\",\"from_wei\":\"$cur\",\"to_wei\":\"$BAL\",\"balance_wei\":\"$BAL\",\"result\":\"error\",\"readback\":\"${READBACK_WEI:-}\""
            LAST_RESULT="error"; LAST_REASON="readback"
            backoff=$(( backoff == 0 ? POLL : backoff * 2 )); [ "$backoff" -gt "$BACKOFF_MAX" ] && backoff=$BACKOFF_MAX
            backoff_until=$((t + backoff))
          fi
        fi
      fi
    fi
  fi
  write_status
  if [ "$MAX_TICKS" != 0 ] && [ "$ticks" -ge "$MAX_TICKS" ]; then
    exit 0
  fi
  sleep "$POLL"
done
