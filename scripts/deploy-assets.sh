#!/usr/bin/env bash
# Digest-pinned deploy assets: one script for the release step and the PR check.
#
#   deploy-assets.sh generate IMAGE DIGEST OUTDIR   write the release assets
#   deploy-assets.sh check    IMAGE DIGEST OUTDIR   check generated assets + templates
#   deploy-assets.sh pr                             generate with a fake digest, check,
#                                                   then run the self-tests
#   deploy-assets.sh --self-test                    every failure mode must fire
#
# Assets (template -> asset):
#   deploy/secretvm/docker-compose.yml -> docker-compose.secretvm.deployed.yml
#   deploy/secretvm/env.template       -> env.secretvm.example
#   deploy/generic/docker-compose.yml  -> docker-compose.generic.deployed.yml
#   deploy/generic/env.example         -> env.generic.example
#   deploy/railway/docker-compose.yml  -> docker-compose.railway.deployed.yml
#   deploy/railway/env.example         -> env.railway.example
# docker-compose.local.yml + .env.example are checked too (not published).
#
# Checks:
#   P  every image: in a generated compose is pinned (@sha256:<64 hex>), none
#      is :latest, and the Uplink image is exactly IMAGE@DIGEST.
#   T  no TODO or REPLACE in generated assets or their templates.
#   V  ${...} variables per template: SecretVM uses exactly the six secrets
#      and has no other environment: key, literal or not (SecretVM makes a
#      form field of each; tunables live in its configs: block); generic, Railway and local use
#      their required variables plus the documented overrides below, each
#      override with a default (${VAR:-default}).
#   E  each env example matches its template: every key (set or commented)
#      is a template variable, every required variable is set, no
#      duplicates; env.secretvm.example sets exactly the six secrets.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SELF="$ROOT/scripts/deploy-assets.sh"

py() {
python3 - "$@" <<'PY'
import os, re, sys

SIX = ["WALLET_PRIVATE_KEY", "ETH_NODE_ADDRESS", "COOKIE_CONTENT",
       "ADMIN_PASSWORD", "API_KEY_SEED", "WEB_PUBLIC_URL"]
# Documented env-overridable defaults (docs/06-developers.md, deploy/*/README.md).
OVERRIDES = {
    "SESSION_DURATION_SECONDS", "SESSION_FAILOVER", "ACTIVE_MODELS_URL",
    "GATEWAY_BIDS_URL", "HOUSEKEEPING", "ADMIN_2FA_REQUIRED", "MASTER_KEY_ADMIN",
    "UPLINK_RESET_2FA", "ETH_NODE_CHAIN_ID", "ETH_NODE_USE_SUBSCRIPTIONS",
    "BLOCKSCOUT_API_URL", "DIAMOND_CONTRACT_ADDRESS", "MOR_TOKEN_ADDRESS",
    "PROXY_STORE_CHAT_CONTEXT", "PROXY_FORWARD_CHAT_CONTEXT", "LOG_LEVEL_APP",
    "LOG_LEVEL_TCP", "LOG_LEVEL_ETH_RPC",
}
# name: (template, env source, compose asset, env asset, required, overrides)
SETS = {
    "secretvm": ("deploy/secretvm/docker-compose.yml", "deploy/secretvm/env.template",
                 "docker-compose.secretvm.deployed.yml", "env.secretvm.example", set(SIX), set()),
    "generic": ("deploy/generic/docker-compose.yml", "deploy/generic/env.example",
                "docker-compose.generic.deployed.yml", "env.generic.example", set(SIX) | {"PUBLIC_HOST"}, OVERRIDES),
    "railway": ("deploy/railway/docker-compose.yml", "deploy/railway/env.example",
                "docker-compose.railway.deployed.yml", "env.railway.example", set(SIX), OVERRIDES),
    "local": ("docker-compose.local.yml", ".env.example", None, None,
              {"WALLET_PRIVATE_KEY", "ETH_NODE_ADDRESS", "ROUTER_AUTH", "ADMIN_PASSWORD", "API_KEY_SEED"}, OVERRIDES),
}
UPLINK_LINE = re.compile(r"^(\s*)image:\s*ghcr\.io/morpheusais/uplink(?:[:@]\S*)?(?:\s+#.*)?$", re.M)
VAR = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?+])?")
IMAGE = re.compile(r"^\s*image:\s*(\S+)", re.M)
PINNED = re.compile(r"^\S+@sha256:[0-9a-f]{64}$")

ENV_ITEM = re.compile(r"^\s+-\s*([A-Za-z_][A-Za-z0-9_]*)=", re.M)

def read(p):
    with open(p) as f:
        return f.read()

def env_keys(text):
    active, commented, dups = [], [], []
    seen = set()
    for line in text.splitlines():
        m = re.match(r"^([A-Z_][A-Z0-9_]*)=", line)
        c = re.match(r"^#\s*([A-Z_][A-Z0-9_]*)=", line)
        k = (m or c).group(1) if (m or c) else None
        if not k:
            continue
        (active if m else commented).append(k)
        if k in seen:
            dups.append(k)
        seen.add(k)
    return active, commented, dups

def template_vars(text):
    required, optional = set(), set()
    for name, op in VAR.findall(text):
        (optional if op == ":-" or op == "-" else required).add(name)
    return required, optional - required

def generate(image, digest, root, out):
    os.makedirs(out, exist_ok=True)
    for name, (tpl, env, c_out, e_out, _, _) in SETS.items():
        if not c_out:
            continue
        src = read(os.path.join(root, tpl))
        n = len(UPLINK_LINE.findall(src))
        if n != 1:
            sys.exit("generate: %s has %d Uplink image lines, want 1" % (tpl, n))
        pinned = UPLINK_LINE.sub(lambda m: m.group(1) + "image: " + image + "@" + digest, src)
        with open(os.path.join(out, c_out), "w") as f:
            f.write(pinned)
        with open(os.path.join(out, e_out), "w") as f:
            f.write(read(os.path.join(root, env)))
    print("generated deploy assets in %s (%s@%s)" % (out, image, digest))

def check(image, digest, root, out):
    bad = []
    for name, (tpl, env, c_out, e_out, req, opt) in SETS.items():
        tsrc = read(os.path.join(root, tpl))
        esrc = read(os.path.join(root, env))
        files = [(tpl, tsrc), (env, esrc)]
        compose, envtext = tsrc, esrc
        if c_out:
            compose = read(os.path.join(out, c_out))
            envtext = read(os.path.join(out, e_out))
            files += [(c_out, compose), (e_out, envtext)]
            if envtext != esrc:
                bad.append("E(%s): %s differs from %s" % (name, e_out, env))
        # T
        for fname, text in files:
            for word in ("TODO", "REPLACE"):
                if word in text:
                    bad.append("T(%s): %s contains %s" % (name, fname, word))
        # P (published assets; local: every image: line it has)
        if c_out or name == "local":
            for img in IMAGE.findall(compose):
                if img.endswith(":latest") or ":latest@" in img:
                    bad.append("P(%s): image %s is :latest" % (name, img))
                elif not PINNED.match(img):
                    bad.append("P(%s): image %s is not digest-pinned" % (name, img))
        if c_out:
            ups = [i for i in IMAGE.findall(compose) if i.startswith("ghcr.io/morpheusais/uplink")]
            if ups != [image + "@" + digest]:
                bad.append("P(%s): Uplink image %s, want %s@%s" % (name, ups, image, digest))
        # V
        r, o = template_vars(compose)
        for v in sorted(r - req):
            bad.append("V(%s): ${%s} without a default is not one of the required variables" % (name, v))
        for v in sorted(o - opt - req):
            bad.append("V(%s): ${%s:-...} is not a documented override" % (name, v))
        if name == "secretvm" and (r | o) != set(SIX):
            bad.append("V(secretvm): variables %s, want exactly the six secrets" % sorted(r | o))
        if name == "secretvm":
            # SecretVM turns every environment: key into a form field, literal
            # values included, so non-secrets belong in the configs: block.
            for k in sorted(set(ENV_ITEM.findall(compose)) - set(SIX)):
                bad.append("V(secretvm): environment key %s is not one of the six secrets (use configs:)" % k)
        for v in sorted(req - r - o):
            bad.append("V(%s): required variable %s is not used" % (name, v))
        # E
        active, commented, dups = env_keys(envtext)
        used = r | o
        for k in dups:
            bad.append("E(%s): %s listed twice in %s" % (name, k, env))
        for k in active + commented:
            if k not in used:
                bad.append("E(%s): %s in %s is not a variable of %s" % (name, k, env, tpl))
        for k in sorted(r):
            if k not in active:
                bad.append("E(%s): required %s is not set in %s" % (name, k, env))
        if name == "secretvm" and (sorted(active) != sorted(SIX) or commented):
            bad.append("E(secretvm): %s must set exactly the six secrets (set %s, commented %s)" % (env, active, commented))
    if bad:
        for b in bad:
            print("BAD " + b, file=sys.stderr)
        sys.exit(1)
    print("OK: deploy assets P/T/V/E (%s)" % ", ".join(n for n in SETS))

cmd = sys.argv[1]
if cmd == "generate":
    generate(sys.argv[2], sys.argv[3], sys.argv[5], sys.argv[4])
elif cmd == "check":
    check(sys.argv[2], sys.argv[3], sys.argv[5], sys.argv[4])
PY
}

FAKE_IMAGE="ghcr.io/morpheusais/uplink"
FAKE_DIGEST="sha256:$(printf '0%.0s' {1..64})"

self_test() {
  local tmp n=0
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  copy_root() {
    rm -rf "$tmp/root" "$tmp/out"; mkdir -p "$tmp/root/deploy" "$tmp/out"
    cp -r "$ROOT/deploy/secretvm" "$ROOT/deploy/generic" "$ROOT/deploy/railway" "$tmp/root/deploy/"
    cp "$ROOT/docker-compose.local.yml" "$ROOT/.env.example" "$tmp/root/"
    py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null
  }
  expect_fail() { # NAME RULE-REGEX: the check must fail and name the rule
    local name="$1" want="$2" err
    if err=$(py check "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" 2>&1); then
      echo "self-test $name: check passed, want failure ($want)" >&2; exit 1
    fi
    printf '%s' "$err" | grep -qE "$want" || { echo "self-test $name: wrong failure: $err" >&2; exit 1; }
    n=$((n + 1))
  }
  edit() { python3 - "$@" <<'PY'
import re, sys
path, pat, rep = sys.argv[1:4]
s = open(path).read(); t = re.sub(pat, rep, s, count=1, flags=re.M)
assert t != s, "self-test edit did not apply to " + path
open(path, "w").write(t)
PY
  }
  copy_root
  py check "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null || { echo "self-test: clean assets fail" >&2; exit 1; }

  copy_root; edit "$tmp/out/docker-compose.secretvm.deployed.yml" '^(services:)' '\1 # TODO digest'; expect_fail todo 'T\(secretvm\).*TODO'
  copy_root; edit "$tmp/out/docker-compose.generic.deployed.yml" '^(services:)' '\1 # REPLACE_AT_RELEASE'; expect_fail replace 'T\(generic\).*REPLACE'
  copy_root; edit "$tmp/root/deploy/railway/docker-compose.yml" '^(services:)' '\1 # TODO later'; expect_fail template-todo 'T\(railway\).*docker-compose.yml contains TODO'
  copy_root; edit "$tmp/out/docker-compose.secretvm.deployed.yml" 'image: ghcr.io/morpheusais/uplink@sha256:0+' 'image: ghcr.io/morpheusais/uplink:latest'; expect_fail uplink-latest 'P\(secretvm\).*:latest'
  copy_root; edit "$tmp/out/docker-compose.secretvm.deployed.yml" '(image: traefik:[^@\s]+)@sha256:[0-9a-f]+' '\1'; expect_fail unpinned 'P\(secretvm\).*not digest-pinned'
  copy_root; edit "$tmp/out/docker-compose.generic.deployed.yml" 'uplink@sha256:0+' 'uplink@sha256:'"$(printf '1%.0s' {1..64})"; expect_fail digest-mismatch 'P\(generic\): Uplink image'
  copy_root; edit "$tmp/root/deploy/secretvm/docker-compose.yml" '(- ADMIN_PASSWORD=\$\{ADMIN_PASSWORD\})' '\1\n      - ADMIN_2FA_REQUIRED=${ADMIN_2FA_REQUIRED:-true}'; py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null; expect_fail secretvm-extra-var 'V\(secretvm\)'
  copy_root; edit "$tmp/root/deploy/secretvm/docker-compose.yml" '\$\{WEB_PUBLIC_URL\}' 'https://x.example'; edit "$tmp/root/deploy/secretvm/docker-compose.yml" '\$\{WEB_PUBLIC_URL\}' 'https://x.example'; py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null; expect_fail secretvm-missing-secret 'V\(secretvm\): variables'
  copy_root; edit "$tmp/root/deploy/generic/docker-compose.yml" '(- DATA_DIR=/data)' '\1\n      - SURPRISE=${SURPRISE_KNOB:-1}'; py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null; expect_fail undocumented-override 'V\(generic\).*SURPRISE_KNOB'
  copy_root; printf '# NOT_IN_TEMPLATE=1\n' >> "$tmp/root/deploy/railway/env.example"; py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null; expect_fail env-extra-key 'E\(railway\).*NOT_IN_TEMPLATE'
  copy_root; edit "$tmp/root/deploy/generic/env.example" '^PUBLIC_HOST=.*$' '# (removed)'; py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null; expect_fail env-missing-required 'E\(generic\): required PUBLIC_HOST'
  copy_root; printf 'ADMIN_2FA_REQUIRED=true\n' >> "$tmp/root/deploy/secretvm/env.template"; py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null; expect_fail secretvm-env-seven 'E\(secretvm\)'
  copy_root; edit "$tmp/root/deploy/secretvm/docker-compose.yml" '(- ADMIN_PASSWORD=\$\{ADMIN_PASSWORD\})' '\1\n      - MASTER_KEY_ADMIN=false'; py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$tmp/out" "$tmp/root" >/dev/null; expect_fail secretvm-literal-env 'V\(secretvm\): environment key MASTER_KEY_ADMIN'
  copy_root; printf 'SESSION_FAILOVER=false\n' >> "$tmp/out/env.generic.example"; expect_fail env-asset-drift 'E\(generic\): env.generic.example differs'
  copy_root; printf '# HOUSEKEEPING=true\n' >> "$tmp/root/.env.example"; expect_fail env-duplicate 'E\(local\): HOUSEKEEPING listed twice'
  echo "OK: deploy-assets self-tests ($n failure modes rejected)"
}

case "${1:-}" in
  generate) [ $# -eq 4 ] || { echo "usage: $0 generate IMAGE DIGEST OUTDIR" >&2; exit 2; }; py generate "$2" "$3" "$4" "$ROOT" ;;
  check)    [ $# -eq 4 ] || { echo "usage: $0 check IMAGE DIGEST OUTDIR" >&2; exit 2; }; py check "$2" "$3" "$4" "$ROOT" ;;
  pr)
    out=$(mktemp -d); trap 'rm -rf "$out"' EXIT
    py generate "$FAKE_IMAGE" "$FAKE_DIGEST" "$out" "$ROOT"
    py check "$FAKE_IMAGE" "$FAKE_DIGEST" "$out" "$ROOT"
    self_test ;;
  --self-test) self_test ;;
  *) echo "usage: $0 generate|check IMAGE DIGEST OUTDIR | pr | --self-test" >&2; exit 2 ;;
esac
