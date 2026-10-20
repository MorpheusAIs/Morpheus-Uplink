# 4. Apps & agents

[← GUI](03-gui.md) · [Guide home](README.md) · [Next: Updates →](05-updates.md)

Uplink speaks **OpenAI-compatible** HTTP. Almost every OpenAI client works with three fields.

| Setting | Value |
|---------|--------|
| Base URL | `https://<your-host>/v1` |
| Auth | `Authorization: Bearer <Prompt or ephemeral sk-…>` |
| Model | Exact name from Probe / `GET /v1/models` / [`gateway_models.json`](https://active.mor.org/gateway_models.json) (not `active_models.json` / ALL) |

Prefer **Prompt** or ephemeral keys in clients; keep **Master** off machines you do not trust.

**Trust model:** every Prompt/ephemeral key can open sessions and lock MOR from
this gateway’s wallet. Do not paste keys into untrusted agents or shared
machines. See [DISCLAIMER.md](../DISCLAIMER.md).

---

<details>
<summary><strong>curl</strong></summary>

```bash
curl -s https://YOUR_HOST/v1/chat/completions \
  -H "Authorization: Bearer sk-prompt.…" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "REPLACE_WITH_PROBE_NAME",
  "messages": [
    {"role": "user", "content": "In one sentence, what is Morpheus?"}
  ],
  "max_tokens": 128
}'
```

</details>

<details>
<summary><strong>Python (OpenAI SDK)</strong></summary>

```python
from openai import OpenAI
client = OpenAI(base_url="https://YOUR_HOST/v1", api_key="sk-prompt.…")
print(client.chat.completions.create(
    model="REPLACE_WITH_PROBE_NAME",
    messages=[{"role": "user", "content": "hi"}],
).choices[0].message.content)
```

</details>

<details>
<summary><strong>Desktop / agent harnesses</strong> (Cursor, Continue, custom agents)</summary>

1. Create an **ephemeral** key (or use Prompt) in the GUI; copy it.
2. In the app: custom OpenAI base URL → `https://YOUR_HOST/v1`, paste the key.
3. Model string = **exact** catalog name from Probe / `/v1/models` / `gateway_models.json` (wrong name → resolve fails; never invent a flashy name).
4. After app updates, re-check base URL + key + model — Uplink’s contract stays OpenAI-shaped.

</details>

<details>
<summary><strong>Errors and provider failover</strong></summary>

- When the model's provider fails (for example its backend is out of
  credit, rate limited or down), Uplink moves to another provider for that
  model and answers the same request; the client sees a normal reply.
  This happens at most once per request, and only when another provider
  serves the model.
- Otherwise the error comes back with its real status (for example `400`
  for a bad parameter, `402` or `429` from the provider when no other
  provider is available), in the OpenAI error shape (`error.message`,
  `error.type`, `error.code`). `502` means Uplink could not reach its
  router.
- Streaming: if the provider fails after part of the answer was sent, the
  stream ends with one `data: {"error":{...,"type":"provider_error"}}`
  event. Send the request again; the next one goes to another provider.
- See [Provider failover](06-developers.md#provider-failover).

</details>


---

## Usage (`GET /v1/usage`)

Poll per-key usage without the admin GUI. Same Bearer auth as other `/v1/*`
routes (Prompt, Master, or ephemeral `sk-…`).

```bash
curl -sS -H "Authorization: Bearer sk-prompt.…"   https://YOUR_HOST/v1/usage
```

Response: `{"usage":[…]}` — rows with `day`, `keyId`, `model`, `requests`,
`promptTokens`, `completionTokens`, and optional `sessionSeconds`.

**WARN — scoping:** Prompt and generated keys see **only their own** rows
(`keyId` matches the caller). Master Bearer returns the **full instance**
(same table as `GET /admin/usage`). `keyId` is a public id (`master`,
`prompt`, or generated id) — never an `sk-` secret. Basic auth does **not**
unlock `/v1/usage`.

---

[← GUI](03-gui.md) · [Guide home](README.md) · [Next: Updates →](05-updates.md)
