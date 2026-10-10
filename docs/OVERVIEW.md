# ws

ws is a self-hosted AI workspace. You get chat with documents the assistant can build, a coding mode where the assistant works in a sandbox you can watch, and one gateway that sends each request to the model that suits it, your own or a hosted one. It runs on your hardware as one Go program and one Postgres database, behind one login.

> ws is under active development and has not had an independent security review. See the [roadmap](ROADMAP.md) for what is built and what is planned.

## What you can do with it

- **Chat with your models.** Pick a model or let a routing policy choose by what the request needs and what each endpoint costs. Attach images and documents. Long conversations are summarized for the model without changing what is stored. See [Chat and models](guides/chat.md).
- **Get documents, not just replies.** The assistant can produce web pages, diagrams, Markdown and code as versioned artifacts that render on a separate, locked-down origin. See [Artifacts](guides/artifacts.md).
- **Let the assistant write code in a sandbox.** A code project gives the assistant its own container with your repository, a file editor, a terminal and live previews. By default it asks before it runs commands, pushes code or opens pull requests. See [Code projects](guides/code-projects.md) and [Tool approvals](guides/approvals.md).
- **Use Claude Code through ws.** Point Claude Code at ws as its API gateway and get routing, budgets and a per-session usage record, or launch real Claude Code sessions on your own machines with a small CLI. See [Use Claude Code with ws](guides/claude-code.md).
- **Generate images.** Ask in chat or use the Media page; ws routes the request to an image model, shows the price first and keeps the results in a gallery. It can also edit a photo and make short videos. Engines are OpenAI (images and Sora), fal.ai, Google (Imagen and Veo) and a ComfyUI you run; no live run of them is on record yet.
- **Run agents on a schedule.** An agent has a goal, its own tools and a project; cron, webhook or GitHub push triggers start it, a monitor page shows its runs (you can pause and resume one), and it can remember facts between runs. Four starting-point presets fill in the new-agent form, and a `SKILL.md` file can be read into a preview of one. These are early features.
- **Run your own models next to hosted ones.** llama.cpp, vLLM, Ollama and anything OpenAI-compatible work as endpoints, with Anthropic, OpenAI and OpenRouter alongside. Budgets can block a call or switch it to local endpoints, and the owner can rent a RunPod GPU from Admin with a daily cap and idle shutdown. See [Admin](guides/admin.md) and [Inference targets](../infra/inference/README.md).
- **Use ws from your other tools.** ws also runs as an MCP server, so Claude Code and other MCP clients can list your projects, read conversations and start runs, with an API key made for that. Keys carry scopes: `chat` for the model API, `mcp` for this, `jobs` for `wsj`, and a key reaches nothing else.
- **Keep control.** Secrets can live in Infisical and never enter a sandbox. Running commands, pushing and opening pull requests ask for approval by default, and you can change the policy for each tool. See [Trust and security](TRUST.md).

## How it is built

Two roles from one binary: `serve` handles the browser, the API and auth; `worker` runs the long jobs, the sandboxes and the only connection to Docker. They coordinate through Postgres. The model gateway speaks two protocols, Anthropic Messages and OpenAI Chat Completions, so every inference box is just an endpoint with declared capabilities. See [How ws works](guides/how-it-works.md) for the plain-language version and [Architecture](ARCHITECTURE.md) for the reference.

## Where to start

1. [Start here](START-HERE.md) gets a development copy running.
2. [Deploy with Compose](../infra/compose/README.md) puts it on a server, with Cloudflare Tunnel, Tailscale or Traefik in front.
3. The guides under **Use it** show how it works day to day, and **Reference** has every setting, route and table.

ws is released under the PolyForm Noncommercial License 1.0.0, which allows noncommercial use only; see [License](LICENSING.md).
