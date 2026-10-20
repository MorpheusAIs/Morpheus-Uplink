# Generic Docker / VPS overlay

Caddy + Let's Encrypt; secrets via `.env` (never commit). Same GHCR image
pair as [SecretVM](../secretvm/) and [Railway](../railway/).

Deploy the release assets `docker-compose.generic.deployed.yml` (every
image digest-pinned) and `env.generic.example` (copy to `.env`):
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

## Var ownership

Form/`.env` stays the operator fillables (+ `PUBLIC_HOST` for Caddy).
Tunables are env-overridable compose defaults (`${VAR:-default}`): unset
keeps the default; set one in `.env` to override (see `env.example`).
Master key admin access defaults to off (`MASTER_KEY_ADMIN`, default
false); turning it on weakens admin security. 2FA is off by default and
turned on with the switch on the GUI Security tab (recommended; no variable
for it; kept in the data volume). 2FA emergency recovery:
`UPLINK_RESET_2FA=true` for one restart, then remove it (2FA comes back
off).

- **Uplink**: ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, SESSION_DURATION_SECONDS,
  SESSION_FAILOVER, HOUSEKEEPING; wiring defaults UPLINK_LISTEN, ROUTER_URL,
  DATA_DIR, USAGE_PRUNE_DAYS, CLOSE_SESSIONS_ON_EXIT.
  Diamond / ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **Router** (proxy-router process env): PROXY_STORE_CHAT_CONTEXT,
  PROXY_FORWARD_CHAT_CONTEXT, LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID,
  ETH_NODE_USE_SUBSCRIPTIONS, BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS,
  MOR_TOKEN_ADDRESS. Never describe router PROXY_*/LOG_LEVEL_* as Uplink knobs.

## Upgrading from an earlier test build

SecretVM upgrades, and `docker compose up` without `--remove-orphans`, do
not remove services that were dropped from the compose file. If you are
coming from an earlier test build, do a fresh deploy, or remove the
orphaned `router-allowance-manager` and `router-user-init` containers (for
example `docker compose up -d --remove-orphans` where available). Then
delete the old volumes `router_auth`, `manager_cred`, `allowance_status`
and `allowance_ctl`. Docker prefixes volume names with the compose project
name:

```bash
docker volume rm <project>_router_auth <project>_manager_cred \
  <project>_allowance_status <project>_allowance_ctl
```

## Session stake (locks, not spends)

Uplink talks to proxy-router over the internal network with the router's
`COOKIE_CONTENT`; the router port is never published. A session stake is
locked while the session runs and is returned to this wallet, held at most
until the next UTC day. Sessions cost ETH gas. Uplink only opens
staking-mode sessions (`directPayment` is always false). Use a dedicated
wallet.

## H3 - single replica

Wallet-bearing `proxy-router` must run as **one replica**. Multi-replica is
unsupported on the stock router (no cross-replica wallet lock).

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
