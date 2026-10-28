# 5. Updates & portability

[← Apps](04-clients.md) · [Guide home](README.md) · [Next: Developers →](06-developers.md)

---

## New Uplink versions

1. GUI header may show a **new release** chip when [GitHub Releases](https://github.com/MorpheusAIs/Morpheus-Uplink/releases) is ahead of the running build (notify only — you still apply the update on the host).
2. Download the new release compose (`docker-compose.secretvm.deployed.yml`, `docker-compose.generic.deployed.yml`, or for Railway the digests in `docker-compose.railway.deployed.yml`). Keep the **same** secrets / `.env` (`API_KEY_SEED`, `WALLET_PRIVATE_KEY`).
3. Export ephemeral keys **before** wiping storage if volumes recreate.
4. After restart: Status health → Probe once → reclaim if **To claim** &gt; 0.

---

## Upgrading to a build with settings in one place

Console settings are no longer read from `.env` or the platform's
variables. A value set there for a setting on the console **Settings** tab
is ignored: Uplink logs one warning at startup naming the variable (never
its value) and uses the default until the value is saved on the tab.

- After upgrading, re-enter on the **Settings** tab every console setting
  you had set in `.env`, in particular **provider exclusions** and **audit
  retention** (also session lengths, failover, provider cooldown, session
  release, housekeeping and the log level, if you set them there).
- **Audit retention** falls back to the default of **90 days** until it is
  saved. The audit prune deletes rotated security audit files older than the
  retention, and it runs at startup, so the first start after the upgrade
  uses 90 days. If you kept a longer retention, back up the audit files in
  the data directory before upgrading. Audit retention takes effect at the
  next restart after it is saved.
- **Provider exclusions** are now a plain comma-separated list of `0x`
  provider addresses, excluded for every model. A value saved in the
  earlier per-model form (with `=` or `;`) is not converted: it is ignored
  at startup (one warning, no exclusions apply) and refused on save, so
  re-enter it on the **Settings** tab, for example `0xProvider1,0xProvider2`.
- Secrets, router settings and the remaining wiring (`WEB_PUBLIC_URL`,
  `ACTIVE_MODELS_URL`, `GATEWAY_BIDS_URL`, `MASTER_KEY_ADMIN`) stay in
  `.env`.

---

## Upgrading from an earlier test build

Upgrades do not remove services that were dropped from the compose file
(`router-allowance-manager`, `router-user-init`) or their volumes
(`router_auth`, `manager_cred`, `allowance_status`, `allowance_ctl`). An
earlier test build may also have left an extra scoped router user in the
router's config.

- **SecretVM (production):** no shell (no SSH or serial access), so the
  only fix is a **fresh deploy**: a new VM with the release compose and the
  same six secrets. It clears the old services, volumes and router user.
- **Generic hosts and dev VMs:** do a fresh deploy, or clean up in place
  from the deploy directory (same `-f` / `--env-file` flags you deploy
  with; Docker prefixes volume names with the compose project name):

  ```bash
  docker compose up -d --remove-orphans
  docker volume rm <project>_router_auth <project>_manager_cred \
    <project>_allowance_status <project>_allowance_ctl
  ```

  Also delete `router.auth` from the Uplink data volume (`/data`), the
  router credential an earlier test build wrote there (the current build
  does not read it), and remove the extra router user, or do a fresh
  deploy if unsure; it clears both.

A fresh deploy starts with empty Uplink state. Generated (ephemeral) API
keys and the 2FA state (enrollment, recovery codes) are gone, and 2FA
comes back off. Master and Prompt keys come back from the same
`API_KEY_SEED`. Export ephemeral keys first and re-create or import them
afterwards, then turn 2FA on and enroll again.

---

## Portability

The portable unit is **published compose + env secrets + TLS terminator + public `:443`** (images from `ghcr.io`).

| Host | Pattern |
|------|---------|
| **SecretVM** | Release compose + encrypted `${}` secrets + platform certs |
| **Generic VPS** | Release compose + `.env` + Caddy |
| **Railway** | `deploy/railway/` scaffold with the release `docker-compose.railway.deployed.yml` digests + Railway Secrets; single-replica router |
| **Phala / dstack** | Same generic release assets + their secret injection |

Avoid baking RPC or wallet keys into the image — inject at runtime. Nothing requires SecretVM-specific APIs; only the Traefik cert mount is platform-specific (generic compose uses Caddy instead).

---

[← Apps](04-clients.md) · [Guide home](README.md) · [Next: Developers →](06-developers.md)
