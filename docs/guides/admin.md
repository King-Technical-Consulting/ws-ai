# Admin

The Admin page is for the owner. Other users do not see a link to it, and its routes answer `403 owner only` to them. A member who types `/admin` into the address bar sees "This page is for the workspace owner." (the same for the Jobs page). Everyone, including the owner, manages their own passkeys and API keys under **Settings**.

## Invites

ws is invite-only. Enter an email under **invites** and ws creates a single-use invite link, shown on the page and emailed if email is configured (without it, the email body, which contains the link, is written to the server log). New users get the `member` role. Invites expire after seven days, and inviting an address that already has an account is refused. The first account is created from the invite link the server prints at first boot, which is why `WS_OWNER_EMAIL` must be set.

## Endpoints and models

**Endpoints** are the models ws can route to. You can add them in three ways:

- **Declared in `config/endpoints.yaml`.** This is the place for local models and other providers; each declares its capabilities, context size and prices. See [Configuration](../CONFIGURATION.md).
- **In the Admin forms.** **Providers** lists every provider with whether this box has what it needs; add or edit one with a kind (`anthropic` or `openai_compat`), a base URL (or the name of an environment variable that holds it), the *name* of the environment variable that holds its API key, and optional headers. ws never stores or shows the key itself: put it in `.env` or Infisical. The model form adds a text model (context, prices, tool, vision and JSON flags) or an image or video model (engine, sizes, the kinds it can make, maximum images, price per image or per second). Removing a provider removes its endpoints. Only an OpenAI-compatible Images engine exists today.
- **From OpenRouter.** With an OpenRouter key configured, "Add models from OpenRouter" searches OpenRouter's live catalog and adds a model as an endpoint in one click. A catalog model's **Routes** lists the upstream providers serving it (Cerebras, Groq and others) with their prices and uptime. Adding one makes a separate endpoint pinned to that upstream, for example `openrouter/openai/gpt-oss-120b@cerebras`.

Endpoints can be disabled or removed here, but removing one that came from the file lasts only until the next boot. Two more things to know: the file's providers and endpoints are written to the database again on every boot, so an edit made here to a row that came from the file is overwritten (only the enabled flag survives), and the enable toggle may not work yet for an endpoint whose id contains a slash (a known issue). To change a file-backed endpoint for good, change the file.

## Rented GPUs

If a RunPod key and a rental key are set (see [Configuration](../CONFIGURATION.md)), the **Rented GPUs** section lists the machine templates, each with a **Start** button showing its hourly price. A started machine goes through *provisioning* and *warming*, and when its model answers it appears as a model endpoint you can pick or route to. Open machines show their status, hours and estimated cost, with **Stop** and **stop all**. A machine stops on its own after it has been idle (default 20 minutes), after its maximum hours, when the daily cap is reached, or when the kill switch is on; each hour is billed to the usage ledger. Only RunPod is supported, and a real start has not been tried yet.

## Routing policies

A **routing policy** says which endpoints a kind of request prefers. The page has a YAML editor; each policy has a priority, and the lower number wins. A policy that comes from a file in `config/policies/` is re-applied at boot, including its priority and its enabled flag, so an edit made here to it is overwritten. The schema is in [Configuration](../CONFIGURATION.md).

## Budgets

A **budget** limits spend for everyone (`global`), one user, or one API key, over a rolling day (24 hours), week (7 days) or month (30 days), or in total. When the limit is reached it either blocks the call or downgrades it to local endpoints only. Calls that go through the Anthropic passthrough count too, because they are recorded in the same ledger.

## Usage

The usage section shows the last 30 days: the total cost and a per-endpoint table of calls, tokens, cost and average latency. Spend by user and the most recent calls are available from the API (`/api/admin/usage`). All of it comes from the ledger described in [Chat and models](chat.md).

## GitHub

The GitHub section shows whether a GitHub App is configured, its installations, and an install link, which [code projects](code-projects.md) use for pushes and pull requests.

*Checked against the code at master `5e9f902`.*
