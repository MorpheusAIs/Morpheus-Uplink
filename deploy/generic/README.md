# Generic Docker / VPS overlay

Caddy + Let's Encrypt; secrets via `.env` (never commit). Same GHCR image
pair as [SecretVM](../secretvm/) and [Railway](../railway/).

Prefer digest-pinned release assets:
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

## H3 - single replica

Wallet-bearing `proxy-router` must run as **one replica**. Multi-replica is
unsupported on stock 7.11.

## H2 - managed-mode blast radius

Pinned **v7.11.6-test** (Lumerin release channel; #889 gateway caps early
access — not Base testnet) still has no operation-journal managed cleanup.
Do not assume journal/stake-limit capabilities (M2). Misconfiguration blast
radius on a future managed-mode digest: sessions may not expire as expected;
collateral can remain locked until native expiry or manual admin/`/node` close.

## L1 - privacy env

Router privacy/log env is set in `docker-compose.yml` (**set / believed
honored; live acceptance pending**). Not TEE privacy.

## SSE

Caddy `reverse_proxy` uses `flush_interval -1` so chat SSE is not buffered.
