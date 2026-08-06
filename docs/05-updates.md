# 5. Updates & portability

[← Apps](04-clients.md) · [Guide home](README.md)

---

## New Uplink versions

1. GUI header may show a **new release** chip when [GitHub Releases](https://github.com/absgrafx/Morpheus-Uplink/releases) is ahead of the running build (notify only — you still apply the update on the host).
2. Download the new release compose (`docker-compose.secretvm.deployed.yml` or `docker-compose.generic.deployed.yml`). Keep the **same** secrets / `.env` (`API_KEY_SEED`, `WALLET_PRIVATE_KEY`).
3. Export ephemeral keys **before** wiping storage if volumes recreate.
4. After restart: Status health → Probe once → reclaim if **To claim** &gt; 0.

---

## Portability

The portable unit is **published compose + env secrets + TLS terminator + public `:443`** (images from `ghcr.io`).

| Host | Pattern |
|------|---------|
| **SecretVM** | Release compose + encrypted `${}` secrets + platform certs |
| **Generic VPS** | Release compose + `.env` + Caddy |
| **Phala / dstack** | Same generic release assets + their secret injection |

Avoid baking RPC or wallet keys into the image — inject at runtime. Nothing requires SecretVM-specific APIs; only the Traefik cert mount is platform-specific (generic compose uses Caddy instead).

---

[← Apps](04-clients.md) · [Guide home](README.md)
