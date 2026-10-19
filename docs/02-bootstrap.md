# 2. Bootstrap

[← Prerequisites](01-prerequisites.md) · [Guide home](README.md) · [Next: GUI →](03-gui.md)

First boot → funded wallet → GUI login. **No git clone** — download the published compose from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) and pull images from `ghcr.io`.

**Default path is SecretVM** ([Secret Labs](https://docs.scrt.network/)). Use
generic VPS if you already run your own Docker + TLS. **Railway** is a
scaffold overlay (same image pair; secrets in Railway Secrets only).
Supported hosts: SecretVM, generic VPS, and this scaffold only -- not third-party
consumer-gateway Railway (no DOMAIN / PUBLIC_ORIGIN operator fields).

---

## Pick a compose path

Three overlays, **same GHCR image pair** (digest-pinned uplink + stock
proxy-router **v7.14.0**). Pick the skin that matches your host:

| Path | Best when | Release / scaffold |
|------|-----------|----------------|
| **SecretVM (recommended)** | Fastest first run — Encrypted Secrets + platform TLS (Traefik) | `docker-compose.secretvm.deployed.yml` + `env.secretvm.example` |
| **Generic VPS** | You already have a box / Phala / dstack — Caddy + Let’s Encrypt / `.env` | `docker-compose.generic.deployed.yml` + `env.generic.example` |
| **Railway (scaffold)** | Railway HTTPS edge; secrets in Railway Secrets only; single-replica proxy-router | `deploy/railway/` + [operator checklist](../deploy/railway/README.md) |

Images (already named in the YAML): `ghcr.io/morpheusais/uplink@sha…` (digest-pinned) · `ghcr.io/morpheusais/morpheus-lumerin-node:v7.14.0@sha256:cc7948399212dd4b2ab89ead5daeb8bac3b25f973a92a5f7928c72bba78ff556` (digest-pinned in compose)

**Pin note:** tag `v7.14.0` is **Lumerin `main`** (includes [#889](https://github.com/MorpheusAIs/Morpheus-Lumerin-Node/pull/889) gateway caps) — **not** Base testnet. Chain ID / Diamond / MOR come from compose env and still default to **Base mainnet** (`8453`, Diamond `0x6aBE1d282f72B474E54527D93b979A4f64d3030a`, MOR `0x7431ada8a591c955a994a21710752ef9b882b8e3`).

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
`environment:` key (literals too), so tunables stay in compose `configs:`
(change via code / compose only -- not operator forms):

- **Uplink** (`uplink_bake_env`): `SESSION_DURATION_SECONDS=600`,
  `SESSION_FAILOVER=true`, `ACTIVE_MODELS_URL` / `GATEWAY_BIDS_URL`,
  `HOUSEKEEPING=true`. Wiring defaults (`UPLINK_LISTEN`, `ROUTER_URL`,
  `DATA_DIR`, `USAGE_PRUNE_DAYS`,
  `CLOSE_SESSIONS_ON_EXIT`) are Uplink process env too. Diamond /
  `ETH_NODE_CHAIN_ID` are **not** Uplink operator knobs.
- **Router** (`router_network_env`; godotenv -- process env wins):
  `PROXY_STORE_CHAT_CONTEXT`, `PROXY_FORWARD_CHAT_CONTEXT`,
  `LOG_LEVEL_APP` / `LOG_LEVEL_TCP` / `LOG_LEVEL_ETH_RPC`,
  `ETH_NODE_CHAIN_ID`, `ETH_NODE_USE_SUBSCRIPTIONS`, `BLOCKSCOUT_API_URL`,
  `DIAMOND_CONTRACT_ADDRESS`, `MOR_TOKEN_ADDRESS`. Never put router
  `PROXY_*` / `LOG_LEVEL_*` into Uplink bake.

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

Tunables are baked in compose (change via code only). Keep ownership separate:
**Uplink** session/catalog/housekeeping (+ wiring defaults);
**Router** `PROXY_*` / `LOG_LEVEL_*` / chain / Diamond / MOR. Never describe
router `PROXY_*` / `LOG_LEVEL_*` as Uplink knobs.

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

Tunables are baked in compose (Uplink vs router ownership stays separate;
see SecretVM block above). Full checklist:
[deploy/railway/README.md](../deploy/railway/README.md).

</details>

<details>
<summary><strong>Var ownership (Uplink vs router)</strong></summary>

Product rule: **form stays six**; tunables change via compose/code, not Encrypted Secrets.

| Bucket | Where | Vars |
|--------|-------|------|
| **Minimum to run** | Operator fillables everywhere | `WALLET_PRIVATE_KEY`, `ETH_NODE_ADDRESS`, `COOKIE_CONTENT`, `ADMIN_PASSWORD`, `API_KEY_SEED`, `WEB_PUBLIC_URL` |
| **Uplink tunable / additional** | Uplink process only (`uplink_bake_env` / `internal/config`) | `ACTIVE_MODELS_URL`, `GATEWAY_BIDS_URL`, `SESSION_DURATION_SECONDS`, `SESSION_FAILOVER`, `HOUSEKEEPING`; wiring defaults `UPLINK_LISTEN`, `ROUTER_URL`, `DATA_DIR`, `USAGE_PRUNE_DAYS`, `CLOSE_SESSIONS_ON_EXIT` |
| **Router tunable / additional** | proxy-router only (`router_network_env`; godotenv -- process env wins) | `PROXY_STORE_CHAT_CONTEXT`, `PROXY_FORWARD_CHAT_CONTEXT`, `LOG_LEVEL_APP` / `TCP` / `ETH_RPC`, `ETH_NODE_CHAIN_ID`, `ETH_NODE_USE_SUBSCRIPTIONS`, `BLOCKSCOUT_API_URL`, `DIAMOND_CONTRACT_ADDRESS`, `MOR_TOKEN_ADDRESS` |

Diamond / `ETH_NODE_CHAIN_ID` may exist in Uplink config defaults but are **not** Uplink operator knobs. Never put router `PROXY_*` / `LOG_LEVEL_*` into Uplink bake docs.

</details>

<details>
<summary><strong>What each required var does</strong></summary>

| Var | Used by | Notes |
|-----|---------|--------|
| `WALLET_PRIVATE_KEY` | Router only | Prefer a **dedicated** consumer wallet; fund from Rabby/MetaMask. Uplink holds no key; stake reclaim runs via the router API. |
| `ETH_NODE_ADDRESS` | Router + Uplink | Base HTTPS RPC. |
| `COOKIE_CONTENT` | Router + router-user-init | Router **admin** `user:password`. Only proxy-router and the init step see it; init mints a scoped `uplink` router user and Uplink uses that (never the admin cookie). |
| `ADMIN_PASSWORD` | Uplink | GUI login (username is always `admin`). |
| `API_KEY_SEED` | Uplink | Hex seed for Master/Prompt. Keep it; rotating changes both built-in keys. |
| `WEB_PUBLIC_URL` | Router (+ Uplink) | Public HTTPS origin (`https://….vm.scrtlabs.com`, `https://$PUBLIC_HOST`, or Railway URL). |
| `PUBLIC_HOST` | Caddy (generic only) | DNS name for Let’s Encrypt (platform-specific; not one of the six). |

</details>

<details>
<summary><strong>MOR allowance (locks, not spends; optional cap)</strong></summary>

Uplink talks to the router as a scoped `uplink` user (never the admin
cookie).

> **MOR allowance.** The gateway's router user can only open inference sessions; it cannot send MOR or ETH, change approvals, or read the wallet key. A session stake is locked while the session runs and is returned to this wallet, held at most until the next UTC day. Sessions cost ETH gas.
> Empty `UPLINK_MOR_ALLOWANCE_WEI`: the gateway may lock up to the live wallet MOR balance (new deposits are included within ~2 min). Set a value in wei to cap how much MOR it can lock in total; `0` disables session opening.
> Note: the router also supports "direct payment" sessions, where the provider is paid from the stake (at most `getTodaysBudget / totalMORSupply` of it, about 0.3% today). The gateway itself never uses it, but whoever holds the router credential could. A cap bounds that worst case to the same fraction of the cap.

**High-value wallets: use cap mode.** Set `UPLINK_MOR_ALLOWANCE_WEI`; a cap
bounds the direct-payment worst case to that fraction of the cap.

- **Empty / unset** (balance mode, default): the `router-allowance-manager`
  service keeps the allowance equal to the live wallet MOR balance. It polls
  every 60 s, includes new deposits within ~2 min and never changes the
  password. It also overwrites manual router-side allowance edits; for a
  fixed budget set a cap. The GUI's **Resync allowance now** button asks for
  an immediate resync.
- **Set** (digits only, wei; no internal spaces): fixed cap; the manager never
  changes it. Example: `1000000000000000000000` = 1000 MOR. Leading zeros are
  ignored.
- **`0`**: session opening disabled. The balance is not read.
- A failed or zero balance read is skipped (the manager never writes 0). At
  boot, init falls back to 0 with a warning.

The effective allowance (wei + source: `balance`, `override`, `zero`,
`ceiling` or `unknown`) and the auto-resync state are shown on the GUI Status
and Security tabs and returned by `/admin/status` (`morAllowance`,
`allowanceResync`), and logged once at boot (`mor allowance` line +
`allowance` audit event; no secrets).

Credentials at rest:

- **Scoped router credential** (`/shared/router.auth`, plus Uplink's copy in
  `/data`). Init keeps it as long as the router still accepts it and only
  updates the allowance, so it is **not** rotated on every run. Init replaces
  it only when the router rejects it or when `/shared/router.auth` is
  missing.
- **Admin cookie for the manager** (`/mgr/admin.cookie`, 0400, uid 10002) on
  the manager-only `manager_cred` volume; only init (read-write) and the
  manager (read-only) mount it. Init overwrites it on every run. It persists
  until the `manager_cred` volume is removed (`docker compose down -v` or
  `docker volume rm <project>_manager_cred`). It is the same exposure class as
  proxy-router's own `/app/data/.cookie`.

**Forcing a new scoped credential** (needs `docker compose` access to the
host). Remove the shared copy, then re-run init; init deletes the router
user, mints a new password, and `restart: true` restarts Uplink and the
manager so they pick it up:

```sh
docker compose run --rm --no-deps --entrypoint rm router-user-init -f /shared/router.auth
docker compose up -d --force-recreate router-user-init
```

Removing only the `router_auth` volume (`docker compose down`, then
`docker volume rm <project>_router_auth`, then `docker compose up -d`) has
the same effect. Do not use `docker compose down -v` for this: it also
deletes Uplink's data and the router's data volumes.

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
