# CLI AI Gateway — Руководство по эксплуатации и Web-админке

Практическая инструкция по настройке, запуску, остановке, перезапуску, диагностике и управлению шлюзом **CLI AI Gateway** через консоль и встроенную Web-админку (`/admin/`).

---

## 1. Архитектура и ключевые пути

Шлюз собирается в **единый статический бинарный файл** (`bin/cli-ai-gateway`) без внешних сервисов-брокеров:
- **OpenAI-совместимый API** (`/v1/*`): `GET /v1/models`, `GET /v1/executors`, `POST/GET/DELETE /v1/files`, `POST /v1/responses`, `POST /v1/chat/completions`.
- **Healthcheck / Readiness**: `GET /health` (liveness) и `GET /ready` (доступность SQLite, директорий хранения и проверка CLI-исполнителей).
- **Встроенная Web-админка** (`/admin/` и `/admin/api/*`): одностраничное приложение, вшитое в бинарник через `go:embed`.
- **Хранилище** (`storage.path`, по умолчанию `./data` или `/var/lib/cli-gateway/data`):
  - `gateway.db` — SQLite WAL-база метаданных (API-ключи, модели, файлы, сессии, запуски, `model_overrides`);
  - `files/` — загруженные файлы для промптов;
  - `sessions/<session_id>/` — `context.md` с историей диалога;
  - `runs/<run_id>/input/` — изолированные рабочие директории каждого запуска CLI.

---

## 2. Подготовка конфигурации

Скопируйте эталонный конфиг и настройте секреты перед первым запуском:

```bash
cp configs/config.example.yaml config.yaml
```

Минимально что нужно изменить в `config.yaml`:

1. **API-ключи клиентов** (`auth.keys`) — выдайте рабочие ключи или их SHA-256 хэши (`key_sha256`).
2. **Пароль Web-админки** (`admin`):
   ```yaml
   admin:
     enabled: true
     token: "сгенерированный-длинный-секрет"
     # Либо безопаснее через SHA-256 (64 hex-символа):
     # token_sha256: "..."
   ```
   > **Важно**: Не используйте дефолтные `gw-dev-change-me` и `gw-admin-change-me` вне локальной разработки. Секретный токен можно также передать через переменную окружения `GATEWAY_ADMIN_TOKEN`.
3. **Модели и изолированные `home_dir`** (`models`) — укажите пути к бинарникам CLI или используйте встроенный `auto` (`codex`, `agy`, `gemini`, `claude`) и `ollama`.

Основные переменные окружения:

| Переменная | Назначение |
| --- | --- |
| `GATEWAY_CONFIG` | Путь к YAML-конфигу |
| `GATEWAY_LISTEN` | Адрес и порт (по умолчанию `127.0.0.1:8080`) |
| `GATEWAY_STORAGE_PATH` | Каталог SQLite и рабочих директорий |
| `GATEWAY_SQLITE_PATH` | Явный путь к файлу `gateway.db` |
| `GATEWAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `GATEWAY_DIAGNOSTIC_LOG` | `true` включает подробный аудит без утечки промптов/ключей |
| `GATEWAY_ADMIN_ENABLED` | `true`/`false` — включить или отключить Web-админку |
| `GATEWAY_ADMIN_TOKEN` | Токен администратора для `/admin/` |

---

## 3. Предварительная авторизация CLI

Шлюз не перехватывает OAuth-токены. Перед использованием каждой модели выполните вход в CLI под сервисным пользователем или с тем же `HOME` (`home_dir` из `config.yaml`):

```bash
# Codex CLI
CODEX_HOME=/var/lib/cli-gateway/homes/codex HOME=/var/lib/cli-gateway/homes/codex codex login

# Antigravity CLI
HOME=/var/lib/cli-gateway/homes/antigravity agy

# Gemini CLI
HOME=/var/lib/cli-gateway/homes/gemini gemini

# Claude CLI
HOME=/var/lib/cli-gateway/homes/claude claude auth login

# Ollama (локальный сервер)
ollama serve
ollama pull qwen3
```

---

## 4. Запуск, проверка, рестарт и остановка

### Вариант A: Локальный демон (`scripts/gateway.sh` и `Makefile`)

Скрипт [scripts/gateway.sh](../scripts/gateway.sh) управляет фоновым процессом через PID-файл (`data/gateway.pid`) и пишет логи в `data/gateway.log`. По умолчанию используется `./config.yaml`, а при его отсутствии — `./configs/config.example.yaml`.

```bash
# Сборка бинарников
make build

# Запуск в фоне с ожиданием прохождения /health
make start
# или напрямую:
./scripts/gateway.sh start

# Проверка статуса, PID, /health и /ready
make status
./scripts/gateway.sh status
./scripts/gateway.sh ready

# Просмотр логов в реальном времени (или последние 100 строк без ожидания)
make logs
./scripts/gateway.sh logs --no-follow

# Плавный перезапуск (пересборка + SIGTERM + новый старт)
make restart
./scripts/gateway.sh restart

# Плавная остановка (SIGTERM с ожиданием до 12 сек, закрытие SQLite WAL и дочерних процессов)
make stop
./scripts/gateway.sh stop
```

Для запуска с нестандартным конфигом используйте переменные окружения:

```bash
GATEWAY_CONFIG=/path/to/prod.yaml GATEWAY_PID_FILE=/tmp/gw.pid GATEWAY_LOG_FILE=/tmp/gw.log ./scripts/gateway.sh start
```

### Вариант B: Systemd (Linux / Ubuntu Server)

Unit-файл находится в [deployments/cli-ai-gateway.service](../deployments/cli-ai-gateway.service).

```bash
# Запуск и добавление в автозагрузку
sudo systemctl enable --now cli-ai-gateway

# Перезапуск после обновления бинарника или config.yaml
sudo systemctl restart cli-ai-gateway

# Остановка
sudo systemctl stop cli-ai-gateway

# Статус и последние ошибки
sudo systemctl status cli-ai-gateway

# Просмотр логов
sudo journalctl -u cli-ai-gateway -f
```

### Вариант C: Docker Compose

Конфигурация описана в [compose.yaml](../compose.yaml).

```bash
# Сборка и запуск в фоне
make docker-up
# или: docker compose up -d --build

# Статус контейнера и healthcheck
docker compose ps

# Логи
make docker-logs
# или: docker compose logs -f gateway

# Перезапуск контейнера
make docker-restart
# или: docker compose restart gateway

# Остановка и удаление контейнера (volumes сохраняются)
make docker-down
# или: docker compose down

# Логин CLI внутри уже запущенного контейнера
docker compose exec -it gateway env HOME=/var/lib/cli-gateway/homes/codex codex login
```

---

## 5. Работа с API и клиентом

### Быстрая проверка через `curl`

Полный сквозной сценарий лежит в [examples/curl.sh](../examples/curl.sh). Базовые вызовы:

```bash
# 1. Список активных моделей
curl -s http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer gw-dev-change-me"

# 2. Проверка возможностей CLI-исполнителей
curl -s http://127.0.0.1:8080/v1/executors \
  -H "Authorization: Bearer gw-dev-change-me"

# 3. Загрузка prompt.md (или кодового файла) и запуск /v1/responses
curl -s -F purpose=user_data -F file=@README.md http://127.0.0.1:8080/v1/files \
  -H "Authorization: Bearer gw-dev-change-me"

# 4. Синхронный запрос (Responses API)
curl -s http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer gw-dev-change-me" \
  -H "Content-Type: application/json" \
  -d '{"model":"codex","input":"Сделай краткое ревью архитектуры проекта"}'
```

### Go-клиент

```bash
go run ./cmd/example-client -url http://127.0.0.1:8080 -key gw-dev-change-me -model codex
```

---

## 6. Встроенная Web-админка (`/admin/`)

Откройте в браузере `http://127.0.0.1:8080/admin/`.

### 6.1 Безопасность и авторизация

- **Отдельный контур аутентификации**: клиентские API-ключи `/v1/*` не подходят для `/admin/*`. Используется только `admin.token` (`token_sha256` или `GATEWAY_ADMIN_TOKEN`).
- **Сессия в браузере**: `POST /admin/api/login` устанавливает подписанную эфемерным HMAC-SHA256 ключом cookie `agy_admin_session` с флагами `HttpOnly; SameSite=Strict`.
- **Защита от CSRF**: все изменяющие запросы (`POST`, `PATCH`, `PUT`, `DELETE`) при работе через cookie требуют заголовок `X-Admin-UI: 1` (UI отправляет его автоматически).
- **Доступ по скрипту/CLI**: любой `/admin/api/*` маршрут принимает и `Authorization: Bearer <ADMIN_TOKEN>`.
- **Anti-RCE**: через Web-админку нельзя подменять пути к CLI-бинарникам (`executable`), а динамическое добавление новых моделей разрешено только для локальных HTTP-моделей `provider: ollama`.

### 6.2 Возможности вкладок админки

1. **Модели и CLI**
   - Просмотр всех моделей (включая выключенные в `config.yaml`), статусов `Ready / Unavailable` и диагностических сообщений.
   - Включение/отключение модели без перезапуска сервера, настройка `timeout_seconds`, `max_concurrent`, `model` (целевая модель Ollama) и `base_url` (изменения сохраняются в SQLite-таблице `model_overrides` и переживают перезапуск).
   - Кнопка **Проверить CLI** (`POST /admin/api/models/{id}/check`) для мгновенной перепроверки исполнителя после `codex login` или запуска Ollama.
   - Добавление и удаление динамических моделей **Ollama** (`POST /admin/api/models`, `DELETE /admin/api/models/{id}`).
2. **Запуски и процессы**
   - Мониторинг активных процессов (`live_active`), статуса (`queued`, `running`, `completed`, `failed`, `cancelled`, `interrupted`), объёма входных байт и длительности.
   - Детальная карточка запуска (метаданные запуска, коды ошибок, фрагменты `result.json` и `events.jsonl`).
   - Принудительная остановка зависшего выполнения кнопкой **Прервать** (`POST /admin/api/runs/{id}/cancel`): отменяет контекст и завершает всю группу процессов (`PGID`).
3. **Сессии**
   - Список диалогов (`gateway_session_id`), их владельцев, привязанных моделей, количества ходов (`turn_count`), `last_response_id` и `provider_session_id`.
   - Просмотр истории шагов (`history.jsonl`) и списка связанных запусков (`runs`).
   - Удаление неактивных сессий вместе с файлами на диске (`DELETE /admin/api/sessions/{id}`).
4. **Файлы**
   - Просмотр загруженных файлов (`file_id`, `owner_id`, размер, `active_refs`, срок истечения TTL).
   - Прямая загрузка файла из браузера, чтение содержимого текстовых/Markdown файлов (до 2 МиБ) и удаление неактивных файлов (`active_refs == 0`).
5. **API-ключи**
   - Выпуск нового ключа (`gw-...`; секрет показывается 1 раз сразу после создания, в БД хранится только SHA-256).
   - Настройка `allowed_models` (`*` или список моделей через запятую) и персонального лимита параллельных процессов `max_concurrent`.
   - Мгновенный отзыв (`Revoke`), восстановление (`Unrevoke`) и удаление ключей.
6. **Песочница (SSE)**
   - Интерактивный тест модели прямо из браузера в режиме стриминга `SSE` или синхронном режиме.
   - Поддержка `instructions`, цепочки диалога (`previous_response_id`) и прикрепления уже загруженных `file_ids`.
7. **Конфиг и обслуживание**
   - Просмотр активной конфигурации (секретные поля маскируются как `[REDACTED]`), команды для оперативного управления и кнопка **TTL-очистка сейчас** (`POST /admin/api/cleanup`).

---

## 7. Резервное копирование и диагностика неполадок

### Резервное копирование SQLite

Так как база работает в режиме `WAL`, не копируйте `gateway.db` обычным `cp` во время нагрузки без учёта WAL-файла. Безопасный онлайн-бэкап:

```bash
sqlite3 data/gateway.db ".backup '/backup/gateway-$(date +%F).db'"
```

### Типичные проблемы

- **`executor_unavailable` / модель помечена `Unavailable` на `/ready`**
  1. Убедитесь, что бинарник CLI находится в `PATH` или задан в `config.yaml`.
  2. Выполните авторизацию CLI с тем же `home_dir` / `HOME`, под которым работает шлюз.
  3. Нажмите **Проверить CLI** в Web-админке или перезапустите шлюз.
- **Статус запуска `interrupted` после рестарта**
  Это штатное восстановление при старте: предыдущий процесс шлюза завершился во время активного выполнения CLI, и при запуске БД автоматически перевела незавершённые `running` записи в `interrupted`, освободив ссылки на файлы (`active_refs`).
- **429 `queue_limit_exceeded`**
  Превышен лимит одновременных запусков (`limits.max_global_processes`, `max_concurrent` модели или `max_concurrent` ключа). Увеличьте лимиты через Web-админку (для моделей/ключей) или в `config.yaml`.
- **401 на `/admin/api/*` или ошибка CSRF `missing_csrf_header`**
  При обращении скриптами передавайте `Authorization: Bearer <ADMIN_TOKEN>`. Браузерные запросы с cookie обязаны нести `X-Admin-UI: 1`.
