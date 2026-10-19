#!/usr/bin/env bash
# docker compose config must render for every template (catches ${#PASS} etc).
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
echo "OK: all compose configs render"
