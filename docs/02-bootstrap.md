# 2. Bootstrap

[← Prerequisites](01-prerequisites.md) · [Guide home](README.md) · [Next: GUI →](03-gui.md)

First boot → funded wallet → GUI login. **No git clone** — download the published compose from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) and pull images from `ghcr.io`.

**Default path is SecretVM** ([Secret Labs](https://docs.scrt.network/)). Use
generic VPS if you already run your own Docker + TLS. **Railway** is a
scaffold overlay (same image pair; secrets in Railway Secrets only).

---

## Pick a compose path

Three overlays, **same GHCR image pair** (digest-pinned uplink + stock
proxy-router **v7.11.6-test**). Pick the skin that matches your host:

| Path | Best when | Release / scaffold |
|------|-----------|----------------|
| **SecretVM (recommended)** | Fastest first run — Encrypted Secrets + platform TLS (Traefik) | `docker-compose.secretvm.deployed.yml` + `env.secretvm.example` |
| **Generic VPS** | You already have a box / Phala / dstack — Caddy + Let’s Encrypt / `.env` | `docker-compose.generic.deployed.yml` + `env.generic.example` |
| **Railway (scaffold)** | Railway HTTPS edge; secrets in Railway Secrets only; single-replica proxy-router | `deploy/railway/` + [operator checklist](../deploy/railway/README.md) |

Images (already named in the YAML): `ghcr.io/morpheusais/uplink@sha…` (digest-pinned) · `ghcr.io/morpheusais/morpheus-lumerin-node:v7.11.6-test@sha256:da9890e376174d587d465c4d2679984939b085e06a3b34fc52a0ac9b19bb999f` (digest-pinned in compose)

**Pin note:** tag `v7.11.6-test` is the **Lumerin release channel** (includes [#889](https://github.com/MorpheusAIs/Morpheus-Lumerin-Node/pull/889) gateway caps early access) — **not** Base testnet. Chain ID / Diamond / MOR come from compose env and still default to **Base mainnet** (`8453`, Diamond `0x6aBE1d282f72B474E54527D93b979A4f64d3030a`, MOR `0x7431ada8a591c955a994a21710752ef9b882b8e3`).

Overlay detail: [deploy/secretvm](../deploy/secretvm/README.md) · [deploy/generic](../deploy/generic/README.md) · [deploy/railway](../deploy/railway/README.md).

Root README CTA: **[Get running on SecretVM](../README.md#get-running-secretvm)**.

---

## SecretVM disk hygiene

Small SecretVM instances have about 20 GB of disk. The published compose files
use Docker's `json-file` logging with rotation enabled by default at `20m` x 3
files per service, limiting retained container logs while keeping a short
history.

The Status tab (and Support section) shows free space for `/` and `DATA_DIR`
from inside the uplink process. When free space is below **2 GiB** or **15%**,
a warn banner appears. Admins can clear older usage-history rows via
**Clear older usage history** (`POST /admin/disk/prune-usage`, typed confirm
`PRUNE_USAGE`, retention default 30 days via `USAGE_PRUNE_DAYS`) — this never
deletes API keys or Docker logs. Docker json-file logs stay rotation-bounded;
if Status still warns after prune, the panel shows copy-paste host
`docker compose … --force-recreate --no-deps` commands (no auto-run).

This does **not** auto-wipe volumes. Do not schedule `docker system prune -a`
as a cron job, and do not hand-delete `*-json.log` files; investigate storage
and use the documented update path instead.

---

## Secrets

Generate a seed once: `openssl rand -hex 32`

<details>
<summary><strong>SecretVM — copyable secrets</strong></summary>

Paste into Encrypted Secrets (or the auto-built form). Replace the placeholders.

```bash
WALLET_PRIVATE_KEY=0xYOUR_PRIVATE_KEY
ETH_NODE_ADDRESS=https://base-mainnet.g.alchemy.com/v2/YOUR_ALCHEMY_KEY
COOKIE_CONTENT=admin:YOUR_STRONG_PASSWORD
ADMIN_PASSWORD=YOUR_GUI_PASSWORD
API_KEY_SEED=PASTE_openssl_rand_hex_32_HERE
WEB_PUBLIC_URL=https://your-vm-name.vm.scrtlabs.com
```

**Only these six** belong in Encrypted Secrets. SecretVM scrapes **every**
`environment:` key (literals too), so behavior bake stays in compose
`configs:`: `SESSION_DURATION_SECONDS=600`, `SESSION_FAILOVER=true`,
`ACTIVE_MODELS_URL` / `GATEWAY_BIDS_URL`, `HOUSEKEEPING=true`, `PROXY_*` /
`LOG_LEVEL_*`, plus Base chain ID / Diamond / MOR. Change those via code /
compose only — not operator forms.

</details>

<details>
<summary><strong>Generic VPS — copyable `.env`</strong></summary>

Save as `.env` next to the compose file. Replace the placeholders. `PUBLIC_HOST` must match DNS.

```bash
PUBLIC_HOST=uplink.yourdomain.com

WALLET_PRIVATE_KEY=0xYOUR_PRIVATE_KEY
ETH_NODE_ADDRESS=https://base-mainnet.g.alchemy.com/v2/YOUR_ALCHEMY_KEY
COOKIE_CONTENT=admin:YOUR_STRONG_PASSWORD
ADMIN_PASSWORD=YOUR_GUI_PASSWORD
API_KEY_SEED=PASTE_openssl_rand_hex_32_HERE
WEB_PUBLIC_URL=https://uplink.yourdomain.com
```

Behavior knobs (session / catalog / housekeeping / `PROXY_*` / `LOG_LEVEL_*`)
are baked in compose — change via code only.

</details>

<details>
<summary><strong>Railway — secrets only in Railway Secrets</strong></summary>

Do **not** put wallet/admin/seed/cookie/RPC/`WEB_PUBLIC_URL` material in
`railway.toml`, image labels, build args, or committed compose. Set these
six as **Railway Secrets** / vars (same surface as SecretVM / generic):

- `WALLET_PRIVATE_KEY`
- `ADMIN_PASSWORD`
- `API_KEY_SEED`
- `COOKIE_CONTENT`
- RPC credentials (`ETH_NODE_ADDRESS` and any provider key material)
- `WEB_PUBLIC_URL` (Railway HTTPS origin)

Behavior knobs are baked in compose. Full checklist:
[deploy/railway/README.md](../deploy/railway/README.md).

</details>

<details>
<summary><strong>What each required var does</strong></summary>

| Var | Used by | Notes |
|-----|---------|--------|
| `WALLET_PRIVATE_KEY` | Router (+ Uplink reclaim) | Prefer a **dedicated** consumer wallet; fund from Rabby/MetaMask. |
| `ETH_NODE_ADDRESS` | Router + Uplink | Base HTTPS RPC. |
| `COOKIE_CONTENT` | Router + Uplink | `user:password` Basic auth for the router API. Same value in both containers. |
| `ADMIN_PASSWORD` | Uplink | GUI login (username is always `admin`). |
| `API_KEY_SEED` | Uplink | Hex seed for Master/Prompt. Keep it; rotating changes both built-in keys. |
| `WEB_PUBLIC_URL` | Router (+ Uplink) | Public HTTPS origin (`https://….vm.scrtlabs.com`, `https://$PUBLIC_HOST`, or Railway URL). |
| `PUBLIC_HOST` | Caddy (generic only) | DNS name for Let’s Encrypt (platform-specific; not one of the six). |

</details>

---

## First start — SecretVM

1. Create a SecretVM that accepts Docker Compose + encrypted secrets.
2. Download **`docker-compose.secretvm.deployed.yml`** from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) and paste it as the VM compose.
3. Fill Encrypted Secrets from the SecretVM block above (**six keys only**, including `WEB_PUBLIC_URL=https://<vm-host>`).
4. Deploy. Wait for health (`uplink … listening`, router healthy).
5. Open `https://<that-host>/gui/` — `admin` / your `ADMIN_PASSWORD`.

---

## First start — generic VPS

DNS A/AAAA → this machine; ports **80** and **443** open. Then:

```bash
mkdir -p uplink && cd uplink

curl -fsSL -O https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest/download/docker-compose.generic.deployed.yml
curl -fsSL -o .env https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest/download/env.generic.example

# Edit .env — fill PUBLIC_HOST + the six required fillables (see Secrets above)
nano .env   # or your editor

docker compose -f docker-compose.generic.deployed.yml --env-file .env up -d
```

Open `https://$PUBLIC_HOST/gui/`.

---

## First start — Railway

Scaffold only — same digests as SecretVM/generic; pin images; single-replica
proxy-router. Full step→done checklist:
**[deploy/railway/README.md](../deploy/railway/README.md)**.

1. Create a Railway project with uplink + proxy-router on a private network.
2. Set secret-class vars as **Railway Secrets** only (see Secrets above).
3. Pin uplink (and router) to release digests — not bare `:latest`.
4. Replicas = **1** for wallet-bearing `proxy-router`.
5. Confirm only uplink is public (`:8080` behind Railway HTTPS).
6. Open `https://<public>/gui/` → fund wallet → Probe (same as other hosts).

Done when GUI login works and Probe returns a completion.

---

## Fund the consumer wallet

On **Status**, copy the wallet address. From Rabby/MetaMask on Base, send:

1. A little **ETH** (gas).
2. Enough **MOR** for sessions you will open (see [how close vs expire affects stake](03-gui.md#where-is-my-mor)).

Use **Get MOR on Uniswap** on Status if you need a swap deep-link.

---

[← Prerequisites](01-prerequisites.md) · [Guide home](README.md) · [Next: GUI →](03-gui.md)
