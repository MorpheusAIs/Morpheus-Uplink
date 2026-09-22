# Uplink — Personal API Gateway (concept)

> Status: concept / design record — 2026-08-05
> Companion image: `UPLINK_architecture.png` (same folder)
>
> **Naming note:** this design was drafted under the working name **PAPIGW**
> ("Personal API Gateway"). The product is now **Uplink** — *your personal
> gateway to the Morpheus decentralized AI network* — living in this repo
> (`MorpheusAIs/Morpheus-Uplink`, image `ghcr.io/morpheusais/uplink`). Vocabulary
> that came with the rename: the GUI login greets with "Operator.", the keys
> package is `keymaker`, and the hosted-APIGW contrast line is "stop sharing
> a party line."

## 1. The problem

The hosted API Gateway (`api.mor.org`) is overloaded, and the guidance to power users is
"run your own consumer proxy-router." But the raw C-Node experience is nothing like an
API gateway:

- Auth is HTTP **Basic Auth** (`COOKIE_CONTENT` / `proxy.conf`), not OpenAI-style `sk-…` keys.
- `POST /v1/chat/completions` requires the caller to **already hold a `session_id`** for
  remote models — there is no auto-session-on-chat. The user must understand approve →
  list models → open session (stake MOR) → prompt-with-header → close.
- There is no usage metering, no per-key accounting, no budget view, no GUI.

Meanwhile, the **provider** onboarding story on SecretVM is excellent and proves the
pattern we want: copy one docker-compose (proxy-router + Traefik TLS sidecar), paste
**5 encrypted env vars**, and you get an HTTPS, DNS-named, attested node you then drive
from a hosted static GUI (MyProvider). See
`Morpheus-Lumerin-Node/docs/providers/full/secretvm-quickstart.mdx` and
`proxy-router/docker-compose.tee.yml`.

**Goal:** the same low-friction, one-compose experience for *consumers* — your own
`https://<name>.vm.scrtlabs.com/v1/chat/completions` with your own API keys, backed by
your own wallet and your own C-Node.

## 2. What already exists (inventory)

### C-Node (proxy-router, consumer mode) — already does the hard part

| Capability | Where |
|---|---|
| Open session **by modelId** with rated provider selection + failover | `POST /blockchain/models/{id}/session` (`blockchainapi/service.go`) |
| Close session, list user sessions | `/blockchain/sessions/*` |
| Chat completions (streaming) via `session_id` header | `POST /v1/chat/completions` (`proxyapi`) |
| On-chain model catalog | `GET /blockchain/models` |
| MOR/ETH balance, allowance, approve | `/blockchain/balance`, `/blockchain/allowance`, txs |
| Local chat history store + API | `/v1/chats*` (`chatstorage`) |
| Multi-user Basic Auth with per-method permissions | `proxy.conf`, `/auth/users*` (`authapi`) |
| TEE Phase-1/2 attestation when hitting `tee` models | `/v1/models/attestation` |

Same binary runs consumer-only (empty models-config → warn and continue). The existing
TEE compose is provider-shaped but the *pattern* (image + Traefik + SecretVM certs)
carries over unchanged.

### Hosted APIGW (Marketplace-API) — what the wrapper actually adds

Distilled to what matters for one user, the hosted service is:

```
API key auth (sk-…, SHA-256 lookup)
  → model name → blockchain id mapping (active.mor.org catalog)
  → claim idle session from pool for that model, else open one via C-Node
  → forward to C-Node /v1/chat/completions with session_id, stream back
  → record usage
```

Everything else — Cognito, credits ledger, Stripe/Coinbase, premium daylock, Redis rate
multipliers, deleted-user tombstones — is **multi-tenant hosting baggage** we drop.

### APP + MyProvider — GUI patterns

- Marketplace-APP pages worth keeping in spirit: API keys, playground, usage, chat.
- **MyProvider proves the delivery model:** a static React app (hosted at
  `myprovider.mor.org` *and* downloadable desktop) that drives any HTTPS proxy-router
  with Basic Auth. No backend of its own. A hosted sibling could do the same against
  an Uplink.

## 3. Proposed shape

One compose file, three containers, deployable on SecretVM (or any Docker VPS):

```
                        ┌────────────────────────── SecretVM (TDX) / any VPS ─────────────────────────┐
                        │                                                                              │
  user's app            │  ┌─────────┐      ┌──────────────────┐        ┌──────────────────────────┐  │
  (OpenAI SDK) ──HTTPS──┼─▶│ Traefik │──┬──▶│  Uplink          │──────▶│  proxy-router (C-Node)    │  │
  Bearer sk-…    :443   │  │ TLS     │  │   │  - sk-… keys     │ :8082 │  - wallet (in enclave)    │  │
                        │  │ sidecar │  │   │  - model mapping │ basic │  - sessions: open/close,  │  │
  browser (GUI) ──HTTPS─┼──┘         │  │   │  - session pool  │ auth  │    rating, failover       │  │
                        │            │  │   │  - usage metering│       │  - /v1/chat/completions   │  │
                        │            │  │   │  - embedded GUI  │       │  - balances, chats        │  │
                        │            │  │   └──────────────────┘       └────────────┬──────────────┘  │
                        │            │  │                                           │                 │
                        │            │  └───(admin-gated /node/* passthrough ──────▶│ for swagger /   │
                        │            │       to raw router API)                     │ power tooling    │
                        └────────────┼──────────────────────────────────────────────┼─────────────────┘
                                     │                                              │ TCP :3333 out
                                 SecretVM DNS + certs                     Morpheus providers (Base)
```

- **Traefik sidecar** — byte-for-byte the provider pattern: SecretVM certs, port 443,
  path routing; everything → Uplink, which itself gates `/node/*` to the router.
- **proxy-router, consumer mode** — no `MODELS_CONFIG_CONTENT`, no public :3333
  (consumer nodes dial out). Wallet key lives only here.
- **Uplink** — deliberately small Go service. Single-tenant: one owner, N API keys.
  JSON state file (SQLite later if needed) — no Postgres, no Redis.

### Request flow

1. `POST /v1/chat/completions`, `Authorization: Bearer sk-…`, `model: "llama-3.3-70b"`.
2. Uplink verifies key (SHA-256 lookup), resolves model name → blockchain id
   (catalog from `active.mor.org/gateway_models.json`, cached).
3. Session pool: reuse an OPEN session for that model; else
   `POST /blockchain/models/{id}/session` (rating + failover live in the C-Node).
4. Forward to C-Node `/v1/chat/completions` with `session_id`, stream back.
5. Meter: tokens, request count, per-key/per-model. Broken sessions invalidated and
   retried once; pooled sessions closed on shutdown to recover stake.

### Why TEE hosting is more than a gimmick here

- `WALLET_PRIVATE_KEY` is an *encrypted secret in the enclave* — the strongest available
  posture for "please paste your wallet key into a VPS."
- The user (or anyone they share the endpoint with) can attest the exact image serving
  their keys, same RTMR3/cosign machinery the provider flow already has.
- SecretVM gives DNS + certs for free, which is what makes the GUI story work from a
  hosted static app (no mixed-content pain — the exact lesson from MyProvider).
- But nothing is Secret-specific: the same compose on any VPS + Caddy/Let's Encrypt
  works; SecretVM is the *premium path*, not a dependency.

## 4. Delivery options considered

| | Option A — 4 sidecars (router + Traefik + trimmed Marketplace-API + APP) | Option B — 3 containers, new thin service with embedded GUI **(chosen)** | Option C — no new service: push gateway mode into proxy-router + hosted static GUI |
|---|---|---|---|
| Footprint | Heavy: FastAPI + Postgres + Redis + Node — won't fit a small SecretVM | Small: one Go container, static GUI embedded | Smallest (2 containers) |
| Effort | Weeks of *deleting* Cognito/credits code from two repos, then maintaining forks | New small codebase; borrow session-pool/streaming logic patterns from Marketplace-API | Upstream PR into proxy-router (foreign codebase politics, release train) |
| Maintenance | Two forks drifting from upstream | One small repo, clean single-tenant assumptions | Ideal long-term: everyone's router grows `sk-…` keys |
| Risk | Multi-tenant assumptions leak everywhere (encryption keyed to `cognito_user_id`, etc.) | Duplicates ~15% of Marketplace-API logic | Slow to land; couples our roadmap to upstream cadence |

**Recommendation: B now, C as the endgame.** If Uplink proves out, the API-key +
auto-session layer is a natural upstream contribution to proxy-router ("consumer
gateway mode"), at which point Uplink shrinks to just GUI + metering, or disappears.

## 5. Decisions log

### 2026-08-05 — initial design + P0

1. **Language: Go**, stdlib-only. Logic patterns borrowed from Marketplace-API
   (key hashing scheme, session routing loop).
2. **Stake posture:** GUI slider (economy / balanced / always-warm) with real economics
   shown — session stake scales with duration × price-per-second.
3. **Router exposure:** :8082 hidden; admin-gated `/node/*` passthrough (injects router
   basic auth) is the only path to it, including `/node/swagger/index.html`. Only 443
   is public. A consumer node needs **no inbound :3333**.
4. **Tenancy:** single owner, many keys now; F&F tenancy later via per-key budgets on
   the existing per-key metering.
5. **Endpoints:** chat completions **and** embeddings from day one (the router also
   supports `/v1/audio/*` — same passthrough shape when wanted).
6. **Key model:** two deterministic keys derived from `API_KEY_SEED` — **master**
   (`sk-uplink.…`, prompt + admin, can mint/revoke subkeys) and **prompt**
   (`sk-prompt.…`, inference only, safe default for clients). Generated subkeys are
   prompt-scoped, SHA-256 at rest. GUI auth = Basic admin password; master key =
   Bearer equivalent for automation.

### Persistence stance (ephemeral-storage-proof)

Assume VPS persistent storage is unreliable (it has been, on SecretVM and others).
Design rule: **everything that matters must be reconstructible from the secrets block
or the chain.**

| State | Where | On storage wipe |
|---|---|---|
| Wallet / funds / sessions | Chain + `WALLET_PRIVATE_KEY` env | Fully recovered |
| Master + prompt API keys | Derived: HMAC-SHA256(`API_KEY_SEED`) — never stored | Same seed → same keys, keep working |
| Generated extra keys | SHA-256 hashes in JSON state file | Lost (= implicitly revoked); re-create in GUI |
| Usage history | JSON state file | Lost — acceptable; (later: optional encrypted export/backup from GUI) |
| Model catalog | Refetched from active.mor.org | N/A |

### Onboarding target (8 steps, secrets block = 5 values)

1. Wallet with MOR + ETH on Base (have private key).
2. Alchemy/Infura RPC key.
3. SecretLabs account, funded.
4. Create VM: paste compose (`deploy/secretvm/docker-compose.yml`), paste secrets
   (`deploy/secretvm/env.template`): `WALLET_PRIVATE_KEY`, `ETH_NODE_ADDRESS`,
   `ROUTER_AUTH`, `ADMIN_PASSWORD`, `API_KEY_SEED`.
5. Start VM, note the DNS name.
6. `https://<vm>.…` → GUI login with admin password ("Operator.").
7. Everything else in the GUI (keys, sessions, usage), swagger via `/node/`.
8. Grab the prompt key → paste endpoint + key into Cursor / agent harness / any OpenAI
   client. It behaves like any hosted OpenAI-compatible API.

Possible "even easier" later: a hosted bootstrap page that generates the compose +
secrets block from a form (never storing them), one-click `secretvm-cli` command copy.

## 6. Status

- **P0 complete** (as `papigw` playground, migrated here): builds clean,
  `go test ./...` green with a mock router covering auth, session open/reuse, key
  scopes and lifecycle, `/node/*` gating. Embedded dark-purple GUI. Full local mock
  stack (`cmd/mockrouter`) demoed end-to-end on macOS incl. streaming and metering.
- **Next:** publish `ghcr.io/morpheusais/uplink` (public image), first SecretVM deploy,
  then real-wallet inference test.

## 7. Source material

- Provider SecretVM flow: `Morpheus-Lumerin-Node/docs/providers/full/secretvm-quickstart.mdx`,
  `proxy-router/docker-compose.tee.yml`
- Consumer router surface: `proxy-router/docs/swagger.yaml`,
  `docs/reference/api-direct.mdx`, `docs/reference/api-auth.mdx`
- Hosted gateway internals: `Morpheus-Marketplace-API/src/services/session_routing_service.py`,
  `src/core/security.py`, `src/services/proxy_router_service.py`,
  `src/core/direct_model_service.py`
- GUI patterns: `Morpheus-Marketplace-APP` (`/api-keys`, `/test`, `/usage-analytics`),
  `Morpheus-MyProvider` (static-app-drives-remote-router pattern)
