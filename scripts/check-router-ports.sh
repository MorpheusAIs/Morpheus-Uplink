#!/usr/bin/env bash
# Fail if a compose file publishes host ports targeting 8082 or 3333 unless
# host_ip is loopback (127.0.0.1 or ::1). Uses rendered JSON, not YAML text.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Dummy env so templates with ${...:?} still render for port inspection.
export ADMIN_PASSWORD="${ADMIN_PASSWORD:-test-admin-pass-ok-xx}"
export API_KEY_SEED="${API_KEY_SEED:-test-seed-value-xx}"
export COOKIE_CONTENT="${COOKIE_CONTENT:-admin:test-cookie-pass-xx}"
export WALLET_PRIVATE_KEY="${WALLET_PRIVATE_KEY:-0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
export ETH_NODE_ADDRESS="${ETH_NODE_ADDRESS:-https://example.invalid}"
export WEB_PUBLIC_URL="${WEB_PUBLIC_URL:-https://uplink.example}"
export UPLINK_MOR_ALLOWANCE_WEI="${UPLINK_MOR_ALLOWANCE_WEI:-1000000000000000000}"
export ROUTER_AUTH="${ROUTER_AUTH:-uplink:scoped-pass-xx}"
export PUBLIC_HOST="${PUBLIC_HOST:-uplink.example}"
export MOR_TOKEN_ADDRESS="${MOR_TOKEN_ADDRESS:-0x7431ada8a591c955a994a21710752ef9b882b8e3}"

# Returns 0 if clean, 1 if a bad publish is found (or compose config fails).
check_one() {
  local f="$1"
  local json
  if ! json=$(docker compose -f "$f" config --format json 2>/dev/null); then
    echo "BAD: docker compose config failed for $f" >&2
    return 1
  fi
  printf '%s' "$json" | python3 -c '
import json, sys
path = sys.argv[1]
data = json.load(sys.stdin)
bad = []
for sname, svc in (data.get("services") or {}).items():
    for p in svc.get("ports") or []:
        target = p.get("target")
        try:
            tnum = int(target)
        except (TypeError, ValueError):
            continue
        if tnum not in (8082, 3333):
            continue
        host_ip = p.get("host_ip") or ""
        if host_ip not in ("127.0.0.1", "::1"):
            bad.append("%s: target=%s published=%s host_ip=%r" % (
                sname, target, p.get("published"), host_ip))
if bad:
    print("BAD publish in %s:" % path, file=sys.stderr)
    for b in bad:
        print("  " + b, file=sys.stderr)
    sys.exit(1)
' "$f"
}

# Explicit paths:
#   --ok FILE...   each file MUST pass
#   FILE...        each file MUST be rejected (bad fixtures)
if [ "$#" -gt 0 ]; then
  if [ "$1" = "--ok" ]; then
    shift
    for f in "$@"; do
      check_one "$f" || exit 1
      echo "OK: accepted $f"
    done
    echo "OK: fixture(s) correctly accepted"
    exit 0
  fi
  for f in "$@"; do
    if check_one "$f"; then
      echo "port guard: expected failure for $f but it passed" >&2
      exit 1
    fi
    echo "OK: rejected $f"
  done
  echo "OK: fixture(s) correctly rejected"
  exit 0
fi

FILES=(
  "$ROOT"/deploy/*/docker-compose.yml
  "$ROOT"/docker-compose.local.yml
)
bad=0
for f in "${FILES[@]}"; do
  [ -f "$f" ] || continue
  if ! check_one "$f"; then
    bad=1
  fi
done
if [ "$bad" -ne 0 ]; then
  echo "Router ports 8082/3333 must not be published (except loopback 127.0.0.1 / ::1)" >&2
  exit 1
fi
echo "OK: no public 8082/3333 publishes"
