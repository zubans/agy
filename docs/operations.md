# CLI AI Gateway — Руководство по развёртыванию (Docker с нуля), эксплуатации и Web-админке

Практическая инструкция по развёртыванию **CLI AI Gateway** на чистом сервере (Ubuntu / Linux / macOS) в изолированных Docker-контейнерах, авторизации каждого CLI-клиента, подключению удалённого сервера Ollama, рестарту/остановке и управлению через Web-админку (`/admin/`).

---

## 1. Архитектура: контейнер на каждый агент

Все консольные CLI-клиенты устанавливаются **строго внутри Docker-образов** (на хосте не нужно ставить Node.js, Go, Claude, Codex, Gemini или Antigravity):

```text
Client -> POST /v1/responses -> gateway (8080, бинарников CLI нет)
                                  |-- http://codex:9090  -> codex (Codex CLI + volume codex_home)
                                  |-- http://claude:9090 -> claude (Claude CLI + volume claude_home)
                                  |-- http://gemini:9090 -> gemini (Gemini CLI + volume gemini_home)
                                  |-- http://agy-1:9090  -> agy-1 (Antigravity CLI + volume agy_1_home)
                                  |-- http://agy-2:9090  -> agy-2 (Antigravity CLI + volume agy_2_home)
                                  |-- http://agy-3:9090  -> agy-3 (Antigravity CLI + volume agy_3_home)
                                  `-- http://<ip>:11434  -> удалённый Ollama-сервер
```

Ключевые принципы безопасности и изоляции:
- **`docker.sock` не пробрасывается**: `gateway` общается с воркерами по внутреннему HTTP-протоколу (`-worker` на порту `9090`) с Bearer-токеном `GATEWAY_WORKER_TOKEN`.
- **Отдельный контейнер и volume на каждый аккаунт**: `agy-1`, `agy-2`, `agy-3` — три отдельных контейнера на базе образа `worker-agy`, каждый со своим volume (`agy_1_home`, `agy_2_home`, `agy_3_home`), чтобы сессии и OAuth-токены никогда не пересекались.
- **Read-only rootfs**: все контейнеры работают от непривилегированного UID `10001`, с `cap_drop: [ALL]` и `no-new-privileges:true`.
- **Передача файлов и отмена задач**: `gateway` стримит `prompt.md`, `context.md` и вложения в воркер по `POST /worker/execute`; при обрыве соединения, таймауте или отмене в админке воркер прибивает всю группу процессов (`PGID`) и удаляет временную рабочую директорию.

---

## 2. Развёртывание на чистом Ubuntu-сервере с нуля

### Шаг 1. Установка базовых пакетов на хост

Нужны только Docker Engine с Compose-плагином, `git` и `make`:

```bash
sudo apt-get update && sudo apt-get install -y ca-certificates curl git make
# Если Docker ещё не установлен — официальный скрипт Docker:
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"
# Перезайдите в SSH-сессию, чтобы применилась группа docker
```

### Шаг 2. Клонирование и генерация `.env`

```bash
git clone <URL-репозитория> agy && cd agy
make init
```

Команда `make init` создаст `.env` (из `.env.example`, файл в `.gitignore`) и автоматически сгенерирует случайные токены:
- `GATEWAY_API_KEY` — клиентский ключ для OpenAI API (`/v1/*`);
- `GATEWAY_ADMIN_TOKEN` — пароль для Web-админки (`/admin/`);
- `GATEWAY_WORKER_TOKEN` — внутренний токен между `gateway` и воркерами (`:9090`).

Базовая Docker-конфигурация моделей находится в [configs/config.docker.yaml](../configs/config.docker.yaml).

### Шаг 3. Подключение удалённого Ollama (если используется)

Если Ollama крутится на другом сервере (например `10.0.0.50`):
1. На сервере с Ollama убедитесь, что демон слушает внешний/локальный интерфейс (`OLLAMA_HOST=0.0.0.0:11434`) и порт `11434` открыт для Docker-сервера с gateway.
2. Пропишите адрес и модель в `.env`:
   ```env
   GATEWAY_OLLAMA_BASE_URL=http://10.0.0.50:11434
   GATEWAY_OLLAMA_MODEL=qwen3
   ```
   При наличии `GATEWAY_OLLAMA_BASE_URL` модель `remote-ollama` активируется автоматически. Дополнительные модели/адреса Ollama можно добавлять прямо в Web-админке без перезапуска.

### Шаг 4. Сборка и запуск стека

```bash
make up
make ps
```

Команда `make up` соберёт образы (`gateway`, `worker-codex`, `worker-claude`, `worker-gemini`, `worker-agy`) и запустит все 7 контейнеров (`gateway`, `codex`, `claude`, `gemini`, `agy-1`, `agy-2`, `agy-3`).

---

## 3. Авторизация консольных агентов в контейнерах (`make *-login`)

Каждая команда запускает интерактивный вход строго внутри своего контейнера и сохраняет авторизацию в его персональный Docker volume:

```bash
# Claude Code CLI (контейнер claude)
make claude-login

# Codex CLI (контейнер codex, device-auth для headless-сервера без браузера)
make codex-login

# Gemini CLI (контейнер gemini, без открытия локального браузера)
make gemini-login

# Antigravity CLI — 3 независимых контейнера/аккаунта:
make agy-1-login   # либо: make agy-login ACCOUNT=1
make agy-2-login   # либо: make agy-login ACCOUNT=2
make agy-3-login   # либо: make agy-login ACCOUNT=3
```

После входа проверьте готовность всех исполнителей:

```bash
curl -s http://127.0.0.1:8080/ready
```

---

## 4. Повседневное управление (старт, статус, логи, рестарт, остановка)

```bash
# Посмотреть статус и healthcheck всех 7 контейнеров
make ps

# Логи (по умолчанию gateway; можно выбрать любой контейнер)
make logs
make logs SERVICE=claude
make logs SERVICE=agy-2

# Перезапуск одного воркера или всего стека
make restart SERVICE=agy-1
make restart SERVICE=gateway
docker compose restart

# Остановка (named volumes с авторизацией CLI и gateway.db сохраняются)
make down

# Повторный запуск после остановки / обновления кода
make up
```

### Добавление ещё одного контейнера Antigravity (например `agy-4`)

1. В [compose.yaml](../compose.yaml) добавьте сервис `agy-4` (копией блока `agy-3`) и volume `agy_4_home`.
2. Поднимите новый контейнер и авторизуйте его:
   ```bash
   docker compose up -d agy-4
   make agy-login ACCOUNT=4
   ```
3. Добавьте модель `agy-4` (`provider: antigravity`, `worker_url: http://agy-4:9090`) в [configs/config.docker.yaml](../configs/config.docker.yaml) или прямо в Web-админке (`/admin/` -> `+ Добавить модель`).

---

## 5. Переменные окружения

| Переменная | Назначение |
| --- | --- |
| `GATEWAY_BIND_HOST` | Host IP публикации порта gateway (по умолчанию `127.0.0.1`) |
| `GATEWAY_PORT` | Порт на хосте (по умолчанию `8080`) |
| `GATEWAY_API_KEY` | Переопределяет первый API-ключ из `auth.keys` для `/v1/*` |
| `GATEWAY_ADMIN_TOKEN` | Токен входа в Web-админку `/admin/` |
| `GATEWAY_WORKER_TOKEN` | Общий Bearer-токен между `gateway` и worker-контейнерами (`:9090`) |
| `GATEWAY_OLLAMA_BASE_URL` | Адрес удалённого Ollama (включает `remote-ollama` автоматически) |
| `GATEWAY_OLLAMA_MODEL` | Имя модели по умолчанию на удалённом Ollama |
| `GATEWAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `GATEWAY_CONFIG` | Путь к YAML-конфигу (в Docker: `/etc/cli-ai-gateway/config.yaml`) |
| `GATEWAY_STORAGE_PATH` | Каталог SQLite и временных файлов (в Docker: `/var/lib/cli-gateway`) |

---

## 6. Работа с API и Go-клиентом

Узнать сгенерированный ключ и проверить вызов можно прямо из `.env`:

```bash
set -a && . ./.env && set +a

# 1. Список моделей
curl -s http://127.0.0.1:${GATEWAY_PORT:-8080}/v1/models \
  -H "Authorization: Bearer $GATEWAY_API_KEY"

# 2. Запрос к контейнеру agy-1 или claude
curl -s http://127.0.0.1:${GATEWAY_PORT:-8080}/v1/responses \
  -H "Authorization: Bearer $GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"agy-1","input":"Проверь работу воркера"}'

# 3. Загрузка файла и передача через file_ids
curl -s -F purpose=user_data -F file=@README.md \
  http://127.0.0.1:${GATEWAY_PORT:-8080}/v1/files \
  -H "Authorization: Bearer $GATEWAY_API_KEY"
```

---

## 7. Встроенная Web-админка (`/admin/`)

Откройте `http://127.0.0.1:8080/admin/` (либо через SSH-туннель `ssh -L 8080:127.0.0.1:8080 user@server`) и войдите по `GATEWAY_ADMIN_TOKEN` из `.env`.

- **Безопасность**: клиентские `/v1/*` ключи не пускают в `/admin/*`; сессия хранится в `HttpOnly; SameSite=Strict` cookie с проверкой CSRF (`X-Admin-UI: 1`).
- **Модели, воркеры и Ollama**:
  - Просмотр статуса готовности (`ready / unavailable`) каждого контейнера-воркера и удалённого Ollama, ручная перепроверка (`Проверить`).
  - Включение/отключение, настройка `timeout_seconds`, `max_concurrent`, `model` и `worker_url` / `base_url` (сохраняется в SQLite `model_overrides`).
  - Динамическое добавление удалённых **Ollama** (`base_url`) и дополнительных **Worker-контейнеров** (`worker_url`, например `http://agy-4:9090`); регистрация локальных путей `executable` из UI по-прежнему заблокирована.
- **Запуски, сессии, файлы, API-ключи и песочница SSE**: управление запусками (включая принудительную отмену `POST /admin/api/runs/{id}/cancel`), просмотр `history.jsonl` / `files`, выпуск и отзыв клиентских ключей, интерактивная SSE-песочница.

---

## 8. Локальный запуск бинарника (для разработки)

Если вы разрабатываете код локально с установленным Go без Docker, используйте локальные цели `Makefile` и [scripts/gateway.sh](../scripts/gateway.sh):

```bash
make build
make local-start
make local-status
make local-logs
make local-restart
make local-stop
```

Для запуска вне Docker как systemd-демона на Linux см. [deployments/cli-ai-gateway.service](../deployments/cli-ai-gateway.service).

---

## 9. Резервное копирование и диагностика неполадок

### Бэкап SQLite (внутри Docker volume `gateway_data`)

```bash
docker compose exec -T gateway cli-ai-gateway -healthcheck
# Для онлайн-копии gateway.db из volume используйте контейнер/скрипт с sqlite3 `.backup`
```

### Типичные проблемы

- **Воркер помечен `unavailable` / ошибка `worker auth failed` (401)**:
  Проверьте, что `GATEWAY_WORKER_TOKEN` в `.env` одинаков для `gateway` и воркеров (после изменения `.env` выполните `make up`, чтобы контейнеры пересоздались с новым токеном).
- **`worker unreachable at http://agy-1:9090`**:
  Проверьте `make ps` и `make logs SERVICE=agy-1`.
- **CLI требует авторизацию во время выполнения**:
  Выполните соответствующую команду `make *-login` (`make claude-login`, `make codex-login`, `make gemini-login`, `make agy-1-login` и т.д.).
- **Удалённый Ollama возвращает `connection refused`**:
  Проверьте, что на удалённой машине задано `OLLAMA_HOST=0.0.0.0:11434` и файервол пропускает порт `11434`.
- **429 `queue_limit_exceeded`**:
  Превышены `limits.max_global_processes`, `max_concurrent` модели или ключа — увеличьте лимиты в Web-админке или [configs/config.docker.yaml](../configs/config.docker.yaml).
