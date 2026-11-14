# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/cli-ai-gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/cli-ai-client ./cmd/example-client

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

RUN groupadd --system --gid 10001 gateway \
 && useradd --system --uid 10001 --gid gateway --home-dir /home/gateway --create-home --shell /usr/sbin/nologin gateway \
 && mkdir -p /var/lib/cli-gateway/homes/codex \
             /var/lib/cli-gateway/homes/antigravity \
             /var/lib/cli-gateway/homes/gemini \
             /var/lib/cli-gateway/homes/claude \
             /etc/cli-ai-gateway \
 && chown -R gateway:gateway /var/lib/cli-gateway /etc/cli-ai-gateway

COPY --from=build /out/cli-ai-gateway /usr/local/bin/cli-ai-gateway
COPY --from=build /out/cli-ai-client /usr/local/bin/cli-ai-client
COPY configs/config.example.yaml /etc/cli-ai-gateway/config.yaml

USER gateway
WORKDIR /var/lib/cli-gateway
ENV GATEWAY_CONFIG=/etc/cli-ai-gateway/config.yaml

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/cli-ai-gateway", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/cli-ai-gateway"]
