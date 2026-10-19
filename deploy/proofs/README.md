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

Production fix: init runs as `user: "0:0"`, `chown 10001:10001`, and Uplink entrypoint copies into `DATA_DIR` before `su-exec`.
