# CLI AI Gateway

Unified OpenAI-compatible HTTP gateway in Go for executing prompts and large file-based tasks through locally installed AI CLI tools (`Codex CLI`, `Antigravity CLI`, `Gemini CLI`, `Claude CLI`) and local `Ollama` models.

## Architecture Overview

```text
Client -> prompt.md -> POST /v1/files -> POST /v1/responses -> Go Gateway -> CLI Adapter / Ollama -> OpenAI Response
```

- **Single Go binary** using standard library `net/http`, `os/exec`, `context.Context`, `encoding/json`, `mime/multipart`, `log/slog`.
- **SQLite metadata store** (`modernc.org/sqlite`, WAL mode) for API keys (SHA-256 hashes), file metadata, sessions, responses, and runs — large prompt payloads stay on disk, never inside SQLite.
- **Filesystem isolation**: uploaded files in `files/`, conversation transcripts in `sessions/<id>/`, per-run isolated workspaces in `runs/<id>/input/`.
- **Strict process execution**: typed argument slices only (never shell concatenation), process-group cleanup (`Setpgid` + `SIGKILL` on timeout/client disconnect), sanitized environment per executor, bounded line-by-line stdout reading (supports arbitrary JSON line lengths), global/executor/user concurrency limits.

## Project Structure

- `cmd/gateway/main.go` — HTTP server, config loader, startup diagnostics, background cleanup, graceful shutdown.
- `cmd/example-client/main.go` — runnable Go client demonstrating `prompt.md` construction, file upload, streaming/sync Responses API.
- `pkg/client/` — Go client library and deterministic `PromptDocument` (`prompt.md`) builder.
- `internal/api/` — OpenAI-compatible HTTP handlers (`/v1/models`, `/v1/executors`, `/v1/files`, `/v1/responses`, `/v1/chat/completions`, `/health`, `/ready`).
- `internal/admin/` — embedded Web Admin UI (`/admin/`, `/admin/api/*`) for managing models, CLI checks, active runs, sessions, files, API keys, and interactive SSE testing.
- `internal/executor/` — `Executor` interface, `Registry`, and adapters (`codex`, `antigravity`, `gemini`, `claude`, `ollama`).
- `internal/process/` — concurrency queue/leases, sanitized env builder, unbounded JSONL stream reader, process-group kill.
- `internal/storage/` — SQLite migrations + startup recovery (`interrupted`), path-traversal/symlink-safe file storage.
- `internal/session/` — `previous_response_id` resolution, per-session sequential locking, CLI session resume vs file-based history fallback (`context.md`).
- `internal/streaming/` — separate OpenAI Responses API SSE and Chat Completions SSE translators.
- `internal/auth/` — hashed Bearer API keys, revocation, per-key model allowlists.
- `migrations/` — embedded SQLite migrations.
- `configs/` — example configuration (`config.example.yaml`).
- `scripts/` — daemon lifecycle script (`gateway.sh`: `start`, `stop`, `restart`, `status`, `health`, `ready`, `logs`).
- `deployments/` — `systemd` service unit (`cli-ai-gateway.service`).
- `docs/` — operations runbook ([docs/operations.md](docs/operations.md)) and OpenAI compatibility notes ([docs/openai-compatibility.md](docs/openai-compatibility.md)).
- `tests/` — integration, admin API/UI, security, and official OpenAI Go SDK compatibility tests.

## Quick Start

### 1. Configure

Copy `configs/config.example.yaml` to `config.yaml`, set secure API keys (or `key_sha256`), update `admin.token` (or `GATEWAY_ADMIN_TOKEN`), and adjust model paths/executables:

```bash
cp configs/config.example.yaml config.yaml
```

Environment overrides:
- `GATEWAY_CONFIG` — path to YAML config
- `GATEWAY_LISTEN` — HTTP bind address (e.g. `127.0.0.1:8080`)
- `GATEWAY_STORAGE_PATH` — storage directory for SQLite, `files/`, `sessions/`, `runs/`
- `GATEWAY_SQLITE_PATH` — override SQLite DB file path
- `GATEWAY_LOG_LEVEL` — `debug`, `info`, `warn`, `error`
- `GATEWAY_DIAGNOSTIC_LOG` — `true` enables extra diagnostic attributes (keep `false` in production)
- `GATEWAY_ADMIN_ENABLED` — `true` / `false` to toggle the embedded Web Admin UI (`/admin/`)
- `GATEWAY_ADMIN_TOKEN` — admin bearer/login token for `/admin/`

### 2. Build, Start, Restart, Stop

Run in background via `Makefile` / `scripts/gateway.sh` (or foreground via `make run`):

```bash
make build
make start      # start daemon & wait for /health
make status     # PID, /health and /ready probes
make logs       # follow data/gateway.log
make restart    # rebuild, graceful stop (SIGTERM), start
make stop       # graceful SIGTERM shutdown
```

Or run directly in the foreground:

```bash
./bin/cli-ai-gateway -config config.yaml
```

Verify health and executor readiness:

```bash
curl -s http://127.0.0.1:8080/health
curl -s http://127.0.0.1:8080/ready
```

Run the example Go client or curl walkthrough:

```bash
go run ./cmd/example-client -url http://127.0.0.1:8080 -key gw-dev-change-me -model codex
./examples/curl.sh
```

## Web Admin Console (`/admin/`)

When `admin.enabled: true` (default in `configs/config.example.yaml`), open `http://127.0.0.1:8080/admin/` and log in with `admin.token` (or `GATEWAY_ADMIN_TOKEN`).

- Manage enabled/disabled models, `timeout_seconds`, `max_concurrent`, arguments, re-run CLI readiness checks (`Check`), and add/remove local `ollama` models dynamically (persisted in SQLite `model_overrides` across restarts).
- Inspect active and historical runs and forcibly cancel hung process groups (`POST /admin/api/runs/{id}/cancel`).
- Issue, scope (`allowed_models`, `max_concurrent`), revoke/unrevoke, and delete client API keys.
- Inspect/upload/delete files, inspect/delete sessions (`context.md`), trigger TTL cleanup, and run live SSE prompts in the built-in Playground.

See the full Russian operations guide in [docs/operations.md](docs/operations.md).

## Initial CLI Authorization (Interactive Login)

The gateway never extracts or intercepts OAuth tokens. Each CLI must be signed in beforehand using its official login command under the service user (or its isolated `home_dir` configured in `config.yaml`):

- **Codex CLI**:
  ```bash
  CODEX_HOME=/var/lib/cli-gateway/homes/codex HOME=/var/lib/cli-gateway/homes/codex codex login
  ```
- **Antigravity CLI (`agy`)**:
  ```bash
  HOME=/var/lib/cli-gateway/homes/antigravity agy
  # Complete Google OAuth login in the interactive session, then exit with /quit
  ```
- **Gemini CLI**:
  ```bash
  HOME=/var/lib/cli-gateway/homes/gemini gemini
  # Follow the interactive Google authentication prompt and exit
  ```
- **Claude CLI**:
  ```bash
  HOME=/var/lib/cli-gateway/homes/claude claude auth login
  # Or set CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY inside that executor's config env
  ```
- **Ollama**:
  Ensure the Ollama daemon is running (`ollama serve`) and target models are pulled (`ollama pull qwen3`).

> Always isolate each CLI's `home_dir` in production so one executor cannot access another executor's credentials.

## Deployment

### Variant A: systemd (Ubuntu Server)

1. Build and install the binary:
   ```bash
   CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /usr/local/bin/cli-ai-gateway ./cmd/gateway
   ```
2. Create unprivileged system user and directories:
   ```bash
   sudo useradd --system --home-dir /var/lib/cli-gateway --create-home --shell /usr/sbin/nologin cli-gateway
   sudo mkdir -p /etc/cli-ai-gateway /var/lib/cli-gateway/homes/{codex,antigravity,gemini,claude}
   sudo cp config.yaml /etc/cli-ai-gateway/config.yaml
   sudo chown -R cli-gateway:cli-gateway /var/lib/cli-gateway
   sudo chmod 0640 /etc/cli-ai-gateway/config.yaml
   ```
3. Install and start the systemd service:
   ```bash
   sudo cp deployments/cli-ai-gateway.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now cli-ai-gateway
   ```
4. Place behind a TLS reverse proxy (Nginx / Caddy) for external HTTPS access; keep `server.listen` bound to `127.0.0.1:8080`.

### Variant B: Docker Compose

`Dockerfile` and `compose.yaml` configure an unprivileged container (`uid 10001`), read-only root filesystem, `cap_drop: [ALL]`, `no-new-privileges`, resource limits, and separate named volumes for gateway storage and each CLI's auth home:

```bash
make docker-up
```

If running interactive CLI login inside the container:

```bash
docker compose exec -it gateway env HOME=/var/lib/cli-gateway/homes/codex codex login
```

## Testing

Run all unit, integration, security, and OpenAI Go SDK compatibility tests:

```bash
make test
```

See [docs/openai-compatibility.md](docs/openai-compatibility.md) for the API compatibility matrix.
