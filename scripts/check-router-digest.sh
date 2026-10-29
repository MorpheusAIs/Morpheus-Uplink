#!/usr/bin/env bash
# Require the pinned Lumerin router digest on every compose template, doc
# and llms file, and the required one-URL ETH_NODE_ADDRESS on every
# compose template.
#
# --resolve (CI, needs network): also resolve the official tag on GHCR and
# require its OCI index digest to equal WANT, and require the image's SLSA
# provenance vcs:revision to equal the commit the tag points at upstream.
# The commit is resolved at check time and never written into the repo.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WANT='ghcr.io/morpheusais/morpheus-lumerin-node:v7.18.0@sha256:070b99a8104c6e19d1c25c932a313e72a50fb5caa82d87554b22899370e444ba'
TAG="${WANT%%@*}"
TAG="${TAG##*:}"
ETH_REQ='ETH_NODE_ADDRESS=${ETH_NODE_ADDRESS:?set one Base RPC URL}'
bad=0
composes=("$ROOT"/deploy/*/docker-compose.yml "$ROOT"/docker-compose.local.yml)
for f in "${composes[@]}"; do
  [ -f "$f" ] || continue
  if ! grep -qF "$WANT" "$f"; then
    echo "Missing router digest pin in $f"
    bad=1
  fi
  # No unpinned router tag without the digest
  if grep -nE 'morpheus-lumerin-node:v7\.18\.0(["[:space:]]|$)' "$f" | grep -v '@sha256:'; then
    echo "Unpinned router tag in $f"
    bad=1
  fi
  if ! grep -qF -- "$ETH_REQ" "$f"; then
    echo "ETH_NODE_ADDRESS is not required (\${ETH_NODE_ADDRESS:?...}) in $f"
    bad=1
  fi
done
# Every pinned router reference anywhere in compose, docs and llms files is
# byte-identical to WANT.
docs=("$ROOT"/README.md "$ROOT"/AGENTS.md "$ROOT"/docs/*.md "$ROOT"/deploy/*/README.md "$ROOT"/llms.txt "$ROOT"/llms-full.txt "${composes[@]}")
for f in "${docs[@]}"; do
  [ -f "$f" ] || continue
  while IFS= read -r ref; do
    if [ "$ref" != "$WANT" ]; then
      echo "Router pin differs from WANT in $f: $ref"
      bad=1
    fi
  done < <(grep -oE 'ghcr\.io/morpheusais/morpheus-lumerin-node:[^@`"[:space:]]+@sha256:[0-9a-f]+' "$f" || true)
done
if [ "$bad" -ne 0 ]; then exit 1; fi
echo "OK: router digest pinned, ETH_NODE_ADDRESS required"

if [ "${1:-}" = "--resolve" ]; then
  repo=morpheusais/morpheus-lumerin-node
  reg="https://ghcr.io/v2/$repo"
  tok=$(curl -fsS "https://ghcr.io/token?scope=repository:$repo:pull" | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
  idx_accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json'
  got=$(curl -fsSI -H "Authorization: Bearer $tok" -H "Accept: $idx_accept" "$reg/manifests/$TAG" |
    tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest"{print $2}')
  if [ "$got" != "${WANT##*@}" ]; then
    echo "Tag $TAG resolves to $got on GHCR, not the pinned ${WANT##*@}"
    exit 1
  fi
  echo "OK: $TAG index digest on GHCR equals the pin"
  # Provenance: the attestation manifest's SLSA layer names the source commit.
  idx=$(curl -fsS -H "Authorization: Bearer $tok" -H "Accept: $idx_accept" "$reg/manifests/${WANT##*@}")
  att=$(python3 -c 'import sys,json
for m in json.loads(sys.argv[1])["manifests"]:
    if m.get("annotations",{}).get("vnd.docker.reference.type")=="attestation-manifest":
        print(m["digest"]); break' "$idx")
  [ -n "$att" ] || { echo "No provenance attestation on the pinned index"; exit 1; }
  layer=$(curl -fsS -H "Authorization: Bearer $tok" -H "Accept: application/vnd.oci.image.manifest.v1+json" "$reg/manifests/$att" |
    python3 -c 'import sys,json
for l in json.load(sys.stdin)["layers"]:
    if "slsa.dev/provenance" in l.get("annotations",{}).get("in-toto.io/predicate-type",""):
        print(l["digest"]); break')
  [ -n "$layer" ] || { echo "No SLSA provenance layer"; exit 1; }
  rev=$(curl -fsSL -H "Authorization: Bearer $tok" "$reg/blobs/$layer" | grep -oE '"vcs:revision":"[0-9a-f]{40}"' | head -1 | cut -d'"' -f4)
  # A lightweight tag has no peeled line; an annotated one lists ^{} last.
  tagrev=$(git ls-remote https://github.com/MorpheusAIs/Morpheus-Lumerin-Node "refs/tags/$TAG" "refs/tags/$TAG^{}" | tail -1 | cut -f1)
  if [ -z "$rev" ] || [ "$rev" != "$tagrev" ]; then
    echo "Provenance vcs:revision ($rev) is not the $TAG tag commit ($tagrev)"
    exit 1
  fi
  echo "OK: provenance vcs:revision equals the $TAG tag commit"
fi
