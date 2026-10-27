# 6. Developers

[← Updates](05-updates.md) · [Guide home](README.md) · [Root README](../README.md)

How the sausage is made — local runs, env vars, CI, repo layout. Operators
who only want to *run* Uplink can stop at chapters **1–5** and the root
[Get running](../README.md#get-running-secretvm) CTA.

---

## Key model (detail)

| Key | Derivation | Prompt (`/v1/*`) | Admin (`/admin/*`, `/node/*`) |
|---|---|---|---|
| **Master** `sk-uplink.…` | HMAC(seed, master) | yes | only with `MASTER_KEY_ADMIN=true` (default off); then the same power as the admin password, without 2FA, except the session-only routes below |
| **Prompt** `sk-prompt.…` | HMAC(seed, prompt) | yes | no — default for clients |
| Generated `sk-…` | random; hash stored | yes | no |

GUI login is `POST /admin/login` with `ADMIN_PASSWORD` (+ TOTP / recovery
code once 2FA is enrolled), which sets an HttpOnly session cookie; Basic auth
is rejected on `/admin/*`. The Master key can drive admin APIs as a Bearer
token only when `MASTER_KEY_ADMIN=true` (default off); each such request
writes an `admin_key_use` audit record (aggregated: the first use per
client address per minute is written at once, repeats become one
`count=N` line). The session-only routes
`POST /admin/logout`, `GET /admin/2fa/status`, `POST /admin/2fa/enroll/start`,
`POST /admin/2fa/enroll/confirm` and `POST /admin/step-up` require the admin cookie session: any
Authorization header is refused on session-only routes (any scheme or case,
empty or repeated, the Master key included) with the same 401 as a request
without credentials, whatever `MASTER_KEY_ADMIN` says.

### Admin sessions and step-up

- **Lengths** are fixed constants: idle 4h, absolute 12h, the same whether
  2FA is on or off. There is no environment variable or GUI setting for
  them. The cookie `MaxAge` is 12h; requests with `X-Uplink-Poll: 1` do not
  extend idle. `/admin/status` reports `adminSession` (`idle`, `idleSec`,
  `absolute`, `absoluteSec`, `stepUpMaxAgeSec`, and `warnings`, which holds
  the informational `2FA is off` while 2FA is off).
- **Step-up**: `POST /admin/step-up {"password", "code"}` (cookie session
  only, CSRF-checked, under the login lockout, checked before the
  password). With 2FA enrolled `code` must be a current TOTP code (a step
  works once); a recovery code is refused without being consumed. With 2FA
  not enrolled the password alone is checked. Success stamps the session
  and audits `step_up`. Credentials are read from the POST body only.
- **Key reveal**: `GET /admin/keys?reveal=1` returns full keys only on a
  cookie session that logged in or stepped up within 30 minutes; otherwise
  `401 step_up_required`. With any Authorization header it is `403`,
  whatever `MASTER_KEY_ADMIN` says; the masked listing over the Bearer is
  unchanged.
- **Client address**: a change of client address within a session is
  audited once per change as `admin_session_ip_change` (aggregated per
  session per minute). The session is not bound to an address.
- **Master Bearer lockout**: the Bearer branch of the admin routes has its
  own per-client-address lockout (5 free failures, then 2^(n-5) s up to
  15 min), checked before the key compare. It neither reads nor feeds the
  global login trip, so a password flood does not lock the Master key out
  from other addresses. Each rejected Bearer is audited as
  `admin_bearer_reject` (path and address only).

### Audit streams

Two JSON Lines files in `DATA_DIR` (directory 0750), opened append-only
with `O_NOFOLLOW`, mode 0600:

| File | Holds |
|---|---|
| `audit-security.jsonl` | Successful admin and credential events: `login`, `step_up`, `key_reveal`, `key_create` / `key_import` / `key_revoke`, 2FA changes, `setting_change`, a successful `session_open_long`, `pool_close` / `pool_close_all`, `admin_session_ip_change`, `admin_key_use`, `balance_drop` |
| `audit.jsonl` | Failures and agent or system events: `admin_bearer_reject`, login and step-up failures, other rejects, `session_release`, a refused `session_open_long`, `provider_failover`, `session_reconcile*` |

- **Aggregation**: unauthenticated failures (`login`, `login_lockout`,
  `admin_bearer_reject`) are written at most once per minute per (client
  address, event), and agent events per (key id, event); repeats become
  one line with `count`. The map holds about 10,000 keys; beyond that new
  keys share one `other` line per event. `admin_key_use` and
  `admin_session_ip_change` are aggregated the same way (64 keys per event).
- **Rotation**: at 5 MiB a file moves to `.1` and `.2`; at most 3 files per
  stream.
- **Retention**: `AUDIT_RETENTION_DAYS` (default 90, 1…3650) for
  `audit-security.jsonl`, 30 days for `audit.jsonl`. Whole rotated files
  whose newest line is older are deleted at boot and daily; the live file
  is never rewritten.
- **Safety**: a symlink, a non-regular file or a file owned by another uid
  is refused (logged once; the in-memory list only). A write error is
  logged once and is not fatal.
- **Content**: `json.Marshal` only. Paths without the query, at most 256
  printable ASCII bytes; never the User-Agent, a token or any part or hash
  of one.
- **Boot**: the last 500 lines of each file are loaded into the in-memory
  list behind `GET /admin/audit` (same shape, plus `stream` and `count`)
  and Security alerts. Status `disk.audit` reports the file sizes.

### Admin 2FA and recovery codes

- **2FA switch** (operator choice in the GUI; no environment variable turns
  2FA on, on any deploy target). Stored as `auth.twoFactorOn` in
  `uplink-state.json` in `DATA_DIR` (mode 0640, atomic write like the rest
  of the state), so it survives restarts and redeploys. Default off: the
  first login is password only, and the Security tab shows a non-blocking
  note recommending turning it on. The switch and the confirmed TOTP secret
  change together, in one store update: enroll confirm stores the secret
  and sets the switch on; Remove wipes the secret and recovery codes and
  sets it off. Starting an enrollment does not turn it on. While on, every
  admin route and `/node/*` enforce the second factor exactly as before.
  Fail closed: login asks for a TOTP or recovery code whenever a secret is
  stored, whatever the switch says, and a state file that cannot be parsed
  stops Uplink at boot instead of starting with 2FA off.
- **`MASTER_KEY_ADMIN` parsing** (`internal/config`): unset, empty or blank
  means false; a boolean (`true`/`false`/`1`/`0`, surrounding space
  ignored) is that value; anything else stays false, logs one warning
  naming the variable (never its value) and shows it on the Security tab
  (`configWarn` in `/admin/2fa/status` and `/admin/status`).
- **TOTP** (RFC 6238, 30 s, one step of clock skew). The last accepted step
  is stored, so a code works once; replays get 401 (`bad_totp` or
  `totp_reuse` in the audit, never the code).
- **Recovery codes**: enrolling issues 10, shown once (copy, `.txt`
  download). Format `xxxx-xxxx-xxxx` from `a-z2-7` (12 symbols, 60 bits,
  `crypto/rand`). Input is case- and dash-insensitive. Stored next to the
  TOTP secret in `uplink-state.json` (same file, same mode) as
  HMAC-SHA256(pepper, salt || code) with a 16-byte salt per code; the pepper
  is derived from `API_KEY_SEED` with HKDF. Rationale in
  `internal/adminauth/recovery.go`. Matching is constant-time over every
  stored code and consumes the match inside one store update, so a code
  works once even under concurrent logins. Recovery logins share the login
  lockout (and, on SecretVM, the edge rate limit).
- **Endpoints** (CSRF and session rules as for other admin POSTs):
  `GET /admin/2fa/status` (`on`, `enrolled`, `need2fa`, `recoveryRemaining`),
  `POST /admin/2fa/enroll/start`, `POST /admin/2fa/enroll/confirm`
  (returns the codes), `POST /admin/2fa/recovery/regen`,
  `POST /admin/2fa/reset`, `POST /admin/2fa/remove`. Enroll start needs
  `{"password"}` and confirm `{"password", "code"}` (wrong password: 401
  `invalid password`, counted by the lockout); a confirmed enrollment
  revokes every admin session and re-issues the caller's cookie. Regen
  needs `{"password", "code"}` with a TOTP code only. Reset and remove need
  `{"password", "code"}`, where `code` is a TOTP or a recovery code.
- **Lifecycle.** Turn on (enroll): Security tab switch, scan the QR code,
  confirm a code; the 10 recovery codes are shown once. Regenerate:
  replaces all codes (password plus a code), shown once. Reset: below.
  Turn off (Remove): below. In the GUI, Regenerate, Reset and Turn off stay
  disabled until both the password and the code field are filled.
- **Reset** wipes the TOTP secret, all recovery codes and any pending
  enrollment, and persists that in one store update. It revokes every admin
  session (this one too), clears the cookie, and audits `2fa_reset` with
  `sessions_revoked`. The GUI drops to the login screen with the Security
  view cleared. The next password login goes straight to a new enrollment
  with a fresh secret; the old secret and old recovery codes never work
  again. The 2FA switch stays on, so every other admin route answers 403
  `2fa enrollment required` until that enrollment is confirmed, and the GUI
  shows only the Security tab. This enroll-only state is the one case where
  the switch is on without a secret; it grants nothing but enrollment.
- **Remove** (`POST /admin/2fa/remove`, the switch's off side) needs a
  valid session, CSRF, the password and a TOTP or recovery code (consumed;
  a TOTP step works once), under the login lockout. In one
  store update it wipes the secret and recovery codes and sets the switch
  off; then it revokes every admin session (this one too), clears the
  cookie and audits `2fa_remove` with `sessions_revoked`. The next login is
  password only.
- **Audit**: `2fa_enroll`, `recovery_codes_issued` (count),
  `recovery_code_used` (remaining count), `recovery_codes_regenerated`,
  `2fa_reset` (sessions revoked), `2fa_remove` (sessions revoked; `fail`
  with the reason on a rejected attempt). Codes and secrets are never
  logged.
- **`API_KEY_SEED` rotation** invalidates the TOTP secret (its encryption
  key is derived from the seed) and every recovery code (their pepper is
  too). With 2FA enrolled, admin login then fails until 2FA is cleared with
  `UPLINK_RESET_2FA`.
- **`UPLINK_RESET_2FA=true`** (emergency recovery, env only, default off):
  on the next start Uplink wipes the TOTP secret, all recovery codes, any
  pending enrollment and the 2FA switch (2FA comes back off), ends all
  admin sessions, clears lockouts, and audits
  `2fa_reset_breakglass` (`ok`, `wiped`). It then sets a marker, so later
  restarts with the variable still set do not wipe again; they audit
  `2fa_reset_breakglass` `blocked` and the Security tab asks you to remove
  it. Restart once with it set, then unset it and restart; that clears the
  marker for any future use. Log in with the password and turn 2FA on
  again with the switch. On SecretVM add
  `UPLINK_RESET_2FA=true` to the `uplink_bake_env` block (see
  [02-bootstrap.md](02-bootstrap.md)); on generic, Railway and local set it
  in the environment (`${UPLINK_RESET_2FA:-false}` in the compose).
- Logout and **Revoke all sessions** end the server-side session; there is
  no remembered device, so the next login asks for a fresh code.

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

Bearer API keys only (`requireAPIKey`). Response `{"usage":[ row… ]}`, the
same row shape as `GET /admin/usage`:
`{day, keyId, model, modelId, requests, promptTokens, completionTokens, sessionSeconds}`.
Usage is recorded by the resolved registration id, so name and id calls
share a row. `model` is the display label `name (0x + 6 hex)`, or the short
id when the catalog does not list the id; `modelId` is the full id. Rows
stored under a model name by older versions are resolved and merged on
read; a name that no longer resolves is shown as stored with no `modelId`.

| Caller | Scope |
|--------|--------|
| Prompt / generated | Rows where `keyId` equals the caller’s id |
| Master | Full instance (intentional ops convenience) |

**WARN:** Master is a full usage reader so automation can poll without an
admin session. Prompt/generated never see other keys’ rows. `keyId` is public — not
`sk-`. Empty `X-Uplink-Key-Id` after auth fails closed (no unfiltered dump).
Basic auth on `/v1/usage` → 401. Keep `GET /admin/usage` for the GUI.

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
| **Railway** | `docker-compose.railway.deployed.yml` (reference) + `deploy/railway/` | Railway does not run compose: copy its digests and vars into the services; single-replica router |
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

Product rule: **form stays six**; tunables are not Encrypted Secrets.
Sources: `internal/config/config.go`, SecretVM `uplink_bake_env` /
`router_network_env`.

Generic, Railway and local compose templates take every tunable as an
env-overridable default (`${VAR:-default}`): leave it unset for the
default, or set it in `.env` or the shell. Overridable in all three:

- Uplink: `SESSION_DURATION_SECONDS`, `SESSION_FAILOVER`,
  `UPLINK_PROVIDER_COOLDOWN_SECONDS`,
  `ACTIVE_MODELS_URL`, `GATEWAY_BIDS_URL`, `HOUSEKEEPING`,
  `UPLINK_RESET_2FA` (default `false`).
- Router: `PROXY_STORE_CHAT_CONTEXT`, `PROXY_FORWARD_CHAT_CONTEXT`,
  `LOG_LEVEL_APP` / `LOG_LEVEL_TCP` / `LOG_LEVEL_ETH_RPC`,
  `ETH_NODE_CHAIN_ID`, `ETH_NODE_USE_SUBSCRIPTIONS`, `BLOCKSCOUT_API_URL`,
  `DIAMOND_CONTRACT_ADDRESS`, `MOR_TOKEN_ADDRESS`.
- Generic and Railway only: `MASTER_KEY_ADMIN` (default `false`). Turning
  the master key admin access on weakens admin security.

2FA has no environment variable: it is the switch on the Security tab
(default off, recommended on; see [Admin 2FA](#admin-2fa-and-recovery-codes)),
stored in `DATA_DIR`. Keep `DATA_DIR` on a persistent volume.

The local compose fixes `WEB_PUBLIC_URL` to `http://localhost:8080` and
takes the router credential as `ROUTER_AUTH`. **Railway caveat:** Railway
does not run compose, so `${VAR:-default}` is not evaluated there; set
plain values in the service variables (Railway's own reference syntax is
`${{SERVICE.VAR}}`). An unset variable takes the process's own default, not
the compose one; the Railway env example sets the vars where that is less
safe (`PROXY_STORE_CHAT_CONTEXT`, `LOG_LEVEL_APP`, `ETH_NODE_CHAIN_ID`)
actively, and `scripts/deploy-assets.sh` checks it.
See [deploy/railway/README.md](../deploy/railway/README.md#unset-on-railway).

SecretVM has no `${VAR:-default}` overrides: it turns every `${...}` into a
form field, so its compose uses exactly the six secrets and keeps tunables
in the `configs:` block (`uplink_bake_env`, `router_network_env`).
`MASTER_KEY_ADMIN=false` is fixed there.

Each env example (`env.template`, `env.example`, `.env.example`) lists only
keys its compose reads; `scripts/deploy-assets.sh` enforces that (see
[Release assets](#release-assets)).

### 1. Minimum to run (six operator fillables)

| Var | Required | Purpose |
|---|---|---|
| `WALLET_PRIVATE_KEY` | yes (deploy) | Consumer wallet, **router only** (Uplink holds no key; reclaim runs via the router API) |
| `ETH_NODE_ADDRESS` | yes (deploy) | Base HTTPS RPC |
| `COOKIE_CONTENT` | yes | Router `user:pass` (proxy-router + Uplink). Uplink reads `ROUTER_AUTH` first, then `COOKIE_CONTENT` |
| `ADMIN_PASSWORD` | yes | GUI/admin login (`admin`) |
| `API_KEY_SEED` | yes | Master/Prompt derivation (`openssl rand -hex 32`) |
| `WEB_PUBLIC_URL` | yes (deploy) | Public HTTPS origin |

### 2. Uplink tunable / additional (Uplink process only)

| Var | Default | Purpose |
|---|---|---|
| `ACTIVE_MODELS_URL` | `https://active.mor.org/gateway_models.json` | Gateway model catalog (bidDetail primary for list/open) |
| `GATEWAY_BIDS_URL` | `https://active.mor.org/gateway_bids.json` | Companion bids feed (not merged into LowestBid) |
| `SESSION_DURATION_SECONDS` | `600` | Default session length (stake scales), 600…86400; above `UPLINK_SESSION_DURATION_MAX` it is clamped down to the max |
| `SESSION_FAILOVER` | `true` | Uplink moves a model to another provider when the provider fails a prompt ([Provider failover](#provider-failover)); `false` relays the provider's error. The router's own failover is always off (`failover:false` on every open) |
| `UPLINK_PROVIDER_EXCLUDE` | (empty) | Providers never opened for a model: `<model>=<provider>[,<provider>]…`, entries separated by `;`; model is an id or a catalog name ([Provider failover](#provider-failover)). Bad entries are warned and ignored |
| `UPLINK_PROVIDER_COOLDOWN_SECONDS` | `600` | How long a provider that failed a prompt or an open is avoided for that model; `0` keeps no memory across requests (the immediate retry still avoids the provider that just failed) |
| `UPLINK_SESSION_DURATION_MAX` | `1800` | Longest session a client may ask for with `X-Uplink-Session-Duration`, 600…86400; every open is capped at it, with or without headers; never raised by the default ([Long jobs](#long-jobs)) |
| `UPLINK_FORWARD_IDLE_SECONDS` | `180` | Default idle timer per inference call (no bytes from upstream), 30…600 |
| `UPLINK_SESSION_IDLE_GRACE_SECONDS` | `300` | A session with nothing in flight and no use for this long is closed early, 60…3600 |
| `UPLINK_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error`; also a console setting ([Operator settings](#operator-settings)). Runtime warnings are written at `warn`, so at `error` they are dropped (warnings about the configuration itself still print at boot, before the level applies); audit lines on stdout are always written (pinned at `info`) |
| `UPLINK_INFLIGHT_MAX` | `32` | In-flight inference requests across all keys, 1…1024 (4 per key, fixed) |
| `UPLINK_LONG_PER_KEY_MODEL` | `1` | Long sessions one generated key may hold per model, 0…16 |
| `UPLINK_LONG_PER_KEY` | `2` | Long sessions one generated key may hold in total, 0…64 |
| `UPLINK_LONG_SESSIONS_MAX` | `3` | Long sessions open at once across all keys, 0…256 |
| `UPLINK_OPENS_PER_KEY_MODEL_HOUR` | `2` | Long opens (and reopens for remaining life) per key per model per rolling hour, 0…120 |
| `HOUSEKEEPING` | `true` | Post-midnight stake reclaim (via router API) |
| `UPLINK_LISTEN` | `:8080` | Listen address |
| `ROUTER_URL` | `http://proxy-router:8082` | C-Node admin API |
| `DATA_DIR` | `./data` | JSON state |
| `USAGE_PRUNE_DAYS` | `30` | Admin usage-history prune retention |
| `AUDIT_RETENTION_DAYS` | `90` | Retention of `audit-security.jsonl` rotated files, 1…3650 ([Audit streams](#audit-streams)) |
| `SESSION_DIRECT_PAYMENT` | ignored | Removed: sessions are always staking-mode (`directPayment:false`); setting `true` only logs a warning |
| `CLOSE_SESSIONS_ON_EXIT` | `true` | On a graceful stop, close every open session on the wallet (as of the last scan, including uncatalogued models and other clients' sessions); `false` leaves them for the next boot reconcile |
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

## Router access and restart safety

- Uplink calls proxy-router over the internal network with the router's
  own credential, `ROUTER_AUTH` or else `COOKIE_CONTENT` (the same
  `user:password` the router uses). There is no separate scoped router user
  and no allowance service; MOR approvals stay on the router. The router
  port is never published.
- `/node/*` is an admin-gated passthrough for read-only router inspection
  by scripts. The GUI does not link it, and the SecretVM edge (Traefik)
  does not route `/node` publicly. Only GET, HEAD, POST and DELETE are
  forwarded; any other method gets 403. Paths are normalized before they
  are matched, and anything ambiguous is refused. Refused for every method,
  with a `node_passthrough_block` audit event (method and path only):
  - sending or approving funds;
  - opening sessions;
  - router file-access routes;
  - routes that make the router contact a caller-chosen address.

  Refused except for reads (GET and HEAD pass through):
  - wallet, router users and permissions, and router config;
  - creating providers, models and bids.

  The router receives exactly the normalized path, without method-override
  headers or the admin cookie, and its redirects are not followed (502).
  The route list, with router source references, is in
  `internal/api/nodeproxy.go` and is pinned by `internal/api/nodeproxy_test.go`.
- Admin responses (`/admin/*`, `/node/*`) carry `Cache-Control: no-store`
  and `Pragma: no-cache`. Cookie-authenticated non-GET admin requests need
  both `Sec-Fetch-Site: same-origin` and an `Origin` matching
  `WEB_PUBLIC_URL`.
- The router image is `FROM scratch` (no shell, no wget), so it has no
  container healthcheck and `depends_on` is start order only. Uplink retries
  the router itself; `/admin/status` reports `routerAuth` (a 401 means the
  cookies do not match).
- Uplink never asks for direct-payment sessions: the open-session body pins
  `directPayment:false`, and the router client refuses to send any body
  that sets it (`scripts/check-no-direct-payment.sh` guards the source).

Tests: `scripts/check-no-direct-payment.sh`, `scripts/check-compose-policy.sh`,
the mock-router e2e `scripts/e2e-mock-router.sh` (session logic) and the
real-router smoke `scripts/e2e-real-router.sh` (pinned v7.17.0 image).

### Sessions across restarts

**Stop.** The `uplink` service has no `init: true`: the entrypoint `exec`s
`su-exec uplink /usr/local/bin/uplink`, so the app is PID 1 and receives
`docker stop`'s SIGTERM itself (no CAP_KILL needed). It stops accepting
requests, lets in-flight ones finish for up to 30 s, then closes sessions
(`CLOSE_SESSIONS_ON_EXIT`, default `true`). The whole stop is bounded to
40 s from SIGTERM, with at most 8 s per close; a session not closed in time
is logged as "left for boot reconcile". `stop_grace_period: 45s` leaves
room for this. CI checks every template for it
(`check-compose-policy.sh`, rule S).

A graceful `docker stop` closes every open session on the wallet, as of
Uplink's most recent session scan: its own pooled sessions, sessions for
models that are not in the catalog, and, on a shared wallet, the other
client's sessions. Closing returns the stake (on hold at most until the next
UTC day); it is a lock, not a loss, and costs gas per close. Set
`CLOSE_SESSIONS_ON_EXIT=false` to opt out: sessions then stay open across a
stop and the next boot reconcile adopts or closes them.

**Boot reconcile.** A close on stop is best-effort; a crash, `docker kill`
or a VM wipe leaves sessions open with their stake locked. So on every boot,
before any request may open a session:

- *When:* right after start-up. The first step is `GET /wallet` on the
  router, so the reconcile only proceeds once the router answers. It then lists the wallet's open sessions.
- *Scope: the wallet. A dedicated wallet is required.* The router does not
  report which client opened a session, so the
  reconcile acts on every open session of the wallet. Recording the ids
  Uplink opened in `DATA_DIR` was considered and not used: a VM wipe or a
  lost volume, the case the reconcile exists for, would lose that record
  too and leave every session orphaned. If the wallet is shared with
  another client, that client's expired sessions, and its second session
  for a model Uplink also adopts, may be closed at boot, and a graceful
  `docker stop` closes all of its open sessions (see *Stop* above).
  Closing returns the stake (on hold at most until the next UTC day) and
  costs gas; it is a liveness issue for that client, not a loss.
- *Listing window:* the list is read newest first and stops after three
  pages of only closed sessions. A very old open session beyond that is
  neither adopted nor closed by the reconcile; it expires on its own.
- *Adopted:* the latest-ending session per model that is not closed and
  has more than the pool's 90 s expiry buffer left. Never extended; no
  session is opened by the reconcile.
- *Closed:* only `expired` (past its end or inside the buffer) and
  `duplicate` (another usable session for a model that already has an
  adopted one).
- *Never closed:* a session whose model is not in the catalog is skipped
  (counted as `skipped`) and left to expire. Model ids are compared
  case-insensitively, and the pool keys sessions by the lowercase model
  id, so a session adopted under a differently cased id is reused. An empty catalog (`{"models":[]}` or `{}`) or one
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
  failed, or that the cap deferred, is closed by a later scan
  (housekeeping and the pool close expired sessions they see), with the
  same `session_reconcile_close` audit event marked `"source":"rescan"`;
  a duplicate whose close failed is left to expire.
- *Idempotent:* re-running it adopts the same sessions and closes nothing
  new.
- *Audit:* one `session_reconcile_adopt` event per adopted session, one
  `session_reconcile_close` event per close (reason `expired` or
  `duplicate`, result, first 10 characters of the session id), a
  `session_reconcile_cap` event when the cap is hit, and one
  `session_reconcile` summary line per pass (`open`, `adopted`, `closed`,
  `failed`, `skipped`, `deferred`, `attempts`, `late`, and `catalog`, the
  number of catalog models, `-1` when unknown); a
  `"result":"deferred"` line when the gate opened before a list succeeded.
  No secrets; no full session ids.
- *Visibility:* `/admin/status` (`sessionReconcile`), the GUI Status tab,
  and the status file `DATA_DIR/session-reconcile.json` (uplink, 0640,
  written atomically) show the state (`reconciling`, `closing`, `ok`,
  `partial`, `deferred`, `retrying`), the time of the last pass, its
  counts, and whether new opens are held.

Session ids in Uplink's other log lines are shortened to the first 10
characters as well.

### Provider failover

Uplink keeps one session per model, and a session is tied to one
provider. When that provider fails a prompt, Uplink moves the model to
another provider and sends the request once more. The router's own
in-prompt failover is always off (every open sends `failover:false`), so
the pool stays in charge of which session is open. `SESSION_FAILOVER=false`
turns Uplink's failover off: errors are relayed and the session is kept.

- *Fails over* (the provider is at fault): a provider model error with
  status 402, 401, 403 or 404 (for example the provider's backend is out
  of credit or rejects its own key); any 429; any 5xx; and a provider
  model error with 400, 413 or 422 whose text says the provider is
  impaired (rate limit, too many requests, capacity, overloaded, model or
  service unavailable, or `"statusCode":429` / `503` in the body), or
  whose text reports a cut of the provider's own stream (`error reading
  stream`, `unexpected eof`, `connection reset`, `broken pipe`). These cut
  strings count only inside a provider model error body from the router,
  never when they come from Uplink's own socket to its client.
- *Relayed with the router's real status* (not a blanket 502), no
  failover, no cool-down: other 4xx (the request itself is wrong, for
  example a bad parameter), TEE attestation or verification failures, and
  `request cancelled while waiting in queue`. A provider model error is
  returned in the OpenAI error shape (`error.message`, `error.type`,
  `error.code`) with the provider's body kept under `providerModelError`.
  A 401/403 from the router itself (Uplink's router credential) stays a
  502, since it is not the client's key.
- *Not a provider fault:* Uplink cannot reach its own router (502), or the
  router rejects the session id (the existing reopen on a dead session).
- *Guardrail:* before closing anything, Uplink lists the model's rated
  bids and counts distinct providers outside the failed one and those in
  cool-down. With none, it keeps the session (stakes lock while a session
  is open, so a close and reopen onto no other provider only ties up
  liquidity) and relays the original error (`no_alternate`).
- *Order:* under the model's open lock, Uplink closes the failed session
  first (synchronously, up to 30 s) and then opens on another provider:
  with one excluded provider, by model with the router's `omitProvider`;
  with more, by bid over the rated bids (best score first, at most 3
  tries, one per provider). A failed close is logged and the session is
  retired: Rehydrate never adopts it back.
- *Once:* one failover and one retry per request. A provider failure on the
  retry puts that provider in cool-down too and is relayed. Concurrent
  failures on one session do one close and one open; the others retry on
  the replacement.
- *Cool-down* (`UPLINK_PROVIDER_COOLDOWN_SECONDS`, default 600): per model
  and provider, in memory (a restart forgets it). New opens and Rehydrate
  avoid cooling providers; if every provider of the model is cooling, a
  normal open is made (it still avoids configured exclusions). `0` keeps no memory across requests; the immediate
  retry still avoids the provider that just failed.
- *Streaming:* an error before the first chunk arrives as a non-2xx and
  fails over like any other request. After part of the answer was sent,
  the answer cannot be restarted. Two upstream events count as a
  mid-stream provider cut: a provider error line, and the router body
  ending (or failing to read) without `data: [DONE]` or a `finish_reason`
  while the request is still live and no Uplink timer ended it. Either
  way the client gets one
  `data: {"error":{"message":...,"type":"provider_error","code":N}}`
  event (`code` 502 for a bare cut) and the stream ends. The provider goes
  into cool-down and the session is retired (only when another provider
  exists). The model's next open walks the rated bids by bid without that
  provider, even when it is the only one cooling or the cool-down is `0`.
- *Never a provider fault:* the client hanging up, a write error to the
  client (broken pipe, connection reset, write deadline), a cancelled
  request, and Uplink's own idle or total timeout. These end the call and
  are logged; nothing is cooled, retired or failed over, and the pooled
  session is kept. A client hang-up also cancels the upstream request.
- *Configured exclusions* (`UPLINK_PROVIDER_EXCLUDE`): providers never
  opened for a model, as `<model>=<provider>[,<provider>]…` with entries
  separated by `;`. The model is a registration id or a catalog name; a
  provider is its `0x` address. Opens and Rehydrate never use them, not
  even in the fallback when every other provider is cooling or was just
  cut. When only excluded providers serve the model, the request gets
  `503` `no_eligible_provider` (no ids in the body) and nothing is opened.
  A session the router opened on an excluded provider anyway is closed at
  once and answered the same way, and so is one whose provider cannot be
  read back after an `omitProvider` open (on failover the caller gets the
  original upstream error). When the rated bids cannot be read, one
  exclusion is sent as `omitProvider`; with two or more nothing is opened
  (`502`). An entry keyed by catalog name needs the model's name: while the
  catalog has no name for the requested id (for example the model feed is
  down at boot and the client sends the `0x` id), opens for that model are
  refused with `503` `no_eligible_provider`, failover keeps the session,
  and on-chain sessions are not adopted, until the name resolves. Entries
  keyed by model id never need the catalog.
  Bad entries are logged by position and ignored.
- *Audit:* a `provider_failover` event (Security alerts) with the model,
  the first 10 characters of the session id, the failed provider's short
  address, the status class (`402`, `429`, `5xx`, `stream_cut` for a bare
  upstream cut, ...), and the outcome:
  `retried_ok`, `retry_failed`, `no_alternate`, `reopen_failed`,
  `long_session_limit` (a long replacement counts toward the long-session
  limits like any long open, and was refused),
  `close_failed` (an extra event when the close did not land), or
  `stream_retired`. No request or response bodies, no keys.

### Long jobs

Calls that run for many minutes (agents, long reasoning) get their own
timers and session handling. Source: `internal/api/longjob.go`,
`internal/pool/lease.go`, `internal/router/client.go`.

- *Timers per call.* An **idle** timer (no bytes from upstream; default
  `UPLINK_FORWARD_IDLE_SECONDS`, 180) and a **total** timer (default the
  session length). Before any answer an expiry is a `504`
  `{"error":{"type":"timeout","code":504,"message":"upstream idle timeout"}}`
  (or `total`). Mid-stream the client gets an SSE error event, a chunk with
  `finish_reason: "uplink_idle_timeout"` (or `uplink_total_timeout`) and
  `data: [DONE]`. A non-stream body is not idle-cut once headers arrive;
  the total still applies. These cuts are Uplink's own and never cool a
  provider. A client hang-up cancels the upstream call.
- *Admin calls* to the router keep a 60 s timeout on a separate HTTP
  client; inference has no client-level timeout.
- *Request headers* (all optional; whole numbers, at most 6 digits, sent
  once; anything else is a `400` that names the header):
  - `X-Uplink-Session-Duration`: session length for a new session,
    600…`UPLINK_SESSION_DURATION_MAX`.
  - `X-Uplink-Request-Timeout`: total for this call; clamped to
    30…(max − 120).
  - `X-Uplink-Idle-Timeout`: idle timer for this call; clamped to 30…600.
  When any of them is sent the response carries
  `X-Uplink-Timeouts: idle=<s>;total=<s>` and
  `X-Uplink-Session-Remaining: <s>`. Without them responses are unchanged.
- *Reuse by remaining life.* A pooled session is reused only when its
  remaining life covers the call's total plus 30 s; otherwise a new
  session opens for max(requested, total + 120) s, capped at the max, and
  the old one is retired and closed when its last call ends.
- *Leases.* Every call holds a lease on its session; a session is closed
  (release, idle sweep, failover, reopen) only when no call is in flight.
  A failover on a session another call is still using retires it and
  closes it later.
- *Idle sweep.* Every 30 s, sessions with nothing in flight and unused for
  `UPLINK_SESSION_IDLE_GRACE_SECONDS` are closed so unused stake returns.
- *Long sessions* (longer than `SESSION_DURATION_SECONDS`) are for
  generated keys only; the master and prompt keys get `429`
  `long_session_limit`. Limits: per key per model, per key, across all keys,
  and opens per key per model per hour (envs above). A release does not
  refund the hourly count; counters start at zero after a restart, and
  long sessions adopted at boot count toward the global cap.
- *In-flight caps.* 4 per key and `UPLINK_INFLIGHT_MAX` overall; over
  either is `429` `too_many_requests`.
- *Server deadlines.* `IdleTimeout` 120 s for keep-alive connections; the
  request body must arrive within 60 s; each response write must finish
  within 60 s (reset before every write, so a slow upstream is not cut, a
  client that stops reading is).
- *`POST /v1/sessions/release`* `{"model":"<name or id>"}` (generated
  keys only; master/prompt → `403` `shared_key`; body ≤ 1 KiB, unknown
  fields → `400`; unknown model → `404`). Drops this key's hold on the
  model's session: `{"released":true,"closed":true}` when it was the last
  holder, `{"released":true,"closed":false,"reason":"shared"}` (or
  `in_flight`, or `retry` when the close did not land) and
  `{"released":false,"reason":"not_held"}` otherwise. No ids in any body.
  Audited as `session_release` in the ops stream.
- *Status.* Each pooled session in `/admin/status` shows `inFlight`,
  `holderCount`, `holders` (key ids), `openedAt`, `endsAt`, `opener` and
  `long`.
- *Close endpoints.* `POST /admin/pool/close/{id}` and `close-all` answer
  fixed text on failure; the router's error text is logged only.


### Operator settings

Five values can be changed in the console (**Security → Settings**) or
with `GET` / `POST /admin/settings` (source `internal/api/settings.go`).
Both need the admin cookie session: any `Authorization` header, the
master key Bearer with `MASTER_KEY_ADMIN=true` included, is `403`; a
request from another site is `403` (CSRF check; a `GET` marked
cross-site too); the 2FA enrolment gate applies as on other admin routes.

| Name | Env | Range | Default |
|---|---|---|---|
| `sessionDurationDefault` | `SESSION_DURATION_SECONDS` | 600…86400 | `600` |
| `sessionDurationMax` | `UPLINK_SESSION_DURATION_MAX` | 600…86400 | `1800` |
| `sessionIdleGrace` | `UPLINK_SESSION_IDLE_GRACE_SECONDS` | 60…3600 | `300` |
| `forwardIdle` | `UPLINK_FORWARD_IDLE_SECONDS` | 30…600 | `180` |
| `logLevel` | `UPLINK_LOG_LEVEL` | `debug` `info` `warn` `error` | `info` |

- *Precedence* is env > saved > default. A setting whose env var holds a
  valid value is shown as "set by environment (ENV)" and cannot be changed
  (`409`); remove the env var and restart to manage it in the console.
- *Saving.* `POST {"name":"forwardIdle","value":240}` or
  `{"name":"forwardIdle","reset":true}` (one JSON object, body ≤ 1 KiB;
  unknown fields or trailing data → `400`). Values are checked against the ranges above, and the default may
  not exceed the max (`400`). The max always wins: at boot a default above
  the max (from env or saved) is clamped down to it with a warning, and
  every session open is capped at the max. A saved value ignored at boot
  (out of range) is logged by name and written to the ops audit stream as
  `setting_ignored`. Settings are saved beside the 2FA state in
  `uplink-state.json` (a fresh temp file in the same directory, fsync,
  rename, directory fsync; mode 0640; a symlinked state file is refused; at boot, temp files a crash left behind, regular files named `uplink-state.json.<digits>.tmp` directly in `DATA_DIR`, are removed, and symlinks or anything else are left alone) and apply at once:
  new sessions, the next call's idle timer, the next idle sweep and the
  log level, no restart.
- *Audit.* Each change is a `setting_change` event (`name old→new`) in the
  high-value stream. The log level filters runtime logs only: the audit
  mirror on stdout is pinned at `info`, so `warn` or `error` never hides
  an audit line.
- *Not settable:* the admin session timeouts (fixed 4 h idle, 12 h
  absolute), `MASTER_KEY_ADMIN`, and every secret.

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
| `deploy/**` | Image entrypoint + release compose/env templates |
| `docker-compose.local.yml` | Local-stack / ASCII contract in CI |
| `.github/workflows/build.yml` | Pipeline itself |

**Skipped** (no test run, no version bump): `docs/**`, root markdown
(`README`, `AGENTS`, `DISCLAIMER`, …), `llms*.txt`, `.ai-docs/**`, etc. A PR that mixes docs + `internal/` still
runs — any matching path is enough.

Keep the `uplink` GHCR package **public** so SecretVM can pull anonymously.
Workflow: [`.github/workflows/build.yml`](../.github/workflows/build.yml).

`scripts/**` and `.env.example` also trigger pull_request CI (not a
release), so the checks, e2e and asset check run when they change.

### Release assets

`scripts/deploy-assets.sh` builds the release assets from the templates and
checks them; the release job and the pull_request job run the same script.
The publish job runs `generate` + `check` twice: before the image push
against a placeholder digest (a bad asset set fails before anything is
published), and after it with the real digest, which only exists once the
multi-arch image is pushed.

| Asset | Template | Env example source |
|-------|----------|--------------------|
| `docker-compose.secretvm.deployed.yml`, `env.secretvm.example` | `deploy/secretvm/docker-compose.yml` | `deploy/secretvm/env.template` |
| `docker-compose.generic.deployed.yml`, `env.generic.example` | `deploy/generic/docker-compose.yml` | `deploy/generic/env.example` |
| `docker-compose.railway.deployed.yml`, `env.railway.example` | `deploy/railway/docker-compose.yml` | `deploy/railway/env.example` |

`docker-compose.local.yml` with `.env.example` is checked too (no asset).

- `generate IMAGE DIGEST DIR` replaces the one Uplink image line with
  `IMAGE@DIGEST` and copies the env examples.
- `check IMAGE DIGEST DIR` enforces:
  - **P**: every image is `@sha256:` pinned, none is `:latest`, and Uplink
    is exactly `IMAGE@DIGEST`;
  - **T**: no `TODO` or `REPLACE` in the assets or templates;
  - **V**: `${...}` variables: SecretVM uses exactly the six secrets; the
    others use their required variables plus the documented overrides
    (with defaults);
  - **E**: each env example is identical to its source, has no duplicate
    keys, names only variables its compose reads, and sets every required
    one; `env.secretvm.example` sets exactly the six.
  - **D**: security defaults (`SECURITY_DEFAULTS`): `MASTER_KEY_ADMIN` is
    `false` in the SecretVM, generic and Railway templates and absent from
    the local compose; `UPLINK_RESET_2FA`, `PROXY_STORE_CHAT_CONTEXT` and
    `PROXY_FORWARD_CHAT_CONTEXT` are `false` wherever they appear. Each
    pinned variable has exactly that value in the compose and its env
    example (default or literal, quoted or with `export`) and is assigned
    at most once per file. Railway's env example must set
    `PROXY_STORE_CHAT_CONTEXT`, `LOG_LEVEL_APP` and `ETH_NODE_CHAIN_ID`
    actively.
- `pr` (pull_request CI, step "Deploy assets") generates against the fake
  digest `sha256:000…0`, runs `check`, then the self-tests: broken
  variants (at least one per failure mode of every rule) that must each be
  rejected by the matching rule. Run it locally with `bash scripts/deploy-assets.sh pr`.

Other pull_request checks in the same job: `check-router-digest.sh`,
`check-compose-config.sh`, `check-compose-policy.sh`,
`check-no-direct-payment.sh`, `check-gui-js.sh`.

---

## Layout

```
cmd/uplink/          entrypoint
internal/config/     env config
internal/keymaker/   sk-… generation, deterministic master key
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

## Quiet boot / log forwarding

Uplink does not print config or secrets at process start (listen line only).
Forward container stdout/stderr with your host log driver (json-file rotation
is set in compose) or a SIEM agent. Do not scrape environment variables from
the runtime.
