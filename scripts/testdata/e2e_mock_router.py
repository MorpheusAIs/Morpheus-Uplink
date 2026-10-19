"""Stateful proxy-router stand-in for the init/e2e tests (no chain, no wallet).

Implements the subset Uplink, router-user-init and the allowance manager use,
with real credential checks: Basic auth must match the admin cookie
(COOKIE_CONTENT) or a confirmed agent user's CURRENT password. Anything else
gets 401 "invalid username or password", like the real router.

Admin-only: /auth/users (GET, DELETE), /auth/users/confirm,
/auth/allowance/requests, /auth/allowance/confirm. /auth/users/request is
unauthenticated (as upstream). Everything else needs any valid user.

Test controls (no auth, test-only): GET /e2e/stats, POST /e2e/fund?mor=N,
POST /e2e/forget-users, POST /e2e/fail?path=P&code=N (inject errors),
POST /e2e/readback-skew?on=1 (make GET /auth/users report a wrong value),
POST /e2e/consume?wei=N (simulate session holds lowering the allowance),
POST /e2e/seed-session?model=M&ends_in=SEC (an open session left behind by a
previous process), GET /e2e/sessions (every session with its open/close
times and prompt count), GET /e2e/catalog (gateway model catalog).
Sessions: list (/blockchain/sessions/user), open, close, detail, and
POST /v1/chat/completions with a session_id header (open sessions only).
Any request asking for direct payment is refused and counted
(stats.direct_payment_requests).

Env: MOCK_MOR (initial balance), MOCK_MODE=fail (balance 500), MOCK_BODY
(raw balance body), MOCK_WALLET (address), MOCK_STATE (json file to persist
users across restarts; absent = forget on restart).
"""
import base64
import hashlib
import json
import os
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

ADMIN = os.environ.get("COOKIE_CONTENT", "admin:mockpass")
LOCK = threading.Lock()
STATE_FILE = os.environ.get("MOCK_STATE", "")
S = {
    "mor": os.environ.get("MOCK_MOR", "0"),
    "users": {},       # name -> {"password", "perms", "allowances", "confirmed"}
    "pending": {},     # name -> request
    "allow_req": {},   # "name:token" -> allowance
    "stats": {"unauthorized": 0, "unauthorized_by_pw": {}, "ok_by_pw": {},
              "allowance_confirms": 0, "user_confirms": 0, "direct_payment_requests": 0,
              "session_opens": 0, "session_closes": 0},
    "fail": {},        # path -> code
    "sessions": {},    # id -> session row (in memory; survives uplink restarts)
    "skew": False,
}
if STATE_FILE and os.path.exists(STATE_FILE):
    with open(STATE_FILE) as f:
        S["users"] = json.load(f).get("users", {})


CATALOG = [
    {"Name": "e2e-alpha", "Id": "0x" + "a1" * 32, "Tags": [], "ModelType": "LLM", "bidDetail": []},
    {"Name": "e2e-beta", "Id": "0x" + "b2" * 32, "Tags": [], "ModelType": "LLM", "bidDetail": []},
]


def new_session(model, ends_at, opened_by):
    sid = "0x" + uuid.uuid4().hex + uuid.uuid4().hex
    S["sessions"][sid] = {"Id": sid, "ModelAgentId": model, "Stake": "1000",
                          "OpenedAt": int(time.time()), "EndsAt": int(ends_at),
                          "ClosedAt": 0, "OpenedBy": opened_by, "Prompts": 0,
                          "ClosedAtMono": 0}
    return sid


def persist():
    if STATE_FILE:
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump({"users": S["users"]}, f)
        os.replace(tmp, STATE_FILE)


def pw_tag(pw):
    return hashlib.sha256(pw.encode()).hexdigest()[:12]


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *a):
        sys.stderr.write("mock-router: " + (fmt % a) + "\n")

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        try:
            return json.loads(raw or b"{}")
        except ValueError:
            return {}

    def _who(self):
        h = self.headers.get("Authorization", "")
        if not h.startswith("Basic "):
            return None
        try:
            user, _, pw = base64.b64decode(h[6:]).decode().partition(":")
        except Exception:
            return None
        if user + ":" + pw == ADMIN:
            return "admin"
        u = S["users"].get(user)
        if u and u.get("confirmed") and u["password"] == pw:
            S["stats"]["ok_by_pw"][pw_tag(pw)] = S["stats"]["ok_by_pw"].get(pw_tag(pw), 0) + 1
            return user
        S["stats"]["unauthorized"] += 1
        tag = pw_tag(pw)
        S["stats"]["unauthorized_by_pw"][tag] = S["stats"]["unauthorized_by_pw"].get(tag, 0) + 1
        return None

    def _deny(self):
        return self._json(401, {"error": "invalid username or password"})

    def _route(self, method):
        u = urlparse(self.path)
        path, q = u.path, parse_qs(u.query)
        with LOCK:
            if path.startswith("/e2e/"):
                return self._control(method, path, q)
            if path in S["fail"]:
                self._body()
                return self._json(S["fail"][path], {"error": "injected"})
            if path == "/healthcheck":
                return self._json(200, {"status": "healthy", "version": "mock"})
            body = self._body() if method in ("POST", "DELETE") else {}
            if isinstance(body, dict) and any(
                    k.lower().replace("_", "") in ("directpayment", "isdirectpaymentfromuser")
                    and v not in (False, None, "false") for k, v in body.items()):
                S["stats"]["direct_payment_requests"] += 1
                return self._json(400, {"error": "direct payment refused by e2e mock"})
            if path == "/auth/users/request" and method == "POST":
                S["pending"][body.get("username", "")] = body
                return self._json(200, {"result": True})
            who = self._who()
            if who is None:
                return self._deny()
            admin_only = path in ("/auth/users", "/auth/users/confirm",
                                  "/auth/allowance/requests", "/auth/allowance/confirm")
            if admin_only and who != "admin":
                return self._json(403, {"error": "forbidden"})
            if path == "/auth/users" and method == "GET":
                agents = []
                for name, x in S["users"].items():
                    al = dict(x.get("allowances", {}))
                    if S["skew"]:
                        al = {k: str(int(v) + 1) for k, v in al.items()}
                    agents.append({"username": name, "perms": x.get("perms", []),
                                   "isConfirmed": x.get("confirmed", False), "allowances": al})
                return self._json(200, {"agents": agents})
            if path == "/auth/users" and method == "DELETE":
                S["users"].pop(body.get("username", ""), None)
                persist()
                return self._json(200, {"result": True})
            if path == "/auth/users/confirm":
                req = S["pending"].pop(body.get("username", ""), None)
                if not req:
                    return self._json(400, {"error": "no request"})
                S["users"][req["username"]] = {
                    "password": req["password"], "perms": req.get("perms", []),
                    "allowances": {k: str(v) for k, v in (req.get("allowances") or {}).items()},
                    "confirmed": True}
                S["stats"]["user_confirms"] += 1
                persist()
                return self._json(200, {"result": True})
            if path == "/auth/allowance/requests":
                S["allow_req"][body.get("username", "") + ":" + body.get("token", "")] = str(body.get("allowance", ""))
                return self._json(200, {"result": True})
            if path == "/auth/allowance/confirm":
                key = body.get("username", "") + ":" + body.get("token", "")
                val = S["allow_req"].pop(key, None)
                usr = S["users"].get(body.get("username", ""))
                if val is None or usr is None:
                    return self._json(500, {"error": "no request"})
                usr.setdefault("allowances", {})[body.get("token", "")] = val
                S["stats"]["allowance_confirms"] += 1
                persist()
                return self._json(200, {"result": True})
            if path == "/blockchain/balance":
                if os.environ.get("MOCK_MODE") == "fail":
                    return self._json(500, {"error": "rpc"})
                raw = os.environ.get("MOCK_BODY")
                if raw:
                    data = raw.encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(data)))
                    self.end_headers()
                    self.wfile.write(data)
                    return
                return self._json(200, {"eth": "1000000000000000000", "mor": S["mor"]})
            if path == "/wallet":
                return self._json(200, {"address": os.environ.get("MOCK_WALLET", "0x" + "ab" * 20)})
            if path == "/blockchain/sessions/user":
                off = int(q.get("offset", ["0"])[0]); lim = int(q.get("limit", ["100"])[0])
                rows = sorted(S["sessions"].values(), key=lambda x: x["OpenedAt"], reverse=True)
                keys = ("Id", "ModelAgentId", "Stake", "OpenedAt", "EndsAt", "ClosedAt")
                return self._json(200, {"sessions": [{k: r[k] for k in keys} for r in rows[off:off + lim]]})
            if path.startswith("/blockchain/models/") and path.endswith("/session") and method == "POST":
                model = path[len("/blockchain/models/"):-len("/session")]
                dur = int(body.get("sessionDuration") or 600)
                S["stats"]["session_opens"] += 1
                return self._json(200, {"sessionID": new_session(model, time.time() + dur, "uplink")})
            if path.startswith("/blockchain/sessions/") and path.endswith("/close") and method == "POST":
                r = S["sessions"].get(path.split("/")[3])
                if r is None:
                    return self._json(404, {"error": "session not found"})
                if not r["ClosedAt"]:
                    r["ClosedAt"] = int(time.time())
                    r["ClosedAtMono"] = time.time()
                    S["stats"]["session_closes"] += 1
                return self._json(200, {"result": True})
            if path.startswith("/blockchain/sessions/0x") and method == "GET":
                r = S["sessions"].get(path.split("/")[3])
                if r is None:
                    return self._json(404, {"error": "session not found"})
                keys = ("Id", "ModelAgentId", "Stake", "OpenedAt", "EndsAt", "ClosedAt")
                return self._json(200, {"session": {k: r[k] for k in keys}})
            if path == "/v1/chat/completions" and method == "POST":
                r = S["sessions"].get(self.headers.get("session_id", ""))
                if r is None or r["ClosedAt"] or r["EndsAt"] <= time.time():
                    return self._json(400, {"error": "session not found"})
                r["Prompts"] += 1
                return self._json(200, {"id": "chatcmpl-e2e", "object": "chat.completion",
                                        "model": body.get("model", ""),
                                        "choices": [{"index": 0, "finish_reason": "stop",
                                                     "message": {"role": "assistant", "content": "ok"}}],
                                        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
            if path == "/blockchain/stakes/onhold":
                return self._json(200, {"available": "0", "hold": "0"})
            return self._json(200, {})

    def _control(self, method, path, q):
        if path == "/e2e/stats":
            out = dict(S["stats"])
            out["mor"] = S["mor"]
            out["users"] = {n: {"pw": pw_tag(x["password"]), "allowances": x.get("allowances", {})}
                            for n, x in S["users"].items()}
            return self._json(200, out)
        if path == "/e2e/sessions":
            return self._json(200, {"now": time.time(), "sessions": list(S["sessions"].values())})
        if path == "/e2e/catalog":
            return self._json(200, {"models": CATALOG})
        if path == "/e2e/bids":
            return self._json(200, {})
        self._body()
        if path == "/e2e/seed-session":
            sid = new_session(q.get("model", [""])[0], time.time() + int(q.get("ends_in", ["3600"])[0]), "seed")
            return self._json(200, {"id": sid})
        if path == "/e2e/fund":
            S["mor"] = q.get("mor", ["0"])[0]
        elif path == "/e2e/consume":
            # Simulate session holds: lower every user's allowance by wei.
            n = int(q.get("wei", ["0"])[0])
            for x in S["users"].values():
                for t, v in list(x.get("allowances", {}).items()):
                    x["allowances"][t] = str(max(0, int(v) - n))
        elif path == "/e2e/forget-users":
            S["users"].clear()
            persist()
        elif path == "/e2e/fail":
            p, code = q.get("path", [""])[0], int(q.get("code", ["0"])[0])
            if code:
                S["fail"][p] = code
            else:
                S["fail"].pop(p, None)
        elif path == "/e2e/readback-skew":
            S["skew"] = q.get("on", ["0"])[0] == "1"
        elif path == "/e2e/reset-stats":
            S["stats"] = {"unauthorized": 0, "unauthorized_by_pw": {}, "ok_by_pw": {},
                          "allowance_confirms": 0, "user_confirms": 0, "direct_payment_requests": 0,
                          "session_opens": 0, "session_closes": 0}
        return self._json(200, {"ok": True})

    def do_GET(self):
        self._route("GET")

    def do_POST(self):
        self._route("POST")

    def do_DELETE(self):
        self._route("DELETE")


ThreadingHTTPServer((os.environ.get("MOCK_BIND", "0.0.0.0"), int(sys.argv[1])), H).serve_forever()
