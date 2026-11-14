# CLI AI Gateway — Развёртывание в Docker с нуля, пул воркеров (`N` агентов) и Web-админка

Практическая инструкция по развёртыванию **CLI AI Gateway** на чистом сервере (Ubuntu / Linux / macOS) с произвольным числом изолированных контейнеров под каждого CLI-агента (`claude-*`, `codex-*`, `gemini-*`, `agy-*`), подключению удалённого Ollama, авторизации и управлению через Web-админку (`/admin/`).

---

## 1. Архитектура: контейнер на каждый аккаунт / модель

Ни один CLI-клиент не ставится на хост. Для **каждого экземпляра** любого агента поднимается свой Docker-контейнер и свой persistent volume:

```text
Client -> POST /v1/responses -> gateway (8080, бинарников CLI нет)
                                  |-- http://claude-1:9090 -> claude-1 (claude + volume claude_1_home)
                                  |-- http://claude-2:9090 -> claude-2 (claude + volume claude_2_home)
                                  |-- http://codex-1:9090  -> codex-1  (codex  + volume codex_1_home)
                                  |-- http://codex-2:9090  -> codex-2  (codex  + volume codex_2_home)
                                  |-- http://agy-1:9090    -> agy-1    (agy    + volume agy_1_home)
                                  |-- ... (любое число agy-N / codex-N / claude-N / gemini-N)
                                  `-- http://<ip>:11434    -> удалённый Ollama-сервер
```

Как это работает под капотом:
- **`docker.sock` не монтируется**: `gateway` общается с воркерами по HTTP (`-worker` на `:9090`, токен `GATEWAY_WORKER_TOKEN`, пакет `internal/worker`).
- **Нет жёсткого лимита на количество контейнеров**: скрипт [scripts/workers.sh](../scripts/workers.sh) хранит список контейнеров в `.workers` и генерирует `compose.workers.yaml` и `workers.models.yaml` (читается шлюзом через `GATEWAY_WORKERS_CONFIG`, поверх [configs/config.docker.yaml](../configs/config.docker.yaml)).
- **Изоляция и безопасность**: read-only rootfs, `uid 10001`, `cap_drop: [ALL]`, `no-new-privileges:true`, отдельный volume под `HOME` каждого контейнера.

---

## 2. Развёртывание на чистом Ubuntu-сервере с нуля

### Шаг 1. Базовые пакеты на хост

```bash
sudo apt-get update && sudo apt-get install -y ca-certificates curl git make
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"
# Перезайдите в SSH-сессию, чтобы применилась группа docker
```

### Шаг 2. Инициализация `.env` и стартового пула контейнеров

```bash
git clone <URL-репозитория> agy && cd agy

# Вариант A: стандартный набор (1 claude, 1 codex, 1 gemini, 3 agy):
make init

# Вариант B: сразу задать нужное количество каждого CLI (например 2 клода, 3 кодекса, 6 аги):
make init CLAUDE_COUNT=2 CODEX_COUNT=3 GEMINI_COUNT=1 AGY_COUNT=6
```

`make init` создаёт:
- `.env` со сгенерированными случайными секретами (`GATEWAY_API_KEY`, `GATEWAY_ADMIN_TOKEN`, `GATEWAY_WORKER_TOKEN`) и `COMPOSE_FILE=compose.yaml:compose.workers.yaml`;
- `.workers`, `compose.workers.yaml` и `workers.models.yaml` (все 4 файла в `.gitignore`, не загрязняют репозиторий).

### Шаг 3. Удалённый сервер Ollama (опционально)

Если Ollama запущен на другом сервере (например `10.0.0.50`):
1. На сервере с Ollama разрешите внешние подключения (`OLLAMA_HOST=0.0.0.0:11434`, порт `11434`).
2. Добавьте в `.env`:
   ```env
   GATEWAY_OLLAMA_BASE_URL=http://10.0.0.50:11434
   GATEWAY_OLLAMA_MODEL=qwen3
   ```
   При наличии `GATEWAY_OLLAMA_BASE_URL` модель `remote-ollama` включается автоматически. Несколько разных серверов/моделей Ollama можно добавить в любой момент через Web-админку (`/admin/` -> `+ Добавить модель`).

### Шаг 4. Сборка и запуск

```bash
make up
make ps
make agent-ls
```

---

## 3. Масштабирование: добавить или удалить контейнеры (`make agent-add` / `agent-rm`)

Добавить новые контейнеры можно в любой момент без правки YAML руками:

```bash
# Авто-нумерация следующего свободного слота:
make agent-add TYPE=agy                   # создаст agy-4 (затем agy-5, agy-6...)
make agent-add TYPE=agy COUNT=3           # создаст сразу 3 новых agy-контейнера
make agent-add TYPE=codex                 # создаст codex-2 (затем codex-3...)
make agent-add TYPE=claude                # создаст claude-2 (затем claude-3...)
make agent-add TYPE=gemini COUNT=2        # создаст gemini-2, gemini-3

# Произвольное имя или номер слота:
make agent-add TYPE=claude NAME=claude-work
make agent-add TYPE=codex SLOT=10         # создаст codex-10
```

Что делает `make agent-add`:
1. Добавляет строки в `.workers`, пересобирает `compose.workers.yaml` и `workers.models.yaml`.
2. Поднимает новые контейнеры через `docker compose up -d --build`.
3. Если `gateway` уже запущен — регистрирует новые модели сразу через `/admin/api/models` **без рестарта `gateway`**.

Удаление контейнера:

```bash
make agent-rm NAME=agy-5                  # удалит контейнер, volume agy_5_home сохранит
make agent-rm NAME=agy-5 PURGE_VOLUME=1   # удалит контейнер вместе с auth-томом
```

---

## 4. Авторизация агентов в контейнерах (`make *-login`)

Для каждого клиента есть единая `make`-команда с параметром `ACCOUNT=1|2|...` (по умолчанию `1`) или явным именем `NAME=...`:

```bash
# Claude Code CLI:
make claude-login                  # вход в claude-1
make claude-login ACCOUNT=2        # вход в claude-2
make claude-login NAME=claude-work # вход в произвольный контейнер claude-work

# Codex CLI (device auth без браузера):
make codex-login                   # вход в codex-1
make codex-login ACCOUNT=2         # вход в codex-2
make codex-login ACCOUNT=3         # вход в codex-3

# Gemini CLI:
make gemini-login                  # вход в gemini-1
make gemini-login ACCOUNT=2        # вход в gemini-2

# Antigravity CLI (agy):
make agy-login ACCOUNT=1           # вход в agy-1
make agy-login ACCOUNT=4           # вход в agy-4
make agy-login ACCOUNT=5           # вход в agy-5

# Универсальная команда для любого контейнера:
make agent-login NAME=codex-2
```

Проверка готовности всех контейнеров:

```bash
curl -s http://127.0.0.1:8080/ready
```

---

## 5. Повседневное управление, логи и рестарт

```bash
make agent-ls                # таблица всех worker-контейнеров, worker_url и volume
make ps                      # статус Docker-контейнеров и healthcheck
make logs                    # логи gateway
make logs SERVICE=codex-2    # логи конкретного контейнера
make restart SERVICE=agy-4   # перезапуск одного воркера
make down                    # остановка (авторизации в volumes сохраняются)
make up                      # повторный запуск / обновление
```

---

## 6. Переменные окружения

| Переменная | Назначение |
| --- | --- |
| `COMPOSE_FILE` | В `.env`: `compose.yaml:compose.workers.yaml` |
| `CLAUDE_COUNT` / `CODEX_COUNT` / `GEMINI_COUNT` / `AGY_COUNT` | Стартовое число контейнеров при `make init` (по умолчанию `1/1/1/3`) |
| `GATEWAY_BIND_HOST` / `GATEWAY_PORT` | Host IP и порт публикации `gateway` (по умолчанию `127.0.0.1:8080`) |
| `GATEWAY_API_KEY` | Переопределяет клиентский Bearer-ключ для `/v1/*` |
| `GATEWAY_ADMIN_TOKEN` | Токен входа в Web-админку `/admin/` |
| `GATEWAY_WORKER_TOKEN` | Общий Bearer-токен между `gateway` и всеми воркерами (`:9090`) |
| `GATEWAY_OLLAMA_BASE_URL` / `GATEWAY_OLLAMA_MODEL` | Адрес и модель удалённого Ollama (включает `remote-ollama` автоматически) |
| `GATEWAY_WORKERS_CONFIG` | Автоматически задаётся `compose.workers.yaml` (`/etc/cli-ai-gateway/workers.yaml`) |
| `GATEWAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |

---

## 7. Встроенная Web-админка (`/admin/`) и API

Откройте `http://127.0.0.1:8080/admin/` (либо через SSH-туннель `ssh -L 8080:127.0.0.1:8080 user@server`) и войдите по `GATEWAY_ADMIN_TOKEN` из `.env`:
- **Модели, воркеры и Ollama**: просмотр статуса готовности каждого `claude-*`, `codex-*`, `gemini-*`, `agy-*` и `remote-ollama`, отключение/включение, настройка `timeout_seconds`, `max_concurrent`, `worker_url` / `base_url`, динамическое добавление удалённых Ollama и внешних воркеров (`model_overrides` в SQLite).
- **Запуски, сессии, файлы, API-ключи и песочница SSE**: отмена зависших запусков (завершает `PGID` процесса внутри воркера), управление клиентскими ключами, проверка потокового ответа в Playground.

Пример вызова конкретного воркера через `/v1/responses`:

```bash
set -a && . ./.env && set +a
curl -s http://127.0.0.1:${GATEWAY_PORT:-8080}/v1/responses \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"codex-2","input":"Проверь контейнер codex-2"}'
```

---

## 8. Локальный запуск бинарника (для разработки)

Цели для локальной разработки без Docker сохранены под префиксом `local-*`: `make build`, `make local-start`, `make local-status`, `make local-logs`, `make local-restart`, `make local-stop` ([scripts/gateway.sh](../scripts/gateway.sh), [deployments/cli-ai-gateway.service](../deployments/cli-ai-gateway.service)).
