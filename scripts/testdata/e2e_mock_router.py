"""Stateful proxy-router stand-in for the mock-router e2e (no chain, no wallet).

Implements the subset Uplink uses, with a real credential check: Basic auth
must match the router cookie (COOKIE_CONTENT). Anything else gets 401
"invalid username or password", like the real router.

Test controls (no auth, test-only): GET /e2e/stats, POST /e2e/fund?mor=N,
POST /e2e/fail?path=P&code=N (inject errors), POST /e2e/seed-session?model=M&ends_in=SEC (an open session left behind by a
previous process), GET /e2e/sessions (every session with its open/close
times and prompt count), GET /e2e/catalog (gateway model catalog, real
shape: last_updated and a healthy bidDetail row per provider),
POST /e2e/catalog-mode?m=normal|empty|braces|nodetail (nodetail: every
bidDetail empty), GET /e2e/bids (gateway bids file, real shape: CATALOG x
PROVIDERS), POST /e2e/bids-mode?m=normal|unhealthy:<provider>|empty|braces|
stale|redirect|500 (stale: the last file served again, last_updated
frozen; redirect: a 302 Uplink must not follow), POST /e2e/slow-close?sec=N,
POST /e2e/provider-fail?provider=P&code=N&msg=M (that provider answers
prompts with a provider model error; code=0 clears), GET /e2e/opens (every
open request: model, bid ("" for an open by model), body field names,
failover, provider picked). Rated rows carry the provider in mixed case,
like the router. Listens on 127.0.0.1 (MOCK_BIND) by default.
Every model has two providers (PROVIDERS, best rated first). Sessions:
list (/blockchain/sessions/user), open by model (best rated provider),
open by bid, rated bids, close, detail, and POST /v1/chat/completions with
a session_id header (open sessions only). Any request asking for direct
payment is refused and counted (stats.direct_payment_requests); an open
asking for router failover is counted (stats.failover_true_opens).

Env: MOCK_MOR (initial balance), MOCK_MODE=fail (balance 500), MOCK_BODY
(raw balance body), MOCK_WALLET (address).
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
S = {
    "mor": os.environ.get("MOCK_MOR", "0"),
    "stats": {"unauthorized": 0, "unauthorized_by_pw": {},
              "direct_payment_requests": 0,
              "session_opens": 0, "session_closes": 0, "failover_true_opens": 0},
    "fail": {},        # path -> code
    "pfail": {},       # provider -> (code, message)
    "opens": [],
    "sessions": {},
    "catalog_mode": "normal",
    "bids_mode": "normal",
    "bids_last": None,  # last bids file served (stale mode repeats it)
    "slow_close": 0,
}


CATALOG = [
    {"Name": "e2e-alpha", "Id": "0x" + "a1" * 32, "Tags": [], "ModelType": "LLM", "bidDetail": []},
    {"Name": "e2e-beta", "Id": "0x" + "b2" * 32, "Tags": [], "ModelType": "LLM", "bidDetail": []},
    {"Name": "e2e-gamma", "Id": "0x" + "c3" * 32, "Tags": [], "ModelType": "LLM", "bidDetail": []},
    {"Name": "e2e-delta", "Id": "0x" + "d4" * 32, "Tags": [], "ModelType": "LLM", "bidDetail": []},
]


PROVIDERS = ["0x" + "a0" * 20, "0x" + "b0" * 20]


def bid_id(model, provider):
    return "0x" + hashlib.sha256((model.lower() + provider).encode()).hexdigest()


def mixed(addr):
    """Mixed-case address, as the router writes providers (case is not data)."""
    return "0x" + "".join(c.upper() if i % 2 == 0 else c for i, c in enumerate(addr[2:]))


def utc(ts):
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ts))


def catalog_file():
    """Gateway models file (real shape)."""
    mode = S["catalog_mode"]
    if mode == "braces":
        return {}
    now = int(time.time())
    models = []
    if mode != "empty":
        for c in CATALOG:
            m = dict(c)
            m["bidDetail"] = [] if mode == "nodetail" else [
                {"bidId": bid_id(c["Id"], p), "providerId": p, "pricePerSecond": "1000",
                 "priceMorPerHour": 0.0036, "status": "healthy"} for p in PROVIDERS]
            models.append(m)
    return {"last_updated": now, "last_updated_utc": utc(now), "models": models}


def bids_file():
    """Gateway bids file (real shape): one row per CATALOG model x provider."""
    mode = S["bids_mode"]
    if mode == "braces":
        return {}
    if mode == "stale" and S["bids_last"] is not None:
        return S["bids_last"]
    stamp = int(time.time())
    bad = mode.split(":", 1)[1].lower() if mode.startswith("unhealthy:") else ""
    rows = []
    if mode != "empty":
        for c in CATALOG:
            for p in PROVIDERS:
                ok = p != bad
                rows.append({"Id": bid_id(c["Id"], p), "Provider": p, "ModelAgentId": c["Id"],
                             "PricePerSecond": "1000", "Nonce": "0", "CreatedAt": "1700000000",
                             "DeletedAt": "0", "ModelName": c["Name"],
                             "health": {"status": "healthy" if ok else "degraded",
                                        "errorKind": "" if ok else "timeout", "httpStatus": 200 if ok else 0,
                                        "latencyMs": 100, "promptCorrect": ok, "providerVersion": "mock",
                                        "providerPingMs": 5, "lastHealthy": stamp, "lastHealthyUTC": utc(stamp),
                                        "lastChecked": stamp, "lastCheckedUTC": utc(stamp)}})
    out = {"last_updated": stamp, "last_updated_utc": utc(stamp), "bids": rows}
    if rows:
        S["bids_last"] = out
    return out


def new_session(model, ends_at, opened_by, provider=PROVIDERS[0]):
    sid = "0x" + uuid.uuid4().hex + uuid.uuid4().hex
    S["sessions"][sid] = {"Id": sid, "ModelAgentId": model, "Stake": "1000",
                          "Provider": provider, "BidID": bid_id(model, provider),
                          "OpenedAt": int(time.time()), "EndsAt": int(ends_at),
                          "ClosedAt": 0, "OpenedBy": opened_by, "Prompts": 0,
                          "ClosedAtMono": 0}
    return sid


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
        S["stats"]["unauthorized"] += 1
        tag = pw_tag(pw)
        S["stats"]["unauthorized_by_pw"][tag] = S["stats"]["unauthorized_by_pw"].get(tag, 0) + 1
        return None

    def _deny(self):
        return self._json(401, {"error": "invalid username or password"})

    def _route(self, method):
        u = urlparse(self.path)
        path, q = u.path, parse_qs(u.query)
        if S["slow_close"] and method == "POST" and path.startswith("/blockchain/sessions/") and path.endswith("/close"):
            time.sleep(S["slow_close"])  # outside the lock: other calls proceed
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
            who = self._who()
            if who is None:
                return self._deny()
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
                keys = ("Id", "ModelAgentId", "Provider", "BidID", "Stake", "OpenedAt", "EndsAt", "ClosedAt")
                return self._json(200, {"sessions": [{k: r[k] for k in keys} for r in rows[off:off + lim]]})
            if path.startswith("/blockchain/models/") and path.endswith("/bids/rated") and method == "GET":
                model = path[len("/blockchain/models/"):-len("/bids/rated")]
                return self._json(200, {"bids": [
                    {"ID": "0x" + "0" * 64, "Bid": {"Id": bid_id(model, p), "Provider": mixed(p), "ModelAgentId": model,
                                                    "PricePerSecond": "1000", "Nonce": "0",
                                                    "CreatedAt": "1700000000", "DeletedAt": "0"},
                     "Score": 10 - i} for i, p in enumerate(PROVIDERS)]})
            if method == "POST" and path.endswith("/session") and (
                    path.startswith("/blockchain/models/") or path.startswith("/blockchain/bids/")):
                by_bid = path.startswith("/blockchain/bids/")
                ref = path.split("/")[3]
                if body.get("failover") not in (False, None, "false"):
                    S["stats"]["failover_true_opens"] += 1
                pick = model = None
                if by_bid:
                    for m in [c["Id"] for c in CATALOG] + [r["ModelAgentId"] for r in S["sessions"].values()]:
                        for p in PROVIDERS:
                            if bid_id(m, p) == ref:
                                pick, model = p, m
                else:
                    model = ref
                    pick = PROVIDERS[0] if PROVIDERS else None
                S["opens"].append({"model": model, "bid": ref if by_bid else "", "fields": sorted(body.keys()),
                                   "failover": body.get("failover"), "provider": pick})
                if pick is None:
                    return self._json(500, {"error": "no provider accepting session"})
                dur = int(body.get("sessionDuration") or 600)
                S["stats"]["session_opens"] += 1
                return self._json(200, {"sessionID": new_session(model, time.time() + dur, "uplink", pick)})
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
                keys = ("Id", "ModelAgentId", "Provider", "BidID", "Stake", "OpenedAt", "EndsAt", "ClosedAt")
                return self._json(200, {"session": {k: r[k] for k in keys}})
            if path == "/v1/chat/completions" and method == "POST":
                r = S["sessions"].get(self.headers.get("session_id", ""))
                if r is None or r["ClosedAt"] or r["EndsAt"] <= time.time():
                    return self._json(400, {"error": "session not found"})
                r["Prompts"] += 1
                if r["Provider"] in S["pfail"]:
                    code, msg = S["pfail"][r["Provider"]]
                    return self._json(code, {"providerModelError": {"error": {"message": msg}}, "statusCode": code})
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
            return self._json(200, out)
        if path == "/e2e/sessions":
            return self._json(200, {"now": time.time(), "sessions": list(S["sessions"].values())})
        if path == "/e2e/catalog":
            return self._json(200, catalog_file())
        if path == "/e2e/opens":
            return self._json(200, {"opens": S["opens"]})
        if path == "/e2e/bids":
            S["stats"]["bids_fetches"] = S["stats"].get("bids_fetches", 0) + 1
            if S["bids_mode"] == "500":
                return self._json(500, {"error": "injected"})
            if S["bids_mode"] == "redirect":
                self.send_response(302)
                self.send_header("Location", "/e2e/bids-moved")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            return self._json(200, bids_file())
        if path == "/e2e/bids-moved":  # redirect target: Uplink must not follow
            S["stats"]["bids_redirect_followed"] = S["stats"].get("bids_redirect_followed", 0) + 1
            return self._json(200, bids_file())
        self._body()
        if path == "/e2e/seed-session":
            sid = new_session(q.get("model", [""])[0], time.time() + int(q.get("ends_in", ["3600"])[0]), "seed")
            return self._json(200, {"id": sid})
        if path == "/e2e/catalog-mode":
            S["catalog_mode"] = q.get("m", ["normal"])[0]
        elif path == "/e2e/bids-mode":
            S["bids_mode"] = q.get("m", ["normal"])[0]
        elif path == "/e2e/slow-close":
            S["slow_close"] = int(q.get("sec", ["0"])[0])
        elif path == "/e2e/provider-fail":
            p, code = q.get("provider", [""])[0].lower(), int(q.get("code", ["0"])[0])
            if code:
                S["pfail"][p] = (code, q.get("msg", ["provider error"])[0])
            else:
                S["pfail"].pop(p, None)
        elif path == "/e2e/fund":
            S["mor"] = q.get("mor", ["0"])[0]
        elif path == "/e2e/fail":
            p, code = q.get("path", [""])[0], int(q.get("code", ["0"])[0])
            if code:
                S["fail"][p] = code
            else:
                S["fail"].pop(p, None)
        elif path == "/e2e/reset-stats":
            S["stats"] = {"unauthorized": 0, "unauthorized_by_pw": {},
                          "direct_payment_requests": 0,
                          "session_opens": 0, "session_closes": 0, "failover_true_opens": 0}
        return self._json(200, {"ok": True})

    def do_GET(self):
        self._route("GET")

    def do_POST(self):
        self._route("POST")

    def do_DELETE(self):
        self._route("DELETE")


ThreadingHTTPServer((os.environ.get("MOCK_BIND", "127.0.0.1"), int(sys.argv[1])), H).serve_forever()
