# How ws works

A plain-language tour of what happens between your message and the reply. The [architecture reference](../ARCHITECTURE.md) has the details and file names.

## Two processes, one database

ws is one program started in two roles. **`serve`** answers the browser and the API: sign-in, the web app, the `/v1` model API, and the artifact and preview hostnames. **`worker`** does the slow and privileged work: running agent steps, summarizing long conversations, managing sandboxes, and the network proxy that sandboxes go through. Both roles run the model gateway, but only the worker can talk to Docker. They never call each other directly except to manage sandboxes, move files and carry terminal bytes; everything else goes through Postgres, which holds jobs, live events, settings and history.

## A message is a run

When you send a message, ws starts a **run**: a series of steps, where each step is one call to a model plus any tools it asked for. After every step the state is saved, and a background job re-queues a run whose process stopped so a worker can finish it. A tool that needs your say-so pauses the run until you answer; see [Tool approvals](approvals.md).

## Picking a model

Every call goes through the **gateway**. It checks your budgets, trims the conversation if it is too long, then lets the **router** choose an endpoint from your policies: which kinds of request prefer which models, what each endpoint can do (tools, images), whether it is healthy, and what it costs. If an endpoint fails with a retryable error before it has sent anything, the gateway tries the next one. Every call is written to a ledger with its tokens, cost and the decision. The gateway only knows two protocols, Anthropic Messages and OpenAI Chat Completions, so a local llama.cpp server is just another endpoint with a list of capabilities.

## Keeping long conversations usable

ws never edits stored messages. When a request would be too large it sends a stored summary of the older turns plus the recent ones, and a background job keeps the summary fresh. Your history is intact.

## Where the assistant's work happens

- **Artifacts** are saved as versions and shown on their own origin in a sandboxed frame that cannot read the app and cannot make network requests of its own (it can load scripts and styles from a short list of CDNs).
- **Code projects** run in a Docker container per user and project, with a read-only root and no network except a proxy that allows package registries and GitHub and injects credentials so they never exist in the container.
- **Previews** of a dev server in the sandbox get their own hostname, reached through a signed sign-in from the app that lasts 12 hours.

For the isolation details and the limits, see [Trust and security](../TRUST.md).

## What is stored

Postgres holds accounts, projects, conversations, messages, runs and steps, approvals, artifacts and their versions, sandbox records, the endpoint and policy configuration, budgets and the usage ledger. Large tool output is kept as files in a blob directory, and each sandbox's working copy lives in its own Docker volume. [Data model](../DATA-MODEL.md) lists every table.

*Checked against the architecture and the code at master `f54aa75`.*
