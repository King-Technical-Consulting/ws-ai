# HTTP API

The routes `ws serve` exposes, grouped by who calls them. Verified against `internal/httpx/server.go`, `internal/externalapi/`, `internal/artifacts/origin.go`, `internal/httpx/preview.go`, `internal/sandbox/internalapi.go` and the handlers they mount. Last verified at commit `5e9f902`.

This is a route and behavior map, not a full schema. Request fields are listed where a handler decodes a body; response bodies are the JSON rows from `internal/store` unless noted. The `/v1` surface is under active change for milestone M5 (passthrough, billing router), so treat that section as the contract today and check the PLAN for where it is heading.

## Conventions

- JSON in, JSON out, except streaming endpoints. Request bodies are capped at 32 MB on `/api` and 64 MB on `/v1`.
- Errors from `/api` are `{"error": "message"}` with a status code. Errors from `/v1` follow the protocol of the endpoint called, including `401` for a missing or invalid key: the Anthropic shape (`{"type":"error","error":{"type":"authentication_error",...}}`) for `/v1/messages*` and the OpenAI shape (`invalid_api_key`) otherwise, both with `x-should-retry: false`. A key without the scope a route needs gets `403`: on `/v1/messages*` `{"type":"error","error":{"type":"permission_error","message":"this API key has no chat scope"}}`, on other `/v1` routes `{"error":{"message":"this API key has no chat scope","type":"invalid_request_error","code":"insufficient_scope"}}`, both with `x-should-retry: false`.
- Request timeout is 15 minutes, to allow long streams.
- Responses from the app origin carry `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: strict-origin-when-cross-origin`. Preview hosts, the artifact origin (its own headers: no `X-Frame-Options`, `Referrer-Policy: no-referrer`) and the worker API don't get these.
- IDs are UUIDs unless noted. A resource you can't access returns `404`, not `403`.
- Unknown `/api` paths return a plain-text `404` and wrong methods an empty `405`. With the web app embedded, unknown `/v1` paths and wrong methods on `/v1` routes fall through to the SPA `index.html` with `200`, not a JSON error.

## Authentication

Three credentials are accepted on authenticated routes, checked in this order. The first one present is the only one tried: a non-`ws_` bearer token gets `401` even with a valid session cookie.

| Credential | Form | Used for |
|---|---|---|
| API key | `Authorization: Bearer ws_...` | `/v1` (needs the `chat` scope) and `/mcp` (needs the `mcp` scope). A key reaches no other route |
| API key | `x-api-key: ws_...` | Anthropic-style clients |
| Session | `ws_session` cookie (HttpOnly, SameSite=Lax, Secure outside `WS_ENV=dev`) | The browser app |

API keys must start with `ws_`. `/v1` accepts the session cookie too. Keys are created under `/api/keys`; the raw key is returned once. Sign-in is by passkey or magic link; there are no passwords.

CSRF: state-changing requests (anything but GET and HEAD) authenticated by session cookie are rejected with `403 cross-site request blocked` when `Sec-Fetch-Site` is present and not `same-origin` or `none`. API-key requests skip this check.

Routes marked **owner** additionally require the `owner` role (`403 owner only`).

## Health

| Route | Auth | Notes |
|---|---|---|
| `GET /healthz` | none | `{"ok":"true"}` |
| `HEAD /api/hello`, `GET /api/hello` | none | `200 {"ok":"true"}`. The probe Claude Code makes before it uses a gateway |
| `GET /login/magic/{token}` | none | Consumes a magic-link token, sets the session cookie, and answers `303` to `/`, or to `/login?error=magic` (bad token) or `/login?error=session`. Meant for the browser |

The binary also answers `ws health` for container healthchecks.

## External API (`/v1`)

For Claude Code, Cursor, opencode, aider and any OpenAI- or Anthropic-compatible client. Authenticated with an API key. Compaction is off for these calls: the client owns its context. Every call is routed, budget-checked and written to the usage ledger under the key (except `count_tokens` on an Anthropic endpoint, which is proxied and records nothing). If the request carries an `x-claude-code-session-id` header (up to 128 characters), it is stored on the ledger row as `session_id`, so a coding session's spend can be grouped.

| Route | Protocol | Notes |
|---|---|---|
| `GET /v1/models` | OpenAI, or Anthropic | Lists `auto`, the policy aliases, then enabled non-embedding endpoints. In the Anthropic shape (`{data:[{type,id,display_name,created_at}], has_more, first_id, last_id}`) when the request carries an `anthropic-version` header, which is how Claude Code's model discovery asks; the OpenAI shape otherwise |
| `GET /v1/models/{id}` | OpenAI, or Anthropic | Echoes the id with `resolved` set to what the name routes to (`{id, object, created, owned_by:"ws", resolved}`; with `anthropic-version`, `{type:"model", id, display_name, created_at, resolved}`). Never `404` |
| `POST /v1/chat/completions` | OpenAI Chat Completions | Streaming (`stream: true`) with usage, tool calls, images. Usage always arrives in a final chunk with `choices: []`. Non-stream responses add `ws: {endpoint}` |
| `POST /v1/messages` | Anthropic Messages | Streaming, tool use, images. Two paths behind one route; see below |
| `POST /v1/messages/count_tokens` | Anthropic | Proxied to Anthropic for a native Anthropic endpoint (an exact count, no ledger row); otherwise a rough local estimate (characters / 4). Returns `{input_tokens}` |

Status codes on the chat routes: `400` bad body or no messages, `402` a blocking budget is spent, `503` no route, `502` any other gateway error. On `/v1/messages*` these come back in the Anthropic shape with `x-should-retry` set: `402` is `rate_limit_error` with `false`, `503` is `overloaded_error` with `true`, `502` is `api_error`.

Model names resolve in this order: empty or `default` uses the key's default policy alias (else `auto`); an exact endpoint id; a policy alias (`auto`, `cheap`, `best`, `code`, `local`, plus any you define); a provider model name matching an endpoint's `model` or the tail of its id; otherwise the key's default alias. The name `code` also sets the task class to `code`.

```bash
curl https://app.example.com/v1/chat/completions \
  -H "Authorization: Bearer ws_..." -H "Content-Type: application/json" \
  -d '{"model":"cheap","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

### Anthropic passthrough and translate

`POST /v1/messages` routes first (policy and budgets, including the downgrade to local endpoints when a budget is spent), then branches on where it landed.

- **Native Anthropic endpoint: passthrough.** The raw body is proxied byte for byte, with only `model` rewritten to the endpoint's model name. `anthropic-version` (default `2023-06-01` when absent), `anthropic-beta`, `accept` and `user-agent` are forwarded, and the query string (for example `?beta=true`) is kept. The client's `ws_` credential is not forwarded; the provider's key is sent instead. The response is relayed unchanged: status, headers (`retry-after`, `x-should-retry`, `request-id`, rate-limit headers), SSE pings and error bodies; `Content-Encoding`, `Set-Cookie` and hop-by-hop headers are dropped. Retryable upstream statuses (`408`, `409`, `429`, `5xx`) fail over to the next Anthropic candidate before anything has been sent; the last candidate's response is relayed as is. The stream is read as it passes to record usage, cost (from the endpoint's pricing) and the stop reason in the ledger.
- **Any other endpoint: translate.** The request goes through the canonical types to an OpenAI-compatible endpoint; unknown betas and block types are dropped, never an error.

Subscription (OAuth) credentials are never accepted or forwarded by ws; that option was removed from the plan.

### MCP server (`/mcp`)

ws serves itself as an MCP server so Claude Code and other MCP clients can use it. `claude mcp add --transport http ws https://<host>/mcp --header "Authorization: Bearer ws_..."` (Settings prints this line for a key minted with MCP access). The key must carry the `mcp` scope: a key without it gets `403 {"error":"this API key has no mcp scope; mint one with MCP access in Settings"}`, and keys minted before scopes were checked carry only `chat`, so `/mcp` needs a new key. It is Streamable HTTP in stateless mode: one POST per request, no session id. It needs a ws API key (`Authorization: Bearer ws_...`); a browser session cookie gets `401`, so a web page cannot drive it. `WS_MCP_SERVER=false` removes the route. Every tool acts as the key's user and checks project access first (a failure reads "no such project", "no such conversation" or "no such run"); errors come back as tool errors, not protocol errors.

| Tool | Arguments | Does |
|---|---|---|
| `ws_projects` | none | Lists your projects (id, name, kind, repo URL, updated) |
| `ws_conversations` | `project_id?`, `limit?` (default 20, max 100) | Lists a project's conversations, or your most recent across projects |
| `ws_conversation` | `conversation_id`, `last?` | The conversation with the latest run's status and the messages as role and text; tool calls are summarized and clipped, reasoning is left out |
| `ws_models` | none | Enabled models (id, display name, provider, local, health) and the policy aliases |
| `ws_ask` | `prompt`, `system?`, `model?`, `max_tokens?` (default 4096), `temperature?` | One gateway completion with no tools or memory, routed, failed over and written to the ledger under the key; 5 minute timeout |
| `ws_run` | `project_id`, `prompt`, `model?`, `title?` | Starts a conversation with the prompt and queues an agent run on the worker; returns the conversation and run ids |
| `ws_run_status` | `run_id` | Status, steps, cost, error, times and the last assistant text |
| `ws_spawn_job` | `prompt`, `lane?`, `cwd?`, `target?`, `model?`, `dry_run?`, `conversation_id?` | The task router's `spawn_job`: returns the decision and a handle, never the output. The non-subscription lanes run inside a conversation, so they need `conversation_id`. Subscription launches stay owner-only and `WS_CC_DISPATCH` still governs real launches (not traced through the MCP path: that the owner check behaves the same here) |

## Auth routes (`/api/auth`)

| Route | Auth | Purpose |
|---|---|---|
| `GET /api/auth/config` | none | `{passkeys, magic_links}` flags for the login page. `magic_links` is always `true` |
| `POST /api/auth/invite/peek` | none | Body `{token}`. Returns `{email}`, or `404` |
| `POST /api/auth/invite/accept` | none | Body `{token, display_name}`. Creates the account, sets the session cookie, returns `{id, email, display_name, role}`; `400` for an invalid or expired invite |
| `POST /api/auth/magic/send` | none | Body `{email}`. `400` if missing or malformed, otherwise always `{"status":"sent-if-known"}` |
| `POST /api/auth/passkey/login/begin`, `.../finish` | none | WebAuthn sign-in. `begin` returns `{options, ceremony_id}` (`503` when passkeys are unavailable). `finish` needs `?ceremony=<id>`, sets the cookie, and returns `401` on failure |
| `POST /api/auth/passkey/register/begin`, `.../finish` | auth | Add a passkey. `begin` as above; `finish` needs `?ceremony=<id>` and `?name=` |
| `GET /api/auth/passkeys` | auth | List the caller's passkeys |
| `DELETE /api/auth/passkeys/{id}` | auth | Remove one. `200 {"status":"ok"}` even for an unknown id; `400` for a malformed one |
| `POST /api/auth/logout` | auth | Revoke the session and clear the cookie. With an API key it only clears the cookie |

"auth" here is any accepted credential, API keys included, not only a session.

## App API (`/api`)

All routes below need authentication.

### Identity and models

| Route | Purpose |
|---|---|
| `GET /api/me` | `{id, email, display_name, role, via_api_key}` |
| `GET /api/models` | `{models, aliases}` for the model picker. `models` is `[{id, display_name, provider, local, capabilities, pricing, health}]` for enabled non-embedding, non-media endpoints; `media` lists the enabled image and video endpoints (`id`, `display_name`, `provider`, `local`, `engine`, `image`, `image_edit`, `video`, `sizes`, `max_images`, `max_seconds`, `health`, and `per_image` and `per_second` for hosted ones); `aliases` maps alias name to endpoint ids |
| `GET /api/keys` | List the caller's API keys as `{id, name, prefix, scopes, default_policy, created_at, last_used_at}` (never the hash) |
| `POST /api/keys` | Body `{name, scopes?, default_policy?}`; `scopes` is any of `chat` and `mcp` (`400 unknown scope "x" (chat, mcp)` otherwise) and defaults to `["chat"]`, `default_policy` to `auto`. `chat` is what `/v1` needs and `mcp` what `/mcp` needs; a browser session has every scope. Returns `201 {key, record}` with `record` in the same shape as the list; `key` is shown only once |
| `DELETE /api/keys/{id}` | Revoke. `200 {"status":"ok"}` even for an unknown id; `400` for a malformed one |

### Projects and conversations

| Route | Purpose |
|---|---|
| `GET /api/projects`, `POST /api/projects` | Body `{name, kind, settings?, repo_url?, default_branch?}`. `kind` defaults to `chat`; `code` makes a coding project with a sandbox. `repo_url` must be an `https://` clone URL |
| `GET /api/projects/{id}` | One project |
| `GET /api/projects/{id}/conversations` | Conversations in a project. `?limit` (1-200, default 50) and `?offset` |
| `POST /api/projects/{id}/conversations` | Body `{title?, mode?, model?}`. `mode` defaults to the project kind (`chat` for an images project); `model` defaults to `auto`. Returns `201` with the row |
| `GET /api/conversations/recent` | The 50 most recent conversations across projects (fixed) |
| `GET /api/conversations/{id}` | `{conversation, messages, run_status}`. Tool parts awaiting approval are marked with their approval id |
| `PATCH /api/conversations/{id}` | Body `{title?, model?, settings?}`. `settings.tool_policies` overrides tool policies, for example `{"tool_policies":{"web_fetch":"ask"}}` |
| `DELETE /api/conversations/{id}` | Archive (not a hard delete). `PATCH` and `DELETE` answer `200 {"status":"ok"}` |
| `GET /api/conversations/{id}/artifacts` | Artifacts created in the conversation |
| `GET /api/artifacts/{id}`, `GET /api/artifacts/{id}/versions/{v}` | An artifact, current or by version (`?version=N` also works). Returns `{artifact, versions, version, version_id, url, content, kind, title}`; `url` is the signed viewer URL on the artifact origin |

### Chat and runs

`POST /api/conversations/{id}/chat` runs one turn and streams the response in the [Vercel AI SDK UI Message Stream](https://ai-sdk.dev) format (header `x-vercel-ai-ui-message-stream: v1`, server-sent events). Body:

```json
{
  "messages": [{"role": "user", "parts": [{"type": "text", "text": "hello"}]}],
  "model": "auto",
  "reasoning": false
}
```

The body also carries `id` and `trigger` (sent by the AI SDK; ignored). Two shapes are accepted. If the last message is from the `user`, a new run starts. If it is the `assistant` message echoed back with tool approval decisions in its parts, the paused run resumes. Any other shape is `400` (`no messages`, `last message must be from the user`, `assistant message carries no approval decisions`). Errors during a resumed run are sent in the stream, not as a status code.

| Route | Purpose |
|---|---|
| `GET /api/conversations/{id}/runs` | The 20 most recent runs for the conversation, newest first |
| `GET /api/runs/{id}` | `{run, steps, approvals}` for one run |
| `POST /api/approvals/{id}` | Body `{approved: bool, note?}`. Decides a pending approval and returns the updated row; `409` if already decided. Once no approvals are pending the run is re-queued for the worker |

### Sandboxes (code projects)

These proxy to the worker's internal API; the browser never reaches the worker or the Docker socket. Every route here returns `400` for a project that isn't a code project and `503` when no worker is configured. A worker `403` (shared secret mismatch) is returned as `502`. Apart from `sandbox/stop`, each route ensures the sandbox is running first, which can take up to 10 minutes on first use.

| Route | Purpose |
|---|---|
| `GET /api/projects/{id}/sandbox` | Creates or starts the sandbox, returns `{id, status, runtime, container_id, repo_url, preview_pattern?}` |
| `POST /api/projects/{id}/sandbox/stop` | Stop it (the volume stays). `204` |
| `GET /api/projects/{id}/fs/tree?path=` | One directory, not recursive: `{path, entries: [{name, type, size}]}` |
| `GET /api/projects/{id}/fs/file?path=` | Read a file: `{path, binary, size, content}`. Files over 8 MiB fail (the worker answers `404` with the reason) |
| `PUT /api/projects/{id}/fs/file` | Body `{path, content}`. Write a file. `204`; `413` over 8 MiB |
| `GET /api/projects/{id}/pty` | WebSocket carrying a `bash` session; query `cols`, `rows`, `cwd` |
| `GET /api/projects/{id}/preview/{port}` | `302` to the preview host's `/__ws/auth`. `400` bad port, `503` if previews aren't configured |

### Media (images)

Generation is a durable job (`media_jobs`, River queue `media`), routed through the gateway so policies and budgets apply and one ledger row is written per job. All routes need a session and access to the project.

| Route | Notes |
|---|---|
| `GET /api/projects/{id}/media` | `{jobs}`, newest first. `?limit=` (default 60, at most 200), `?offset=`, `?kind=` |
| `POST /api/projects/{id}/media` | Body `{kind?, prompt, size?, n?, quality?, model?, conversation_id?}`. `kind` defaults to image; video, edit and upscale answer `400` not supported yet. Returns `201` and the job, with `inputs.estimate_usd` (the price shown before generating). `400` for an invalid request (no prompt, size or count the endpoint does not accept, no media endpoint for the selector, conversation not in the project), `402` when a blocking budget is spent, `503` when media is not configured |
| `GET /api/media/{id}` | One job: `status` (`queued`, `running`, `done`, `failed`, `cancelled`), `progress`, `cost_usd`, `error`, and `outputs` (`[{id, url, mime, bytes, filename, width, height}]`) |
| `DELETE /api/media/{id}` | Cancels a queued job (`{status:"cancelled"}`), removes a finished one (`{status:"deleted"}`; its attachments are kept), `409` while it runs (a started generation cannot be stopped) |
| `GET /api/attachments/{id}` | Streams an attachment to its owner, or to anyone who can see a media job that produced it. Never served as HTML (`text/html` becomes `application/octet-stream`), with `Content-Security-Policy: sandbox; default-src 'none'`, `Content-Disposition: inline` and an immutable one-year private cache. `503` when attachments are not configured |

Jobs found running for more than 15 minutes (a worker died) are marked abandoned. The agent tool `generate_image` (registered, and allowed in chat, only when an image endpoint exists on the box) creates a job in the conversation's project, waits up to 4 minutes for the worker, and returns the attachment URLs so the chat renders them.

### Agents and runs (PLAN M7)

An agent is a standing goal with its own tools, triggers and run history; each run is a conversation in the agent's project (`mode='agent'`). Every route needs a session and answers `404 not found` unless the agent belongs to the caller. An agent has at most one open run (`queued`, `running` or a pause): starting another gives `409`.

| Route | Notes |
|---|---|
| `GET /api/agents` | The caller's agents, each with `project_id` |
| `POST /api/agents` | Body `{name, goal, system_prompt, project_id, model: {selector, task_class, reasoning}, tool_allowlist[], tool_policies{tool: auto, ask or deny}, max_steps, enabled}`. `name` and `project_id` are required and the project must be accessible; `max_steps` defaults to 50 and is at most 500 (`400`). `enabled` is ignored on create. `201` |
| `GET /api/agents/{id}` | `{agent, triggers, runs (30), pending_approvals, spend}`; `spend` is `{since, usd, budgets}` for the UTC month, with the agent-scoped budgets |
| `PUT /api/agents/{id}` | Same body as create; `enabled` keeps its value when omitted |
| `DELETE /api/agents/{id}` | `200 {"status":"ok"}` |
| `POST /api/agents/{id}/enabled` | Body `{enabled}` |
| `POST /api/agents/{id}/run` | Body `{input?}`. Starts a run now (the goal is the input when empty). `201` with the run; `409` if the agent is disabled or already has an open run; `503` when agents are not configured |
| `POST /api/agents/{id}/triggers` | Body `{kind, name, spec}`. `kind` is `cron` (`spec.expr`, a five-field expression or `@hourly` style, and optional `spec.input`), `webhook` or `manual`; `repo_push` is rejected. A webhook answers with its `secret` and `url` once; only the hash is stored |
| `DELETE /api/agents/{id}/triggers/{tid}`, `POST .../{tid}/enabled` | Remove a trigger; set `{enabled}` |
| `POST /api/runs/{id}/cancel` | Cancels an agent's run (the run stops between steps). `404` for a run with no agent |
| `POST /api/runs/{id}/steer` | Body `{text}`. Posts the text into the run's conversation and starts a new run there; `409` while the run is still open |
| `POST /hooks/agents/{id}/{secret}` | The webhook. No session: the secret in the path is the credential. Any content type up to 64 KB (`413` over), `202 {run_id, status}`; the payload becomes the run's input. `404` for a wrong secret, a non-webhook or a disabled trigger; `409` when the agent is busy or disabled |

Cron triggers fire from the `agent.tick` job every minute. A firing is skipped, with `last_error` set on the trigger, when the agent already has an open run. The agent page also calls `GET /api/agents/{id}/memories` and `DELETE` on it and on `.../memories/{mid}`; those routes are not registered on the server yet, so the memory section gets `404`.

### Admin (owner only)

| Route | Purpose |
|---|---|
| `GET`, `POST /api/admin/invites` | List invites; create with `{email, role}` (`role` defaults to `member`), returns `201 {link}`. `400` on an empty or already-registered email |
| `GET /api/admin/users` | Users |
| `GET /api/admin/usage?days=` | Ledger summary: `{since, total_usd, by_endpoint, by_user, recent}`. `days` defaults to 30; `recent` is capped at 100 rows |
| `GET /api/admin/github` | `{configured, fallback_token, slug?, install_url?, installations?, error?}`: the GitHub App's installations and install link |
| `GET /api/admin/endpoints` | `{providers, endpoints, policies, all_providers, engines}` as loaded. `all_providers` lists every stored provider, configured or not (`{id, kind, name, base_url, base_url_env, api_key_env, headers, configured, reason?}`; `reason` says which environment variable is missing or that the registry has not reloaded yet); `engines` lists the media engines this build has (`{id, name, image, image_edit, video, image_to_video, sizes, provider_kind, note}`; only `openai_images` today) |
| `PUT /api/admin/providers` | Create or update a provider and reload the registry. Body `{id, kind, name?, base_url, base_url_env?, api_key_env?, headers?}`: `id` matches `^[a-z0-9][a-z0-9._-]{0,63}$`, `kind` is `anthropic` or `openai_compat`, `base_url` starts with `http://` or `https://` unless `base_url_env` (an environment variable name, which wins and is stored as `env:NAME`) is set, `api_key_env` is a variable name or empty. Only variable names are stored, never a key. `400` with a message for each violated rule; returns the provider view. Providers that also appear in `config/endpoints.yaml` are overwritten by the file on the next boot |
| `DELETE /api/admin/providers/{id}` | Delete a provider (its endpoints go with it) and reload the registry. `200 {"status":"ok"}`; does not check that the id exists |
| `PUT /api/admin/endpoints` | Create or update an endpoint and reload the registry. Requires `provider_id` (a configured provider) and `model_name`; with `capabilities.media` set the engine must be one of `engines` (`400 unknown media engine`), the endpoint must make images or video (`400`), `sizes` are trimmed and `max_images` is clamped to 4; `id` defaults to `provider_id/model_name` and must not contain whitespace; `enabled` defaults to `true` |
| `DELETE /api/admin/endpoints/*` | Delete an endpoint by id (ids contain slashes). `200 {"status":"ok"}` |
| `POST /api/admin/endpoints/{id}/enabled` | Body `{enabled}`. Known issue: doesn't route for ids containing `/` (for example `anthropic/claude-opus-5-5`); use `PUT` with the `enabled` field instead |
| `GET /api/admin/providers/{id}/catalog` | A provider's model catalog. OpenRouter only. `?q=` filters, `?refresh=1` skips the 10-minute cache, at most 200 results. `404` provider not configured, `400` any other provider, `502` upstream failure |
| `GET /api/admin/providers/{id}/routes?model=` | Upstreams serving a catalog model. `model` is required (`400`); same `404`, `400`, `502` as the catalog |
| `GET /api/admin/policies` | Routing policies with validation errors and unknown endpoint references |
| `PUT /api/admin/policies/{name}` | Body `{yaml, priority, enabled?}`. `priority` defaults to 100, `enabled` to `true`. The YAML is validated (`400 invalid policy`) |
| `DELETE /api/admin/policies/{name}` | Remove a policy |
| `GET /api/admin/rentals` | `?limit=` (default 50, at most 200). `{templates, instances, providers, caps}`: the templates from `infra/rental/templates`, rented machines newest first with `estimated_usd`, which providers are configured, and `caps` (`daily_cap_hours`, `used_today_hours`, `disabled`, `key_set`; `key_set` is always true in the current build, so do not rely on it) |
| `POST /api/admin/rentals` | Body `{template}`. Starts a machine: `201` with the instance; `400 template required`, `404 unknown template`, `409` if the template already has an open machine, the kill switch is on, the daily cap is reached, or no provider or rental key is set; `502` for a provider error; `503 rentals are not configured` |
| `POST /api/admin/rentals/{id}/stop` | Stops one machine; `200` with the instance |
| `POST /api/admin/rentals/stop-all` | Stops every open machine; `200 {"status":"ok"}` |
| `GET /api/admin/rentals/offers/{provider}` | The provider's GPU offers `{offers:[{gpu, memory_gb, hourly_usd, available}]}` (the Admin page does not call it yet) |
| `GET`, `PUT /api/admin/budgets` | List; set with `{scope, scope_id, period, limit_usd, on_exceed}`. `scope` is `global`, `user`, `agent` or `api_key`; `period` is `day`, `week`, `month` or `total`; `on_exceed` is `block` or `downgrade` (default); `limit_usd` must be above 0; `scope_id` must be a UUID unless `scope` is `global`. `400` otherwise |
| `DELETE /api/admin/budgets/{id}` | Remove a budget. `200 {"status":"ok"}` even for an unknown id; `400` for a malformed one |

### Claude Code jobs (owner only, network-gated)

The read-only job tab and the reports `wsj` sends it. All four routes need the `owner` role **and** a client address inside `WS_CC_WEB_ALLOW` (by default the tailnet and loopback), otherwise `403 this route is only served on the tailnet`. The address checked is the one the server sees after its real-IP middleware, which takes it from the `X-Forwarded-For`, `X-Real-IP` or `True-Client-IP` headers; so the gate is only as strong as the proxy in front of ws, which must overwrite those headers (see [Trust and security](TRUST.md)). If the job tab is not configured on the host the routes answer `503`.

| Route | Notes |
|---|---|
| `GET /api/jobs/cc` | `{jobs, refresh}`. `jobs` are `cc_jobs` rows, newest first (`?limit=`, default 100, at most 500). Unless `?refresh=0`, it first refreshes against the targets' tmux (throttled to once per 10 seconds; `?refresh=force` skips the throttle). `refresh` is `{at, targets:[{name, ok, error?, windows}], error?}`. Also carries `budget: {week, used, cap, soft_pct, level}`: the week's launch count against `WS_CC_WEEKLY_CAP` (`week` is the Monday 00:00 UTC the count starts, `level` is `ok`, `soft` or `full`); left out only when the count cannot be read (see [Configuration](CONFIGURATION.md)) |
| `POST /api/jobs/cc` | `wsj` reports a launch. Body `{id, target, session, window, cwd, lane, model?, started, status}`, `status` `alive` or `dead`, `lane` only `claude-subscription`. `201` with the row; `400` for a bad body or id. A repeat report keeps the row's user and source |
| `DELETE /api/jobs/cc/{id}` | `wsj` reports a kill. `200 {"status":"ok"}`, `404` for an unknown id. Nothing is killed from ws |
| `GET /api/jobs/cc/{id}/term` | WebSocket, `?cols=&rows=`. Binary frames carry terminal bytes to the browser. The browser may send only a text frame `{"type":"resize","cols":N,"rows":N}`. If the view cannot open (window gone, target unreachable, no `tmux` or `ssh` on this host) the server sends one text frame `{"type":"error","message":...}` and closes |

## Other origins

### Artifact origin (`WS_ARTIFACT_URL`, `:8081`)

| Route | Notes |
|---|---|
| `GET /a/{version}?t=` | Serves one artifact version. `t` is a signed token minted by the app, valid for 24 hours by default; a bad or missing token, or a bad UUID, gets `404 not found`. Responses carry `Cache-Control: private, max-age=300`, `Cross-Origin-Resource-Policy: cross-origin`, a CSP that blocks network access, and the app embeds them in `<iframe sandbox="allow-scripts">` |
| `GET /healthz` | Plain `ok` |

### Preview hosts (`WS_PREVIEW_HOST`, `WS_PREVIEW_DOMAIN`)

Requests whose `Host` matches the preview pattern are reverse-proxied to a port in the user's sandbox. The first request arrives at `/__ws/auth` with a short-lived token from the app's redirect; it sets a host-only `ws_preview` cookie and redirects to `/`. An invalid or expired token gets `401`, as does a request without the cookie. A stopped sandbox or a port nothing listens on gets `502`. The token and the cookie last 12 hours. The whole `Cookie` header is stripped before proxying, so the dev server sees no cookies at all (neither the app session nor any cookie its own pages set).

## Worker internal API (`WS_INTERNAL_LISTEN`, `:8082`)

Not for users. `serve` calls it to reach sandboxes, authenticated by an `X-WS-Internal` header derived from `WS_SESSION_SECRET` (a `token` query parameter is accepted for tooling because browsers can't set headers on WebSocket dials). Never publish this port.

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness. Also needs the header; returns an empty `200` |
| `POST /internal/sandboxes/ensure` | Body `{project_id, user_id}`. Create or start a user's sandbox for a project |
| `GET /internal/sandboxes/{id}` | Status |
| `POST /internal/sandboxes/{id}/stop` | Stop |
| `GET /internal/sandboxes/{id}/tree` | File tree |
| `GET`, `PUT /internal/sandboxes/{id}/file?path=` | Read, write. Read returns raw bytes with `X-WS-Path` and (for binary content) `X-WS-Binary: 1` headers. Both cap at 8 MiB (write returns `413`) |
| `GET /internal/sandboxes/{id}/pty` | WebSocket terminal |

The egress proxy (`WS_EGRESS_LISTEN`, `:3128`) is a separate listener used only by sandbox containers; see PLAN.md for its role.

## Frontend

Any other path on the app origin serves the embedded single-page app, with `index.html` as the fallback. Hashed files under `/assets/` are cached for a year.
