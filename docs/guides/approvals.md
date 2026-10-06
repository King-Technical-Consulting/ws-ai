# Tool approvals

Models act through tools, and each tool has a policy that decides whether it runs on its own, waits for you, or never runs.

## The three policies

| Policy | What happens |
|---|---|
| `auto` | The tool runs without asking. |
| `ask` | The run pauses and an approval card appears in the chat. Nothing happens until you answer. |
| `deny` | The tool is blocked. |

Each tool ships with a default. In a chat, the built-in tools (`create_artifact`, `update_artifact`, `web_fetch`, `read_blob`, `ask_user`) are `auto`. In a [code project](code-projects.md), the file tools (`read_file`, `write_file`, `edit_file`, `list_files`, `grep`) are `auto`, while `bash` is `ask`, and so are `git_push` and `open_pr`. Tools from MCP servers default to `ask`, unless the server's entry in `config/mcp.yaml` sets a different default or a per-tool override; see [Configuration](../CONFIGURATION.md). The task router's `spawn_job` is `auto` while dispatch is off (it only decides) and `ask` when the operator turns dispatch on; see [Use Claude Code with ws](claude-code.md).

## Answering an approval

The card reads "The assistant wants to run `<tool>`", shows the exact arguments, and has **Allow** and **Decline** buttons. Allowing runs the tool and the run continues; declining tells the model the call was refused and the run continues without it. The run waits until you answer (nothing expires a pending approval today), and it is saved, so a restart of the server does not lose it.

## Changing a policy for one conversation

A conversation can override the defaults with `settings.tool_policies`: send `{"settings": {"tool_policies": {"bash": "auto"}}}` to `PATCH /api/conversations/{id}` (see [HTTP API](../API.md)). The `settings` object is replaced, not merged, and policies are read when a run starts. There is no setting for a policy across all conversations yet; defaults come from the tools themselves.

## Why `ask` exists

A model can be steered by text it reads, such as a web page it fetched or a file in a repository. Approvals keep a person between that text and anything with side effects: running a command, pushing code, opening a pull request. Isolation does the rest; see [Trust and security](../TRUST.md).

*Checked against the code at master `3364287`.*
