# Chat and models

How a conversation works in ws, how a model gets chosen, and what happens to long conversations. For the commands to get ws running, see [Start here](../START-HERE.md).

## Projects and conversations

A **project** holds conversations. The sidebar's "New conversation" starts a chat in your most recently active project, or creates one called "General" if you have none; a project can also be a [code project](code-projects.md), which adds a sandbox. Every turn is an agent run: one or more model calls, plus any tool calls the model asks for. Runs are saved as they go, so they survive a restart of the server; a run whose process stopped is picked up and finished by the worker. Hover a conversation in the sidebar to rename it or archive it.

## Choosing a model

The model picker has three groups:

- **Routing:** aliases defined in your policy files (the shipped ones are `best`, `cheap`, `local` and `code`), plus the built-in `auto`, which is the default. A routing name lets ws pick the endpoint for each request from your policies, filtered by what the request needs (tools, images) and by which endpoints are healthy.
- **Local:** endpoints on your own hardware.
- **Hosted:** endpoints from providers such as Anthropic, OpenAI and OpenRouter.

An endpoint that is down is shown disabled with "(down)"; an endpoint an admin has disabled is not listed.

The brain icon next to the picker asks for extended thinking. It has an effect only on Anthropic-protocol endpoints that support it; local and OpenRouter endpoints ignore it today. It is not remembered and is off again when you reload the page. Which endpoints exist, and what each can do, comes from `config/endpoints.yaml` and from the Admin page; see [Admin](admin.md) and [Configuration](../CONFIGURATION.md).

## Attachments

"Attach" accepts images, PDF, plain text, Markdown, JSON and CSV files. What the model sees depends on the endpoint. The router only filters on images (it picks a vision-capable endpoint) and on tools. A PDF is read only by Anthropic-protocol endpoints; an OpenAI-compatible endpoint gets a short placeholder saying the file was omitted, and text and JSON files are included as text there. A file whose type the browser leaves blank can be omitted the same way.

## Tools in chat

A chat turn can call these built-in tools: `create_artifact` and `update_artifact` (see [Artifacts](artifacts.md)), `web_fetch` (public URLs only; private, loopback and tailnet addresses are refused), `read_blob` (to page through a long tool result that was stored outside the conversation) and `ask_user`. Tools from MCP servers listed in `config/mcp.yaml` appear as `mcp__<server>__<tool>`. Code projects are offered every tool the worker has, including these and the sandbox tools. Each tool has a policy that decides whether it runs on its own or asks you first; see [Approvals](approvals.md).

## Long conversations

ws never edits what is stored. When a request would be larger than the compaction budget (`WS_COMPACTION_BUDGET_TOKENS`, 60,000 tokens by default), it sends a stored summary of the older turns plus the recent turns instead, and drops the oldest turns for that one request if it is still too long. A background job writes a fresh summary with a cheap model. Your full history stays as it was.

## Cost and limits

Every model call is written to a usage ledger with its tokens, cost and the routing decision. The owner can set budgets that block a call or switch it to local endpoints once a limit is reached; see [Admin](admin.md).

*Checked against the code at master `3364287`.*
