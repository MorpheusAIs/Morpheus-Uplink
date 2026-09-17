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

---

[← GUI](03-gui.md) · [Guide home](README.md) · [Next: Updates →](05-updates.md)
