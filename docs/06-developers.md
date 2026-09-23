# 6. Developers

[← Updates](05-updates.md) · [Guide home](README.md) · [Root README](../README.md)

How the sausage is made — local runs, env vars, CI, repo layout. Operators
who only want to *run* Uplink can stop at chapters **1–5** and the root
[Get running](../README.md#get-running-secretvm) CTA.

---

## Key model (detail)

| Key | Derivation | Prompt (`/v1/*`) | Admin (`/admin/*`, `/node/*`) |
|---|---|---|---|
| **Master** `sk-uplink.…` | HMAC(seed, master) | yes | yes (same power as admin password) |
| **Prompt** `sk-prompt.…` | HMAC(seed, prompt) | yes | no — default for clients |
| Generated `sk-…` | random; hash stored | yes | no |

GUI login is Basic auth (`admin` + `ADMIN_PASSWORD`). The Master key exists so
automation can hit admin APIs without that password.

### Soft revoke (tombstone)

`DELETE /admin/keys/{id}` **soft-disables** ephemeral keys: sets `revokedAt`
(UTC), clears `secret`, and **keeps** `id` + `name` (+ prefix/hash) so Usage
labels stay `Name (id)`. Auth via `LookupKey` treats revoked keys as unknown →
**401**. Revoking `master` / `prompt` is refused (**400**). Create/Import that
collides with a tombstone id or hash fails (**409**) — do not clear `revokedAt`
to revive; mint a new key.

**Orphan migration:** keys revoked **before** soft-revoke shipped were
hard-removed from `keys[]`. Their usage rows may show a bare hex `keyId`
forever — do **not** invent names. New revokes keep names. The Usage GUI
**Active** filter (default) hides revoked + orphan ephemeral rows; **All** shows
everything. Time range defaults to **7d** (UTC day buckets; no hourly precision).

### `GET /v1/usage` (self-scoped)

Bearer API keys only (`requireAPIKey`). Response `{"usage":[ UsageRow… ]}`.

| Caller | Scope |
|--------|--------|
| Prompt / generated | Rows where `keyId` equals the caller’s id |
| Master | Full instance (intentional ops convenience) |

**WARN:** Master is a full usage reader so automation can poll without admin
Basic. Prompt/generated never see other keys’ rows. `keyId` is public — not
`sk-`. Empty `X-Uplink-Key-Id` after auth fails closed (no unfiltered dump).
Admin Basic on `/v1/usage` → 401. Keep `GET /admin/usage` for the GUI.

The proxy-router still has its own Basic-auth surface (`COOKIE_CONTENT`).
Uplink uses one full-power router credential and enforces Prompt vs Master at
its own layer.

---

## Other deploy targets

Same images; different compose. Assets from the
[latest release](https://github.com/MorpheusAIs/Morpheus-Uplink/releases/latest).

| Target | Compose | Notes |
|--------|---------|--------|
| **SecretVM (default)** | `docker-compose.secretvm.deployed.yml` | Encrypted Secrets; platform TLS |
| **Any Docker VPS** | `docker-compose.generic.deployed.yml` | `.env` + Caddy / Let’s Encrypt (`PUBLIC_HOST`) |
| **Railway** | `deploy/railway/` scaffold | Same digests; Railway secret vars (H4); single-replica router (H3) |
| **TEE / compose clouds** | generic pattern | Phala / dstack fit here |

Step-by-step: [02-bootstrap.md](02-bootstrap.md). On SecretVM, Uplink bake
(`uplink_bake_env`) and router bake (`router_network_env`) are compose
`configs:` mounts -- not Encrypted Secrets. Keep them separate: never put
router `PROXY_*` / `LOG_LEVEL_*` into Uplink bake. Operator fillables are the
same six everywhere (`WEB_PUBLIC_URL` included).

---

## Local development

Not the production path. For hacking the Go service:

```bash
# full stack (needs funded wallet + RPC in .env)
cp .env.example .env
docker compose -f docker-compose.local.yml up --build
# Gateway: http://localhost:8080  ·  GUI: /gui  ·  Router: localhost:8082

# gateway only (against any reachable router)
export ROUTER_URL=http://localhost:8082 ROUTER_AUTH=admin:pass \
       ADMIN_PASSWORD=devpass API_KEY_SEED=$(openssl rand -hex 32)
go run ./cmd/uplink

go test ./...   # includes mock-router e2e (auth → session → chat → usage)
```

---

## Environment variables

Product rule: **form stays six**; tunables change via compose/code, not
Encrypted Secrets. Sources: `internal/config/config.go`, SecretVM
`uplink_bake_env` / `router_network_env`.

### 1. Minimum to run (six operator fillables)

| Var | Required | Purpose |
|---|---|---|
| `WALLET_PRIVATE_KEY` | yes (deploy) | Consumer wallet (router + Uplink reclaim) |
| `ETH_NODE_ADDRESS` | yes (deploy) | Base HTTPS RPC |
| `COOKIE_CONTENT` | yes | Router basic auth `user:pass` (`ROUTER_AUTH` fallback locally) |
| `ADMIN_PASSWORD` | yes | GUI/admin login (`admin`) |
| `API_KEY_SEED` | yes | Master/Prompt derivation (`openssl rand -hex 32`) |
| `WEB_PUBLIC_URL` | yes (deploy) | Public HTTPS origin |

### 2. Uplink tunable / additional (Uplink process only)

| Var | Default | Purpose |
|---|---|---|
| `ACTIVE_MODELS_URL` | `https://active.mor.org/gateway_models.json` | Gateway model catalog (bidDetail primary for list/open) |
| `GATEWAY_BIDS_URL` | `https://active.mor.org/gateway_bids.json` | Companion bids feed (not merged into LowestBid) |
| `SESSION_DURATION_SECONDS` | `600` | Session length (stake scales) |
| `SESSION_FAILOVER` | `true` | Provider failover at open |
| `HOUSEKEEPING` | on when wallet key present | Post-midnight stake reclaim |
| `UPLINK_LISTEN` | `:8080` | Listen address |
| `ROUTER_URL` | `http://proxy-router:8082` | C-Node admin API |
| `DATA_DIR` | `./data` | JSON state |
| `USAGE_PRUNE_DAYS` | `30` | Admin usage-history prune retention |
| `SESSION_DIRECT_PAYMENT` | `false` | Direct payment vs stake |
| `CLOSE_SESSIONS_ON_EXIT` | `true` | Close pooled sessions on shutdown |

Diamond / `ETH_NODE_CHAIN_ID` may exist in Uplink config defaults but are
**not** Uplink operator knobs.

### 3. Router tunable / additional (proxy-router only)

Loaded via `router_network_env` / process env (godotenv -- process env wins).
Never document these as Uplink knobs or put them in `uplink_bake_env`:

| Var | Role |
|---|---|
| `PROXY_STORE_CHAT_CONTEXT` | Privacy: store chat context |
| `PROXY_FORWARD_CHAT_CONTEXT` | Privacy: forward chat context |
| `LOG_LEVEL_APP` / `LOG_LEVEL_TCP` / `LOG_LEVEL_ETH_RPC` | Router log levels |
| `ETH_NODE_CHAIN_ID` | Chain id (Base mainnet `8453`) |
| `ETH_NODE_USE_SUBSCRIPTIONS` | WS subscriptions toggle |
| `BLOCKSCOUT_API_URL` | Block explorer API |
| `DIAMOND_CONTRACT_ADDRESS` | Inference diamond |
| `MOR_TOKEN_ADDRESS` | MOR ERC-20 |

---

## CI/CD and versioning

- **PR → `main`:** vet + tests (only when material paths change — see below).
- **Merge to `main`:** next `vX.Y.Z` (patch by default; `#minor` / `#major` in
  the merge commit message), multi-arch image to `ghcr.io/morpheusais/uplink`,
  GitHub release with digest-pinned compose + env examples.
- **workflow_dispatch on a branch:** prerelease image `vX.Y.N-<branch>` only
  (no release, no `latest`). Always available even for doc-only trees.

### What triggers CI

| Path | Why |
|------|-----|
| `cmd/**`, `internal/**` | Binary + embedded GUI |
| `go.mod`, `go.sum` | Module graph |
| `Dockerfile` | Image build |
| `deploy/**` | Image entrypoint + release compose/env assets |
| `docker-compose.local.yml` | Local-stack / ASCII contract in CI |
| `.github/workflows/build.yml` | Pipeline itself |

**Skipped** (no test run, no version bump): `docs/**`, root markdown
(`README`, `AGENTS`, `DISCLAIMER`, …), `llms*.txt`, `scripts/**`,
`.ai-docs/**`, `.env.example`, etc. A PR that mixes docs + `internal/` still
runs — any matching path is enough.

Keep the `uplink` GHCR package **public** so SecretVM can pull anonymously.
Workflow: [`.github/workflows/build.yml`](../.github/workflows/build.yml).

---

## Layout

```
cmd/uplink/          entrypoint
internal/config/     env config
internal/keys/       sk-… generation, deterministic master key
internal/store/      JSON state (keys, usage), atomic writes
internal/catalog/    active.mor.org model name → blockchain id
internal/router/     proxy-router client (sessions, balance, forward)
internal/pool/       one-session-per-model pool + invalidation
internal/api/        HTTP: /v1/*, /admin/*, /node/*, health
internal/gui/        embedded single-page admin UI
deploy/              SecretVM + generic + Railway overlays (CI pins digests)
```

Concept sketch (optional): [`.ai-docs/UPLINK_CONCEPT.md`](../.ai-docs/UPLINK_CONCEPT.md).

---

[← Updates](05-updates.md) · [Guide home](README.md)
