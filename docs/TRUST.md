# Trust and security

What ws isolates, from what, and where the limits are. Each claim here was checked against the code; the file is named so you can check it too. This page describes the design as built. It is not an audit, and ws has not had an independent security review.

## Artifacts

Model-written HTML, SVG, Markdown, Mermaid and code render only on a separate origin (`WS_ARTIFACT_URL`), inside an `<iframe sandbox="allow-scripts">`. The sandbox attribute never includes `allow-same-origin`, so artifact code cannot read the app's cookies or storage. The artifact origin sends a content security policy that blocks network access, and the app reaches it through a signed URL that expires after 24 hours (`internal/artifacts`, `internal/httpx`). Do not point the app and artifact hostnames at the same host.

## Coding sandboxes

A code project gets one Docker container per user, created by the worker. From `internal/sandbox/manager.go`, each container has:

- a read-only root filesystem, with writable space only in `/tmp`, `/home/dev` and `/run` tmpfs mounts and the project's `/workspace` volume;
- all Linux capabilities dropped and `no-new-privileges` set;
- a memory limit (4096 MB by default), a CPU limit and a process limit;
- a non-root user, an init process, and gVisor (`runsc`) as the runtime when it is installed, otherwise runc.

Containers sit on an internal Docker network whose only route out is the worker's egress proxy. The proxy enforces a host allowlist (extended with `WS_EGRESS_ALLOW`; the host of `WS_PUBLIC_URL` is always allowed) and tunnels HTTPS without inspecting it.

## Secrets

Secrets never enter a sandbox. For GitHub, the egress proxy swaps in a fresh, repo-scoped GitHub App installation token for plain-HTTP git traffic to `github.com`, so the credential exists only in the worker. Provider keys live in the server's environment, optionally loaded from Infisical at boot and refreshed every five minutes (`internal/secrets`). The Infisical MCP sidecar runs outside any sandbox, so its machine identity never enters one, and secret values are masked in its tool output by default.

## The Docker socket

Only the worker mounts the Docker socket. The browser never talks to the worker: file and terminal requests go browser to `serve` to the worker's internal API, authenticated by a header derived from `WS_SESSION_SECRET`. A process with the Docker socket can control the host, so treat the worker as privileged and do not publish its port.

## Previews

Dev-server previews are served on their own hostnames. The app redirects the browser there with a short-lived token that becomes a host-only cookie. The preview proxy forwards to the container and strips the app's session cookie.

## Accounts and API keys

Sign-in is by passkey or magic link; there are no passwords. Registration is invite-only. API keys start with `ws_` and are stored as hashes, shown once when created. A key carries scopes: `chat` (the default) lets it use the `/v1` gateway, `mcp` lets it use ws as an MCP server, and `jobs` lets `wsj` report Claude Code sessions to the job tab; a key reaches nothing else. The web `/api` routes take a signed-in session and refuse keys, so a key that leaks from a Claude Code environment cannot read projects, call the owner-only routes or mint more keys. Keep keys secret and revoke ones you do not use. Each model call is recorded in a usage ledger under the key, user and route, and budgets can block or downgrade calls. When Claude Code or another Anthropic client uses ws as its gateway and a request lands on a native Anthropic endpoint, ws proxies it byte for byte: your `ws_` key is checked and not forwarded, ws sends its own provider key upstream, and the provider's response is relayed unchanged. The client's session id header, if sent, is stored on the ledger row.

## Claude Code and subscriptions

ws never reads, stores or forwards a Claude subscription credential, and never parses Claude Code's output. Claude Code on a subscription is launched only by the separate `wsj` CLI, which starts the official `claude` binary in a real terminal (tmux) and returns a handle. Programmatic use of Claude goes through an API key and the gateway. The task router's `spawn_job` tool is off by default (it only decides and logs a routing decision). When it is switched on, it refuses the subscription lane for anyone but the owner, and prompts that look sensitive are never sent to OpenRouter or the subscription lane. A prompt the rules cannot place and did not flag as sensitive may be shown to a small model for classification: by default gpt-oss-120b on Cerebras through OpenRouter, with local models behind it, and no other hosted model. A prompt flagged as sensitive is never sent to OpenRouter or to the subscription lane; it stays on a local model, or goes to the hosted API lane only when the user names `@api` for it.

## The Claude Code job tab

The Jobs page lists the Claude Code sessions `wsj` has reported and can show one as a read-only terminal. The routes are owner-only and, by default, answer only to the tailnet and loopback (`WS_CC_WEB_ALLOW`). Two things limit how far that goes. The network check uses the address the server sees after its real-IP middleware, which reads `X-Forwarded-For`, `X-Real-IP` and `True-Client-IP`, so a proxy in front of ws must overwrite those headers, or a client could claim a tailnet address; the owner check still applies. And the terminal only exists where the server can run `tmux` and `ssh`: the `ws` container image has an `ssh` client but no `tmux`, so the view works for ssh targets (see the launcher setup) and not for a local tmux. The view is `tmux attach -r` on a session of its own: bytes flow to the browser and only the window size comes back, nothing is stored, and no prompt, output or credential goes in the database.

## Imported skills

The new-agent form can read one `SKILL.md` file (a YAML frontmatter with a name and description, and a markdown body) into a preview. The file is untrusted text, and what protects you is mostly what ws does not do with it. It reads a local file you pick: no registry, no download, no folder, no auto-update, and a skill is never loaded because its description matches something. It never runs scripts, hooks or installers it mentions. It saves nothing until you read the prompt in the form and press Create. The imported agent gets no tools (`allowed-tools` or `tools` in the file are shown as "asked for, not granted"), so it cannot reach the sandbox, memory writes or the Claude Code lane unless you add those tools yourself. Zero-width and direction-override characters, other invisible format characters and HTML comments are removed and counted, and links, fenced code, instruction-override phrases and ignored keys are listed as warnings; limits are 64 KB for the file and 32 KB for the body.

The label that marks the body as imported content, with ws's rules and the agent's goal placed after it, is a request to the model, not enforcement: there is no separate trust section in a request today, and the block's end marker is not escaped, so a hostile file can close it early. If you then give such an agent write tools and it also reads untrusted input (the form warns about this), a prompt injection in the skill or in what it reads can act through those tools. Licensing of a skill file is not checked; whoever imports it is responsible for having the right to use it. It has been tested only against made-up files, not against skills from a real skill library.

## Training data

Fine-tuning datasets are built by the owner from conversations, and only from users who switched on "Allow my conversations in training datasets" in Settings; it is off for everyone by default, and a user can turn it off at any time. Turning it off keeps new datasets from reading that user's conversations, but it does not remove them from datasets already built. Archived conversations are left out. Nothing is scrubbed: a dataset holds the messages with tool calls and their results verbatim (reasoning is dropped, images and files become placeholders), so secrets or personal data that appeared in a conversation can be in it. Any user with the `owner` role can list, download and delete every dataset and adapter, whoever the conversations belonged to, and the exported files live in the blob store. Deleting a dataset removes its row but not its files. Thumbs up and down on an answer are stored per user and a dataset can use the highest rating any user gave. This is a first cut and has only been tested against fakes.

## Limits

- Sandboxes share the host kernel unless gVisor is installed. Container isolation is not a virtual machine.
- The egress allowlist limits where sandboxes can connect. It does not inspect encrypted traffic.
- Anyone you invite can use the models and budgets you give them. ws is built for one owner and a few trusted people, not for untrusted tenants.
- Passkeys require a domain over https.

## Reporting a security problem

Report it privately through GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**. Please do not open a public issue for a security problem, because an issue is visible to everyone as soon as it is filed. ws is a one-person project with no service-level promise; reports are acknowledged as soon as the maintainer can. The public repository's `SECURITY.md` says the same.
