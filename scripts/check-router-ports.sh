#!/usr/bin/env bash
# Fail if deploy templates publish host ports 8082 or 3333 (except 127.0.0.1:8082).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FILES=(
  "$ROOT"/deploy/*/docker-compose.yml
  "$ROOT"/docker-compose.local.yml
)
bad=0
# Host publish forms: "8082:8082", 8082:8082, 0.0.0.0:8082:8082, "[::]:8082:8082"
pattern='^[[:space:]]*-[[:space:]]*"?([0-9.]+:)?(8082|3333):'
published='published:[[:space:]]*"?(8082|3333)"?'
for f in "${FILES[@]}"; do
  [ -f "$f" ] || continue
  while IFS= read -r line; do
    # Allow loopback publish of 8082 only.
    if echo "$line" | grep -qE '127\.0\.0\.1:8082'; then
      continue
    fi
    echo "BAD publish in $f: $line"
    bad=1
  done < <(grep -nE "$pattern|$published" "$f" || true)
done
if [ "$bad" -ne 0 ]; then
  echo "Router ports 8082/3333 must not be published (except local 127.0.0.1:8082)"
  exit 1
fi
echo "OK: no public 8082/3333 publishes"
