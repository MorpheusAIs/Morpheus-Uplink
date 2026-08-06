# 2. Bootstrap

[← Prerequisites](01-prerequisites.md) · [Guide home](README.md) · [Next: GUI →](03-gui.md)

First boot → funded wallet → GUI login. **No git clone** — download the published compose from the [latest release](https://github.com/absgrafx/Morpheus-Uplink/releases/latest) and pull images from `ghcr.io`.

---

## Pick a compose path

| Path | Best when | Release assets |
|------|-----------|----------------|
| **SecretVM** | Encrypted secrets UI + platform TLS | `docker-compose.secretvm.deployed.yml` + `env.secretvm.example` |
| **Any Docker VPS** | You already have a box / Phala / dstack | `docker-compose.generic.deployed.yml` + `env.generic.example` |

Images (already named in the YAML): `ghcr.io/absgrafx/uplink@sha…` (digest-pinned) · `ghcr.io/morpheusais/morpheus-lumerin-node:latest`

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
WEB_PUBLIC_URL=https://localhost
```

Optional (leave commented / omit unless you need them):

```bash
#SESSION_DURATION_SECONDS=3600
#HOUSEKEEPING=true
```

Base network constants (chain ID, Diamond, MOR token) live in the compose `configs` block — **not** Encrypted Secrets.

</details>

<details>
<summary><strong>Generic VPS — copyable `.env`</strong></summary>

Save as `.env` next to the compose file. Replace the placeholders. `PUBLIC_HOST` must match DNS.

```bash
PUBLIC_HOST=uplink.yourdomain.com

WALLET_PRIVATE_KEY=0xYOUR_PRIVATE_KEY
ETH_NODE_ADDRESS=https://base-mainnet.g.alchemy.com/v2/YOUR_ALCHEMY_KEY
ETH_NODE_CHAIN_ID=8453
COOKIE_CONTENT=admin:YOUR_STRONG_PASSWORD
ADMIN_PASSWORD=YOUR_GUI_PASSWORD
API_KEY_SEED=PASTE_openssl_rand_hex_32_HERE

#SESSION_DURATION_SECONDS=3600
#HOUSEKEEPING=true
#SESSION_FAILOVER=true
```

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
| `PUBLIC_HOST` | Caddy (generic only) | DNS name for Let’s Encrypt. |
| `WEB_PUBLIC_URL` | Router (SecretVM) | Public HTTPS origin; set after you know the VM hostname. |

</details>

---

## First start — SecretVM

1. Create a SecretVM that accepts Docker Compose + encrypted secrets.
2. Download **`docker-compose.secretvm.deployed.yml`** from the [latest release](https://github.com/absgrafx/Morpheus-Uplink/releases/latest) and paste it as the VM compose.
3. Fill Encrypted Secrets from the SecretVM block above (`WEB_PUBLIC_URL=https://localhost` is fine for first boot).
4. Deploy. Wait for health (`uplink … listening`, router healthy).
5. Copy the public HTTPS hostname (e.g. `https://something.vm.scrtlabs.com`).
6. Set `WEB_PUBLIC_URL=https://<that-host>` and restart once.
7. Open `https://<that-host>/gui/` — `admin` / your `ADMIN_PASSWORD`.

---

## First start — generic VPS

DNS A/AAAA → this machine; ports **80** and **443** open. Then:

```bash
mkdir -p uplink && cd uplink

curl -fsSL -O https://github.com/absgrafx/Morpheus-Uplink/releases/latest/download/docker-compose.generic.deployed.yml
curl -fsSL -o .env https://github.com/absgrafx/Morpheus-Uplink/releases/latest/download/env.generic.example

# Edit .env — fill PUBLIC_HOST + the five required secrets (see Secrets above)
nano .env   # or your editor

docker compose -f docker-compose.generic.deployed.yml --env-file .env up -d
```

Open `https://$PUBLIC_HOST/gui/`.

---

## Fund the consumer wallet

On **Status**, copy the wallet address. From Rabby/MetaMask on Base, send:

1. A little **ETH** (gas).
2. Enough **MOR** for sessions you will open (see [how close vs expire affects stake](03-gui.md#where-is-my-mor)).

Use **Get MOR on Uniswap** on Status if you need a swap deep-link.

---

[← Prerequisites](01-prerequisites.md) · [Guide home](README.md) · [Next: GUI →](03-gui.md)
