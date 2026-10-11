# HTTP API

The routes `ws serve` exposes, grouped by who calls them. Verified against `internal/httpx/server.go`, `internal/externalapi/`, `internal/artifacts/origin.go`, `internal/httpx/preview.go`, `internal/sandbox/internalapi.go` and the handlers they mount. Last verified at commit `92ae622` (2026-10-09; the training routes of #184 and the `/v1/models` selector listing of #188 included).

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
| API key | `Authorization: Bearer ws_...` | `/v1` (needs the `chat` scope), `/mcp` (needs the `mcp` scope) and the `wsj` job reports under `/api/jobs/cc` (needs the `jobs` scope). Every other `/api` route refuses a key with `403`, whatever its scopes and whoever owns it |
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
| `GET /v1/models` | OpenAI, or Anthropic | Lists `auto`, every name the policies give meaning to (the aliases and the selectors rules match on, such as `fast`), then enabled non-embedding endpoints. In the Anthropic shape (`{data:[{type,id,display_name,created_at}], has_more, first_id, last_id}`) when the request carries an `anthropic-version` header, which is how Claude Code's model discovery asks; the OpenAI shape otherwise |
| `GET /v1/models/{id}` | OpenAI, or Anthropic | Echoes the id with `resolved` set to what the name routes to (`{id, object, created, owned_by:"ws", resolved}`; with `anthropic-version`, `{type:"model", id, display_name, created_at, resolved}`). Never `404` |
| `POST /v1/chat/completions` | OpenAI Chat Completions | Streaming (`stream: true`) with usage, tool calls, images. Usage always arrives in a final chunk with `choices: []`. Non-stream responses add `ws: {endpoint}` |
| `POST /v1/messages` | Anthropic Messages | Streaming, tool use, images. Two paths behind one route; see below |
| `POST /v1/messages/count_tokens` | Anthropic | Proxied to Anthropic for a native Anthropic endpoint (an exact count, no ledger row); otherwise a rough local estimate (characters / 4). Returns `{input_tokens}` |

Status codes on the chat routes: `400` bad body or no messages, `402` a blocking budget is spent, `503` no route, `502` any other gateway error. On `/v1/messages*` these come back in the Anthropic shape with `x-should-retry` set: `402` is `rate_limit_error` with `false`, `503` is `overloaded_error` with `true`, `502` is `api_error`.

Model names resolve in this order: empty or `default` uses the key's default policy alias (else `auto`); an exact endpoint id; a policy alias (`auto`, `cheap`, `best`, `code`, `local`, plus any you define) or the selector of a rule that matches on one (`fast`, which ranks by measured speed); a provider model name matching an endpoint's `model` or the tail of its id; otherwise the key's default alias. The name `code` also sets the task class to `code`.

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
| `POST /api/auth/magic/send` | none | Body `{email}`. `400` if missing or malformed, otherwise always `{"status":"sent-if-known"}`, whether or not the address is a member, and the reply does not wait for the mail. An address that has asked for three links in 15 minutes, or a server that has sent 60 in that time, gets the same reply and no mail (keyed by the user, not the typed address). The brake also means someone who can reach this route and knows an owner's address can keep that owner's magic-link sign-in refused for as long as they keep asking; passkey sign-in and an existing session are unaffected, and the owner can sign in again once the 15 minutes pass without a request |
| `POST /api/auth/passkey/login/begin`, `.../finish` | none | WebAuthn sign-in. `begin` returns `{options, ceremony_id}` (`503` when passkeys are unavailable). `finish` needs `?ceremony=<id>`, sets the cookie, and returns `401` on failure |
| `POST /api/auth/passkey/register/begin`, `.../finish` | auth | Add a passkey. `begin` as above; `finish` needs `?ceremony=<id>` and `?name=` |
| `POST /api/auth/stepup/begin` | session | Step-up (M5): starts a passkey assertion limited to the caller's own passkeys, user verification required. `{options, ceremony_id}`; `403 {code: "step_up_unavailable"}` when the account has no passkey, `429` after ten starts in a minute, `503` without passkeys on this host. Browser sessions only |
| `POST /api/auth/stepup/finish?ceremony=` | session | The browser's assertion as the body. Marks the session (`sessions.stepped_up_at`) and returns `{stepped_up_until}`; `401` when the assertion is not accepted or the ceremony is another person's or used. One assertion unlocks every gated class for `WS_STEPUP_WINDOW` |
| `GET /api/auth/passkeys` | auth | List the caller's passkeys |
| `DELETE /api/auth/passkeys/{id}` | auth | Remove one. `200 {"status":"ok"}` even for an unknown id; `400` for a malformed one |
| `POST /api/auth/logout` | auth | Revoke the session and clear the cookie. With an API key it only clears the cookie |

"auth" here is any accepted credential, API keys included, not only a session.

## App API (`/api`)

All routes below need authentication.

### Identity and models

| Route | Purpose |
|---|---|
| `GET /api/me` | `{id, email, display_name, role, via_api_key, training_consent, locality, step_up_until, step_up_window_seconds, has_passkey}`. `step_up_until` is when this session's passkey assertion runs out (null: none, or expired; only filled once `WS_HOME_NETWORKS` is set) and `has_passkey` whether the account can step up at all. `locality` is how this request was judged, home or away (M5, `internal/httpx/locality.go`): `{configured, locality, listener, address, reason}`. `configured` is whether `WS_HOME_NETWORKS` is set (when it is not, `locality` is `home` for everyone and nothing is gated); `listener` is `main` or `tunnel`; `address` is the client address it was judged by (`""` when there was none) and is reported even when unconfigured, so the owner can read what ws sees; `reason` is a sentence fit for a tooltip, such as "tunnel, from your home address", "direct, address in the home networks" or "tunnel, address not in the home networks". On the tunnel listener the address is `CF-Connecting-IP` and only that |
| `PUT /api/me` | Body `{display_name}` (1 to 80 characters after trimming, `400` otherwise). Changes the caller's display name, the one field of their own they edit; the address and role are not changeable here. Answers `{id, email, display_name, role}` |
| `PUT /api/me/training-consent` | Body `{enabled}`. Whether the caller's conversations may be exported into training datasets (PLAN M10); off by default |
| `GET /api/me/provider-keys` | Session only. `{enabled, providers: [{provider, name, has_key, last4?, updated_at?}]}`: every hosted provider (one with an `api_key_env`) and whether the caller has saved a key for it. `enabled` is false when the server has no `WS_SECRETS_KEY`. The key itself is never returned. |
| `PUT /api/me/provider-keys/{provider}` | Session only. Body `{key}` (8 to 512 characters, no whitespace). Seals and stores the caller's own API key for that provider; `400` for something that does not look like a key, `404` for an unknown or keyless provider, `503` when `WS_SECRETS_KEY` is unset. Answers `{provider, name, has_key, last4}`. |
| `DELETE /api/me/provider-keys/{provider}` | Session only. Removes the caller's key for that provider. |
| `PUT /api/messages/{id}/rating`, `DELETE /api/messages/{id}/rating` | Body `{score, note?}` with `score` `1` or `-1`: the caller's rating of an assistant message (one per user; a put replaces). `400` for a user message or another score; `404` for a message in a conversation the caller cannot see |
| `GET /api/conversations/{id}/ratings` | `{ratings: {message_id: score}}`, the caller's ratings in the conversation |
| `GET /api/models` | `{models, aliases}` for the model picker. `models` is `[{id, display_name, provider, local, capabilities, pricing, health}]` for enabled non-embedding, non-media endpoints; `media` lists the enabled image and video endpoints (`id`, `display_name`, `provider`, `local`, `engine`, `image`, `image_edit`, `video`, `image_to_video`, `sizes`, `max_images`, `max_seconds`, `seconds` (the allowed video lengths, `[]` when unrestricted), `health`, and `per_image` and `per_second` for hosted ones); `aliases` maps alias name to endpoint ids |
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
| `DELETE /api/conversations/{id}` | Archive (not a hard delete). `PATCH` and `DELETE` answer `200 {"status":"ok"}`. An archived conversation, and any conversation of an archived project, answers `404` on every other route until restored |
| `POST /api/conversations/{id}/restore` | Un-archive. The conversation's own member, or its project's owner; anyone else gets `404` |
| `DELETE /api/conversations/{id}/permanent` | Delete an **archived** conversation for good: messages, attachments no other conversation, media job or notebook source uses (and their files on disk unless another row uses the same bytes), artifacts and versions, agent runs and steps, document chunks. The usage ledger keeps its rows with the conversation and run references cleared; memories and notebook sources made from it stay with the link cleared. `409` when it is not archived or an agent run in it is open. Returns `{conversations, messages, artifacts, runs, attachments, bytes, blobs_deleted, blobs_shared, blobs_failed}`. Step-up class `conversation_delete` (asked for from away) |
| `GET /api/archive` | The caller's archived conversations (own, or in a project they own, outside archived projects) each with `messages`, `bytes` and `can_delete`, their archived projects each with what deleting it removes, and `all`: what `delete-all` would remove, counted together |
| `POST /api/archive/delete-all` | Permanently delete every archived conversation `GET /api/archive` lists. Same report as above; step-up class `conversation_delete` |
| `POST /api/projects/{id}/archive` | Owner only. The project and its conversations leave every list; nothing is removed |
| `POST /api/projects/{id}/restore` | Owner only |
| `GET /api/projects/{id}/purge-plan` | Owner only. `{name, archived, plan}`: what deleting it would remove (conversations, messages, files, artifacts, runs, agents, sandboxes, bytes) |
| `POST /api/projects/{id}/delete-permanently` | Owner only, body `{"name": "<the project's name>"}`, which must match exactly (`400` otherwise). Only an **archived** project (`409` otherwise): every conversation in it as above, its agents and their runs and triggers, scheduled prompts, media jobs and their files, its sandbox containers and workspace volumes (removed through the worker; `500` with the reason when the worker cannot, and nothing else is deleted), and its settings and design system. Same report. Step-up class `project_delete`, which asks for the passkey at home too |
| `GET /api/conversations/{id}/artifacts` | Artifacts created in the conversation |
| `GET /api/artifacts/{id}`, `GET /api/artifacts/{id}/versions/{v}` | An artifact, current or by version (`?version=N` also works). Returns `{artifact, versions, version, version_id, url, content, kind, title, design_context}`; `url` is the signed viewer URL on the artifact origin; `design_context` is the design system a `design` version was made under (null otherwise) |
| `GET /api/artifacts/{id}/export?version=N` | The version's content as a download (`Content-Disposition: attachment`): `.html` for `html` and `design`, `.svg`, `.md`, `.mmd`, or the code language's extension. The model's document as stored, never rendered on the app origin |
| `POST /api/artifacts/{id}/variants` | Body `{n?, instruction?, version?, model?}`. Generates `n` (default 3, at most 4) alternatives of a `design` or `html` artifact's version in parallel, each a model call billed to the caller, and stores each as a new `design` artifact in the conversation titled "`title` · variant k" with the same `design_context`. `201 {variants: [ref], errors: [string]}`; `502` when none succeeded, and also for an `n` above 4 (it is not a `400`). The call stops if the client disconnects, and there is no per-user rate limit |

### Favorites and saved prompts

The member's own (PLAN M13). A member keeps at most 500 favorites and 50 saved prompts.

| Route | Notes |
|---|---|
| `GET /api/favorites` | `{favorites}`: `{id, kind, ref_id, title, snippet, conversation_id, project_id, created_at}`, newest first; only items whose target still exists and whose project the caller can reach |
| `POST /api/favorites` | Body `{kind: "message" or "artifact" or "media", ref_id}`. Stars it (starring twice is one favorite). `404` when the target does not exist or is not in a project the caller can reach, whichever it was; `400` for an unknown kind or at the limit. `201` |
| `DELETE /api/favorites/{kind}/{ref}` | Un-stars; `200` even when it was not starred |
| `GET /api/saved-prompts` | `{prompts}` by name: `{id, name, text}` |
| `POST /api/saved-prompts` | Body `{name, text}`: a name of one line up to 60 characters, text up to 8 KiB. `201` |
| `PUT /api/saved-prompts/{id}` | Same body; `404` if not the caller's |
| `DELETE /api/saved-prompts/{id}` | `200`; deleting another member's is a no-op |

### Research notebooks

A member's notebooks (PLAN M13). Every route works on the signed-in member's own notebooks; another member's id is `404`. A member keeps at most 20 notebooks, 30 sources and 200 notes in each.

| Route | Notes |
|---|---|
| `GET /api/notebooks` | `{notebooks}`: `{id, name, conversation_id, sources, updated_at}`, most recently used first |
| `POST /api/notebooks` | Body `{name}`; `201` with the notebook |
| `GET /api/notebooks/{id}` | `{notebook, sources, notes}`; notes are the model's summary and open-questions notes first, then the rest |
| `PATCH /api/notebooks/{id}` | Body `{name}` |
| `DELETE /api/notebooks/{id}` | Deletes it with its sources and notes; its chat stays as a conversation |
| `POST /api/notebooks/{id}/sources` | One of: a multipart `file` (PDF, Word, Excel, PowerPoint, text, or an image when OCR is configured; read as in [document reading](CONFIGURATION.md#document-reading-internaldocread-internaldocuments)); JSON `{url}` (a public page, fetched with the same guard as `web_fetch`: private and loopback addresses are refused); `{text, title?}`; or `{conversation_id}` (one of the caller's own). `201` with the source; `400` with the reason |
| `GET /api/notebooks/{id}/sources/{sid}` | `{source, pages: [{page, text}]}` |
| `DELETE /api/notebooks/{id}/sources/{sid}` | |
| `POST /api/notebooks/{id}/notes` | Body `{title?, body}` (at most 20,000 characters); `201` |
| `PUT /api/notebooks/{id}/notes/{nid}` | Body `{title?, body}`; the editor becomes the author |
| `DELETE /api/notebooks/{id}/notes/{nid}` | |
| `POST /api/notebooks/{id}/chat` | `{conversation_id}`: the notebook's conversation, created on first call in the caller's first chat project (or a new "Notebooks" one) |

A turn in a notebook's chat gets the source list with ids, the summary and open-questions notes and the passages nearest the message (about 9,000 characters at most) in its system prompt, and the `notebook_note` tool (`kind` `summary`, `open_questions` or `note`; it works only in that notebook's own chat). Replies cite with `[label](src: <source id> p.3)` and may explain a term with `[word](tip: a sentence)`; the web client renders both (tooltip, jump to the source).

### Scheduled prompts

A member's prompts that run on their own (PLAN M13). Every route works on the signed-in member's rows only; another member's id is `404`.

| Route | Notes |
|---|---|
| `GET /api/scheduled` | `{schedules}`: each `{id, project_id, title, prompt, model, kind, cron, timezone, run_at, enabled, allow_web_fetch, notify_email, reuse_conversation, conversation_id, next_run_at, last_run_at, last_status, last_error}` |
| `POST /api/scheduled` | Body `{project_id, prompt, title?, model?, kind: "once" or "cron", cron?, timezone?, run_at?, enabled, allow_web_fetch?, notify_email?, reuse_conversation?}`. `once` needs a future `run_at`; `cron` needs 5 fields and may not fire more often than every 15 minutes; `timezone` is an IANA name (default UTC); the prompt is at most 8 KiB; a member keeps at most 20; the project must be accessible. `201` with the schedule, `400` with the reason otherwise |
| `PUT /api/scheduled/{id}` | Same body; recomputes the next firing. A paused schedule (`enabled: false`) has no next run |
| `DELETE /api/scheduled/{id}` | Deletes it and its history |
| `POST /api/scheduled/{id}/run` | Fires it now without touching the next firing; `202` with the run `{id, run_id, conversation_id, status, error, started_at}`. A failure to start is a run with status `failed`, not an HTTP error |
| `GET /api/scheduled/{id}/runs` | `?limit=` (default 30, at most 100): `{runs}`, newest first |

A due schedule starts an ordinary chat run (task class `chat`, at most 8 steps, no tools, or only `web_fetch`) in a new conversation titled after it, or the schedule's own when `reuse_conversation` is set. The worker's `schedule.tick` fires them each minute; a firing missed while the worker was down runs once and the next one is computed from then.

### Personal memory

Each member's own memory and card (PLAN M13). Every route works on the signed-in member's rows only; another member's id is `404`. Memory is never part of training exports.

| Route | Notes |
|---|---|
| `GET /api/memory` | `?limit=` (default 100, at most 200) and `?offset=`. `{settings, memories, total}`: `settings` is `{enabled, reflect_enabled, reflect_at, timezone, profile, last_reflected_at}` (the defaults when never saved), each memory `{id, kind, content, source, importance, last_used_at, created_at}`, never the vector, most important first |
| `PUT /api/memory/settings` | Body is `settings` as above (`last_reflected_at` ignored). `reflect_at` is `HH:MM`, `timezone` an IANA name; the card's fields are trimmed and bounded (12 projects and 12 tools of at most 80 characters, a role of 120, a week note of 300). `400` with the reason otherwise |
| `POST /api/memory` | Body `{content, kind?, importance?}`; kind is `fact` (default), `preference` or `decision`, content at most 500 characters. `201` with the memory; `400` at the limit of 500 memories |
| `PATCH /api/memory/{id}` | Body `{content, kind, importance}` (importance above 0, at most 1); the embedding is recomputed. `404` for one that is not yours |
| `DELETE /api/memory/{id}` | Forget one. `200 {"status":"ok"}`, `404` if not yours |
| `DELETE /api/memory` | Forget every memory; the card and switches stay |
| `POST /api/memory/reflect` | Queue a `memory.reflect` job for the member now ("tidy up now"). `202` |

In a chat turn the tools `remember` and `recall` read and write this memory (in an agent's run they use the agent's). Each chat turn also starts with the card and the memories nearest the message in its system prompt, within about 600 tokens; the stream carries a `data-memory` part listing which ones, and nothing is recalled when `enabled` is false.

### Chat and runs

`POST /api/conversations/{id}/chat` runs one turn and streams the response in the [Vercel AI SDK UI Message Stream](https://ai-sdk.dev) format (header `x-vercel-ai-ui-message-stream: v1`, server-sent events). Body:

```json
{
  "messages": [{"role": "user", "parts": [{"type": "text", "text": "hello"}]}],
  "model": "auto",
  "reasoning": false
}
```

The body also carries `id` and `trigger` (sent by the AI SDK; ignored). `plan: true` starts the turn in plan mode (M13): the model first calls `submit_plan`, the run pauses as an approval, and no other tool runs until the person approves. Approve, or approve with `reason` set on the approval response to the edited plan (one step per line), or reject (`approved: false`, `reason` optional), which ends the turn. `plan` is read only on a new user message; a resume uses the plan setting the run started with. The approval's `reason` is stored as the approval's `note`. Two shapes are accepted. If the last message is from the `user`, a new run starts. If it is the `assistant` message echoed back with tool approval decisions in its parts, the paused run resumes. Any other shape is `400` (`no messages`, `last message must be from the user`, `assistant message carries no approval decisions`). Errors during a resumed run are sent in the stream, not as a status code.

| Route | Purpose |
|---|---|
| `GET /api/conversations/{id}/runs` | The 20 most recent runs for the conversation, newest first |
| `GET /api/runs/{id}` | `{run, steps, approvals}` for one run |
| `POST /api/approvals/{id}` | Body `{approved: bool, note?}`. Decides a pending approval and returns the updated row; `409` if already decided. For a `submit_plan` approval (plan mode), `approved: true` with a `note` approves an edited plan and the note is the plan the model follows; `approved: false` ends the turn. Once no approvals are pending the run is re-queued for the worker |

### Sandboxes (code projects)

These proxy to the worker's internal API; the browser never reaches the worker or the Docker socket. Every route here returns `400` for a project that isn't a code project and `503` when no worker is configured. A worker `403` (shared secret mismatch) is returned as `502`. Apart from `sandbox/stop`, each route ensures the sandbox is running first, which can take up to 10 minutes on first use.

| Route | Purpose |
|---|---|
| `GET /api/projects/{id}/sandbox` | Creates or starts the sandbox, returns `{id, status, runtime, container_id, repo_url, preview_pattern?}` |
| `GET /api/projects/{id}/sandbox/status` | Reads the caller's sandbox without starting it, so it answers at once while `GET .../sandbox` is still waiting on the worker: `{status, error, last_error}`. `status` is the row's (`none` before any sandbox exists), `error` its stored reason or `null`, and `last_error` is `{stage, error, at}` for the newest setup failure (`stage` is `pull`, `volume`, `create` or `clone`) unless a later start or successful clone followed it, else `null` |
| `POST /api/projects/{id}/sandbox/stop` | Stop it (the volume stays). `204` |
| `GET /api/projects/{id}/fs/tree?path=` | One directory, not recursive: `{path, entries: [{name, type, size}]}` |
| `GET /api/projects/{id}/fs/file?path=` | Read a file: `{path, binary, size, content}`. Files over 8 MiB fail (the worker answers `404` with the reason) |
| `PUT /api/projects/{id}/fs/file` | Body `{path, content}`. Write a file. `204`; `413` over 8 MiB |
| `GET /api/projects/{id}/pty` | WebSocket carrying a `bash` session; query `cols`, `rows`, `cwd` |
| `GET /api/projects/{id}/preview/{port}` | `302` to the preview host's `/__ws/auth`. `400` bad port, `503` if previews aren't configured |

### Media (images and video)

Generation is a durable job (`media_jobs`, River queue `media`), routed through the gateway so policies and budgets apply and one ledger row is written per job. All routes need a session and access to the project.

| Route | Notes |
|---|---|
| `GET /api/projects/{id}/media` | `{jobs}`, newest first. `?limit=` (default 60, at most 200), `?offset=`, `?kind=` |
| `POST /api/projects/{id}/attachments` | Uploads a source photo for an edit or an image-to-video job. Multipart field `file`, images only (the type is read from the bytes), at most 32 MB. `201 {id, url, mime, bytes, filename, width, height}`; `404` for an inaccessible project, `400` for a missing or non-image file, `413 file over 32 MB`, `503 attachments are not configured`. The attachment belongs to the uploader |
| `POST /api/projects/{id}/media` | Body `{kind?, prompt, size?, n?, quality?, seconds?, source_attachment_id?, mask_attachment_id?, scale?, model?, conversation_id?}`. `kind` is `image`, `edit` (needs `source_attachment_id`; `mask_attachment_id` marks the area to change, and only an edit takes one (`400` otherwise); OpenAI reads it as its `mask` field (transparent pixels are edited), fal sends it as `mask_url`, ComfyUI fills `{{mask}}`, and the Google engine ignores it), `upscale` (needs a source; `scale` 2, the default, or 4; the prompt is optional; one output) or `video` (text to video, or image to video when a source is given); it defaults to `image`, or to `edit` when a source is given. A source or mask must be an image of at most 32 MB that the caller owns or that a media job in the same project produced. `n` is at most 4 (and at most the endpoint's `max_images`; one per video job); `seconds` must be one of the endpoint's listed lengths, defaulting to the first, else 5 capped by `max_seconds`; a prompt is at most 32,000 characters. Returns `201` and the job, with `inputs.estimate_usd` (the price shown before generating). `400` for an invalid request (no prompt, size or count the endpoint does not accept, no media endpoint for the selector, conversation not in the project), `402` when a blocking budget is spent, `503` when media is not configured |
| `GET /api/media/{id}` | One job: `status` (`queued`, `running`, `done`, `failed`, `cancelled`), `progress`, `cost_usd`, `error`, and `outputs` (`[{id, url, mime, bytes, filename, width, height}]`) |
| `DELETE /api/media/{id}` | Cancels a queued job (`{status:"cancelled"}`), removes a finished one (`{status:"deleted"}`; its attachments are kept), `409` while it runs (a started generation cannot be stopped) |
| `GET /api/attachments/{id}` | Streams an attachment to its owner, or to anyone who can see a media job that produced it. Never served as HTML (`text/html` becomes `application/octet-stream`), with `Content-Security-Policy: sandbox; default-src 'none'`, `Content-Disposition: inline` and an immutable one-year private cache. `503` when attachments are not configured |

The Google engine (Imagen and Veo) takes `image`, `video` and image to video, but not `edit`, a mask or `upscale`. The Media page asks for a 2x upscale only; this route takes 2 or 4. Jobs found running for more than 15 minutes (a worker died) are marked abandoned. The agent tools `generate_image` (registered, and allowed in chat, only when an image endpoint exists on the box; an optional `source_attachment_id` makes it an edit, and `mask_attachment_id` adds a mask; there is no upscale tool, upscaling is on the Media page and this route) and `generate_video` (only when a video endpoint exists; `prompt`, `seconds` 1 to 60, `size`, `source_attachment_id`, `model`) create a job in the conversation's project, wait for the worker (4 minutes for an image, 15 for a video), and return the attachment URLs so the chat renders them, with an inline player for video.

### Agents and runs (PLAN M7)

An agent is a standing goal with its own tools, triggers and run history; each run is a conversation in the agent's project (`mode='agent'`). Every route needs a session and answers `404 not found` unless the agent belongs to the caller. An agent has at most one open run (`queued`, `running` or a pause): starting another gives `409`.

| Route | Notes |
|---|---|
| `GET /api/agents` | The caller's agents, each with `project_id` |
| `GET /api/agents/presets` | `{presets}`: the starting points the new-agent form offers, from `config/presets/*.yaml` (`WS_PRESETS_DIR`), in name order; each has `name`, `title`, `description`, `goal`, `prompt`, `model`, `tools` (an allowlist; `["none"]` means no tools, `[]` every tool), `max_steps`, `note`. Picking one copies its fields into the form; nothing links the agent to the preset afterwards |
| `POST /api/agents/presets/import` | Body `{text}`: one `SKILL.md` (AgentSkills shape: YAML frontmatter with `name` and `description`, a markdown body). Returns a preset preview `{preset, requested_tools, warnings}` and saves nothing: the form copies the preview and the person presses Create. The text is untrusted: the body goes into `prompt` under a label that says it is imported content (ws's rules and the goal come after it), `tools` is `["none"]`, `allowed-tools` and `tools` from the frontmatter are reported in `requested_tools` and never granted, other frontmatter keys are ignored and listed, zero-width and direction-override characters and HTML comments are removed and counted, links (by host), fenced blocks and instruction-override phrases are listed in `warnings`. Limits: 64 KB file, 64-character name, 1 KB description, 32 KB body (longer is cut and said). `400` for an empty, oversize, non-UTF-8 file, no frontmatter, no name or no body |
| `POST /api/agents/{id}/skills` | Owner only. Two forms. Multipart, field `file`: a `.zip` skill folder, approved onto the caller's shelf (as `POST /api/skills`) and attached to this agent. JSON `{"skill_id"}`: attach a skill already on the shelf (`404` when it is not theirs). `409` past 10 skills on an agent. Attaching grants no tool, secret or network |
| `DELETE /api/agents/{id}/skills/{name}` | Owner only. Takes the skill off this agent; the shelf keeps it. `204`, or `404` when the agent has none by that name |
| `GET /api/skills` | The caller's skill shelf (no instructions): `id`, `name`, `title`, `description`, `has_scripts`, `files` (path, kind, size, sha256, refused, findings), `refused_secrets`, `findings`, `approved_by`, `approved_at`, `agent_count` |
| `POST /api/skills/preview` | Multipart `file`: a skill folder zip. `{import, name, existing?, diff?, agents?}`: the import's findings and, when a skill of that name is on the shelf, what would change (`diff`: `same`, instruction lines `+`/`-`, files added, changed and removed, scripts added, findings on what is new) and which of the caller's agents use it. Stores nothing |
| `POST /api/skills` | Multipart `file`: approve the folder onto the caller's shelf, replacing a skill of the same name (every agent that uses it gets the new version). The zip is read again on the server; a file with a literal credential is listed as refused and not stored. File bytes go to the blob store by hash |
| `DELETE /api/skills/{id}` | Off the shelf and so off every agent. `204`; `404` when it is not the caller's |
| `POST /api/skills/bulk/preview` | Multipart `file`: a `.zip` of a skills directory (each folder with a `SKILL.md`, up to two levels down, at most 50, 20 MB). `{items: [{dir, preview?, error?}]}`: each folder is read and previewed on its own; one that cannot be read says why and does not stop the others |
| `POST /api/skills/bulk/approve` | Multipart `file` (the same zip) and one form field `dir` per folder to approve; the rest are skipped. `{approved, failed: [{dir, error}]}` |
| `POST /api/agents/presets/import-folder` | Multipart, field `file`: a `.zip` of one skill folder (`SKILL.md` at the archive root or in its one top-level folder, optional `scripts/`, `references/`, `assets/`). Returns `{skill, files, has_scripts, usable_anywhere, refused_secrets, findings, bytes}`: `skill` is the single-file preview above; `files` inventories every other file (`path`, `kind` script, reference, asset or other, `size`, `sha256`, `interpreter`, `binary`, `refused` (a literal credential: the file is not stored if approved), `findings`: network use or installs, risky shell, hidden characters, instruction-override language, links, credential-shaped strings counted and never quoted); `refused_secrets` are front matter keys that name credentials (`env`, `apiKey`, `secrets` and the names under `env`, never their values). Nothing is executed, stored or granted. Limits: 5 MB archive, 200 files, 2 MB a file, 20 MB uncompressed; a path that leaves the folder, a symbolic link, a duplicate name or more than one skill is `400` |
| `POST /api/agents` | Body `{name, goal, system_prompt, project_id, model: {selector, task_class, reasoning, plan_first?}, tool_allowlist[], tool_policies{tool: auto, ask or deny}, max_steps, max_run_usd, enabled}`. An empty `tool_allowlist` means every tool the worker has; `["none"]` means no tools. `name` and `project_id` are required and the project must be accessible; `max_steps` defaults to 50 and is at most 500 (`400`); `max_run_usd` is a cap on one run's spend in dollars, 0 (the default) for none, at most 10000 (`400`): a run that reaches it ends as done after the step that crossed it, with a notice, and the tool calls that step asked for do not run. `enabled` is ignored on create. `201` |
| `GET /api/agents/{id}` | `{agent, triggers, runs (30), pending_approvals, spend}`; `spend` is `{since, usd, budgets, default}` for the UTC month, with the agent-scoped budgets; when the agent has no budget of its own and a default for every agent exists, `budgets` carries the default row and `default` is true |
| `PUT /api/agents/{id}` | Same body as create; `enabled` keeps its value when omitted |
| `DELETE /api/agents/{id}` | `200 {"status":"ok"}` |
| `POST /api/agents/{id}/enabled` | Body `{enabled}` |
| `POST /api/agents/{id}/run` | Body `{input?}`. Starts a run now (the goal is the input when empty). `201` with the run; `409` if the agent is disabled or already has an open run; `503` when agents are not configured |
| `POST /api/agents/{id}/triggers` | Body `{kind, name, spec}`. `kind` is `cron` (`spec.expr`, a five-field expression or `@hourly` style, and optional `spec.input`), `webhook`, `manual`, or `repo_push` (a GitHub webhook: `spec` is `{repo?, branches?, events?, input?}`; `repo` is `owner/name`, `branches` are names or patterns such as `release/*` (`*` does not cross `/`), `events` default to `["push"]`, `input` is prepended to what the agent is told; `400` for a malformed repo or branch pattern). A webhook or `repo_push` trigger answers with its `secret` and `url` once; only the hash is stored |
| `DELETE /api/agents/{id}/triggers/{tid}`, `POST .../{tid}/enabled` | Remove a trigger; set `{enabled}` |
| `POST /api/runs/{id}/cancel` | Cancels an agent's run (the run stops between steps). `404` for a run with no agent |
| `POST /api/runs/{id}/pause` | Holds a `queued` or `running` run: it stops between steps and keeps its place as `paused_manual`. `200 {"status":"paused_manual"}`; `400` for a run in any other state |
| `POST /api/runs/{id}/resume` | Queues a `paused_manual` run again; the worker continues it where it stopped. `200 {"status":"queued"}`; `400` unless the run is paused |
| `POST /api/runs/{id}/steer` | Body `{text}`. Posts the text into the run's conversation and starts a new run there; `409` while the run is still open |
| `POST /hooks/agents/{id}/{secret}` | The webhook. `{id}` is the trigger's id, not the agent's: use the `url` the trigger-create response returns (an agent id answers `404`). No session: the secret in the path is the credential. Any content type up to 64 KB, or up to 1 MB when an `X-GitHub-Event` header is present (`413 body too large` over). `202 {run_id, status}`; the payload becomes the run's input (for a `repo_push` trigger the event is rendered as text: pushes with commits, authors, files and a compare link; pull requests, issues, comments and releases; anything else generically; capped at 64 KB). A GitHub ping, an event the trigger does not list, a repository or a branch that does not match answers `200 {"status":"ignored"}` and starts no run (branches are checked for pushes and pull requests only). `403` for a `repo_push` delivery without `X-Hub-Signature-256` (the trigger's secret must be set as the repository webhook's secret, so GitHub signs each delivery) and `404` for a signature that does not verify; `404` for a wrong secret, a trigger of another kind or a disabled one (the secret is checked before the body is read, so these answer before the size limit applies); `409` when the agent is busy or disabled. When the delivery carries `X-Hub-Signature-256` (GitHub sends it once the repository's webhook has a secret; set it to the secret in the URL) the signature is checked against the body under that secret and a mismatch is `404` too; without the header the secret in the URL is the only credential |
| `POST /hooks/github` | The GitHub App's one webhook, open only when `GITHUB_WEBHOOK_SECRET` is set (`404` otherwise, and `404` for a missing or wrong `X-Hub-Signature-256` over the body under that secret; up to 1 MB). The delivery goes to every enabled `repo_push` trigger whose `spec.repo` names the delivery's repository (case does not matter; a trigger with no `repo` is never reached this way), each rendered by its own spec. `202 {run_ids, status: started}` with the runs that started (a busy or disabled agent is recorded on its trigger and skipped); `200 {"status":"ignored"}` when no trigger wanted it (a ping, no matching repository, event or branch) |

Cron triggers fire from the `agent.tick` job every minute. A firing is skipped, with `last_error` set on the trigger, when the agent already has an open run. What an agent remembers is under `GET /api/agents/{id}/memories` (`?limit=` default 50, at most 500, `?offset=`; `{memories, total}` newest first, each `{id, kind, content, importance, embedded, source_run_id, last_used_at, created_at}`, never the vector), `DELETE /api/agents/{id}/memories/{mid}` (forget one) and `DELETE /api/agents/{id}/memories` (forget all), all `200 {"status":"ok"}` and `404` unless the agent is yours. During an agent's run the tools `remember` and `recall` read and write the same memory; in a chat they use the person's own (see Personal memory).

### Admin (owner only)

| Route | Purpose |
|---|---|
| `GET`, `POST /api/admin/invites` | List invites; create with `{email, role}` (`role` defaults to `member`), returns `201 {link}`. `400` on an empty, malformed or already-registered email (a bare address only; `Name <a@b>` is refused) |
| `DELETE /api/admin/invites/{id}` | Owner only. Revokes a pending invite (the row is removed, so the link stops working); `204`. `409` once accepted, `404` unknown |
| `POST /api/admin/invites/{id}/resend` | Owner only. Issues a fresh link for a pending invite's address and role, withdraws the old one and mails the new link like create does; `201 {link}`. `409` once accepted, `404` unknown |
| `GET /api/admin/users` | Users |
| `PUT /api/admin/users/{id}/shared-keys` | Owner only. Body `{shared: bool}`. Lets one member use the server's shared provider keys (`users.shared_provider_keys`); without it a member reaches hosted models only with their own saved key. |
| `DELETE /api/admin/users/{id}/sessions` | Ends every browser session of that user: their next request answers `401`. Passkeys, API keys and the account stay, and a fresh sign-in works at once. The owner may run it on themselves (it ends their own session too). `200 {"status":"ok"}`; `400` for a malformed id, `404` for an unknown user |
| `GET /api/admin/users/{id}/keys` | That user's live API keys in the shape of `GET /api/keys` (`{id, name, prefix, scopes, default_policy, created_at, last_used_at}`, never the hash). `404` for an unknown user |
| `DELETE /api/admin/users/{id}/keys/{keyId}` | Revokes one of that user's API keys; `/v1` and `/mcp` refuse it from then on. `200 {"status":"ok"}`; `404` when the key is not that user's or is already revoked; `400` for a malformed id |
| `PUT /api/admin/users/{id}/disabled` | Owner only. Body `{disabled: bool}`. Parks a member (`users.disabled_at`) or restores them; returns the user row. Disabled, their sessions and API keys are refused at once (`401`), a sign-in link for their address is not sent (the reply is the same as for any address) and an existing link or passkey fails like a bad one; enabled again, everything works without a new invite. The owner cannot be disabled (`400`). |
| `GET /api/admin/routing` | Verdict routing: `{mode: "auto" or "off", max_usd_day, default_usd_day, spent_today_usd, deadline_ms, reuse_turns}`. `max_usd_day` is the owner's ceiling (the default when never set; 0 is no ceiling) and `spent_today_usd` the classifier's cost since UTC midnight |
| `PUT /api/admin/routing` | Body `{max_usd_day}`: a number from 0 to 10000, or `null` to go back to the environment's default (`400` otherwise). Returns the view above; takes effect on the next turn in both processes |
| `GET /api/admin/usage?days=` | Ledger summary: `{since, total_usd, by_endpoint, by_user, recent}`. `days` defaults to 30; `recent` is capped at 100 rows; each row's `decision` carries `verdict` and `verdict_source` (`llm`, `cache`, `skipped:*`, `fallthrough:*`) when verdict routing is on |
| `GET /api/admin/failures` | Owner only. The newest 50 background jobs that failed, of every kind, read from River's own table (`river_job`): `[{id, kind, state, attempt, max_attempts, at, error, ref}]`. `state` is `discarded` (gave up) or `retryable` (failed, waiting to retry); `error` is the last one the job recorded, cut to 600 characters; `ref` is `{label, link}` where the job's arguments name an agent run, media job, dataset, fine-tune job, adapter or conversation, else `null`. Rows hidden by the next route are left out. River removes discarded rows itself after its retention period (a week by default), so this is recent history, not an archive |
| `POST /api/admin/failures/clear` | Owner only. Hides the failures now listed (and any older ones still failing) from that list by setting `ws_hidden` in the job's `metadata`; nothing is deleted and no job changes state. `{hidden: n}`. A later failure is listed as usual |
| `GET /api/admin/github` | `{configured, fallback_token, slug?, install_url?, installations?, error?}`: the GitHub App's installations and install link |
| `GET /api/admin/endpoints` | `{providers, endpoints, policies, throughput, all_providers, engines}` as loaded. `throughput` maps endpoint id to `{tokens_per_sec, ttft_ms, samples, in_flight}`: the decode speed and time to first token the gateway has measured since this process started, and the requests being served now (an endpoint never used and idle is absent; one busy before its first measurement has `in_flight` and zeros). `all_providers` lists every stored provider, configured or not (`{id, kind, name, base_url, base_url_env, api_key_env, headers, configured, reason?}`; `reason` says which environment variable is missing or that the registry has not reloaded yet); `engines` lists the media engines this build has (`{id, name, image, image_edit, video, image_to_video, sizes, provider_kind, note}`; `openai_images`, `openai_videos`, `fal`, `comfyui` and `google`) |
| `PUT /api/admin/providers` | Create or update a provider and reload the registry. Body `{id, kind, name?, base_url, base_url_env?, api_key_env?, headers?}`: `id` matches `^[a-z0-9][a-z0-9._-]{0,63}$`, `kind` is `anthropic` or `openai_compat`, `base_url` starts with `http://` or `https://` unless `base_url_env` (an environment variable name, which wins and is stored as `env:NAME`) is set, `api_key_env` is a variable name or empty. Only variable names are stored, never a key. `400` with a message for each violated rule; returns the provider view. Providers that also appear in `config/endpoints.yaml` are overwritten by the file on the next boot |
| `DELETE /api/admin/providers/{id}` | Delete a provider (its endpoints go with it) and reload the registry. `200 {"status":"ok"}`; does not check that the id exists |
| `PUT /api/admin/endpoints` | Create or update an endpoint and reload the registry. Requires `provider_id` (a configured provider) and `model_name`; with `capabilities.media` set the engine must be one of `engines` (`400 unknown media engine`), the endpoint must make images or video (`400`), `sizes` are trimmed and `max_images` is clamped to 4; `id` defaults to `provider_id/model_name` and must not contain whitespace; `enabled` defaults to `true`; `tier` is `small`, `medium`, `large` or empty (derived from the model name; `400` otherwise) and `not_routable` keeps the endpoint out of the router's fallbacks (both are in the endpoint rows `GET` returns) |
| `DELETE /api/admin/endpoints/*` | Delete an endpoint by id (ids contain slashes). `200 {"status":"ok"}` |
| `POST /api/admin/endpoints/<id>/enabled` | Body `{enabled}`. The id may contain slashes (for example `anthropic/claude-opus-5-5`); the route is a wildcard and the id is everything before the final `/enabled` |
| `GET /api/admin/providers/{id}/catalog` | A provider's model catalog. OpenRouter only. `?q=` filters, `?refresh=1` skips the 10-minute cache, at most 200 results. `404` provider not configured, `400` any other provider, `502` upstream failure |
| `GET /api/admin/providers/{id}/routes?model=` | Upstreams serving a catalog model. `model` is required (`400`); same `404`, `400`, `502` as the catalog |
| `GET /api/admin/policies` | Routing policies with validation errors and unknown endpoint references |
| `PUT /api/admin/policies/{name}` | Body `{yaml, priority, enabled?}`. `priority` defaults to 100, `enabled` to `true`. The YAML is validated (`400 invalid policy`) |
| `DELETE /api/admin/policies/{name}` | Remove a policy |
| `GET /api/admin/rentals` | `?limit=` (default 50, at most 200). `{templates, instances, providers, caps}`: the templates from `infra/rental/templates`, rented machines newest first with `estimated_usd`, which providers are configured, and `caps` (`daily_cap_hours`, `used_today_hours`, `disabled`, `key_set`; `key_set` is always true in the current build, so do not rely on it) |
| `POST /api/admin/rentals` | Body `{template}`. Starts a machine: `201` with the instance; `400 template required`, `400` for a `kind: trainer` template (a fine-tune job starts those), `404 unknown template`, `409` if the template already has an open machine, the kill switch is on, the daily cap is reached, or no provider or rental key is set; `502` for a provider error; `503 rentals are not configured` |
| `POST /api/admin/rentals/{id}/stop` | Stops one machine; `200` with the instance |
| `POST /api/admin/rentals/stop-all` | Stops every open machine; `200 {"status":"ok"}` |
| `GET /api/admin/rentals/offers/{provider}` | The provider's GPU offers `{offers:[{gpu, memory_gb, hourly_usd, available}]}` (the Admin page does not call it yet) |
| `GET`, `PUT /api/admin/budgets` | List; set with `{scope, scope_id, period, limit_usd, on_exceed}`. `scope` is `global`, `user`, `agent` or `api_key`; `period` is `day`, `week`, `month` or `total`; `on_exceed` is `block` or `downgrade` (default); `limit_usd` must be above 0; `scope_id` must be a UUID for `api_key`, and for `user` or `agent` it may be empty: that row is then the **default** for every member or agent that has no row of its own for that period. `400` otherwise. The worker mails the people a budget concerns (the member, an agent's owner, the owners for `global` and `api_key`) once at 80 percent and once at the limit per window (`budget.warn`, hourly) |
| `GET /api/admin/spend?period=` | This period's spend by member and by agent against the budget each is under: `{period, since, total_usd, users: [{id, name, calls, usd, limit_usd, on_exceed, default, pct}], agents: [...]}`, sorted by spend, members and agents with no spend included; `period` is `day`, `week` or `month` (the default). `limit_usd` is 0 when nothing covers the row; `default` says the limit is the scope's default row. Spend on a member's own key is not counted, as the budgets do not count it. `400` for another period |
| `DELETE /api/admin/budgets/{id}` | Remove a budget. `200 {"status":"ok"}` even for an unknown id; `400` for a malformed one |

### Training (owner only, PLAN M10)

Datasets exported from opted-in users' conversations, fine-tune jobs and the adapters they make. `503 training is not configured` when the service is absent; starting a job needs a trainer runner (`WS_FINETUNE_IMAGE`, `WS_FINETUNE_URL` for a trainer box, or `WS_FINETUNE_TEMPLATE` for a rented machine), else `503`. Every route here needs the `owner` role, not ownership of the row: any owner sees, downloads and deletes every dataset and adapter.

| Route | Notes |
|---|---|
| `GET /api/training/datasets` | `{datasets}`, newest first (100) |
| `POST /api/training/datasets` | Body `{name, task_class?, filters: {modes?, models?, min_rating, since?, holdout_pct, max_examples?}}`. Records the dataset and queues `training.build`, which reads the conversations of users with `training_consent` (in the listed modes, active since the date, archived ones left out, at most `max_conversations` of them (default 2000), `max_prefix` earlier turns per example (default 40) and `max_examples` examples (default 5000)), keeps each assistant turn that passes the rating floor (`1` upvoted only, `0` upvoted or unrated, `-1` all; a rating by any user counts, and the highest one wins) and the model filter, writes one chat-format JSONL line per turn with the turns before it, splits conversations by a stable hash into train and held-out (`holdout_pct`), and stores both. Nothing is scrubbed: tool calls and their results go in verbatim, and only reasoning is dropped (images and files become placeholders). `201` with the row; `400` for a missing name or an unknown mode. A build that matches nothing fails with the reason on the row |
| `POST /api/training/datasets/upload` | A `multipart/form-data` form read as a stream, so the 32 MB JSON limit does not apply; `WS_TRAINING_UPLOAD_MAX_BYTES` does (default 4 GiB; `413` over it). The fields `name`, `task_class?`, `holdout_pct?` (0 to 50, default 10, 0 holds nothing out) and `mapping?` (a JSON object, below) come first and the `file` part last. The file is kept in the blob store as uploaded and `training.build` is queued; the row has `kind: "uploaded"`, `source_name`, `mapping` and `report`. Accepted files, by extension: `.jsonl`/`.ndjson` (a line with `messages` is a chat example taken as is, any other object is a record read through the mapping), `.csv`/`.tsv` (a header row, then records), `.txt`/`.md` (a document cut into chunks of `chunk_chars` about 3000 that continue each other: the first chunk answers "Write the document ...", the next ones "Continue the document ... It so far ends: ..." with the tail of the previous chunk), or a `.zip`, `.tar.gz`, `.tgz` or `.tar` of those (an archive may unpack to 8 times the limit; other files in it are skipped and counted). Mapping `{input?, output?, system?, system_text?, split?, chunk_chars?}`: `input` and `output` name a column or a JSON field (a dotted path reaches into objects) and are detected per file from common names (prompt, question, instruction ... and completion, answer, response ...) when empty; `system` names a per-record system prompt column and `system_text` is one for every record; `split` names a column holding `train` or `eval` (also test, val, validation, holdout) to pin a record, otherwise a stable hash of the input decides (`holdout_pct`). Records with an empty input or output are skipped and counted. At most 100000 examples per dataset. `400` for a missing name, an unreadable extension, an empty file or a bad mapping. Nothing is scrubbed. Images and audio are not read in this cut |
| `PUT /api/training/datasets/{id}/mapping` | Body is a mapping (above). Sets it on an uploaded dataset whose build has finished (`ready` or `failed`) and queues a rebuild from the stored upload; `400` otherwise, `404` for an unknown id. The row's `report` (set by every build, kept when it fails) lists the files read with examples and skipped counts, file types not read, the columns found, the mapping as applied, warnings (up to 8) and the train and held-out counts |
| `GET /api/training/datasets/{id}` | The row with `status`, `examples`, `eval_examples`, `bytes`, `error` |
| `GET /api/training/datasets/{id}/download?split=train|eval` | The JSONL as a download; `409` until the dataset is ready |
| `GET /api/training/datasets/{id}/preview?split=train|eval&n=5` | `{examples}`: the first `n` (default 5, at most 50) examples of the split, parsed (`{messages, meta}`); `409` until the dataset is ready or when the split has no file |
| `DELETE /api/training/datasets/{id}` | Removes the row only; the exported files stay in the blob store. A queued job whose dataset was deleted fails with "the dataset was deleted" |
| `GET /api/training/jobs` | `{jobs, runner, targets}`; `runner` says whether this deployment can fine-tune; `targets` lists where (`[{id: local|remote|rental, label}]`, the default first): the worker's Docker (`WS_FINETUNE_IMAGE`), a trainer box of yours (`WS_FINETUNE_URL`) and a rented trainer (`WS_FINETUNE_TEMPLATE`) |
| `GET /api/training/models?q=` | Base models the trainer can load, for the fine-tune form's picker: with `q`, a search of the Hugging Face hub through the server (text-generation models, most downloaded first, GGUF and MLX conversions and non-transformers libraries dropped, at most 20; `HF_TOKEN` when set lets gated and private models show), cached ten minutes per query; without `q`, a fixed list of suggested small instruct bases. `{models: [{id, downloads, likes, params?, gated}]}`; `502` when the hub does not answer |
| `POST /api/training/jobs` | **From a recipe (M12 cut c):** send `recipe_id` instead of `dataset_id` (never both: `400`). The job trains on the recipe's datasets merged under its spec: the spec is copied onto the job when it is queued (editing the recipe later does not change a queued or running job), and when the job starts it builds a dataset of kind `merged` from the recipe's ready datasets (an uploaded one is read again under the spec's mapping when that names other input or output columns than the dataset was built with; the dataset itself is not changed; the spec's system prompt becomes the first message of every example, replacing a system message already there), then trains on it as on any dataset. `base_model` and `base_endpoint_id` default to the recipe's; `config` fills from the request, then the recipe's `config`, then the spec's hyperparameters, then the usual defaults; `target` falls back to the recipe's. `400` when the recipe has no spec (flesh it out or write the system prompt or rubric) or no ready dataset. The finished adapter row carries `recipe_id`, `system_prompt` and `rubric`; its endpoint serves with the system prompt when a request brings none, and `POST .../evaluate` judges with the rubric and gives the base model the same system prompt. Otherwise: body `{dataset_id, base_model?, base_endpoint_id?, adapter_name, config?: {epochs, learning_rate, rank, alpha, max_seq_len, image, target}}`. `target` is `local`, `remote` or `rental` (one of `targets`; empty takes the default, which is stored on the job; `400` for one this deployment lacks). The dataset must be ready; `base_model` defaults to the base endpoint's model name when that is a Hugging Face id (`org/name`; `400` naming the model when it is not, as a llama-server name such as `gpt-oss-20b` is not one the trainer can load); the adapter name is lowercase letters, digits, dots, dashes and underscores. Queues `training.finetune`: the worker runs the trainer container on the dataset (or drives the trainer box, or rents a trainer machine, drives it and stops it), stores the adapter and registers an endpoint `lora/<name>` next to the base endpoint, disabled (for a base served by llama-server, the endpoint's `extra_body` asks for the server's first loaded adapter per request). `201` with the job; `400` for an invalid request; `503` without a runner |
| `GET /api/training/jobs/{id}` | The job with `status`, `progress`, `log` (tail), `adapter_id`, `error` |
| `POST /api/training/jobs/{id}/cancel` | Marks a queued or running job cancelled; the worker sees it within 15 s and stops the trainer container, or the rented machine |
| `GET /api/training/adapters` | `{adapters, smoke_eval_examples}` with `eval_score`, `baseline_score`, `eval`, `promoted`, `endpoint_id`, and `eval_job`: where the newest evaluation of the adapter stands, read from River's `river_job` table, `{state, error?, at}` with `state` `queued` (waiting, scheduled or retrying), `running`, `failed` (gave up or cancelled; `error` is the last one it recorded, cut to 600 characters) or `done`, or `null` before any. `smoke_eval_examples` (30, a constant of the server, not configuration) is the held-out size under which an eval is a smoke result: the Training page marks it, and `POST .../promote` adds "This is a smoke result: the eval had only N held-out examples, and one example moves the score by P points." to its refusal when the verdict rests on fewer examples than that |
| `GET /api/training/adapters/{id}/download` | The adapter as `<name>.tar.gz`, to place where the serving engine loads LoRA modules from |
| `POST /api/training/adapters/{id}/evaluate` | Queues `training.eval`: the held-out examples through the adapter's endpoint and through its base, each answer scored 0 to 10 by the judge model (`WS_TRAINING_JUDGE`) against the reference; the means land on the adapter (40 examples at most). The adapter's endpoint must answer (load the adapter on the server first). `202` once queued. The checks the worker makes run first, in the handler: `404` unknown adapter, `400` the adapter has no endpoint or base endpoint, no fine-tune job or its dataset is gone, `409` the dataset has no held-out examples or an evaluation of this adapter is already queued or running, each with the message the worker would have given. A check that only the run can make (an example the endpoint cannot answer, the judge failing) still fails in the worker; that shows as `eval_job.state` `failed` with its error, and in Admin, System, recent failures |
| `POST /api/training/adapters/{id}/promote` | Body `{force?, task_class?}`. Enables the adapter's endpoint, only when it was evaluated and scored at least its base unless `force`. With `task_class`, the adapter also goes first for that class in the `training-adapters` routing policy (the routes below). `409` otherwise; `400` for an adapter without an endpoint or a malformed class |
| `DELETE /api/training/adapters/{id}/promote` | Disables the endpoint again and takes its routes out of the policy |
| `DELETE /api/training/adapters/{id}` | Removes the adapter, its endpoint row, its routes and its file |
| `GET /api/training/recipes` | `{recipes}`, newest first (100). A recipe (M12, cut b) is a base model, the owner's instructions in plain words, the datasets attached to it and a spec a model writes from those. Row: `name`, `base_model`, `base_endpoint_id`, `instructions`, `dataset_ids`, `target`, `config` (a fine-tune config for a later cut), `spec`, `edited` (the spec keys the owner changed by hand), `spec_draft` (the model's last reply, untouched by edits), `spec_model` (the selector flesh out routes with, default `auto`), `spec_status` (`none`, `queued`, `running`, `ready`, `failed`), `spec_error`, `spec_used_model`, `spec_at`. The spec is `{system_prompt, mappings: [{dataset_id, input, output, system, split, note}], rubric, hyperparameters: {epochs, learning_rate, rank, max_seq_len}, data_notes}` |
| `POST /api/training/recipes` | Body `{name, base_model, base_endpoint_id?, instructions?, dataset_ids?, target?, config?, spec_model?}`. `201` with the row; `400` for a missing name or base model, instructions over 20000 characters, an unknown dataset, endpoint or target, or more than 20 datasets |
| `GET /api/training/recipes/{id}` | The row |
| `PUT /api/training/recipes/{id}` | The same body as create, replacing the owner's fields (not the spec); `404` for an unknown id |
| `PUT /api/training/recipes/{id}/spec` | Body: any of the five spec keys, each replaced whole. A key whose value changed is added to `edited`; a value equal to the stored one is not. `400` for an unknown key, a wrong type, or hyperparameters out of range (epochs 1 to 10, rank 1 to 256, max_seq_len 128 to 32768, learning_rate 1e-6 to 1e-2) |
| `POST /api/training/recipes/{id}/flesh-out` | Body `{model?, replace?}`. Queues `training.spec` and answers `202` with the row at `spec_status: queued`. `model` overrides the recipe's selector for this run (and is kept); `replace: true` drops the owner's edits when the draft lands, otherwise the edited keys are kept and only the others are rewritten. `400` without instructions, `409` while one is queued or running, `503` with no worker or gateway |
| `DELETE /api/training/recipes/{id}` | Removes the recipe. Datasets and adapters stay |
| `GET /api/training/spec-prompt` | `{prompt, schema}`: the system prompt and JSON schema flesh out sends, as written in `internal/training/spec/` |
| `GET /api/training/routes` | `{routes: [{task_class, endpoint}], policy}`: the task-class routes the `training-adapters` policy holds, newest first. The policy (priority 50, ahead of the default's 100) is rebuilt from this list on every change: one rule per class preferring the routed endpoints, newest first, then what the other policies prefer for the class, so the gateway still fails over. It shows in `GET /api/admin/policies` like any policy; its header says it is generated |
| `PUT /api/training/routes` | Body `{task_class, endpoint_id}`. Sends the class to the endpoint first: an adapter, or a frontier model whose answers a later dataset collects (distillation: build the dataset with `filters.models` naming it). `400` for an unknown endpoint or a class that is not lowercase letters, digits, dashes and underscores |
| `POST /api/training/routes/remove` | Body `{task_class?, endpoint_id}`. Takes the route out; without a class, every route to the endpoint. The policy is deleted with its last route |

### Claude Code jobs (owner only, network-gated)

The read-only job tab and the reports `wsj` sends it. These are the only `/api` routes an API key may call, and only one carrying the `jobs` scope. All four routes need the `owner` role **and** a client address inside `WS_CC_WEB_ALLOW` (by default the tailnet and loopback), otherwise `403 this route is only served on the tailnet`. The address checked is the TCP peer, unless the peer is inside `WS_TRUSTED_PROXIES`, in which case the client is taken from `X-Forwarded-For` or `X-Real-IP`; `True-Client-IP` is never used (see [Configuration](CONFIGURATION.md) and [Trust and security](TRUST.md)). A deployment that reaches ws through a proxy must set the variable or the gate sees the proxy's address. If the job tab is not configured on the host the routes answer `503`.

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
