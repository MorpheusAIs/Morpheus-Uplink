<p align="center">
  <img src="docs/assets/readme-hero.png" alt="Uplink — matrix green rotary phone, your own line into Morpheus" width="920">
</p>

<h1 align="center">Uplink</h1>

<p align="center">
  <strong>Your personal gateway to the Morpheus decentralized AI network.</strong>
</p>

<p align="center">
  <a href="LICENSE">MIT</a> ·
  <a href="DISCLAIMER.md">Disclaimer</a> ·
  <a href="NOTICE">NOTICE</a>
  &nbsp;·&nbsp;
  <a href="docs/README.md">Operator guide</a> ·
  <a href="docs/04-clients.md">Clients</a> ·
  <a href="docs/06-developers.md">Developers</a>
  &nbsp;·&nbsp;
  <a href="AGENTS.md">Agents</a>
</p>

> Self-custodial and experimental: you hold the wallet keys; any Prompt key can
> lock MOR on that wallet; prompts go to third-party providers. Read
> [DISCLAIMER.md](DISCLAIMER.md) before you fund a box or share a key.

## What it is

The hosted Morpheus API ([api.mor.org](https://api.mor.org)) is a **shared**
gateway — many users, pooled sessions, credits billed by Morpheus.

**Uplink is the opposite:** *your* OpenAI-compatible HTTPS front door on *your*
machine, backed by *your* consumer proxy-router (C-Node) and *your* wallet.

- Same shape clients already know: `POST /v1/chat/completions`, embeddings,
  `GET /v1/models`, Bearer `sk-…` keys
- Sessions open and reuse automatically (model name → on-chain session)
- On boot it reuses still-valid open sessions and closes expired or duplicate ones before opening any. A graceful `docker stop` closes every open session on the wallet, including sessions for models not in the catalog and, on a shared wallet, the other client's sessions; the stake is returned (a lock, not a loss). `CLOSE_SESSIONS_ON_EXIT=false` opts out. Use a dedicated wallet ([details](docs/06-developers.md#sessions-across-restarts))
- Admin GUI at `/gui` — balances, keys, Probe, usage
- You pay providers in MOR from the wallet you fund — no multi-tenant credits

## Get running (SecretVM)

**Fastest path:** [SecretVM](https://docs.scrt.network/) (Secret Labs) → paste
the release compose → Encrypted Secrets → fund → Probe → point a client at
`/v1`. Do **not** `git clone` for production — use the
[latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest).

| Step | Do this |
|------|---------|
| **1. Box** | Create a SecretVM that accepts Docker Compose + Encrypted Secrets. |
| **2. Compose** | Paste **`docker-compose.secretvm.deployed.yml`** from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest). |
| **3. Secrets** | Fill Encrypted Secrets (block below) — **operator fillables** (wallet / RPC / cookie / admin / seed / `WEB_PUBLIC_URL`). |
| **4. Deploy** | Start the VM; wait until `uplink` is healthy (proxy-router has no container healthcheck), then check that the GUI Status line reads Healthy. |
| **5. GUI** | Open `https://<host>/gui/` → `admin` / your `ADMIN_PASSWORD`. |
| **6. Fund** | On **Status**, copy the wallet → send a little **ETH** + **MOR** on Base. |
| **7. Use** | **Probe** once, then put the **Prompt** key in any OpenAI client with base URL `https://<host>/v1`. |

<details>
<summary><strong>Encrypted Secrets (copy / paste)</strong></summary>

```bash
WALLET_PRIVATE_KEY=0xYOUR_PRIVATE_KEY
ETH_NODE_ADDRESS=https://base-mainnet.g.alchemy.com/v2/YOUR_ALCHEMY_KEY
COOKIE_CONTENT=admin:YOUR_STRONG_PASSWORD
ADMIN_PASSWORD=YOUR_GUI_PASSWORD
API_KEY_SEED=PASTE_openssl_rand_hex_32_HERE
WEB_PUBLIC_URL=https://your-vm-name.vm.scrtlabs.com
```

`openssl rand -hex 32` for the seed. Prefer a **dedicated** consumer wallet.
The SecretVM form has exactly these **six** fields (`env.secretvm.example` is the same six). Tunables are **not** Encrypted Secrets: SecretVM keeps them in the compose `configs:` block; generic and local compose take them as env-overridable `${VAR:-default}` defaults, while Railway does not run compose and applies none of them (see the Railway steps below and [docs/06-developers.md](docs/06-developers.md#environment-variables)):
- **Uplink** (`uplink_bake_env` / Uplink process): `ACTIVE_MODELS_URL`, `GATEWAY_BIDS_URL`, `SESSION_DURATION_SECONDS`, `SESSION_FAILOVER`, `HOUSEKEEPING`, and the `UPLINK_RESET_2FA` break-glass; wiring defaults `UPLINK_LISTEN`, `ROUTER_URL`, `DATA_DIR`, `USAGE_PRUNE_DAYS`, `CLOSE_SESSIONS_ON_EXIT`. Diamond / `ETH_NODE_CHAIN_ID` are **not** Uplink operator knobs.
- **Router** (`router_network_env` / proxy-router only; godotenv -- process env wins): `PROXY_STORE_CHAT_CONTEXT`, `PROXY_FORWARD_CHAT_CONTEXT`, `LOG_LEVEL_APP`/`TCP`/`ETH_RPC`, `ETH_NODE_CHAIN_ID`, `ETH_NODE_USE_SUBSCRIPTIONS`, `BLOCKSCOUT_API_URL`, `DIAMOND_CONTRACT_ADDRESS`, `MOR_TOKEN_ADDRESS`. Never put router `PROXY_*` / `LOG_LEVEL_*` into Uplink bake docs.
Full walkthrough (and plain Docker VPS): [docs/02-bootstrap.md](docs/02-bootstrap.md).

</details>

**Done when:** GUI at `/gui/`, wallet funded with ETH+MOR on Base, Probe succeeds, OpenAI client hits `https://<host>/v1` with a Prompt key.

### Generic VPS

Same GHCR images (`ghcr.io/morpheusais/uplink` from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest)) and Lumerin pin as SecretVM. Release asset: **`docker-compose.generic.deployed.yml`** (+ `env.generic.example`). Full walkthrough: [docs/02-bootstrap.md#first-start--generic-vps](docs/02-bootstrap.md#first-start--generic-vps).

| Step | Do this |
|------|---------|
| **1. DNS / ports** | Point A/AAAA at the box; open **80** and **443**. |
| **2. Compose** | Download **`docker-compose.generic.deployed.yml`** (+ `env.generic.example` → `.env`) from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest). |
| **3. Secrets** | Fill `.env`: `PUBLIC_HOST` + the operator fillables (wallet / RPC / cookie / admin / seed / `WEB_PUBLIC_URL`) — never commit. |
| **4. Deploy** | `docker compose -f docker-compose.generic.deployed.yml --env-file .env up -d` (Caddy + LE). |
| **5. GUI → fund → Probe** | Open `https://$PUBLIC_HOST/gui/` → fund ETH+MOR on Base → Probe → Prompt key at `/v1`. |

**Done when:** same as SecretVM — GUI, funded wallet, Probe OK, client on `https://<host>/v1`.

### Railway scaffold

Supported Uplink hosts are **SecretVM**, **generic VPS**, and **this in-repo Railway scaffold** only. Do not follow third-party consumer-gateway Railway stacks (different images/vars such as DOMAIN / PUBLIC_ORIGIN).

Same image pair. The [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) ships a digest-pinned reference **`docker-compose.railway.deployed.yml`** (+ `env.railway.example`); Railway does not run compose itself, so copy its image digests and variables into the Railway services (scaffold: [`deploy/railway/`](deploy/railway/)). Checklist: [docs/02-bootstrap.md#first-start--railway](docs/02-bootstrap.md#first-start--railway) · [deploy/railway/README.md](deploy/railway/README.md).

| Step | Do this |
|------|---------|
| **1. Project** | Create Railway project: uplink + proxy-router on a private network. |
| **2. Images** | Use the digests from `docker-compose.railway.deployed.yml` for `ghcr.io/morpheusais/uplink` and Lumerin — not bare `:latest`. |
| **3. Secrets + vars** | Set the operator fillables (wallet / admin / seed / cookie / RPC / `WEB_PUBLIC_URL`) as **Railway Secrets**. Railway applies no compose defaults, so also set the active lines of `env.railway.example` as plain service variables: `PROXY_STORE_CHAT_CONTEXT=false` (unset stores chat context), `LOG_LEVEL_APP=warn`, `ETH_NODE_CHAIN_ID=8453` and the chain addresses on proxy-router. |
| **4. Replicas** | `proxy-router` replicas = **1** (wallet-bearing). |
| **5. Edge** | Public HTTPS → uplink `:8080` only; open `/gui/` → turn 2FA on (Security tab, recommended) → fund → Probe → `/v1`. |

**Done when:** same as SecretVM — GUI, funded wallet, Probe OK, client on `https://<host>/v1`.

```bash
curl -s https://YOUR_HOST/v1/chat/completions \
  -H "Authorization: Bearer sk-prompt.…" \
  -H "Content-Type: application/json" \
  -d '{"model":"REPLACE_WITH_PROBE_NAME","messages":[{"role":"user","content":"hi"}],"max_tokens":64}'
```

Use an **exact** model name from Probe / `GET /v1/models` / the gateway
catalog — do **not** hardcode a flashy name, and do **not** treat generic
[active.mor.org](https://active.mor.org) or `active_models.json` / ALL feeds
as the catalog. Defaults:

| Env (baked) | Default |
|-------------|---------|
| `ACTIVE_MODELS_URL` | `https://active.mor.org/gateway_models.json` |
| `GATEWAY_BIDS_URL` | `https://active.mor.org/gateway_bids.json` |

Images: `ghcr.io/morpheusais/uplink` ([latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest)). Release compose **digest-pins Uplink and proxy-router (Lumerin) v7.15.0**
(Lumerin `main` / #889 caps — **not** Base testnet) — not
`:latest`. More clients: [docs/04-clients.md](docs/04-clients.md).

## Things worth knowing

- **Not the hosted APIGW.** No Morpheus credits account, no shared pool with
  strangers. One wallet, one gateway, your ops.
- **Any Prompt / ephemeral key can lock MOR** from that wallet as session
  stake (returned, held at most until the next UTC day). Keep Master off
  shared machines.
- **Built-in keys survive a wipe** if you keep the same `API_KEY_SEED`.
  Rotating it also invalidates the 2FA secret and recovery codes; the way
  back is one restart with `UPLINK_RESET_2FA=true`.
  Generated (ephemeral) keys and local usage history do not — export them if
  you care. Sessions and stake live on-chain with the wallet.
- **Deploy from release assets**, not a git checkout. Every image in them is
  digest-pinned, Uplink and **proxy-router v7.15.0** included (not `:latest`).
- **Gateway catalog only:** `ACTIVE_MODELS_URL` →
  `https://active.mor.org/gateway_models.json`, `GATEWAY_BIDS_URL` →
  `https://active.mor.org/gateway_bids.json`. Never default to
  `active_models.json` / ALL.
- **Admin 2FA** (TOTP) is an on/off switch on the Security tab, off by
  default on every deploy (first login is password only). We recommend
  turning it on: the switch runs the enrollment (QR code, confirm a code)
  and issues 10 single-use recovery codes, shown once; from then on login
  needs a TOTP or recovery code. The switch is kept in `DATA_DIR`, so it
  survives restarts and redeploys. The Security tab shows how many codes
  are left and can regenerate them, reset 2FA (wipe and re-enroll; 2FA
  stays on) or turn it off (password plus a code; signs out every session).
  No environment variable turns 2FA on.
  Emergency recovery when locked out: one restart with
  `UPLINK_RESET_2FA=true` (2FA comes back off).
  [Details](docs/06-developers.md#admin-2fa-and-recovery-codes)
- **Only expose the gateway** (HTTPS). The router admin port stays private.
- **`/node/*` router passthrough** takes GET, HEAD, POST and DELETE only,
  is admin-gated, and refuses routes that send or approve funds, open
  sessions, create providers, models or bids, change the wallet, router
  users, permissions or config, access router files, or make the router
  contact a caller-chosen address (403, audited). Reads (GET, HEAD) pass
  through, wallet, router-user and config reads included. [Details](docs/06-developers.md#router-access-and-restart-safety)

Operator chapters (wallet → GUI → clients → updates): **[docs/README.md](docs/README.md)**.  
Building / CI / env / layout: **[docs/06-developers.md](docs/06-developers.md)**.
