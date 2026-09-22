# Railway overlay (scaffold)

Supported Uplink path: this scaffold (sibling to SecretVM/generic). Not third-party consumer-gateway Railway (no DOMAIN/PUBLIC_ORIGIN).

Peer host overlay for Uplink + stock Lumerin **v7.11.6-test** (release channel / #889 caps early access -- not Base testnet). Same GHCR image
pair as SecretVM and generic - different skin for ports, TLS edge, and
secrets injection. **No** private patched node. **No** Railway Dockerfile
for the Lumerin node.

## Pick overlay, pull same digests

| Overlay | Edge | Secrets |
|---------|------|---------|
| [SecretVM](../secretvm/) | Traefik TEE + cert mounts | Encrypted Secrets |
| [Generic](../generic/) | Caddy + Let's Encrypt | `.env` (not committed) |
| **Railway (this)** | Railway HTTPS in front of uplink `:8080` | Railway **secret** vars (H4) |

Production path: download digest-pinned release assets from
[GitHub Releases](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest)
(`docker-compose.*.deployed.yml`). Prefer those digests over `:latest` in
this scaffold when you cut a real deploy. Stock router pin today:

`ghcr.io/morpheusais/morpheus-lumerin-node:v7.11.6-test@sha256:da9890e376174d587d465c4d2679984939b085e06a3b34fc52a0ac9b19bb999f`

## Services

- **uplink** - public (Railway edge -> `:8080`); healthcheck `GET /healthcheck`
- **proxy-router** - private only (`:8082`); **no public port**

See `docker-compose.yml` and `railway.toml` (non-secret knobs only).

## H4 - operator fillables (Railway Secrets / vars)

Never put these in `railway.toml`, image labels, build args, or committed
compose values. Same six as SecretVM Encrypted Secrets / generic `.env`:

- `WALLET_PRIVATE_KEY`
- `ADMIN_PASSWORD`
- `API_KEY_SEED`
- `COOKIE_CONTENT`
- RPC credentials (`ETH_NODE_ADDRESS` and any provider key material)
- `WEB_PUBLIC_URL` (Railway HTTPS origin)

Session / catalog / housekeeping / PROXY_* / LOG_LEVEL_* are baked in
compose (change via code only). No wallet material in logs or (future) journals.

## H3 - single replica (wallet-bearing proxy-router)

This overlay **requires a single replica** for the wallet-bearing
`proxy-router` service. Multi-replica is **explicitly unsupported** (no
cross-replica wallet lock on stock 7.11).

## H2 - managed-mode blast radius

Pinned **v7.11.6-test** has **no** `operation-journal-v1` / managed-gateway
cleanup contract. Uplink on this digest must not assume journal headers or
stake-limit capabilities (M2). Tag is Lumerin release channel (#889 gateway
caps early access), **not** Base testnet -- chain defaults remain mainnet.

If a future digest enables gateway-managed session cleanup:

- Either refuse managed mode unless a companion gateway is documented **and**
  enforced, **or** keep the native expiry loop unless an explicit
  heartbeat/lease proves the gateway owns cleanup.
- Misconfiguration blast radius: sessions may not expire/cleanup as
  operators expect; collateral can remain locked until native expiry or
  manual close via admin/`/node` (admin-gated).
- Until Workstream 1 lands and Uplink bumps the digest, treat cleanup as
  **native router expiry + Uplink housekeeping** only.

## L1 - privacy env honesty

Compose sets on `proxy-router`:

- `PROXY_STORE_CHAT_CONTEXT=false`
- `PROXY_FORWARD_CHAT_CONTEXT=false`
- `LOG_LEVEL_APP=warn` / `LOG_LEVEL_TCP=warn` / `LOG_LEVEL_ETH_RPC=warn`

**Set / believed honored; live acceptance pending.** Do **not** claim TEE
privacy on the pinned v7.11.6-test digest until verified.

## M1 / keep list

- `/node/*` remains **admin-gated** (Basic admin or Master Bearer)
- Keep: Basic admin, `sk-` keys, soft-revoke, `GET /v1/usage`,
  `gateway_models.json`, GHCR release-asset deploy

## Operator checklist

| Step | Done when |
|------|-----------|
| **1. Project** | Railway project has uplink + proxy-router on a private network (compose plugin or two services). |
| **2. Secrets** | H4 vars are **Railway Secrets** only; non-secrets are plain vars. |
| **3. Digests** | Images pinned to [release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) digests (not bare `:latest`). |
| **4. Replicas** | `proxy-router` replicas = **1** (H3). |
| **5. Public edge** | Only uplink is publicly reachable (Railway HTTPS -> `:8080`). |
| **6. GUI + fund** | `https://<public>/gui` works; wallet funded per [docs/01-prerequisites.md](../../docs/01-prerequisites.md). |

**Done** when GUI login works and Probe returns a completion. Bootstrap summary:
[docs/02-bootstrap.md -- First start -- Railway](../../docs/02-bootstrap.md#first-start--railway).
