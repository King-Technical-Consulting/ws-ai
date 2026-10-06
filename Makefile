SHELL := /bin/bash
GO ?= go
BIN := bin/ws

# Deployment: HW selects infra/compose/override.<HW>.yaml, INGRESS a profile.
#   make up HW=orin-cuda INGRESS=cloudflare
#   make install-cd HW=cloud INGRESS=traefik     (systemd timer: git pull, pull, up every 2 min)
# When not passed, both default to WS_DEPLOY_HW / WS_DEPLOY_INGRESS from .env
# (written by install-cd), so a bare `make up` on a deployed box applies the
# same files the CD timer does instead of silently dropping ingress labels.
HW ?= $(or $(shell grep -sE '^WS_DEPLOY_HW=' .env | cut -d= -f2-),dev)
INGRESS ?= $(shell grep -sE '^WS_DEPLOY_INGRESS=' .env | cut -d= -f2-)
# Extra Compose profiles, comma-separated (e.g. infisical); also from .env.
PROFILES ?= $(shell grep -sE '^WS_DEPLOY_PROFILES=' .env | cut -d= -f2- | tr ',' ' ')
# --env-file makes the repo-root .env drive ${VAR} interpolation too; without it
# Compose would read infra/compose/.env (which doesn't exist) and use defaults.
# INGRESS=traefik is an override file (labels on app); cloudflare/tailscale are profiles.
# The worker needs the host's docker group to use the socket as non-root.
DOCKER_GID ?= $(shell stat -c %g /var/run/docker.sock 2>/dev/null || stat -f %g /var/run/docker.sock 2>/dev/null || echo 999)
export DOCKER_GID
COMPOSE := docker compose --env-file .env -f infra/compose/compose.yaml -f infra/compose/override.$(HW).yaml $(if $(filter traefik,$(INGRESS)),-f infra/compose/override.traefik.yaml,) $(if $(filter cloudflare tailscale,$(INGRESS)),--profile $(INGRESS),) $(foreach p,$(PROFILES),--profile $(p))

.PHONY: all build wsj wsj-install dev serve worker test lint migrate sqlc web web-dev image db-up db-down tidy up down pull logs ps backup config install-cd uninstall-cd deploy cd-logs

all: build

build: web
	$(GO) build -o $(BIN) ./cmd/ws

# wsj: the Mac CLI that runs Claude Code sessions in tmux on local/ssh
# targets (docs/CLAUDE_CODE_JOBS.md). No frontend, so it builds in a second.
wsj:
	$(GO) build -o bin/wsj ./cmd/wsj

wsj-install:
	$(GO) install ./cmd/wsj

# Dev: Postgres in Docker, Go binary native, Vite dev server proxying to it.
dev: db-up
	@echo "Run in three terminals:"
	@echo "  make serve     # Go API on :8080 (+ artifacts :8081)"
	@echo "  make worker    # River worker"
	@echo "  make web-dev   # Vite on :5173 proxying /api to :8080"

serve:
	$(GO) run ./cmd/ws serve

worker:
	$(GO) run ./cmd/ws worker

migrate:
	$(GO) run ./cmd/ws migrate up

migrate-down:
	$(GO) run ./cmd/ws migrate down

test:
	$(GO) test ./...
	@if [ -d web/node_modules ]; then cd web && npm test; fi

lint:
	$(GO) vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || true

sqlc:
	@command -v sqlc >/dev/null || (echo "install sqlc: brew install sqlc" && exit 1)
	sqlc generate

web:
	cd web && npm ci && npm run build

web-dev:
	cd web && npm run dev

db-up:
	$(COMPOSE) up -d postgres

db-down:
	$(COMPOSE) down

# ---- deployment on a GPU box ----
up:
	@echo "compose up (HW=$(HW) INGRESS=$(INGRESS))"
	$(COMPOSE) up -d --remove-orphans

down:
	$(COMPOSE) down

pull:
	$(COMPOSE) pull

logs:
	$(COMPOSE) logs -f --tail=200 app worker

ps:
	$(COMPOSE) ps

config:
	$(COMPOSE) config --quiet && echo "compose config OK (HW=$(HW) INGRESS=$(INGRESS))"

# Continuous delivery: a user systemd timer runs ws-deploy.sh every 2 minutes.
# Records HW/INGRESS in .env so the timer needs no arguments. Linux only.
install-cd:
	@grep -q '^WS_DEPLOY_HW=' .env && sed -i 's/^WS_DEPLOY_HW=.*/WS_DEPLOY_HW=$(HW)/' .env || echo 'WS_DEPLOY_HW=$(HW)' >> .env
	@grep -q '^WS_DEPLOY_INGRESS=' .env && sed -i 's/^WS_DEPLOY_INGRESS=.*/WS_DEPLOY_INGRESS=$(INGRESS)/' .env || echo 'WS_DEPLOY_INGRESS=$(INGRESS)' >> .env
	chmod +x infra/compose/ws-deploy.sh
	mkdir -p ~/.config/systemd/user
	cp infra/compose/systemd/ws-deploy.service infra/compose/systemd/ws-deploy.timer ~/.config/systemd/user/
	systemctl --user daemon-reload
	systemctl --user enable --now ws-deploy.timer
	@loginctl show-user $$USER -p Linger | grep -q yes || echo "NOTE: run 'sudo loginctl enable-linger $$USER' so the timer runs when you're logged out"
	systemctl --user list-timers ws-deploy.timer --no-pager

uninstall-cd:
	-systemctl --user disable --now ws-deploy.timer
	rm -f ~/.config/systemd/user/ws-deploy.service ~/.config/systemd/user/ws-deploy.timer
	systemctl --user daemon-reload

deploy:
	infra/compose/ws-deploy.sh

cd-logs:
	journalctl --user -u ws-deploy.service -n 50 --no-pager

backup:
	@mkdir -p data/backups
	$(COMPOSE) exec -T postgres pg_dump -U ws -d ws | gzip > data/backups/ws-$$(date +%Y%m%d-%H%M%S).sql.gz
	@ls -la data/backups | tail -1

image:
	docker buildx build --platform linux/arm64,linux/amd64 -f infra/Dockerfile -t ghcr.io/king-technical-consulting/ws:latest .

tidy:
	$(GO) mod tidy
