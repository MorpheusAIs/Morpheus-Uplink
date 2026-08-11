# 1. Prerequisites

[← Guide home](README.md) · [Next: Bootstrap →](02-bootstrap.md)

What you need before compose. Skip anything you already have.

Read the short [DISCLAIMER](../DISCLAIMER.md) first — self-custody, experimental software, and the fact that **any inference key can escrow MOR** from this wallet.

| Need | Why | Links |
|------|-----|--------|
| **Host that runs Docker Compose + HTTPS** | Runs Uplink + proxy-router + TLS | **Preferred:** [SecretVM](https://docs.scrt.network/) (Secret Labs — Encrypted Secrets + platform TLS). Alternate: any Docker VPS ([Hetzner](https://www.hetzner.com/), [DigitalOcean](https://www.digitalocean.com/), …) or [Phala](https://cloud.phala.network) / [dstack](https://github.com/Dstack-TEE/dstack). Pull compose from [releases](https://github.com/absgrafx/Morpheus-Uplink/releases/latest) — no git clone. |
| **External wallet (Rabby / MetaMask)** | You fund the *consumer* address; Uplink is not custodial | [Rabby](https://rabby.io/), [MetaMask](https://metamask.io/) |
| **ETH on Base** | Gas for open / close / reclaim | [Base](https://www.base.org/) |
| **MOR on Base** | Escrowed when sessions open | Token `0x7431ada8a591c955a994a21710752ef9b882b8e3` — [Uniswap ETH→MOR](https://app.uniswap.org/swap?chain=base&inputCurrency=ETH&outputCurrency=0x7431ada8a591c955a994a21710752ef9b882b8e3) |
| **Base HTTPS RPC** | Chain reads/writes | [Alchemy](https://www.alchemy.com/) or [Infura](https://www.infura.io/) → Base mainnet → `https://base-mainnet.g.alchemy.com/v2/…` |
| **Strong passwords + seed** | Admin GUI + API key derivation | `openssl rand -hex 32` |

**Base mainnet facts:** chain ID `8453` · Diamond `0x6aBE1d282f72B474E54527D93b979A4f64d3030a` · catalog [active.mor.org](https://active.mor.org)

---

[← Guide home](README.md) · [Next: Bootstrap →](02-bootstrap.md)
