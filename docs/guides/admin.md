# Admin

The Admin page is for the owner. Other users do not see a link to it, and its routes answer `403 owner only` to them. A member who types `/admin` into the address bar sees "This page is for the workspace owner." (the same for the Jobs page). Everyone, including the owner, manages their own passkeys and API keys under **Settings**.

## Invites

ws is invite-only. Enter an email under **invites** and ws creates a single-use invite link, shown on the page and emailed if email is configured (without it, the email body, which contains the link, is written to the server log). New users get the `member` role. Invites expire after seven days, and inviting an address that already has an account is refused: the page shows the server's reason under the form and keeps what you typed so you can correct it. A pending invite has **Resend** (a fresh link, shown and mailed, the old one withdrawn) and **Revoke** (the link stops working) next to it; an accepted one has neither. The first account is created from the invite link the server prints at first boot, which is why `WS_OWNER_EMAIL` must be set.

## Endpoints and models

**Endpoints** are the models ws can route to. You can add them in three ways:

- **Declared in `config/endpoints.yaml`.** This is the place for local models and other providers; each declares its capabilities, context size and prices. See [Configuration](../CONFIGURATION.md).
- **In the Admin forms.** **Providers** lists every provider with whether this box has what it needs; add or edit one with a kind (`anthropic` or `openai_compat`), a base URL (or the name of an environment variable that holds it), the *name* of the environment variable that holds its API key, and optional headers. ws never stores or shows the key itself: put it in `.env` or Infisical. The model form adds a text model (context, prices, tool, vision and JSON flags) or an image or video model (engine, sizes, the kinds it can make, maximum images, price per image or per second). Removing a provider removes its endpoints. Engines: OpenAI Images and Sora, fal.ai and ComfyUI; video models also take a list of allowed lengths. No live run of any engine is on record; all were tested against fake servers, and the video, Wan and ComfyUI endpoints ship disabled until you try them. When no image endpoint is enabled (the server has no `COMFYUI_URL` or media provider key, or every endpoint is switched off here), the Media page says so above the form and keeps Generate off, and a job that is sent anyway fails with the same words: "No image endpoint is configured; the owner sets COMFYUI_URL or a media provider key, or enables one under Admin, endpoints." When the endpoint is enabled but its server does not answer (health "down", for example a ComfyUI that is not reachable at `COMFYUI_URL`), the page and a failed job name that endpoint instead ("The image endpoint comfyui/sdxl is down; the owner checks the server it points at (COMFYUI_URL) and Admin, endpoints."; a failed job also carries the health error), rather than blaming a missing API key.
- **From OpenRouter.** With an OpenRouter key configured, "Add models from OpenRouter" searches OpenRouter's live catalog and adds a model as an endpoint in one click. A catalog model's **Routes** lists the upstream providers serving it (Cerebras, Groq and others) with their prices and uptime. Adding one makes a separate endpoint pinned to that upstream, for example `openrouter/openai/gpt-oss-120b@cerebras`.

Endpoints can be disabled or removed here, but removing one that came from the file lasts only until the next boot. Two more things to know: the file's providers and endpoints are written to the database again on every boot, so an edit made here to a row that came from the file is overwritten (only the enabled flag survives), (the enable toggle works for ids that contain a slash too; before #136 it answered 405 for them). To change a file-backed endpoint for good, change the file.

## Rented GPUs

If a RunPod key and a rental key are set (see [Configuration](../CONFIGURATION.md)), the **Rented GPUs** section lists the machine templates, each with a **Start** button showing its hourly price. A started machine goes through *provisioning* and *warming*, and when its model answers it appears as a model endpoint you can pick or route to. Open machines show their status, hours and estimated cost, with **Stop** and **stop all**. A machine stops on its own after it has been idle (default 20 minutes), after its maximum hours, when the daily cap is reached, or when the kill switch is on; each hour is billed to the usage ledger. Only RunPod is supported, and a real start has not been tried yet.

## Training

The owner-only **Training** page is a first cut of the fine-tuning flow. **Datasets** are exported from the conversations of users who opted in (filters: mode, model, a rating floor, a start date, a held-out share), and can be downloaded as JSONL. **Fine-tune jobs** run a trainer container on the worker (`WS_FINETUNE_IMAGE`) or on a rented trainer machine (`WS_FINETUNE_TEMPLATE`) and store the adapter, registering a disabled `lora/<name>` endpoint next to its base. **Adapters** can be evaluated (a judge model scores the adapter and its base on the held-out examples) and promoted, which enables the endpoint; promote refuses an adapter that scored below its base unless you force it. You load the downloaded adapter into your serving engine yourself. Set `WS_FINETUNE_IMAGE` or `WS_FINETUNE_TEMPLATE` on both the web process and the worker; nothing here has run against a real trainer. See the [API reference](../API.md) for the details.

## Routing policies

A **routing policy** says which endpoints a kind of request prefers. The page has a YAML editor; each policy has a priority, and the lower number wins. A policy that comes from a file in `config/policies/` is re-applied at boot, including its priority and its enabled flag, so an edit made here to it is overwritten. The schema is in [Configuration](../CONFIGURATION.md).

## Budgets

A **budget** limits spend for everyone (`global`), one user, one API key, or one agent (the agent's page sets its monthly budget inline), over a rolling day (24 hours), week (7 days) or month (30 days), or in total. When the limit is reached it either blocks the call or downgrades it to local endpoints only. Calls that go through the Anthropic passthrough count too, because they are recorded in the same ledger.

Members reach hosted models only with their own API key (see [Secrets and MCP servers](secrets-and-mcp.md#your-own-provider-keys)). The **people** section in Admin has a "may use shared keys" box per person; ticking it lets that member use this server's shared provider keys too, and their spend on them counts against their budget. The owner always has the shared keys. The same row has a **Disable** button: a disabled person is signed out everywhere at once, their API keys stop working and no sign-in link is sent to them; **Enable** restores all of it without a new invite. The owner cannot be disabled.

The same row has two more controls per person. **Keys** opens that person's API keys (name, prefix, policy and scopes, never the key itself) with a **Revoke** button on each; a revoked key is refused by `/v1` and `/mcp` from then on, the same as when the person revokes it under Settings. **Sign out everywhere** ends every browser session that person has, so their next request is refused and they are sent to the login page. Their passkeys, API keys and account stay as they are, and they can sign in again at once with a passkey or a fresh magic link. The owner's own row is not listed; the owner manages their own keys under Settings (the API routes, `DELETE /api/admin/users/{id}/sessions`, `GET /api/admin/users/{id}/keys` and `DELETE /api/admin/users/{id}/keys/{keyId}`, work on the owner's own account too).

## Usage

The usage section shows the last 30 days: the total cost and a per-endpoint table of calls, tokens, cost and average latency. Spend by user and the most recent calls are available from the API (`/api/admin/usage`). All of it comes from the ledger described in [Chat and models](chat.md).

## GitHub

The GitHub section shows whether a GitHub App is configured, its installations, and an install link, which [code projects](code-projects.md) use for pushes and pull requests.

*Checked against the code at master `5e9f902`.*
