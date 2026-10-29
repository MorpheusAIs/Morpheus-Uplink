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
#  failover      a client 400 is relayed (no close, no open); a 402 on
#                provider A closes A and opens on B by bid (router
#                failover off) and the request answers 200; an impaired
#                400 on B with A cooling keeps the session (no alternate).
#  health        opens go only to bids the gateway bids feed marks healthy:
#                a provider marked unhealthy is never opened; after a good
#                load, a bids feed answering 500, a redirect or a frozen
#                last_updated keeps opening on the last good copy and
#                /admin/status shows feeds.bids loaded with a growing age;
#                with the bids feed never loaded and no bidDetail, a request
#                needing an open gets 503 no_available_provider. Uplink
#                runs in the mock's network namespace (router and feeds at
#                http://127.0.0.1:8082); the mock is never restarted.
#  2fa           2FA starts off (password-only); turning it on (enroll,
#                password in the body; confirm re-issues the session and
#                ends the old cookie) makes login need TOTP or a recovery code; the switch
#                survives a container restart; reset keeps it on and forces
#                re-enrollment; turning it off (password + code) ends every
#                session, login is password-only again, and that survives a
#                restart. The state file is read as uid 10001.
#  final         no secrets in logs, no direct-payment request, no open by
#                model (every open names a bid).
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
# Console requests send its host as Host: /admin, /gui and /node answer only on it.
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
    -H 'Content-Type: application/json' -H "Host: ${WEB_PUBLIC_URL#https://}" -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' \
    --data-binary @- http://127.0.0.1:8080/admin/login <<<"{\"password\":\"$ADMIN_PASSWORD\"}")
  SESSION=$(printf '%s' "$hdr" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' | head -n 1)
  [ -n "$SESSION" ] || fail "admin login failed: $hdr"
}
# uapi METHOD PATH -> body; HTTP code in $UCODE.
uapi() {
  local out
  out=$("${C:-dc}" exec -T uplink curl -sS -w '\n%{http_code}' -X "$1" \
    -H "Host: ${WEB_PUBLIC_URL#https://}" -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' -H "Cookie: $SESSION" \
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
  local id; id=$(cid uplink)
  docker kill -s KILL "$id" >/dev/null
  # Wait until Docker reports it stopped, or "up -d" may still see it
  # running and not start it again.
  docker wait "$id" >/dev/null
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

echo "== provider failover: client 400 relayed, 402 on A moves to B, impaired 400 with no alternate kept =="
MC=0x$(printf 'c3%.0s' $(seq 1 32))
PA=0x$(printf 'a0%.0s' $(seq 1 20)); PB=0x$(printf 'b0%.0s' $(seq 1 20))
# chat_body MODEL -> response body (code in $E2E_BOOT_DIR/ccode).
chat_body() {
  local out
  out=$(dc exec -T uplink sh -c 'curl -sS -w "\n%{http_code}" -m 60 -K - -X POST \
    -H "Content-Type: application/json" --data-binary "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
    http://127.0.0.1:8080/v1/chat/completions' _ "$1" <<<"header = \"Authorization: Bearer $APIKEY\"")
  printf '%s' "$out" | tail -n 1 > "$E2E_BOOT_DIR/ccode"
  printf '%s' "$out" | sed '$d'
}
gamma_open() { sessions | jq -r --arg m "$MC" '[.sessions[] | select((.ModelAgentId | ascii_downcase) == $m and .ClosedAt == 0)] | map(.Id + " " + .Provider) | join(",")'; }
pfail() { mockctl POST "/e2e/provider-fail?provider=$1&code=$2&msg=$3" >/dev/null; }
fo_lines() { ulog | grep -o '{.*' | jq -cR 'fromjson? | select(.msg == "audit" and .event == "provider_failover")'; }
[ "$(chat e2e-gamma)" = 200 ] || fail "chat e2e-gamma before failover"
before=$(gamma_open)
case "$before" in *" $PA") ;; *) fail "want one open e2e-gamma session on provider A, got: $before" ;; esac
SG=${before%% *}
o4=$(opens); c4=$(stats | jq -r .session_closes)
pfail "$PA" 400 "max_tokens%20is%20too%20large"
b=$(chat_body e2e-gamma)
[ "$(cat "$E2E_BOOT_DIR/ccode")" = 400 ] || fail "client 400: got $(cat "$E2E_BOOT_DIR/ccode") $b"
printf '%s' "$b" | jq -e '.error.message == "max_tokens is too large"' >/dev/null || fail "client 400 body: $b"
[ "$(opens)" = "$o4" ] && [ "$(stats | jq -r .session_closes)" = "$c4" ] || fail "a client 400 opened or closed a session"
echo "OK client 400 relayed with its status; no close, no open"
pfail "$PA" 402 "Insufficient%20balance"
b=$(chat_body e2e-gamma)
[ "$(cat "$E2E_BOOT_DIR/ccode")" = 200 ] || fail "402 on A: got $(cat "$E2E_BOOT_DIR/ccode") $b"
[ "$(sess_field "$SG" ClosedAt)" != 0 ] || fail "failed session on A was not closed"
after=$(gamma_open)
case "$after" in *" $PB") ;; *) fail "want the e2e-gamma session on provider B, got: $after" ;; esac
[ "$(opens)" = $((o4 + 1)) ] || fail "want exactly one new open, got $(( $(opens) - o4 ))"
mockctl GET /e2e/opens | jq -e --arg a "$PA" '.opens | last | .bid != "" and (.fields - ["directPayment", "failover", "sessionDuration"]) == [] and .failover == false and .provider != $a' >/dev/null \
  || fail "failover open: $(mockctl GET /e2e/opens | jq -c '.opens | last')"
fo_lines | jq -e -s 'map(select(.result == "retried_ok" and .status == "402")) | length == 1' >/dev/null || fail "no provider_failover retried_ok audit: $(fo_lines)"
echo "OK 402 on A: closed A, opened on B by bid (staking-only body) and failover:false, answered 200, audited"
o5=$(opens); c5=$(stats | jq -r .session_closes)
pfail "$PB" 400 "Rate%20limit%20exceeded"
b=$(chat_body e2e-gamma)
[ "$(cat "$E2E_BOOT_DIR/ccode")" = 400 ] || fail "impaired 400 on B with A cooling: got $(cat "$E2E_BOOT_DIR/ccode") $b"
[ "$(opens)" = "$o5" ] && [ "$(stats | jq -r .session_closes)" = "$c5" ] || fail "no-alternate case opened or closed a session"
[ "$(gamma_open)" = "$after" ] || fail "no-alternate case changed the pooled session"
fo_lines | jq -e -s 'map(select(.result == "no_alternate")) | length == 1' >/dev/null || fail "no provider_failover no_alternate audit: $(fo_lines)"
pfail "$PA" 0 x; pfail "$PB" 0 x
[ "$(chat e2e-gamma)" = 200 ] || fail "chat e2e-gamma after clearing faults"
[ "$(stats | jq -r .failover_true_opens)" = 0 ] || fail "an open asked the router for failover"
echo "OK impaired 400 on B, A cooling: no alternate, session kept, original status relayed; no open ever sent failover:true"

echo "== provider health: unhealthy never opened, last good kept, never loaded refuses =="
bids_fetches() { stats | jq -r '.bids_fetches // 0'; }
nopens() { mockctl GET /e2e/opens | jq '.opens | length'; }
close_pool() { uapi POST /admin/pool/close-all >/dev/null; }
feeds() { status_json | jq -c .feeds; }
# opens_since N: the opens after the first N, as "provider bid" lines.
opens_since() { mockctl GET /e2e/opens | jq -r --argjson n "$1" '.opens[$n:][] | .provider + " " + .bid'; }
# open_on_b_only N WHAT: every open after the first N is by bid on provider B.
open_on_b_only() {
  local o; o=$(opens_since "$1")
  [ -n "$o" ] || fail "$2: no open"
  printf '%s\n' "$o" | awk -v b="$PB" '$1 != b || $2 == "" {bad=1} END {exit bad}' || fail "$2: opens not all by bid on B: $o"
}
mockctl POST "/e2e/bids-mode?m=unhealthy:$PA" >/dev/null
kill_restart
close_pool
n=$(nopens)
for m in e2e-alpha e2e-beta e2e-gamma; do [ "$(chat "$m")" = 200 ] || fail "chat $m with provider A unhealthy"; done
open_on_b_only "$n" "provider A unhealthy"
f=$(feeds); echo "feeds: $f"
jq -e '.health_source == "bids" and .bids.loaded == true and .bids.stale == false and .models.loaded == true' <<<"$f" >/dev/null || fail "status feeds after a good load: $f"
echo "OK unhealthy provider A never opened; every open by bid on B"

# After a good load the copy is refetched only past the 5 min TTL; then each
# failed fetch pauses 10 s. A, unhealthy in the last good copy, stays unused.
age0=$(feeds | jq -r .bids.age_seconds)
mockctl POST "/e2e/bids-mode?m=500" >/dev/null
f0=$(bids_fetches)
refetched() { chat e2e-alpha >/dev/null; [ "$(bids_fetches)" -gt "$1" ]; }
echo "waiting for the bids TTL (5 min) ..."
for _ in $(seq 1 40); do refetched "$f0" && break; sleep 10; done
[ "$(bids_fetches)" -gt "$f0" ] || fail "uplink never refetched the bids feed after the TTL"
for mode in 500 redirect stale; do
  mockctl POST "/e2e/bids-mode?m=$mode" >/dev/null
  f1=$(bids_fetches)
  wait_for "bids refetch ($mode)" 40 refetched "$f1"
  close_pool
  n=$(nopens)
  [ "$(chat e2e-alpha)" = 200 ] || fail "chat with the bids feed on $mode"
  open_on_b_only "$n" "bids feed $mode (last good copy)"
  f=$(feeds); echo "bids=$mode feeds: $f"
  jq -e --argjson a "$age0" '.health_source == "bids" and .bids.loaded == true and .bids.age_seconds > $a' <<<"$f" >/dev/null \
    || fail "status feeds with the bids feed on $mode: $f"
done
[ "$(stats | jq -r '.bids_redirect_followed // 0')" = 0 ] || fail "uplink followed a bids feed redirect"
echo "OK bids feed 500 / redirect / frozen after a good load: opens on the last good copy, status shows it loaded and ageing"

mockctl POST "/e2e/bids-mode?m=500" >/dev/null
mockctl POST "/e2e/catalog-mode?m=nodetail" >/dev/null
kill_restart
close_pool
n=$(nopens)
b=$(chat_body e2e-alpha)
[ "$(cat "$E2E_BOOT_DIR/ccode")" = 503 ] || fail "never-loaded bids + no bidDetail: got $(cat "$E2E_BOOT_DIR/ccode") $b"
printf '%s' "$b" | jq -e '.error.type == "no_available_provider"' >/dev/null || fail "never-loaded bids body: $b"
[ "$(nopens)" = "$n" ] || fail "an open was sent with no health known"
f=$(feeds); echo "feeds: $f"
jq -e '.health_source == "none" and .bids.loaded == false' <<<"$f" >/dev/null || fail "status feeds with no health: $f"
mockctl POST "/e2e/bids-mode?m=normal" >/dev/null
mockctl POST "/e2e/catalog-mode?m=normal" >/dev/null
kill_restart
[ "$(chat e2e-alpha)" = 200 ] || fail "chat after the bids feed recovered"
echo "OK never-loaded bids feed and no bidDetail: 503 no_available_provider, no open; recovers once the feed loads"

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
    -H 'Content-Type: application/json' -H "Host: ${WEB_PUBLIC_URL#https://}" -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' \
    --data-binary @- http://127.0.0.1:8080/admin/login <<<"$body")
  LCODE=$(printf '%s' "$out" | tail -n 1)
  LSESSION=$(printf '%s' "$out" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' | head -n 1)
}
# pw_body: {"password": ADMIN_PASSWORD} for the enrolment calls.
pw_body() { jq -cn --arg p "$ADMIN_PASSWORD" '{password: $p}'; }
# enroll_confirm CODE: POST /admin/2fa/enroll/confirm {password, code}. A
# success signs out the other sessions and re-issues this one, so the new
# cookie replaces $SESSION. Body in $ECONF, HTTP code in $E2E_BOOT_DIR/ucode.
enroll_confirm() {
  local out body cookie
  body=$(jq -cn --arg p "$ADMIN_PASSWORD" --arg c "$1" '{password: $p, code: $c}')
  out=$(dc exec -T uplink curl -sS -D - -w '\n%{http_code}' -X POST \
    -H "Host: ${WEB_PUBLIC_URL#https://}" -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' -H "Cookie: $SESSION" \
    -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:8080/admin/2fa/enroll/confirm <<<"$body" | tr -d '\r')
  printf '%s' "$out" | tail -n 1 > "$E2E_BOOT_DIR/ucode"
  cookie=$(printf '%s\n' "$out" | sed -n '1,/^$/s/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' | head -n 1)
  ECONF=$(printf '%s\n' "$out" | sed '1,/^$/d' | sed '$d')
  if [ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ]; then
    [ -n "$cookie" ] || fail "enroll confirm did not re-issue the session cookie"
    SESSION=$cookie
  fi
}
# state_auth: the auth object of the state file, read as the app user
# (uid 10001): the service drops CAP_DAC_OVERRIDE, so a root exec cannot.
state_auth() { dc exec -T --user 10001 uplink cat /data/uplink-state.json | jq -c '.auth // {}'; }
login
uapi GET /admin/2fa/status | jq -e '.on == false and .enrolled == false and .need2fa == false' >/dev/null || fail "2FA must start off"
state_auth | jq -e '.twoFactorOn == false' >/dev/null || fail "state file: want twoFactorOn false on a fresh install"
start=$(uapi POST /admin/2fa/enroll/start "$(pw_body)")
TOTP_SECRET=$(printf '%s' "$start" | jq -r .secret)
[ -n "$TOTP_SECRET" ] && [ "$TOTP_SECRET" != null ] || fail "2fa enroll start: $start"
PRE_ENROLL_SESSION=$SESSION
enroll_confirm "$(totp "$TOTP_SECRET")"; conf=$ECONF
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "2fa enroll confirm: $conf"
NEW_ENROLL_SESSION=$SESSION; SESSION=$PRE_ENROLL_SESSION; uapi GET /admin/2fa/status >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "pre-enrolment cookie still works after confirm: $(cat "$E2E_BOOT_DIR/ucode")"
SESSION=$NEW_ENROLL_SESSION
RECOVERY=$(printf '%s' "$conf" | jq -r '.recoveryCodes[]')
[ "$(printf '%s\n' "$RECOVERY" | grep -cE '^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$')" = 10 ] || fail "want 10 recovery codes at enrollment"
uapi GET /admin/2fa/status | jq -e '.on == true and .enrolled == true' >/dev/null || fail "2FA must be on after enrollment"
state_auth | jq -e '.twoFactorOn == true' >/dev/null || fail "state file: want twoFactorOn true after enrollment"
try_login "$ADMIN_PASSWORD" "$(totp "$TOTP_SECRET" 1)"
USED_STEP=$(( $(date +%s) / 30 + 1 ))   # last TOTP step the server accepted
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

echo "== admin 2FA switch survives a restart; reset keeps it on and forces re-enrollment =="
dc restart uplink >/dev/null
wait_healthy uplink
state_auth | jq -e '.twoFactorOn == true and .enrolled == true' >/dev/null || fail "state file after restart: want 2FA on and enrolled"
try_login "$ADMIN_PASSWORD" ""
[ "$LCODE" = 401 ] && [ -z "$LSESSION" ] || fail "password-only login after restart with 2FA on: $LCODE"
RC2=$(printf '%s\n' "$RECOVERY" | sed -n 2p)
try_login "$ADMIN_PASSWORD" "$RC2"
[ "$LCODE" = 200 ] || fail "login before reset: $LCODE"
SESSION=$LSESSION; OLD_SESSION=$LSESSION
# A TOTP step newer than any the server accepted (a step works once).
while [ $(( $(date +%s) / 30 )) -le "$USED_STEP" ]; do sleep 1; done
out=$(uapi POST /admin/2fa/reset "{\"password\":\"$ADMIN_PASSWORD\",\"code\":\"$(totp "$TOTP_SECRET")\"}")
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "reset with password + TOTP: $(cat "$E2E_BOOT_DIR/ucode") $out"
printf '%s' "$out" | jq -e '.ok == true and .reenroll == true' >/dev/null || fail "reset response: $out"
dc logs --no-color uplink | grep -q '"event":"2fa_reset","result":"ok".*"reason":"sessions_revoked=' || fail "no 2fa_reset audit"
SESSION=$OLD_SESSION; uapi GET /admin/2fa/status >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "old cookie after reset: $(cat "$E2E_BOOT_DIR/ucode")"
state_auth | jq -e '(has("totpBlob") or has("recoveryCodes") or has("enrolled") or has("pendingSecretEnc")) | not' >/dev/null \
  || fail "state file still holds 2FA material after reset"
state_auth | jq -e '.twoFactorOn == true' >/dev/null || fail "state file: reset must keep the 2FA switch on"
try_login "$ADMIN_PASSWORD" ""
[ "$LCODE" = 200 ] && [ -n "$LSESSION" ] || fail "password-only login after reset: $LCODE"
SESSION=$LSESSION
uapi GET /admin/2fa/status | jq -e '.on == true and .enrolled == false and .need2fa == true' >/dev/null || fail "want the enrollment-required state after reset"
body=$(uapi GET /admin/status)
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 403 ] && printf '%s' "$body" | grep -q '2fa enrollment required' || fail "admin routes before re-enroll: $(cat "$E2E_BOOT_DIR/ucode") $body"
start=$(uapi POST /admin/2fa/enroll/start "$(pw_body)")
NEW_SECRET=$(printf '%s' "$start" | jq -r .secret)
[ -n "$NEW_SECRET" ] && [ "$NEW_SECRET" != null ] && [ "$NEW_SECRET" != "$TOTP_SECRET" ] || fail "re-enroll must issue a new secret"
if [ "$(totp "$TOTP_SECRET")" != "$(totp "$NEW_SECRET")" ]; then
  enroll_confirm "$(totp "$TOTP_SECRET")"
  [ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "OLD secret accepted at enroll confirm: $(cat "$E2E_BOOT_DIR/ucode")"
fi
enroll_confirm "$(totp "$NEW_SECRET")"; conf=$ECONF
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "enroll confirm with new secret: $conf"
NEW_RECOVERY=$(printf '%s' "$conf" | jq -r '.recoveryCodes[]')
[ "$(printf '%s\n' "$NEW_RECOVERY" | grep -c .)" = 10 ] || fail "want 10 new recovery codes"
if [ "$(totp "$TOTP_SECRET" 1)" != "$(totp "$NEW_SECRET" 1)" ]; then
  try_login "$ADMIN_PASSWORD" "$(totp "$TOTP_SECRET" 1)"
  [ "$LCODE" = 401 ] || fail "OLD TOTP accepted at login after re-enroll: $LCODE"
fi
try_login "$ADMIN_PASSWORD" "$RC2"
[ "$LCODE" = 401 ] || fail "old recovery code accepted after re-enroll: $LCODE"
try_login "$ADMIN_PASSWORD" "$(totp "$NEW_SECRET" 1)"
[ "$LCODE" = 200 ] || fail "login with the new TOTP: $LCODE"
SESSION=$LSESSION; uapi GET /admin/status >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "admin access after re-enroll: $(cat "$E2E_BOOT_DIR/ucode")"
echo "OK 2FA switch: on survives a restart; reset wiped it on disk, ended sessions, re-enrolled a new secret, old secret and codes rejected"

echo "== admin 2FA off: password + code, every session ends, password-only login survives a restart =="
uapi POST /admin/2fa/remove "{\"password\":\"$ADMIN_PASSWORD\"}" >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "turning 2FA off without a code: $(cat "$E2E_BOOT_DIR/ucode")"
NRC1=$(printf '%s\n' "$NEW_RECOVERY" | head -n 1)
uapi POST /admin/2fa/remove "{\"password\":\"wrong-$ADMIN_PASSWORD\",\"code\":\"$NRC1\"}" >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "turning 2FA off with a wrong password: $(cat "$E2E_BOOT_DIR/ucode")"
state_auth | jq -e '.twoFactorOn == true and .enrolled == true' >/dev/null || fail "a rejected turn-off changed the state file"
OFF_SESSION=$SESSION
out=$(uapi POST /admin/2fa/remove "{\"password\":\"$ADMIN_PASSWORD\",\"code\":\"$NRC1\"}")
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "turning 2FA off with password + recovery code: $(cat "$E2E_BOOT_DIR/ucode") $out"
dc logs --no-color uplink | grep -q '"event":"2fa_remove","result":"ok".*"reason":"sessions_revoked=' || fail "no 2fa_remove audit"
SESSION=$OFF_SESSION; uapi GET /admin/2fa/status >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 401 ] || fail "session after turning 2FA off: $(cat "$E2E_BOOT_DIR/ucode")"
state_auth | jq -e '.twoFactorOn == false and ((has("totpBlob") or has("recoveryCodes") or has("enrolled")) | not)' >/dev/null \
  || fail "state file after turning 2FA off: want twoFactorOn false and no 2FA material"
try_login "$ADMIN_PASSWORD" ""
[ "$LCODE" = 200 ] && [ -n "$LSESSION" ] || fail "password-only login with 2FA off: $LCODE"
dc restart uplink >/dev/null
wait_healthy uplink
state_auth | jq -e '.twoFactorOn == false' >/dev/null || fail "state file after restart: want 2FA still off"
try_login "$ADMIN_PASSWORD" ""
[ "$LCODE" = 200 ] && [ -n "$LSESSION" ] || fail "password-only login after restart with 2FA off: $LCODE"
SESSION=$LSESSION
uapi GET /admin/2fa/status | jq -e '.on == false and .enrolled == false' >/dev/null || fail "want 2FA off after restart"
uapi GET /admin/status >/dev/null
[ "$(cat "$E2E_BOOT_DIR/ucode")" = 200 ] || fail "admin access with 2FA off: $(cat "$E2E_BOOT_DIR/ucode")"
echo "OK 2FA off: needs password + code, sessions ended, password-only login, still off after a restart"

echo "== logs carry no secrets; no direct payment ever requested =="
logs="$(dc logs --no-color 2>&1)"
for s in "$COOKIE_SECRET" "$ADMIN_PASSWORD" "$API_KEY_SEED" "${WALLET_PRIVATE_KEY#0x}" "$APIKEY" "$TOTP_SECRET" "$NEW_SECRET" $RECOVERY $NEW_RECOVERY; do
  if printf '%s' "$logs" | grep -qF -- "$s"; then fail "a secret appears in the compose logs"; fi
done
[ "$(stats | jq -r .direct_payment_requests)" = 0 ] || fail "a direct-payment request reached the router"
mockctl GET /e2e/opens | jq -e '(.opens | length) > 0 and ([.opens[] | select(.bid == "")] | length) == 0' >/dev/null \
  || fail "an open by model reached the router: $(mockctl GET /e2e/opens | jq -c '[.opens[] | select(.bid == "")]')"
echo "OK no secrets in logs, no direct-payment request, every open by bid"

echo "OK: mock-router e2e (boot, session reconcile, graceful stop)"
