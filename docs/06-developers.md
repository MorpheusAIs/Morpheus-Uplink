# 6. Developers

[← Updates](05-updates.md) · [Guide home](README.md) · [Root README](../README.md)

How the sausage is made — local runs, env vars, CI, repo layout. Operators
who only want to *run* Uplink can stop at chapters **1–5** and the root
[Get running](../README.md#get-running-secretvm) CTA.

---

## Key model (detail)

| Key | Derivation | Prompt (`/v1/*`) | Admin (`/admin/*`) |
|---|---|---|---|
| **Master** `sk-uplink.…` | HMAC(seed, master) | yes | yes (same power as admin password) |
| **Prompt** `sk-prompt.…` | HMAC(seed, prompt) | yes | no — default for clients |
| Generated `sk-…` | random; hash stored | yes | no |

GUI login is `POST /admin/login` with `ADMIN_PASSWORD` (+ TOTP / recovery
code once 2FA is enrolled), which sets an HttpOnly session cookie; Basic auth
is rejected on `/admin/*`. The Master key can drive admin APIs as a Bearer
token only when `MASTER_KEY_ADMIN=true` (default off).

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
| `WALLET_PRIVATE_KEY` | yes (deploy) | Consumer wallet, **router only** (Uplink holds no key; reclaim runs via the router API) |
| `ETH_NODE_ADDRESS` | yes (deploy) | Base HTTPS RPC |
| `COOKIE_CONTENT` | yes | Router **admin** `user:pass`; proxy-router + router-user-init only. Uplink uses the scoped user from `ROUTER_AUTH_FILE` / `ROUTER_AUTH` (`COOKIE_CONTENT` only with `UPLINK_DEV_HTTP`) |
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
| `HOUSEKEEPING` | `true` | Post-midnight stake reclaim (via router API) |
| `UPLINK_LISTEN` | `:8080` | Listen address |
| `ROUTER_URL` | `http://proxy-router:8082` | C-Node admin API |
| `DATA_DIR` | `./data` | JSON state |
| `USAGE_PRUNE_DAYS` | `30` | Admin usage-history prune retention |
| `SESSION_DIRECT_PAYMENT` | ignored | Removed: sessions are always staking-mode (`directPayment:false`); setting `true` only logs a warning |
| `UPLINK_ALLOWANCE_STATUS_FILE` | unset | Allowance manager status file (read-only to Uplink; never under `DATA_DIR`) |
| `UPLINK_ALLOWANCE_CTL_DIR` | unset | Dir for the presence-only resync request flag (never under `DATA_DIR`) |
| `UPLINK_ALLOWANCE_POLL_SECONDS` | `60` | Manager poll interval; resync shows as available while its heartbeat is newer than 3 polls |
| `CLOSE_SESSIONS_ON_EXIT` | `true` | Close pooled sessions on shutdown |
| `UPLINK_RECONCILE_GATE_SECONDS` | `60` | Boot reconcile: how long session opens wait for the first session list |
| `UPLINK_RECONCILE_RETRY_SECONDS` | `30` | Boot reconcile: background retry interval after that |

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

## Boot order, restart safety and the allowance manager

- **router-user-init** (one-shot, root, `cap_drop: ALL` + `CHOWN`) removes its
  previous files, then writes `router.auth` (0400), the allowance and wallet
  records (0444) and last a ready marker (`router.ready`, 0400) holding this
  kernel boot's id, a random generation and the files' SHA-256. If the
  router still accepts the current scoped credential it keeps the password
  and only updates the allowance in place (read back); otherwise it re-mints.
- **Uplink's entrypoint** waits (up to `UPLINK_INIT_WAIT_SECONDS`, default
  300) for a marker from the current boot, copies the files and verifies the
  hashes. The Docker daemon ignores `depends_on` when it restarts containers
  after a host reboot; the marker is what stops Uplink from using an earlier
  boot's credential. On a router 401 Uplink re-reads its auth file and, if
  the 401 persists (`UPLINK_ROUTER_401_EXIT_SECONDS`, default 120), exits so
  the restart picks up the current credential. `depends_on` also carries
  `restart: true` so a compose re-run of init restarts Uplink.
- **router-allowance-manager** (SecretVM and generic only): uid 10002, group
  10003, no capabilities, read-only rootfs, internal `router_admin` network
  (proxy-router only), no ports. It reads `/mgr/admin.cookie` (0400) once and
  keeps the allowance equal to the live balance in balance mode; any
  non-empty `UPLINK_MOR_ALLOWANCE_WEI` or a `/mgr/mode` marker other than
  exactly `balance` makes it a permanent no-op. Status:
  `/status/allowance-status.json` (0444). Requests: Uplink creates the
  presence-only flag `/ctl/resync.request` (`O_CREAT|O_EXCL|O_NOFOLLOW`);
  the manager never opens or lists it. `/mgr` is root:10002 0750, `/status`
  10002:10002 0755, `/ctl` 10001:10003 0730; init sets only those mount
  directories (never recursively). `/ctl` and `/status` must not be under
  `DATA_DIR` (Uplink refuses to start otherwise).
- Uplink never asks for direct-payment sessions: the open-session body pins
  `directPayment:false`, and the router client refuses to send any body
  that sets it (`scripts/check-no-direct-payment.sh` guards the source).

Tests: `scripts/test-router-user-init.sh`, `scripts/test-allowance-manager.sh`,
`scripts/check-allowance-manager-policy.sh`, `scripts/check-no-direct-payment.sh`
and the compose e2e `scripts/e2e-secretvm-handoff.sh`.

### Sessions across restarts

**Stop.** The `uplink` service has no `init: true`: the entrypoint `exec`s
`su-exec uplink /usr/local/bin/uplink`, so the app is PID 1 and receives
`docker stop`'s SIGTERM itself (no CAP_KILL needed). It stops accepting
requests, lets in-flight ones finish for up to 30 s, then closes every pooled
and on-chain open session (`CLOSE_SESSIONS_ON_EXIT`, default on).
`stop_grace_period: 45s` leaves room for those 30 s plus the closes. CI
checks every template for this (`check-allowance-manager-policy.sh`, rule S).

**Boot reconcile.** A close on stop is best-effort; a crash, `docker kill`
or a VM wipe leaves sessions open with their stake locked. So on every boot,
before any request may open a session:

- *When:* right after start-up; the entrypoint has already accepted this
  boot's init marker. The first step is `GET /wallet` on the router, so the
  reconcile only proceeds once the router answers with the scoped
  credential. It then lists the wallet's open sessions.
- *Scope: the wallet. A dedicated wallet is required.* The router does not
  tell the scoped credential which router user opened a session, so the
  reconcile acts on every open session of the wallet. Recording the ids
  Uplink opened in `DATA_DIR` was considered and not used: a VM wipe or a
  lost volume, the case the reconcile exists for, would lose that record
  too and leave every session orphaned. If the wallet is shared with
  another client, that client's expired sessions, and its second session
  for a model Uplink also adopts, may be closed. Closing returns the
  stake (on hold at most until the next UTC day) and costs gas; it is a
  liveness issue for that client, not a loss.
- *Adopted:* the latest-ending session per model that is not closed and
  has more than the pool's 90 s expiry buffer left. Never extended; no
  session is opened by the reconcile.
- *Closed:* only `expired` (past its end or inside the buffer) and
  `duplicate` (another usable session for a model that already has an
  adopted one).
- *Never closed:* a session whose model is not in the catalog is skipped
  (counted as `skipped`) and left to expire. Model ids are compared
  case-insensitively. An empty catalog (`{"models":[]}` or `{}`) or one
  that cannot be read is treated as unknown: nothing is skipped for it and
  usable sessions are adopted.
- *Cap:* at most 20 closes per pass (`MaxBootCloses`), expired first. The
  rest are counted as `deferred` and a `session_reconcile_cap` audit event
  is written. Deferred expired sessions are closed by later scans; deferred
  duplicates are left to expire.
- *Gate:* session opens wait until the list succeeded and adopt/close is
  decided, so a request that arrives during boot uses the adopted session
  instead of opening a second one. The gate is then released and the
  closes run in the background, so a slow router close never holds
  requests. Status polls during the reconcile show the previous scan.
- *List failure or partial list:* an error on any page, or a row that
  cannot be verified (no id, no model, no end time), means no closes and
  no adopts for that attempt. It is retried with backoff (2 s doubling,
  15 s cap) for up to 60 s (`UPLINK_RECONCILE_GATE_SECONDS`). After that
  the gate opens, but Uplink does not open a new session until some session
  list succeeds (the request gets an error instead), so it never opens next
  to a session it could not see. The reconcile keeps retrying every 30 s
  (`UPLINK_RECONCILE_RETRY_SECONDS`); such a late pass closes expired
  sessions only, because the pool may already be serving a "duplicate".
- *Failed closes:* counted as `failed`. An expired session whose close
  failed is closed by a later scan (housekeeping and the pool close
  expired sessions they see); a duplicate whose close failed is left to
  expire.
- *Idempotent:* re-running it adopts the same sessions and closes nothing
  new.
- *Audit:* one `session_reconcile_adopt` event per adopted session, one
  `session_reconcile_close` event per close (reason `expired` or
  `duplicate`, result, first 10 characters of the session id), a
  `session_reconcile_cap` event when the cap is hit, and one
  `session_reconcile` summary line per pass (`open`, `adopted`, `closed`,
  `failed`, `skipped`, `deferred`, `attempts`, `late`); a
  `"result":"deferred"` line when the gate opened before a list succeeded.
  No secrets; no full session ids.
- *Visibility:* `/admin/status` (`sessionReconcile`), the GUI Status tab,
  and the status file `DATA_DIR/session-reconcile.json` (uplink, 0640,
  written atomically) show the state (`reconciling`, `closing`, `ok`,
  `partial`, `deferred`, `retrying`), the time of the last pass, its
  counts, and whether new opens are held.

Session ids in Uplink's other log lines are shortened to the first 10
characters as well.

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
internal/api/        HTTP: /v1/*, /admin/*, health
internal/gui/        embedded single-page admin UI
deploy/              SecretVM + generic + Railway overlays (CI pins digests)
```

Concept sketch (optional): [`.ai-docs/UPLINK_CONCEPT.md`](../.ai-docs/UPLINK_CONCEPT.md).

---

[← Updates](05-updates.md) · [Guide home](README.md)

## Quiet boot / log forwarding

Uplink does not print config or secrets at process start (listen line only).
Forward container stdout/stderr with your host log driver (json-file rotation
is set in compose) or a SIEM agent. Do not scrape environment variables from
the runtime.
