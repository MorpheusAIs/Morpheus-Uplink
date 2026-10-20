#!/usr/bin/env bash
# E2E: SecretVM-shaped stack with a stateful mock router (real credential
# checks; the router's own COOKIE_CONTENT is Uplink's credential). Uses
# deploy/secretvm/docker-compose.yml unchanged (caps, volumes, networks,
# depends_on) plus scripts/testdata/e2e-mock-router-override.yml.
#
#  boot          uplink healthy, runs as uid 10001, reaches the router with
#                the router cookie, /admin/status reports routerAuth ok.
#  sessions      SIGKILL with open sessions, restart: the boot reconcile
#                adopts the usable ones, closes duplicate/expired ones, leaves
#                the unconfigured one open, opens nothing new, inference
#                works, audits each adopt/close with a short id, reports in
#                /admin/status and the status file; a second SIGKILL is
#                idempotent; an empty or {} catalog closes nothing live; a
#                failing list holds opens and closes nothing, then recovers
#                closing expired only; a slow close does not block opens;
#                a model id that differs only by case is reused, never closed;
#                then docker stop closes the sessions before exit, timed.
#  final         no secrets in logs, no direct-payment request.
#
# Needs a Docker daemon. Image: $UPLINK_E2E_IMAGE (default uplink:e2e);
# pass --build to build it first. E2E_EXTRA_COMPOSE adds a third overlay
# (hosts without container bridge networking).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

[ "${1:-}" = --build ] && docker build -t "${UPLINK_E2E_IMAGE:-uplink:e2e}" --build-arg VERSION=e2e .
export UPLINK_E2E_IMAGE="${UPLINK_E2E_IMAGE:-uplink:e2e}"

export E2E_MOCK_ROUTER="$ROOT/scripts/testdata/e2e_mock_router.py"
# Throwaway fillables (no real wallet / RPC).
export ADMIN_PASSWORD="e2e-Admin-$(openssl rand -hex 12)"
export API_KEY_SEED="$(openssl rand -hex 32)"
COOKIE_SECRET="e2e-cookie-$(openssl rand -hex 12)"
export COOKIE_CONTENT="admin:$COOKIE_SECRET"
export WALLET_PRIVATE_KEY="0x$(openssl rand -hex 32)"
export ETH_NODE_ADDRESS="https://rpc.invalid"
export WEB_PUBLIC_URL="https://uplink.example"
export E2E_MOCK_MOR=1000000
E2E_BOOT_DIR=$(mktemp -d); export E2E_BOOT_DIR

PROJECT="uplink-e2e-mock-$$"
EXTRA=(); [ -n "${E2E_EXTRA_COMPOSE:-}" ] && EXTRA=(-f "$E2E_EXTRA_COMPOSE")
dc() {
  docker compose -p "$PROJECT" -f deploy/secretvm/docker-compose.yml \
    -f scripts/testdata/e2e-mock-router-override.yml "${EXTRA[@]}" "$@"
}
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "---- compose ps ----"; dc ps -a || true
    echo "---- logs ----"; dc logs --no-color 2>&1 | tail -n 250 || true
  fi
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$E2E_BOOT_DIR"
  exit "$rc"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
cid() { dc ps -a -q "$1"; }

# mockctl METHOD PATH [STDIN-AUTH]: talk to the mock from inside its own
# container (works with bridge or shared-netns networking).
mockctl() {
  local c="$1" p="$2"
  "${C:-dc}" exec -T proxy-router python3 -c '
import base64, sys, urllib.request as u, urllib.error as e
m, p = sys.argv[1], sys.argv[2]
r = u.Request("http://127.0.0.1:8082" + p, method=m, data=(b"" if m == "POST" else None))
cred = sys.stdin.read().strip() if len(sys.argv) > 3 else ""
if cred:
    r.add_header("Authorization", "Basic " + base64.b64encode(cred.encode()).decode())
try:
    sys.stdout.write(u.urlopen(r, timeout=5).read().decode())
except e.HTTPError as x:
    sys.stdout.write("HTTP %d" % x.code)
' "$c" "$p" ${3:+auth}
}
stats() { mockctl GET /e2e/stats; }

wait_healthy() {
  local svc="$1" id status i
  id=$("${C:-dc}" ps -a -q "$svc"); [ -n "$id" ] || fail "$svc container missing"
  for i in $(seq 1 90); do
    status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$id")
    [ "$status" = healthy ] && { echo "$svc healthy after ~$((i * 2))s"; return 0; }
    [ "$(docker inspect -f '{{.State.Running}}' "$id")" = true ] || fail "$svc not running (status=$status)"
    sleep 2
  done
  fail "$svc did not become healthy (last status=$status)"
}

# wait_for DESC SECONDS CMD...: poll until CMD succeeds.
wait_for() {
  local desc="$1" n="$2"; shift 2
  for _ in $(seq 1 "$n"); do "$@" >/dev/null && return 0; sleep 1; done
  fail "timed out waiting for: $desc"
}

SESSION=""
UCODE=""
login() {
  local hdr
  hdr=$("${C:-dc}" exec -T uplink curl -sS -D - -o /dev/null -X POST \
    -H 'Content-Type: application/json' -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' \
    --data-binary @- http://127.0.0.1:8080/admin/login <<<"{\"password\":\"$ADMIN_PASSWORD\"}")
  SESSION=$(printf '%s' "$hdr" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' | head -n 1)
  [ -n "$SESSION" ] || fail "admin login failed: $hdr"
}
# uapi METHOD PATH -> body; HTTP code in $UCODE.
uapi() {
  local out
  out=$("${C:-dc}" exec -T uplink curl -sS -w '\n%{http_code}' -X "$1" \
    -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' -H "Cookie: $SESSION" \
    -H 'Content-Type: application/json' ${3:+--data-binary "$3"} "http://127.0.0.1:8080$2")
  UCODE=$(printf '%s' "$out" | tail -n 1)
  printf '%s' "$UCODE" > "$E2E_BOOT_DIR/ucode"   # survives $(...) subshells
  printf '%s' "$out" | sed '$d'
}
status_json() { uapi GET /admin/status; }
router_ok_through_uplink() {
  local st; st=$(status_json)
  [ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] && printf '%s' "$st" | jq -e '.routerAuth.ok == true and .walletAddressSource == "router"' >/dev/null
}

# The uplink server process (not the entrypoint) must run as uid 10001.
assert_uplink_uid() {
  local uids
  uids=$("${C:-dc}" exec -T uplink sh -c '
    for p in /proc/[0-9]*; do
      cmd=$(tr "\0" " " < "$p/cmdline" 2>/dev/null) || continue
      case "$cmd" in /usr/local/bin/uplink*) awk "/^Uid:/{print \$2\" \"\$3}" "$p/status" ;; esac
    done')
  [ -n "$uids" ] || fail "uplink process not found"
  printf '%s\n' "$uids" | while read -r r e; do
    [ "$r" = 10001 ] && [ "$e" = 10001 ] || fail "uplink runs as uid $r/$e, want 10001"
  done
  echo "OK uplink process runs as uid 10001"
}

echo "== boot =="
dc up -d
wait_healthy uplink
body=$(dc exec -T uplink wget -qO- http://127.0.0.1:8080/healthcheck)
case "$body" in *'"status":"healthy"'*) ;; *) fail "unexpected healthcheck body: $body" ;; esac
assert_uplink_uid
login
wait_for "router 200 through uplink" 30 router_ok_through_uplink
echo "OK uplink authenticated to the router with the router cookie"

echo "== sessions: SIGKILL then restart (boot reconcile) =="
MA=0x$(printf 'a1%.0s' $(seq 1 32)); MB=0x$(printf 'b2%.0s' $(seq 1 32)); MX=0x$(printf 'ee%.0s' $(seq 1 32))
APIKEY=$(uapi POST /admin/keys '{"name":"e2e"}' | jq -r .key)
[ -n "$APIKEY" ] && [ "$APIKEY" != null ] || fail "could not create an API key"
# chat MODEL -> HTTP code; the key goes to curl on stdin (-K -), never argv.
chat() {
  "${C:-dc}" exec -T uplink sh -c 'curl -sS -o /dev/null -w "%{http_code}" -m 30 -K - -X POST \
    -H "Content-Type: application/json" --data-binary "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
    http://127.0.0.1:8080/v1/chat/completions' _ "$1" <<<"header = \"Authorization: Bearer $APIKEY\""
}
sessions() { mockctl GET /e2e/sessions; }
sess_field() { sessions | jq -r --arg id "$1" ".sessions[] | select(.Id == \$id) | .$2"; }
opens() { stats | jq -r .session_opens; }
ulog() { dc logs --no-color uplink; }
# Reconcile audit lines (summary, adopt, close, cap) as JSON objects.
rec_lines() { ulog | grep -o '{.*' | jq -cR 'fromjson? | select(.msg == "audit" and (.event // "" | startswith("session_reconcile")))'; }
nsum() { rec_lines | jq -s '[.[] | select(.event == "session_reconcile" and .result != "deferred")] | length'; }
last_sum() { rec_lines | jq -s -c '[.[] | select(.event == "session_reconcile" and .result != "deferred")] | last'; }
# kill_restart: SIGKILL uplink, start it again, wait for the next reconcile summary.
kill_restart() {
  local n0; n0=$(nsum)
  docker kill -s KILL "$(cid uplink)" >/dev/null
  [ "${1:-}" = "--before-start" ] && "$2"
  dc up -d uplink >/dev/null
  wait_healthy uplink >/dev/null
  login
  if [ "${1:-}" != "--no-wait" ] && [ "${1:-}" != "--before-start" ]; then
    summary_after() { [ "$(nsum)" -gt "$n0" ]; }
    wait_for "reconcile summary" 60 summary_after
  fi
}
expect_sum() { # adopted closed skipped
  local l; l=$(last_sum); echo "reconcile: $l"
  [ "$(jq -r .adopted <<<"$l")/$(jq -r .closed <<<"$l")/$(jq -r .skipped <<<"$l")/$(jq -r .failed <<<"$l")" = "$1/$2/$3/0" ] \
    || fail "reconcile counts: want adopted/closed/skipped/failed $1/$2/$3/0, got $l"
}
no_full_ids_in_audit() {
  if rec_lines | grep -qiE '0x[0-9a-f]{9,}'; then fail "reconcile audit carries a full session id"; fi
  rec_lines | jq -r 'select(.session) | .session' | awk 'length($0) != 10 {bad=1} END {exit bad}' || fail "session prefix is not 10 chars"
}
[ "$(chat e2e-alpha)" = 200 ] || fail "chat e2e-alpha through uplink"
SA=$(sessions | jq -r --arg m "$MA" '[.sessions[] | select(.ModelAgentId == $m and .OpenedBy == "uplink")][0].Id')
[ -n "$SA" ] && [ "$SA" != null ] || fail "uplink opened no session for e2e-alpha"
SB=$(mockctl POST "/e2e/seed-session?model=$MB&ends_in=3600" | jq -r .id)       # adoptable
SDUP=$(mockctl POST "/e2e/seed-session?model=$MA&ends_in=300" | jq -r .id)      # duplicate of SA
SEXP=$(mockctl POST "/e2e/seed-session?model=$MB&ends_in=30" | jq -r .id)       # inside expiry buffer
SX=$(mockctl POST "/e2e/seed-session?model=$MX&ends_in=3600" | jq -r .id)       # model not in catalog
open0=$(opens)
kill_restart
[ "$(sess_field "$SA" ClosedAt)" = 0 ] || fail "adoptable session SA was closed"
expect_sum 2 2 1
for id in "$SDUP" "$SEXP"; do [ "$(sess_field "$id" ClosedAt)" != 0 ] || fail "stale session ${id:0:10} left open"; done
for id in "$SA" "$SB" "$SX"; do [ "$(sess_field "$id" ClosedAt)" = 0 ] || fail "session ${id:0:10} was closed (adoptable or unconfigured)"; done
[ "$(rec_lines | jq -s '[.[] | select(.event == "session_reconcile_close")] | map(.reason) | sort | join(",")' -r)" = "duplicate,expired" ] \
  || fail "per-close audit events: $(rec_lines | jq -s -c '[.[] | select(.event == "session_reconcile_close")]')"
[ "$(rec_lines | jq -s '[.[] | select(.event == "session_reconcile_adopt")] | length')" = 2 ] || fail "per-adopt audit events"
no_full_ids_in_audit
pa=$(sess_field "$SA" Prompts); pb=$(sess_field "$SB" Prompts)
[ "$(chat e2e-alpha)" = 200 ] || fail "chat e2e-alpha after restart"
[ "$(chat e2e-beta)" = 200 ] || fail "chat e2e-beta after restart"
[ "$(opens)" = "$open0" ] || fail "uplink opened a new session for a model that had an adoptable one ($(opens) != $open0)"
[ "$(sess_field "$SA" Prompts)" -gt "$pa" ] && [ "$(sess_field "$SB" Prompts)" -gt "$pb" ] || fail "inference did not use the adopted sessions"
st=$(status_json | jq -c .sessionReconcile)
[ "$(jq -r '.state + " " + (.adopted|tostring) + " " + (.closed|tostring) + " " + (.opensHeld|tostring)' <<<"$st")" = "ok 2 2 false" ] || fail "status sessionReconcile: $st"
sf=$(dc exec -T --user 10001 uplink cat /data/session-reconcile.json)
[ "$(jq -r '.state + " " + (.adopted|tostring) + " " + (.skipped|tostring)' <<<"$sf")" = "ok 2 1" ] || fail "status file: $sf"
[ "$(dc exec -T --user 10001 uplink stat -c "%u %a" /data/session-reconcile.json)" = "10001 640" ] || fail "status file owner/mode"
echo "OK reconcile: adopted 2, closed 2 (duplicate, expired), unconfigured left open, no new open, inference works, audited per session, status + file"

echo "== sessions: second SIGKILL right away (idempotent) =="
kill_restart
expect_sum 2 0 1
[ "$(opens)" = "$open0" ] || fail "second restart opened a session"
[ "$(chat e2e-alpha)" = 200 ] && [ "$(opens)" = "$open0" ] || fail "inference after second restart"
echo "OK idempotent: adopted 2 again, nothing new closed or opened"

echo "== sessions: empty and {} catalog close nothing live =="
for m in empty braces; do
  mockctl POST "/e2e/catalog-mode?m=$m" >/dev/null
  kill_restart
  l=$(last_sum); echo "catalog=$m reconcile: $l"
  [ "$(jq -r .closed <<<"$l")" = 0 ] || fail "catalog=$m closed sessions: $l"
  for id in "$SA" "$SB" "$SX"; do [ "$(sess_field "$id" ClosedAt)" = 0 ] || fail "catalog=$m closed ${id:0:10}"; done
done
mockctl POST "/e2e/catalog-mode?m=normal" >/dev/null
echo "OK empty / {} catalog: zero closes, live sessions kept"

echo "== sessions: list failure (deferred, opens held, then recovery) =="
SE2=$(mockctl POST "/e2e/seed-session?model=$MB&ends_in=30" | jq -r .id)
inject_list_fail() { mockctl POST "/e2e/fail?path=/blockchain/sessions/user&code=503" >/dev/null; }
n0=$(nsum)
kill_restart --before-start inject_list_fail
deferred() { ulog | grep -q '"event":"session_reconcile","result":"deferred"'; }
wait_for "deferred audit line" 40 deferred
st=$(status_json | jq -c .sessionReconcile)
jq -e '(.state == "deferred" or .state == "retrying") and .opensHeld == true' <<<"$st" >/dev/null || fail "status during list failure: $st"
o1=$(opens)
[ "$(chat e2e-gamma)" != 200 ] || fail "a new session was opened while the list was failing"
[ "$(opens)" = "$o1" ] || fail "opened a session while the list was failing"
[ "$(sess_field "$SE2" ClosedAt)" = 0 ] || fail "closed a session without a successful list"
mockctl POST "/e2e/fail?path=/blockchain/sessions/user&code=0" >/dev/null
recovered() { [ "$(nsum)" -gt "$n0" ]; }
wait_for "reconcile recovery" 30 recovered
l=$(last_sum); echo "recovery: $l"
jq -e '.late == true and .failed == 0' <<<"$l" >/dev/null || fail "late recovery summary: $l"
[ "$(sess_field "$SE2" ClosedAt)" != 0 ] || fail "late recovery did not close the expired session"
for id in "$SA" "$SB" "$SX"; do [ "$(sess_field "$id" ClosedAt)" = 0 ] || fail "late recovery closed ${id:0:10}"; done
[ "$(chat e2e-alpha)" = 200 ] && [ "$(opens)" = "$o1" ] || fail "inference after recovery"
echo "OK list failure: no closes, no opens while failing; recovery closed expired only"

echo "== sessions: slow close does not block opens =="
SE3=$(mockctl POST "/e2e/seed-session?model=$MB&ends_in=30" | jq -r .id)
slow() { mockctl POST "/e2e/slow-close?sec=20" >/dev/null; }
n0=$(nsum)
kill_restart --before-start slow
closing() { status_json | jq -e '.sessionReconcile.state == "closing"' >/dev/null; }
wait_for "reconcile closing in background" 20 closing
o2=$(opens); t0=$(date +%s)
[ "$(chat e2e-gamma)" = 200 ] || fail "open during slow close"
t1=$(date +%s)
[ "$(opens)" = $((o2 + 1)) ] || fail "expected exactly one new open for e2e-gamma"
[ $((t1 - t0)) -lt 10 ] || fail "open waited $((t1 - t0))s behind a slow close"
closing || fail "close finished early; slow-close not exercised"
mockctl POST "/e2e/slow-close?sec=0" >/dev/null
wait_for "slow close done" 40 recovered
[ "$(sess_field "$SE3" ClosedAt)" != 0 ] || fail "slow close did not complete"
echo "TIMING open during slow close: $((t1 - t0))s"
echo "OK slow close: opens proceed while closes run in the background"

echo "== sessions: model id differs only by case (adopted, reused, never closed) =="
SD=$(mockctl POST "/e2e/seed-session?model=0x$(printf 'D4%.0s' $(seq 1 32))&ends_in=3600" | jq -r .id)
kill_restart
o3=$(opens); pd=$(sess_field "$SD" Prompts)
[ "$(chat e2e-delta)" = 200 ] || fail "chat e2e-delta"
[ "$(opens)" = "$o3" ] || fail "opened a new session next to a case-mismatched adoptable one"
[ "$(sess_field "$SD" Prompts)" -gt "$pd" ] || fail "inference did not use the case-mismatched session"
kill_restart
[ "$(sess_field "$SD" ClosedAt)" = 0 ] || fail "case-mismatched session closed on a later boot"
[ "$(opens)" = "$o3" ] || fail "second boot opened a session"
echo "OK case-only match: adopted, reused (no new open), kept across boots"
no_full_ids_in_audit

echo "== sessions: docker stop closes them before exit =="
grace=$(docker inspect -f '{{.Config.StopTimeout}}' "$(cid uplink)")
[ "$grace" = 45 ] || fail "uplink stop grace is ${grace}s, want 45s"
[ "$(dc exec -T uplink cat /proc/1/comm)" = uplink ] || fail "PID 1 in uplink is $(dc exec -T uplink cat /proc/1/comm), not the app"
t0=$(date +%s.%N)
dc stop uplink >/dev/null
t1=$(date +%s.%N)
code=$(docker inspect -f '{{.State.ExitCode}}' "$(cid uplink)")
for id in "$SA" "$SB"; do
  c=$(sessions | jq -r --arg id "$id" '.sessions[] | select(.Id == $id) | .ClosedAtMono')
  [ "$c" != 0 ] || fail "session ${id:0:10} still open after docker stop"
done
last=$(sessions | jq -r --arg a "$SA" --arg b "$SB" '[.sessions[] | select(.Id == $a or .Id == $b) | .ClosedAtMono] | max')
stop_s=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b-a}')
close_s=$(awk -v a="$t0" -v b="$last" 'BEGIN{printf "%.2f", b-a}')
dc logs --no-color uplink | grep -E 'shutdown: (in-flight|closed)' | tail -n 2
echo "TIMING docker stop: total ${stop_s}s, last session close at +${close_s}s, exit code $code (grace ${grace}s)"
[ "$code" = 0 ] || fail "uplink exit code $code (137 = killed at grace end)"
awk -v s="$stop_s" 'BEGIN{exit !(s < 45)}' || fail "docker stop took ${stop_s}s"
dc up -d uplink >/dev/null
wait_healthy uplink
login
echo "OK graceful stop: sessions closed before exit, within the grace period"

echo "== admin 2FA: revoke-all and logout force a fresh second factor =="
# totp SECRET [STEP_OFFSET] -> 6-digit code (RFC 6238, SHA-1, 30 s).
totp() {
  python3 - "$1" "${2:-0}" <<'PY'
import base64, hashlib, hmac, struct, sys, time
s = sys.argv[1]
k = base64.b32decode(s + "=" * (-len(s) % 8))
h = hmac.new(k, struct.pack(">Q", int(time.time()) // 30 + int(sys.argv[2])), hashlib.sha1).digest()
o = h[-1] & 15
print("%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000))
PY
}
# try_login PASSWORD CODE -> HTTP code in $LCODE, cookie (if any) in $LSESSION.
try_login() {
  local out body
  body=$(jq -cn --arg p "$1" --arg c "$2" '{password: $p} + (if $c == "" then {} else {code: $c} end)')
  out=$(dc exec -T uplink curl -sS -D - -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' \
    --data-binary @- http://127.0.0.1:8080/admin/login <<<"$body")
  LCODE=$(printf '%s' "$out" | tail -n 1)
  LSESSION=$(printf '%s' "$out" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' | head -n 1)
}
login
start=$(uapi POST /admin/2fa/enroll/start '{}')
TOTP_SECRET=$(printf '%s' "$start" | jq -r .secret)
[ -n "$TOTP_SECRET" ] && [ "$TOTP_SECRET" != null ] || fail "2fa enroll start: $start"
conf=$(uapi POST /admin/2fa/enroll/confirm "{\"code\":\"$(totp "$TOTP_SECRET")\"}")
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "2fa enroll confirm: $conf"
RECOVERY=$(printf '%s' "$conf" | jq -r '.recoveryCodes[]')
[ "$(printf '%s\n' "$RECOVERY" | grep -cE '^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$')" = 10 ] || fail "want 10 recovery codes at enrollment"
try_login "$ADMIN_PASSWORD" "$(totp "$TOTP_SECRET" 1)"
[ "$LCODE" = 200 ] && [ -n "$LSESSION" ] || fail "password + TOTP login: $LCODE"
SESSION=$LSESSION
uapi POST /admin/sessions/revoke-all '{}' >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "revoke-all"
uapi GET /admin/status >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "old cookie still works after revoke-all"
try_login "$ADMIN_PASSWORD" ""
[ "$LCODE" = 401 ] && [ -z "$LSESSION" ] || fail "password-only login after revoke-all: $LCODE cookie=${LSESSION:+set}"
dc logs --no-color uplink | grep -q '"event":"login".*"reason":"2fa_required"' || fail "no 2fa_required audit for the password-only login"
RC1=$(printf '%s\n' "$RECOVERY" | head -n 1)
try_login "$ADMIN_PASSWORD" "$RC1"
[ "$LCODE" = 200 ] && [ -n "$LSESSION" ] || fail "recovery-code login: $LCODE"
SESSION=$LSESSION
uapi POST /admin/logout '{}' >/dev/null
uapi GET /admin/status >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "cookie still works after logout"
try_login "$ADMIN_PASSWORD" "$RC1"
[ "$LCODE" = 401 ] || fail "recovery code accepted twice: $LCODE"
echo "OK 2FA: revoke-all and logout end every session; password alone stops at 2fa_required; recovery codes are single-use"

echo "== logs carry no secrets; no direct payment ever requested =="
logs=$(dc logs --no-color 2>&1)
for s in "$COOKIE_SECRET" "$ADMIN_PASSWORD" "$API_KEY_SEED" "${WALLET_PRIVATE_KEY#0x}" "$APIKEY" "$TOTP_SECRET" $RECOVERY; do
  if printf '%s' "$logs" | grep -qF -- "$s"; then fail "a secret appears in the compose logs"; fi
done
[ "$(stats | jq -r .direct_payment_requests)" = 0 ] || fail "a direct-payment request reached the router"
echo "OK no secrets in logs, no direct-payment request"

echo "OK: mock-router e2e (boot, session reconcile, graceful stop)"
