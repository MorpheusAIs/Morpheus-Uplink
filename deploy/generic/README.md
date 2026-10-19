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
  DATA_DIR, USAGE_PRUNE_DAYS, CLOSE_SESSIONS_ON_EXIT.
  Diamond / ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **Router** (proxy-router process env): PROXY_STORE_CHAT_CONTEXT,
  PROXY_FORWARD_CHAT_CONTEXT, LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID,
  ETH_NODE_USE_SUBSCRIPTIONS, BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS,
  MOR_TOKEN_ADDRESS. Never describe router PROXY_*/LOG_LEVEL_* as Uplink knobs.

## MOR allowance (locks, not spends; optional cap)

Uplink talks to the router as a scoped `uplink` user (never the admin
cookie).

> **MOR allowance.** The gateway's router user can only open inference sessions; it cannot send MOR or ETH, change approvals, or read the wallet key. A session stake is locked while the session runs and is returned to this wallet, held at most until the next UTC day. Sessions cost ETH gas.
> Empty `UPLINK_MOR_ALLOWANCE_WEI`: the gateway may lock up to the live wallet MOR balance (new deposits are included within ~2 min). Set a value in wei to cap how much MOR it can lock in total; `0` disables session opening.
> Note: the router also supports "direct payment" sessions, where the provider is paid from the stake (at most `getTodaysBudget / totalMORSupply` of it, about 0.3% today). The gateway itself never uses it, but whoever holds the router credential could. A cap bounds that worst case to the same fraction of the cap.

**High-value wallets: use cap mode.** Set `UPLINK_MOR_ALLOWANCE_WEI`; a cap
bounds the direct-payment worst case to that fraction of the cap.

- **Empty / unset** (balance mode, default): the `router-allowance-manager`
  service keeps the allowance equal to the live wallet MOR balance. It polls
  every 60 s, includes new deposits within ~2 min and never changes the
  password. It also overwrites manual router-side allowance edits; for a
  fixed budget set a cap. The GUI's **Resync allowance now** button asks for
  an immediate resync.
- **Set** (digits only, wei; no internal spaces): fixed cap; the manager never
  changes it. Example: `1000000000000000000000` = 1000 MOR. Leading zeros are
  ignored.
- **`0`**: session opening disabled. The balance is not read.
- A failed or zero balance read is skipped (the manager never writes 0). At
  boot, init falls back to 0 with a warning.

The effective allowance (wei + source: `balance`, `override`, `zero`,
`ceiling` or `unknown`) and the auto-resync state are shown on the GUI Status
and Security tabs and returned by `/admin/status` (`morAllowance`,
`allowanceResync`), and logged once at boot (`mor allowance` line +
`allowance` audit event; no secrets).

Credentials at rest: the scoped router credential stays on disk in `/shared`
and in Uplink's `/data` until the next init run replaces it. The admin
credential the manager uses is stored as `/mgr/admin.cookie` (0400, uid 10002)
on the manager-only `manager_cred` volume; only init (read-write) and the
manager (read-only) mount it.

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
