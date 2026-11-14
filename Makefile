.DEFAULT_GOAL := help

DC ?= docker compose
BINARY := bin/cli-ai-gateway
CLIENT_BINARY := bin/cli-ai-client
SERVICE ?= gateway
ACCOUNT ?= 1

.PHONY: help init env up down restart ps logs \
        claude-login codex-login gemini-login agy-login agy-1-login agy-2-login agy-3-login \
        docker-build docker-up docker-down docker-restart docker-logs \
        build run local-start local-stop local-restart local-status local-logs \
        test test-verbose lint fmt clean

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*## "; printf "\n\033[1mCLI AI Gateway — Commands\033[0m\n"} \
	  /^##@/ {printf "\n\033[36m%s\033[0m\n", substr($$0, 5); next} \
	  /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[33m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@printf "\nExamples:\n  make init && make up\n  make claude-login\n  make agy-login ACCOUNT=2\n  make agy-3-login\n  make logs SERVICE=claude\n\n"

##@ Docker Stack (Recommended)
init: env ## Create .env with generated random tokens (idempotent)

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

up: init ## Build images and start gateway + all CLI worker containers
	$(DC) up -d --build

down: ## Stop and remove containers (preserves persistent auth volumes)
	$(DC) down

restart: ## Restart containers (override SERVICE=agy-1 to restart one)
	$(DC) restart $(SERVICE)

ps: ## Show status and health of all containers
	$(DC) ps

logs: ## Follow logs (override SERVICE=claude|codex|gemini|agy-1|...)
	$(DC) logs -f $(SERVICE)

docker-build: ## Build all Docker images without starting
	$(DC) build

docker-up: up ## Alias for up

docker-down: down ## Alias for down

docker-restart: ## Restart gateway container
	$(DC) restart gateway

docker-logs: logs ## Alias for logs

##@ Agent Authentication (Inside Worker Containers)
claude-login: ## Authenticate Claude Code CLI inside the 'claude' container
	$(DC) exec -it claude claude auth login

codex-login: ## Authenticate Codex CLI inside the 'codex' container (device auth)
	$(DC) exec -it codex codex login --device-auth

gemini-login: ## Authenticate Gemini CLI inside the 'gemini' container
	$(DC) exec -it -e NO_BROWSER=true gemini gemini

agy-login: ## Authenticate Antigravity worker (pass ACCOUNT=1|2|3, default 1)
	$(DC) exec -it agy-$(ACCOUNT) agy

agy-1-login: ## Authenticate Antigravity container agy-1
	@$(MAKE) agy-login ACCOUNT=1

agy-2-login: ## Authenticate Antigravity container agy-2
	@$(MAKE) agy-login ACCOUNT=2

agy-3-login: ## Authenticate Antigravity container agy-3
	@$(MAKE) agy-login ACCOUNT=3

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
