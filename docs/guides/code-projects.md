# Code projects

A code project gives the assistant a sandbox: a private Linux container with your repository in it, where it can read and edit files, run commands and commit. You watch and steer from the browser.

## Creating one

Choose "New code project" in the sidebar, optionally with an `https://` clone URL. ws creates your sandbox and clones the repository into it when you open the project's panel or the assistant first uses a tool. There is one sandbox per user and project, with a persistent volume at `/workspace`. Code conversations run on the worker, the only process that can talk to Docker, and stream back to your browser.

Sandboxes need Docker reachable from the worker. If Docker is not available the worker logs it and runs without sandboxes, and code projects will not work.

## What the assistant can do

| Tool | Default policy |
|---|---|
| `read_file`, `write_file`, `edit_file`, `list_files`, `grep` | runs on its own |
| `bash` | asks you first |
| `git_status`, `git_diff`, `git_commit` | runs on its own |
| `git_push`, `open_pr` | asks you first |

"Asks" means an approval card appears and the run waits; see [Approvals](approvals.md). `git_push`, `open_pr` and cloning a private repository need a GitHub App or token (below); the other git tools work on the local copy.

## The side panel

Code conversations open with a side panel (you can close it and reopen it, and while it is open it takes the place of the artifact panel). It has three tabs:

- **Files:** a file tree and a CodeMirror editor; Cmd or Ctrl+S saves into the sandbox.
- **Terminal:** a real `bash` in the sandbox.
- **Preview:** you enter a port, and the dev server running on it in the sandbox opens in a new browser tab on its own hostname. The preview domain defaults to `preview.localhost` for local development; a real deployment needs a wildcard DNS name for it. See [Configuration](../CONFIGURATION.md).

## GitHub

With a GitHub App configured (permissions Contents and Pull requests, read and write) and installed on the repository's owner, pushes use a short-lived, repo-scoped installation token that the egress proxy adds. The token never exists inside the container. `open_pr` commits any uncommitted changes, creates a branch named `ws/` plus a short slug of the pull request title if you are on the default branch (the assistant can pass a branch name instead), pushes it and opens the pull request from the worker. A `GITHUB_TOKEN` in the server's environment is a fallback for cloning and pushing whenever no App installation covers the repository, and `open_pr` is unavailable without an App. The settings are in [Configuration](../CONFIGURATION.md).

## Limits

- By default, idle sandboxes stop after 15 minutes, and a stopped container is removed after a week (`WS_SANDBOX_IDLE_STOP`, `WS_SANDBOX_REMOVE_AFTER`). The volume stays, so your working copy survives.
- A sandbox has memory, CPU and process limits, a read-only root filesystem, and no network except through the worker's egress proxy, which allows package registries, GitHub and Go modules (the operator can add hosts with `WS_EGRESS_ALLOW`).
- A sandbox shares the host kernel unless the host has gVisor.

How the isolation works, and where it stops, is in [Trust and security](../TRUST.md).

*Checked against the code at master `3364287`.*
