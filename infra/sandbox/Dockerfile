# ws-sandbox: the coding sandbox image. Debian slim with git, Node, Go,
# Python, ripgrep and build tools, running as uid 1000 ("dev"). The
# container gets a read-only root; /workspace is the persistent volume,
# /tmp and /home/dev are tmpfs. No credentials are ever baked in; egress
# goes through the worker's proxy. Multi-arch (amd64, arm64).
FROM debian:bookworm-slim
ARG TARGETARCH
ARG NODE_VERSION=22.14.0
ARG GO_VERSION=1.26.0

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq less procps unzip xz-utils zip \
      build-essential pkg-config \
      python3 python3-pip python3-venv \
      ripgrep openssh-client \
    && rm -rf /var/lib/apt/lists/*

# Node (official tarball per arch).
RUN set -eux; case "$TARGETARCH" in amd64) a=x64;; arm64) a=arm64;; *) echo "unsupported arch $TARGETARCH" && exit 1;; esac; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${a}.tar.xz" | tar -xJ -C /usr/local --strip-components=1; \
    node --version && npm --version

# Go.
RUN set -eux; curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -xz -C /usr/local; ln -s /usr/local/go/bin/go /usr/local/bin/go; ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt; go version

RUN useradd -m -u 1000 -s /bin/bash dev && mkdir -p /workspace && chown dev:dev /workspace
ENV PATH=/usr/local/go/bin:/home/dev/go/bin:/home/dev/.local/bin:/usr/local/bin:/usr/bin:/bin \
    GOPATH=/home/dev/go GOFLAGS=-buildvcs=false GOTOOLCHAIN=local \
    NPM_CONFIG_CACHE=/tmp/npm-cache PIP_CACHE_DIR=/tmp/pip-cache GOCACHE=/tmp/go-cache GOMODCACHE=/tmp/go-mod \
    PYTHONDONTWRITEBYTECODE=1

USER dev
WORKDIR /workspace
CMD ["sleep", "infinity"]
