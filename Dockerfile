# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/cli-ai-gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/cli-ai-client ./cmd/example-client

FROM debian:bookworm-slim AS base-runtime

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git tzdata \
 && rm -rf /var/lib/apt/lists/*

RUN groupadd --system --gid 10001 gateway \
 && useradd --system --uid 10001 --gid gateway --home-dir /var/lib/cli-gateway/home --shell /bin/bash gateway \
 && mkdir -p /var/lib/cli-gateway/home /etc/cli-ai-gateway \
 && chown -R gateway:gateway /var/lib/cli-gateway /etc/cli-ai-gateway

COPY --from=build /out/cli-ai-gateway /usr/local/bin/cli-ai-gateway

# Thin HTTP gateway router & Web Admin (holds zero CLI binaries locally).
FROM base-runtime AS gateway

COPY --from=build /out/cli-ai-client /usr/local/bin/cli-ai-client
COPY configs/config.docker.yaml /etc/cli-ai-gateway/config.yaml

USER gateway
WORKDIR /var/lib/cli-gateway
ENV GATEWAY_CONFIG=/etc/cli-ai-gateway/config.yaml \
    HOME=/var/lib/cli-gateway/home

EXPOSE 7070

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/cli-ai-gateway", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/cli-ai-gateway"]

# Shared worker HTTP server runtime (runs single CLI agent per container).
FROM base-runtime AS worker-base

WORKDIR /var/lib/cli-gateway
ENV GATEWAY_MODE=worker \
    GATEWAY_WORKER_LISTEN=:9090 \
    GATEWAY_WORKER_HOME=/var/lib/cli-gateway/home \
    GATEWAY_WORKER_FS_ACCESS=workspace-read \
    GATEWAY_HEALTHCHECK_URL=http://127.0.0.1:9090/worker/health

EXPOSE 9090

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/cli-ai-gateway", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/cli-ai-gateway", "-worker"]

# Shared Node.js 22 base for npm-distributed CLI agents (Codex CLI, Gemini CLI).
FROM worker-base AS worker-node-base

RUN apt-get update \
 && apt-get install -y --no-install-recommends gnupg ripgrep \
 && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

# OpenAI Codex CLI worker
FROM worker-node-base AS worker-codex

RUN npm install -g @openai/codex \
 && npm cache clean --force

USER gateway
ENV HOME=/var/lib/cli-gateway/home \
    CODEX_HOME=/var/lib/cli-gateway/home/.codex \
    GATEWAY_WORKER_PROVIDER=codex \
    GATEWAY_WORKER_EXECUTABLE=codex \
    GATEWAY_WORKER_SANDBOX_MODE=read-only

# Google Gemini CLI worker
FROM worker-node-base AS worker-gemini

RUN npm install -g @google/gemini-cli \
 && npm cache clean --force

USER gateway
ENV HOME=/var/lib/cli-gateway/home \
    GATEWAY_WORKER_PROVIDER=gemini \
    GATEWAY_WORKER_EXECUTABLE=gemini

# Anthropic Claude Code CLI worker
FROM worker-base AS worker-claude

RUN apt-get update \
 && apt-get install -y --no-install-recommends ripgrep \
 && rm -rf /var/lib/apt/lists/* \
 && HOME=/root bash -c 'curl -fsSL https://claude.ai/install.sh | bash' \
 && install -m 755 "$(readlink -f /root/.local/bin/claude)" /usr/local/bin/claude \
 && rm -rf /root/.local /root/.claude

USER gateway
ENV HOME=/var/lib/cli-gateway/home \
    DISABLE_AUTOUPDATER=1 \
    GATEWAY_WORKER_PROVIDER=claude \
    GATEWAY_WORKER_EXECUTABLE=claude \
    GATEWAY_WORKER_ALLOWED_TOOLS=Read

# Antigravity CLI (agy) worker — instantiated once per account container (agy-1, agy-2, agy-3, ...)
FROM worker-base AS worker-agy

RUN mkdir -p /opt/agy \
 && HOME=/opt/agy bash -c 'curl -fsSL https://antigravity.google/cli/install.sh | bash' \
 && rm -rf /opt/agy/.cache \
 && chmod -R a+rX /opt/agy \
 && ln -sf "$(readlink -f /opt/agy/.local/bin/agy)" /usr/local/bin/agy

USER gateway
ENV HOME=/var/lib/cli-gateway/home \
    GATEWAY_WORKER_PROVIDER=antigravity \
    GATEWAY_WORKER_EXECUTABLE=agy \
    GATEWAY_WORKER_SANDBOX_MODE=strict
