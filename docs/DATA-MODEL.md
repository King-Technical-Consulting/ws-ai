# Data model

The Postgres schema as migrated. Source of truth is `internal/store/migrations/` (`0001_init`, `0002_agents`, `0003_endpoint_extra_body`, `0004_sandboxes`, `0005_cc_route_decisions`, `0006_usage_session`, `0007_cc_jobs`, `0008_cc_launch_counter`, `0009_media_jobs`, `0010_agents_m7`, `0011_agent_memory`, `0012_rental`, `0013_run_pause`, `0014_training`, `0015_user_provider_keys`, `0016_byok_routing`, `0017_endpoint_throughput`, `0018_uploaded_datasets`, `0020_budgets_m7`, `0021_user_memory`, `0022_documents`, `0023_scheduled_prompts`, `0026_notebooks`, `0027_favorites`, `0028_endpoint_tier`, `0029_training_recipes`, `0031_train_from_recipe`, `0032_server_settings`, `0033_agent_skills`, `0034_session_stepup`), applied by goose at boot; this page was written from those files and last verified at commit `e4e8b6f` (2026-10-10; the newest migration is `0033_agent_skills` (checked against its SQL file); the numbers `0019`, `0024`, `0025` and `0030` are not used, and a migration number is no longer reserved for anyone (board `b7fe`): check `internal/store/migrations/` before taking one; the tables of `0018`, `0020`, `0021`, `0022`, `0023`, `0026`, `0027`, `0028`, `0029`, `0031` and `0032` were checked against their SQL files). The schema PLAN.md sketches is larger than what exists; [Planned but not migrated](#planned-but-not-migrated) lists the difference.

Conventions: UUID primary keys from `gen_random_uuid()` (except `usage_ledger` and `sandbox_events`, which use `bigserial`, `providers` and `endpoints`, which use `text` slug ids, and join tables, which use composite keys), `timestamptz` timestamps, `jsonb` for flexible payloads, and `ON DELETE CASCADE` from owning rows. Tables with `updated_at` get a `set_updated_at` trigger, except `sandboxes`, whose queries set `updated_at` explicitly. Extensions: `pgcrypto`, `citext`, and `vector` (pgvector, enabled by `0011`; the pool registers its types at connect). River adds its own tables through its own migration (`jobs.Migrate`), not listed here.

Queries live in `internal/store/queries/*.sql` and are compiled to Go by sqlc (`make sqlc`).

## Overview

```
users ─┬─ passkeys, magic_links, sessions, invites, api_keys
       │
       └─ projects ─┬─ project_members
                    ├─ sandboxes ── sandbox_events
                    └─ conversations ─┬─ messages ── attachments (via message_attachments)
                                      ├─ compactions
                                      ├─ artifacts ── artifact_versions
                                      └─ agent_runs ─┬─ agent_steps
                                                     └─ approvals
agents ── agent_triggers        (agent_runs.agent_id is NULL for plain chat turns)
cc_route_decisions              (task router log; optional links to users, conversations, agent_runs)
providers ── endpoints          routing_policies   budgets   usage_ledger
```

## Identity

| Table | Purpose and notable columns |
|---|---|
| `users` | `email` (citext, unique), `display_name`, `role` (`owner` or `member`), `disabled_at` (set by `PUT /api/admin/users/{id}/disabled`; sessions, API keys and sign-ins are refused while set), `training_consent` (since `0014`: only consenting users' conversations are exported into datasets) |
| `invites` | Single-use invitations. `token_hash` (the raw token is never stored), `role`, `invited_by`, `expires_at`, `used_at`, `used_by` |
| `passkeys` | WebAuthn credentials: `credential_id`, `public_key`, `sign_count`, `transports`, `aaguid`, backup flags, `name`, `last_used_at` |
| `magic_links` | Email sign-in tokens: `token_hash`, `expires_at`, `used_at` |
| `sessions` | Browser sessions: `token_hash`, `expires_at`, `user_agent`, `ip` (the address the server trusted for the request, never a header the caller chose), `revoked_at`, and since `0034` `stepped_up_at` (M5 step-up: when a passkey assertion was last made on this session, null if never; set at creation by a passkey login, by `POST /api/auth/stepup/finish` otherwise; a gated action from away passes while it is inside `WS_STEPUP_WINDOW`); `webauthn_sessions.kind` also allows `stepup`, the ceremony of that assertion) |
| `webauthn_sessions` | Short-lived server state between the begin and finish steps of a passkey ceremony: `kind` (`register` or `login`), `data`, `expires_at` |
| `api_keys` | Keys for `/v1`, `/mcp` and the `wsj` job reports: `prefix` (first 8 characters, for display), `key_hash`, `scopes` (any of `chat`, `mcp`, `jobs`; default `{chat}`), `default_policy` (default `auto`), optional `budget_id`, `last_used_at`, `revoked_at` |

Tokens and keys are stored only as hashes.

## Workspace

| Table | Purpose and notable columns |
|---|---|
| `projects` | `owner_id`, `name`, `kind` (`chat`, `code`, `design`, `images`; the API accepts any kind; the web app creates `chat`, `code` and `design` projects), `settings`, `repo_url`, `default_branch`, `github_installation_id`, `archived_at` |
| `project_members` | Sharing: `(project_id, user_id)` with `role` of `viewer`, `editor` or `admin` |

## Conversations

| Table | Purpose and notable columns |
|---|---|
| `conversations` | `project_id`, `user_id`, `title`, `mode` (`chat`, `code`, `design`, `images`, `agent`), `agent_id` (nullable FK to `agents`, added in `0002`), `model_selector` (default `auto`), `settings` (including `tool_policies` and, in a design conversation, `design_context`; the PATCH does not validate it, and a bad one is silently ignored when the prompt is built), `compaction_head_message_seq`, `archived_at` |
| `messages` | `conversation_id`, `seq` (unique per conversation), `role` (`system`, `user`, `assistant`, `tool`), `parts` (the canonical gateway `Part` array, provider-neutral), `endpoint_id`, `model`, `usage`, `finish_reason`, `parent_id` (branching) |
| `attachments` | Uploaded files in the blob store: `blob_key`, `mime`, `bytes`, `sha256`, `filename`, `width`, `height` |
| `message_attachments` | Join of messages to attachments |
| `compactions` | Summary blocks: `covers_through_seq`, `block` (`{goals, decisions, open_threads, file_state, tool_state, summary}`), `token_count`, `endpoint_id` |

Messages are never rewritten by compaction; a compaction row only records what a summary covers.

## Artifacts

| Table | Purpose and notable columns |
|---|---|
| `artifacts` | `conversation_id`, `kind` (`html`, `svg`, `markdown`, `mermaid`, `design`, `code`; `react` is planned and not accepted yet), `title`, `language`, `current_version` |
| `artifact_versions` | `(artifact_id, version)` unique; `content` or `blob_key`, `design_context`, `created_by_message_id` |

## Gateway

| Table | Purpose and notable columns |
|---|---|
| `providers` | `id` (slug), `kind` (`anthropic` or `openai_compat`), `base_url`, `api_key_env`, `headers`. Keys are never stored, only the environment variable name. A base URL taken from an environment variable is stored as `env:VARNAME` and resolved at load time; if unset, the provider is skipped. A provider that names a key variable which is empty is skipped too |
| `endpoints` | `id` (slug such as `anthropic/claude-sonnet-5-5`), `provider_id`, `model_name`, `display_name`, `capabilities`, `pricing`, `throughput_class`, `latency_class`, `tier` (`''`, `small`, `medium`, `large`; `''` means read off the model name; `0028`), `routable` (default true; false keeps general routing and the fallbacks off it; `0028`), `is_local`, `enabled`, health columns maintained by the worker (`health_status`: `unknown`, `healthy`, `degraded`, `down`; `health_checked_at`, `health_error`, `p50_latency_ms`, and `error_rate`, which is reserved and always NULL today), and `extra_body` (added in `0003`, merged into every OpenAI-compatible request) |
| `routing_policies` | `name` (unique), `yaml`, `priority`, `enabled`. Edited in Admin; also seeded from `config/policies/` |
| `budgets` | `scope` (`user`, `agent`, `api_key`, `global`), `scope_id`, `period` (`day`, `week`, `month`, `total`), `limit_usd`, `on_exceed` (`block` or `downgrade` to local endpoints). Unique per `(scope, scope_id, period)`, which Postgres does not enforce for `global` budgets because their `scope_id` is NULL (a code gap, not a doc one) |
| `usage_ledger` | One row per model call, `bigserial` id. User, conversation, agent, run and API key ids (stored without foreign keys so history survives deletions, except `user_id` and `conversation_id` which are set null), `endpoint_id`, `model`, `task_class`, `policy_name`, `decision` (routing decision as JSON), token counts including cache read and write, `cost_usd`, `latency_ms`, `ttft_ms`, `finish_reason`, `error`, and `session_id` (text, nullable, from the client's `x-claude-code-session-id` header; set only by the external API; added in `0006`) |

Endpoint health is written by the worker and read by both roles through the periodic registry reload; see [ARCHITECTURE.md](ARCHITECTURE.md).

## Agent runtime

Every chat turn is a run, so these tables are in use, and since PLAN M7 first cut user-defined agents run on them too.

| Table | Purpose and notable columns |
|---|---|
| `agent_runs` | `conversation_id`, `agent_id` (null for plain chat turns), `user_id`, `trigger_id`, `status` (`queued`, `running`, `paused_approval`, `paused_steer`, `paused_manual` since `0013`: held from the monitor, `done`, `failed`, `cancelled`), `request` (the request skeleton without messages), `tool_policies` (resolved for the run), `max_steps`, `step_count`, `first_message_id` (the assistant message the UI streams into), `cost_usd`, `error`, `heartbeat_at` and `owner_pid` (used by the reaper), `started_at`, `ended_at` |
| `agent_steps` | `(run_id, seq)` unique; `kind` (`llm`, `tool`, `approval`, `compaction`), `input`, `output`, `checkpoint`, `usage`, `error`, timestamps |
| `approvals` | A pending tool call: `run_id`, `step_seq`, `tool_call_id`, `tool_name`, `args`, `status` (`pending`, `approved`, `denied`, `expired`), `decided_by`, `decided_at`, `note` |
| `agents` | Definitions for long-lived agents: `goal`, `system_prompt`, `model_policy`, `tool_allowlist`, `tool_policies`, `mcp_servers`, `memory_config` (`{disabled, k, top_n}`), `max_steps`, `max_run_usd` (`0020`; a cap on one run's spend, 0 is none, copied onto `agent_runs.max_run_usd` when a run starts), `enabled`, and from `0010` a nullable `project_id` (cascades; the project an agent's runs live in, index `agents_project_idx`) and `last_run_at`, and, from `0033` to `0035`, a `skills` column that `0035` moved to the shelf below. Used by `internal/agents` and `/api/agents`; the Agents page does not edit `mcp_servers` or `memory_config` |
| `agent_triggers` | `kind` (`cron`, `webhook`, `repo_push`, `manual`; `cron`, `webhook`, `manual` and `repo_push` (a GitHub webhook) can be created), `spec`, `secret_hash` (sha256 of a webhook's secret, shown once at creation), `enabled`, and from `0010` `name`, `next_run_at`, `last_run_at` and `last_error`. Partial index `agent_triggers_due_idx(next_run_at)` where `enabled and kind = 'cron'` |

## Sandboxes

| Table | Purpose and notable columns |
|---|---|
| `sandboxes` | One per `(project_id, user_id)`: `container_id`, `volume_name`, `image`, `runtime` (default `runc`), `status` (`created`, `running`, `stopped`, `failed`), `error`, `last_used_at`. The container is disposable; the volume holds the user's working copy |
| `sandbox_events` | Append-only log: `kind` (created, started, stopped, removed, exec, error, clone), `detail` |

## Agent memory

| Table | Purpose and notable columns |
|---|---|
| `agent_memory` | What an agent remembers across runs (`0011`). `agent_id` (cascades), `kind` (`fact`, `episode`, `preference`), `content` (at most 1000 characters), `content_hash` (sha256 of the lowercased content; `UNIQUE (agent_id, content_hash)` so a repeat only raises `importance` and fills a missing embedding), `embedding vector(768)` (nullable: a run without an embedding endpoint stores none, and a vector of another width is stored as NULL), `source_run_id` (set null on delete), `importance` (0 to 1, default 0.5), `last_used_at`, `created_at`. Indexes: `(agent_id, created_at desc)`, `(agent_id, importance desc)` and an HNSW index on `embedding` with cosine distance |

### Personal memory (`0021`, PLAN M13)

| Table | Purpose |
|---|---|
| `user_memory` | What ws keeps about one member between chats. `user_id` (cascades), `kind` (`fact`, `preference`, `decision`), `content` (at most 500 characters), `content_hash` (sha256 of the lowercased content; `UNIQUE (user_id, content_hash)` so a repeat only raises `importance`), `embedding vector(768)` (nullable, as `agent_memory`), `source_conversation_id` (set null on delete), `source` (`chat`, `reflection`, `manual`), `importance` (0 to 1; reflection multiplies unused ones by 0.8 down to a floor of 0.05, never deleting), `last_used_at`, `created_at`. Indexes: `(user_id, created_at desc)`, `(user_id, importance desc)` and an HNSW index on `embedding`. A separate table from `agent_memory`, which is unchanged |
| `memory_settings` | One row per member who has touched the page (no row means memory on, reflection off): `enabled`, `reflect_enabled`, `reflect_at` (HH:MM) and `timezone` (IANA) for the nightly job, `profile` (jsonb, the personal-context card: `name`, `address_as`, `role`, `projects[]`, `tools[]`, `tone`, `this_week`), `last_reflected_at`, `updated_at` |

### Documents (`0022`, PLAN M13)

| Table | Purpose |
|---|---|
| `documents` | One row per uploaded document that was read, keyed by `attachment_id` (the original stays in the blob store through `attachments`; cascades). `user_id`, `conversation_id` (set null on delete), `kind` (`pdf`, `docx`, `xlsx`, `pptx`, `text`), `method` (`text` for the text layer, `ocr` for the OCR container), `pages`, `chars`, `truncated` (the page limit cut it short), `created_at` |
| `document_chunks` | The extracted text in reading order: `attachment_id` (cascades), `seq`, `page` (a page, sheet or slide number; a chunk never spans two), `content` (about 1,200 characters), `embedding vector(768)` (nullable), `UNIQUE (attachment_id, seq)`. Indexes: `(attachment_id, page, seq)` and an HNSW index on `embedding` |

### Scheduled prompts (`0023`, PLAN M13)

| Table | Purpose |
|---|---|
| `scheduled_prompts` | A member's prompt that runs on its own. `user_id` and `project_id` (cascade), `title`, `prompt`, `model_selector`, `kind` (`once`, `cron`), `cron_expr` (5 fields, evaluated in `timezone`), `run_at` (for `once`), `enabled`, `allow_web_fetch` (the only tool a scheduled run can have), `notify_email`, `reuse_conversation` with `conversation_id` (set null on delete), `next_run_at` (NULL: nothing left to run; the claim moves it before a run starts), `last_run_at`, `last_status` and `last_error`. Index on `next_run_at` for enabled rows |
| `scheduled_prompt_runs` | One row per firing: `prompt_id` (cascades), `run_id` (the `agent_runs` row, set null on delete), `conversation_id`, `status` (`queued`, `running`, `done`, `failed`, `cancelled`, `skipped`), `error`, `notified`, `started_at`, `ended_at`. Open rows are settled by the minute tick from the run's own status |

### Research notebooks (`0026`, PLAN M13)

| Table | Purpose |
|---|---|
| `notebooks` | A member's notebook: `user_id` (cascades), `name`, `conversation_id` (its chat, made on first use; set null on delete), `created_at`, `updated_at`. The chat is an ordinary conversation whose `settings` carry `{"notebook_id": ...}`; deleting the notebook leaves it |
| `notebook_sources` | One source: `notebook_id` (cascades), `kind` (`document`, `page`, `text`, `conversation`), `title`, `url` (pages), `attachment_id` (the uploaded original; set null on delete), `from_conversation_id` (set null on delete), `pages`, `chars` |
| `notebook_chunks` | A source's text in reading order: `source_id` (cascades), `seq`, `page`, `content` (about 1,200 characters, never across two pages), `embedding vector(768)` (nullable), `UNIQUE (source_id, seq)`; HNSW index on `embedding` |
| `notebook_notes` | `notebook_id` (cascades), `kind` (`summary`, `open_questions`, `note`), `title`, `body`, `author` (`user`, `model`), timestamps. A partial unique index allows one summary and one open-questions note per notebook; any number of notes |

### Favorites and saved prompts (`0027`, PLAN M13)

| Table | Purpose |
|---|---|
| `favorites` | A starred item: `user_id` (cascades), `kind` (`message`, `artifact`, `media`), `ref_id` (the target's id; polymorphic, so no foreign key), `title` and `snippet` (taken when it was starred), `conversation_id` (set null on delete) and `project_id` (cascades) for opening it. `UNIQUE (user_id, kind, ref_id)`. Listing drops rows whose target is gone or whose project the member can no longer reach |
| `saved_prompts` | A member's reusable text: `user_id` (cascades), `name`, `text`, timestamps; offered in the composer's slash list |

### Skill shelf (`0035`, OpenClaw plan step 2)

| Table | What it holds |
|---|---|
| `skills` | A member's approved skill folders, kept once: `owner_id`, `name` (slug, unique per member), `title`, `description`, `instructions` (the cleaned SKILL.md body), `has_scripts`, `files` (jsonb inventory: path, kind, size, sha256, interpreter, `refused`, findings), `refused_secrets` (front matter keys that named credentials, keys only), `findings`, `warnings`, `approved_by`, `approved_at`. The file bytes are in the blob store under `sha256/<hex>`, not in the row; a file that held a literal credential is listed and was never stored. Keeping a skill grants no tool, secret or network |
| `agent_skills` | `(agent_id, skill_id)`, both cascading: an agent's choice from the shelf. Deleting a skill takes it off every agent; deleting an agent leaves the shelf alone |

`0035` also moved the skills approved on agents under `0033` (`agents.skills`) onto their owner's shelf, attached to the same agents, and dropped that column (its down migration rebuilds it).

### Server settings (`0032`)

| Table | Purpose |
|---|---|
| `server_settings` | Server-wide settings the owner edits in Admin: `key` (primary key), `value` (jsonb), `updated_at`. Today one key, `route_classifier_max_usd_day`, the daily ceiling on verdict-routing classifier spend in US dollars; an absent key means the default from `WS_ROUTE_CLASSIFIER_MAX_USD_DAY` |

### Permanent deletion (no migration)

Archiving sets `archived_at` on a conversation or project and nothing else. `internal/purge` deletes an archived one for good. Most of it is the schema's own `ON DELETE CASCADE` (messages, `message_attachments`, compactions, artifacts and versions, agent runs and steps and approvals; for a project also its conversations, agents and their triggers, sandboxes, scheduled prompts, media jobs and project-scoped favorites). The rest is done explicitly, in one transaction: the usage ledger's `agent_run_id` (a plain column) is cleared and its `conversation_id` goes to NULL by its foreign key, so spend history stays; favorites that point at the conversation are deleted; `user_memory.source_conversation_id`, `notebook_sources.from_conversation_id`, `notebooks.conversation_id`, `documents.conversation_id` and `media_jobs.conversation_id` go to NULL, so a kept memory or a notebook source's text survives its chat. An `attachments` row (with its `documents` row and chunks) goes only when no message of a conversation that stays, no media job that stays and no notebook source uses it, and its blob file goes only when no `attachments`, `artifact_versions`, `datasets` or `adapters` row still names the same key (blobs are content-addressed, so two rows can share one). Files are removed after the commit and checked to be gone; one that cannot be removed is counted as failed, not hidden.

## Task router

| Table | Purpose and notable columns |
|---|---|
| `cc_launch_counter` | One row per calendar week (`week`, a timestamptz that is Monday 00:00 UTC, primary key) with a `count` and `updated_at` (`0008`). Incremented when a new `cc_jobs` row is created, in the week the job started; a re-report of a known job is not a launch. The router reads it for the weekly soft cap. The upsert of the `cc_jobs` row says in the same statement whether it inserted (`xmax = 0`), so two reports of the same new job that race count it once, and a database error never counts as a launch |
| `cc_jobs` | The handle of each Claude Code job, for the read-only job tab (`0007`): where a tmux window lives, never what is in it. `id` (text, the tmux handle id), `user_id` (set null on delete), `target`, `session`, `window`, `cwd`, `lane`, `model`, `source` (`wsj`, `router` or `tmux`, the last for a window a refresh found that nobody reported), `status` (`alive`, `dead`, `killed`, `gone`, `unknown`), `started_at`, `seen_at`, `ended_at`, timestamps. tmux stays authoritative for liveness: `status` is what the last refresh saw. No prompt, output or credential is stored. Indexed by `started_at DESC` and, for open jobs, by `target` |
| `cc_route_decisions` | One row per `spawn_job` call, dry run or not (`0005`). `user_id`, `conversation_id` and `run_id` are nullable foreign keys that set NULL on delete. `prompt_sha256` (the prompt text is never stored), `features` (jsonb: the counts and flags the rules saw, plus a `classification` object when the small-model classifier answered), `lane` (checked against `claude-subscription`, `api`, `openrouter`, `local`), `rule`, `reason`, `target`, `model`, `dry_run`, `job_id` (the tmux handle id), `created_at`. Indexed by `created_at DESC` and by `(lane, created_at DESC)` |

## Media

| Table | Purpose and notable columns |
|---|---|
| `media_jobs` | One row per media generation (`0009`), written by `internal/media` and driven by the River job `media.generate`. `user_id` and `project_id` cascade on delete; `conversation_id` sets NULL. `kind` (`image`, `video`, `edit`, `upscale`; all four run), `selector` (the picker's choice: endpoint id, alias or `auto`), `endpoint_id` (set when the job runs), `inputs` (jsonb: `prompt`, `size`, `n`, `quality`, `seconds`, `aspect`, `source_attachment_id`, `mask_attachment_id`, `scale`, `estimate_usd`), `provider_job_id`, `status` (`queued`, `running`, `done`, `failed`, `cancelled`), `progress`, `output_attachment_ids` (uuid array into `attachments`), `cost_usd`, `error`, `created_at`, `started_at`, `ended_at`, `updated_at` (trigger). Indexes on `(project_id, created_at desc)`, `(user_id, created_at desc)` and a partial one on open jobs. Each finished job also writes a `usage_ledger` row under task class `image` or `video` |

## Rented GPUs

| Table | Purpose and notable columns |
|---|---|
| `rental_instances` | One row per rented machine (`0012`). `provider` (`runpod`; the column comment also lists `lambda` and `vast`), `template`, `gpu`, `provider_instance_id`, `endpoint_id` (the endpoint registered while it runs), `base_url`, `hourly_usd`, `status` (`provisioning`, `warming`, `ready`, `stopping`, `stopped`, `failed`), `started_by` (set null on delete), `started_at`, `ready_at`, `last_request_at`, `stopped_at`, `hours_used`, `billed_hours` (hours already written to the ledger), `stop_reason`, `error`. Indexes on `started_at` and a partial one on open machines. Each billed hour is a `usage_ledger` row with task class `rental` |

### Bring your own key (`0015`)

| Table | Notes |
|---|---|
| `endpoint_throughput` | `0017`. One row per endpoint id: `tokens_per_sec`, `ttft_ms`, `samples`, `updated_at`, what the gateway has measured of an endpoint's decode speed (M8). Written after every counted reply and loaded at boot, so a restart ranks by the last measurement instead of the declared `throughput_class`; a loaded row older than 15 minutes is a prior, not a trusted measurement: it stands in for the declared class when it is higher, and is replaced by fresh replies. No foreign key: endpoint ids come and go with the seed. |
| `users.shared_provider_keys`, `usage_ledger.own_key` | `0016`. The first lets the owner grant a member the server's shared provider keys (default false; the owner always has them). The second marks a call that ran on the person's own key; the budget sums skip those rows. |
| `user_provider_keys` | One row per (`user_id`, `provider_id`): `sealed` (AES-256-GCM under `WS_SECRETS_KEY`, bound to the user and provider), `last4`, timestamps. Cascades on user or provider delete. The key is never selectable through the API. |

### Training flywheel (`0014`, PLAN M10)

| Table | Columns |
|---|---|
| `message_ratings` | `message_id`, `user_id` (primary key together: one rating per user per message), `score` (`1` or `-1`), `note`, `created_at`. Cascades with the message and the user |
| (`0031`) | Training from a recipe (M12 cut c). `datasets.kind` also allows `merged` (the dataset a recipe job builds from the recipe's datasets). `finetune_jobs.recipe_id` (set null on delete) and `finetune_jobs.spec` (jsonb: the recipe's spec as it was when the job was queued). `adapters.recipe_id`, `adapters.system_prompt` and `adapters.rubric` (what the adapter was trained under; the endpoint's default system prompt is stored in its `extra_body` under `ws_default_system` and lifted out when the registry loads, so it is never sent to a provider) |
| `training_recipes` | `0029`. A recipe (M12 cut b): `owner_id`, `name`, `base_model`, `base_endpoint_id`, `instructions`, `dataset_ids` (uuid array; no foreign key, a deleted dataset is skipped when read), `target`, `config` (jsonb, for the fine-tune cut), `spec` (jsonb: `system_prompt`, `mappings`, `rubric`, `hyperparameters`, `data_notes`), `edited` (text array: the spec keys the owner changed by hand), `spec_draft` (the model's last reply as parsed), `spec_model` (selector, default `auto`), `spec_status` (`none`, `queued`, `running`, `ready`, `failed`), `spec_error`, `spec_used_model`, `spec_at`, `created_at`, `updated_at` |
| `datasets` | One export. `owner_id`, `name`, `task_class`, `filters` (`{modes, models, min_rating, since, holdout_pct, max_examples, max_conversations, max_prefix}`), `status` (`queued`, `building`, `ready`, `failed`), `blob_key` and `eval_blob_key` (the train and held-out JSONL in the blob store), `bytes`, `examples`, `eval_examples`, `error`, `created_at`, `built_at`; since `0018`: `kind` (`chat`, the default, built from conversations, or `uploaded`), `mapping` (jsonb: how an upload's records become examples), `source_blob_key` and `source_name` (the uploaded file as received, in the blob store, and its name) and `report` (jsonb: what the last build found; see `PUT /api/training/datasets/{id}/mapping` in API.md). For an uploaded dataset `bytes` is the upload's size until the build sets it to the train file's |
| `finetune_jobs` | One trainer run. `owner_id`, `dataset_id` (set null on delete), `base_model` (what the trainer loads), `base_endpoint_id` (the ws endpoint the adapter is registered next to), `adapter_name`, `config` (`{epochs, learning_rate, rank, alpha, max_seq_len, image, target}`), `status` (`queued`, `running`, `done`, `failed`, `cancelled`), `progress`, `log` (the trainer's output tail), `adapter_id`, `error`, `created_at`, `started_at`, `ended_at` |
| `adapters` | A LoRA adapter. `name` (unique; the model name its endpoint sends), `base_model`, `base_endpoint_id`, `finetune_job_id` (set null on delete), `blob_key` and `bytes` (a gzipped tar of the trainer's output), `eval_score` and `baseline_score` (0 to 1, from the eval gate), `eval` (the gate's detail), `promoted`, `endpoint_id` (the `lora/<name>` endpoints row, `enabled` by promotion), `created_at`, `evaluated_at` |

### Budgets, M7 third cut (`0020`)

`budgets` (from `0001`) holds one row per `(scope, scope_id, period)`; since `0020` the unique constraint is `NULLS NOT DISTINCT`, so a row with a NULL `scope_id` is unique too: for `global` that is the one shared budget, and for `user` or `agent` it is the **default** that applies to every member or agent without a row of its own for that period (`internal/store/budgets.go` falls back to it; the admin spend overview and the agent page show it as `default`). The migration folds any duplicate NULL rows into the newest before rebuilding the constraint. `budget_warnings` records the warning mails: `scope`, `scope_id`, `period`, `pct` (80 or 100), `spent_usd`, `limit_usd`, `sent_at`, unique on the first four (NULLs not distinct); the hourly `budget.warn` job sends again only when the row is older than the period's window.

## Indexes worth knowing

- `usage_ledger`: by `(user_id, created_at DESC)`, `(endpoint_id, created_at DESC)`, partial on `agent_id`, and partial on `(session_id, created_at DESC)` where `session_id` is not null.
- `agent_runs`: partial index on `(status, heartbeat_at)` for `queued` and `running`, which the reaper scans; `(conversation_id, created_at DESC)` for the runs list.
- `messages`: unique `(conversation_id, seq)`. Inserts must allocate the next `seq` per conversation.
- `compactions`: `(conversation_id, covers_through_seq DESC)` to find the latest block.

## Planned but not migrated

From PLAN.md, not in any migration yet (the billing-router tables `subscription_quota` and `user_credentials` were dropped from the plan and will not be added):

| Planned | Milestone |
|---|---|
| `rental_templates` (templates are YAML files, not a table) | M9 |

Differences between the plan and the current schema, so nobody writes queries from PLAN.md by mistake:

- PLAN shows `conversations.compaction_head_message_id`; the column is `compaction_head_message_seq`.
- PLAN describes message `parts` as AI SDK UIMessage parts; they are canonical gateway parts, converted at the edge.
- PLAN names `artifacts.current_version_id`; the column is `current_version` (an integer).
- PLAN names a ledger column `policy_id`; the ledger stores `policy_name`.
- A comment in `0001_init.sql` says the `conversations.agent_id` foreign key is added in "0003"; it is added in `0002_agents.sql`.
