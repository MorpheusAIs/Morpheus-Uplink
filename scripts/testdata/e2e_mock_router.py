"""Minimal proxy-router stand-in for the SecretVM handoff e2e.

Answers /healthcheck, /blockchain/balance (Basic auth) and the scoped-user
endpoints used by router-user-init. Everything else returns 200 {} so Uplink
background loops stay quiet. No real chain, no wallet.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MOR = os.environ.get("MOCK_MOR", "0")


class H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *a):
        sys.stderr.write("mock-router: " + (fmt % a) + "\n")

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _drain(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)

    def do_GET(self):
        if self.path == "/healthcheck":
            return self._json(200, {"status": "ok"})
        if self.path == "/blockchain/balance":
            if not self.headers.get("Authorization", "").startswith("Basic "):
                return self._json(401, {"error": "unauthorized"})
            return self._json(200, {"eth": "0", "mor": MOR})
        return self._json(200, {})

    def do_POST(self):
        self._drain()
        return self._json(200, {})

    def do_DELETE(self):
        self._drain()
        return self._json(200, {})


ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
