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

**MOR allowance (locks, not spends).** The Status tab shows the router
allowance: the most MOR the gateway may lock as session stake. Stake is
returned to this wallet, held at most until the next UTC day. In balance
mode (empty `UPLINK_MOR_ALLOWANCE_WEI`) the allowance follows the live
wallet balance; new deposits are included within ~2 min, and **Resync
allowance now** asks for an immediate update. The button is shown only when
the allowance manager is running in balance mode; otherwise the tab says
"auto-resync unavailable". With a cap the allowance is fixed. See
[02-bootstrap](02-bootstrap.md) for the full wording and the cap-mode
recommendation for high-value wallets.

**Practical tip:** if you only need a short burst, close the session when you are done so unused duration returns immediately as liquid. If you leave sessions open until they expire, plan float for the **full** stake until the next reclaim cycle.

Deeper: [nodedocs — Where is my MOR?](https://nodedocs.mor.org/ai/where-is-my-mor) · [tech.mor.org/session.html](https://tech.mor.org/session.html)

---

## Sessions & Usage

- Open sessions rehydrate from chain after restart (still-valid ones are reused).
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
