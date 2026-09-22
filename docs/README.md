# Uplink operator guide

**Uplink** is your personal gateway to the [Morpheus](https://mor.org) network: an OpenAI-compatible HTTPS endpoint backed by your own consumer proxy-router (C-Node) and wallet.

**Recommended host:** [SecretVM](https://docs.scrt.network/) (Secret Labs) —
encrypted secrets + platform TLS. Root README has the fast CTA:
**[Get running on SecretVM](../README.md#get-running-secretvm--recommended)**.

AI agents: read root [`AGENTS.md`](../AGENTS.md) first (hard rules + [`llms-full.txt`](../llms-full.txt)).

**Before you fund anything:** [DISCLAIMER.md](../DISCLAIMER.md) (self-custody, shared wallet, third-party inference). License: [MIT](../LICENSE).

---

## Fast path (SecretVM)

1. [Prerequisites](01-prerequisites.md) — wallet, ETH/MOR on Base, RPC, seed  
2. [Bootstrap → First start — SecretVM](02-bootstrap.md#first-start--secretvm) — release compose + Encrypted Secrets  
3. [GUI](03-gui.md) — login, fund Status wallet, **Probe** once  
4. [Apps & agents](04-clients.md) — base URL `https://<host>/v1` + Prompt key  

Open [Updates](05-updates.md) when a new release ships. Other hosts share the
same GHCR image pair:

- **Generic VPS** (Caddy + LE) — [Bootstrap](02-bootstrap.md#first-start--generic-vps) · [deploy/generic](../deploy/generic/README.md)
- **Railway** (scaffold) — [Bootstrap](02-bootstrap.md#first-start--railway) · [deploy/railway](../deploy/railway/README.md)

SecretVM overlay notes: [deploy/secretvm](../deploy/secretvm/README.md). Local
build / CI / env tables: [Developers](06-developers.md).

---

## Chapters

| | Chapter | When you need it |
|---|---------|------------------|
| **1** | [Prerequisites](01-prerequisites.md) | Host, wallet, ETH/MOR, RPC, seed |
| **2** | [Bootstrap](02-bootstrap.md) | SecretVM · generic VPS · Railway scaffold → fund → GUI |
| **3** | [GUI basics](03-gui.md) | Keys, MOR buckets, Probe, support bundle |
| **4** | [Apps & agents](04-clients.md) | Cursor / SDKs / agents → `…/v1` |
| **5** | [Updates & portability](05-updates.md) | New releases, redeploy, move hosts |
| **6** | [Developers](06-developers.md) | Local run, env vars, CI/CD, layout |

---

## Quick checklist

- [ ] Compose from **[latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest)** (no git clone) — or Railway scaffold under `deploy/railway/`
- [ ] Six operator fillables set (SecretVM Encrypted Secrets / generic `.env` + `PUBLIC_HOST` / Railway Secrets) including `WEB_PUBLIC_URL`
- [ ] `/gui` login works
- [ ] Wallet funded (ETH + MOR on Base)
- [ ] Probe returns a completion
- [ ] Client: `…/v1` + Prompt/ephemeral key + exact model name
