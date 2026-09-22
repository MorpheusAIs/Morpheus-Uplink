# Uplink vs morpheus-consumer-gateway

Comparison for Alan. Analysis only — no product code in this change.

| | |
|---|---|
| Uplink checkout | `MorpheusAIs/Morpheus-Uplink` `main` @ `de4685e` (2026-09-22) |
| Sibling | [bowtiedbluefin/morpheus-consumer-gateway](https://github.com/bowtiedbluefin/morpheus-consumer-gateway) `main`, pushed 2026-09-14, package version `0.2.0` (`pyproject.toml`) |
| Relationship | **Not a fork.** GitHub `isFork: false`, `parent: null`. No shared history. Go gateway + stock proxy-router image vs a Python gateway that **rebuilds** proxy-router from a pinned tarball and a local patch. |
| Their own release bar | README: audit fixes locally validated; **production deployment acceptance remains open.** `docs/PRODUCTION-READINESS-REPORT.md` is a historical NO-GO record. Do not treat v0.2 as a drop-in production replacement. |

This file is an internal recommendation. `scripts/gen-llms.sh` lists operator chapters explicitly, so this report stays out of `llms.txt` / `llms-full.txt` unless someone adds it there.

“GHCI” in the request is treated as **GHCR** (GitHub Container Registry).

---

## Executive summary

- Same job, different stacks: both are single-tenant OpenAI-shaped fronts on a Morpheus **consumer** node and **your** wallet. Uplink is Go (`cmd/uplink`, `internal/*`) in front of the published Lumerin image. Consumer-gateway is FastAPI + SQLite + React (`gateway/`, `web/`, `node_helper/`) in front of a **patched** v7.11.0 binary it compiles itself.
- **Proxy pin matches the upstream tag, not the bits.** Uplink runs `ghcr.io/morpheusais/morpheus-lumerin-node:v7.11.0@sha256:3b2b1dea272124ce3c71ab35132f5f8a6dad54bb1e59614a50401a76c062a2b1` (`deploy/secretvm/docker-compose.yml`, `deploy/generic/docker-compose.yml`, `docker-compose.local.yml`). Consumer-gateway downloads Lumerin commit `99a8d86af1f9453d59797f9a208aa729ff3eb00d` (tarball SHA-256 `66f25ab…b8e4` in `deploy/node.Dockerfile`), applies `deploy/patch_node.py` + `deploy/consumer_node.py`, and **refuses the stock image**. `railway_setup.md` says replacing their node image with upstream GHCR is unsupported.
- **Do not swap images.** Their patch adds `maxStakeWei`, an operation journal (`X-Gateway-Operation`), wallet-mutation locking, and `GatewayCapabilities`. It also **deletes** IPFS and Docker host-admin routes (`docs/native-node-review/diffstat.txt`: 18 files, +849/−3076). Uplink’s `/node/*` passthrough and stock-router failover assume the unpatched API.
- **Catalog policy conflicts.** Uplink resolves names from `gateway_models.json` (`ACTIVE_MODELS_URL`) with `gateway_bids.json` as a companion feed (`internal/catalog/catalog.go`, AGENTS.md rule 9). Consumer-gateway treats `GET /blockchain/models` on the node as authority and warns that copying the hosted catalog recreates a central dependency (`docs/DESIGN.md` §12). Keep Uplink’s catalog. Optionally enrich; do not replace it.
- **Auth models conflict.** Uplink: Basic `admin` + HMAC seed keys `sk-uplink.` / `sk-prompt.` plus ephemeral `sk-` keys, soft-revoke tombstones, `GET /v1/usage` (Master sees the whole instance so staff automation can poll without admin Basic — comment in `internal/api/openai.go` names Seraph). Consumer-gateway: HttpOnly cookie + CSRF, hashed `mg_…` keys, model scopes, RPM and concurrency, **no usage API**, revoke = `enabled: false` with no usage-label tombstone.
- **Privacy gap on Uplink’s compose is the cheapest win.** Consumer-gateway forces `PROXY_STORE_CHAT_CONTEXT=false`, `PROXY_FORWARD_CHAT_CONTEXT=false`, and `LOG_LEVEL_*=warn` (`node_helper/app.py`, `compose.yaml`). Uplink compose never sets those. Confirm the stock v7.11.0 image actually honors them before documenting them as a guarantee.
- **Escrow-before-validate is the main behavioral hole to close in Uplink.** `handleInference` (`internal/api/openai.go`) checks that JSON has `model`, then `pool.EnsureDuration` (which can open a session) before the provider sees `messages`. Consumer-gateway validates chat shape, rejects injected `session_id` / `provider_url`, and only then acquires (`gateway/app.py`, `gateway/transport.py`). Their audit F05 / PR-03 is exactly this class of bug.
- **CI/GHCR: Uplink is ahead.** `.github/workflows/build.yml` already vet/tests, semver-tags, multi-arch pushes `ghcr.io/absgrafx/uplink`, and attaches digest-pinned compose to a GitHub Release. Consumer-gateway’s `.github/workflows/ci.yml` **never pushes a registry**. Their published images are Docker Hub tags `bowtiedbluefin/morpheus-consumer-*:railway-20260914.1` (`railway_setup.md`), not a GHCR pipeline. Borrow their **PR image smoke + non-root assert + govulncheck**, not their release story.
- **Skip the dashboard.** `web/src/main.tsx` (~77k lines) and Playwright specs are a different UI. Uplink’s embedded `/gui` (Probe, MOR buckets, usage, disk meters) stays.
- **Their production claims are qualified.** `docs/IMPLEMENTATION.md` still lists funded acceptance (real providers, day-lock, kill-during-submit, backup/restore) as remaining. Port patterns and tests, not the “v0.2 is done” narrative.

---

## Side-by-side capability matrix

| Area | Uplink (this repo) | Consumer-gateway | Incorporate? |
|---|---|---|---|
| Language / state | Go 1.22, JSON file under `DATA_DIR` (`internal/store`) | Python 3.13, FastAPI, SQLite (`gateway/store.py`) | No rewrite |
| Proxy-router | Stock image **v7.11.0** digest `sha256:3b2b1dea…a2b1` | Patched rebuild of commit `99a8d86`, capabilities `stake-limit-v1`, `operation-journal-v1`, `transaction-progress-v1` (`deploy/node.Dockerfile`, `deploy/patch_node.py`) | Upstream proposal only |
| Open session | `POST /blockchain/models/{id}/session` with `failover` (`internal/router/client.go`) | Explicit bid: `POST /blockchain/bids/{id}/session` plus `maxStakeWei` and `X-Gateway-Operation` (`gateway/node.py`) | Later, only with the journal |
| Pool | One open session per model; adopt chain session; 90s expiry buffer (`internal/pool/pool.go`) | Per-model `max_sessions` 1–8, retention `on_demand` / `until_expiry` / `maintain`, queue (`gateway/models.py`, `gateway/sessions.py`) | Optional later |
| Failover | Router flag `SESSION_FAILOVER` (default true). Uplink retries forward **only** when the error looks session-dead (`isSessionDeadError`) | Up to 3 providers **only** after a proven pre-submit failure. Ambiguous open blocks new opens. No prompt replay after bytes flow | Tighten Uplink; do not add blind multi-open |
| Catalog | `gateway_models.json` + companion `gateway_bids.json` | Node `GET /blockchain/models`, LLM + not deleted; aliases | Keep Uplink catalog |
| `/v1` routes | `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`, `GET /v1/usage` | Chat + models only. README: no embeddings, audio, Responses, billing | Keep embeddings + usage |
| Model names | Exact catalog `Name` | Operator alias or on-chain id | Keep catalog names |
| Auth | Bearer `sk-…`; admin Basic; Master = inference + admin | Bearer `mg_…` inference only; admin cookie session | Keep `sk-` model |
| Key policy | Master/Prompt from `API_KEY_SEED`; ephemeral create/import; soft-revoke tombstone (`docs/06-developers.md`) | Hash at rest, scopes, RPM, concurrency, enable/disable. No seed, no import, no tombstone | Add scopes/limits beside existing keys |
| Usage | Local rows; `/v1/usage` self-scoped except Master; `/admin/usage`; prune (`USAGE_PRUNE_DAYS`) | No token ledger. Event log of ids/timings, not prompt text | Keep usage API |
| Admin API | `/admin/keys`, status, usage, pool close, housekeep, estimate-stake, disk prune | `/admin/api/*` policy, catalog, bids, prewarm, quote, recovery, withdraw, sessions bind, node restart, events | Cherry-pick ideas |
| Raw node API | `/node/*` admin-gated reverse proxy (`internal/api/server.go`) | Not exposed. Helper is a Unix socket / private URL (`node_helper/`) | Keep gated `/node` |
| Health | `GET /healthcheck` | `GET /healthz` (process) and `GET /readyz` (node/recovery) | Add readiness |
| MOR reclaim | Gateway holds `WALLET_PRIVATE_KEY` and signs `withdrawUserStakes` (`internal/housekeep`, `internal/chain`) | Gateway **does not** receive the wallet file. Withdraw goes through the node. Auto-withdraw + manual check | Do not split until node withdraw is proven |
| Daylock display | `getUserStakesOnHold` from the gateway (`internal/chain/onhold.go`) | Reads node on-hold / available | Keep Uplink reads |
| Stake caps | Estimate endpoints for the GUI; open is not hard-capped in the gateway | `Budget`: per-session, total, price/sec, min liquid MOR, min ETH (`gateway/models.py`). Per-session cap enforced **in the patched node** | Gateway-side checks yes; native cap needs the patch |
| Provider policy | Router rating + failover. No gateway allow/deny | Allow/deny (deny wins; empty strict allowlist = nobody), rating weights, cooldown (`gateway/models.py`) | Should, carefully |
| Privacy / TEE | Not set in compose. Body forwarded as-is, including `venice_parameters` / `thinking` (AGENTS.md rule 11) | Chat context store/forward off; prompts not logged; `TEE attestation failed` is a **pre-submit** provider decline (`gateway/node.py`) | Set env flags; keep extension forwarding |
| Rate / body limits | 16 MiB body (`maxRequestBody`). No RPM, no connection cap, no upload deadline | 2 MiB upload, 15s body timeout, 128 connections, per-key RPM + concurrency, 16 MiB response cap | Port limits |
| Idempotency | None on chat | `Idempotency-Key` → 409, response **not** stored or replayed | Should |
| Request ID / headers | No `X-Request-ID`, no CSP | `X-Request-ID`, `nosniff`, `no-referrer`, CSP, `no-store` (`gateway/app.py` middleware) | Should |
| Restart | `CLOSE_SESSIONS_ON_EXIT` (default true) | Drain (150s) vs explicit immediate restart; helper rewrites rating JSON (`node_helper/`) | Nice; needs supervisor |
| Secrets | Env / SecretVM Encrypted Secrets (wallet in **both** containers) | Files under `secrets/`, Compose secrets; wallet only on the node | Partial; see risks |
| TLS | SecretVM Traefik (`deploy/secretvm/docker-compose.yml`); generic Caddy 2.8 **without** `flush_interval` | Optional Caddy 2.10.2 profile, `flush_interval -1` (`deploy/Caddyfile`) | Fix SSE flush |
| Hosts | SecretVM first; generic VPS; local compose. **No git clone** for prod | `git clone` + `docker compose --profile https`; Railway two-service guide | Keep release-asset deploy |
| Hardening | Entrypoint drops to uid `uplink` (`deploy/docker-entrypoint.sh`). No `cap_drop` / `no-new-privileges` / healthcheck on the app | `cap_drop: [ALL]`, `no-new-privileges`, `init`, healthcheck, loopback `:8000`, node `:8082` unpublished | Harden gateway/router; do not drop Traefik’s docker.sock pattern blindly |
| GUI | Embedded `/gui`: keys, Probe, MOR buckets, usage, disk warn & prune | React dashboard: aliases, rating, provider lists, restart, wallet recovery | Skip UI |
| Tests | `go test ./...` in CI, mock router | pytest (recovery, faults, HTTP boundaries), Playwright, soak (`devtools/soak.py`), live audit scripts under `audits/` | Port fault cases into Go tests |
| Registry | **GHCR** multi-arch + GitHub Release assets (`.github/workflows/build.yml`) | CI builds only. Docker Hub tags documented in `railway_setup.md` | Keep Uplink pipeline |
| Agent docs | `AGENTS.md`, `llms.txt`, `llms-full.txt`, `docs/01`–`06` | None of those. Long design/audit set under `docs/` | Do not replace Uplink docs |

---

## Recommended incorporate list

### Must

1. **Set consumer privacy env on the stock router, then verify.**  
   Add to `deploy/secretvm/docker-compose.yml`, `deploy/generic/docker-compose.yml`, and `docker-compose.local.yml` (router service only):

   - `PROXY_STORE_CHAT_CONTEXT=false`
   - `PROXY_FORWARD_CHAT_CONTEXT=false`
   - `LOG_LEVEL_APP=warn`, `LOG_LEVEL_TCP=warn`, `LOG_LEVEL_ETH_RPC=warn`

   Source: `compose.yaml` and `node_helper/app.py` (around the env block that also sets `IPFS_DISABLED=true`).  
   **Rationale:** prompts should not be persisted or forwarded as node chat context.  
   **Gate:** confirm stock v7.11.0 reads these. Consumer-gateway’s own `docs/OPERATIONS.md` says native logging still needs live acceptance. Do not document a TEE/privacy guarantee until that check passes. `IPFS_DISABLED` is already implied by Uplink’s consumer-mode compose (no `MODELS_CONFIG_CONTENT`); don’t copy their IPFS **code deletion**.

2. **Validate the chat body before any session open.**  
   In `internal/api/openai.go`, reject missing/empty `messages`, non-object JSON, and client-supplied routing fields (`session_id`, `model_id`, `chat_id`, `provider_url`, `node_url`) **before** `pool.EnsureDuration`. Keep forwarding unknown provider extras (`venice_parameters`, `thinking`, `tools`) — AGENTS.md rule 11. Go’s `encoding/json` already rejects `NaN`; the Python PR-03 hole is not a literal port, but “model present, messages garbage, session already open” is.  
   Source: `gateway/app.py` completion handler; `gateway/transport.py` `ChatRequest`; audit F05 / PR-03 in `docs/RECOVERY-AND-AUDIT-FIXES.md` and `docs/PRODUCTION-READINESS-REPORT.md`.

3. **Do not replay a prompt after the provider has started, and do not open a second session on an ambiguous error.**  
   Keep today’s rule: rotate only on session-dead errors (`isSessionDeadError`). Add an explicit test that a provider 4xx/5xx or a partial SSE body does **not** call `OpenSession` again. Consumer-gateway will not stitch a new generation onto a broken stream (`gateway/app.py` yields `stream_interrupted` and does not replay).  
   **Rationale:** a second open escrows more MOR; a replay re-runs agent **tool** calls the gateway does not execute itself (see risks).

4. **Keep the stock digest pin. Do not ship their node image.**  
   If stake limits or journals are wanted, send `docs/native-node-review/node-v7.11.0.patch` (or the recipe in `deploy/patch_node.py` + `deploy/native/gateway_stake.go` + `deploy/native/gateway_progress.go`) **upstream to Morpheus-Lumerin-Node**, then bump Uplink’s digest. Their Dockerfile fails the build if the pinned source text does not match — that recipe will break on the next Lumerin commit. `REQUIRE_NODE_GUARDS=false` is documented as an unsupported escape hatch (`docs/RECOVERY-AND-AUDIT-FIXES.md`).

5. **Leave GHCR + release-asset deploy as the production path.**  
   Nothing in consumer-gateway replaces `.github/workflows/build.yml`. Operators stay on SecretVM / generic **release** compose, not `git clone`.

### Should

1. **Per-key model scope, requests/minute, and concurrency** on ephemeral keys (and optionally Prompt). Defaults should match today’s behavior (no scope = all catalog models; a high RPM) so existing clients do not 429 on upgrade. Store the policy next to the key record; revoked tombstones stay. Do not switch the secret prefix from `sk-` to `mg_`.  
   Source: `KeyCreate` in `gateway/models.py`; enforcement in `gateway/app.py` (`rates`, `key_active`). Their counters reset on process restart — say so if we copy that.

2. **Admission ceilings before open:** max price/second, max session stake (estimate), minimum ETH, optional minimum liquid MOR. Uplink already **displays** stake (`GET /admin/estimate-stake`). Consumer-gateway **blocks** opens (`Budget` in `gateway/models.py`). A gateway-side check is safe on the stock router. The exact `maxStakeWei` enforcement is inside their patch and is not real until the node checks the calculated stake (`deploy/native/gateway_stake.go`). Do not advertise a hard cap the stock image ignores.

3. **Provider allow/deny in the gateway only after bid-level open exists.** Deny-wins and “empty allowlist means nobody” are the right rules (`Providers.permits` in `gateway/models.py`). Today Uplink asks the **router** to pick (`failover: true`). Filtering after the router already chose is too late; filtering by opening bids ourselves without an operation journal can double-escrow when the HTTP response is lost (their F01). Sequence: journal/upstream patch first, then policy.

4. **Stable error codes without leaving the OpenAI error envelope.** Map `pool_busy`, `no_eligible_provider`, `open_unknown` style cases from `docs/OPERATIONS.md` into `error.code` while keeping `error.message`. Clients already parse Uplink’s OpenAI shape.

5. **`Idempotency-Key` on `POST /v1/chat/completions`:** if seen for that key, return 409 and do **not** store the prompt or the completion (`gateway/app.py`). Same idea for key-create so a lost response cannot mint two secrets blindly — Uplink import/create already has tombstone collisions; don’t weaken that.

6. **Edge limits:** connection cap, upload deadline, response/SSE byte cap, `X-Request-ID`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`. Source: middleware at the top of `gateway/app.py`. Pick limits that still allow long contexts (Uplink’s 16 MiB body is more generous than their 2 MiB upload — keep a large prompt ceiling, add the timeout).

7. **`GET /readyz` (or equivalent) separate from liveness.** `/healthcheck` can stay process-up. Readiness should fail when the router health check fails or rehydrate is stuck, and it must **clear** after recovery (their PR-06: a sticky `ready=false` never recovered). Do not put wallet addresses on the unauthenticated body.

8. **Generic Caddy: `flush_interval -1`.** `deploy/generic/docker-compose.yml` config `caddyfile` has no flush setting. `deploy/Caddyfile` in the sibling sets `flush_interval -1` so SSE is not buffered. Check Traefik on SecretVM for the same buffering issue; don’t copy their whole ingress.

9. **Container hardening on uplink + proxy-router:** `init: true`, `security_opt: ["no-new-privileges:true"]`, `cap_drop: ["ALL"]`, a HTTP healthcheck, `stop_grace_period` long enough for session close (they use 180s). **Do not** remove Traefik’s docker socket on SecretVM — that compose follows `proxy-router/docker-compose.tee.yml`. **Do not** switch SecretVM from env secrets to `secrets/` files; Encrypted Secrets are the product path (`docs/02-bootstrap.md`).

10. **CI additions that do not publish:**
    - On pull_request, `docker build` (or compose build) and a non-root assert modeled on `.github/workflows/ci.yml` (“Verify mounted volumes and privilege drop”).
    - `govulncheck` on **Uplink’s** module (`go.mod` is go-ethereum 1.14.12). Their `deploy/node.Dockerfile` target `security` scans the **patched node**, which we are not building.
    - Fault tests inspired by `tests/test_production_faults.py`, `tests/test_recovery.py`, `tests/test_http_boundaries.py`: invalid body does not open; session-dead vs provider error; revoke during wait; oversized body.

11. **Ops notes worth folding into existing chapters later** (not a second manual): one wallet / one gateway; don’t delete journals or state to “clear” an unknown open; close is the recovery path (already Uplink/AGENTS.md rule 6); backup = stop both volumes together (`docs/OPERATIONS.md`). Their error table is the best short source.

### Nice

- Activity log of event type, model id, provider, timing — never prompt/response text (`store.event` usage in `gateway/app.py`).
- Provider cooldown after a classified failure (default 120s in `RecoveryPolicy`) once policy exists.
- Prewarm / “keep available” as an **explicit** per-model opt-in. It locks MOR continuously. Their README is careful: it does not reserve GPU or guarantee uptime.
- Demo mode without a wallet (`devtools/demo.py`) for GUI work. Production factory must not fall back to it (they test that).
- Base Sepolia as a **dev** compose overlay only. Their helper defaults for chain `84532` live in `node_helper/app.py`. **Do not copy those addresses into Uplink** until they are checked against nodedocs (AGENTS.md rule 2). SecretVM stays Base mainnet `8453`.
- Upstream-shaped rating editor only after Lumerin can reload rating without a sidecar supervisor (`node_helper/`).

### Skip

- **UI rewrite.** `web/src/main.tsx`, `web/src/style.css`, `web/e2e/*.spec.ts`, `web/audit/`, dashboard screenshots. Includes mobile balance layout (PR-07).
- **Replacing** Basic auth, `API_KEY_SEED`, `sk-uplink.` / `sk-prompt.` / ephemeral import, or soft-revoke tombstones with cookie CSRF + `mg_` keys.
- **Dropping** `GET /v1/usage`, `POST /v1/embeddings`, `/admin/usage`, `/node/*`, disk status (`internal/api/disk.go`, Status meters), or housekeeping.
- **SQLite migration** for its own sake.
- **Node-only catalog** in place of `gateway_models.json` / `gateway_bids.json`.
- **git clone + `docker compose up --build` as the operator install.** `README.md` and `railway_setup.md` are fine as a sibling’s runbook; they contradict Uplink release-asset deploy.
- **Docker Hub as the registry** and date tags `railway-20260914.1`. No GHCR workflow exists to port.
- **Bundled deletion of IPFS/Docker routes** (`deploy/consumer_node.py`). That is their consumer image, not a change to the shared GHCR Lumerin image.
- **Pasting audit corpora** into `docs/`: `DESIGN.md` (~75 KB), `PRODUCTION-READINESS-REPORT.md`, `CUSTOMER-EXPERIENCE-AUDIT.md`, `REMEDIATION-*`, `PRODUCTION-TEST-EVIDENCE.json`, `docs/NATIVE-VULNERABILITY-SCAN.txt`. Mine them; don’t publish them as the operator guide.
- **Five-minute sessions.** Their UI allowed 300s; funded tests hit `SessionTooShort` and still spent gas on approvals (PR-02). Uplink’s floor of 600s (`X-Uplink-Session-Duration`) is the safer protocol floor. Keep it.
- **Playwright-as-CI-gate** until someone is actually changing the GUI.

---

## Files and workflows to port

### CI/CD and GHCR — do not replace Uplink’s pipeline

| Keep (Uplink already does this better) | Path |
|---|---|
| PR vet + `go test`, main semver (`#major` / `#minor` / patch), multi-arch GHCR, `latest` only on real releases | `.github/workflows/build.yml` |
| Image name | `ghcr.io/absgrafx/uplink` (`IMAGE_NAME` in that workflow) |
| Digest-pinned release assets | steps “Generate digest-pinned deploy assets” and “Create GitHub release” → `docker-compose.secretvm.deployed.yml`, `docker-compose.generic.deployed.yml`, env examples |
| Doc-only pushes do not cut a release | `paths:` filters on `pull_request` and `push` |
| SecretVM secret-form allowlist | job `test`, step “SecretVM compose prompts only for real secrets” |
| ASCII deploy files | same job, “Deploy files must be ASCII” |

| Borrow ideas from (no `docker push`, no GHCR) | Path | What to copy |
|---|---|---|
| Verify workflow | `.github/workflows/ci.yml` | Last three steps: `docker compose build`, uid **10001** write to a tmpfs volume, `docker build --target security` |
| Node vuln scan | `deploy/node.Dockerfile` stage `security` | `govulncheck` invocation only. Retarget at `./...` in **this** module. Do not adopt the Dockerfile (it compiles a fork). |
| Privilege model | `Dockerfile` `USER 10001`, `scripts/container_entrypoint.py` | Uplink already drops privs in `deploy/docker-entrypoint.sh`. Add the **CI assert**, plus “don’t follow symlinks out of the volume” if SecretVM volumes can contain links. |
| Lockfile discipline | `requirements.lock`, `pyproject.toml` | Analog is already `go.mod` tidy check in `build.yml`. No Python lock to import. |

Consumer-gateway image publication is **manual Docker Hub**, documented not automated:

- `railway_setup.md` §1 and §10 — images `bowtiedbluefin/morpheus-consumer-gateway:railway-20260914.1` and `bowtiedbluefin/morpheus-consumer-node:railway-20260914.1`, plus index digests.
- Same doc warns that upstream tag `sha256-3b2b1dea…` was **Sigstore metadata**, and the runnable image is `ghcr.io/morpheusais/morpheus-lumerin-node@sha256:3b2b1dea272124ce3c71ab35132f5f8a6dad54bb1e59614a50401a76c062a2b1`. That digest **matches Uplink’s pin**. Useful ops note for `docs/05-updates.md`; not a new registry.

There is no `.github/workflows/*` besides `ci.yml`. Nothing to retarget at `ghcr.io`.

### Docs worth mining (into existing Uplink chapters, later)

| Sibling file | Use |
|---|---|
| `docs/OPERATIONS.md` | Privacy, backup, “unknown open”, credential rotation. Best short port. |
| `docs/DESIGN.md` §10–11 | API and failover tables. Ignore §12’s “drop the hosted catalog” conclusion. |
| `docs/RECOVERY-AND-AUDIT-FIXES.md` | Which failures are pre-submit vs unknown. Test oracles. |
| `docs/native-node-review/README.md` + `diffstat.txt` + `node-v7.11.0.patch` | Spec if Lumerin takes the journal/stake patch. Do not vendor the 284 KB patch into Uplink. |
| `README.md` “Operational limits” | One worker, one wallet, rate counters reset on restart. |
| `docs/IMPLEMENTATION.md` | Scope boundary: what they still refuse to claim. |

### Docs not to port

`railway_setup.md` (Railway-specific; git-build fallback points at branch `fix/recovery-and-customer-reliability`), `docs/PRODUCTION-*`, `docs/REMEDIATION-*`, `docs/CUSTOMER-EXPERIENCE-AUDIT.md`, `audits/`, `web/`.

Uplink docs that should stay canonical: `AGENTS.md`, `README.md`, `docs/01`–`06`, `llms.txt`. Consumer-gateway has **no** `AGENTS.md` and **no** `llms.txt`.

### Behavior sources (when implementing later)

| Behavior | Sibling | Uplink touch point |
|---|---|---|
| Pre-open validation | `gateway/transport.py`, `gateway/app.py` | `internal/api/openai.go` |
| Privacy env | `compose.yaml`, `node_helper/app.py` | three compose files, router service |
| SSE flush | `deploy/Caddyfile` | `deploy/generic/docker-compose.yml` `caddyfile` |
| Key limits | `gateway/models.py` `KeyCreate` | `internal/keymaker`, `internal/store`, `internal/api/server.go` |
| Budgets | `gateway/models.py` `Budget` | new check before `internal/pool` open; GUI already has estimate routes in `internal/api/admin.go` |
| Journal / max stake | `deploy/patch_node.py`, `deploy/native/gateway_*.go` | Morpheus-Lumerin-Node, not this repo |
| Error taxonomy | `docs/OPERATIONS.md` | `internal/api/openai.go` error writer |
| Hardening | `compose.yaml` | `deploy/secretvm/docker-compose.yml`, `deploy/generic/docker-compose.yml` |
| CI smoke | `.github/workflows/ci.yml` | new job or extra steps in `.github/workflows/build.yml` **without** pushing on PRs |

---

## Risks and conflicts with Uplink-specific behavior

### Usage API and per-key usage

Consumer-gateway does not record prompt/completion tokens and has no `/v1/usage`. Uplink records them on the buffered (non-stream) path (`internal/router/client.go` `Forward`) and serves:

- `GET /v1/usage` — Prompt/ephemeral: own rows; **Master: full instance**
- `GET /admin/usage` — GUI
- `POST /admin/disk/prune-usage` — typed confirm, keeps keys

`internal/api/openai.go` states why Master is unscoped: staff/ops automation (**Seraph**) polls with the Master bearer and must not need admin Basic. A “self-only for every key” port of their model would break that. Streaming responses skip token parse today; don’t pretend a new activity log replaces the usage ledger.

### Soft-revoke tombstone

Uplink `DELETE /admin/keys/{id}` sets `revokedAt`, clears `secret`, **keeps id and name** so Usage can still say `Name (id)`. Import/create that hits a tombstone id or hash returns 409. Master/Prompt cannot be revoked (`docs/06-developers.md`).

Consumer-gateway sets `enabled: false` and has no usage labels to protect. Do not hard-delete tombstones to match them. If we add scopes, editing a tombstone must stay forbidden.

### Status disk UI

`GET /admin/status` includes `disk` from `internal/api/disk.go` (warn under 2 GiB or 15% free). The GUI renders meters and prune on the support/status surface. Their retention story is “30 days of terminal history, 500 events, rotated container logs” (`docs/OPERATIONS.md`) plus a warning that native journals grow. Useful sentence for SecretVM’s small disk (`docs/02-bootstrap.md`); not a reason to replace the meters.

### Staff / tool-bridge assumptions

Two different mechanisms, both easy to break:

1. **Staff usage bridge.** Master bearer on `GET /v1/usage` is an intentional full-instance reader (Seraph comment in `internal/api/openai.go`). Tests cover fail-closed when `X-Uplink-Key-Id` is missing (`TestHandleUsageV1EmptyKeyIdFailClosed`). Any new admin-cookie session must not become the only way to read usage.

2. **Chat tool passthrough.** Uplink forwards the raw body, so client `tools` / `tool_calls` reach the provider. The gateway does not execute them. `Forward` copies only `Content-Type` and `Accept` plus `session_id` (`internal/router/client.go`). Consumer-gateway’s design text (`docs/DESIGN.md` §11): do not auto-replay side-effecting tool calls; tool definitions are not gateway tool execution. The probe-then-reopen path in `handleInference` is safe only while `probeOnly` refuses to write a non-2xx **and** the error is truly “session id rejected” before provider work. Widening failover to “try another provider after any failure” would re-issue tool-call prompts. Idempotency, if added, must not store tool arguments or completions.

`/node/*` is the admin bridge to raw router routes (Swagger, power tools). Their design explicitly does not expose node Basic auth to the browser. Keep the passthrough admin-gated; do not mount it for Prompt keys (already tested in `internal/api/server_test.go`).

### Housekeeping vs “gateway must not see the wallet”

Uplink puts `WALLET_PRIVATE_KEY` on **both** uplink and proxy-router so the gateway can sign `withdrawUserStakes` after 00:00 UTC (`internal/housekeep/housekeep.go`, SecretVM env). Consumer-gateway’s threat model (`docs/OPERATIONS.md`): wallet file is node-only; the gateway holds the node admin password and is still trusted. Moving the key off the gateway means housekeeping must call the node’s withdraw route instead. That is a behavior change, not a five-line env edit. Until then, document that the gateway is wallet-capable — `DISCLAIMER.md` already treats admin and router credentials as wallet-power.

### Catalog and model identity

Probe, `GET /v1/models`, and clients use **catalog names**, with `morpheus.blockchainId` and `bidDetail` prices (`internal/catalog/catalog.go`, `internal/api/openai.go`). Consumer-gateway’s `/v1/models` returns **aliases** for enabled policies only. Switching to node-catalog aliases would break Cursor/SDK setups in `docs/04-clients.md` and violate the gateway-feed rule. A later alias layer must resolve to the catalog id, not replace it.

### Session pool and stake

Uplink: one session per model, reuse until 90s before `endsAt`, adopt an existing chain session before opening (`internal/pool/pool.go`). Close on shutdown by default.  
Sibling: up to 8 sessions per model and a “maintain” mode that keeps replacing sessions. That increases MOR locked in Active. Their PR-04 also shows extra opens failing while a hot session was merely busy. Do not copy multi-session until the single-session pool is failing real traffic.

Duration: Uplink default 600s and header floor 600s. Sibling default 1800s, UI range 300–86400, and a funded proof that 300s is below the contract once rounding is applied (PR-02). Keep the 600s floor.

### SecretVM compose contract

`build.yml` fails the build if non-allowlisted `${VARS}` appear in `deploy/secretvm/docker-compose.yml`. New router env should be **literals** (`PROXY_STORE_CHAT_CONTEXT=false`), not new prompts, or the allowlist and `docs/02-bootstrap.md` secret block must change together. Network addresses stay in the `configs:` block (Diamond `0x6aBE1d282f72B474E54527D93b979A4f64d3030a`, MOR `0x7431ada8a591c955a994a21710752ef9b882b8e3`, chain `8453`) — already aligned with their `.env.example` comments for mainnet.

### What Uplink has that we should not regress

Embeddings passthrough, Probe (`docs/03-gui.md`), support bundle, deterministic keys across a wipe, ephemeral export/import, on-hold MOR buckets, post-midnight reclaim, admin-gated `/node`, digest-pinned releases, path-filtered CI so this markdown file does not tag a release.

---

## Suggested implementation order (when Alan says go)

1. Compose privacy env + Caddy flush + CI non-root smoke (no API change).
2. Pre-open chat validation and “no second open” tests.
3. Readiness endpoint, request id, body timeout, connection cap.
4. Ephemeral key scope + RPM with backward-compatible defaults.
5. Gateway-side price/stake/ETH admission using existing estimate math.
6. Only then: Lumerin conversation about `gateway_stake.go` / `gateway_progress.go`, then provider policy and bid-level open.

UI work stays off this list.
