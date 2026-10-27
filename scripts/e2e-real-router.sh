#!/usr/bin/env bash
# E2E smoke: the REAL pinned proxy-router image (deploy/secretvm digest) with
# Uplink, using deploy/secretvm/docker-compose.yml plus
# scripts/testdata/e2e-real-router-override.yml. Throwaway wallet key (never
# funded), public Base RPC, no CI secrets. No session is opened.
#
#  - router /healthcheck polled from the host (the image is FROM scratch,
#    so there is no container healthcheck)
#  - Uplink boots, reaches the router with the router cookie, serves
#    /admin/status (routerAuth ok, wallet from the router)
#  - /node/* passthrough: /node/healthcheck and the Swagger spec pass
#    (basePath rewritten); /node/blockchain/send/eth and spelling variants
#    (plus audio/transcriptions, provider/ping, sessions/initiate) get 403 for
#    every method; PUT/PATCH/PROPFIND get 403 anywhere; writes to bids, providers, models,
#    config/ethNode, config/models/reload, wallet and auth get 403; GET
#    /node/wallet passes through, are audited, and never reach the router
#    (checked in the router's own request log)
#  - boot reconcile on an empty session list: ok, nothing adopted or closed
#  - docker stop: Uplink is PID 1, exits 0 within the 45 s grace
#  - no secrets in logs
#
# Needs a Docker daemon. Image: $UPLINK_E2E_IMAGE (default uplink:e2e);
# --build builds it first. E2E_EXTRA_COMPOSE adds a third overlay (hosts
# without container bridge networking); E2E_ROUTER_HEALTH_URL overrides the
# host-side health URL (default: the router container's IP).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

[ "${1:-}" = --build ] && docker build -t "${UPLINK_E2E_IMAGE:-uplink:e2e}" --build-arg VERSION=e2e .
export UPLINK_E2E_IMAGE="${UPLINK_E2E_IMAGE:-uplink:e2e}"

export ADMIN_PASSWORD="e2e-Admin-$(openssl rand -hex 12)"
export API_KEY_SEED="$(openssl rand -hex 32)"
COOKIE_SECRET="e2e-cookie-$(openssl rand -hex 12)"
export COOKIE_CONTENT="admin:$COOKIE_SECRET"
export WALLET_PRIVATE_KEY="0x$(openssl rand -hex 32)"   # throwaway, never funded
export ETH_NODE_ADDRESS="${E2E_ETH_RPC:-https://mainnet.base.org}"
export WEB_PUBLIC_URL="https://uplink.example"

PROJECT="uplink-e2e-real-$$"
EXTRA=(); [ -n "${E2E_EXTRA_COMPOSE:-}" ] && EXTRA=(-f "$E2E_EXTRA_COMPOSE")
dc() {
  docker compose -p "$PROJECT" -f deploy/secretvm/docker-compose.yml \
    -f scripts/testdata/e2e-real-router-override.yml "${EXTRA[@]}" "$@"
}
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "---- compose ps ----"; dc ps -a || true
    echo "---- logs ----"; dc logs --no-color 2>&1 | tail -n 200 || true
  fi
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  exit "$rc"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
cid() { dc ps -a -q "$1"; }
wait_for() {
  local desc="$1" n="$2"; shift 2
  for _ in $(seq 1 "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
  fail "timed out waiting for: $desc"
}

pin=$(sed -n 's/^ *image: *\(ghcr.io\/morpheusais\/morpheus-lumerin-node:v7\.17\.0@sha256:[0-9a-f]\{64\}\).*/\1/p' deploy/secretvm/docker-compose.yml)
[ -n "$pin" ] || fail "router image is not the pinned v7.17.0 digest"
echo "router image: $pin"

echo "== up =="
dc up -d
[ "$(docker inspect -f '{{.Config.Image}}' "$(cid proxy-router)")" = "$pin" ] || fail "proxy-router is not running the pinned image"

HURL="${E2E_ROUTER_HEALTH_URL:-}"
if [ -z "$HURL" ]; then
  ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$(cid proxy-router)" | awk '{print $1}')
  [ -n "$ip" ] || fail "no router container IP"
  HURL="http://$ip:8082/healthcheck"
fi
router_healthy() { curl -fsS -m 3 "$HURL" >/dev/null; }
t0=$(date +%s)
wait_for "router /healthcheck 200 (host poll $HURL)" 180 router_healthy
echo "OK router /healthcheck 200 from the host after ~$(( $(date +%s) - t0 ))s"

uplink_healthy() { [ "$(docker inspect -f '{{.State.Health.Status}}' "$(cid uplink)")" = healthy ]; }
wait_for "uplink healthy" 120 uplink_healthy
echo "OK uplink healthy"

SESSION=""
login() {
  local hdr
  hdr=$(dc exec -T uplink curl -sS -D - -o /dev/null -X POST \
    -H 'Content-Type: application/json' -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' \
    --data-binary @- http://127.0.0.1:8080/admin/login <<<"{\"password\":\"$ADMIN_PASSWORD\"}")
  SESSION=$(printf '%s' "$hdr" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' | head -n 1)
  [ -n "$SESSION" ] || fail "admin login failed"
}
status_json() {
  dc exec -T uplink curl -sS -f -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' \
    -H "Cookie: $SESSION" http://127.0.0.1:8080/admin/status
}
login
router_ok() { status_json | jq -e '.routerAuth.ok == true and .walletAddressSource == "router" and (.walletAddress // "" | test("^0x[0-9a-fA-F]{40}$"))' >/dev/null; }
wait_for "/admin/status 200 with routerAuth ok and the router's wallet" 60 router_ok
echo "OK /admin/status 200: routerAuth ok, wallet from the router"

echo "== /node passthrough =="
# node_req METHOD PATH -> HTTP code (admin session + CSRF headers, raw path).
node_req() {
  dc exec -T uplink curl -sS --path-as-is -o /dev/null -w '%{http_code}' -X "$1" \
    -H "Origin: $WEB_PUBLIC_URL" -H 'Sec-Fetch-Site: same-origin' -H "Cookie: $SESSION" \
    -H 'Content-Type: application/json' --data-binary '{}' "http://127.0.0.1:8080$2"
}
rlog() { dc logs --no-color proxy-router 2>&1; }
c=$(node_req GET /node/healthcheck); [ "$c" = 200 ] || fail "/node/healthcheck did not pass through ($c)"
spec=$(dc exec -T uplink curl -sS -f -H "Cookie: $SESSION" http://127.0.0.1:8080/node/swagger/doc.json)
printf '%s' "$spec" | jq -e '.basePath == "/node"' >/dev/null || fail "swagger basePath not rewritten"
seen_spec() { rlog | grep -q 'HTTP-REQ\] GET /swagger/doc.json'; }
wait_for "router request log shows the passthrough" 20 seen_spec
echo "OK /node/healthcheck 200 and Swagger spec (basePath /node) pass through; router logs them"
n=0
for p in /node/blockchain/send/eth /node/BLOCKCHAIN/Send/ETH/ /node/blockchain/s%65nd/eth \
         /node/blockchain/%2e%2e/blockchain/send/eth /node/healthcheck/../blockchain/send/eth \
         /node/blockchain%2fsend/eth /node/blockchain//send/eth /node/blockchain/send/mor \
         /node/v1/audio/transcriptions /node/V1/Audio/Transcriptions/ \
         /node/proxy/provider/ping /node/PROXY/Provider/Ping/ /node/proxy/sessions/initiate \
         /node/proxy/sessions/%69nitiate; do
  for m in GET POST PUT PATCH DELETE; do
    c=$(node_req "$m" "$p"); n=$((n + 1))
    [ "$c" = 403 ] || fail "$m $p: $c, want 403"
  done
done
# Write-blocked routes: every non-read method is refused.
for p in /node/blockchain/bids /node/Blockchain/BIDS/ /node/blockchain/providers /node/blockchain/models \
         /node/config/ethNode /node/Config/EthNode/ \
         /node/config/models/reload /node/wallet/privateKey /node/wallet /node/auth/users; do
  for m in POST PUT PATCH DELETE; do
    c=$(node_req "$m" "$p"); n=$((n + 1))
    [ "$c" = 403 ] || fail "$m $p: $c, want 403"
  done
done
# Only GET, HEAD, POST and DELETE are forwarded.
for m in PUT PATCH PROPFIND; do
  c=$(node_req "$m" /node/healthcheck); n=$((n + 1))
  [ "$c" = 403 ] || fail "$m /node/healthcheck: $c, want 403"
done
sleep 1
# One regex for the self-check and the real router-log check.
blocked_re='HTTP-REQ\] ([A-Z]+ [^ ]*(send|wallet/privatekey|audio|config/ethnode|config/models|provider/ping|sessions/initiate)|(POST|PUT|PATCH|DELETE) /(blockchain/(bids|providers|models)|wallet|auth)\b|(PUT|PATCH|PROPFIND) )'
# Self-check: the regex must match a canned blocked line and must not match a
# canned allowed line, so the router-log assertion below can actually fail.
printf '%s\n' '[HTTP-REQ] POST /blockchain/send/eth' | grep -qiE "$blocked_re" \
  || fail "router-log regex self-check: does not match a canned blocked request line"
if printf '%s\n' '[HTTP-REQ] GET /healthcheck' | grep -qiE "$blocked_re"; then
  fail "router-log regex self-check: matches a canned allowed request line"
fi
if rlog | grep -iE "$blocked_re"; then
  fail "a blocked request reached the router"
fi
blocks=$(dc logs --no-color uplink | grep -c '"event":"node_passthrough_block"' || true)
[ "$blocks" -ge "$n" ] || fail "audit: $blocks node_passthrough_block events, want >= $n"
echo "OK /node deny list: $n blocked requests -> 403, audited, none in the router request log"
w=$(dc exec -T uplink curl -sS -f -H "Cookie: $SESSION" http://127.0.0.1:8080/node/wallet)
st_addr=$(status_json | jq -r .walletAddress)
[ "$(printf '%s' "$w" | jq -r .address | tr 'A-F' 'a-f')" = "$(printf '%s' "$st_addr" | tr 'A-F' 'a-f')" ] || fail "GET /node/wallet: unexpected body"
printf '%s' "$w" | jq -e 'keys - ["address","kind","derivationPath"] == []' >/dev/null || fail "GET /node/wallet returned extra fields"
echo "OK GET /node/wallet passes through (200 from the router; address only)"

rec_lines() { dc logs --no-color uplink | grep -o '{.*' | jq -cR 'fromjson? | select(.msg == "audit" and (.event // "" | startswith("session_reconcile")))'; }
summary() { rec_lines | jq -s -c '[.[] | select(.event == "session_reconcile" and .result != "deferred")] | last // empty'; }
has_summary() { [ -n "$(summary)" ]; }
wait_for "boot reconcile summary" 120 has_summary
l=$(summary); echo "reconcile: $l"
jq -e '.adopted == 0 and .closed == 0 and .failed == 0' <<<"$l" >/dev/null || fail "reconcile on an empty list: $l"
status_json | jq -e '.sessionReconcile.state == "ok" and .sessionReconcile.opensHeld == false' >/dev/null \
  || fail "status sessionReconcile: $(status_json | jq -c .sessionReconcile)"
echo "OK boot reconcile: empty list, nothing adopted or closed, opens not held"

echo "== graceful stop =="
grace=$(docker inspect -f '{{.Config.StopTimeout}}' "$(cid uplink)")
[ "$grace" = 45 ] || fail "uplink stop grace is ${grace}s, want 45s"
[ "$(dc exec -T uplink cat /proc/1/comm)" = uplink ] || fail "PID 1 in uplink is not the app"
t0=$(date +%s.%N); dc stop uplink >/dev/null; t1=$(date +%s.%N)
code=$(docker inspect -f '{{.State.ExitCode}}' "$(cid uplink)")
stop_s=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b-a}')
echo "TIMING docker stop: ${stop_s}s, exit code $code (grace ${grace}s)"
[ "$code" = 0 ] || fail "uplink exit code $code"
awk -v s="$stop_s" 'BEGIN{exit !(s < 45)}' || fail "docker stop took ${stop_s}s"
echo "OK graceful stop: exit 0 within the grace period"

logs=$(dc logs --no-color 2>&1)
for s in "$COOKIE_SECRET" "$ADMIN_PASSWORD" "$API_KEY_SEED" "${WALLET_PRIVATE_KEY#0x}"; do
  if printf '%s' "$logs" | grep -qF -- "$s"; then fail "a secret appears in the compose logs"; fi
done
echo "OK no secrets in logs"
echo "OK: real-router e2e smoke ($pin)"
