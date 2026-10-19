#!/usr/bin/env bash
# Integration: router-user-init.sh against a mock router (python http.server).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT_PATH="$ROOT/deploy/init/router-user-init.sh"
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"; kill ${MOCK_PID:-} 2>/dev/null || true' EXIT

PORT=18765
# Prefer a free port
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')

MOCK="$ROOT/scripts/testdata/e2e_mock_router.py"
start_mock() {
  kill ${MOCK_PID:-} 2>/dev/null || true
  COOKIE_CONTENT="admin:testpass" MOCK_BIND=127.0.0.1 python3 "$MOCK" "$PORT" 2>/dev/null &
  MOCK_PID=$!
  for _ in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:$PORT/healthcheck" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  echo "mock router did not start" >&2; return 1
}

# check_outputs DIR BOOT: new credential, wallet file, ready marker (boot id,
# generation, hashes match), modes, no leftover temp files, no secrets in log.
check_outputs() {
  local out="$1" boot="$2"
  if grep -q 'stale-previous-boot' "$out/router.auth"; then
    echo "stale credential survived" >&2; return 1
  fi
  [ "$(stat -c %a "$out/router.auth")" = 400 ] || { echo "auth mode" >&2; return 1; }
  [ "$(stat -c %a "$out/router.mor-allowance.json")" = 444 ] || { echo "allowance mode" >&2; return 1; }
  [ "$(stat -c %a "$out/router.wallet.json")" = 444 ] || { echo "wallet mode" >&2; return 1; }
  [ "$(stat -c %a "$out/router.ready")" = 400 ] || { echo "marker mode" >&2; return 1; }
  grep -q '"address":"0xabababababababababababababababababababab"' "$out/router.wallet.json" || { echo "wallet file" >&2; return 1; }
  grep -qx "boot_id=$boot" "$out/router.ready" || { echo "marker boot id" >&2; return 1; }
  grep -qE '^generation=[0-9a-f]{32}$' "$out/router.ready" || { echo "marker generation" >&2; return 1; }
  for f in auth:router.auth allowance:router.mor-allowance.json wallet:router.wallet.json; do
    k=${f%%:*}; file=${f#*:}
    want=$(sed -n "s/^${k}_sha256=//p" "$out/router.ready")
    got=$(sha256sum "$out/$file" | cut -d' ' -f1)
    [ "$want" = "$got" ] || { echo "marker hash mismatch for $file" >&2; return 1; }
  done
  if ls -A "$out" | grep -q '^\.router-init\.'; then
    echo "temp file left behind" >&2; return 1
  fi
  pass=$(cut -d: -f2- "$out/router.auth")
  if grep -qF "$pass" "$out.log" || grep -qF 'testpass' "$out.log"; then
    echo "secret in init log" >&2; return 1
  fi
}

run_case() {
  local name="$1" mode="$2" mor="$3" override="$4" want_wei="$5" want_source="$6" want_warn="${7:-}"
  echo "== case $name =="
  MOCK_MODE="$mode" MOCK_MOR="$mor" MOCK_BODY="${MOCK_BODY:-}" start_mock
  local out="$WORKDIR/$name"
  mkdir -p "$out"
  # Stale files from a "previous boot" must be replaced, never reused.
  printf 'uplink:stale-previous-boot\n' > "$out/router.auth"
  printf 'boot_id=old\ngeneration=old\n' > "$out/router.ready"
  : > "$out/.router-init.leftover"
  printf 'boot-%s\n' "$name" > "$WORKDIR/boot_id"
  env -i PATH="$PATH" HOME="$HOME" \
    ROUTER_URL="http://127.0.0.1:$PORT" \
    ROUTER_AUTH_OUT="$out/router.auth" \
    UPLINK_MOR_ALLOWANCE_FILE="$out/router.mor-allowance.json" \
    UPLINK_BOOT_ID_FILE="$WORKDIR/boot_id" \
    COOKIE_CONTENT="admin:testpass" \
    UPLINK_MOR_ALLOWANCE_WEI="$override" \
    sh "$SCRIPT_PATH" > "$out.log" 2>&1 || { cat "$out.log"; return 1; }
  test -f "$out/router.auth"
  test -f "$out/router.mor-allowance.json"
  check_outputs "$out" "boot-$name"
  python3 - "$out/router.mor-allowance.json" "$want_wei" "$want_source" "$want_warn" <<'PY'
import json, sys
meta = json.load(open(sys.argv[1]))
if sys.argv[4]:
    assert sys.argv[4] in meta.get("warning", ""), meta
assert meta["wei"] == sys.argv[2], meta
assert meta["source"] == sys.argv[3], meta
if meta["source"] == "zero":
    assert meta.get("warning"), meta
print("OK", meta)
PY
  kill "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=
}

run_case balance balance 9000000000000000000 "" 9000000000000000000 balance
run_case override balance 9000000000000000000 1000000000000000000 1000000000000000000 override
run_case zero balance 0 "" 0 zero
run_case failread fail 0 "" 0 zero
# Explicit 0 must cap at 0, never fall through to wallet balance.
run_case zero_override balance 9000000000000000000 "0" 0 zero "UPLINK_MOR_ALLOWANCE_WEI=0"
# Explicit 0 must not read balance (read failure is irrelevant).
run_case zero_override_noread fail 0 "0" 0 zero "UPLINK_MOR_ALLOWANCE_WEI=0"
# Leading zeros normalized; still explicit override.
run_case padded_override balance 9000000000000000000 "000123" 123 override
run_case padded_zero balance 9000000000000000000 "000" 0 zero
# Surrounding whitespace trimmed; still an override.
run_case trimmed_override balance 9000000000000000000 "  123 " 123 override
# Balance parsing: bare integer, pretty-printed multi-line, leading zeros.
MOCK_BODY='{"eth":"0","mor":7000}' run_case bal_number balance 0 "" 7000 balance
MOCK_BODY=$'{\n  "eth": "1",\n  "mor": "0005000"\n}' run_case bal_multiline balance 0 "" 5000 balance
# Ambiguous / malformed balance must fail closed to 0 (never a wrong cap).
MOCK_BODY='{"mor":"5","nested":{"mor":"9"}}' run_case bal_duplicate balance 0 "" 0 zero "unparseable"
MOCK_BODY='{"mor":1.5e18,"eth":"0"}' run_case bal_float balance 0 "" 0 zero "unparseable"
MOCK_BODY='{"mor":"-5"}' run_case bal_negative balance 0 "" 0 zero "unparseable"
MOCK_BODY='{"amor":"5","eth":"0"}' run_case bal_missing balance 0 "" 0 zero "unparseable"

# Invalid overrides must exit non-zero and write nothing.
run_fail_case() {
  local name="$1" override="$2"
  echo "== fail case $name =="
  MOCK_MODE=balance start_mock
  local out="$WORKDIR/$name"
  mkdir -p "$out"
  printf 'uplink:stale-previous-boot\n' > "$out/router.auth"
  printf 'generation=old\n' > "$out/router.ready"
  if env -i PATH="$PATH" HOME="$HOME" \
    ROUTER_URL="http://127.0.0.1:$PORT" \
    ROUTER_AUTH_OUT="$out/router.auth" \
    UPLINK_MOR_ALLOWANCE_FILE="$out/router.mor-allowance.json" \
    COOKIE_CONTENT="admin:testpass" \
    UPLINK_MOR_ALLOWANCE_WEI="$override" \
    sh "$SCRIPT_PATH"; then
    echo "expected failure for override=$override" >&2
    exit 1
  fi
  # Old files are removed at start even when this run then fails, so a
  # reader can never pick up the previous boot's credential.
  test ! -e "$out/router.auth"
  test ! -e "$out/router.ready"
  test ! -e "$out/router.mor-allowance.json"
  echo "OK rejected override=$override"
  kill "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=
}
run_fail_case internal_space "1 000"
run_fail_case internal_tab $'12\t34'
run_fail_case alpha "abc"
run_fail_case exponent "1e18"
run_fail_case negative "-5"
run_fail_case hex "0x10"
# Private staging dir (no-sidecar entrypoint path) is never opened to 0755.
echo "== case staging_dir_mode =="
MOCK_MODE=balance start_mock
stage="$WORKDIR/stage"; mkdir -p "$stage"; chmod 0700 "$stage"
env -i PATH="$PATH" HOME="$HOME" ROUTER_URL="http://127.0.0.1:$PORT" \
  ROUTER_AUTH_OUT="$stage/router.auth" ROUTER_AUTH_DIR_MODE=0700 \
  COOKIE_CONTENT="admin:testpass" UPLINK_MOR_ALLOWANCE_WEI=5 sh "$SCRIPT_PATH" >/dev/null
[ "$(stat -c %a "$stage")" = 700 ] || { echo "staging dir opened: $(stat -c %a "$stage")" >&2; exit 1; }
kill "$MOCK_PID" 2>/dev/null || true; MOCK_PID=
echo "OK staging dir stays 0700"

# Idempotent re-run: a credential the router still accepts is kept and only
# the allowance changes (in place, read back); a credential the router no
# longer knows is replaced.
echo "== case rerun_keeps_valid_credential =="
MOCK_MODE=balance MOCK_MOR=0 start_mock
rr="$WORKDIR/rerun"; mkdir -p "$rr"
run_init() {
  env -i PATH="$PATH" HOME="$HOME" ROUTER_URL="http://127.0.0.1:$PORT" \
    ROUTER_AUTH_OUT="$rr/router.auth" COOKIE_CONTENT="admin:testpass" \
    UPLINK_MOR_ALLOWANCE_WEI="$1" sh "$SCRIPT_PATH" > "$rr.log" 2>&1 || { cat "$rr.log"; exit 1; }
}
run_init 0
p1=$(cut -d: -f2- "$rr/router.auth")
run_init 777
p2=$(cut -d: -f2- "$rr/router.auth")
[ "$p1" = "$p2" ] || { echo "valid credential was rotated" >&2; exit 1; }
grep -q 'password kept' "$rr.log" || { echo "expected in-place path" >&2; exit 1; }
got=$(curl -fsS "http://127.0.0.1:$PORT/e2e/stats" | jq -r '.users.uplink.allowances["0x7431ada8a591c955a994a21710752ef9b882b8e3"]')
[ "$got" = 777 ] || { echo "allowance not updated in place: $got" >&2; exit 1; }
grep -q '"wei":"777","source":"override"' "$rr/router.mor-allowance.json" || { echo "allowance file" >&2; exit 1; }
curl -fsS -X POST "http://127.0.0.1:$PORT/e2e/forget-users" >/dev/null
run_init 5
p3=$(cut -d: -f2- "$rr/router.auth")
[ "$p3" != "$p2" ] || { echo "unknown credential was kept" >&2; exit 1; }
if grep -qF "$p3" "$rr.log" || grep -qF testpass "$rr.log"; then echo "secret in log" >&2; exit 1; fi
kill "$MOCK_PID" 2>/dev/null || true; MOCK_PID=
echo "OK re-run keeps a valid credential, replaces an unknown one"
echo "OK: router-user-init integration cases"
