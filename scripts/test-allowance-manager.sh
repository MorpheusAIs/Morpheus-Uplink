#!/usr/bin/env bash
# Integration: router-allowance-manager.sh against the stateful mock router.
# Covers the A1 fail-closed matrix (test 3), flag hostility (test 4), secret
# hygiene in logs/argv (part of test 5) and failure handling (test 7).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MGR="$ROOT/deploy/init/router-allowance-manager.sh"
INIT="$ROOT/deploy/init/router-user-init.sh"
MOCK="$ROOT/scripts/testdata/e2e_mock_router.py"
W=$(mktemp -d)
trap 'kill ${MOCK_PID:-} 2>/dev/null || true; rm -rf "$W"' EXIT
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
URL="http://127.0.0.1:$PORT"
TOKEN=0x7431ada8a591c955a994a21710752ef9b882b8e3
COOKIE="admin:mgr-test-$(openssl rand -hex 8)"

start_mock() {
  kill ${MOCK_PID:-} 2>/dev/null || true
  COOKIE_CONTENT="$COOKIE" MOCK_BIND=127.0.0.1 MOCK_MOR="${1:-5000}" python3 "$MOCK" "$PORT" 2>"$W/mock.log" &
  MOCK_PID=$!
  for _ in $(seq 1 50); do curl -fsS "$URL/healthcheck" >/dev/null 2>&1 && return 0; sleep 0.1; done
  echo "mock did not start" >&2; exit 1
}
stats() { curl -fsS "$URL/e2e/stats"; }
allowance() { stats | jq -r --arg t "$TOKEN" '.users.uplink.allowances[$t] // empty'; }
confirms() { stats | jq -r '.allowance_confirms'; }

# mint DIR WEI: run init (non-root: manager dirs are plain tmp dirs here).
mint() {
  local d="$1" wei="$2"
  mkdir -p "$d/shared" "$d/mgr" "$d/status" "$d/ctl"
  env -i PATH="$PATH" HOME="$HOME" ROUTER_URL="$URL" COOKIE_CONTENT="$COOKIE" \
    ROUTER_AUTH_OUT="$d/shared/router.auth" ROUTER_MANAGER_CRED_DIR="$d/mgr" \
    ROUTER_ALLOWANCE_STATUS_DIR="$d/status" ROUTER_ALLOWANCE_CTL_DIR="$d/ctl" \
    ROUTER_MANAGER_UID="$(id -u)" ROUTER_MANAGER_GID="$(id -g)" ROUTER_INIT_ROOT_OWNER="$(id -u):$(id -g)" UPLINK_UID="$(id -u)" ROUTER_ALLOWANCE_CTL_GID="$(id -g)" \
    UPLINK_MOR_ALLOWANCE_WEI="$wei" sh "$INIT" > "$d/init.log" 2>&1 || { cat "$d/init.log"; exit 1; }
}

# manage DIR TICKS [ENV...]: run the manager for TICKS polls of 1s.
manage() {
  local d="$1" ticks="$2"; shift 2
  env -i PATH="$PATH" HOME="$HOME" ROUTER_URL="$URL" \
    ROUTER_MANAGER_CRED_DIR="$d/mgr" ROUTER_ALLOWANCE_STATUS_DIR="$d/status" ROUTER_ALLOWANCE_CTL_DIR="$d/ctl" \
    ALLOWANCE_POLL_SECONDS=1 ALLOWANCE_WRITE_MIN_SECONDS=0 ALLOWANCE_FLAG_MIN_SECONDS="${FLAG_MIN:-60}" \
    ALLOWANCE_BACKOFF_MAX_SECONDS=2 ALLOWANCE_MAX_TICKS="$ticks" "$@" \
    timeout 60 sh "$MGR" >> "$d/mgr.log" 2>&1
}

no_secrets_in() {
  local f="$1"
  if grep -qF "${COOKIE#*:}" "$f"; then echo "admin cookie leaked into $f" >&2; exit 1; fi
  local pw; pw=$(cut -d: -f2- "$2/shared/router.auth")
  if grep -qF "$pw" "$f"; then echo "scoped password leaked into $f" >&2; exit 1; fi
}

echo "== T3 A1 fail-closed matrix =="
start_mock 5000
d="$W/a1"; mint "$d" ""
[ "$(cat "$d/mgr/mode")" = "balance" ] || { echo "marker should be balance"; exit 1; }
[ "$(stat -c %a "$d/mgr/admin.cookie")" = 400 ] || { echo "cookie mode"; exit 1; }
[ "$(stat -c %a "$d/mgr/mode")" = 444 ] || { echo "marker mode"; exit 1; }
base=$(confirms)
# Only "env unset/empty + marker exactly balance" may write.
for v in "0" "007" "1e18" " " "abc"; do
  manage "$d" 3 UPLINK_MOR_ALLOWANCE_WEI="$v"
  [ "$(confirms)" = "$base" ] || { echo "A1: wrote with env=[$v]" >&2; exit 1; }
  jq -e '.mode=="cap"' "$d/status/allowance-status.json" >/dev/null || { echo "A1 status mode env=[$v]"; exit 1; }
  echo "OK no write with UPLINK_MOR_ALLOWANCE_WEI=[$v]"
done
cp "$d/mgr/mode" "$W/mode.bak"
for m in absent cap ceiling "balance " "Balance" "" 'balance\n' 'balance\r' 'balance\r\n' 'balance\0' 'bal\0ance'; do
  if [ "$m" = absent ]; then rm -f "$d/mgr/mode"; else printf "$m" > "$d/mgr/mode"; fi
  manage "$d" 3
  [ "$(confirms)" = "$base" ] || { echo "A1: wrote with marker=[$m]" >&2; exit 1; }
  echo "OK no write with marker=[$m]"
done
cp "$W/mode.bak" "$d/mgr/mode"
manage "$d" 3
[ "$(allowance)" = 5000 ] || { echo "A1: balance mode did not resync: $(allowance)" >&2; exit 1; }
echo "OK balance mode (empty env, marker 'balance') resyncs to the balance"
# Cap minted (env 0) => marker cap => manager never writes even with empty env.
d2="$W/a1cap"; mint "$d2" "0"
[ "$(cat "$d2/mgr/mode")" = "cap" ] || { echo "marker should be cap for env 0"; exit 1; }
b2=$(confirms); manage "$d2" 3
[ "$(confirms)" = "$b2" ] || { echo "A1: cap marker + empty env wrote" >&2; exit 1; }
[ "$(allowance)" = 0 ] || { echo "A1: zero cap changed: $(allowance)" >&2; exit 1; }
echo "OK marker cap + empty env: no write, zero cap untouched"
no_secrets_in "$d/mgr.log" "$d"

echo "== T2-lite funding follows within 2 polls, password unchanged =="
d="$W/fund"; mint "$d" ""
pw1=$(cut -d: -f2- "$d/shared/router.auth")
manage "$d" 3
curl -fsS -X POST "$URL/e2e/fund?mor=9000" >/dev/null
manage "$d" 3
[ "$(allowance)" = 9000 ] || { echo "funding not followed: $(allowance)" >&2; exit 1; }
[ "$(stats | jq -r .users.uplink.pw)" = "$(printf '%s' "$pw1" | sha256sum | cut -c1-12)" ] || { echo "password changed" >&2; exit 1; }
jq -e '.lastResult=="ok" and .wei=="9000" and .balanceWei=="9000" and (.heartbeatAt|length>0)' "$d/status/allowance-status.json" >/dev/null
[ "$(stat -c %a "$d/status/allowance-status.json")" = 444 ]
echo "OK allowance follows funding; password unchanged; status file 0444"

echo "== T4 flag hostility =="
curl -fsS -X POST "$URL/e2e/fund?mor=11000" >/dev/null
f="$d/ctl/resync.request"
# FIFO: must not block (manager never opens it) and is removed.
mkfifo "$f"
FLAG_MIN=60 manage "$d" 1
[ ! -e "$f" ] || { echo "FIFO flag not consumed" >&2; exit 1; }
[ "$(allowance)" = 11000 ] || { echo "flag did not trigger resync" >&2; exit 1; }
echo "OK FIFO flag consumed without blocking; value from fresh balance"
# Symlink to a sensitive file and to a directory: link removed, target intact.
echo "do-not-touch" > "$W/target.txt"; chmod 0640 "$W/target.txt"; mkdir -p "$W/targetdir"; touch "$W/targetdir/keep"
before=$(stat -c '%a %s %Y' "$W/target.txt")
ln -s "$W/target.txt" "$f"; manage "$d" 1
[ ! -L "$f" ] && [ "$(stat -c '%a %s %Y' "$W/target.txt")" = "$before" ] || { echo "symlink flag followed" >&2; exit 1; }
ln -s "$W/targetdir" "$f"; manage "$d" 1
[ ! -L "$f" ] && [ -e "$W/targetdir/keep" ] || { echo "dir symlink followed" >&2; exit 1; }
ln -s /etc/passwd "$f"; manage "$d" 1
[ ! -L "$f" ] && [ -s /etc/passwd ] || { echo "passwd symlink" >&2; exit 1; }
echo "OK symlink flags (file, dir, /etc/passwd) unlinked, targets untouched"
# Oversized file: presence only, never read.
head -c 1048576 /dev/zero > "$f"; manage "$d" 1
[ ! -e "$f" ] || { echo "1MB flag not consumed" >&2; exit 1; }
echo "OK 1 MB flag consumed (presence only)"
# A directory named like the flag: cannot be removed; logged once, skipped.
mkdir "$f"; manage "$d" 2
[ "$(grep -c flag_unremovable "$d/mgr.log")" -ge 1 ] || { echo "dir flag not reported" >&2; exit 1; }
rmdir "$f"
echo "OK directory flag reported and skipped"
# 10k junk files: never listed; manager still runs normally.
for i in $(seq 1 10000); do : > "$d/ctl/junk.$i"; done
start=$(date +%s); manage "$d" 1; el=$(( $(date +%s) - start ))
[ "$el" -lt 10 ] || { echo "junk files slowed the manager: ${el}s" >&2; exit 1; }
echo "OK 10k junk files ignored"
# Flood: flags re-created every poll => at most 1 flag-triggered write per FLAG_MIN.
c0=$(confirms)
( for i in $(seq 1 12); do : > "$f" 2>/dev/null; sleep 0.25; done ) &
flooder=$!
# Balance moves each time so every honoured flag would write.
( for i in $(seq 1 6); do curl -fsS -X POST "$URL/e2e/fund?mor=$((20000 + i))" >/dev/null; sleep 0.5; done ) &
funder=$!
FLAG_MIN=60 manage "$d" 3 ALLOWANCE_WRITE_MIN_SECONDS=60
wait "$flooder" "$funder" 2>/dev/null || true
c1=$(confirms)
[ $((c1 - c0)) -le 1 ] || { echo "flood caused $((c1 - c0)) writes (want <= 1)" >&2; exit 1; }
rm -f "$f"
echo "OK flag flood: $((c1 - c0)) write(s) in the window (<= 1)"
no_secrets_in "$d/mgr.log" "$d"

echo "== T7 failure handling =="
d="$W/fail"; mint "$d" ""
curl -fsS -X POST "$URL/e2e/fund?mor=7000" >/dev/null
# Balance read failure => skip, never 0.
curl -fsS -X POST "$URL/e2e/fail?path=/blockchain/balance&code=500" >/dev/null
before=$(allowance); manage "$d" 3
[ "$(allowance)" = "$before" ] || { echo "write despite balance failure" >&2; exit 1; }
grep -q '"reason":"read_error"' "$d/mgr.log" && grep -q '"reason":"backoff"' "$d/mgr.log" || { echo "expected read_error + backoff" >&2; exit 1; }
curl -fsS -X POST "$URL/e2e/fail?path=/blockchain/balance&code=0" >/dev/null
echo "OK balance 5xx: skip + backoff, allowance unchanged"
# Router 401 => backoff, no hammering.
curl -fsS -X POST "$URL/e2e/reset-stats" >/dev/null
printf 'admin:wrong' > "$d/mgr/admin.cookie.tmp"; mv "$d/mgr/admin.cookie" "$W/cookie.bak"; mv "$d/mgr/admin.cookie.tmp" "$d/mgr/admin.cookie"
: > "$d/mgr.log"; manage "$d" 6
n401=$(stats | jq -r .unauthorized)
[ "$n401" -le 4 ] || { echo "hammering on 401: $n401 rejected calls in 6 polls" >&2; exit 1; }
mv "$W/cookie.bak" "$d/mgr/admin.cookie"
echo "OK router 401: backoff ($n401 rejected calls over 6 polls)"
# Read-back mismatch => error, audit, backoff.
curl -fsS -X POST "$URL/e2e/fund?mor=8100" >/dev/null
curl -fsS -X POST "$URL/e2e/readback-skew?on=1" >/dev/null
: > "$d/mgr.log"; manage "$d" 5
grep -q '"event":"allowance_resync_mismatch"' "$d/mgr.log" || { echo "no mismatch audit" >&2; exit 1; }
jq -e '.lastResult=="error"' "$d/status/allowance-status.json" >/dev/null || { echo "status not error" >&2; exit 1; }
grep -q '"reason":"backoff"' "$d/mgr.log" || { echo "no backoff after mismatch" >&2; exit 1; }
curl -fsS -X POST "$URL/e2e/readback-skew?on=0" >/dev/null
echo "OK read-back mismatch: error + audit + backoff"
# Zero balance => skip, never write 0.
curl -fsS -X POST "$URL/e2e/fund?mor=0" >/dev/null
before=$(allowance); : > "$d/mgr.log"; manage "$d" 3
[ "$(allowance)" = "$before" ] && grep -q zero_balance "$d/mgr.log" || { echo "zero balance handling" >&2; exit 1; }
echo "OK zero balance: skip"
no_secrets_in "$d/mgr.log" "$d"

echo "== manager --health =="
ROUTER_ALLOWANCE_STATUS_DIR="$d/status" ALLOWANCE_POLL_SECONDS=60 sh "$MGR" --health || { echo "fresh heartbeat reported unhealthy" >&2; exit 1; }
touch -d '-10 minutes' "$d/status/allowance-status.json"
if ROUTER_ALLOWANCE_STATUS_DIR="$d/status" ALLOWANCE_POLL_SECONDS=60 sh "$MGR" --health; then echo "stale heartbeat reported healthy" >&2; exit 1; fi
echo "OK health follows heartbeat"
echo "OK: allowance manager integration cases"
