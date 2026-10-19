#!/usr/bin/env bash
# Policy checks for the allowance manager across every compose template.
#  C   only router-user-init (rw) and router-allowance-manager (ro) mount
#      manager_cred; nothing else, in any template.
#  T6  the scoped user's permission list never includes admin/transfer perms.
#  T8  manager: internal-only networks, no ports, cap_drop ALL with no
#      cap_add, read-only rootfs, uid 10002, same image as uplink, never
#      router_auth, no docker socket anywhere; proxy-router publishes nothing.
#  T9  manager and init use the same UPLINK_MOR_ALLOWANCE_WEI expression;
#      UPLINK_MOR_ALLOWANCE_MODE (ceiling opt-in) never appears in the
#      SecretVM or generic templates.
#  B   Uplink's /ctl and /status mounts are never under DATA_DIR.
# Self-test: each rule must reject a deliberately broken render.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export ADMIN_PASSWORD='test-admin-pass-ok-xx' API_KEY_SEED='test-seed-value-xx'
export COOKIE_CONTENT='admin:test-cookie-pass-xx' WALLET_PRIVATE_KEY='0x'$(printf 'a%.0s' {1..64})
export ETH_NODE_ADDRESS='https://example.invalid' WEB_PUBLIC_URL='https://uplink.example'
export UPLINK_MOR_ALLOWANCE_WEI='' ROUTER_AUTH='uplink:scoped-pass-xx' PUBLIC_HOST='uplink.example'
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

FORBIDDEN='request_allowance agent_requests approve send_mor send_eth add_user remove_user'
for f in "$ROOT"/deploy/init/router-user-init.sh "$ROOT"/deploy/init/uplink-with-router-init.sh; do
  perms=$(grep -E "^[[:space:]]*PERMS=" "$f" || true)
  [ -n "$perms" ] || continue
  for p in $FORBIDDEN '\*'; do
    if printf '%s' "$perms" | grep -E "\"${p}\"" >/dev/null; then
      echo "BAD(T6): $f grants forbidden perm $p" >&2; exit 1
    fi
  done
done
grep -E '^[[:space:]]*PERMS=' "$ROOT/deploy/init/router-user-init.sh" >/dev/null || { echo "BAD(T6): PERMS not found" >&2; exit 1; }
echo "OK(T6): scoped perms exclude admin/transfer permissions"

cat > "$tmp/check.py" <<'PY'
import json, re, sys
name, rendered, raw_path, kind = sys.argv[1:5]
d = json.load(open(rendered))
raw = open(raw_path).read()
svcs = d.get("services") or {}
nets = d.get("networks") or {}
bad = []
INIT, MGR, UP, PR = "router-user-init", "router-allowance-manager", "uplink", "proxy-router"

def vols(s):
    return [v for v in (svcs[s].get("volumes") or []) if isinstance(v, dict)]

def env(s):
    e = svcs[s].get("environment") or {}
    return e if isinstance(e, dict) else {}

for s in svcs:
    for v in vols(s):
        if v.get("source") == "manager_cred":
            ro = bool(v.get("read_only"))
            if s == INIT and not ro: continue
            if s == MGR and ro: continue
            bad.append("C: %s mounts manager_cred (%s)" % (s, "ro" if ro else "rw"))
        if "docker.sock" in str(v.get("source", "")) or "docker.sock" in str(v.get("target", "")):
            bad.append("T8: %s mounts the docker socket" % s)

if kind == "manager":
    if MGR not in svcs:
        bad.append("T8: %s has no %s service" % (name, MGR))
    else:
        m = svcs[MGR]
        if m.get("ports"): bad.append("T8: manager publishes ports")
        if [c for c in (m.get("cap_drop") or [])] != ["ALL"]: bad.append("T8: manager must cap_drop ALL")
        if m.get("cap_add"): bad.append("T8: manager must not cap_add")
        if not m.get("read_only"): bad.append("T8: manager rootfs must be read_only")
        if str(m.get("user")) != "10002:10002": bad.append("T8: manager user must be 10002:10002")
        if m.get("image") != svcs[UP].get("image"): bad.append("T8: manager image differs from uplink image")
        if m.get("privileged"): bad.append("T8: manager privileged")
        for v in vols(MGR):
            if v.get("source") == "router_auth": bad.append("T8: manager mounts router_auth")
        mn = list((m.get("networks") or {}).keys())
        if not mn: bad.append("T8: manager has no explicit network")
        for n in mn:
            if not (nets.get(n) or {}).get("internal"):
                bad.append("T8: manager network %s is not internal" % n)
        if m.get("network_mode"): bad.append("T8: manager sets network_mode")
        if PR in svcs and svcs[PR].get("ports"): bad.append("T8: proxy-router publishes ports")
        # T9: same raw expression for init and manager.
        def expr(svc):
            blk = re.search(r"\n  %s:\n(.*?)(?=\n  [A-Za-z0-9_-]+:\n|\nvolumes:|\Z)" % re.escape(svc), raw, re.S)
            if not blk: return None
            m2 = re.findall(r"UPLINK_MOR_ALLOWANCE_WEI=(\S+)", blk.group(1))
            return m2
        ei, em = expr(INIT), expr(MGR)
        if not ei or not em or ei != em:
            bad.append("T9: WEI expression differs init=%s manager=%s" % (ei, em))
        if "UPLINK_MOR_ALLOWANCE_MODE" in raw:
            bad.append("T9: UPLINK_MOR_ALLOWANCE_MODE must not appear in %s" % name)

# B: /ctl and /status never under DATA_DIR (uplink).
if UP in svcs:
    data_dir = env(UP).get("DATA_DIR")
    if not data_dir:
        mm = re.search(r"^\s*DATA_DIR=(\S+)", raw, re.M)
        data_dir = mm.group(1) if mm else "/data"
    dd = data_dir.rstrip("/") + "/"
    for v in vols(UP):
        t = str(v.get("target", ""))
        if t in ("/ctl", "/status") or "allowance" in str(v.get("source", "")):
            if (t.rstrip("/") + "/").startswith(dd):
                bad.append("B: uplink mount %s is under DATA_DIR %s" % (t, data_dir))
    for k in ("UPLINK_ALLOWANCE_STATUS_FILE", "UPLINK_ALLOWANCE_CTL_DIR"):
        val = env(UP).get(k)
        if not val:
            mm = re.search(r"^\s*-?\s*%s=(\S+)" % k, raw, re.M)
            val = mm.group(1) if mm else None
        if val and (val.rstrip("/") + "/").startswith(dd):
            bad.append("B: %s=%s is under DATA_DIR" % (k, val))

if bad:
    for b in bad: print("BAD(%s): %s" % (name, b), file=sys.stderr)
    sys.exit(1)
print("OK(%s): C/T8/T9/B" % name)
PY

render() { (cd "$(dirname "$1")" && docker compose -f "$(basename "$1")" config --format json) > "$2"; }
check() { python3 "$tmp/check.py" "$@"; }

for t in deploy/secretvm/docker-compose.yml:manager deploy/generic/docker-compose.yml:manager \
         deploy/railway/docker-compose.yml:none docker-compose.local.yml:none; do
  f="${t%%:*}"; kind="${t##*:}"
  render "$ROOT/$f" "$tmp/r.json"
  check "$f" "$tmp/r.json" "$ROOT/$f" "$kind"
done

# Self-test: every rule must catch a broken variant of the SecretVM render.
render "$ROOT/deploy/secretvm/docker-compose.yml" "$tmp/base.json"
mutate() {
  python3 - "$tmp/base.json" "$tmp/bad.json" "$1" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); s = d["services"]; m = sys.argv[3]
M = "router-allowance-manager"
if m == "uplink_cred": s["uplink"]["volumes"].append({"type": "volume", "source": "manager_cred", "target": "/mgr", "read_only": True})
if m == "mgr_rw":
    for v in s[M]["volumes"]:
        if v["source"] == "manager_cred": v["read_only"] = False
if m == "mgr_net": s[M]["networks"] = {"web": None}
if m == "mgr_cap": s[M]["cap_add"] = ["NET_RAW"]
if m == "mgr_image": s[M]["image"] = "example/other:1"
if m == "mgr_sock": s[M]["volumes"].append({"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"})
if m == "mgr_auth": s[M]["volumes"].append({"type": "volume", "source": "router_auth", "target": "/shared", "read_only": True})
if m == "router_ports": s["proxy-router"]["ports"] = [{"target": 9999, "published": "9999"}]
if m == "ctl_in_data":
    for v in s["uplink"]["volumes"]:
        if v["target"] == "/ctl": v["target"] = "/data/ctl"
json.dump(d, open(sys.argv[2], "w"))
PY
}
for m in uplink_cred mgr_rw mgr_net mgr_cap mgr_image mgr_sock mgr_auth router_ports ctl_in_data; do
  mutate "$m"
  if check "selftest-$m" "$tmp/bad.json" "$ROOT/deploy/secretvm/docker-compose.yml" manager 2>/dev/null; then
    echo "BAD: self-test $m was not rejected" >&2; exit 1
  fi
done
# T9 self-test on raw text.
sed 's/UPLINK_MOR_ALLOWANCE_WEI=\${UPLINK_MOR_ALLOWANCE_WEI}$/&/' "$ROOT/deploy/secretvm/docker-compose.yml" > "$tmp/raw1.yml"
printf '\n# UPLINK_MOR_ALLOWANCE_MODE=ceiling\n' >> "$tmp/raw1.yml"
if check selftest-mode "$tmp/base.json" "$tmp/raw1.yml" manager 2>/dev/null; then echo "BAD: self-test mode not rejected" >&2; exit 1; fi
python3 - "$ROOT/deploy/secretvm/docker-compose.yml" "$tmp/raw2.yml" <<'PY'
import sys
s = open(sys.argv[1]).read()
i = s.index("  router-allowance-manager:")
j = s.index("UPLINK_MOR_ALLOWANCE_WEI=${UPLINK_MOR_ALLOWANCE_WEI}", i)
s = s[:j] + "UPLINK_MOR_ALLOWANCE_WEI=${UPLINK_MOR_ALLOWANCE_WEI:-}" + s[j + len("UPLINK_MOR_ALLOWANCE_WEI=${UPLINK_MOR_ALLOWANCE_WEI}"):]
open(sys.argv[2], "w").write(s)
PY
if check selftest-expr "$tmp/base.json" "$tmp/raw2.yml" manager 2>/dev/null; then echo "BAD: self-test expr not rejected" >&2; exit 1; fi
echo "OK: allowance manager policy (self-tests reject 11 broken variants)"
