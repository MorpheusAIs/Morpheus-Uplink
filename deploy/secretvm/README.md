# SecretVM overlay

Default production host for Uplink. Traefik terminates TLS with SecretVM
certs; Encrypted Secrets supply only the operator fillables (wallet /
RPC / cookie / admin / seed / WEB_PUBLIC_URL). Form stays six; tunables
change via compose/code, not Encrypted Secrets:

- **Uplink** (`uplink_bake_env`): SESSION_DURATION_SECONDS, SESSION_FAILOVER,
  ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, HOUSEKEEPING (+ wiring defaults
  UPLINK_LISTEN, ROUTER_URL, DATA_DIR, USAGE_PRUNE_DAYS,
  CLOSE_SESSIONS_ON_EXIT). Diamond / ETH_NODE_CHAIN_ID
  are not Uplink operator knobs.
- **Router** (`router_network_env`; godotenv -- process env wins):
  PROXY_STORE_CHAT_CONTEXT, PROXY_FORWARD_CHAT_CONTEXT,
  LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID, ETH_NODE_USE_SUBSCRIPTIONS,
  BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS, MOR_TOKEN_ADDRESS.

Never put router PROXY_*/LOG_LEVEL_* into uplink bake docs.

Same GHCR image pair as [generic](../generic/) and [Railway](../railway/).
Prefer digest-pinned release assets:
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

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
unsupported on stock 7.11 (no cross-replica wallet lock).

## H2 - managed-mode blast radius

Pinned **v7.15.0** (Lumerin main; includes #889 gateway caps -- not Base
testnet). The image advertises `operation-journal-v1` and `stake-limit-v1`.
Uplink does not drive managed-gateway cleanup or send journal/lease headers.
Rely on native router expiry + Uplink housekeeping. Sessions can stay locked
until native expiry or a manual admin close.

## L1 - privacy env

Router privacy/log env is set in compose `configs:` (`router_network_env`) (**set / believed
honored; live acceptance pending**). Do not market as TEE privacy until
verified on the pinned digest.

## Traefik / SSE

TLS still uses the SecretVM cert mount (`/mnt/secure/cert`). Routing is a
static file (`routes_config` -> `routes.yml`) to `uplink:8080`. SecretVM
rejects a `/var/run/docker.sock` bind, so this overlay does not use the
Docker provider or Traefik service labels. Do not invent Traefik flags that
break TEE TLS, and do not put the socket back. Buffering class for
streaming: check pending; generic overlay uses Caddy `flush_interval -1`.

## Edge allowlist / rate limit

Traefik is pinned (digest) with JSON access logs, an explicit path allowlist
(`/`, `/gui*`, `/v1*`, `/admin*`, `/healthcheck`; else 404 via low-priority
deny router), and an average/burst rate limit middleware on the allowlist
router. Router `:8082` is not published. Traefik still gets zero secrets.
