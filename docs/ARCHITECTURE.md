# Architecture

How ws is put together today. PLAN.md explains why and where it is heading; this page describes what the code does. Verified against `cmd/ws/main.go`, `internal/jobs`, `internal/agent`, `internal/chat`, `internal/compaction`, `internal/sandbox` and `internal/httpx`; last verified at commit `bf89aee`. Since then the Claude Code launcher and router, the MCP server and stdio client, and the image changed (`cmd/wsj`, `internal/ccjobs`, `internal/ccrouter`, `internal/mcpclient`, `internal/mcpserver`, `internal/media`, `internal/agents`, `internal/fleet/rental`, `internal/sandbox`, `infra/Dockerfile`, their wiring in `cmd/ws`, `internal/config`, `internal/chat` and `internal/store`), checked against `5e9f902`. The media, agents, jobs and training rows and the job table were re-checked against `9d28c2f`.

## One binary, two roles

`cmd/ws` builds the `ws` binary (the module also builds `wsj`, a separate session launcher; see [Differences from the plan](#differences-from-the-plan)). The same image runs in two roles that share packages, one query set and one migration set:

| Role | Command | Owns |
|---|---|---|
| `serve` | `ws serve` | Browser and API traffic: the HTTP API, the embedded web app, auth, the `/v1` external API, the artifact origin (second listener), sandbox preview proxy. Inserts jobs, never runs them |
| `worker` | `ws worker` | Everything that is slow, long-lived or privileged: River queues (agent runs, compaction, housekeeping), the Docker socket, sandboxes, the egress proxy, endpoint health checks, the internal API that `serve` relays to |

Other subcommands: `ws migrate [up|down|status]`, `ws health` (container healthcheck; the image is `debian:bookworm-slim` with an `ssh` client and no `curl`), `ws version`.

The two roles never talk to each other directly except for sandbox files and terminals. They coordinate through Postgres: River jobs for work, `LISTEN/NOTIFY` for live streaming, and periodic reloads for configuration.

```
 browser ──HTTPS──▶ serve ──────────────┐
 Claude Code, Cursor ─▶ /v1 (serve)     │ insert jobs, read/write rows
                          │             ▼
                          │        Postgres  (state, River queues, NOTIFY)
                          │             ▲
                          │ HTTP        │ claim jobs, write steps, NOTIFY
                          └────────▶ worker ──Docker API──▶ sandbox containers
                       (files, PTY)     │  ▲                      │
                                        │  └── egress proxy ◀─────┘ (only way out)
                                        ▼
                                  model endpoints (hosted APIs, llama-server, vLLM)
```

`serve` also calls model endpoints directly for chat turns that run in-process (see [A chat turn](#a-chat-turn)).

## Boot sequence

Both roles start the same way (`main`):

1. Load `.env` (the bootstrap: database URL, Infisical identity).
2. If Infisical is configured, log in and load the project's secrets into the environment. A failure is logged and boot continues on `.env`. A background refresh runs every five minutes.
3. Parse configuration from the environment (`internal/config`). Missing `WS_SESSION_SECRET` or `WS_DATABASE_URL` stops startup here.
4. Run the role.

Each role then connects to Postgres and applies migrations (`up`), so either can start first. `serve` additionally sets up auth, creates the owner invite on first boot if `WS_OWNER_EMAIL` has no user, and prints it to the log.

Both roles call the same `buildStack`, which wires, in order:

1. **Gateway.** Seed providers and endpoints from `config/endpoints.yaml` and policies from `config/policies/` into Postgres (upsert), load them into an in-memory registry and router, register the two protocol adapters, and add the budget middleware. A goroutine reloads the registry and policies from Postgres every 30 seconds, which is how edits made in Admin on one process reach the other, and how the worker's health writes reach `serve`.
2. **Blob store** on the filesystem (`WS_BLOB_DIR`).
3. **Tools.** Built-ins (`read_blob`, `web_fetch`, `ask_user`, artifact tools). On the worker only, if sandboxes are enabled and Docker is reachable: sandbox and git tools, and the runner that lets MCP command servers run inside a sandbox. If Docker is unavailable the worker logs it and runs without sandboxes.
4. **MCP.** Connect to the servers in `config/mcp.yaml` (retrying 45 seconds for sidecars) and register their tools as `mcp__<server>__<tool>`. URL servers connect from both roles; command (stdio) servers only on the worker with sandboxes, where a one-off probe in a throwaway container lists their tools and real calls go through `docker exec` into the project's sandbox. Then register `spawn_job` (`internal/ccrouter`) with its classifier, targets and owner check; it is bound to the runtime and the job client in steps 5 and 6, and is offered to chat conversations only when `WS_CC_DISPATCH=true`.
5. **Agent runtime** and the **compaction** middleware (added to the gateway after the budget middleware).
6. **River** (`internal/jobs`): migrate River's own schema, build the client. Only the worker starts the queues.

## Package map

| Package | Responsibility |
|---|---|
| `internal/gateway` | Provider-neutral request and stream types, the endpoint registry, the router (policies, aliases, capability filters), budget middleware, and the usage recorder hook. `adapter/anthropic` and `adapter/openaicompat` are the only code that knows a provider protocol |
| `internal/externalapi` | `/v1` handlers: translate OpenAI and Anthropic requests to canonical types and back, and proxy `/v1/messages` byte for byte to native Anthropic endpoints (`passthrough.go`) |
| `internal/agent` | The run loop (`runtime.go`), tool interface and registry (`tool.go`), built-in tools, output sinks, and the NOTIFY relay |
| `internal/chat` | One conversation turn: builds a run, picks in-process or worker execution, adapts events to the AI SDK stream |
| `internal/compaction` | Context-budget middleware and the background summarizer |
| `internal/jobs` | River client, job definitions (`agent.run`, `agent.tick`, `agent.reflect`, `conversation.compact`, `media.generate`, `rental.reconcile`, `training.build`, `training.finetune`, `training.eval`), the run reaper |
| `internal/agents` | Long-lived agents (PLAN M7): `Service` starts a run for an agent (a new `mode='agent'` conversation in the agent's project, one open run per agent), fires cron triggers (`Tick`) and webhook and GitHub (`repo_push`) deliveries (`FireHook`; `github.go` renders a GitHub event as text), steers, cancels, pauses and resumes runs (a pause is a status, `paused_manual`, that the run loop reads between steps); checks a GitHub delivery's `X-Hub-Signature-256` when it carries one; `Memory` embeds, recalls, searches, remembers and reflects (`memory.go`); `tools.go` holds the `remember` and `recall` agent tools (agent runs only); `presets.go` loads the starting points in `config/presets` (YAML, validated at boot) that the new-agent form copies, with no link back to the agent; `skills.go` (`ImportSkill`) turns one `SKILL.md` into a preset preview (hidden text removed, body labelled as imported, no tools, nothing saved). The routes are in `internal/httpx/handlers_agents.go` |
| `internal/training` | The training flywheel (PLAN M10): `Service` builds datasets from the conversations of users who opted in (`export.go`: chat-format JSONL, a stable-hash train and held-out split), runs fine-tune jobs through a `Runner` (`docker.go`: a trainer container on the worker; `rental.go`: a rented trainer machine), stores the adapter tarball in the blob store and registers a disabled `lora/<name>` endpoint, and scores an adapter against its base on the held-out set with a judge model (`eval.go`); promote enables the endpoint only when the adapter scored at least its base. The web process declares the fine-tune targets from the config (so the routes can accept a job) and only the worker builds the runners. Routes are in `internal/httpx/handlers_training.go`; `infra/training` holds the trainer image |
| `internal/fleet/rental` | The rental controller (PLAN M9 first cut): templates from YAML, a `Provider` interface with one implementation (RunPod over its REST API), `Start` (registers a provider row and a disabled endpoint, then a `rental_instances` row), `Reconcile` (moves a machine from provisioning to warming to ready, enables the endpoint, bills whole hours to the ledger, and stops on idle, max hours, the daily cap or the kill switch). Routes are in `internal/httpx/handlers_rental.go` |
| `internal/media` | The media gateway: the `Service` (create and validate a job against the routed endpoint, run it with failover, store outputs as attachments, price it, write the ledger row, abandon stuck jobs), five engines (`openai_images`, `openai_videos`, `fal`, `comfyui`, `google` for Imagen and Veo; shared HTTP plumbing in `http.go`) and the `generate_image` and `generate_video` agent tools. Jobs are `image`, `edit` (optionally with a mask), `upscale` and `video`; the Google engine takes only text to image and video (and image to video), not edits, masks or upscaling; a source image is an attachment the user owns or one a media job in the project produced, and `gateway.Request.Media` tells the router the exact need (`image_edit`, `image_to_video`, `upscale`) so endpoints that cannot take the job are skipped |
| `internal/sandbox` | Docker lifecycle, hardening, egress proxy, sandbox file and git tools, `docker exec` streams for MCP command servers (`mcpstdio.go`), the worker's internal API and the client `serve` uses to call it |
| `internal/github` | GitHub App auth and installation tokens |
| `internal/mcpclient` | MCP client and tool adapter: Streamable HTTP and SSE servers, and stdio servers over a `Runner` (`stdio.go`) |
| `internal/mcpserver` | ws as an MCP server at `/mcp` (`WS_MCP_SERVER`): the `ws_*` tools over projects, conversations, models, runs and the task router, for API-key callers |
| `internal/ccjobs` | The `wsj` launcher: targets file, local and ssh tmux hosts, job metadata, the startup check, `clean`, ssh error reporting, and the routing rules (`Extract`, `Route`, `ChooseTarget`). Used by `cmd/wsj` and by `internal/ccrouter` |
| `internal/ccweb` | The job tab's backend: the registry (job reports from `wsj` and the router, kills, a throttled refresh that lists every configured target's job windows and marks them alive, dead or gone, adopting unreported ones as source `tmux`) and the terminal bridge (a pty running `tmux attach -r` on a grouped session, through `ssh` for a remote target; only the window size goes back). Uses `creack/pty` |
| `internal/ccrouter` | The `spawn_job` tool on the agent runtime: decides a lane with the `ccjobs` rules, logs a `cc_route_decisions` row, and dispatches. Prompts the rules call ambiguous are first classified by a small model through the gateway (`WS_CC_CLASSIFY`; any failure keeps the rules' decision). The subscription lane goes through `ccjobs.Launcher`; other lanes as a new conversation plus a queued run under a gateway alias. Registered in `serve` and `worker` |
| `internal/secrets` | Infisical loader and refresher |
| `internal/artifacts` | Artifact storage, signed URLs, the separate-origin handler; design artifacts (PLAN M6 third cut, `design.go`): `ParseDesignContext` validates a design system (library, colors, type, spacing, radius, components, notes; at most 16 KiB) stored on `artifact_versions.design_context`, the conversation's design system is rendered into the system prompt, `Variants` makes up to four alternatives in parallel through the gateway (each a model call billed to the caller, task class `chat`), and the export route downloads a version as an attachment with an extension for its kind (never rendered on the app origin) |
| `internal/auth` | Passkeys, magic links, invites, sessions, API keys, mailers |
| `internal/httpx` | chi router, handlers, middleware, preview proxy, `aistream` (AI SDK stream writer) |
| `internal/store` | sqlc-generated queries, goose migrations, glue between rows and gateway types, `blob` |
| `internal/config` | Environment parsing |
| `web` | The Vite and React app, embedded into the binary at build time |

## Core flows

### A chat turn

`POST /api/conversations/{id}/chat` on `serve` calls `chat.Service`, which creates an `agent_runs` row (every turn is a run) and then chooses where it executes:

- **Chat-mode conversations run in `serve`.** The runtime drives the run in-process and the model stream is written straight to the browser.
- **Code-mode conversations run on the worker**, because only the worker has sandbox tools and the Docker socket. `serve` enqueues an `agent.run` job and then subscribes to the run's Postgres channel, relaying events to the browser as they arrive.

The run loop (`internal/agent/runtime.go`) repeats: reconcile any tool calls left pending by a previous process or an approval pause; build the request from the conversation's messages; send it through the gateway; persist the assistant message and an `agent_steps` row; execute requested tools according to their policy; loop until the model stops, the step limit is reached, or a tool needs approval. A run can also be held by hand (`POST /api/runs/{id}/pause`): the loop reads the run's status at the top of each step, so a pause takes effect between steps, never during a model call or a tool, and a run held this way still counts as open for steering and for the one-open-run rule.

Tool policies are `auto`, `ask` or `deny`, defaulted per tool and overridable per conversation. `ask` records an approval and pauses the run with status `paused_approval`. The browser decides it through `POST /api/approvals/{id}` or by echoing the assistant message back to the chat endpoint with the decision; `serve` then re-queues the run for the worker once no approvals are pending. Large tool output is written to the blob store and replaced by a stub, which the model can page through with `read_blob`.

### Streaming across processes

For worker-run turns the worker writes events through a `NotifySink` using `pg_notify` on a channel named from the run id; payloads stay under Postgres's 8000-byte limit. `serve` starts listening before it enqueues the job, so the first event can't be missed, and relays each event to the browser. If the worker dies after its last event but before the final notify, `serve` notices by checking the run's status rather than waiting forever. The browser sees the same stream either way.

### Surviving restarts

While it owns a run, a process updates a heartbeat every 20 seconds. A River periodic job, `agent.reap`, runs every minute (and at start): it finds runs whose owner stopped heartbeating, and runs that were queued but never picked up, and enqueues them for the worker. Resuming reconciles from the stored steps and messages, so a killed worker continues from the last checkpoint. Jobs are unique per run, so a run is never driven twice at once.

### The gateway pipeline

Every model call, from chat, agents, compaction and the external API alike, goes through the gateway: `gateway.Stream` is `Prepare` (steps 1 to 3) followed by `StreamCandidates` (steps 4 and 5), and the external API calls the two halves itself:

1. **Budget** middleware checks the caller's budgets against the ledger (and can downgrade to local endpoints).
2. **Compaction** middleware trims the outgoing request to `WS_COMPACTION_BUDGET_TOKENS`. It substitutes the latest stored summary block for the turns it covers, then drops the oldest turns if still over, and schedules a fresh summary. It never modifies stored messages. External API calls skip it.
3. The **router** resolves a selector (endpoint id, alias or `auto`) and the task class against the policies and capability requirements into an ordered candidate list.
4. The **adapter** for the chosen provider's protocol streams the response. On retryable errors before the first token, the gateway fails over to the next candidate.
5. The **usage recorder** writes a `usage_ledger` row with tokens, cost and the routing decision.

### The external API

`/v1/chat/completions`, `/v1/messages` and `/v1/models` authenticate with a `ws_` API key and go through the same pipeline minus compaction, tagged as external so policies can match on it. `/v1/chat/completions` always translates to canonical types. `/v1/messages` routes first with `gateway.Prepare` (middlewares and policy), then branches on the chosen endpoint: a native Anthropic provider gets the request proxied byte for byte (only `model` rewritten, the provider's key swapped in, the response relayed unchanged and read as it passes to write the ledger row through `gateway.Record`, so budgets see it); any other endpoint takes the translate path through `StreamCandidates`. The client's `x-claude-code-session-id` is stored on the ledger row. See [API.md](API.md).

### Sandboxes

A code project gets one container per user, created by the worker on first tool use, with a persistent `/workspace` volume. The worker is the only process with the Docker socket. The browser never talks to it: file and terminal requests go browser → `serve` → the worker's internal API, authenticated by a header derived from `WS_SESSION_SECRET`.

Sandbox containers sit on an internal Docker network whose only route out is the worker's **egress proxy**. The proxy enforces a host allowlist and tunnels HTTPS (`CONNECT`) opaquely. It injects a fresh repo-scoped GitHub App token only on plain-HTTP requests to `github.com`, which it rewrites to https (the sandbox's git is configured to use `http://github.com`), so credentials exist only in the worker. A periodic job, `sandbox.reap` (every five minutes), stops idle containers and removes old ones.

Dev-server previews are served by `serve` on their own hostnames. The app redirects the browser to the preview host with a short-lived token, which becomes a host-only cookie; the proxy forwards to the container and strips the app cookie.

### Artifacts

Artifacts are stored by the app but served by a second listener on a different origin (`WS_ARTIFACT_URL`). The app embeds them in a sandboxed iframe (`allow-scripts` only, never `allow-same-origin`) using a signed URL (24 hour default TTL), and the artifact origin sends a CSP that blocks network access.

## Background jobs

River runs in Postgres; `serve` inserts, `worker` executes. Queues: `agents` and `housekeeping`.

| Job kind | Queue | Trigger | Does |
|---|---|---|---|
| `agent.run` | agents | A code-mode turn, an approval decision, or the reaper | Drives a run to completion or pause; 25 minute timeout; unique per run |
| `conversation.compact` | housekeeping | Compaction middleware when the request is still over budget after the stored block is applied | Writes a summary block with a cheap model; at most one per conversation per 2 minutes |
| `agent.reap` | housekeeping | Every minute, and at start | Requeues abandoned or never-started runs |
| `agent.tick` | housekeeping | Every minute, and at start (when agents are enabled) | Fires due cron triggers: up to 50 per tick, each starts a run unless the agent already has an open one (then the firing is skipped and `last_error` says so); 2 minute timeout, unique per minute |
| `agent.reflect` | housekeeping | After an agent's run ends (done, or failed other than a pause) | Asks a model (task class `reflect`) to extract facts, preferences and an episode from the run's transcript and stores them with embeddings; 5 minute timeout, two attempts, unique per run |
| `rental.reconcile` | housekeeping | Every minute, and at start (when the rental controller exists) | Advances every open rented machine and stops the ones past a limit; 3 minute timeout, unique per minute, one pass at a time |
| `training.build` | training | `POST /api/training/datasets` | Reads opted-in conversations, writes the train and held-out JSONL to the blob store; 30 minute timeout; one worker on the `training` queue, one attempt |
| `training.finetune` | training | `POST /api/training/jobs` | Runs the trainer (container or rented machine) on a dataset and stores the adapter; 8 hour timeout |
| `training.eval` | training | `POST /api/training/adapters/{id}/evaluate` | Scores the adapter and its base on the held-out examples with the judge model; 2 hour timeout; a failure shows only in the worker's log, not on the adapter |
| `media.generate` | media | `POST /api/projects/{id}/media` or the `generate_image` and `generate_video` tools | Runs one media job: routes through the gateway so budgets apply, tries candidates in order, stores outputs as attachments, writes the ledger row; 25 minute timeout, one attempt (a failure is recorded on the row), two workers, unique per job |
| `sandbox.reap` | housekeeping | Every five minutes, and at start (worker with Docker) | Stops and removes idle sandboxes |

The worker also runs an endpoint health-check loop outside River.

## Where state lives

| State | Location |
|---|---|
| Users, projects, conversations, messages, runs, steps, approvals, ledger, endpoints, policies, budgets, sandboxes, River queues | Postgres |
| Attachments, externalized tool output | Blob directory (`WS_BLOB_DIR`, the `blobs` volume) |
| Workspace files | Per-sandbox Docker volume |
| Provider and GitHub credentials | Environment (loaded from `.env` or Infisical); the database stores only the variable names |
| Live run events | Postgres `NOTIFY`, replayed from persisted rows |

## Security boundaries

- **Artifacts** run on a separate origin in a sandboxed iframe with no network.
- **Sandboxes** are unprivileged containers on an internal network with one egress path; secrets are injected outside the container.
- **The Docker socket** is mounted only into the worker.
- **The worker's internal API** is not published. Compose publishes ports only on loopback by default (Postgres, the inference containers, and with `override.cloud.yaml` the app); see the Compose guide.
- **Previews** are reachable only with a cookie obtained through the app.
- **Session cookies** are HttpOnly and SameSite=Lax, and cookie-authenticated writes are checked for cross-site origin.

## Differences from the plan

PLAN.md describes things that are not in the code yet: the rest of design (Sandpack for React, chromedp screenshots, TSX export, uploaded component libraries) (M6); the rental providers beyond RunPod and the tailnet sidecar (M9); and the real-client acceptance runs of M5 (not done).

The plan no longer includes a subscription passthrough, a billing router, or running `claude -p` inside sandboxes; ws never forwards or stores a subscription credential. Claude Code on a subscription is launched by a separate CLI, `wsj` (`cmd/wsj`, `internal/ccjobs`), which starts the official `claude` in tmux on local or ssh targets and never reads its output. The task router (`internal/ccrouter`, the `spawn_job` tool) is built into `serve` and `worker`: it decides a lane with deterministic rules, then (when the rules are unsure) a small-model classifier, then (for the subscription lane) a weekly soft cap read from `cc_launch_counter` through the `ccweb` registry, and logs the decision, and launches real jobs only when `WS_CC_DISPATCH=true` (off by default; the subscription lane is owner-only and needs `tmux`, `ssh` and a targets file that the `ws` image does not have). The read-only job tab is built (`internal/ccweb`, the `/api/jobs/cc` routes and the Jobs page): `wsj` reports each launch and kill to `serve`, a refresh over the launcher (at most every 10 seconds, only while the tab is open) lists the targets' job windows over ssh to keep liveness current, and the browser's terminal is a WebSocket to a pty running `tmux attach -r` on a grouped session of its own. The routes are owner-only and limited to `WS_CC_WEB_ALLOW` (the tailnet by default). On a Compose deployment the `ws` image has no `tmux` or `ssh`, so refreshes report each target unlistable and the terminal shows that reason; the job list from `wsj` reports still works. The classifier for ambiguous prompts is built (`WS_CC_CLASSIFY`, on by default). The module therefore builds a second binary, `wsj`, next to `ws`. The spec is CLAUDE_CODE_JOBS.md.

The plan's middleware chain lists cache hints as a stage. In the code, hints are set as `CacheHint` marks on message parts (by the agent runtime and the compaction block), and adapters that support prompt caching act on them. Check `internal/gateway` before relying on exact ordering.
