# 3. GUI basics

[← Bootstrap](02-bootstrap.md) · [Guide home](README.md) · [Next: Apps →](04-clients.md)

One topic at a time. You do not need every section on day one.

---

## Login

- URL: `https://<your-host>/gui/`
- User: `admin`
- Password: `ADMIN_PASSWORD`
- Two steps when 2FA is enrolled: password first, then the 6-digit code (or
  a recovery code). Wrong password and missing code get the same answer, so
  the page never reveals whether 2FA is enrolled; lockout and audit apply to
  both steps.
- Tabs: Status, API Keys, Probe, Sessions & Usage, Security (far right).

---

## API Keys

| Kind | Survives wipe / redeploy? | Role |
|------|---------------------------|------|
| **Master** | Yes (`API_KEY_SEED`) | Inference + admin API |
| **Prompt** | Yes (`API_KEY_SEED`) | Inference only — default for clients |
| **Ephemeral** | No — export/import | Named keys for apps/agents |

- Rotate built-ins by changing `API_KEY_SEED` and restarting.
- Ephemeral: create, red trash to delete, **Export all** / **Import list** after redeploy.

---

## Where is my MOR?

<a id="where-is-my-mor"></a>

![Where is my MOR? — Wallet → Active → On hold → To claim → Reclaim](assets/mor-buckets.jpg)

Opening a session moves stake from **Wallet → Active** (escrow for the full session duration). What happens next depends on **how the session ends** — not on how many tokens you prompted with.

| How the session ends | Liquid (back to Wallet on the close tx) | Time-locked (**On hold**) |
|----------------------|-----------------------------------------|---------------------------|
| **Close early** (GUI / housekeeping / router close before end) | Unused time portion of the stake | Used time portion |
| **Expire naturally** (session runs to its end time) | Nothing | **Entire** stake |

Then the clock:

1. **On hold** — daylocked until the next UTC midnight (`releaseAt`).
2. **To claim** — past `releaseAt`; ready to pull back.
3. **Reclaim** — `withdrawUserStakes` (Status button, or auto ~00:05 UTC) → **Wallet**.

| Bucket | Meaning |
|--------|---------|
| **Wallet** | Liquid MOR in the wallet |
| **Active** | Open session stake |
| **On hold** | Closed/expired stake still daylocked |
| **To claim** | Daylock done — reclaim to Wallet |

**Session stake (locks, not spends).** Stake is locked while a session runs
and returned to this wallet, held at most until the next UTC day. See
[02-bootstrap](02-bootstrap.md) for the full wording.

**Practical tip:** if you only need a short burst, close the session when you are done so unused duration returns immediately as liquid. If you leave sessions open until they expire, plan float for the **full** stake until the next reclaim cycle.

Deeper: [nodedocs — Where is my MOR?](https://nodedocs.mor.org/ai/where-is-my-mor) · [tech.mor.org/session.html](https://tech.mor.org/session.html)

---

## Sessions & Usage

- On boot Uplink lists the open sessions before opening any: still-valid
  ones are reused (one per model), expired and duplicate ones are closed so
  their stake is returned, and sessions for models not in the catalog are
  left to expire. The Status tab shows the last boot check and its counts.
  A graceful `docker stop` closes every open session on the wallet before
  Uplink exits, including sessions for models not in the catalog and, on a
  shared wallet, the other client's sessions; the stake is returned (a
  lock, not a loss). `CLOSE_SESSIONS_ON_EXIT=false` opts out. Use a
  dedicated wallet: the boot check and the stop act on every session of
  the wallet.
- **Usage** counters are local scratchpad — they reset on redeploy; funds do not.

---

## Probe

1. Open **Probe**.
2. Pick a model (fuzzy search) and an API key.
3. **Run test** or **Copy curl**.
4. Confirm a normal completion in **Response**.

---

## Logs & support

Router / container logs are **not** streamed into the GUI yet.

| Source | How |
|--------|-----|
| GUI Status | Health, version chips, MOR buckets, reclaim |
| Support JSON | Status → **Download support bundle** (no private keys) |
| Host logs | `docker logs <uplink\|proxy-router>` or your platform log panel |

---

[← Bootstrap](02-bootstrap.md) · [Guide home](README.md) · [Next: Apps →](04-clients.md)
