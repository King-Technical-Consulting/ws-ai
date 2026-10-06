# Deploying ws with Compose

One base file, one hardware override, one ingress profile.

| File | Purpose |
|---|---|
| `compose.yaml` | Postgres, `app` (API + web + artifact origin), `worker`, the two ingress sidecars behind profiles (`cloudflare`, `tailscale`), and the `mcp-infisical` sidecar behind the `infisical` profile (`WS_DEPLOY_PROFILES=infisical`) |
| `override.orin-cuda.yaml` | Jetson AGX Orin: llama-server with CUDA, optional embedding server |
| `override.amd-rocm.yaml` | AMD W7700 box: llama-server with Vulkan |
| `override.spark-cuda.yaml` | DGX Spark: vLLM, optional ComfyUI |
| `override.cloud.yaml` | Web host only, hosted models (Anthropic, OpenRouter, OpenAI); no local inference |
| `override.cpu.yaml` | Any Linux box, CPU only, small model, for testing the stack |
| `override.dev.yaml` | Laptop: Postgres only |
| `override.traefik.yaml` | Labels for an existing Traefik (`INGRESS=traefik`): `WS_HOST` and `WS_ART_HOST` on the `websecure` entrypoint over the external `traefik-net` |
| `tailscale-serve.json` | Tailscale Serve mapping for the `tailscale` profile |

From the repo root, `make up HW=orin-cuda INGRESS=cloudflare` expands to the right `docker compose` command. `make logs`, `make down`, `make ps` take the same variables. If you call `docker compose` by hand, run it from the repo root and pass `--env-file .env` so the root `.env` also drives `${VAR}` interpolation (ports, passwords, image tag); the containers' `env_file` is separate.

## First deploy

1. **Host prerequisites.** Docker Engine 24+ with the compose plugin. For NVIDIA: `nvidia-container-toolkit` (preinstalled on JetPack and DGX OS). For AMD: `amdgpu` driver and your user in `video` and `render`. Check JetPack is 6.x on the Orin: `cat /etc/nv_tegra_release`.
2. **Clone and configure.**
   ```bash
   git clone https://github.com/King-Technical-Consulting/ws-ai && cd ws-ai
   cp .env.example .env
   ```
   Set in `.env`: `WS_ENV=prod`, a long random `WS_SESSION_SECRET`, `POSTGRES_PASSWORD`, `WS_OWNER_EMAIL`, `WS_PUBLIC_URL` and `WS_ARTIFACT_URL` for your hostnames, `RESEND_API_KEY`, any provider keys, and the ingress token. Hosted providers without a key are skipped; local endpoints come from the override.
3. **Models.** For llama-server, download GGUF files into `data/models/` (commands in `infra/inference/README.md`). vLLM pulls from Hugging Face into `data/hf/` on first start.
4. **Ingress.** One of:
   - **Existing Traefik** (homelab): `INGRESS=traefik`. Set `WS_HOST` and `WS_ART_HOST` (two hostnames under your wildcard cert's domain, e.g. `ws.home.arpa` and `ws-art.home.arpa`), `WS_PUBLIC_URL`/`WS_ARTIFACT_URL` to their `https://` forms, `WS_ENV=prod`, `WS_RP_ID=<WS_HOST>`, and point both names at the box in DNS. Passkeys work because the cert is trusted on your devices. For sandbox previews also set `WS_PREVIEW_HOST=ws-p-{port}-{id}.<domain>` and `WS_PREVIEW_PARENT=<domain>`.
   - **Cloudflare Tunnel**: create a remote-managed tunnel in Zero Trust, copy its token to `CLOUDFLARE_TUNNEL_TOKEN`, and add public hostnames `app.<domain>` → `http://app:8080`, `art.<domain>` → `http://app:8081`, `*.preview.<domain>` → `http://app:8080`. Set `WS_PUBLIC_URL=https://app.<domain>` and `WS_ARTIFACT_URL=https://art.<domain>`, and `WS_PREVIEW_DOMAIN=preview.<domain>` so sandbox previews use the `*.preview.<domain>` route (the default, `preview.localhost`, does not).
   - **Tailscale**: create a tagged auth key (`tag:ws`) with HTTPS enabled on the tailnet, set `TS_AUTHKEY`. The sidecar serves `https://ws.<tailnet>.ts.net` and `:8443` for artifacts. Set the two URLs accordingly. Friends must join your tailnet.
5. **Start.**
   ```bash
   make up HW=orin-cuda INGRESS=cloudflare
   make logs
   ```
   The app migrates the database, then prints the owner invite:
   ```
   OWNER INVITE created (open this link to set up the first account) link=https://app.example.com/invite/...
   ```
   Open it, create your account, add a passkey. Invite friends from Admin.

## Continuous delivery

```bash
make install-cd HW=cloud INGRESS=traefik
```

installs a user systemd timer that runs `infra/compose/ws-deploy.sh` every two minutes: `git pull --ff-only`, `compose pull`, `compose up -d`. Compose only recreates containers whose image or config changed, so the run is a no-op most of the time, and when CI publishes a new image after a merge the app and worker are replaced within a couple of minutes with the full Compose config (networks, labels, env). Because the repo is pulled too, changes to the Compose files roll out the same way; only `.env` edits remain manual, since `.env` is not in git.

`install-cd` records `WS_DEPLOY_HW` and `WS_DEPLOY_INGRESS` in `.env`, and every `make` target that runs Compose (`up`, `down`, `logs`, `ps`, `pull`, `config`, `backup`, `db-up`, `db-down`) defaults to them when `HW`/`INGRESS` aren't passed. So on a deployed box a bare `make up` applies the same files the timer does. Avoid running `make up` by hand in the same minute the timer fires; Compose will report the container the timer is recreating as "exited (0)". `make deploy` runs the timer's script once, which is the better way to force a rollout.

Run `sudo loginctl enable-linger $USER` once so the timer keeps running after you log out. `make cd-logs` shows recent runs, `make deploy` runs one now, `make uninstall-cd` removes the timer. Pinning `WS_IMAGE` to a SHA in `.env` turns image updates off while keeping config updates.

Watchtower was tried first and dropped: it recreates containers with only their first network, which detached `app` from `traefik-net` and took the site down.

## Day two

- **Upgrade** without CD: `make pull && make up HW=... INGRESS=...`. Images are published to `ghcr.io/king-technical-consulting/ws` on every merge to master that changes code (docs-only merges run no CI and publish nothing), tagged `latest` and by commit SHA. Pin `WS_IMAGE=ghcr.io/king-technical-consulting/ws:<sha>` in `.env` for reproducible deploys.
- **Backups**: `make backup` writes `data/backups/ws-<date>-<time>.sql.gz` with `pg_dump`. Blobs live in the `blobs` volume (`docker volume inspect ws_blobs`); back up that directory too. A cron line that runs `make backup` nightly is enough for a household.
- **Where state lives**: Postgres in the `pgdata` volume, uploads and artifacts in `blobs`, models in `data/models` or `data/hf` bind mounts, Tailscale identity in `tsstate`.
- **`blob: mkdir: permission denied` on start**: the `blobs` volume predates the image that creates `/data/blobs` owned by the container's non-root user (uid 65532), so Docker created it root-owned. One-time repair, then the app's restart loop recovers on its own: `docker run --rm -v ws_blobs:/b busybox chown -R 65532:65532 /b`.
- **Changing models**: edit the override's `--model` and `--alias`, make sure `config/endpoints.yaml` has an endpoint whose `model:` matches the alias, then `make up`. Enable or disable endpoints in Admin without restarting.
- **Logs**: json-file, rotated at 20 MB × 5 per container.
- **Health**: `make ps` shows the healthchecks; `app` is probed by the binary itself (`ws health`), inference by its `/health`.

## Security notes

- Published ports are bound to loopback (`127.0.0.1`) by default: Postgres (5432), the inference containers (llama and vllm on 8000, llama-embed on 8001, ComfyUI on 8188) and, with `override.cloud.yaml`, the app on `WS_HTTP_PORT` and `WS_ART_PORT` at `WS_BIND_ADDR`. Setting `WS_BIND_ADDR` to a LAN or tailnet address exposes the app on that address; keep it loopback when using the tunnel. Everything else is reached through the tunnel or the tailnet.
- Artifacts are served from a separate hostname (`art.`) so the browser treats them as a different origin. Do not point `art.` and `app.` at the same hostname.
- The worker mounts the Docker socket for coding sandboxes; `app` never does. The worker also joins the internal `sandbox-net` (Compose names it `ws_sandbox-net`, which is the `WS_SANDBOX_NETWORK` default) so sandboxes can reach its egress proxy at `http://worker:3128`. Sandbox containers are named `ws-sb-*`, labeled `ws.sandbox`, and are not part of the Compose project, so `make down` leaves them; `docker ps --filter label=ws.sandbox` lists them. The sandbox image is pulled from `ghcr.io/king-technical-consulting/ws-sandbox:latest` on first use (CI publishes it next to the app image).
- Secrets stay in `.env` on the host. Never commit it.
