# Generic Docker / VPS overlay

Caddy + Let's Encrypt; secrets via `.env` (never commit). Same GHCR image
pair as [SecretVM](../secretvm/) and [Railway](../railway/).

Prefer digest-pinned release assets:
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

## Var ownership

Form/`.env` stays the six operator fillables (+ `PUBLIC_HOST` for Caddy).
Tunables change via compose/code:

- **Uplink**: ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, SESSION_DURATION_SECONDS,
  SESSION_FAILOVER, HOUSEKEEPING; wiring defaults UPLINK_LISTEN, ROUTER_URL,
  DATA_DIR, USAGE_PRUNE_DAYS, SESSION_DIRECT_PAYMENT, CLOSE_SESSIONS_ON_EXIT.
  Diamond / ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **Router** (proxy-router process env): PROXY_STORE_CHAT_CONTEXT,
  PROXY_FORWARD_CHAT_CONTEXT, LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID,
  ETH_NODE_USE_SUBSCRIPTIONS, BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS,
  MOR_TOKEN_ADDRESS. Never describe router PROXY_*/LOG_LEVEL_* as Uplink knobs.

## H3 - single replica

Wallet-bearing `proxy-router` must run as **one replica**. Multi-replica is
unsupported on stock 7.11.

## H2 - managed-mode blast radius

Pinned **v7.11.6-test** (Lumerin release channel; #889 gateway caps early
access -- not Base testnet) still has no operation-journal managed cleanup.
Do not assume journal/stake-limit capabilities (M2). Misconfiguration blast
radius on a future managed-mode digest: sessions may not expire as expected;
collateral can remain locked until native expiry or manual admin/`/node` close.

## L1 - privacy env

Router privacy/log env is set in `docker-compose.yml` (**set / believed
honored; live acceptance pending**). Not TEE privacy.

## SSE

Caddy `reverse_proxy` uses `flush_interval -1` so chat SSE is not buffered.
