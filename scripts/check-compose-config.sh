#!/usr/bin/env bash
# docker compose config must render for every template (catches ${#PASS} etc).
# Also: single-file deploy templates must render from an empty temp dir with
# no file: configs or relative bind mounts.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export ADMIN_PASSWORD='test-admin-pass-ok-xx'
export API_KEY_SEED='test-seed-value-xx'
export COOKIE_CONTENT='admin:test-cookie-pass-xx'
export WALLET_PRIVATE_KEY='0x'$(printf 'a%.0s' {1..64})
export ETH_NODE_ADDRESS='https://example.invalid'
export WEB_PUBLIC_URL='https://uplink.example'
export UPLINK_MOR_ALLOWANCE_WEI='1000000000000000000'
export ROUTER_AUTH='uplink:scoped-pass-xx'
export PUBLIC_HOST='uplink.example'
export MOR_TOKEN_ADDRESS='0x7431ada8a591c955a994a21710752ef9b882b8e3'

render() {
  local dir="$1" file="$2"
  echo "== $dir/$file =="
  (cd "$ROOT/$dir" && docker compose -f "$file" config >/dev/null)
  echo "OK render $dir/$file"
}

render deploy/generic docker-compose.yml
render deploy/secretvm docker-compose.yml
render deploy/railway docker-compose.yml
render . docker-compose.local.yml

check_single_file() {
  local src="$1"
  local tmp name jsonfile
  name="$(basename "$(dirname "$src")")"
  tmp=$(mktemp -d)
  jsonfile="$tmp/rendered.json"
  cp "$src" "$tmp/docker-compose.yml"
  echo "== single-file ${name}/$(basename "$src") =="
  if grep -nE '^\s+file:\s' "$tmp/docker-compose.yml"; then
    echo "BAD: file: config reference breaks paste-only deploy" >&2
    rm -rf "$tmp"
    exit 1
  fi
  if grep -nE '^\s+-[[:space:]]+(\./|\.\./)' "$tmp/docker-compose.yml" \
    || grep -nE '^\s+source:[[:space:]]*["'"'"']?(\./|\.\./|~)' "$tmp/docker-compose.yml"; then
    echo "BAD: relative bind mount breaks paste-only deploy" >&2
    rm -rf "$tmp"
    exit 1
  fi
  (cd "$tmp" && docker compose -f docker-compose.yml config --format json > "$jsonfile")
  if ! python3 - "$jsonfile" "$tmp" <<'PY'
import json, os, sys
data = json.load(open(sys.argv[1]))
tmpdir = os.path.realpath(sys.argv[2])
bad = []
for sname, svc in (data.get("services") or {}).items():
    for v in svc.get("volumes") or []:
        if not isinstance(v, dict):
            continue
        if v.get("type") == "bind":
            src = v.get("source") or ""
            # Compose resolves relative sources (short or long form)
            # against the project dir, i.e. this empty temp dir.
            real = os.path.realpath(src) if src.startswith("/") else src
            if (src.startswith("./") or src.startswith("../")
                    or real == tmpdir or real.startswith(tmpdir + os.sep)):
                bad.append("%s bind %s (relative)" % (sname, src))
for cname, cfg in (data.get("configs") or {}).items():
    if cfg.get("file"):
        bad.append("config %s file=%s" % (cname, cfg.get("file")))
if bad:
    print("BAD single-file refs:", file=sys.stderr)
    for b in bad:
        print("  " + b, file=sys.stderr)
    sys.exit(1)
PY
  then
    rm -rf "$tmp"
    exit 1
  fi
  (cd "$tmp" && docker compose -f docker-compose.yml config >/dev/null)
  rm -rf "$tmp"
  echo "OK single-file ${name}"
}

check_single_file "$ROOT/deploy/secretvm/docker-compose.yml"
check_single_file "$ROOT/deploy/generic/docker-compose.yml"

# Self-test: relative binds in short AND long form must be refused.
selftest_dir=$(mktemp -d)
mkdir -p "$selftest_dir/x"
for form in short long; do
  if [ "$form" = short ]; then
    printf 'services:\n  a:\n    image: alpine\n    volumes:\n      - ./data:/data\n' > "$selftest_dir/x/docker-compose.yml"
  else
    printf 'services:\n  a:\n    image: alpine\n    volumes:\n      - type: bind\n        source: data\n        target: /data\n' > "$selftest_dir/x/docker-compose.yml"
  fi
  if (check_single_file "$selftest_dir/x/docker-compose.yml") >/dev/null 2>&1; then
    echo "BAD: single-file check accepted a $form-form relative bind" >&2
    rm -rf "$selftest_dir"
    exit 1
  fi
  echo "OK: single-file check refuses $form-form relative bind"
done
rm -rf "$selftest_dir"
echo "OK: all compose configs render"
