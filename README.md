# Uplink

**Your personal gateway to the Morpheus decentralized AI network.**

The hosted API gateway is a party line — everyone on one wire. Uplink is
your own line in: a single small Go service that turns a Morpheus
**consumer proxy-router (C-Node)** into a personal, OpenAI-compatible API
gateway:

- `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models` with
  **Bearer `sk-…` keys** (any OpenAI SDK works as-is)
- **Automatic session management** — resolves model name → blockchain id,
  opens/reuses one session per model, retries once on a broken session
- **Usage metering** per key / model / day (JSON file, SQLite later)
- **Admin GUI** at `/gui` — balance, keys CRUD, open sessions, usage
- **`/node/*` passthrough** to the raw router API (admin-gated, injects the
  router's basic auth) — e.g. `/node/swagger/index.html`

Concept doc: `../.ai-docs/UPLINK_CONCEPT.md` (+ architecture PNG).

## Key model (two built-in keys + generated keys)

| Key | Derivation | Can prompt (`/v1/*`) | Can admin (`/admin/*`, `/node/*`) |
|---|---|---|---|
| **Master** `sk-uplink.…` | HMAC(seed, master label) | yes | yes (Bearer, same power as the admin password) |
| **Prompt** `sk-prompt.…` | HMAC(seed, prompt label) | yes | no — safe default to paste into clients |
| Generated `sk-…` | random, hash stored | yes | no |

The GUI itself logs in with the Basic-auth admin password; the master key
exists so automation/IaC can hit the admin API without that password.

The proxy-router underneath has its own per-method Basic-auth whitelists
(`proxy.conf` / `/auth/users`); UPLINK currently uses one full-power router
credential and enforces division at its own layer. Future hardening: give
the gateway a router user whitelisted to only the methods it needs.

## Design invariants

- **State is disposable.** Both built-in API keys are derived from
  `API_KEY_SEED` (HMAC-SHA256), so they survive a full storage wipe:
  redeploy with the same secrets block, same keys keep working. Sessions
  and funds live on-chain and recover through the wallet key. Only
  *generated* extra keys and usage history are lost on a wipe —
  acceptable, and exactly why the derived keys exist. This is deliberate
  armor against flaky VPS persistent storage.
- **Only the gateway is exposed.** The router's `:8082` never faces the
  internet; `/node/*` is the audited path to it. A consumer node needs **no
  inbound `:3333`** — it dials out to providers.
- **Zero external services.** No Postgres, no Redis, no cloud auth. Stdlib
  Go, one JSON state file.

## Local development

Two ways to run:

### Full stack (docker compose, needs a funded wallet)

```bash
cp .env.example .env    # wallet key, RPC, passwords, seed
docker compose -f docker-compose.local.yml up --build
```

- Gateway: http://localhost:8080 — GUI at `/gui`
- Router (localhost only, debugging): http://localhost:8082

Then from any OpenAI client:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $(uplink master key from /gui)" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama-3.3-70b","messages":[{"role":"user","content":"hi"}]}'
```

### Gateway only (against any reachable router)

```bash
export ROUTER_URL=http://localhost:8082 ROUTER_AUTH=admin:pass \
       ADMIN_PASSWORD=devpass API_KEY_SEED=$(openssl rand -hex 32)
go run ./cmd/uplink
```

### Tests

`go test ./...` — includes an end-to-end test against a mock router
(auth → session open → chat forward → usage record), no wallet needed.

## Environment variables

| Var | Required | Default | Purpose |
|---|---|---|---|
| `ROUTER_URL` | — | `http://proxy-router:8082` | C-Node admin API |
| `ROUTER_AUTH` | yes | — | Router basic auth, `user:pass` (= `COOKIE_CONTENT`) |
| `ADMIN_PASSWORD` | yes | — | GUI/admin login (user `admin`) |
| `API_KEY_SEED` | yes | — | Master-key derivation seed (`openssl rand -hex 32`) |
| `UPLINK_LISTEN` | — | `:8080` | Listen address |
| `DATA_DIR` | — | `./data` | JSON state file location |
| `ACTIVE_MODELS_URL` | — | `https://active.mor.org/active_models.json` | Model catalog |
| `SESSION_DURATION_SECONDS` | — | `3600` | Per-session duration (stake scales with this) |
| `SESSION_FAILOVER` | — | `true` | Router-side provider failover at open |
| `SESSION_DIRECT_PAYMENT` | — | `false` | Direct payment instead of stake |
| `CLOSE_SESSIONS_ON_EXIT` | — | `true` | Close pooled sessions on shutdown |

## Target VPS layout (next step, not in this folder yet)

The SecretVM deployment adds a Traefik TLS sidecar in front (same pattern as
`Morpheus-Lumerin-Node/proxy-router/docker-compose.tee.yml`) routing `443 →
uplink:8080`, and the secrets block collapses to **4 values**:
`WALLET_PRIVATE_KEY`, `ETH_NODE_ADDRESS`, `ROUTER_AUTH`+`ADMIN_PASSWORD`
(can be one value), `API_KEY_SEED`.

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
```
