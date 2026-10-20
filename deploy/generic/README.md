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
  SESSION_FAILOVER, UPLINK_PROVIDER_COOLDOWN_SECONDS, HOUSEKEEPING; wiring
  defaults UPLINK_LISTEN, ROUTER_URL,
  DATA_DIR, USAGE_PRUNE_DAYS, CLOSE_SESSIONS_ON_EXIT.
  Diamond / ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **Router** (proxy-router process env): PROXY_STORE_CHAT_CONTEXT,
  PROXY_FORWARD_CHAT_CONTEXT, LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID,
  ETH_NODE_USE_SUBSCRIPTIONS, BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS,
  MOR_TOKEN_ADDRESS. Never describe router PROXY_*/LOG_LEVEL_* as Uplink knobs.

## Upgrading from an earlier test build

`docker compose up` without `--remove-orphans` does not remove services
that were dropped from the compose file (`router-allowance-manager`,
`router-user-init`) or their volumes. On a generic host or dev VM you can
clean up in place. Run this in the deploy directory, with the same `-f` and
`--env-file` flags you deploy with. Docker prefixes volume names with the
compose project name:

```bash
docker compose up -d --remove-orphans
docker volume rm <project>_router_auth <project>_manager_cred \
  <project>_allowance_status <project>_allowance_ctl
```

Also delete `router.auth` from the Uplink data volume (`/data`): the
router credential an earlier test build wrote there. The current build
does not read it. An earlier test build may also have left an extra scoped
router user in the router's config: remove it, or do a fresh deploy if
unsure (a fresh deploy clears both). On a production SecretVM
(no shell) a fresh deploy is the only fix; see
[deploy/secretvm](../secretvm/README.md#upgrading-from-an-earlier-test-build).

A fresh deploy starts with empty Uplink state. Generated (ephemeral) API
keys and the 2FA state (enrollment, recovery codes) are gone, and 2FA
comes back off. Master and Prompt keys come back from the same
`API_KEY_SEED`. Export ephemeral keys first and re-create or import them
afterwards, then turn 2FA on and enroll again.

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
until native expiry or a manual close from the GUI.

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
