# 2. Bootstrap

[← Prerequisites](01-prerequisites.md) · [Guide home](README.md) · [Next: GUI →](03-gui.md)

First boot → funded wallet → GUI login. **No git clone** — download the published compose from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) and pull images from `ghcr.io`.

**Default path is SecretVM** ([Secret Labs](https://docs.scrt.network/)). Use
generic VPS if you already run your own Docker + TLS. **Railway** is a
scaffold overlay (same image pair; secrets in Railway Secrets only; the
release ships a digest-pinned reference compose for it).
Supported hosts: SecretVM, generic VPS, and this scaffold only -- not third-party
consumer-gateway Railway (no DOMAIN / PUBLIC_ORIGIN operator fields).

---

## Pick a compose path

Three overlays, **same GHCR image pair** (digest-pinned uplink + stock
proxy-router **v7.15.0**). Pick the skin that matches your host:

| Path | Best when | Release / scaffold |
|------|-----------|----------------|
| **SecretVM (recommended)** | Fastest first run — Encrypted Secrets + platform TLS (Traefik) | `docker-compose.secretvm.deployed.yml` + `env.secretvm.example` |
| **Generic VPS** | You already have a box / Phala / dstack — Caddy + Let’s Encrypt / `.env` | `docker-compose.generic.deployed.yml` + `env.generic.example` |
| **Railway (scaffold)** | Railway HTTPS edge; secrets in Railway Secrets only; single-replica proxy-router | `docker-compose.railway.deployed.yml` + `env.railway.example` (reference; Railway does not run compose) + [operator checklist](../deploy/railway/README.md) |

Images (already named in the YAML): `ghcr.io/morpheusais/uplink@sha…` (digest-pinned) · `ghcr.io/morpheusais/morpheus-lumerin-node:v7.15.0@sha256:c52e9f12725d7c85e09eec11e0c06ae2b0970d12d1c770dc4daa4cc66f961ea4` (digest-pinned in compose). Every image in the release compose is digest-pinned; none is `:latest`.

**Pin note:** tag `v7.15.0` is **Lumerin `main`** (includes [#889](https://github.com/MorpheusAIs/Morpheus-Lumerin-Node/pull/889) gateway caps) — **not** Base testnet. Chain ID / Diamond / MOR come from compose env and still default to **Base mainnet** (`8453`, Diamond `0x6aBE1d282f72B474E54527D93b979A4f64d3030a`, MOR `0x7431ada8a591c955a994a21710752ef9b882b8e3`).

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

> To change a SecretVM tunable, edit the `configs:` block
> (`uplink_bake_env` / `router_network_env`) in
> `deploy/secretvm/docker-compose.yml` (or your downloaded
> `docker-compose.secretvm.deployed.yml`). SecretVM turns every `${...}` in
> the compose file into an operator prompt, and the form deliberately stays
> at the six secrets, so SecretVM has no `${VAR:-default}` overrides.

- **Uplink** (`uplink_bake_env`): `SESSION_DURATION_SECONDS=600`,
  `SESSION_FAILOVER=true`, `ACTIVE_MODELS_URL` / `GATEWAY_BIDS_URL`,
  `HOUSEKEEPING=true`, and master key admin access fixed off
  (`MASTER_KEY_ADMIN=false`). Wiring defaults (`UPLINK_LISTEN`, `ROUTER_URL`,
  `DATA_DIR`, `USAGE_PRUNE_DAYS`,
  `CLOSE_SESSIONS_ON_EXIT`) are Uplink process env too. Diamond /
  `ETH_NODE_CHAIN_ID` are **not** Uplink operator knobs.
  2FA is turned on in the GUI, not here (see [2FA](#admin-2fa-switch)).
  2FA emergency recovery: add `UPLINK_RESET_2FA=true` to `uplink_bake_env`,
  redeploy once, then remove it and redeploy
  ([details](06-developers.md#admin-2fa-and-recovery-codes)).
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

Tunables are env-overridable compose defaults (`${VAR:-default}`): unset
keeps the default; set one in `.env` to override (the list is in
[06](06-developers.md#environment-variables)). Master key admin access
defaults to off (`MASTER_KEY_ADMIN=false`); turning it on weakens admin
security. 2FA is turned on in the GUI (see [2FA](#admin-2fa-switch)).
2FA emergency recovery: `UPLINK_RESET_2FA=true` in `.env` for one restart,
then remove it. Keep ownership separate:
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

Railway does not run compose, so no compose default applies there: an
unset variable takes the process's own default, which can differ from the
compose value (table in [deploy/railway/README.md](../deploy/railway/README.md#unset-on-railway)).
Set plain values as Railway Variables (Uplink vs router ownership stays
separate; see SecretVM block above). On proxy-router set
`PROXY_STORE_CHAT_CONTEXT=false` (unset stores chat context),
`LOG_LEVEL_APP=warn`, `ETH_NODE_CHAIN_ID=8453` and the chain addresses from
`env.railway.example`. Then turn 2FA on in the GUI (recommended; see
[2FA](#admin-2fa-switch)), and keep `DATA_DIR` on a persistent volume so the
2FA switch and enrollment survive redeploys. Full checklist:
[deploy/railway/README.md](../deploy/railway/README.md).

</details>

<details>
<summary><strong>Var ownership (Uplink vs router)</strong></summary>

Product rule: **form stays six**; tunables are not Encrypted Secrets. SecretVM keeps them in compose `configs:`; generic and local compose take them as env-overridable defaults (`${VAR:-default}`); Railway does not run compose and applies none of them.

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
| `ETH_NODE_ADDRESS` | Router only | Base HTTPS RPC. Uplink reads chain state through the router API. |
| `COOKIE_CONTENT` | Router + Uplink | Router `user:password`. Uplink uses it to call the router over the internal network; the router port is never published. |
| `ADMIN_PASSWORD` | Uplink | GUI login (username is always `admin`). |
| `API_KEY_SEED` | Uplink | Hex seed for Master/Prompt. Keep it; rotating changes both built-in keys and invalidates the 2FA secret and recovery codes (way back: `UPLINK_RESET_2FA=true`, see [06](06-developers.md#admin-2fa-and-recovery-codes)). |
| `WEB_PUBLIC_URL` | Router (+ Uplink) | Public HTTPS origin (`https://….vm.scrtlabs.com`, `https://$PUBLIC_HOST`, or Railway URL). |
| `PUBLIC_HOST` | Caddy (generic only) | DNS name for Let’s Encrypt (platform-specific; not one of the six). |

</details>

<details>
<summary><strong>Session stake (locks, not spends)</strong></summary>

A session stake is locked while the session runs and is returned to this
wallet, held at most until the next UTC day. Sessions cost ETH gas. Uplink
only opens staking-mode sessions (`directPayment` is always false). Use a
dedicated wallet: the boot reconcile and a graceful stop act on every open
session of the wallet (see [06-developers](06-developers.md#sessions-across-restarts)).

</details>

---

## First start — SecretVM

1. Create a SecretVM that accepts Docker Compose + encrypted secrets.
2. Download **`docker-compose.secretvm.deployed.yml`** from the [latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest) and paste it as the VM compose.
3. Fill Encrypted Secrets from the SecretVM block above (**six keys only**, including `WEB_PUBLIC_URL=https://<vm-host>`).
4. Deploy. Wait until `uplink` is healthy (proxy-router has no container
   healthcheck); after login, the GUI Status line should read Healthy.
5. Open `https://<that-host>/gui/` — `admin` / your `ADMIN_PASSWORD`.
6. Turn 2FA on (recommended): Security tab → 2FA switch → scan the QR
   code, confirm a code, and save the 10 recovery codes (shown once).
   See [2FA](#admin-2fa-switch).

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

Open `https://$PUBLIC_HOST/gui/` and turn 2FA on with the switch on the
Security tab (recommended; off by default, as on every deploy).

---

## First start — Railway

Scaffold — the release's `docker-compose.railway.deployed.yml` is the
reference (same digests as SecretVM/generic); single-replica
proxy-router. Full step→done checklist:
**[deploy/railway/README.md](../deploy/railway/README.md)**.

1. Create a Railway project with uplink + proxy-router on a private network.
2. Set secret-class vars as **Railway Secrets** only (see Secrets above).
3. Use the uplink and router digests from `docker-compose.railway.deployed.yml` — not bare `:latest`.
4. Replicas = **1** for wallet-bearing `proxy-router`.
5. Confirm only uplink is public (`:8080` behind Railway HTTPS).
6. Open `https://<public>/gui/`, turn 2FA on with the switch on the
   Security tab (recommended; off by default), then fund wallet → Probe
   (same as other hosts).

Done when GUI login works and Probe returns a completion.

---

## Admin 2FA switch

2FA (TOTP) is an operator choice in the GUI, the same on SecretVM,
generic, Railway and local: there is no environment variable for it.

- **Default off.** The first login is password only. While 2FA is off the
  Security tab shows a short note recommending turning it on; nothing is
  forced.
- **Turn on:** Security tab → 2FA switch. Scan the QR code (or enter the
  secret), confirm a code, and save the 10 recovery codes (shown once).
  2FA is on only once a code is confirmed; from then on every login needs a
  TOTP or recovery code.
- **Turn off:** the switch asks for the password plus a current TOTP or
  recovery code. It removes the secret and recovery codes and signs out
  every session; login is password only again.
- **Reset lockdown:** after a Reset the switch stays on, and the next
  password login gets 403 on every admin route except 2FA status,
  enrollment and logout until a new enrollment is confirmed.
- **Persistent:** the switch is stored with the 2FA state in `DATA_DIR`
  (`uplink-state.json`), so it survives restarts and redeploys. Keep
  `DATA_DIR` on a persistent volume.
- **Emergency recovery** (authenticator and recovery codes lost):
  `UPLINK_RESET_2FA=true` for one restart wipes 2FA and all sessions; 2FA
  comes back off. Remove the variable, log in with the password and turn
  2FA on again ([details](06-developers.md#admin-2fa-and-recovery-codes)).

---

## Fund the consumer wallet

On **Status**, copy the wallet address. From Rabby/MetaMask on Base, send:

1. A little **ETH** (gas).
2. Enough **MOR** for sessions you will open (see [how close vs expire affects stake](03-gui.md#where-is-my-mor)).

Use **Get MOR on Uniswap** on Status if you need a swap deep-link.

---

[← Prerequisites](01-prerequisites.md) · [Guide home](README.md) · [Next: GUI →](03-gui.md)
