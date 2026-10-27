#!/usr/bin/env bash
# Require the pinned Lumerin router digest on every compose template.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WANT='ghcr.io/morpheusais/morpheus-lumerin-node:v7.17.0@sha256:da1baa55299133ad63e2e590bd8cc1e95772ff81f4b44e2aa34cabe682e9a14d'
bad=0
for f in "$ROOT"/deploy/*/docker-compose.yml "$ROOT"/docker-compose.local.yml; do
  [ -f "$f" ] || continue
  if ! grep -qF "$WANT" "$f"; then
    echo "Missing router digest pin in $f"
    bad=1
  fi
  # No unpinned :v7.17.0 without digest
  if grep -nE 'morpheus-lumerin-node:v7\.17\.0(["[:space:]]|$)' "$f" | grep -v '@sha256:'; then
    echo "Unpinned router tag in $f"
    bad=1
  fi
done
if [ "$bad" -ne 0 ]; then exit 1; fi
echo "OK: router digest pinned"
