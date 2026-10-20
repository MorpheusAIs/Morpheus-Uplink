#!/usr/bin/env bash
# Compose policy for every template.
#  S  uplink: no init: true, no CAP_KILL, stop_grace_period >= 45s (the app
#     is PID 1 so docker stop's SIGTERM closes sessions before exit).
#  H  proxy-router has no container healthcheck (the image is FROM scratch:
#     no shell, no wget) and uplink never waits on service_healthy for it.
# Self-test: each rule must reject a deliberately broken render.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export ADMIN_PASSWORD='test-admin-pass-ok-xx' API_KEY_SEED='test-seed-value-xx'
export COOKIE_CONTENT='admin:test-cookie-pass-xx' WALLET_PRIVATE_KEY='0x'$(printf 'a%.0s' {1..64})
export ETH_NODE_ADDRESS='https://example.invalid' WEB_PUBLIC_URL='https://uplink.example'
export ROUTER_AUTH='admin:test-cookie-pass-xx' PUBLIC_HOST='uplink.example'
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/check.py" <<'PY'
import json, re, sys
name, rendered = sys.argv[1:3]
svcs = json.load(open(rendered)).get("services") or {}
bad = []
u = svcs.get("uplink")
if u is None:
    bad.append("no uplink service")
else:
    if u.get("init"):
        bad.append("S: uplink sets init: true (SIGTERM must reach the app as PID 1)")
    if "KILL" in [c.upper().replace("CAP_", "") for c in (u.get("cap_add") or [])]:
        bad.append("S: uplink adds CAP_KILL")
    g = str(u.get("stop_grace_period") or "")
    m = re.fullmatch(r"(?:(\d+)m)?(?:(\d+)s)?", g)
    secs = (int(m.group(1) or 0) * 60 + int(m.group(2) or 0)) if (g and m) else 0
    if secs < 45:
        bad.append("S: uplink stop_grace_period %r < 45s" % g)
    dep = (u.get("depends_on") or {}).get("proxy-router") or {}
    if dep.get("condition") == "service_healthy":
        bad.append("H: uplink waits on proxy-router service_healthy (router image has no shell for a healthcheck)")
r = svcs.get("proxy-router")
if r is not None:
    hc = r.get("healthcheck") or {}
    if hc and not hc.get("disable"):
        bad.append("H: proxy-router has a healthcheck (image is FROM scratch; it can never pass)")
if bad:
    for b in bad: print("BAD(%s): %s" % (name, b), file=sys.stderr)
    sys.exit(1)
print("OK(%s): S/H" % name)
PY

render() { (cd "$(dirname "$1")" && docker compose -f "$(basename "$1")" config --format json) > "$2"; }
for f in deploy/secretvm/docker-compose.yml deploy/generic/docker-compose.yml \
         deploy/railway/docker-compose.yml docker-compose.local.yml; do
  render "$ROOT/$f" "$tmp/r.json"
  python3 "$tmp/check.py" "$f" "$tmp/r.json"
done

render "$ROOT/deploy/secretvm/docker-compose.yml" "$tmp/base.json"
n=0
for m in up_init up_kill up_grace router_hc up_healthy; do
  python3 - "$tmp/base.json" "$tmp/bad.json" "$m" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); s = d["services"]; m = sys.argv[3]
if m == "up_init": s["uplink"]["init"] = True
if m == "up_kill": s["uplink"]["cap_add"] = s["uplink"].get("cap_add", []) + ["KILL"]
if m == "up_grace": s["uplink"]["stop_grace_period"] = "10s"
if m == "router_hc": s["proxy-router"]["healthcheck"] = {"test": ["CMD-SHELL", "wget -qO- http://127.0.0.1:8082/healthcheck"]}
if m == "up_healthy": s["uplink"]["depends_on"] = {"proxy-router": {"condition": "service_healthy", "required": True}}
json.dump(d, open(sys.argv[2], "w"))
PY
  if python3 "$tmp/check.py" "selftest-$m" "$tmp/bad.json" 2>/dev/null; then
    echo "BAD: self-test $m was not rejected" >&2; exit 1
  fi
  n=$((n + 1))
done
echo "OK: compose policy (self-tests reject $n broken variants)"
