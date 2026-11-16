# CLI AI Gateway

Unified OpenAI-compatible HTTP gateway in Go for executing prompts and large file-based tasks across any number of isolated, containerized AI CLI instances (`claude-*`, `codex-*`, `gemini-*`, `agy-*`) and remote or local `Ollama` servers.

## Architecture Overview

```text
Client -> POST /v1/responses -> gateway container (7070, zero CLIs)
                                   |-- http://claude-1:9090 -> claude-1 container (claude + volume claude_1_home)
                                   |-- http://claude-2:9090 -> claude-2 container (claude + volume claude_2_home)
                                   |-- http://codex-1:9090  -> codex-1 container  (codex + volume codex_1_home)
                                   |-- http://codex-2:9090  -> codex-2 container  (codex + volume codex_2_home)
                                   |-- http://agy-1:9090    -> agy-1 container    (agy + volume agy_1_home)
                                   |-- ... (agy-N, codex-N, claude-N, gemini-N via `make agent-add`)
                                   `-- http://<host>:11434  -> remote / local Ollama API
```

- **Zero host CLI dependencies**: Every CLI client is installed inside its own Docker image (`worker-claude`, `worker-codex`, `worker-gemini`, `worker-agy`). Every account or variant runs in its own container with its own dedicated Docker volume — whether you run 1 Claude or 4, 2 Codexes or 10 Antigravity accounts — with zero `docker.sock` mount.
- **Dynamic N-worker pool (`scripts/workers.sh` + `Makefile`)**: Declare initial counts during `make init` (`CLAUDE_COUNT=2 CODEX_COUNT=3 AGY_COUNT=5 make init`), or add/remove containers on the fly with `make agent-add` / `make agent-rm`. Adding a container generates `compose.workers.yaml` and `workers.models.yaml` (loaded via `GATEWAY_WORKERS_CONFIG`) and registers the new worker live in running `gateway` with zero downtime.
- **Stateless worker HTTP protocol (`internal/worker`)**: `gateway` streams workspace input files and instructions over `POST /worker/execute` (`multipart/form-data` + `application/x-ndjson`) protected by `GATEWAY_WORKER_TOKEN` Bearer auth; each worker stages into an ephemeral workspace, enforces PGID cancellation on disconnect/timeout, and cleans temp files before emitting the terminal frame.
- **SQLite metadata store** (`modernc.org/sqlite`, WAL mode) for API keys (SHA-256 hashes), file metadata, sessions, responses, runs, and `model_overrides`.
- **Embedded Web Admin UI (`/admin/`)**: Manage enabled/disabled models, `timeout_seconds`, `max_concurrent`, `worker_url` / `base_url`, add remote `ollama` or custom worker URLs dynamically, inspect/cancel runs, inspect sessions/files, manage API keys, and test live SSE responses.

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
- `configs/` — `config.docker.yaml` (Docker Compose core gateway + remote Ollama) and `config.example.yaml` (standalone host/local binary).
- `scripts/workers.sh` — dynamic worker pool generator & login helper (`.workers` -> `compose.workers.yaml` + `workers.models.yaml`).
- `Dockerfile` & `compose.yaml` — multi-target builds (`gateway`, `worker-codex`, `worker-claude`, `worker-gemini`, `worker-agy`) with unprivileged UID `10001`, read-only rootfs, `cap_drop: [ALL]`, `no-new-privileges`, and isolated per-container volumes.
- `docs/` — operations runbook ([docs/operations.md](docs/operations.md)) and OpenAI compatibility notes ([docs/openai-compatibility.md](docs/openai-compatibility.md)).
- `tests/` — integration, admin API/UI, remote worker, security, and official OpenAI Go SDK compatibility tests.

## Quick Start (Docker Compose — Ubuntu / macOS)

Only Docker (`docker compose`) and `make` are required on the host machine.

### 1. Initialize secrets and container pool

```bash
# Default counts: 1 claude, 1 codex, 1 gemini, 3 agy
make init

# Or choose any initial count per CLI right from the start:
make init CLAUDE_COUNT=2 CODEX_COUNT=3 GEMINI_COUNT=1 AGY_COUNT=6
```

`make init` creates `.env` (random `GATEWAY_API_KEY`, `GATEWAY_ADMIN_TOKEN`, `GATEWAY_WORKER_TOKEN`, plus `COMPOSE_FILE=compose.yaml:compose.workers.yaml`), `.workers`, `compose.workers.yaml`, and `workers.models.yaml` (all gitignored so host deployments stay clean).

### 2. Start gateway and all worker containers

```bash
make up
make ps
make agent-ls
```

### 3. Add more containers at any time (`agy`, `codex`, `claude`, `gemini`)

```bash
make agent-add TYPE=agy COUNT=2            # Adds agy-4, agy-5 (or next free numbers)
make agent-add TYPE=codex                    # Adds codex-2
make agent-add TYPE=claude                   # Adds claude-2
make agent-add TYPE=claude NAME=claude-work  # Custom container & model ID
```

New containers start immediately and register into running `gateway` live via `/admin/api/models` with zero downtime.

### 4. Authenticate any containerized CLI agent

Each command runs interactive auth inside that container and saves credentials inside its dedicated Docker volume:

```bash
make claude-login                      # Logs into claude-1
make claude-login ACCOUNT=2            # Logs into claude-2
make codex-login                       # Logs into codex-1 (`codex login --device-auth`)
make codex-login ACCOUNT=2             # Logs into codex-2
make gemini-login ACCOUNT=1            # Logs into gemini-1
make agy-login ACCOUNT=1               # Logs into agy-1
make agy-login ACCOUNT=4               # Logs into agy-4
make agent-login NAME=claude-work      # Logs into any custom-named container
```

### 5. Connect Remote Ollama (Optional)

If Ollama runs on another server (e.g. `http://10.0.0.50:11434`), ensure it listens on `OLLAMA_HOST=0.0.0.0:11434` and set in `.env` before `make up`:

```env
GATEWAY_OLLAMA_BASE_URL=http://10.0.0.50:11434
GATEWAY_OLLAMA_MODEL=qwen3
```

Additional Ollama instances can be added/edited anytime via Web Admin (`/admin/`).

### 6. Verify readiness, publish models, and send a test request

Raw worker containers (`claude-*`, `codex-*`, `gemini-*`, `agy-*`, `remote-ollama`) are registered with `public: false` so `GET /v1/models` only exposes models explicitly published from the Web Admin console (individual sub-models or composite pools):

```bash
curl -s http://127.0.0.1:7070/health
curl -s http://127.0.0.1:7070/ready

# After publishing a standalone or composite model (e.g. "agy-pool") in http://127.0.0.1:7070/admin/
set -a && . ./.env && set +a
curl -s http://127.0.0.1:7070/v1/models -H "Authorization: Bearer $GATEWAY_API_KEY"
curl -s http://127.0.0.1:7070/v1/responses \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"agy-pool","input":"Hello from published model"}'
```

## Web Admin Console (`/admin/`)

Open `http://127.0.0.1:7070/admin/` and sign in with `GATEWAY_ADMIN_TOKEN` from `.env`:

- **Providers & Model Discovery**: inspect readiness of every worker container and Ollama backend, refresh discovered CLI/API sub-models (`agy models`, `gemini --list-models`, Codex cache/presets, Claude settings/aliases, Ollama `/api/tags`), add custom model IDs, and publish chosen sub-models as standalone public gateway models.
- **Model Constructor (Composite Pools & Priority Failover)**: assemble composite models across homogeneous (`3x agy`) or heterogeneous (`codex -> agy -> claude`) `(provider_id, provider_model)` targets with explicit **priority tiers** (`priority = 1, 2, ...`) and `priority_order` / `round_robin` routing within a tier.
- **Per-Model Cooldowns**: when a target hits a rate/quota limit before streaming begins, the gateway parses reset/retry durations from the response (`Resets in ...`, `Try again in ...`, `Retry-After`, ISO/UTC timestamps) or falls back to `default_cooldown_seconds`, automatically fails over to the next available target on the same request, isolates cooldown strictly per `(provider_id, provider_model)`, returns immediately to higher-priority tiers once recovered, and lets admins inspect or manually release active cooldowns.
- **Runs, Sessions, Files, Keys & Playground**: inspect target provider/model per run, cancel live runs (`PGID` killed cleanly inside worker container), manage API keys/sessions/files, and test published models via SSE Playground.

See the full Russian operations guide in [docs/operations.md](docs/operations.md).

## Lifecycle & Local Go Development

```bash
make logs SERVICE=codex-2    # Follow logs of a specific container
make restart SERVICE=agy-4   # Restart a single container
make agent-rm NAME=agy-5     # Remove container (keeps auth volume unless PURGE_VOLUME=1)
make down                    # Stop containers (volumes with logins remain intact)

# Local Go development / tests:
make lint                    # go vet ./...
make test                    # go test -race ./...
```

Local non-Docker daemon targets (`make build`, `make local-start`, `make local-stop`, `make local-status`) remain available via [scripts/gateway.sh](scripts/gateway.sh) and [deployments/cli-ai-gateway.service](deployments/cli-ai-gateway.service).
