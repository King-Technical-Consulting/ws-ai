# Start here

ws is a self-hosted AI workspace: chat with artifacts, an agentic coding mode with Docker sandboxes, and a gateway that routes between your own models and hosted APIs. It is one Go binary, one Postgres, and one login. This page gets a development copy running on your machine; [Deploy with Compose](../infra/compose/README.md) covers a real deployment.

Status: ws is under active development. [Roadmap](ROADMAP.md) lists what is built and what is planned.

## What you need

- Go 1.26 and Node 22.
- PostgreSQL 17 with pgvector, or Docker (the quick start below starts Postgres in a container).
- At least one model: a hosted provider key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or `OPENROUTER_API_KEY`), or an OpenAI-compatible server such as llama.cpp, set as `LLAMA_SERVER_URL`. See [Inference targets](../infra/inference/README.md).

## Run it

```bash
git clone https://github.com/King-Technical-Consulting/ws-ai && cd ws-ai
cp .env.example .env
```

Edit `.env`. At minimum set a long random `WS_SESSION_SECRET` (32 bytes or more) and `WS_OWNER_EMAIL`, plus one provider key. Hosted providers without a key are skipped, so one file works anywhere. Every setting is listed in [Configuration](CONFIGURATION.md).

Then start Postgres and the processes, each in its own terminal:

```bash
make dev        # starts Postgres in Docker, then prints the next three commands
make serve      # API on :8080, artifacts on :8081; migrates and prints the OWNER INVITE link
make worker     # background jobs, sandboxes, the egress proxy
make web-dev    # the web app on :5173, proxying /api to :8080
```

`make serve` prints a line like `OWNER INVITE created (open this link to set up the first account) link=...` the first time it boots. Open that link in your browser, create your account and sign in. Use the Vite address (`http://localhost:5173`) while developing the frontend. To serve the built frontend from the Go binary instead, run `make web` once and open `http://localhost:8080`.

Without `RESEND_API_KEY`, invite and magic-link emails are printed to the `serve` log instead of being sent. Passkeys need a domain over https; on plain `localhost` the server may disable them (the log says so), and a magic link from the log works instead. I have not confirmed which browsers accept passkeys on `localhost`.

## Try it

1. Open a conversation and pick a model, or leave it on `auto`, which routes by policy.
2. Create a **code project**. The worker starts a Docker sandbox for you and you get a file tree, a terminal and a preview. Sandboxes need Docker reachable from the worker.
3. In Admin, add providers and endpoints, set budgets and invite someone.
4. Create an API key in Settings, pick a default policy, and copy the environment block it prints for Claude Code, or point any OpenAI- or Anthropic-compatible client at `http://localhost:8080`. See [HTTP API](API.md).

## Where next

- [Chat and models](guides/chat.md), [Tool approvals](guides/approvals.md), [Code projects](guides/code-projects.md), [Artifacts](guides/artifacts.md), [Use Claude Code with ws](guides/claude-code.md) and [Admin](guides/admin.md): how ws works day to day.
- [Trust and security](TRUST.md): how artifacts, sandboxes and secrets are isolated.
- [Architecture](ARCHITECTURE.md): how the pieces fit.
- [Deploy with Compose](../infra/compose/README.md): hardware profiles, ingress, continuous delivery.
- [Roadmap](ROADMAP.md): what is built and what is next.
