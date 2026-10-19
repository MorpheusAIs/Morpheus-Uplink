# Generic Docker / VPS overlay

Caddy + Let's Encrypt; secrets via `.env` (never commit). Same GHCR image
pair as [SecretVM](../secretvm/) and [Railway](../railway/).

Prefer digest-pinned release assets:
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

## Var ownership

Form/`.env` stays the operator fillables (+ `PUBLIC_HOST` for Caddy).
Tunables change via compose/code:

- **Uplink**: ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, SESSION_DURATION_SECONDS,
  SESSION_FAILOVER, HOUSEKEEPING; wiring defaults UPLINK_LISTEN, ROUTER_URL,
  DATA_DIR, USAGE_PRUNE_DAYS, SESSION_DIRECT_PAYMENT, CLOSE_SESSIONS_ON_EXIT.
  Diamond / ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **Router** (proxy-router process env): PROXY_STORE_CHAT_CONTEXT,
  PROXY_FORWARD_CHAT_CONTEXT, LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID,
  ETH_NODE_USE_SUBSCRIPTIONS, BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS,
  MOR_TOKEN_ADDRESS. Never describe router PROXY_*/LOG_LEVEL_* as Uplink knobs.

## MOR allowance (optional cap)

Uplink talks to the router as a scoped `uplink` user (never the admin
cookie). That user is recreated at each boot with a MOR **allowance**: the
most MOR it can stake for sessions before a restart.

- `UPLINK_MOR_ALLOWANCE_WEI` **empty / unset** (default): allowance = the
  **full wallet MOR balance** read at boot. Convenient, but it is the
  maximum exposure if Uplink is compromised.
- **Set** (digits only, wei): explicit cap. Setting a cap **limits
  exposure**; e.g. `1000000000000000000000` = 1000 MOR. Leading zeros are
  ignored.
- **`0`**: explicit zero cap. The balance is not read; no sessions open
  until you raise the cap and restart.
- If the balance read fails or the wallet holds 0 MOR, the allowance is 0
  with a warning (restart after funding).

The effective allowance (wei + source: `balance`, `override`, `zero`, or
`unknown`) is shown on the GUI Status and Security tabs, returned by
`/admin/status` (`morAllowance`), and logged once at boot
(`mor allowance` line + `allowance` audit event; no secrets).

## H3 - single replica

Wallet-bearing `proxy-router` must run as **one replica**. Multi-replica is
unsupported on stock 7.11.

## H2 - managed-mode blast radius

Pinned **v7.15.0** (Lumerin main; includes #889 gateway caps -- not Base
testnet). The image advertises `operation-journal-v1` and `stake-limit-v1`.
Uplink does not drive managed-gateway cleanup (M2). Misconfiguration blast
radius: sessions may not expire as expected; collateral can remain locked
until native expiry or manual admin close.

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
