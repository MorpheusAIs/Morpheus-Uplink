#!/usr/bin/env bash
# Integration: router-user-init.sh against a mock router (python http.server).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT_PATH="$ROOT/deploy/init/router-user-init.sh"
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"; kill ${MOCK_PID:-} 2>/dev/null || true' EXIT

PORT=18765
# Prefer a free port
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')

cat > "$WORKDIR/mock_router.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

MODE = os.environ.get("MOCK_MODE", "balance")
MOR = os.environ.get("MOCK_MOR", "9000000000000000000")

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def _auth_ok(self):
        return self.headers.get("Authorization", "").startswith("Basic ")
    def do_GET(self):
        if self.path == "/healthcheck":
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok"); return
        if self.path == "/blockchain/balance":
            if MODE == "fail":
                self.send_response(500); self.end_headers(); return
            if not self._auth_ok():
                self.send_response(401); self.end_headers(); return
            body = json.dumps({"mor": MOR, "eth": "0"}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404); self.end_headers()
    def do_DELETE(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        _ = self.rfile.read(n) if n else b""
        self.send_response(200); self.end_headers(); self.wfile.write(b"{}")

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY

run_case() {
  local name="$1" mode="$2" mor="$3" override="$4" want_wei="$5" want_source="$6"
  echo "== case $name =="
  kill ${MOCK_PID:-} 2>/dev/null || true
  MOCK_MODE="$mode" MOCK_MOR="$mor" python3 "$WORKDIR/mock_router.py" "$PORT" &
  MOCK_PID=$!
  sleep 0.3
  local out="$WORKDIR/$name"
  mkdir -p "$out"
  env -i PATH="$PATH" HOME="$HOME" \
    ROUTER_URL="http://127.0.0.1:$PORT" \
    ROUTER_AUTH_OUT="$out/router.auth" \
    UPLINK_MOR_ALLOWANCE_FILE="$out/router.mor-allowance.json" \
    COOKIE_CONTENT="admin:testpass" \
    UPLINK_MOR_ALLOWANCE_WEI="$override" \
    sh "$SCRIPT_PATH"
  test -f "$out/router.auth"
  test -f "$out/router.mor-allowance.json"
  python3 - "$out/router.mor-allowance.json" "$want_wei" "$want_source" <<'PY'
import json, sys
meta = json.load(open(sys.argv[1]))
assert meta["wei"] == sys.argv[2], meta
assert meta["source"] == sys.argv[3], meta
if meta["source"] == "zero":
    assert meta.get("warning"), meta
print("OK", meta)
PY
  kill "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=
}

run_case balance balance 9000000000000000000 "" 9000000000000000000 balance
run_case override balance 9000000000000000000 1000000000000000000 1000000000000000000 override
run_case zero balance 0 "" 0 zero
run_case failread fail 0 "" 0 zero
echo "OK: router-user-init integration cases"
