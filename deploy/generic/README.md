# Generic Docker / VPS overlay

Caddy + Let's Encrypt; secrets via `.env` (never commit). Same GHCR image
pair as [SecretVM](../secretvm/) and [Railway](../railway/).

Prefer digest-pinned release assets:
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

## Var ownership

Form/`.env` stays the operator fillables (+ `PUBLIC_HOST` for Caddy).
Tunables are env-overridable compose defaults (`${VAR:-default}`): unset
keeps the default; set one in `.env` to override (see `env.example`).
`ADMIN_2FA_REQUIRED=false` or `MASTER_KEY_ADMIN=true` weakens admin security.

- **Uplink**: ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, SESSION_DURATION_SECONDS,
  SESSION_FAILOVER, HOUSEKEEPING; wiring defaults UPLINK_LISTEN, ROUTER_URL,
  DATA_DIR, USAGE_PRUNE_DAYS, CLOSE_SESSIONS_ON_EXIT.
  Diamond / ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **Router** (proxy-router process env): PROXY_STORE_CHAT_CONTEXT,
  PROXY_FORWARD_CHAT_CONTEXT, LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID,
  ETH_NODE_USE_SUBSCRIPTIONS, BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS,
  MOR_TOKEN_ADDRESS. Never describe router PROXY_*/LOG_LEVEL_* as Uplink knobs.

## Session stake (locks, not spends)

Uplink talks to proxy-router over the internal network with the router's
`COOKIE_CONTENT`; the router port is never published. A session stake is
locked while the session runs and is returned to this wallet, held at most
until the next UTC day. Sessions cost ETH gas. Uplink only opens
staking-mode sessions (`directPayment` is always false). Use a dedicated
wallet.

## H3 - single replica

Wallet-bearing `proxy-router` must run as **one replica**. Multi-replica is
unsupported on stock 7.11.

## H2 - managed-mode blast radius

Pinned **v7.15.0** (Lumerin main; includes #889 gateway caps -- not Base
testnet). The image advertises `operation-journal-v1` and `stake-limit-v1`.
Uplink does not drive managed-gateway cleanup (M2). Misconfiguration blast
radius: sessions may not expire as expected; collateral can remain locked
until native expiry or manual admin/`/node` close.

## L1 - privacy env

Router privacy/log env is set in `docker-compose.yml` (**set / believed
honored; live acceptance pending**). Not TEE privacy.

## SSE

Caddy `reverse_proxy` uses `flush_interval -1` so chat SSE is not buffered.

## Edge allowlist / rate limit

Caddy allowlists `/`, `/gui*`, `/v1*`, `/admin*`, `/healthcheck` (else 404) and
writes JSON access logs to stdout. Stock `caddy:*-alpine` has no rate_limit
module; put a reverse-proxy / WAF rate limit in front, or rebuild Caddy with
`github.com/mholt/caddy-ratelimit` if you need in-process limits. Router
`:8082` stays off the public edge.
