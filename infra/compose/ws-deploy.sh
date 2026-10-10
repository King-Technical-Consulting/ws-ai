#!/usr/bin/env bash
# Continuous delivery for a ws checkout: fast-forward the repo, pull images,
# and apply through Compose so networks, labels and env always come from the
# compose files. Idempotent; a no-op when nothing changed. Run it from a
# systemd timer (make install-cd) or by hand.
#
# Reads HW / INGRESS from .env (WS_DEPLOY_HW, WS_DEPLOY_INGRESS) or the
# environment, so the timer needs no arguments.
set -euo pipefail
cd "$(dirname "$0")/../.."

# One deploy at a time: the systemd timer and a `make deploy` by hand would
# otherwise recreate the same containers at once and one of them fails on
# the port binding. The second run waits for the first, then applies again
# (a no-op when the first already brought everything up).
lock="${XDG_RUNTIME_DIR:-/tmp}/ws-deploy.lock"
exec 9>"$lock"
if ! flock -w 600 9; then
  printf '%s ws-deploy: another deploy has held %s for 10 minutes; giving up\n' "$(date -u +%FT%TZ)" "$lock" >&2
  exit 1
fi

if [ -f .env ]; then
  HW="${HW:-$(grep -E '^WS_DEPLOY_HW=' .env | cut -d= -f2- || true)}"
  INGRESS="${INGRESS:-$(grep -E '^WS_DEPLOY_INGRESS=' .env | cut -d= -f2- || true)}"
fi
HW="${HW:-cloud}"
INGRESS="${INGRESS:-}"

log() { printf '%s ws-deploy: %s\n' "$(date -u +%FT%TZ)" "$*"; }

before_repo=$(git rev-parse HEAD)
if git pull -q --ff-only origin "$(git rev-parse --abbrev-ref HEAD)"; then
  after_repo=$(git rev-parse HEAD)
  [ "$before_repo" != "$after_repo" ] && log "repo $before_repo -> $after_repo"
else
  log "git pull failed (local changes?); deploying current checkout"
fi

image_before=$(docker image inspect "${WS_IMAGE:-ghcr.io/king-technical-consulting/ws:latest}" --format '{{.Id}}' 2>/dev/null || echo none)
make -s pull HW="$HW" INGRESS="$INGRESS" >/dev/null 2>&1 || log "pull failed; applying what we have"
image_after=$(docker image inspect "${WS_IMAGE:-ghcr.io/king-technical-consulting/ws:latest}" --format '{{.Id}}' 2>/dev/null || echo none)
[ "$image_before" != "$image_after" ] && log "image ${image_before:7:12} -> ${image_after:7:12}"

# `up -d` only recreates containers whose config or image changed.
out=$(make -s up HW="$HW" INGRESS="$INGRESS" 2>&1) || { log "up failed: $out"; exit 1; }
changed=$(printf '%s\n' "$out" | grep -cE "Recreate|Created|Started" || true)
if [ "$changed" -gt 0 ]; then
  log "applied: $(printf '%s\n' "$out" | grep -E "Recreated|Started" | awk '{print $2}' | sort -u | tr '\n' ' ')"
  docker image prune -f --filter "label=org.opencontainers.image.source=https://github.com/King-Technical-Consulting/ws" >/dev/null 2>&1 || true
fi
exit 0
