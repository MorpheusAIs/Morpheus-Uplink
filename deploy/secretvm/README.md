# SecretVM overlay

Default production host for Uplink. Traefik terminates TLS with SecretVM
certs; Encrypted Secrets supply wallet/admin/seed/cookie/RPC.

Same GHCR image pair as [generic](../generic/) and [Railway](../railway/).
Prefer digest-pinned release assets:
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

## H3 - single replica

Wallet-bearing `proxy-router` must run as **one replica**. Multi-replica is
unsupported on stock 7.11 (no cross-replica wallet lock).

## H2 - managed-mode blast radius

Pinned **v7.11.6-test** (Lumerin release channel; #889 gateway caps early
access -- not Base testnet) still has no operation-journal managed cleanup.
Do not assume `operation-journal-v1` / `stake-limit-v1` (M2). If a future
digest enables managed mode without a companion gateway or lease, sessions
may not clean up as expected and collateral can stay locked until native
expiry or manual admin close. Until the digest bumps again, rely on native
router expiry + Uplink housekeeping.

## L1 - privacy env

Router privacy/log env is set in `docker-compose.yml` (**set / believed
honored; live acceptance pending**). Do not market as TEE privacy until
verified on the pinned digest.

## Traefik / SSE

Do not invent Traefik flags that break TEE TLS or remove `:ro` docker.sock /
cert mounts. Buffering class for streaming: check pending; generic overlay
uses Caddy `flush_interval -1`.
