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

Sign-in is by passkey or magic link; there are no passwords. Registration is invite-only. API keys start with `ws_` and are stored as hashes, shown once when created. A key carries scopes: `chat` (the default) lets it use the `/v1` gateway, and `mcp` lets it use ws as an MCP server; a key reaches nothing else. Each model call is recorded in a usage ledger under the key, user and route, and budgets can block or downgrade calls. When Claude Code or another Anthropic client uses ws as its gateway and a request lands on a native Anthropic endpoint, ws proxies it byte for byte: your `ws_` key is checked and not forwarded, ws sends its own provider key upstream, and the provider's response is relayed unchanged. The client's session id header, if sent, is stored on the ledger row.

## Claude Code and subscriptions

ws never reads, stores or forwards a Claude subscription credential, and never parses Claude Code's output. Claude Code on a subscription is launched only by the separate `wsj` CLI, which starts the official `claude` binary in a real terminal (tmux) and returns a handle. Programmatic use of Claude goes through an API key and the gateway. The task router's `spawn_job` tool is off by default (it only decides and logs a routing decision). When it is switched on, it refuses the subscription lane for anyone but the owner, and prompts that look sensitive are never sent to OpenRouter or the subscription lane. A prompt the rules cannot place and did not flag as sensitive may be shown to a small model for classification: by default gpt-oss-120b on Cerebras through OpenRouter, with local models behind it, and no other hosted model. Anything flagged as sensitive never leaves the box.

## The Claude Code job tab

The Jobs page lists the Claude Code sessions `wsj` has reported and can show one as a read-only terminal. The routes are owner-only and, by default, answer only to the tailnet and loopback (`WS_CC_WEB_ALLOW`). Two things limit how far that goes. The network check uses the address the server sees after its real-IP middleware, which reads `X-Forwarded-For`, `X-Real-IP` and `True-Client-IP`, so a proxy in front of ws must overwrite those headers, or a client could claim a tailnet address; the owner check still applies. And the terminal only exists where the server can run `tmux` and `ssh`: the `ws` container image has neither. The view is `tmux attach -r` on a session of its own: bytes flow to the browser and only the window size comes back, nothing is stored, and no prompt, output or credential goes in the database.

## Limits

- Sandboxes share the host kernel unless gVisor is installed. Container isolation is not a virtual machine.
- The egress allowlist limits where sandboxes can connect. It does not inspect encrypted traffic.
- Anyone you invite can use the models and budgets you give them. ws is built for one owner and a few trusted people, not for untrusted tenants.
- Passkeys require a domain over https.

## Reporting a security problem

Report it privately through GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**. Please do not open a public issue for a security problem, because an issue is visible to everyone as soon as it is filed. ws is a one-person project with no service-level promise; reports are acknowledged as soon as the maintainer can. The public repository's `SECURITY.md` says the same. Until the public repository exists, report to the maintainer directly.
