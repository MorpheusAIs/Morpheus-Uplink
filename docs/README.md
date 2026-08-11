# Uplink operator guide

**Uplink** is your personal gateway to the [Morpheus](https://mor.org) network: an OpenAI-compatible HTTPS endpoint backed by your own consumer proxy-router (C-Node) and wallet.

AI agents: read root [`AGENTS.md`](../AGENTS.md) first (hard rules + [`llms-full.txt`](../llms-full.txt)).

**Before you fund anything:** [DISCLAIMER.md](../DISCLAIMER.md) (self-custody, shared wallet, third-party inference). License: [MIT](../LICENSE).

Developer / CI detail stays in the root [README](../README.md).

---

## Pick a chapter

| | Chapter | When you need it |
|---|---------|------------------|
| **1** | [Prerequisites](01-prerequisites.md) | Before you touch compose — host, wallet, ETH/MOR, RPC, seed |
| **2** | [Bootstrap](02-bootstrap.md) | First deploy (SecretVM or generic VPS) → fund wallet → open GUI |
| **3** | [GUI basics](03-gui.md) | Login, keys, MOR buckets, Probe, support bundle |
| **4** | [Apps & agents](04-clients.md) | Point Cursor / SDKs / agents at `…/v1` |
| **5** | [Updates & portability](05-updates.md) | New releases, redeploy, move hosts |

---

## Quick checklist

- [ ] Wallet funded (ETH + MOR on Base)
- [ ] RPC URL works
- [ ] Compose + env from **[latest release](https://github.com/absgrafx/Morpheus-Uplink/releases/latest)** (no git clone)
- [ ] Mandatory secrets set; `WEB_PUBLIC_URL` / `PUBLIC_HOST` matches DNS
- [ ] `/gui` login works
- [ ] Probe returns a completion
- [ ] Client base URL `…/v1` + Prompt/ephemeral key + real model name
- [ ] Ephemeral keys exported if you care about them across redeploys

---

Suggested path: **1 → 2 → 3 → Probe once → 4**. Open **5** when a new release ships or you change hosts.
