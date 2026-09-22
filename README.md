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
| **3. Secrets** | Fill Encrypted Secrets (block below). First boot may use `WEB_PUBLIC_URL=https://localhost`. |
| **4. Deploy** | Start the VM; wait until Uplink and the router are healthy. |
| **5. Hostname** | Set `WEB_PUBLIC_URL=https://….vm.scrtlabs.com` to your public host, restart once. |
| **6. GUI** | Open `https://<host>/gui/` → `admin` / your `ADMIN_PASSWORD`. |
| **7. Fund** | On **Status**, copy the wallet → send a little **ETH** + **MOR** on Base. |
| **8. Use** | **Probe** once, then put the **Prompt** key in any OpenAI client with base URL `https://<host>/v1`. |

<details>
<summary><strong>Encrypted Secrets (copy / paste)</strong></summary>

```bash
WALLET_PRIVATE_KEY=0xYOUR_PRIVATE_KEY
ETH_NODE_ADDRESS=https://base-mainnet.g.alchemy.com/v2/YOUR_ALCHEMY_KEY
COOKIE_CONTENT=admin:YOUR_STRONG_PASSWORD
ADMIN_PASSWORD=YOUR_GUI_PASSWORD
API_KEY_SEED=PASTE_openssl_rand_hex_32_HERE
WEB_PUBLIC_URL=https://localhost
```

`openssl rand -hex 32` for the seed. Prefer a **dedicated** consumer wallet.
Full walkthrough (and plain Docker VPS): [docs/02-bootstrap.md](docs/02-bootstrap.md).

</details>

**Other hosts:** [Generic VPS](docs/02-bootstrap.md#first-start--generic-vps) (Caddy + LE) · [Railway scaffold](docs/02-bootstrap.md#first-start--railway) — same GHCR digests; see [docs/02-bootstrap.md](docs/02-bootstrap.md) and [deploy/](deploy/).

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

| Env | Default |
|-----|---------|
| `ACTIVE_MODELS_URL` | `https://active.mor.org/gateway_models.json` |
| `GATEWAY_BIDS_URL` | `https://active.mor.org/gateway_bids.json` |

Release compose **digest-pins proxy-router (Lumerin) at v7.11.6-test**
(Lumerin release channel / #889 caps early access — **not** Base testnet) — not
`:latest`. More clients: [docs/04-clients.md](docs/04-clients.md).

## Things worth knowing

- **Not the hosted APIGW.** No Morpheus credits account, no shared pool with
  strangers. One wallet, one gateway, your ops.
- **Any Prompt / ephemeral key can lock MOR** from that wallet. Treat keys like
  spend authority; keep Master off shared machines.
- **Built-in keys survive a wipe** if you keep the same `API_KEY_SEED`.
  Generated (ephemeral) keys and local usage history do not — export them if
  you care. Sessions and stake live on-chain with the wallet.
- **Deploy from release assets**, not a git checkout. Images are digest-pinned
  for SecretVM / compose — including **proxy-router v7.11.6-test** (not `:latest`).
- **Gateway catalog only:** `ACTIVE_MODELS_URL` →
  `https://active.mor.org/gateway_models.json`, `GATEWAY_BIDS_URL` →
  `https://active.mor.org/gateway_bids.json`. Never default to
  `active_models.json` / ALL.
- **Only expose the gateway** (HTTPS). The router admin port stays private.

Operator chapters (wallet → GUI → clients → updates): **[docs/README.md](docs/README.md)**.  
Building / CI / env / layout: **[docs/06-developers.md](docs/06-developers.md)**.
