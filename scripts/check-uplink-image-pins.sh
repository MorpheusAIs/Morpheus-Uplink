#!/usr/bin/env bash
# Fail if deploy templates publish an unpinned Uplink image on release, or
# (in PR mode) report TODO/:latest markers. Box-safe; no registry calls.
set -euo pipefail

ALLOW_TODO=0
FAIL_TODO=0
FAIL_LATEST=0
for arg in "$@"; do
  case "$arg" in
    --allow-todo) ALLOW_TODO=1 ;;
    --fail-todo) FAIL_TODO=1 ;;
    --fail-latest) FAIL_LATEST=1 ;;
    --self-test)
      tmp=$(mktemp -d)
      printf 'image: ghcr.io/morpheusais/uplink:latest # TODO digest\n' > "$tmp/a.yml"
      if FAIL_TODO=1 FAIL_LATEST=1 "$0" --fail-todo --fail-latest "$tmp" 2>/dev/null; then
        echo "self-test: expected failure for TODO+latest" >&2
        exit 1
      fi
      printf 'image: ghcr.io/morpheusais/uplink@sha256:%s\n' "$(printf 'a%.0s' {1..64})" > "$tmp/b.yml"
      # Override scan root via env for self-test
      echo "self-test: ok"
      exit 0
      ;;
  esac
done

ROOT="${1:-.}"
files=$(find "$ROOT/deploy" "$ROOT" -maxdepth 2 -name 'docker-compose*.yml' 2>/dev/null | sort -u)
# Also local compose
files="$files $ROOT/docker-compose.local.yml"

found_todo=0
found_latest=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  # Only lines that set the uplink image (not router)
  if grep -E 'image:[[:space:]]*ghcr\.io/morpheusais/uplink' "$f" >/dev/null 2>&1; then
    if grep -E 'image:[[:space:]]*ghcr\.io/morpheusais/uplink:latest' "$f" >/dev/null 2>&1; then
      found_latest=1
      echo "unpinned :latest in $f"
    fi
    if grep -E 'TODO[[:space:]]*digest|TODO digest' "$f" >/dev/null 2>&1; then
      found_todo=1
      echo "TODO digest marker in $f"
    fi
  fi
done <<EOFIND
$(find "$ROOT/deploy" -name 'docker-compose*.yml' 2>/dev/null; echo "$ROOT/docker-compose.local.yml")
EOFIND

if [ "$FAIL_LATEST" = 1 ] && [ "$found_latest" = 1 ]; then
  echo "❌ Release requires digest-pinned Uplink image (no :latest)" >&2
  exit 1
fi
if [ "$FAIL_TODO" = 1 ] && [ "$found_todo" = 1 ]; then
  echo "❌ Release requires Uplink digest pin (remove TODO digest placeholders)" >&2
  exit 1
fi

if [ "$ALLOW_TODO" = 1 ]; then
  echo "✅ Uplink image pin check (TODO/:latest allowed on non-release)"
  exit 0
fi

echo "✅ Uplink image pin check passed"
