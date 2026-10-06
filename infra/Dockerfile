# Multi-arch build: docker buildx build --platform linux/arm64,linux/amd64 -f infra/Dockerfile .
# Stage 1: frontend
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Go binary (cross-compiled on the build host)
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ws ./cmd/ws
# Blob root owned by the runtime user (uid 65532). A named volume mounted
# here inherits this ownership when Docker first creates it; without it the
# volume is root-owned and `ws serve` dies with "blob: mkdir: permission denied".
RUN mkdir -p /out/data/blobs

# Stage 3: runtime. Debian slim rather than distroless so the image has an
# ssh client: the Claude Code launcher (spawn_job's subscription lane and
# the Jobs tab's refresh and terminal view) reaches its targets over ssh
# with a key and ssh_config mounted read-only under /secrets (see
# infra/launcher/README.md). tmux and claude stay on the targets; nothing
# in the image launches Claude Code locally. Still non-root, same uid as
# distroless' nonroot so existing volumes keep working.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates openssh-client \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 65532 nonroot && useradd -u 65532 -g 65532 -m -d /home/nonroot -s /usr/sbin/nologin nonroot
WORKDIR /app
COPY --from=build /out/ws /app/ws
COPY --from=build --chown=65532:65532 /out/data /data
COPY config /app/config
ENV WS_ENDPOINTS_FILE=/app/config/endpoints.yaml \
    WS_POLICIES_DIR=/app/config/policies \
    WS_BLOB_DIR=/data/blobs \
    HOME=/home/nonroot \
    XDG_CACHE_HOME=/tmp/ws-cache
VOLUME ["/data"]
EXPOSE 8080 8081
USER nonroot
ENTRYPOINT ["/app/ws"]
CMD ["serve"]
