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
BODY = os.environ.get("MOCK_BODY", "")

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
            body = (BODY or json.dumps({"mor": MOR, "eth": "0"})).encode()
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
  local name="$1" mode="$2" mor="$3" override="$4" want_wei="$5" want_source="$6" want_warn="${7:-}"
  echo "== case $name =="
  kill ${MOCK_PID:-} 2>/dev/null || true
  MOCK_MODE="$mode" MOCK_MOR="$mor" MOCK_BODY="${MOCK_BODY:-}" python3 "$WORKDIR/mock_router.py" "$PORT" &
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
  python3 - "$out/router.mor-allowance.json" "$want_wei" "$want_source" "$want_warn" <<'PY'
import json, sys
meta = json.load(open(sys.argv[1]))
if sys.argv[4]:
    assert sys.argv[4] in meta.get("warning", ""), meta
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
# Explicit 0 must cap at 0, never fall through to wallet balance.
run_case zero_override balance 9000000000000000000 "0" 0 zero "UPLINK_MOR_ALLOWANCE_WEI=0"
# Explicit 0 must not read balance (read failure is irrelevant).
run_case zero_override_noread fail 0 "0" 0 zero "UPLINK_MOR_ALLOWANCE_WEI=0"
# Leading zeros normalized; still explicit override.
run_case padded_override balance 9000000000000000000 "000123" 123 override
run_case padded_zero balance 9000000000000000000 "000" 0 zero
# Surrounding whitespace trimmed; still an override.
run_case trimmed_override balance 9000000000000000000 "  123 " 123 override
# Balance parsing: bare integer, pretty-printed multi-line, leading zeros.
MOCK_BODY='{"eth":"0","mor":7000}' run_case bal_number balance 0 "" 7000 balance
MOCK_BODY=$'{\n  "eth": "1",\n  "mor": "0005000"\n}' run_case bal_multiline balance 0 "" 5000 balance
# Ambiguous / malformed balance must fail closed to 0 (never a wrong cap).
MOCK_BODY='{"mor":"5","nested":{"mor":"9"}}' run_case bal_duplicate balance 0 "" 0 zero "unparseable"
MOCK_BODY='{"mor":1.5e18,"eth":"0"}' run_case bal_float balance 0 "" 0 zero "unparseable"
MOCK_BODY='{"mor":"-5"}' run_case bal_negative balance 0 "" 0 zero "unparseable"
MOCK_BODY='{"amor":"5","eth":"0"}' run_case bal_missing balance 0 "" 0 zero "unparseable"

# Invalid overrides must exit non-zero and write nothing.
run_fail_case() {
  local name="$1" override="$2"
  echo "== fail case $name =="
  kill ${MOCK_PID:-} 2>/dev/null || true
  MOCK_MODE=balance python3 "$WORKDIR/mock_router.py" "$PORT" &
  MOCK_PID=$!
  sleep 0.3
  local out="$WORKDIR/$name"
  mkdir -p "$out"
  if env -i PATH="$PATH" HOME="$HOME" \
    ROUTER_URL="http://127.0.0.1:$PORT" \
    ROUTER_AUTH_OUT="$out/router.auth" \
    UPLINK_MOR_ALLOWANCE_FILE="$out/router.mor-allowance.json" \
    COOKIE_CONTENT="admin:testpass" \
    UPLINK_MOR_ALLOWANCE_WEI="$override" \
    sh "$SCRIPT_PATH"; then
    echo "expected failure for override=$override" >&2
    exit 1
  fi
  test ! -e "$out/router.auth"
  test ! -e "$out/router.mor-allowance.json"
  echo "OK rejected override=$override"
  kill "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=
}
run_fail_case internal_space "1 000"
run_fail_case internal_tab $'12\t34'
run_fail_case alpha "abc"
run_fail_case exponent "1e18"
run_fail_case negative "-5"
run_fail_case hex "0x10"
echo "OK: router-user-init integration cases"
