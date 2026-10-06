# SecretVM overlay

Default production host for Uplink. Traefik terminates TLS with SecretVM
certs; Encrypted Secrets supply only the six operator fillables (wallet /
RPC / cookie / admin / seed / WEB_PUBLIC_URL). Form stays six; tunables
change via compose/code, not Encrypted Secrets:

- **Uplink** (`uplink_bake_env`): SESSION_DURATION_SECONDS, SESSION_FAILOVER,
  ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, HOUSEKEEPING (+ wiring defaults
  UPLINK_LISTEN, ROUTER_URL, DATA_DIR, USAGE_PRUNE_DAYS,
  SESSION_DIRECT_PAYMENT, CLOSE_SESSIONS_ON_EXIT). Diamond / ETH_NODE_CHAIN_ID
  are not Uplink operator knobs.
- **Router** (`router_network_env`; godotenv -- process env wins):
  PROXY_STORE_CHAT_CONTEXT, PROXY_FORWARD_CHAT_CONTEXT,
  LOG_LEVEL_APP/TCP/ETH_RPC, ETH_NODE_CHAIN_ID, ETH_NODE_USE_SUBSCRIPTIONS,
  BLOCKSCOUT_API_URL, DIAMOND_CONTRACT_ADDRESS, MOR_TOKEN_ADDRESS.

Never put router PROXY_*/LOG_LEVEL_* into uplink bake docs.

Same GHCR image pair as [generic](../generic/) and [Railway](../railway/).
Prefer digest-pinned release assets:
https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest

## H3 - single replica

Wallet-bearing `proxy-router` must run as **one replica**. Multi-replica is
unsupported on stock 7.11 (no cross-replica wallet lock).

## H2 - managed-mode blast radius

Pinned **v7.14.0** (Lumerin main; includes #889 gateway caps -- not Base
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
static file (`routes_config` → `routes.yml`) to `uplink:8080`. SecretVM
rejects a `/var/run/docker.sock` bind, so this overlay does not use the
Docker provider or Traefik service labels. Do not invent Traefik flags that
break TEE TLS, and do not put the socket back. Buffering class for
streaming: check pending; generic overlay uses Caddy `flush_interval -1`.
