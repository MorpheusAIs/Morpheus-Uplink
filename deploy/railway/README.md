# Railway overlay (scaffold)

Peer host overlay for Uplink + stock Lumerin **v7.11.0**. Same GHCR image
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
[GitHub Releases](https://github.com/absgrafx/Morpheus-Uplink/releases/latest)
(`docker-compose.*.deployed.yml`). Prefer those digests over `:latest` in
this scaffold when you cut a real deploy. Stock router pin today:

`ghcr.io/morpheusais/morpheus-lumerin-node:v7.11.0@sha256:3b2b1dea272124ce3c71ab35132f5f8a6dad54bb1e59614a50401a76c062a2b1`

## Services

- **uplink** - public (Railway edge -> `:8080`); healthcheck `GET /healthcheck`
- **proxy-router** - private only (`:8082`); **no public port**

See `docker-compose.yml` and `railway.toml` (non-secret knobs only).

## H4 - secret-class vars (Railway Secrets only)

Never put these in `railway.toml`, image labels, build args, or committed
compose values:

- `WALLET_PRIVATE_KEY`
- `ADMIN_PASSWORD`
- `API_KEY_SEED`
- `COOKIE_CONTENT`
- RPC credentials (`ETH_NODE_ADDRESS` and any provider key material)

No wallet material in logs or (future) journals.

## H3 - single replica (wallet-bearing proxy-router)

This overlay **requires a single replica** for the wallet-bearing
`proxy-router` service. Multi-replica is **explicitly unsupported** (no
cross-replica wallet lock on stock 7.11).

## H2 - managed-mode blast radius

Stock **v7.11.0** has **no** `operation-journal-v1` / managed-gateway cleanup
contract. Uplink on this digest must not assume journal headers or
stake-limit capabilities (M2).

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
privacy on the stock v7.11.0 digest until verified.

## M1 / keep list

- `/node/*` remains **admin-gated** (Basic admin or Master Bearer)
- Keep: Basic admin, `sk-` keys, soft-revoke, `GET /v1/usage`,
  `gateway_models.json`, GHCR release-asset deploy

## Operator checklist

1. Create Railway project with two services (or compose plugin) sharing a
   private network
2. Set H4 vars as **Secrets**; set non-secrets as plain vars
3. Pin uplink image to the release digest (not only `:latest`)
4. Replicas = 1 for `proxy-router` (H3)
5. Confirm only uplink is publicly reachable
6. Open `https://<public>/gui` and fund the wallet per docs/01-prerequisites.md
