# CLI AI Gateway

Unified OpenAI-compatible HTTP gateway in Go for executing prompts and large file-based tasks across containerized AI CLI agents (`Codex CLI`, `Claude Code CLI`, `Gemini CLI`, `Antigravity CLI` — single or multiple isolated accounts such as `agy-1`, `agy-2`, `agy-3`) and local or remote `Ollama` servers.

## Architecture Overview

```text
Client -> POST /v1/responses -> gateway container (8080, zero CLIs)
                                   |-- http://codex:9090  -> codex container (codex + volume codex_home)
                                   |-- http://claude:9090 -> claude container (claude + volume claude_home)
                                   |-- http://gemini:9090 -> gemini container (gemini + volume gemini_home)
                                   |-- http://agy-1:9090  -> agy-1 container (agy + volume agy_1_home)
                                   |-- http://agy-2:9090  -> agy-2 container (agy + volume agy_2_home)
                                   |-- http://agy-3:9090  -> agy-3 container (agy + volume agy_3_home)
                                   `-- http://<host>:11434 -> remote / local Ollama API
```

- **Zero host CLI dependencies**: Every CLI client is installed inside its own purpose-built Docker image (`worker-codex`, `worker-claude`, `worker-gemini`, `worker-agy`). Multi-account setups run one dedicated container and volume per instance (`agy-1`, `agy-2`, `agy-3`), keeping credentials and sessions strictly separated without mounting `docker.sock`.
- **Single Go binary (`gateway` & `-worker` modes)**: Uses Go standard library `net/http`, `os/exec`, `context.Context`, `encoding/json`, `mime/multipart`, `log/slog`.
- **Stateless worker HTTP protocol (`internal/worker`)**: `gateway` streams workspace input files and instructions over `POST /worker/execute` (`multipart/form-data` + `application/x-ndjson`) protected by `GATEWAY_WORKER_TOKEN` Bearer auth; each worker stages into a temporary run directory, enforces PGID cancellation on disconnect/timeout, and removes temporary files before emitting the completion frame.
- **SQLite metadata store** (`modernc.org/sqlite`, WAL mode) for API keys (SHA-256 hashes), file metadata, sessions, responses, runs, and dynamic/overridden models (`model_overrides`).
- **Embedded Web Admin UI (`/admin/`)**: Manage enabled/disabled models, `timeout_seconds`, `max_concurrent`, `worker_url` / `base_url`, add remote `ollama` or worker-backed CLI models dynamically, inspect/cancel runs, inspect sessions/files, manage API keys, and test live SSE responses.

## Project Structure

- `cmd/gateway/main.go` — entrypoint supporting gateway server mode, `-worker` container worker mode, and `-healthcheck`.
- `cmd/example-client/main.go` — runnable Go client demonstrating `prompt.md` construction, file upload, streaming/sync Responses API.
- `pkg/client/` — Go client library and deterministic `PromptDocument` (`prompt.md`) builder.
- `internal/api/` — OpenAI-compatible HTTP handlers (`/v1/models`, `/v1/executors`, `/v1/files`, `/v1/responses`, `/v1/chat/completions`, `/health`, `/ready`).
- `internal/admin/` — embedded Web Admin UI (`/admin/`, `/admin/api/*`).
- `internal/worker/` — isolated container worker HTTP server and `RemoteExecutor` client (`GET /worker/health`, `POST /worker/execute`, `GET /livez`).
- `internal/executor/` — `Executor` interface, `Registry`, and adapters (`codex`, `antigravity`, `gemini`, `claude`, `ollama`).
- `internal/process/` — concurrency queue/leases, sanitized env builder, unbounded JSONL stream reader, process-group kill.
- `internal/storage/` — SQLite migrations + startup recovery (`interrupted`), path-traversal/symlink-safe file storage.
- `internal/session/` — `previous_response_id` resolution, per-session sequential locking, CLI session resume vs file-based history fallback (`context.md`).
- `internal/streaming/` — OpenAI Responses API and Chat Completions SSE translators.
- `internal/auth/` — hashed Bearer API keys, revocation, per-key model allowlists.
- `migrations/` — embedded SQLite migrations (`001_init.sql`, `002_admin.sql`, `003_worker.sql`).
- `configs/` — `config.docker.yaml` (Docker Compose multi-worker stack) and `config.example.yaml` (standalone host/local binary).
- `Dockerfile` & `compose.yaml` — multi-target builds (`gateway`, `worker-codex`, `worker-claude`, `worker-gemini`, `worker-agy`) with unprivileged UID `10001`, read-only rootfs, `cap_drop: [ALL]`, `no-new-privileges`, and isolated persistent home volumes.
- `docs/` — operations runbook ([docs/operations.md](docs/operations.md)) and OpenAI compatibility notes ([docs/openai-compatibility.md](docs/openai-compatibility.md)).
- `tests/` — integration, admin API/UI, remote worker, security, and official OpenAI Go SDK compatibility tests.

## Quick Start (Docker Compose — Ubuntu / macOS)

Only Docker (`docker compose`) and `make` are required on the host machine.

### 1. Initialize `.env` and start all containers

```bash
make init   # Creates .env with random GATEWAY_API_KEY, GATEWAY_ADMIN_TOKEN, GATEWAY_WORKER_TOKEN
make up     # Builds images and starts gateway + codex + claude + gemini + agy-1..agy-3
make ps     # Verify container status and healthchecks
```

### 2. Authenticate each containerized CLI agent

Credentials stay inside each service's dedicated Docker volume (`claude_home`, `codex_home`, `gemini_home`, `agy_1_home`, `agy_2_home`, `agy_3_home`):

```bash
make claude-login          # Runs `claude auth login` inside the claude container
make codex-login           # Runs `codex login --device-auth` inside the codex container
make gemini-login          # Runs interactive Gemini CLI login inside the gemini container
make agy-1-login           # Runs `agy` login inside agy-1 (or `make agy-login ACCOUNT=1`)
make agy-2-login           # Runs `agy` login inside agy-2 (or `make agy-login ACCOUNT=2`)
make agy-3-login           # Runs `agy` login inside agy-3 (or `make agy-login ACCOUNT=3`)
```

### 3. Connect Remote Ollama (Optional)

If Ollama runs on another server (for example `http://10.0.0.50:11434`), set these in `.env` before `make up` (and ensure the Ollama server listens with `OLLAMA_HOST=0.0.0.0:11434`):

```env
GATEWAY_OLLAMA_BASE_URL=http://10.0.0.50:11434
GATEWAY_OLLAMA_MODEL=qwen3
```

You can also add/edit multiple Ollama servers or extra worker containers (`agy-4`, etc.) dynamically in the Web Admin UI at `http://127.0.0.1:8080/admin/`.

### 4. Verify health and run a test call

```bash
curl -s http://127.0.0.1:8080/health
curl -s http://127.0.0.1:8080/ready

# Source generated GATEWAY_API_KEY from .env
set -a && . ./.env && set +a
curl -s http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"agy-1","input":"Hello from containerized worker"}'
```

Or run the example Go client:

```bash
go run ./cmd/example-client -url http://127.0.0.1:8080 -key "$GATEWAY_API_KEY" -model claude
```

## Web Admin Console (`/admin/`)

Open `http://127.0.0.1:8080/admin/` and log in with `GATEWAY_ADMIN_TOKEN` from `.env`:

- View all configured models (`Ready / Unavailable`), inspect `worker_url` / `base_url`, toggle enable/disable, tune `timeout_seconds` & `max_concurrent`, and trigger live readiness checks.
- Dynamically register/delete remote `ollama` models (`base_url`) or additional containerized CLI workers (`worker_url`), persisted across restarts in SQLite `model_overrides`.
- Inspect active runs and forcibly kill hung worker process groups (`POST /admin/api/runs/{id}/cancel`).
- Issue, scope, revoke, and delete client API keys, inspect/delete sessions (`context.md`) and files, and run interactive SSE prompts in the Playground.

See the full Russian deployment & operations guide in [docs/operations.md](docs/operations.md).

## Lifecycle & Local Go Development

```bash
make logs SERVICE=agy-1    # Follow logs of a specific container
make restart SERVICE=agy-1 # Restart a single worker container
make down                  # Stop containers (volumes with logins remain intact)

# Local Go development / tests:
make lint                  # go vet ./...
make test                  # go test -race ./...
```

Local non-Docker daemon targets (`make build`, `make local-start`, `make local-stop`, `make local-status`) remain available via [scripts/gateway.sh](scripts/gateway.sh) and [deployments/cli-ai-gateway.service](deployments/cli-ai-gateway.service).

See [docs/openai-compatibility.md](docs/openai-compatibility.md) for the API compatibility matrix.
