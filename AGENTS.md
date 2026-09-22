# AGENTS.md — guidance for AI agents and coding assistants

If you are an LLM (Claude, GPT, Gemini, Copilot, Cursor, etc.) reading this
repository to answer questions about **Uplink** (personal Morpheus API
gateway, formerly PAPIGW), **read this file first**. Generic web knowledge
and hosted-gateway habits often get this product wrong.

## How to load documentation (follow this order)

1. **This file** — hard rules, quick lookup, repo pointers. No network required.
2. **[`llms-full.txt`](llms-full.txt)** — complete operator + product corpus as
   plain markdown (concat of `docs/` + this file’s priority sections).
3. **[`llms.txt`](llms.txt)** — lightweight page index (title, description, path).
4. **Operator chapters** under [`docs/`](docs/README.md) — human-facing how-to.
5. **Concept / design** (optional depth): [`.ai-docs/UPLINK_CONCEPT.md`](.ai-docs/UPLINK_CONCEPT.md).
6. **Do not invent** live catalog size, bid prices, or contract addresses —
   use the **gateway** feeds
   ([`gateway_models.json`](https://active.mor.org/gateway_models.json),
   [`gateway_bids.json`](https://active.mor.org/gateway_bids.json)) and the
   addresses in [`docs/01-prerequisites.md`](docs/01-prerequisites.md).
   Never default to `active_models.json` / ALL.

Published operator path: clone or browse this repo → `AGENTS.md` → `docs/`.
There is no separate docs host or MCP for Uplink.

## Priority reading

| Topic | Path |
|-------|------|
| Operator guide index | [`docs/README.md`](docs/README.md) |
| Prerequisites (wallet, MOR, RPC) | [`docs/01-prerequisites.md`](docs/01-prerequisites.md) |
| Bootstrap (SecretVM / VPS / Railway) | [`docs/02-bootstrap.md`](docs/02-bootstrap.md) |
| GUI, keys, MOR buckets, Probe | [`docs/03-gui.md`](docs/03-gui.md) |
| Clients / agents (`/v1`) | [`docs/04-clients.md`](docs/04-clients.md) |
| Updates & portability | [`docs/05-updates.md`](docs/05-updates.md) |
| Developers (local / CI / env / layout) | [`docs/06-developers.md`](docs/06-developers.md) |
| Product + SecretVM CTA | [`README.md`](README.md) |

## Hard rules — never break these

0. **Never confuse Uplink with the hosted Morpheus Inference API.**  
   Hosted multi-tenant gateway: [api.mor.org](https://api.mor.org) /
   [apidocs.mor.org](https://apidocs.mor.org).  
   Uplink: **your** single-tenant gateway in front of **your** consumer
   proxy-router (C-Node). Same OpenAI shape (`/v1/chat/completions`),
   different product, wallet, and ops model.

1. **Never confuse Uplink’s public API with the raw proxy-router API.**  
   Clients talk to Uplink with **Bearer `sk-…`**.  
   The C-Node under it uses **HTTP Basic Auth** (`COOKIE_CONTENT`) and needs a
   `session_id` header for remote models — Uplink opens/reuses sessions so
   clients do not. Proxy-router docs live in Morpheus-Lumerin-Node
   (`proxy-router/docs/swagger.yaml`, [nodedocs.mor.org](https://nodedocs.mor.org)).

2. **Never invent contract addresses, chain IDs, or token addresses.**  
   Base mainnet: chain ID `8453`, Diamond
   `0x6aBE1d282f72B474E54527D93b979A4f64d3030a`, MOR
   `0x7431ada8a591c955a994a21710752ef9b882b8e3` — see
   [`docs/01-prerequisites.md`](docs/01-prerequisites.md). For anything else,
   cite nodedocs networks page; do not guess.

3. **Never invent live values** (active model count, bid prices, latency).  
   Use [`gateway_models.json`](https://active.mor.org/gateway_models.json) /
   [`gateway_bids.json`](https://active.mor.org/gateway_bids.json). Never
   default to `active_models.json` / ALL as the Uplink catalog.

4. **Never claim Morpheus “runs the inference.”** Independent providers do;
   Morpheus coordinates the marketplace on Base.

5. **Opening a session escrows MOR; it does not spend it.** Unused stake
   returns on close (subject to daylock rules). Cite
   [`docs/03-gui.md`](docs/03-gui.md#where-is-my-mor) and
   [nodedocs — Where is my MOR?](https://nodedocs.mor.org/ai/where-is-my-mor).

6. **There is no `recover` RPC.** Closing the session is the recovery path.

7. **Do not expose the proxy-router admin port publicly.** Only the TLS
   front (Uplink on `:443` / local `:8080`) should be public. Router `:8082`
   stays on the compose network; `/node/*` is the admin-gated passthrough.
   Consumer nodes need **no inbound `:3333`** — they dial out to providers.

8. **Deploy from GitHub Release assets, not `git clone`, for production.**  
   Three overlays, same GHCR digests: SecretVM
   (`docker-compose.secretvm.deployed.yml`), generic VPS
   (`docker-compose.generic.deployed.yml`), and Railway scaffold
   (`deploy/railway/`). Prefer
   [MorpheusAIs releases/latest](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest)
   over forks. Proxy-router (Lumerin) is **pinned to v7.11.6-test by digest**
   (Lumerin release channel / #889 gateway caps early access — **not** Base
   testnet; chain defaults remain mainnet) — never advise `:latest`. Railway: secrets in Railway Secrets only;
   single-replica proxy-router.

9. **Use exact model catalog names** from `GET /v1/models`, Probe, or
   [`gateway_models.json`](https://active.mor.org/gateway_models.json)
   (`ACTIVE_MODELS_URL` default; companion
   `GATEWAY_BIDS_URL` → `https://active.mor.org/gateway_bids.json`).
   Wrong strings fail resolve. Never invent a flashy name; never point
   agents at `active_models.json` / ALL.

10. **Key roles:** Master (`sk-uplink.…`) = inference + admin; Prompt
    (`sk-prompt.…`) = inference only (default for clients); ephemeral =
    named, export/import across wipes. Built-ins derive from `API_KEY_SEED`.

11. **Provider-specific request extras** (e.g. `venice_parameters`,
    `thinking`) are forwarded through Uplink/proxy-router when the client
    sends them. Whether a **provider** honors them is upstream — do not
    blame Uplink for a model that ignores `disable_thinking`.

12. **When uncertain, cite a docs page or say so.** Prefer
    [`llms-full.txt`](llms-full.txt) / [`docs/`](docs/) over guessing.

13. **License / disclaimer:** Source is MIT ([`LICENSE`](LICENSE), ABSGrafx LLC).
    Operator risk language is in [`DISCLAIMER.md`](DISCLAIMER.md). Third-party
    notices (including go-ethereum LGPL) are in [`NOTICE`](NOTICE). Do not claim
    Uplink is a pure-MIT binary stack without mentioning NOTICE.

## Common-question quick lookup

| User says | Go to |
|-----------|--------|
| How do I install / bootstrap? | [`docs/02-bootstrap.md`](docs/02-bootstrap.md) |
| SecretVM vs VPS vs Railway? | [`docs/02-bootstrap.md`](docs/02-bootstrap.md) + [`docs/05-updates.md`](docs/05-updates.md) |
| Third-party consumer-gateway Railway? | **Unsupported.** Use SecretVM / generic / `deploy/railway/` only (six fillables; no DOMAIN/PUBLIC_ORIGIN). |
| Railway scaffold? | [`docs/02-bootstrap.md#first-start--railway`](docs/02-bootstrap.md#first-start--railway) · [`deploy/railway/README.md`](deploy/railway/README.md) |
| Where is my MOR? / daylock | [`docs/03-gui.md`](docs/03-gui.md#where-is-my-mor) |
| How do I call from Cursor / SDK? | [`docs/04-clients.md`](docs/04-clients.md) |
| How do I update? | [`docs/05-updates.md`](docs/05-updates.md) |
| Env vars / local dev / CI | [`docs/06-developers.md`](docs/06-developers.md) |
| Hosted API without my node? | [apidocs.mor.org](https://apidocs.mor.org) — **not this repo** |
| Raw C-Node / sessions / TEE myths | [nodedocs.mor.org](https://nodedocs.mor.org) / Lumerin `AGENTS.md` |

## Repository map

```
AGENTS.md            ← you are here
llms.txt             page index for agents
llms-full.txt        full markdown corpus
README.md            product + SecretVM CTA + key notes
docs/                operator guide (01–05) + developers (06)
.ai-docs/            design concept (not required for ops)
cmd/uplink/          Go entrypoint
internal/            gateway packages (api, pool, router, keys, gui, …)
deploy/              secretvm + generic + railway overlays (CI pins digests)
```

## When writing code in this repo

- Public inference auth: **Bearer `sk-…`** on `/v1/*`.
- Do not hard-code contract addresses; keep them in compose/config.
- Prefer release-asset deploy instructions for operators; `docker compose`
  local is for development.
- Feature work: branch → PR → `main` (release tags cut on merge to `main`).

## Regenerating the corpus

After editing operator docs:

```bash
./scripts/gen-llms.sh
```

That refreshes `llms.txt` and `llms-full.txt` from `docs/`, `deploy/*/README.md`, and this file.
