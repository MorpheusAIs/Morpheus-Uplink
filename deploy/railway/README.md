# Railway overlay (scaffold)

Supported Uplink path: this scaffold (sibling to SecretVM/generic). Not third-party consumer-gateway Railway (no DOMAIN/PUBLIC_ORIGIN).

Peer host overlay for Uplink + stock Lumerin **v7.15.0** (Lumerin main / #889 caps -- not Base testnet). Same GHCR image
pair as SecretVM and generic - different skin for ports, TLS edge, and
secrets injection. **No** private patched node. **No** Railway Dockerfile
for the Lumerin node.

## Pick overlay, pull same digests

| Overlay | Edge | Secrets |
|---------|------|---------|
| [SecretVM](../secretvm/) | Traefik TEE + cert mounts | Encrypted Secrets |
| [Generic](../generic/) | Caddy + Let's Encrypt | `.env` (not committed) |
| **Railway (this)** | Railway HTTPS in front of uplink `:8080` | Railway **secret** vars (H4) |

Production path: the release ships `docker-compose.railway.deployed.yml`
(every image digest-pinned) and `env.railway.example` on
[GitHub Releases](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest).
Railway does not run compose, so use them as the reference: copy the image
digests and variables into the Railway services instead of `:latest`.
Stock router pin today:

`ghcr.io/morpheusais/morpheus-lumerin-node:v7.15.0@sha256:c52e9f12725d7c85e09eec11e0c06ae2b0970d12d1c770dc4daa4cc66f961ea4`

## Services

- **uplink** - public (Railway edge -> `:8080`); healthcheck `GET /healthcheck`
- **proxy-router** - private only (`:8082`); **no public port**

See `docker-compose.yml` and `railway.toml` (non-secret knobs only).

## H4 - operator fillables (Railway Secrets / vars)

Never put these in `railway.toml`, image labels, build args, or committed
compose values. Same fillables as SecretVM Encrypted Secrets / generic `.env`:

- `WALLET_PRIVATE_KEY`
- `ADMIN_PASSWORD`
- `API_KEY_SEED`
- `COOKIE_CONTENT`
- RPC credentials (`ETH_NODE_ADDRESS` and any provider key material)
- `WEB_PUBLIC_URL` (Railway HTTPS origin)

Tunables are not Railway Secrets, and Railway applies none of the compose
defaults (see [Unset on Railway](#unset-on-railway)). 2FA is not a
variable: it is off by default (first login is password only) and turned on
with the switch on the GUI Security tab (recommended: scan the QR code,
confirm a code, save the 10 recovery codes). The switch and enrollment are
kept in `DATA_DIR`: keep it on a persistent Railway volume, or each
redeploy starts with 2FA off. `MASTER_KEY_ADMIN=true` weakens admin
security. 2FA emergency recovery: `UPLINK_RESET_2FA=true` for one restart,
then remove it (2FA comes back off).

Railway caveat: Railway does not run `docker-compose.yml`; each service's
environment comes from its Railway Variables. Railway variables have no
`${VAR:-default}` fallback (its own syntax is `${{SERVICE.VAR}}`
references). If a compose import leaves a value as the literal
`${VAR:-default}` string, set the plain value (the default shown here, or
your override) in that service's Variables. The defaults apply as-is
under `docker compose`.

## Unset on Railway

Railway applies neither the compose `${VAR:-default}` defaults nor its
literal values. An unset variable takes the process's own default (Uplink
code, Uplink image, or stock router v7.15.0):

| Variable (service) | Compose value | Unset on Railway | `env.railway.example` |
|---|---|---|---|
| `PROXY_STORE_CHAT_CONTEXT` (router) | `false` | `true`: chat context stored | active line, checked |
| `LOG_LEVEL_APP` (router) | `warn` | `debug` | active line, checked |
| `ETH_NODE_CHAIN_ID` (router) | `8453` | `0`: no check that the RPC node is on Base | active line, checked |
| `BLOCKSCOUT_API_URL`, `DIAMOND_CONTRACT_ADDRESS`, `MOR_TOKEN_ADDRESS` (router) | Base mainnet values | router does not start | active lines |
| `LOG_LEVEL_TCP`, `LOG_LEVEL_ETH_RPC` (router) | `warn` | `info` | commented |
| `ENVIRONMENT` (router) | `production` | `development` (only changes how `/auth/cookie/path` resolves a relative path) | not listed |
| `AUTH_CONFIG_FILE_PATH`, `COOKIE_FILE_PATH` (router) | `/app/data/proxy.conf`, `/app/data/.cookie` | `/app/proxy.conf`, `/app/.cookie` (outside a volume mounted at `/app/data`) | not listed; set both if you mount `/app/data` |
| `TRUSTED_PROXY_CIDR` (uplink) | `172.28.0.0/16` | none: `X-Forwarded-For` ignored, lockout keys on the peer address | not listed |
| `DATA_DIR` (uplink) | `/data` | `/data` (image default) | not listed |

Same when unset: `MASTER_KEY_ADMIN`, `UPLINK_RESET_2FA` (both `false`),
`PROXY_FORWARD_CHAT_CONTEXT`, `ETH_NODE_USE_SUBSCRIPTIONS` (both `false`),
`SESSION_DURATION_SECONDS`, `SESSION_FAILOVER`, `HOUSEKEEPING`,
`ACTIVE_MODELS_URL`, `GATEWAY_BIDS_URL`, `ROUTER_URL`.
`scripts/deploy-assets.sh` fails if a checked line is commented out.

Keep ownership separate:

- **Uplink**: ACTIVE_MODELS_URL, GATEWAY_BIDS_URL, SESSION_DURATION_SECONDS,
  SESSION_FAILOVER, HOUSEKEEPING; wiring defaults UPLINK_LISTEN, ROUTER_URL,
  DATA_DIR, USAGE_PRUNE_DAYS, CLOSE_SESSIONS_ON_EXIT.
  Diamond / ETH_NODE_CHAIN_ID are not Uplink operator knobs.
- **Router**: PROXY_* / LOG_LEVEL_* / chain / Diamond / MOR (see L1 below).
  Never put router PROXY_*/LOG_LEVEL_* into Uplink bake docs.

No wallet material in logs or (future) journals.

## Session stake (locks, not spends)

Uplink talks to proxy-router over the internal network with the router's
`COOKIE_CONTENT`; the router port is never published. A session stake is
locked while the session runs and is returned to this wallet, held at most
until the next UTC day. Sessions cost ETH gas. Uplink only opens
staking-mode sessions (`directPayment` is always false). Use a dedicated
wallet.

## H3 - single replica (wallet-bearing proxy-router)

This overlay **requires a single replica** for the wallet-bearing
`proxy-router` service. Multi-replica is **explicitly unsupported** (no
cross-replica wallet lock on the stock router).

## H2 - managed-mode blast radius

Pinned **v7.15.0** advertises `operation-journal-v1` and `stake-limit-v1`
(#889, on Lumerin main -- not Base testnet). Uplink on this digest still
does not send journal headers or stake-limit leases (M2). Chain defaults
remain mainnet.

The router can do gateway-managed session cleanup. Uplink does not use it:

- Keep the native expiry loop. Do not assume a heartbeat/lease.
- Misconfiguration blast radius: sessions may not expire/cleanup as
  operators expect; collateral can remain locked until native expiry or
  manual close via admin/`/node` (admin-gated).
- Treat cleanup as **native router expiry + Uplink housekeeping** only.

## L1 - privacy env honesty

Compose sets these on `proxy-router`, but Railway does not apply them, so
set them as `proxy-router` service variables:

- `PROXY_STORE_CHAT_CONTEXT=false` (unset: the router stores chat context)
- `PROXY_FORWARD_CHAT_CONTEXT=false` (unset: also false)
- `LOG_LEVEL_APP=warn` / `LOG_LEVEL_TCP=warn` / `LOG_LEVEL_ETH_RPC=warn`
  (unset: debug / info / info)

**Set / believed honored; live acceptance pending.** Do **not** claim TEE
privacy on the pinned v7.15.0 digest until verified.

## M1 / keep list

- `/node/*` remains **admin-gated** (admin session, or the Master key with
  `MASTER_KEY_ADMIN=true`), with a block list (see docs/06-developers.md)
- Keep: password + session-cookie admin login (Basic auth is refused), `sk-` keys, soft-revoke, `GET /v1/usage`,
  `gateway_models.json`, GHCR release-asset deploy

## Operator checklist

| Step | Done when |
|------|-----------|
| **1. Project** | Railway project has uplink + proxy-router on a private network (compose plugin or two services). |
| **2. Secrets + vars** | H4 vars are **Railway Secrets** only; every active line of `env.railway.example` (including `PROXY_STORE_CHAT_CONTEXT=false`, `LOG_LEVEL_APP=warn`, `ETH_NODE_CHAIN_ID=8453`) is set as a plain var on its service. |
| **3. Digests** | Images pinned to the digests in the [release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) `docker-compose.railway.deployed.yml` (not bare `:latest`). |
| **4. Replicas** | `proxy-router` replicas = **1** (H3). |
| **5. Public edge** | Only uplink is publicly reachable (Railway HTTPS -> `:8080`). |
| **6. GUI + fund** | `https://<public>/gui` works; 2FA turned on with the Security tab switch (recommended); wallet funded per [docs/01-prerequisites.md](../../docs/01-prerequisites.md). |

**Done** when GUI login works and Probe returns a completion. Bootstrap summary:
[docs/02-bootstrap.md -- First start -- Railway](../../docs/02-bootstrap.md#first-start--railway).

## Platform edge limits (Railway)

Railway terminates TLS and exposes only the uplink service port you bind
(`8080`). There is no Traefik/Caddy file provider on this path, so Uplink
cannot enforce path allowlists or login rate limits at the platform edge.

What Railway can / cannot do for this overlay:

- Can: private networking between uplink and proxy-router; single public port.
- Cannot (from this compose): Traefik-style path allowlist, access-log middleware,
  or router-login rate limit. Rely on Railway dashboard networking + Uplink
  admin lockout (application layer). Do not publish proxy-router `:8082`.
