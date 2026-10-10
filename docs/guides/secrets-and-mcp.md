# Secrets and MCP servers

How to keep provider keys out of files, and how to give the assistant extra tools.

## Secrets from Infisical

ws can load its secrets from [Infisical](https://infisical.com), cloud or self-hosted, instead of keeping them in `.env`. Set three variables in `.env` and the rest can live in Infisical:

```bash
INFISICAL_CLIENT_ID=...
INFISICAL_CLIENT_SECRET=...
INFISICAL_PROJECT_ID=...
```

At boot, both `serve` and `worker` log in with that machine identity and load every secret at that environment and path (`prod` and `/` by default; `INFISICAL_ENV` and `INFISICAL_PATH` change them; names that are not valid environment variable names are skipped) into their environment before the rest of the configuration is read. They check again every five minutes, so a rotated provider key reaches ws without a restart. If Infisical cannot be reached at boot, ws logs it and continues on `.env`.

Which wins when a name is in both? At boot, a value already in the environment wins unless you set `INFISICAL_OVERWRITE=1`. But the five-minute refresh always overwrites, so a name Infisical holds replaces a `.env` value after a few minutes. Keep each name in one place. For a self-hosted Infisical, set `INFISICAL_SITE_URL`.

The full variable list is in [Configuration](../CONFIGURATION.md).

## MCP servers

The Model Context Protocol lets the assistant use tools that live outside ws. List servers in `config/mcp.yaml`; each one needs a `name` and either a `url` (Streamable HTTP, or `transport: sse`) or a `command` (see below). Their tools appear to the assistant as `mcp__<server>__<tool>` and work in chat as well as in code projects for URL servers; `command:` servers (below) work only in code projects.

```yaml
servers:
  - name: infisical
    url: http://mcp-infisical:8000/mcp
    policy: ask                  # the default for every tool: auto | ask | deny
    policies:                    # per-tool overrides
      list-secrets: auto
      delete-secret: deny
    headers_env:                 # header name -> environment variable holding its value
      Authorization: MCP_FOO_TOKEN
```

- **Approvals still apply.** A tool's policy decides whether it runs, asks or is blocked, and the default is `ask`; see [Tool approvals](approvals.md). `allow` registers only the tools it lists and `deny` removes the ones it lists, and `idempotent` marks tools that are safe to run again after a crash.
- **Credentials stay out of the file.** Header values are read from the environment when ws connects. The variable must hold the whole header value, such as `Bearer abc123`; ws does not add a prefix.
- **At boot** ws retries each server for about 45 seconds so a sidecar can start, and skips servers that stay unreachable. Each tool call has a two-minute limit.

### The Infisical MCP server

ws ships a Compose option that runs Infisical's own MCP server as a sidecar (`WS_DEPLOY_PROFILES=infisical`), so the assistant can list and manage secrets in a project. It uses a second, least-privilege machine identity (`INFISICAL_MCP_CLIENT_ID` and `INFISICAL_MCP_CLIENT_SECRET`), so ws's own identity never reaches a sandbox, and secret values are masked in tool output by default (`INFISICAL_MCP_MASK`, which the Infisical server itself applies). The sidecar exposes only `list-projects`, `list-secrets`, `get-secret`, `create-secret` and `update-secret` unless you change `INFISICAL_MCP_TOOLS`. Treat it like any tool that can read secrets: leave its policies on `ask`.

MCP servers that need a filesystem or a shell can be listed with a `command:` instead of a `url`. ws then runs the command inside the code project's sandbox, so it only works there, the command has to exist in the sandbox image, and no secrets are passed in (only plain `KEY=value` entries in `env`). Nothing is enabled by default; the commented example in `config/mcp.yaml` shows the shape.

## ws as an MCP server

If you use Claude Code or another MCP client, point it at ws: `claude mcp add --transport http ws https://<your-host>/mcp --header "Authorization: Bearer ws_..."` (the Settings page prints the line for a key you mint with **MCP access** ticked; a key without it, including every key made before this setting existed, gets a `403`). The tools let the client list your projects, conversations and models, read a conversation, ask a model a one-off question, start a run in a project and follow it, and hand a task to the task router. It needs an API key with the `mcp` scope, not a browser session, and every call acts as the key's user. `WS_MCP_SERVER=false` turns it off.

## Your own provider keys

Settings has a section where you can save your own API key for a hosted provider (one that takes a key). The key is sealed on the server with `WS_SECRETS_KEY` before it is stored and is never shown again, only its last four characters; without that variable set, saving is refused and Settings says so.

Saved keys are used (#160). A hosted model serves a **member** only with their own key for that provider. The **owner**, and any member the owner ticks in the **people** section of Admin ("may use shared keys"), keep using the server's shared keys. Local models are not affected, and the server's own housekeeping calls are not either. If the only models that could answer are hosted and the person has no key, the chat says: "This model needs your own API key. Add one under Settings, provider keys, or pick a model that runs on this server." Spend on a person's own key is left out of the budget sums (the ledger row carries `own_key`); spend on a shared key still counts against the person's budget.

A provider the server has no key for (its key variable in `config/endpoints.yaml` is unset) still shows up, and a member's own saved key makes it usable (#169); the owner's shared-key grant does not reach it. The change applies to every member at once: an existing member loses access to hosted models until they save a key or the owner ticks them. Read from the code and its unit tests (`internal/gateway/keys.go`); no real provider call with a member's key is on record.

*Checked against the code and configuration at master `13a673a`.*
