.DEFAULT_GOAL := help

DC := docker compose -f compose.yaml $(if $(wildcard compose.workers.yaml),-f compose.workers.yaml)
BINARY := bin/cli-ai-gateway
CLIENT_BINARY := bin/cli-ai-client
SERVICE ?= gateway
ACCOUNT ?= 1

.PHONY: help init env up down restart ps logs \
        agent-ls agent-add agent-rm agent-login \
        claude-login codex-login gemini-login agy-login \
        docker-build docker-up docker-down docker-restart docker-logs \
        build run local-start local-stop local-restart local-status local-logs \
        test test-verbose lint fmt clean

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*## "; printf "\n\033[1mCLI AI Gateway — Commands\033[0m\n"} \
	  /^##@/ {printf "\n\033[36m%s\033[0m\n", substr($$0, 5); next} \
	  /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[33m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@printf "\nExamples (any number of isolated containers per agent):\n"
	@printf "  make init                                        # Create .env + default containers (override CLAUDE_COUNT/CODEX_COUNT/AGY_COUNT)\n"
	@printf "  CLAUDE_COUNT=2 CODEX_COUNT=3 AGY_COUNT=5 make init\n"
	@printf "  make up                                          # Build images and start gateway + all worker containers\n"
	@printf "  make agent-add TYPE=agy COUNT=2                  # Add agy-4, agy-5 containers on the fly\n"
	@printf "  make agent-add TYPE=codex                        # Add codex-2 (or TYPE=claude NAME=claude-work)\n"
	@printf "  make claude-login ACCOUNT=2                      # Authenticate claude-2 inside its container\n"
	@printf "  make codex-login ACCOUNT=2                       # Authenticate codex-2 inside its container\n"
	@printf "  make agy-login ACCOUNT=4                         # Authenticate agy-4 inside its container\n"
	@printf "  make agent-login NAME=claude-work                # Authenticate any custom-named worker container\n"
	@printf "  make agent-ls                                    # List all worker containers, URLs, and auth volumes\n\n"

##@ Docker Stack (Recommended)
init: env ## Create .env (secrets) & initial worker pool (.workers -> compose.workers.yaml)
	@chmod +x scripts/workers.sh
	@$(if $(CLAUDE_COUNT),CLAUDE_COUNT="$(CLAUDE_COUNT)") $(if $(CODEX_COUNT),CODEX_COUNT="$(CODEX_COUNT)") $(if $(GEMINI_COUNT),GEMINI_COUNT="$(GEMINI_COUNT)") $(if $(AGY_COUNT),AGY_COUNT="$(AGY_COUNT)") ./scripts/workers.sh init

env:
	@if [ -f .env ]; then \
		echo ".env already exists, keeping existing secrets"; \
	else \
		cp .env.example .env; \
		gen_tok() { (openssl rand -hex 24 2>/dev/null || (LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 48)); }; \
		API_KEY="gw-key-$$(gen_tok)"; \
		ADMIN_TOK="gw-adm-$$(gen_tok)"; \
		WORKER_TOK="gw-wrk-$$(gen_tok)"; \
		tmp=$$(mktemp); \
		awk -v ak="$$API_KEY" -v ad="$$ADMIN_TOK" -v wk="$$WORKER_TOK" '\
			/^GATEWAY_API_KEY=/ {print "GATEWAY_API_KEY=" ak; next} \
			/^GATEWAY_ADMIN_TOKEN=/ {print "GATEWAY_ADMIN_TOKEN=" ad; next} \
			/^GATEWAY_WORKER_TOKEN=/ {print "GATEWAY_WORKER_TOKEN=" wk; next} \
			{print}' .env > "$$tmp" && mv "$$tmp" .env; \
		echo "Created .env with generated GATEWAY_API_KEY, GATEWAY_ADMIN_TOKEN, GATEWAY_WORKER_TOKEN"; \
	fi

up: init ## Build images and start gateway + all worker containers
	$(DC) up -d --build

down: ## Stop and remove containers (preserves persistent auth volumes)
	$(DC) down

restart: ## Restart containers (override SERVICE=agy-4 to restart one)
	$(DC) restart $(SERVICE)

ps: ## Show status and health of all containers
	$(DC) ps

logs: ## Follow logs (override SERVICE=codex-2|claude-1|agy-4|...)
	$(DC) logs -f $(SERVICE)

docker-build: init ## Build all Docker images without starting
	$(DC) build

docker-up: up ## Alias for up

docker-down: down ## Alias for down

docker-restart: ## Restart gateway container
	$(DC) restart gateway

docker-logs: logs ## Alias for logs

##@ Multi-Container Worker Pool & Auth
agent-ls: ## List all worker containers, worker_urls, and auth volumes
	@./scripts/workers.sh ls

agent-add: ## Add isolated container(s): TYPE=agy|codex|claude|gemini [NAME=...] [SLOT=...] [COUNT=1]
	@TYPE="$(TYPE)" NAME="$(NAME)" SLOT="$(SLOT)" COUNT="$(COUNT)" ./scripts/workers.sh add

agent-rm: ## Remove a worker container: NAME=<container> [PURGE_VOLUME=1]
	@NAME="$(NAME)" PURGE_VOLUME="$(PURGE_VOLUME)" ./scripts/workers.sh rm

agent-login: ## Run interactive login in any container: NAME=<container> (or TYPE=... ACCOUNT=...)
	@NAME="$(NAME)" TYPE="$(TYPE)" ACCOUNT="$(ACCOUNT)" ./scripts/workers.sh login

claude-login: ## Authenticate Claude container (ACCOUNT=1|2|... or NAME=...)
	@TYPE=claude ACCOUNT="$(ACCOUNT)" NAME="$(NAME)" ./scripts/workers.sh login

codex-login: ## Authenticate Codex container (ACCOUNT=1|2|... or NAME=...)
	@TYPE=codex ACCOUNT="$(ACCOUNT)" NAME="$(NAME)" ./scripts/workers.sh login

gemini-login: ## Authenticate Gemini container (ACCOUNT=1|2|... or NAME=...)
	@TYPE=gemini ACCOUNT="$(ACCOUNT)" NAME="$(NAME)" ./scripts/workers.sh login

agy-login: ## Authenticate Antigravity container (ACCOUNT=1|2|3|4|... or NAME=...)
	@TYPE=agy ACCOUNT="$(ACCOUNT)" NAME="$(NAME)" ./scripts/workers.sh login

##@ Local Go Development & Tests
build: ## Build Go gateway and example client binaries locally
	@mkdir -p bin
	go build -o $(BINARY) ./cmd/gateway
	go build -o $(CLIENT_BINARY) ./cmd/example-client

run: build ## Run gateway locally against configs/config.example.yaml
	$(BINARY) -config configs/config.example.yaml

local-start: build ## Start local background daemon via scripts/gateway.sh
	./scripts/gateway.sh start

local-stop: ## Stop local background daemon
	./scripts/gateway.sh stop

local-restart: build ## Restart local background daemon
	./scripts/gateway.sh restart

local-status: ## Check local daemon status
	./scripts/gateway.sh status

local-logs: ## Tail local daemon logs
	./scripts/gateway.sh logs

test: ## Run Go test suite with race detector
	go test -race ./...

test-verbose: ## Run Go test suite with verbose output
	go test -race -v ./...

lint: ## Run go vet
	go vet ./...

fmt: ## Format Go sources
	gofmt -w cmd internal pkg tests

clean: ## Remove local build artifacts (does not touch Docker volumes)
	./scripts/gateway.sh stop >/dev/null 2>&1 || true
	rm -rf bin data
