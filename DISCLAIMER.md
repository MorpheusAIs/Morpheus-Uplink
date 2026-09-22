# Uplink — Disclaimer

**Operator:** ABSGrafx LLC (South Dakota, USA)  
**Software:** Uplink (this repository) — a personal, self-hosted gateway to the
Morpheus network  
**License:** MIT — see [`LICENSE`](LICENSE)

Last updated: 2026-08-11

This is not a hosted service contract and not a bank. You run Uplink on
**your** machine with **your** wallet. Please read before you fund it or share
an API key.

---

## Heads up (self-custody)

- Uplink is **self-custodial**. The wallet private key and `API_KEY_SEED` live
  in **your** environment. We cannot recover lost keys, seed, passwords, or
  daylocked MOR. No one can.
- Prefer a **dedicated** consumer wallet with only what you need for sessions
  and gas — not your main holdings.

## What you are running

- **Experimental software**, provided **AS IS**, with no uptime, accuracy, or
  fitness guarantee.
- A **single-tenant** gateway: one wallet, one session pool. Any Prompt or
  ephemeral API key can open sessions and **escrow / lock MOR** from that
  wallet. Treat every inference key as spend authority on this box.
- Admin login and the Master key can reach `/admin/*` and `/node/*` (full
  proxy-router power). Protect them like the wallet.
- You must terminate **TLS** yourself (SecretVM / Caddy / etc.). Do not expose
  Uplink or the router admin port without HTTPS and strong secrets.

## Inference and privacy

- Prompts and completions go to **independent Morpheus providers** (and their
  upstreams). Do not put secrets, keys, or data you cannot afford to leak into
  prompts.
- Model quality, availability, and whether vendor extras (e.g. Venice
  parameters) are honored are **upstream** — not controlled by Uplink.

## Not the hosted API

- Uplink is **not** [api.mor.org](https://api.mor.org) / the Morpheus
  Marketplace hosted gateway. Different product, different risk, your ops.
- Morpheus coordinates a marketplace; providers run inference. See
  [nodedocs.mor.org](https://nodedocs.mor.org).

## No warranty / liability

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED. TO THE MAXIMUM EXTENT PERMITTED BY LAW, ABSGrafx LLC AND ITS
CONTRIBUTORS ARE NOT LIABLE FOR ANY DAMAGES ARISING FROM USE OF UPLINK,
INCLUDING LOSS OF MOR, KEYS, DATA, PROFITS, OR BUSINESS INTERRUPTION — WHETHER
FROM SOFTWARE BUGS, MISCONFIGURATION, THIRD-PARTY PROVIDERS, NETWORK FAILURE,
OR YOUR OWN OPERATIONAL CHOICES.

Forks and self-built copies are governed by the MIT license alone. This
disclaimer is how we ask operators to understand the product; it does not
replace advice from your own counsel if you redistribute or embed Uplink
commercially.

## Contact

| For | Reach |
|-----|--------|
| Bugs / features | [GitHub Issues](https://github.com/MorpheusAIs/Morpheus-Uplink/issues) |
| Security disclosure / license | Use a private channel to the maintainer (ABSGrafx) |

By running Uplink, you acknowledge self-custody, third-party inference, and
the AS IS nature of this software.
