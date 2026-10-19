#!/usr/bin/env bash
# E2E: SecretVM-shaped stack with a stateful mock router (real credential
# checks). Uses deploy/secretvm/docker-compose.yml unchanged (caps, volumes,
# networks, depends_on) plus scripts/testdata/e2e-secretvm-override.yml.
#
#  boot 1        init -> uplink healthy; uplink runs as uid 10001; handoff
#                modes; mounts and ownership of the manager volumes (T1);
#                no admin credential in env/cmdline, no netrc, no core dumps
#                (T5); manager network isolation (T8, bridge hosts only).
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
#  funding       allowance follows the wallet balance with the password
#                unchanged (T2); GUI resync request path; rate limit.
#  init re-run   init alone while uplink keeps running: credential kept,
#                uplink still gets 200s from the router; symlinks planted in
#                /ctl are not followed (SHOULD A).
#  manager down  uplink stays healthy, resync reported unavailable (T9).
#  boot 2        host reboot + daemon restart race: uplink starts before
#                init with the previous boot's files on disk and the router
#                has forgotten the user. Uplink must wait (never use the old
#                credential), init mints a new one, the router rejects the
#                old one, uplink authenticates with the new one and reports
#                the new allowance.
#  boot 3        fixed cap: allowance = cap, manager never writes (A1).
#  railway       Railway template (entrypoint mints, caps dropped).
#
# --repro-old IMAGE: run only the boot-2 race against IMAGE and expect the
# old failure (uplink keeps the stale credential). Local diagnostics only.
#
# Needs a Docker daemon. Image: $UPLINK_E2E_IMAGE (default uplink:e2e);
# pass --build to build it first. E2E_EXTRA_COMPOSE / E2E_EXTRA_COMPOSE_RAILWAY
# add a third overlay (hosts without container bridge networking).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

REPRO_IMAGE=""
case "${1:-}" in
  --build) docker build -t "${UPLINK_E2E_IMAGE:-uplink:e2e}" --build-arg VERSION=e2e . ;;
  --repro-old) REPRO_IMAGE="${2:?--repro-old IMAGE}" ;;
esac
export UPLINK_E2E_IMAGE="${REPRO_IMAGE:-${UPLINK_E2E_IMAGE:-uplink:e2e}}"

export E2E_MOCK_ROUTER="$ROOT/scripts/testdata/e2e_mock_router.py"
# Throwaway fillables (no real wallet / RPC).
export ADMIN_PASSWORD="e2e-Admin-$(openssl rand -hex 12)"
export API_KEY_SEED="$(openssl rand -hex 32)"
COOKIE_SECRET="e2e-cookie-$(openssl rand -hex 12)"
export COOKIE_CONTENT="admin:$COOKIE_SECRET"
export WALLET_PRIVATE_KEY="0x$(openssl rand -hex 32)"
export ETH_NODE_ADDRESS="https://rpc.invalid"
export WEB_PUBLIC_URL="https://uplink.example"
export UPLINK_MOR_ALLOWANCE_WEI=""
export E2E_MOCK_MOR=1000000
TOKEN=0x7431ada8a591c955a994a21710752ef9b882b8e3

E2E_BOOT_DIR=$(mktemp -d); chmod 0755 "$E2E_BOOT_DIR"; export E2E_BOOT_DIR
set_boot() { printf '%s\n' "$1" > "$E2E_BOOT_DIR/boot_id"; chmod 0644 "$E2E_BOOT_DIR/boot_id"; }
set_boot "e2e-boot-1-$(openssl rand -hex 4)"

PROJECT="uplink-e2e-$$"
RPROJECT="uplink-e2e-rw-$$"
EXTRA=(); [ -n "${E2E_EXTRA_COMPOSE:-}" ] && EXTRA=(-f "$E2E_EXTRA_COMPOSE")
REXTRA=(); [ -n "${E2E_EXTRA_COMPOSE_RAILWAY:-}" ] && REXTRA=(-f "$E2E_EXTRA_COMPOSE_RAILWAY")
dc() {
  docker compose -p "$PROJECT" -f deploy/secretvm/docker-compose.yml \
    -f scripts/testdata/e2e-secretvm-override.yml "${EXTRA[@]}" "$@"
}
rdc() {
  docker compose -p "$RPROJECT" -f deploy/railway/docker-compose.yml \
    -f scripts/testdata/e2e-railway-override.yml "${REXTRA[@]}" "$@"
}
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "---- compose ps ----"; dc ps -a || true
    echo "---- logs ----"; dc logs --no-color 2>&1 | tail -n 250 || true
    rdc logs --no-color 2>&1 | tail -n 80 || true
  fi
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  rdc down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$E2E_BOOT_DIR"
  exit "$rc"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

cid() { dc ps -a -q "$1"; }
# Capabilities as "DROP... |ADD... " (Docker versions differ in CAP_ prefixes and order).
caps() {
  local d a
  d=$(docker inspect -f '{{range .HostConfig.CapDrop}}{{.}}{{"\n"}}{{end}}' "$1" | sed 's/^CAP_//' | grep . | sort | tr '\n' ' ')
  a=$(docker inspect -f '{{range .HostConfig.CapAdd}}{{.}}{{"\n"}}{{end}}' "$1" | sed 's/^CAP_//' | grep . | sort | tr '\n' ' ')
  printf '%s|%s' "$d" "$a"
}

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
mock_allowance() { stats | jq -r --arg t "$TOKEN" '.users.uplink.allowances[$t] // empty'; }
cur_pw_tag() {
  "${C:-dc}" exec -T uplink sh -c 'sed -n 1p "$0" | cut -d: -f2- | tr -d "\n" | sha256sum | cut -c1-12' "${1:-/shared/router.auth}"
}

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

assert_handoff() {
  local shared copy want_uid body
  shared=$(dc exec -T uplink stat -c '%u %a' /shared/router.auth)
  copy=$(dc exec -T uplink stat -c '%u %a' /data/router.auth)
  want_uid=$(dc exec -T uplink id -u uplink)
  echo "shared router.auth: $shared ; data copy: $copy (uplink uid $want_uid)"
  [ "$shared" = "0 400" ] || fail "shared auth must be root 0400"
  [ "$(dc exec -T uplink stat -c '%u %a' /shared)" = "0 755" ] || fail "/shared must be root 0755"
  [ "$copy" = "$want_uid 400" ] || fail "data copy must be uplink 0400"
  body=$(dc exec -T uplink wget -qO- http://127.0.0.1:8080/healthcheck)
  case "$body" in *'"status":"healthy"'*) ;; *) fail "unexpected healthcheck body: $body" ;; esac
  if docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(cid uplink)" | grep -q '^COOKIE_CONTENT='; then
    fail "COOKIE_CONTENT present in uplink container env"
  fi
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

# T5: the admin credential never shows in any process cmdline/environ in the
# uplink or manager containers; no netrc; core dumps disabled.
assert_no_cookie_leak() {
  local svc n
  for svc in uplink router-allowance-manager; do
    # Secret goes in via a temp pattern file so the checker's own argv cannot match.
    n=$(dc exec -T "$svc" sh -c 'umask 077; f=$(mktemp); cat > "$f"; cat /proc/[0-9]*/cmdline /proc/[0-9]*/environ 2>/dev/null | tr "\0" "\n" | grep -cFf "$f"; rm -f "$f"' <<<"$COOKIE_SECRET" || true)
    [ "$n" = 0 ] || fail "admin credential visible in $svc process cmdline/environ"
    if dc exec -T "$svc" sh -c 'ls -a / /root /home/* /tmp 2>/dev/null | grep -qxE "\.netrc|\.curlrc|_netrc"'; then fail "netrc/curlrc in $svc"; fi
  done
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$(cid router-allowance-manager)" | grep -q '^COOKIE_CONTENT=' \
    && fail "COOKIE_CONTENT in manager env"
  [ "$(dc exec -T router-allowance-manager sh -c 'awk "/Max core file size/{print \$5}" /proc/1/limits')" = 0 ] || fail "manager core dumps not disabled"
  echo "OK T5: no admin credential in cmdline/environ, no netrc, manager core size 0"
}

# T1: mounts and ownership of the manager volumes.
assert_manager_mounts() {
  local m
  m=$(docker inspect -f '{{range .Mounts}}{{.Name}}:{{.Destination}}:{{.RW}} {{end}}' "$(cid router-allowance-manager)")
  echo "manager mounts: $m"
  case "$m" in *"_manager_cred:/mgr:false"*) ;; *) fail "manager must mount manager_cred ro" ;; esac
  case "$m" in *"_router_auth:"*|*"_uplink_data:"*|*docker.sock*) fail "manager mounts router_auth/uplink_data/docker socket" ;; esac
  m=$(docker inspect -f '{{range .Mounts}}{{.Name}}:{{.Destination}}:{{.RW}} {{end}}' "$(cid uplink)")
  case "$m" in *"_manager_cred:"*) fail "uplink mounts manager_cred" ;; esac
  case "$m" in *"_allowance_status:/status:false"*) ;; *) fail "uplink must mount allowance_status ro" ;; esac
  [ "$(dc exec -T router-allowance-manager id -u)" = 10002 ] || fail "manager uid"
  dc exec -T router-allowance-manager id -G | tr ' ' '\n' | grep -qx 10003 || fail "manager lacks group 10003"
  [ "$(dc exec -T router-allowance-manager stat -c '%u %g %a' /mgr)" = "0 10002 750" ] || fail "/mgr owner/mode"
  [ "$(dc exec -T router-allowance-manager stat -c '%u %g %a' /mgr/admin.cookie)" = "10002 10002 400" ] || fail "/mgr/admin.cookie owner/mode"
  [ "$(dc exec -T router-allowance-manager stat -c '%u %a' /mgr/mode)" = "0 444" ] || fail "/mgr/mode owner/mode"
  [ "$(dc exec -T router-allowance-manager stat -c '%u %g %a' /status)" = "10002 10002 755" ] || fail "/status owner/mode"
  [ "$(dc exec -T router-allowance-manager stat -c '%u %g %a' /ctl)" = "10001 10003 730" ] || fail "/ctl owner/mode"
  dc exec -T router-allowance-manager sh -c 'touch /mgr/x 2>/dev/null' && fail "manager can write /mgr"
  dc exec -T router-allowance-manager sh -c 'touch /usr/x 2>/dev/null' && fail "manager rootfs writable"
  dc exec -T router-allowance-manager sh -c 'ls /ctl >/dev/null 2>&1' && fail "manager can list /ctl"
  dc exec -T --user 10001 uplink sh -c 'touch /status/x 2>/dev/null' && fail "uplink can write /status"
  [ "$(caps "$(cid router-allowance-manager)")" = "ALL |" ] || fail "manager caps: $(caps "$(cid router-allowance-manager)")"
  [ "$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "$(cid router-allowance-manager)")" = true ] || fail "manager rootfs not read-only"
  echo "OK T1: manager volumes, owners and modes"
}

# T8: manager reaches proxy-router only (bridge networking only).
assert_manager_isolated() {
  if [ -n "${E2E_EXTRA_COMPOSE:-}" ]; then
    echo "SKIP T8 network isolation (shared-netns overlay in use; covered in CI)"; return 0
  fi
  dc exec -T router-allowance-manager curl -fsS -m 5 http://proxy-router:8082/healthcheck >/dev/null || fail "manager cannot reach proxy-router"
  dc exec -T router-allowance-manager curl -sS -m 5 -o /dev/null http://uplink:8080/healthcheck 2>/dev/null && fail "manager reached uplink"
  dc exec -T router-allowance-manager curl -sS -m 5 -o /dev/null http://1.1.1.1/ 2>/dev/null && fail "manager reached an external address"
  echo "OK T8: manager reaches only proxy-router"
}

repro_old() {
  echo "== repro on $UPLINK_E2E_IMAGE: daemon restart race =="
  dc up -d uplink
  wait_healthy uplink
  local old new
  old=$(cur_pw_tag)
  mockctl POST /e2e/forget-users >/dev/null
  dc stop uplink >/dev/null
  set_boot "e2e-boot-2-$(openssl rand -hex 4)"
  docker start "$(cid uplink)" >/dev/null
  sleep 5
  docker start "$(cid router-user-init)" >/dev/null
  docker wait "$(cid router-user-init)" >/dev/null
  new=$(cur_pw_tag)
  login; status_json >/dev/null; sleep 3; status_json >/dev/null
  local s; s=$(stats)
  echo "old=$old new=$new rejected(old)=$(printf '%s' "$s" | jq -r --arg t "$old" '.unauthorized_by_pw[$t] // 0') ok(new)=$(printf '%s' "$s" | jq -r --arg t "$new" '.ok_by_pw[$t] // 0')"
  if [ "$(printf '%s' "$s" | jq -r --arg t "$old" '.unauthorized_by_pw[$t] // 0')" -gt 0 ] \
     && [ "$(printf '%s' "$s" | jq -r --arg t "$new" '.ok_by_pw[$t] // 0')" = 0 ]; then
    echo "REPRODUCED: uplink kept the previous boot's credential and the router rejects it"
    return 0
  fi
  echo "NOT REPRODUCED"; return 1
}

if [ -n "$REPRO_IMAGE" ]; then repro_old; exit $?; fi

echo "== boot 1 =="
dc up -d
wait_healthy uplink
wait_healthy router-allowance-manager
assert_handoff
assert_uplink_uid
login
wait_for "router 200 through uplink" 30 router_ok_through_uplink
echo "OK uplink authenticated to the router"
assert_manager_mounts
assert_no_cookie_leak
assert_manager_isolated
[ "$(dc exec -T router-allowance-manager cat /mgr/mode)" = balance ] || fail "mode marker should be balance"
allowance_is() { [ "$(mock_allowance)" = "$1" ]; }
wait_for "allowance = balance" 30 allowance_is "$E2E_MOCK_MOR"
[ "$(mock_allowance)" = "$E2E_MOCK_MOR" ] || fail "boot allowance $(mock_allowance) != balance $E2E_MOCK_MOR"

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

echo "== T2 funding resync (password unchanged) =="
pw0=$(cur_pw_tag); up0=$(docker inspect -f '{{.State.StartedAt}}' "$(cid uplink)")
mockctl POST "/e2e/fund?mor=7770000" >/dev/null
wait_for "allowance follows funding" 30 allowance_is 7770000
status_wei() { [ "$(status_json | jq -r .morAllowance.wei)" = "$1" ]; }
wait_for "status shows new allowance" 20 status_wei 7770000
[ "$(stats | jq -r .users.uplink.pw)" = "$pw0" ] || fail "funding resync changed the password"
[ "$(cur_pw_tag)" = "$pw0" ] || fail "router.auth changed"
[ "$(docker inspect -f '{{.State.StartedAt}}' "$(cid uplink)")" = "$up0" ] || fail "uplink restarted during resync"
dc logs --no-color router-allowance-manager | grep -q '"event":"allowance_resync".*"trigger":"poll".*"result":"ok"' || fail "no resync audit line"
mockctl POST "/e2e/consume?wei=3000000" >/dev/null   # stake turnover lowers it
wait_for "allowance restored after turnover" 30 allowance_is 7770000
[ "$(stats | jq -r .users.uplink.pw)" = "$pw0" ] || fail "turnover restore changed the password"
echo "OK T2: allowance followed funding and was restored after turnover; password and uplink unchanged"
status_json | jq -e '.allowanceResync.available == true and .allowanceResync.lastResult == "ok"' >/dev/null || fail "resync not reported available"
mockctl POST "/e2e/fund?mor=8880000" >/dev/null
uapi POST /admin/allowance/resync '{}' >/dev/null
[ "$UCODE" = 202 ] || fail "resync request code $UCODE"
wait_for "flag-triggered resync" 20 allowance_is 8880000
uapi POST /admin/allowance/resync '{}' >/dev/null
[ "$UCODE" = 429 ] || fail "second resync request should be rate limited (got $UCODE)"
dc logs --no-color router-allowance-manager | grep -q '"trigger":"flag"' || echo "note: poll beat the flag to the write (both paths ok)"
echo "OK resync request: 202, applied, then 429"

echo "== init re-run while uplink keeps running (+ SHOULD A: planted symlinks) =="
dc exec -T --user 10001 uplink ln -s /mgr/admin.cookie /ctl/planted-file
dc exec -T --user 10001 uplink ln -s /mgr /ctl/planted-dir
auth0=$(dc exec -T uplink sha256sum /shared/router.auth | cut -d' ' -f1)
up0=$(docker inspect -f '{{.State.StartedAt}}' "$(cid uplink)")
ok0=$(stats | jq -r --arg t "$pw0" '.ok_by_pw[$t] // 0')
docker start -a "$(cid router-user-init)" > "$E2E_BOOT_DIR/init-rerun.log" 2>&1 || { cat "$E2E_BOOT_DIR/init-rerun.log"; fail "init re-run failed"; }
grep -q 'password kept' "$E2E_BOOT_DIR/init-rerun.log" || { cat "$E2E_BOOT_DIR/init-rerun.log"; fail "init re-minted a valid credential"; }
grep -qF "$COOKIE_SECRET" "$E2E_BOOT_DIR/init-rerun.log" && fail "admin credential in init log"
[ "$(dc exec -T uplink sha256sum /shared/router.auth | cut -d' ' -f1)" = "$auth0" ] || fail "router.auth changed on re-run"
[ "$(docker inspect -f '{{.State.Running}} {{.State.StartedAt}}' "$(cid uplink)")" = "true $up0" ] || fail "uplink restarted/stopped"
router_ok_through_uplink || fail "uplink lost router access after init re-run"
[ "$(stats | jq -r --arg t "$pw0" '.ok_by_pw[$t] // 0')" -gt "$ok0" ] || fail "no router 200 for the kept credential after re-run"
echo "OK init re-run: credential kept, uplink untouched, router 200 through uplink"
[ "$(dc exec -T router-allowance-manager stat -c '%u %g %a' /mgr/admin.cookie)" = "10002 10002 400" ] || fail "A: symlink target (cookie) changed"
[ "$(dc exec -T router-allowance-manager stat -c '%u %g %a' /mgr)" = "0 10002 750" ] || fail "A: symlink target (/mgr) changed"
[ "$(dc exec -T --user 10001 uplink readlink /ctl/planted-file)" = /mgr/admin.cookie ] || fail "A: planted link changed"
[ "$(dc exec -T --user 10001 uplink stat -c '%u' /ctl/planted-file)" = 10001 ] || fail "A: planted link re-owned"
[ "$(dc exec -T router-allowance-manager stat -c '%u %g %a' /ctl)" = "10001 10003 730" ] || fail "A: /ctl owner/mode"
dc exec -T --user 10001 uplink rm -f /ctl/planted-file /ctl/planted-dir
echo "OK SHOULD A: init did not follow symlinks planted in /ctl"

echo "== T9 manager down =="
dc stop router-allowance-manager >/dev/null
resync_unavailable() { status_json | jq -e '.allowanceResync.available == false' >/dev/null; }
wait_for "resync reported unavailable" 30 resync_unavailable
uapi POST /admin/allowance/resync '{}' >/dev/null
[ "$UCODE" = 409 ] || fail "resync with manager down should be 409 (got $UCODE)"
dc exec -T uplink wget -qO- http://127.0.0.1:8080/healthcheck | grep -q healthy || fail "uplink unhealthy with manager down"
router_ok_through_uplink || fail "router access with manager down"
echo "OK T9: manager down => uplink healthy, resync unavailable"

echo "== boot 2: reboot + daemon restart race, router forgot the user =="
oldraw=$(dc exec -T uplink cat /shared/router.auth)
oldtag=$(cur_pw_tag)
mockctl POST /e2e/forget-users >/dev/null
mockctl POST "/e2e/fund?mor=5550000" >/dev/null
dc stop uplink >/dev/null
set_boot "e2e-boot-2-$(openssl rand -hex 4)"
rej0=$(stats | jq -r --arg t "$oldtag" '.unauthorized_by_pw[$t] // 0')
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
docker start "$(cid uplink)" >/dev/null      # daemon ignores depends_on
sleep 6
docker logs --since "$since" "$(cid uplink)" 2>&1 | grep -q 'waiting for router-user-init' || fail "uplink did not wait for this boot's init"
[ "$(docker inspect -f '{{.State.Health.Status}}' "$(cid uplink)")" != healthy ] || fail "uplink went healthy on stale files"
[ "$(stats | jq -r --arg t "$oldtag" '.unauthorized_by_pw[$t] // 0')" = "$rej0" ] || fail "uplink used the previous boot's credential"
docker start "$(cid router-user-init)" >/dev/null
[ "$(docker wait "$(cid router-user-init)")" = 0 ] || fail "init failed on boot 2"
# Init itself checks the old credential once before re-minting (expected).
rej1=$(stats | jq -r --arg t "$oldtag" '.unauthorized_by_pw[$t] // 0')
[ "$rej1" -le $((rej0 + 1)) ] || fail "old credential used more than init's one validity check"
rej0=$rej1
wait_healthy uplink
docker start "$(cid router-allowance-manager)" >/dev/null
newtag=$(cur_pw_tag)
[ "$newtag" != "$oldtag" ] || fail "router forgot the user but the credential was not replaced"
[ "$(mockctl GET /wallet auth <<<"$oldraw")" = "HTTP 401" ] || fail "router accepted the old credential"
rej0=$((rej0 + 1))   # the probe above is the only expected use of the old credential
login
wait_for "router 200 through uplink (new credential)" 30 router_ok_through_uplink
[ "$(stats | jq -r --arg t "$newtag" '.ok_by_pw[$t] // 0')" -gt 0 ] || fail "uplink did not authenticate with the new credential"
[ "$(stats | jq -r --arg t "$oldtag" '.unauthorized_by_pw[$t] // 0')" = "$rej0" ] || fail "uplink tried the old credential"
st=$(status_json)
[ "$(printf '%s' "$st" | jq -r .morAllowance.wei)" = 5550000 ] || fail "new allowance not reported: $(printf '%s' "$st" | jq -c .morAllowance)"
docker logs --since "$since" "$(cid uplink)" 2>&1 | grep -qE '"msg":"mor allowance","wei":"5550000"|wei=5550000' || fail "new allowance not logged at boot"
assert_handoff; assert_uplink_uid
echo "OK boot 2: waited for init, new credential (old rejected), new allowance reported"

echo "== boot 3: fixed cap =="
export UPLINK_MOR_ALLOWANCE_WEI=4242
dc up -d
wait_healthy uplink
wait_healthy router-allowance-manager
login
wait_for "router 200 through uplink" 30 router_ok_through_uplink
[ "$(dc exec -T router-allowance-manager cat /mgr/mode)" = cap ] || fail "mode marker should be cap"
st=$(status_json)
printf '%s' "$st" | jq -e '.morAllowance.wei == "4242" and .morAllowance.source == "override"' >/dev/null || fail "cap not reported: $(printf '%s' "$st" | jq -c .morAllowance)"
[ "$(mock_allowance)" = 4242 ] || fail "router allowance $(mock_allowance) != cap"
c0=$(stats | jq -r .allowance_confirms)
mockctl POST "/e2e/fund?mor=9990000" >/dev/null
sleep 8
[ "$(mock_allowance)" = 4242 ] && [ "$(stats | jq -r .allowance_confirms)" = "$c0" ] || fail "manager wrote in cap mode"
status_json | jq -e '.allowanceResync.available == false' >/dev/null || fail "resync offered in cap mode"
echo "OK boot 3: cap honoured, manager never wrote"

echo "== logs carry no secrets; no direct payment ever requested =="
logs=$(dc logs --no-color 2>&1)
printf '%s' "$logs" | grep -qF "$COOKIE_SECRET" && fail "admin credential in container logs"
printf '%s' "$logs" | grep -qF "$(printf '%s' "$oldraw" | cut -d: -f2-)" && fail "scoped password in container logs"
[ "$(stats | jq -r .direct_payment_requests)" = 0 ] || fail "a direct-payment request reached the router"
echo "OK T5 logs / T10: no secrets in logs, no direct-payment request"

echo "== railway template (entrypoint mints; caps dropped) =="
export UPLINK_MOR_ALLOWANCE_WEI=""
C=rdc
rdc up -d
wait_healthy uplink
[ "$(caps "$(rdc ps -q uplink)")" = "ALL |CHOWN SETGID SETUID " ] || fail "railway caps: $(caps "$(rdc ps -q uplink)")"
[ "$(rdc exec -T uplink stat -c '%u %a' /data/router.auth)" = "10001 400" ] || fail "railway auth file owner/mode"
assert_uplink_uid
login
wait_for "railway router 200 through uplink" 30 router_ok_through_uplink
n=$(rdc exec -T uplink sh -c 'for p in /proc/[0-9]*; do case "$(tr "\0" " " < $p/cmdline 2>/dev/null)" in /usr/local/bin/uplink*) tr "\0" "\n" < $p/environ;; esac; done | grep -c "^COOKIE_CONTENT=" || true')
[ "$n" = 0 ] || fail "railway uplink process still has COOKIE_CONTENT"
C=dc
[ "$(stats | jq -r .direct_payment_requests)" = 0 ] || fail "railway: direct-payment request"
echo "OK railway: minted in-container with caps dropped, uplink uid 10001, no admin cookie in the server env"

echo "OK: SecretVM auth handoff e2e (boot, session reconcile, graceful stop, funding resync, init re-run, manager down, reboot race, cap, railway)"
