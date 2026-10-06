# Data model

The Postgres schema as migrated. Source of truth is `internal/store/migrations/` (`0001_init`, `0002_agents`, `0003_endpoint_extra_body`, `0004_sandboxes`, `0005_cc_route_decisions`, `0006_usage_session`, `0007_cc_jobs`, `0008_cc_launch_counter`, `0009_media_jobs`, `0010_agents_m7`, `0011_agent_memory`, `0012_rental`, `0013_run_pause`, `0014_training`), applied by goose at boot; this page was written from those files and last verified at commit `9d28c2f` (migrations `0005` to `0008` are the only ones added since `bf89aee`). The schema PLAN.md sketches is larger than what exists; [Planned but not migrated](#planned-but-not-migrated) lists the difference.

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
| `users` | `email` (citext, unique), `display_name`, `role` (`owner` or `member`), `disabled_at`, `training_consent` (since `0014`: only consenting users' conversations are exported into datasets) |
| `invites` | Single-use invitations. `token_hash` (the raw token is never stored), `role`, `invited_by`, `expires_at`, `used_at`, `used_by` |
| `passkeys` | WebAuthn credentials: `credential_id`, `public_key`, `sign_count`, `transports`, `aaguid`, backup flags, `name`, `last_used_at` |
| `magic_links` | Email sign-in tokens: `token_hash`, `expires_at`, `used_at` |
| `sessions` | Browser sessions: `token_hash`, `expires_at`, `user_agent`, `ip`, `revoked_at` |
| `webauthn_sessions` | Short-lived server state between the begin and finish steps of a passkey ceremony: `kind` (`register` or `login`), `data`, `expires_at` |
| `api_keys` | Keys for `/v1`, `/mcp` and the `wsj` job reports: `prefix` (first 8 characters, for display), `key_hash`, `scopes` (any of `chat`, `mcp`, `jobs`; default `{chat}`), `default_policy` (default `auto`), optional `budget_id`, `last_used_at`, `revoked_at` |

Tokens and keys are stored only as hashes.

## Workspace

| Table | Purpose and notable columns |
|---|---|
| `projects` | `owner_id`, `name`, `kind` (`chat`, `code`, `design`, `images`; the API accepts any kind, but the web app creates only `chat` and `code`), `settings`, `repo_url`, `default_branch`, `github_installation_id`, `archived_at` |
| `project_members` | Sharing: `(project_id, user_id)` with `role` of `viewer`, `editor` or `admin` |

## Conversations

| Table | Purpose and notable columns |
|---|---|
| `conversations` | `project_id`, `user_id`, `title`, `mode` (`chat`, `code`, `design`, `images`, `agent`), `agent_id` (nullable FK to `agents`, added in `0002`), `model_selector` (default `auto`), `settings` (including `tool_policies`), `compaction_head_message_seq`, `archived_at` |
| `messages` | `conversation_id`, `seq` (unique per conversation), `role` (`system`, `user`, `assistant`, `tool`), `parts` (the canonical gateway `Part` array, provider-neutral), `endpoint_id`, `model`, `usage`, `finish_reason`, `parent_id` (branching) |
| `attachments` | Uploaded files in the blob store: `blob_key`, `mime`, `bytes`, `sha256`, `filename`, `width`, `height` |
| `message_attachments` | Join of messages to attachments |
| `compactions` | Summary blocks: `covers_through_seq`, `block` (`{goals, decisions, open_threads, file_state, tool_state, summary}`), `token_count`, `endpoint_id` |

Messages are never rewritten by compaction; a compaction row only records what a summary covers.

## Artifacts

| Table | Purpose and notable columns |
|---|---|
| `artifacts` | `conversation_id`, `kind` (`html`, `react`, `svg`, `markdown`, `mermaid`, `design`, `code`), `title`, `language`, `current_version` |
| `artifact_versions` | `(artifact_id, version)` unique; `content` or `blob_key`, `design_context`, `created_by_message_id` |

## Gateway

| Table | Purpose and notable columns |
|---|---|
| `providers` | `id` (slug), `kind` (`anthropic` or `openai_compat`), `base_url`, `api_key_env`, `headers`. Keys are never stored, only the environment variable name. A base URL taken from an environment variable is stored as `env:VARNAME` and resolved at load time; if unset, the provider is skipped. A provider that names a key variable which is empty is skipped too |
| `endpoints` | `id` (slug such as `anthropic/claude-sonnet-5-5`), `provider_id`, `model_name`, `display_name`, `capabilities`, `pricing`, `throughput_class`, `latency_class`, `is_local`, `enabled`, health columns maintained by the worker (`health_status`: `unknown`, `healthy`, `degraded`, `down`; `health_checked_at`, `health_error`, `p50_latency_ms`, and `error_rate`, which is reserved and always NULL today), and `extra_body` (added in `0003`, merged into every OpenAI-compatible request) |
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
| `agents` | Definitions for long-lived agents: `goal`, `system_prompt`, `model_policy`, `tool_allowlist`, `tool_policies`, `mcp_servers`, `memory_config` (`{disabled, k, top_n}`), `max_steps`, `enabled`, and from `0010` a nullable `project_id` (cascades; the project an agent's runs live in, index `agents_project_idx`) and `last_run_at`. Used by `internal/agents` and `/api/agents`; the Agents page does not edit `mcp_servers` or `memory_config` |
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

## Task router

| Table | Purpose and notable columns |
|---|---|
| `cc_launch_counter` | One row per calendar week (`week`, a timestamptz that is Monday 00:00 UTC, primary key) with a `count` and `updated_at` (`0008`). Incremented when a new `cc_jobs` row is created, in the week the job started; a re-report of a known job is not a launch. The router reads it for the weekly soft cap. It is an approximation: two reports of the same new job that race can both count, and a database error on the lookup is treated as "new" |
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

### Training flywheel (`0014`, PLAN M10)

| Table | Columns |
|---|---|
| `message_ratings` | `message_id`, `user_id` (primary key together: one rating per user per message), `score` (`1` or `-1`), `note`, `created_at`. Cascades with the message and the user |
| `datasets` | One export. `owner_id`, `name`, `task_class`, `filters` (`{modes, models, min_rating, since, holdout_pct, max_examples, max_conversations, max_prefix}`), `status` (`queued`, `building`, `ready`, `failed`), `blob_key` and `eval_blob_key` (the train and held-out JSONL in the blob store), `bytes`, `examples`, `eval_examples`, `error`, `created_at`, `built_at` |
| `finetune_jobs` | One trainer run. `owner_id`, `dataset_id` (set null on delete), `base_model` (what the trainer loads), `base_endpoint_id` (the ws endpoint the adapter is registered next to), `adapter_name`, `config` (`{epochs, learning_rate, rank, alpha, max_seq_len, image, target}`), `status` (`queued`, `running`, `done`, `failed`, `cancelled`), `progress`, `log` (the trainer's output tail), `adapter_id`, `error`, `created_at`, `started_at`, `ended_at` |
| `adapters` | A LoRA adapter. `name` (unique; the model name its endpoint sends), `base_model`, `base_endpoint_id`, `finetune_job_id` (set null on delete), `blob_key` and `bytes` (a gzipped tar of the trainer's output), `eval_score` and `baseline_score` (0 to 1, from the eval gate), `eval` (the gate's detail), `promoted`, `endpoint_id` (the `lora/<name>` endpoints row, `enabled` by promotion), `created_at`, `evaluated_at` |

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
