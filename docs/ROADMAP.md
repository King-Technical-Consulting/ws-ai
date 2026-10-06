# Roadmap

What is built, what is in progress and what is planned. Milestones M1 to M4 are built and used; M5 is in progress; M6, M9 and M10 are in progress, M7 is built, M8 is planned and M11 is a stretch goal, and both may change. This page is a summary: each built area has a reference page, and planned work has no code yet. Last checked against the code at master `a6a3e21` (see [Architecture](ARCHITECTURE.md) for what exists today).

| Milestone | State | What it covers |
|---|---|---|
| M1 Skeleton and chat | Built | Accounts (invite, passkeys, magic links), the model gateway with Anthropic and OpenAI-compatible adapters, routing, streaming chat, the usage ledger, the web app |
| M2 External API and routing | Built | `/v1/chat/completions` and `/v1/messages` with API keys, routing policies and budgets with an Admin editor, sandboxed artifacts on a separate origin |
| M3 Agent runtime | Built | Durable runs with steps and checkpoints, resume after restart, tool approvals, conversation compaction, River jobs |
| M4 Coding sandbox | Built | Per-user Docker sandboxes, egress proxy, GitHub App with git tools and `open_pr`, a files, terminal and preview panel |
| M5 Claude Code and MCP | In progress | Built: secrets from Infisical, an MCP client, the `wsj` launcher for Claude Code sessions in tmux, and the task router (rules plus a small-model classifier, off by default), and the outbound gateway (a byte-for-byte `/v1/messages` passthrough for Anthropic endpoints so Claude Code can use ws as its API gateway), ws as an MCP server (`/mcp`, for API keys with the `mcp` scope), MCP servers that run inside a code project's sandbox, the read-only job tab (a page listing the Claude Code sessions `wsj` reports, with a read-only terminal view, tailnet-gated), and a weekly launch counter that lets the router keep low-value work off the subscription as the week fills. Next: acceptance runs with a real Claude Code client and a real target |
| M6 Design, images and video | In progress | Built: design artifacts (a design system per conversation, variants, export, a diff view), a media gateway with OpenAI, Sora, fal.ai, ComfyUI and Google (Imagen, Veo) engines, image edits with masks, upscale and video, `generate_image` and `generate_video` chat tools and a Media page. Next: Sandpack for React artifacts and chromedp screenshots |
| M7 Long-lived agents | Built | Agents with goals, cron, webhook and GitHub triggers, memory, a monitor with a budget control, and pause and resume of a run |
| M8 Fleet | Planned | vLLM and Ollama/MLX profiles, throughput-aware routing |
| M9 Rented GPUs | In progress | Built: starting a RunPod GPU machine from Admin with a daily cap and idle shutdown, appearing as a model while it runs. Next: Lambda and Vast.ai. A rental template can also be a trainer machine for fine-tuning |
| M10 Training flywheel | In progress, optional | Built: thumbs on answers, per-user consent, dataset export, fine-tune jobs on a trainer container or a rented trainer machine, an adapter registry and an eval gate. Next: serving an adapter, distillation. Only tested against fakes |
| M11 OpenClaw integration | Planned, stretch | Not scheduled. Today one `SKILL.md` file imports as an untrusted, tool-less agent preset (the AgentSkills shape OpenClaw skills reportedly use; not yet tried on a real OpenClaw file). Later, in order: OpenClaw as a client of ws (`/v1` and `/mcp`), skill folders whose scripts run only in the sandbox, a reviewed skills shelf, an OpenClaw agent run as a ws agent. Safety against prompt injection comes first |

## Claude Code and subscriptions

ws does not forward a Claude subscription credential and does not read Claude Code's output. Programmatic Claude access goes through an API key and the gateway. A subscription session is launched by the separate `wsj` CLI, which starts the official `claude` in tmux and returns a handle. See [Trust and security](TRUST.md).

## How to follow along

Each milestone's status is on this page and in the repository README. Planned items are intentions, not commitments, and they do not carry dates.
