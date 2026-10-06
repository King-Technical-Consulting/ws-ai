# Use Claude Code with ws

ws supports Claude Code in two separate ways. They do different jobs and use different billing, so keep them apart.

| | ws as Claude Code's API gateway | `wsj`: Claude Code sessions in tmux |
|---|---|---|
| What it does | Claude Code talks to ws instead of Anthropic; ws routes, budgets and records the calls | `wsj` starts the real `claude` in a tmux window on a machine you pick, and you attach to it |
| Billing | The provider API key behind ws | Your own Claude subscription, on that machine |
| Credentials | A `ws_` key; ws sends its own provider key upstream | Each machine does its own `claude` login; ws never sees it |

ws never reads, stores or forwards a Claude subscription credential, and never reads Claude Code's output. See [Trust and security](../TRUST.md).

## ws as the API gateway

1. Open **Settings**, create an API key and pick its default policy: `auto`, or any routing alias (`best`, `cheap`, `local`, `code`, or your own). The policy decides where a request goes when Claude Code names no model, or one ws does not know. A default key (scope `chat`) is all Claude Code needs; leave "MCP access" off unless the key is also for ws's MCP server.
2. The page prints an environment block with a copy button, once, right after you create the key (the key cannot be shown again, so copy it then). For `auto` and `best` it is:

   ```bash
   export ANTHROPIC_BASE_URL=https://<your ws host>
   export ANTHROPIC_AUTH_TOKEN=ws_...
   export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
   ```

   For a policy that can land on local models it also sets `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`, which turns off betas only hosted Claude understands, and pins `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL` and `ANTHROPIC_DEFAULT_HAIKU_MODEL` to the policy name.
3. Run `claude` in a shell with those variables set.

**What ws does with the requests.** ws routes each call by your policies and budgets. If the first endpoint it picks is a native Anthropic one, ws passes the request through unchanged except for the model name and the credential, and returns Anthropic's response unchanged, including errors, retry headers and streaming events (ws can first fail over to another Anthropic endpoint if one answers `408`, `409`, `429` or a `5xx` before anything has been sent to you). If it lands on a local or other OpenAI-compatible endpoint, ws translates; unknown beta features are dropped rather than rejected. Every call is recorded in the usage ledger under your key, and Claude Code's session id is stored with it, so you can see what a session cost. `HEAD /api/hello`, which Claude Code uses to check a gateway, answers without a key. The routes are in [HTTP API](../API.md).

**Status.** This was tested against a fake Anthropic server. It has not yet been run with a real Claude Code client, and quality on local models depends on the model.

## `wsj`: Claude Code sessions in tmux

`wsj` is a small command-line tool for your own computer. It starts the official `claude` in a tmux window on the machine you choose, local or over ssh, and returns a handle. You see Claude Code only by attaching to that real terminal.

```bash
make wsj-install                                  # go install; usually puts wsj in ~/go/bin
wsj targets                                       # reads ~/.config/wsj/targets.toml
wsj run --on homelab --dir ~/proj/app "Fix the failing test in pkg/x"
wsj ls --all
wsj attach --on homelab <job>                     # detach with your tmux prefix, then d
wsj kill --on homelab <job>
wsj clean --all                                   # kill dead windows, remove orphaned prompt directories
```

- Each target needs `tmux` and its own logged-in Claude Code (run `claude` once and `/login`). `wsj` tells you when either is missing and does not try to fix it. If `claude` exits at once, `wsj run` prints the handle and exits with an error, and keeps the window so you can attach and see why.
- Prompts are sent over stdin into a mode-600 file on the target, which `kill` and `clean` remove. They never go on the ssh or tmux command line, though the `claude` process receives the prompt as its argument.
- The targets file lists the machines; see [Configuration](../CONFIGURATION.md) for its keys. If it has a `[ws]` table (`url` and `key_file`, a file holding a ws API key minted with **Job reporting** ticked; or `WSJ_WS_URL` and `WSJ_WS_KEY`), `run`, `kill` and `clean` also report each job to ws, and the owner-only **Jobs** page lists them with a read-only terminal view. The page is limited to the tailnet by default and needs `ssh` and `tmux` where ws runs. The `ws` container image has an `ssh` client but no `tmux`, so on a Compose deployment the terminal and the launcher work for ssh targets (set `WS_CC_TARGETS` and `WS_CC_SSH_CONFIG`, see [Configuration](../CONFIGURATION.md)) and not for a local one. Add `--no-mux` to `run`, `ls`, `attach`, `kill` or `clean` to skip ssh connection sharing if a stale connection gets in the way.

## The task router (experimental, off by default)

ws also has a `spawn_job` tool that can decide where a task should run, a Claude Code session on a target, a hosted model or a local one, and log the decision. It is off by default: until the operator sets `WS_CC_DISPATCH=true`, nothing launches (every call is only a recorded decision) and chat conversations are not offered the tool, though code projects still see it. The subscription lane only works for the owner. `wsj route "<prompt>"` shows what the rules would decide without starting anything. The operator can also set a weekly soft cap (`WS_CC_WEEKLY_CAP`): ws counts the Claude Code launches it knows about each week, and as the count fills the router sends work that is not clearly tied to a repository to a hosted model instead. Asking for the subscription lane explicitly always works, so it steers the router and is not a limit. The settings are in [Configuration](../CONFIGURATION.md).

*Checked against the code at master `13a673a`.*
