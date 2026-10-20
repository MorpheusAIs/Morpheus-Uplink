# SecretVM overlay

Default production host for Uplink. Traefik terminates TLS with SecretVM
certs; Encrypted Secrets supply only the operator fillables (wallet /
RPC / cookie / admin / seed / WEB_PUBLIC_URL). The form has exactly six
fields: SecretVM makes a field of every `environment:` key (literals too),
so the compose has no other. Tunables change via compose/code, not
Encrypted Secrets:

- **Uplink** (`uplink_bake_env`): SESSION_DURATION_SECONDS, SESSION_FAILOVER,
  ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, HOUSEKEEPING (+ wiring defaults
  UPLINK_LISTEN, ROUTER_URL, DATA_DIR, USAGE_PRUNE_DAYS,
  CLOSE_SESSIONS_ON_EXIT), and master key admin access fixed off
  (MASTER_KEY_ADMIN=false; bake values win over environment:). Diamond /
  ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **2FA** is off by default and turned on with the switch on the GUI
  Security tab (recommended); there is no variable for it. The switch is
  kept in the uplink data volume, so it survives redeploys.
- **2FA emergency recovery**: add `UPLINK_RESET_2FA=true` to
  `uplink_bake_env`, redeploy once, then remove it and redeploy; 2FA comes
  back off (see
  [docs/06-developers.md](../../docs/06-developers.md#admin-2fa-and-recovery-codes)).
- **Router** (`router_network_env`; godotenv -- process env wins):
  PROXY_STORE_CHAT_CONTEXT, PROXY_FORWARD_CHAT_CONTEXT,
  LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID, ETH_NODE_USE_SUBSCRIPTIONS,
  BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS, MOR_TOKEN_ADDRESS.

Never put router PROXY_*/LOG_LEVEL_* into uplink bake docs.

To change a tunable, edit the `configs:` block (`uplink_bake_env` /
`router_network_env`) in `docker-compose.yml` (or the downloaded
`docker-compose.secretvm.deployed.yml`). SecretVM turns every `${...}` in
the compose file into an operator prompt and the form deliberately stays at
the six secrets, so there are no `${VAR:-default}` overrides here (unlike
generic / Railway / local).

Same GHCR image pair as [generic](../generic/) and [Railway](../railway/).
Deploy the release asset `docker-compose.secretvm.deployed.yml` (every
image digest-pinned) and fill the form from `env.secretvm.example` (the
six secrets):
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

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
