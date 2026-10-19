# Auth-file handoff proof (MUST 3)

Reproduces the sidecar -> Uplink `router.auth` ownership fix.

```bash
cd deploy/proofs
docker compose -f auth-handoff-fail-uid100.yml up writer
docker compose -f auth-handoff-fail-uid100.yml run --rm --no-deps reader
# expect: fail-closed-uid100-0400

docker compose -f auth-handoff-proof.yml up router-user-init
docker compose -f auth-handoff-proof.yml run --rm --no-deps uplink-ro-reader
# expect: ro-read-ok
```

Production fix (supersedes the `chown 10001` variant shown in
`auth-handoff-proof.yml`): the Uplink container runs `cap_drop: ALL` with only
CHOWN/SETUID/SETGID, so its root entrypoint has no DAC_OVERRIDE and cannot
read a 0400 file owned by 10001. Init (`user: "0:0"`) therefore leaves
`/shared/router.auth` **root-owned 0400** (allowance JSON root 0444). The
entrypoint reads it as owner and streams it into `DATA_DIR` via `su-exec
uplink`, so the copy is uplink-owned 0400, then points `ROUTER_AUTH_FILE` at
the copy before dropping privileges.

CI regression: `scripts/e2e-secretvm-handoff.sh` runs the real SecretVM
compose (caps, volumes, `:ro` handoff) with a mock router and asserts Uplink
reaches healthy on first and second boot.
