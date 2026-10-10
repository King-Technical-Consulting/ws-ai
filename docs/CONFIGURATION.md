# Configuration reference

Everything an operator can set: environment variables, and the three YAML files in `config/`. Verified against `internal/config/config.go`, `internal/secrets/infisical.go`, `internal/mcpclient/mcpclient.go`, `internal/ccrouter`, `internal/ccjobs`, `cmd/ws/main.go`, `infra/Dockerfile`, the Makefile, the compose overrides, `internal/gateway/router.go`, `internal/gateway/types.go`, `.env.example` and `infra/compose/compose.yaml`, last verified at commit `9341046`.

Both `ws serve` and `ws worker` read the same environment. A `.env` file in the working directory is loaded if present (real environment wins over `.env`). With Infisical configured, secrets from the project are also loaded into the environment before parsing; see [Secrets from Infisical](#secrets-from-infisical).

Two variables are required: `WS_SESSION_SECRET` (at least 32 bytes) and `WS_DATABASE_URL`. Startup fails without them.

## Core

| Variable | Default | Purpose |
|---|---|---|
| `WS_ENV` | `dev` | `dev` or `prod`. Outside `dev`, session cookies are marked Secure, so `prod` needs HTTPS |
| `WS_LISTEN` | `:8080` | App listener (API, web UI, preview proxy) |
| `WS_ARTIFACT_LISTEN` | `:8081` | Artifact origin listener |
| `WS_PUBLIC_URL` | `http://localhost:8080` | Public origin of the app. Also the default WebAuthn origin |
| `WS_ARTIFACT_URL` | `http://localhost:8081` | Public origin for artifacts. Must be a different origin from the app |
| `WS_PREVIEW_DOMAIN` | `preview.localhost` | Parent domain for sandbox previews, used to derive `{port}-{id}.<domain>` |
| `WS_SESSION_SECRET` | none, required | HMAC key for session cookies and, via derivation, the app-to-worker shared secret. 32 bytes minimum |
| `WS_DATABASE_URL` | none, required | Postgres connection string |
| `WS_BLOB_DIR` | `./data/blobs` | Blob store directory (attachments, externalized tool output, artifact snapshots) |
| `WS_OWNER_EMAIL` | empty | The first user to register with this email becomes owner; an invite is created and logged at first boot |
| `WS_LOG` | `info` | `debug`, `warn`, anything else is `info`. Read before `.env` and Infisical are loaded, so set it in the real process environment (for example Compose `environment:`), not in `.env` |
| `WS_COMPACTION_BUDGET_TOKENS` | `60000` | Ceiling on tokens sent per request before older turns are replaced by a summary. Per request the budget is the smaller of this and the largest context window among the endpoints the request's model or alias can route to, less the reply reserve (`max_tokens`, else 4096), so a conversation bound for a 32k local slot is compacted to fit it and this number only matters for larger models |

## Email

| Variable | Default | Purpose |
|---|---|---|
| `RESEND_API_KEY` | empty | Resend key. Without it, invite and magic-link emails are printed to the log |
| `WS_EMAIL_FROM` | `ws@localhost` | Sender address; the domain must be verified in Resend |

## WebAuthn (passkeys)

| Variable | Default | Purpose |
|---|---|---|
| `WS_RP_ID` | host of `WS_PUBLIC_URL` | Relying party id. Passkeys are bound to it; changing it invalidates existing passkeys |
| `WS_RP_NAME` | `ws` | Display name shown by the authenticator |
| `WS_RP_ORIGINS` | `[WS_PUBLIC_URL]` | Comma-separated allowed origins |

## Model providers

Hosted providers are declared in `config/endpoints.yaml` and read their key from the variable named there. A provider whose key variable is unset still loads, without a key: its models are listed, only people who saved their own key for it under Settings can use it, the owner's shared-key grant does not reach it, and the health probe leaves it alone.

| Variable | Used by |
|---|---|
| `ANTHROPIC_API_KEY` | `anthropic` provider |
| `OPENAI_API_KEY` | `openai` provider; also enables image generation through the seeded `openai/gpt-image-1` endpoint (and the disabled `openai/sora-2` video endpoint) |
| `FAL_KEY` | `fal` provider (fal.ai media models: `fal/flux-dev`, `fal/flux-kontext`, `fal/clarity-upscaler`, the Wan video models) |
| `GEMINI_API_KEY` | `google` provider (Imagen through `google/imagen-4`, and the disabled `google/veo-3` video endpoint), sent as `x-goog-api-key` |
| `COMFYUI_URL` | `comfyui` provider: base URL of a ComfyUI server. The provider and its `comfyui/sdxl` endpoint are skipped when it is unset |
| `WS_COMFY_WORKFLOWS` | Directory of ComfyUI workflow templates (default `infra/comfyui/workflows`; the container image ships them at `/app/infra/comfyui/workflows` and sets this) |
| `WS_FINETUNE_IMAGE` | Trainer container for fine-tune jobs (`infra/training` builds one). Set it on **both** processes: serve declares the `local` target from it (the routes that accept a job run there), the worker, which needs Docker and a GPU, runs the container. Unset everywhere, fine-tune jobs are refused with `503`; datasets, ratings and adapters still work |
| `WS_FINETUNE_GPUS` | GPUs the trainer container gets: `all` (default), a count, or empty for none |
| `WS_FINETUNE_URL` | Base URL of a trainer box you run: the trainer image in serve mode (`TRAINER_MODE=serve`), or `serve.py` by itself around `train_mlx.py` on a Mac, reached over the tailnet. The target for a ws whose worker has no GPU (the worker on a small box, the trainer on the Orin, the Spark or the Mac). Set it on both processes (serve declares the `remote` target, the worker drives the box); a job posted while the box is busy fails with its `409` |
| `WS_FINETUNE_KEY` | The bearer key that box requires (its `TRAINER_API_KEY`); worker only |
| `WS_FINETUNE_TEMPLATE` | Name of a `kind: trainer` rental template (`infra/rental/templates`, `trainer-a100` ships) a fine-tune job can run on instead of the worker's Docker; needs the template's provider key (`RUNPOD_API_KEY`) and `WS_RENTAL_API_KEY`. Set it on both processes, like `WS_FINETUNE_IMAGE` (serve declares the `rental` target, the worker rents and drives the machine). With several set, the job's `config.target` (`local`, `remote` or `rental`) picks; the default is the first that exists in that order |
| `WS_TRAINING_DIR` | Directory a fine-tune job's files sit in while it runs, bind-mounted into the trainer (default `data/training`) |
| `WS_TRAINING_JUDGE` | Model selector that scores eval answers in the eval gate, under task class `classify` (default `auto`) |
| `HF_TOKEN` | Passed to the trainer container for gated base models, and sent by the serve process to the Hugging Face hub when the fine-tune form searches for a base (so gated and private models show); never stored |
| `OPENROUTER_API_KEY` | `openrouter` provider |
| `CEREBRAS_API_KEY` | `cerebras` provider |
| `LLAMA_SERVER_URL` | `local-llama` base URL, for example `http://llama:8000/v1`; on a box without a GPU, another box's llama over the tailnet, `http://<its tailnet address>:8000/v1` |
| `LLAMA_BIND_ADDR` | Compose only (the GPU overrides), default `127.0.0.1`: the host address llama-server (and the Orin's embedding server) is published on. Set it to the box's tailnet address so a ws on another box can route to it; llama-server has no auth of its own, so never a public address |
| `LLAMA_EXTRA_ARGS` | Compose only (the llama-server overrides: Orin, AMD, CPU), default empty: extra flags appended to the llama-server command. How a trained adapter is served: `--lora-init-without-apply --lora /models/lora/<name>-lora.gguf` loads the GGUF LoRA a fine-tune job wrote (put it under `data/models/lora/`) without applying it to the base model; the adapter's `lora/<name>` endpoint asks for it per request (`extra_body.lora`), so the base endpoint stays the base |
| `LLAMA_EMBED_URL` | `local-llama-embed` base URL for the local embedding endpoint, for example `http://llama-embed:8001/v1` (the Orin override sets it; unset, `local-llama/nomic-embed-text` is skipped) |
| `VLLM_URL` | `local-vllm` base URL |
| `MAC_STUDIO_URL` | `mac-studio` base URL |
| `WS_ENDPOINTS_FILE` | Path to the endpoints seed file. Default `config/endpoints.yaml` |
| `WS_POLICIES_DIR` | Directory of routing policy YAML. Default `config/policies` |
| `WS_PRESETS_DIR` | Directory of agent presets (YAML), the starting points the new-agent form offers. Default `config/presets`; four ship: `general`, `code`, `research`, `quick`. A preset names a `title`, `description`, `goal`, `prompt`, `model` (`selector`, `task_class`, `reasoning`), `tools` (required: `[]` for none, `['*']` for every tool, else an allowlist), `max_steps` and a `note`. A bad file is logged and the directory skipped; an unknown tool name is logged and kept |
| `WS_MCP_FILE` | MCP server list. Default `config/mcp.yaml` |
| `WS_MCP_SERVER` | `true` | Serve ws itself as an MCP server at `/mcp` (Streamable HTTP, stateless, ws API key with the `mcp` scope as bearer). `false` removes the route. See [API.md](API.md) |

## Coding sandboxes (worker)

The worker is the only process with the Docker socket. `serve` relays sandbox file and terminal requests to the worker's internal API.

| Variable | Default | Purpose |
|---|---|---|
| `WS_SANDBOX_ENABLED` | `true` | Turn sandboxes off entirely. Read by the worker only |
| `WS_SANDBOX_IMAGE` | `ghcr.io/king-technical-consulting/ws-sandbox:latest` | Image for sandbox containers |
| `WS_SANDBOX_NETWORK` | `ws_sandbox-net` | Internal Docker network; the Compose-created name. `none` disables |
| `WS_SANDBOX_PROXY_URL` | `http://worker:3128` | Egress proxy address as seen from inside a sandbox. `none` disables |
| `WS_SANDBOX_RUNTIME` | `auto` | `auto`, `runc`, or `runsc` (gVisor, used by `auto` when installed) |
| `WS_SANDBOX_JAIL` | `false` | Run the `bash` tool's commands inside bubblewrap (read-only container filesystem; writable only `/workspace`, `/home/dev`, `/tmp`; private PID, IPC and UTS namespaces). Needs user namespaces in the container, which Docker's default seccomp profile blocks, so turn it on only after checking that `bwrap` runs in a sandbox container; with it on and `bwrap` failing, every `bash` call fails. On an Ubuntu host `kernel.apparmor_restrict_unprivileged_userns=1` also blocks `bwrap` in the sandbox image: one test run (board `f8b2`) needed both seccomp and AppArmor relaxed, and relaxing seccomp alone was not enough. Read by the worker only |
| `WS_SANDBOX_MEMORY_MB` | `4096` | Memory limit per sandbox |
| `WS_SANDBOX_CPUS` | `2` | CPU limit per sandbox |
| `WS_SANDBOX_IDLE_STOP` | `15m` | Stop a container after this long idle. Go duration syntax; an invalid value becomes 0 |
| `WS_SANDBOX_REMOVE_AFTER` | `168h` | Remove a container after this long idle; the volume stays. Go duration syntax; an invalid value becomes 0 |
| `WS_WORKER_URL` | `http://worker:8082` | Where `serve` reaches the worker's internal API. `none` or empty disables the sandbox relay and previews |
| `WS_INTERNAL_LISTEN` | `:8082` | The worker's internal API listener |
| `WS_PREVIEW_HOST` | derived | Preview hostname pattern with `{port}` and `{id}`. Empty derives `{port}-{id}.<WS_PREVIEW_DOMAIN>`. Behind a single-level wildcard cert use something like `ws-p-{port}-{id}.home.arpa` |
| `WS_EGRESS_LISTEN` | `:3128` | Egress proxy listener |
| `WS_EGRESS_ALLOW` | empty | Extra allowlisted hosts, comma-separated, added to the built-in list (`DefaultAllow` in `internal/sandbox/egress_proxy.go`). The `WS_PUBLIC_URL` host is always added |

### GitHub

Both `serve` and `worker` read these (`serve` for the Admin install view), but only the worker's egress proxy injects them; they never enter a sandbox.

| Variable | Purpose |
|---|---|
| `GITHUB_APP_ID` | GitHub App id. Enables installation tokens, `open_pr`, and the Admin install view |
| `GITHUB_APP_PRIVATE_KEY` | App private key, inline. PEM with `\n` escapes, or base64 |
| `GITHUB_APP_PRIVATE_KEY_FILE` | Path to the key instead, for example `/secrets/github-app.pem` (Compose mounts `./secrets` read-only at `/secrets`) |
| `GITHUB_APP_SLUG` | App slug, for the Admin install link |
| `GITHUB_WEBHOOK_SECRET` | The App's webhook secret (set the same value on the App's webhook, payload URL `<public url>/hooks/github`). With it, one webhook on the App serves every agent's `repo_push` trigger that names a repository, no per-repository webhook needed; without it the route answers 404 |
| `GITHUB_TOKEN` | Fallback personal token when no App is configured. `open_pr` is unavailable in this mode |

## Secrets from Infisical

Read by `internal/secrets`. With the first three set, both processes log in with the machine identity at boot, load every secret in the project environment into their environment before configuration is parsed, and refresh every five minutes.

| Variable | Default | Purpose |
|---|---|---|
| `INFISICAL_CLIENT_ID` | none | Machine identity client id. Required to enable |
| `INFISICAL_CLIENT_SECRET` | none | Machine identity secret. Required to enable |
| `INFISICAL_PROJECT_ID` | none | Project to read. Required to enable |
| `INFISICAL_ENV` | `prod` | Environment slug |
| `INFISICAL_PATH` | `/` | Secret path |
| `INFISICAL_SITE_URL` | `https://app.infisical.com` | Set for a self-hosted Infisical |
| `INFISICAL_OVERWRITE` | unset | `1` makes Infisical values replace anything already in the environment at boot. By default existing values win at boot, but the five-minute refresh always overwrites, so a `.env` value for a name Infisical also holds is replaced after about five minutes |

If Infisical is unreachable at boot, ws logs it and continues on `.env`.

## Deployment (Compose)

These are read by Compose and the Makefile rather than by the Go binary. See [../infra/compose/README.md](../infra/compose/README.md).

| Variable | Default | Purpose |
|---|---|---|
| `POSTGRES_PASSWORD` | `ws` | Password for the Compose Postgres. Change it in any non-dev setup |
| `WS_IMAGE` | `ghcr.io/king-technical-consulting/ws:latest` | App and worker image. Pin to a commit SHA to stop image updates |
| `WS_BIND_ADDR` | loopback | Host binding for `override.cloud.yaml` or LAN testing |
| `WS_HTTP_PORT`, `WS_ART_PORT` | `8080`, `8081` | Host ports for the same |
| `WS_HOST`, `WS_ART_HOST` | `ws.home.arpa`, `ws-art.home.arpa` | Hostnames for `override.traefik.yaml` |
| `WS_PREVIEW_PARENT` | `home.arpa` | Parent domain for the Traefik preview router |
| `WS_DEPLOY_HW`, `WS_DEPLOY_INGRESS` | none | Written by `make install-cd`; default `HW` and `INGRESS` for `make` targets |
| `WS_DEPLOY_PROFILES` | none | Compose profiles to enable, for example `infisical` |
| `DOCKER_GID` | `999` | Host docker group id for the worker. `make up` derives it from the socket |
| `CLOUDFLARE_TUNNEL_TOKEN` | none | Token for the `cloudflare` profile |
| `TS_AUTHKEY` | none | Tagged auth key for the `tailscale` profile |
| `HF_TOKEN` | none | Hugging Face token for vLLM model downloads, and (through a rental template's `env`) for rented machines |
| `VLLM_API_KEY` | empty | API key passed to the vLLM container by `override.spark-cuda.yaml` |
| `TRAEFIK_NETWORK` | `traefik-net` | External network name used by `override.traefik.yaml` |

Compose sets `WS_DATABASE_URL`, `WS_BLOB_DIR`, `WS_LISTEN`, `WS_ARTIFACT_LISTEN`, `WS_ENDPOINTS_FILE` and `WS_POLICIES_DIR` for the app and worker (`compose.yaml`), and the image bakes in `WS_ENDPOINTS_FILE`, `WS_POLICIES_DIR`, `WS_BLOB_DIR`, `HOME=/home/nonroot` and `XDG_CACHE_HOME=/tmp/ws-cache` (where the ssh multiplexing sockets go), so inside the containers these differ from the defaults above (for example blobs live in `/data/blobs`). `INFISICAL_SITE_URL` is also read by the `mcp-infisical` sidecar.

### Infisical MCP sidecar

For the `infisical` Compose profile (`WS_DEPLOY_PROFILES=infisical`). These configure the `mcp-infisical` container, not the app.

| Variable | Default | Purpose |
|---|---|---|
| `INFISICAL_MCP_CLIENT_ID`, `INFISICAL_MCP_CLIENT_SECRET` | none | A second, least-privilege machine identity for the agent's `mcp__infisical__*` tools |
| `INFISICAL_MCP_MASK` | `true` | Mask secret values in tool output |
| `INFISICAL_MCP_TOOLS` | `list-projects,list-secrets,get-secret,create-secret,update-secret` | Tools the sidecar exposes |

## Rented GPUs (`internal/fleet/rental`)

An owner can start a rented GPU machine from Admin; it appears as a model endpoint while it runs. Only RunPod exists so far. See ../infra/rental/README.md for the templates.

| Variable | Default | Purpose |
|---|---|---|
| `WS_RENTAL_TEMPLATES` | `infra/rental/templates` | Directory of machine templates (YAML). Loaded at boot; a bad file is logged and skipped |
| `RUNPOD_API_KEY` | none | RunPod API key. Without it the `runpod` provider is not registered and no machine can start |
| `WS_RENTAL_API_KEY` | none | The key the rented machine's vLLM is started with and ws then calls it with. Read from the environment (not the config struct); starting fails if it is empty. A template's `args` and `env` may use `$VAR` and `${VAR}`, expanded from the ws environment when the machine starts (an unset variable becomes empty). Only the variable name is stored in the database |
| `WS_RENTAL_DAILY_CAP_HOURS` | `8` | Machine-hours per day; reaching it stops machines and refuses new starts. `0` means no cap |
| `WS_RENTAL_DISABLED` | `false` | Kill switch: refuses starts and stops open machines |

A template names the provider, GPU, image, model, port and disk, the endpoint ws registers (its `id` and capabilities), and its limits: `idle_timeout` (default 20m, measured from the last ledger row for the endpoint, or the last touch by a fine-tune job driving a trainer), `max_hours` (default 6), `warmup_timeout` (default 25m) and `hourly_usd`. Three ship: `h100-gpt-oss-120b` and `h100-qwen3-coder-next`, each one H100 with vLLM, and `trainer-a100`, a `kind: trainer` template that registers no endpoint and is started only by a fine-tune job (`WS_FINETUNE_TEMPLATE`). A machine stops on idle, `max_hours`, the daily cap, the kill switch, a failed warmup, Admin, or the provider reporting it gone. Each whole instance-hour is written to the usage ledger (task class `rental`) at the template's hourly price, and the remainder when it stops. Not verified here: a real RunPod start, and Lambda, Vast.ai and the tailnet sidecar are not built.

## Claude Code task router (`spawn_job`)

Read by `internal/config`; both `serve` and `worker` read them. The router decides which lane a task should run on and, when dispatch is on, starts it. See CLAUDE_CODE_JOBS.md for the design.

| Variable | Default | Purpose |
|---|---|---|
| `WS_SECRETS_KEY` | _(empty)_ | Base64 of 32 random bytes (`openssl rand -base64 32`). Seals people's own provider API keys at rest (AES-256-GCM). Unset, saving a key is refused (`503`) and Settings says so; a key that cannot be read is logged at boot and treated as unset. Losing or changing it makes every saved key unreadable (people save theirs again); it is not a password and is never shown. A member's own keys only work for providers this server has a key for in its env (a provider with no key is not loaded). |
| `WS_TRUSTED_PROXIES` | _(empty)_ | Networks (comma-separated CIDRs or single addresses) whose `X-Forwarded-For` and `X-Real-IP` headers are believed. Empty believes none: the client address is the TCP peer, which is what the request log, the session's recorded address and the `WS_CC_WEB_ALLOW` gate see. Name only the ingress in front of `app` (for the Compose `traefik-net` network, its subnet), never a broad range that also holds the sandbox network: a sandbox could then forge its address. For a trusted peer the client is the rightmost forwarded address that is not itself trusted. Before this variable existed ws believed those headers from any caller, so a deployment that relies on a proxy's forwarded address (for example `wsj` reporting through Traefik) must set it, or the gate sees the proxy's address |
| `WS_CC_WEB_ALLOW` | `100.64.0.0/10,127.0.0.0/8,::1/128` | Which client networks may use the job tab routes (`/api/jobs/cc`): comma-separated CIDRs or single addresses; `*` turns the check off; a list that cannot be parsed refuses everyone. The address is the TCP peer, or, when the peer is inside `WS_TRUSTED_PROXIES`, the client that proxy forwarded (see that variable). The routes also need the owner role |
| `WS_CC_WEEKLY_CAP` | `0` | A soft cap on subscription launches per calendar week (Monday 00:00 UTC), counted locally in `cc_launch_counter`; Claude's own usage is never read, so it is a count of the launches ws knows of, not of tokens or hours. `0` counts without ever changing a route. A launch is counted once, when ws first learns of the job, in the week the job started, whoever reported it (`wsj run`, the router, or a refresh that finds a window nobody reported). `wsj` launches are counted but never refused: the cap only steers the router |
| `WS_CC_WEEKLY_SOFT` | `75` | The percent of the cap from which the router keeps low-value work off the subscription lane (see the notes below). A value outside 1 to 100 is read as 75 |
| `WS_CC_DISPATCH` | `false` | While `false`, every `spawn_job` call only decides and logs a `cc_route_decisions` row, and chat conversations are not offered the tool. When `true`, chat conversations get the tool, real launches are allowed, and the tool's approval policy is `ask` |
| `WS_CC_TARGETS` | empty | Path to the launcher targets file as seen from this host. Empty uses the `wsj` default (`WSJ_TARGETS`, else `$XDG_CONFIG_HOME/wsj/targets.toml`, else `~/.config/wsj/targets.toml`) |
| `WS_CC_SSH_CONFIG` | empty | An `ssh_config` file passed as `ssh -F` for every `ssh` target in the targets file (key, user, host name and `known_hosts` per target). Overrides the targets file's `ssh_config` key; `WSJ_SSH_CONFIG` is the same setting for `wsj`. In the container it is `/secrets/ssh_config`, next to the mounted key |
| `WS_CC_CLASSIFY` | `true` | When a prompt matches no rule, `spawn_job` asks a small model (task class `classify`) to classify it; `false` keeps the rules' default lane (`api`). In the shipped `config/policies/default.yaml` the `classify` class has its own rule: it prefers `openrouter/openai/gpt-oss-120b@cerebras` (gpt-oss-120b on Cerebras through OpenRouter, so it needs `OPENROUTER_API_KEY`), then `local-llama/gpt-oss-20b` and `local-llama/qwen3-14b`, and nothing else. A prompt the rules did not flag as sensitive can therefore be shown to Cerebras via OpenRouter; a flagged one never leaves the box. If OpenRouter is unconfigured and no local endpoint is up, the call fails and the rules' decision stands. Chat turns carry `chat` or `code`, never `classify`, so this rule is reached only by `spawn_job` |
| `WS_CC_ALIASES` | `api=best,openrouter=cheap,local=local` | Maps the three non-subscription lanes to gateway selectors (policy aliases from `config/policies/`). A value you set overrides the matching default; unlisted lanes keep theirs |

Notes:

- The subscription lane (`claude-subscription`) only works for the owner. `spawn_job` refuses it for any other user, whoever approves the call, and refuses it on a host where no owner check is wired.
- With a cap set, `spawn_job` reads the week's count after the rules and the classifier. Below the soft threshold nothing changes. From the threshold up, a subscription decision that is not clearly repo-bound (no working directory and fewer than two file paths: long agentic prompts, code fences with edit verbs, classifier verdicts) goes to the `api` lane instead, as rule `budget:soft`. At the cap every subscription decision goes to `api` as `budget:full`. A lane the caller asked for, with the `lane` argument or an `@claude` override, is never overridden, only noted, so the cap is a steering signal and not a hard limit. The decision log keeps the original rule and reason inside the new reason. If the count cannot be read the decision stands, with a note.
- Sensitive prompts (secrets, personal data markers) are routed to `local` or `api` and never to OpenRouter or the subscription lane; a forced lane or `@openrouter` does not override that, and a model hint is ignored for them.
- The `ws` image carries an `ssh` client but no `tmux`, `claude` or `wsj` config: from a container every target must be `type = "ssh"`, and tmux and `claude` run on the target. Mount the key, an `ssh_config`, `known_hosts` and `targets.toml` under `./secrets/` (Compose mounts it at `/secrets`, read-only) and set `WS_CC_TARGETS=/secrets/targets.toml` and `WS_CC_SSH_CONFIG=/secrets/ssh_config`; see ../infra/launcher/README.md. Nothing in the code enforces "ssh only"; a `local` target in the container simply fails. Not verified against a real server: whether the server's sshd accepts the extra key, and the launch itself. The other three lanes need nothing extra.

## `wsj` (Claude Code session launcher)

`wsj` is a separate CLI (`cmd/wsj`) and does not read `.env` or the server configuration. See CLAUDE_CODE_JOBS.md for the design.

| Setting | Default | Purpose |
|---|---|---|
| `WSJ_TARGETS` | `$XDG_CONFIG_HOME/wsj/targets.toml`, else `~/.config/wsj/targets.toml` | Path to the targets file |
| `XDG_CACHE_HOME` | `~/.cache` | Local directory (`wsj/`) for the ssh multiplexing sockets. Prompt files are not stored here: each job's prompt is written on the target host to `~/.cache/wsj/jobs/<id>/prompt.txt` with `umask 077` (mode 600), and removed by `wsj kill` and, when no window refers to the directory any more and it is over a minute old, by `wsj clean` |

`wsj` can also report its jobs to ws for the read-only job tab. Add a `[ws]` table to the targets file with `url` (the ws origin) and `key_file` (a file holding one ws API key, `~` allowed), or set `WSJ_WS_URL` and `WSJ_WS_KEY`, which override them. With neither configured, `wsj` reports nothing. `run` reports after its startup check, and `kill` and `clean` report what they remove; a failed report is a warning and the command still exits 0. The key must belong to the owner and carry the `jobs` scope (tick **Job reporting** when minting it in Settings; a key without it gets `403`).

The targets file names the machines jobs can run on (`type = "local"` or `"ssh"`, an ssh config alias as `host`, a tmux `session`, `default_dir`, and optionally `claude = "/path/to/claude"`, `tmux_socket` and `repos = ["~/proj", "/srv/apps"]`, directories the router may match a job's working directory against; `default_dir` counts without being listed; a `~` is compared literally and the longest matching directory wins). The top-level `default = "<name>"` key names the target used when `--on` is omitted; it is required when there is more than one target and none is called `local`. `session` defaults to `subscription`. Target names must match `^[a-z0-9][a-z0-9_-]{0,31}$`. A missing targets file gives a single synthetic `local` target. Each target needs tmux and its own logged-in `claude`. `--no-mux` (disable ssh multiplexing) and `--grace` (startup check on `wsj run`) are command-line flags only, not settings.

## `config/endpoints.yaml`

Seeds providers and endpoints into the database at boot (upsert by id). The seed is re-applied on every boot: edits made in Admin to a seeded provider or endpoint are overwritten, except an endpoint's `enabled` flag, which is kept. To change a seeded row permanently, change this file. Keys are referenced by variable name and never stored in the file.

```yaml
providers:
  - id: openrouter
    kind: openai_compat          # anthropic | openai_compat
    name: OpenRouter
    base_url: https://openrouter.ai/api/v1   # or base_url_env: VAR for per-box URLs
    api_key_env: OPENROUTER_API_KEY
    headers: { X-Title: ws }     # optional extra request headers

endpoints:
  - id: anthropic/claude-sonnet-5-5   # referenced by policies and API model names
    provider: anthropic
    model: claude-sonnet-5-5          # name sent upstream
    display_name: Claude Sonnet 5.5
    capabilities: { context_window: 1000000, max_output: 128000, tools: true,
                    vision: true, json_mode: true, reasoning: true,
                    prompt_cache: true, embeddings: false,
                    max_concurrency: 0 }      # optional: requests served at once at full speed (llama-server --parallel); 0 = unknown
    pricing: { input_per_m: 2.00, output_per_m: 10.00,
               cache_read_per_m: 0.20, cache_write_per_m: 2.50 }   # USD per million tokens
    throughput_class: high            # low | medium | high
    latency_class: fast               # fast | normal | slow
    local: true                       # optional; marks free local endpoints (budget downgrade targets these)
    extra_body: { provider: { order: [cerebras], allow_fallbacks: false } }   # optional; merged into every request
```

**`max_concurrency`** is the slot count of a local server. A rule with `rank: throughput` multiplies an endpoint's speed by a factor for how busy it is: 1 when idle, falling to one half when every slot is taken, and lower again for each request queued past that (a first guess; the Orin's measured sweep should replace it). An endpoint without `max_concurrency` is not scaled. Admin shows `n of m busy` on an endpoint with requests in flight, and its `slots` field edits the value. It is also a queue: once all slots are taken, the gateway holds further requests itself instead of letting llama.cpp queue them first come, first served, and hands each freed slot to the owner's waiting request before anyone else's (members rank equal, in arrival order). A request already running is never preempted. An endpoint with no `max_concurrency` has no queue. Set it equal to the server's `--parallel`, or llama.cpp will queue on top of the gateway and the order is lost. Admin shows "n waiting" beside "n of m busy".

**Embedding endpoints** carry `embeddings: true`. Agent memory uses task class `embed` and stores 768-dimension vectors, so the shipped `local-llama/nomic-embed-text` (preferred) and `openai/text-embedding-3-small` (`extra_body: {dimensions: 768}`, cheaper than a call to a chat model) fit; a model that returns another width gets its memories stored without a vector. The reflection that writes memories runs under task class `reflect` with model `auto`; the shipped policies have no rule for it.

**Providers and models can also be added in Admin** (the owner's Providers and model forms): a provider stores only the *names* of the environment variables that hold its base URL and key, never the key. A provider or endpoint that also appears in this file is overwritten from the file on every boot, so edit those here.

**Media endpoints** (image and video) use the same file. `capabilities.media` marks an endpoint as a media endpoint: text requests never route to it, and requests of task class `image` or `video` route only to endpoints like it. Pricing is per output instead of per token:

```yaml
  - id: openai/gpt-image-1
    provider: openai
    model: gpt-image-1
    capabilities: { media: { engine: openai_images, image: true, sizes: [1024x1024, 1536x1024, 1024x1536, auto], max_images: 4 } }
    pricing: { input_per_m: 5.00, output_per_m: 40.00, per_image: 0.04 }   # per_image: the estimate shown before a job runs
```

`media` keys: `engine` (the adapter in `internal/media`: `openai_images` for generations and edits, `openai_videos` for Sora, `fal` for fal.ai's queue API, `comfyui` for a ComfyUI server, `google` for Imagen and Veo through the Gemini API), `image`, `image_edit`, `video`, `image_to_video`, `upscale` (what it can make), `sizes` (accepted `WxH` or engine keywords such as `auto` or `16:9`; empty means the engine's default only), `max_images` (per job, `0` means 1), `max_seconds` (video length) and `seconds` (the allowed video lengths, the first is the default; Sora takes 4, 8 or 12). `pricing.per_image` and `per_second` price a job by output; when the engine reports token usage and the endpoint has token rates (`gpt-image-1` does), the real price is computed from tokens and `per_image` is only the estimate. A job is `image`, `edit` (optionally with a mask), `upscale` (a source to 2x or 4x, prompt optional) or `video`. How the engines read an endpoint: **`openai_images`** calls `/images/generations`, and `/images/edits` (multipart) for an edit. **`openai_videos`** calls `/videos`, polls it and downloads the result. **`fal`**: the endpoint's `model` is the fal model id, the provider's `base_url` is `https://queue.fal.run` with the key sent as `Authorization: Key`, ws sends `prompt`, `num_images`, `image_size`, `duration` and, for an edit or image-to-video, `image_url` as a data URL, `mask_url` for a mask, `upscale_factor` for an upscale, and `extra_body` fields override or add model-specific inputs. **`comfyui`**: `extra_body.workflow` names a template under `WS_COMFY_WORKFLOWS` (or is an inline object; absolute paths and `..` are refused) with placeholders `{{prompt}}`, `{{negative}}`, `{{width}}`, `{{height}}`, `{{seed}}`, `{{batch}}`, `{{seconds}}`, `{{frames}}`, `{{source}}`, `{{mask}}` (empty with no mask), `{{scale}}` (2 by default, for an upscale) and `{{model}}` (see `infra/comfyui/README.md`); other `extra_body` keys are `negative`, `size` and `fps`. **`google`**: provider `google` reads `GEMINI_API_KEY` and sends it as `x-goog-api-key`; Imagen goes through `models/{model}:predict` (`sampleCount`, an `aspectRatio` mapped from the size to the nearest of 1:1, 3:4, 4:3, 9:16 and 16:9), Veo through `:predictLongRunning` (ws polls the operation, then downloads the video; image to video is supported); it cannot edit, take a mask or upscale. Seeded: `openai/gpt-image-1` (generate and edit), `google/imagen-4`, `fal/flux-dev` and `fal/flux-kontext` (edits), `fal/clarity-upscaler` (upscale), and, **disabled until you try them**, `openai/sora-2`, `google/veo-3`, `fal/wan-2.2-t2v`, `fal/wan-2.2-i2v` and `comfyui/sdxl` (its shipped workflow has no `{{source}}` node, so it cannot really edit). The shipped `image` policy prefers `comfyui/sdxl`, then `openai/gpt-image-1`, `google/imagen-4`, `fal/flux-dev`, `fal/flux-kontext` and `fal/clarity-upscaler`; `video` prefers `openai/sora-2`, then `google/veo-3` and the Wan models. Endpoints appear at boot once their keys are set, so restart `app` and `worker`. Not verified: no live run of any engine is on record: the OpenAI Images, Sora, fal, ComfyUI and Google adapters were tested against fake servers, and nothing tests the OpenAI mask, fal `mask_url` and `upscale_factor` or ComfyUI `{{mask}}` and `{{scale}}` at all.

Notes:

- An endpoint may set `enabled: true|false` (default true). It is honored only when the endpoint is first inserted.
- An empty `throughput_class` becomes `medium` and an empty `latency_class` becomes `normal`.
- A provider with `base_url_env` is skipped when that variable is unset, and so are its endpoints. One file can serve every box.
- Capabilities are declared because engines rarely report them accurately. The router filters on them.
- `extra_body` is how an OpenRouter upstream is pinned and how engine-specific parameters are passed to local servers.
- Pricing feeds the ledger and cost-aware routing. Local endpoints are free but still recorded.
- The header comment in the file mentions `WS_ENDPOINTS_SEED_MODE=overwrite`. **No code reads this variable** (checked with a repo-wide search), so setting it has no effect. This is a stale comment, not a feature.

## `config/policies/*.yaml`

Routing policy. Each file is one policy; a lower `priority` number wins (default 100 when omitted or 0), and `name` defaults to the filename without `.yaml`. Policies are re-upserted from these files on every boot, so Admin edits to a file-backed policy are overwritten.

```yaml
name: default
priority: 100
rules:                           # evaluated top-down; first match supplies the preference list
  - match:
      task_class: [code]         # chat | code | summarize | title | embed | vision | classify | reflect | image | video
      # users: [...]  agents: [...]  selector: [...]  external: true|false
    require: { tools: true }     # capability filter (same fields as endpoint capabilities)
    prefer:                      # ordered endpoint ids; the gateway fails over down this list
      - anthropic/claude-opus-5-5
      - local-vllm/qwen3-coder-next
    deny: []                     # endpoint ids never to use for this rule
    max_cost_per_call_usd: 0     # drop candidates estimated above this
    local_only: false            # restrict to local endpoints
    rank: ""                     # "throughput": fastest first, by measured decode speed
    min_tokens_per_sec: 0        # drop candidates measured slower than this
  - match: {}                    # empty match catches everything else

aliases:                         # names usable as a model name by clients
  best: [anthropic/claude-opus-5-5, anthropic/claude-sonnet-5-5]
  cheap: [local-llama/gpt-oss-20b, anthropic/claude-haiku-4-5]
```

Empty match lists match everything. Endpoints whose provider isn't configured on this box are skipped automatically. Failover only happens on retryable errors before the first token is streamed.

A rule whose `match` names a `selector` (the shipped `fast` rule: `match: { selector: [fast] }`, `rank: throughput`) makes that name usable wherever an alias is: as a model on `/v1`, as a key's default policy in Settings, in the MCP server's model list. Unlike an alias it carries the rule's rank, filters and requirements; an alias of the same name wins. `rank: throughput` reorders the rule's candidates by the speed a new request is expected to get: the average decode speed (output tokens per second, scaled for how busy the endpoint is, see `max_concurrency` under endpoints) the gateway has seen for that endpoint (replies of at least 16 tokens with a decode phase of at least 200 ms count; a reply whose billed output is far more than the stream carried, a reasoning model thinking silently, is not counted), or the endpoint's `throughput_class` as a prior (`low` 15, `medium` 40, `high` 100 tokens/s; anything else ranks as medium) until it has three observations, and again once its last observation is more than 15 minutes old, so an endpoint measured slow is tried again later instead of staying last. Equal speeds keep the `prefer` order. `min_tokens_per_sec` drops candidates whose speed has been measured below the value; endpoints without a recent measurement are kept and the filter never empties the list. An unknown `rank` or a negative `min_tokens_per_sec` is refused when the policy is loaded or saved. Measurements are written to `endpoint_throughput` after every counted reply and loaded at boot, so a restart ranks by the last measurement rather than the declared class; a loaded or stale measurement is a prior that stands in for the declared class only when it is higher (an endpoint measured slow is still tried again at its class), until three fresh replies replace it. The decision's `reason` gets `+throughput` when a rank was applied and `+min-speed` when the filter dropped a candidate.

## `config/mcp.yaml`

MCP servers whose tools the agent runtime exposes as `mcp__<server>__<tool>`.

```yaml
servers:
  - name: infisical
    url: http://mcp-infisical:8000/mcp
    transport: streamable_http   # streamable_http (default) | sse
    enabled: true
    headers_env:                 # header name -> env var holding its value
      Authorization: MCP_FOO_TOKEN
    policy: ask                  # default for every tool: auto | ask | deny
    policies:                    # per-tool overrides
      list-secrets: auto
      delete-secret: deny
    allow: []                    # if set, only these tools are registered
    deny: []                     # tools to leave out
    idempotent: [list-secrets]   # safe to re-run after a crash
```

Server `name` must match `^[a-z0-9][a-z0-9_-]{0,30}$`; each server has either `url` or `command`, never both and never neither; `policy` defaults to `ask`. Tool names are truncated to 64 characters and each tool call has a 2 minute timeout. At boot ws retries each server for about 45 seconds so sidecars can start, and skips servers that stay unreachable with a warning. `auto` runs without asking, `ask` pauses the run for approval, `deny` blocks. Header values are read from the environment at connect time, so credentials stay out of the file.

**Command (stdio) servers** run inside a code project's sandbox instead of connecting over the network:

```yaml
  - name: fs
    command: [npx, -y, "@modelcontextprotocol/server-filesystem", /workspace]
    env: ["FOO=bar"]            # KEY=value entries, plain values only: secrets never enter a sandbox
    policy: ask
    policies: {read_file: auto}
```

The worker starts the command with `docker exec` in the project's sandbox (working directory `/workspace`, user `dev`) the first time a call needs it, one session per sandbox container, and reconnects once if a call fails. At boot the worker probes the command once in a throwaway container with the same limits, no volume and a 256 MB tmpfs, to learn the tool list (retrying for about 45 seconds), then removes it. Command servers connect in the worker only (`ws serve` skips them) and only when the sandbox is enabled, and their tools appear in code projects. The command must exist in the sandbox image; the shipped image is unchanged and no command server is enabled by default (the example in `config/mcp.yaml` is commented out). `headers_env` and `transport` are for URL servers. Policies, `allow`, `deny` and `idempotent` work as for URL servers. Not verified: how an `ask` approval looks for a stdio tool in the web UI.
