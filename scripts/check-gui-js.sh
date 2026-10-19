#!/usr/bin/env bash
# Extract inline GUI script and syntax-check with node.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HTML="$ROOT/internal/gui/static/index.html"
OUT=$(mktemp --suffix=.js)
trap 'rm -f "$OUT"' EXIT
python3 - "$HTML" "$OUT" <<'PY'
import re, sys
html = open(sys.argv[1]).read()
scripts = re.findall(r"<script(?:\s[^>]*)?>(.*?)</script>", html, re.S | re.I)
body = "\n".join(s for s in scripts if s.strip())
open(sys.argv[2], "w").write(body)
PY
node --check "$OUT"
echo "OK: GUI script parses"
