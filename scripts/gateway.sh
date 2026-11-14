#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_PATH="${GATEWAY_BIN:-$ROOT_DIR/bin/cli-ai-gateway}"
PID_FILE="${GATEWAY_PID_FILE:-$ROOT_DIR/data/gateway.pid}"
LOG_FILE="${GATEWAY_LOG_FILE:-$ROOT_DIR/data/gateway.log}"

if [[ -n "${GATEWAY_CONFIG:-}" ]]; then
  CONFIG_PATH="$GATEWAY_CONFIG"
elif [[ -f "$ROOT_DIR/config.yaml" ]]; then
  CONFIG_PATH="$ROOT_DIR/config.yaml"
else
  CONFIG_PATH="$ROOT_DIR/configs/config.example.yaml"
fi

ensure_config() {
  if [[ ! -f "$CONFIG_PATH" ]]; then
    echo "Error: config file not found at $CONFIG_PATH" >&2
    echo "Create one with: cp configs/config.example.yaml config.yaml" >&2
    exit 1
  fi
}

ensure_binary() {
  if [[ ! -x "$BIN_PATH" ]] || [[ "${GATEWAY_REBUILD:-0}" == "1" ]]; then
    echo "Building $BIN_PATH..."
    mkdir -p "$(dirname "$BIN_PATH")"
    (cd "$ROOT_DIR" && go build -o "$BIN_PATH" ./cmd/gateway)
  fi
}

read_pid() {
  if [[ -f "$PID_FILE" ]]; then
    tr -d '[:space:]' <"$PID_FILE"
  fi
}

is_running() {
  local pid="${1:-}"
  [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null
}

resolve_base_url() {
  local listen
  if [[ -n "${GATEWAY_LISTEN:-}" ]]; then
    listen="$GATEWAY_LISTEN"
  else
    listen="$(awk '/^[[:space:]]*listen:[[:space:]]*/ {
      val=$2;
      gsub(/["'\'']/, "", val);
      print val;
      exit
    }' "$CONFIG_PATH" 2>/dev/null || true)"
  fi
  listen="${listen:-127.0.0.1:7070}"
  if [[ "$listen" == :* ]]; then
    listen="127.0.0.1${listen}"
  fi
  echo "http://${listen}"
}

cmd_start() {
  ensure_config
  local existing_pid
  existing_pid="$(read_pid)"
  if is_running "$existing_pid"; then
    echo "cli-ai-gateway is already running (PID $existing_pid, config: $CONFIG_PATH)"
    return 0
  fi
  rm -f "$PID_FILE"

  ensure_binary
  mkdir -p "$(dirname "$PID_FILE")" "$(dirname "$LOG_FILE")"

  echo "Starting cli-ai-gateway with config: $CONFIG_PATH"
  nohup "$BIN_PATH" -config "$CONFIG_PATH" >>"$LOG_FILE" 2>&1 </dev/null &
  local pid=$!
  echo "$pid" >"$PID_FILE"

  local i=0
  while (( i < 30 )); do
    if ! is_running "$pid"; then
      echo "Error: cli-ai-gateway exited prematurely. Last log lines:" >&2
      tail -n 30 "$LOG_FILE" >&2 || true
      rm -f "$PID_FILE"
      exit 1
    fi
    if "$BIN_PATH" -config "$CONFIG_PATH" -healthcheck >/dev/null 2>&1; then
      local url
      url="$(resolve_base_url)"
      echo "Started cli-ai-gateway (PID $pid)"
      echo "  API / Health : ${url}/health"
      echo "  Readiness    : ${url}/ready"
      echo "  Web Admin UI : ${url}/admin/"
      echo "  Log file     : ${LOG_FILE}"
      return 0
    fi
    sleep 0.5
    ((i++))
  done

  echo "Error: cli-ai-gateway (PID $pid) did not pass healthcheck within 15s." >&2
  tail -n 30 "$LOG_FILE" >&2 || true
  exit 1
}

cmd_stop() {
  local pid
  pid="$(read_pid)"
  if [[ -z "$pid" ]]; then
    echo "cli-ai-gateway is not running (no PID file at $PID_FILE)."
    return 0
  fi
  if ! is_running "$pid"; then
    echo "Removing stale PID file ($pid)."
    rm -f "$PID_FILE"
    return 0
  fi

  echo "Stopping cli-ai-gateway (PID $pid)..."
  kill -TERM "$pid" 2>/dev/null || true

  local i=0
  while (( i < 24 )); do
    if ! is_running "$pid"; then
      rm -f "$PID_FILE"
      echo "Stopped cli-ai-gateway cleanly."
      return 0
    fi
    sleep 0.5
    ((i++))
  done

  echo "Graceful shutdown timed out; sending SIGKILL to PID $pid..."
  kill -KILL "$pid" 2>/dev/null || true
  sleep 1
  rm -f "$PID_FILE"
  echo "Stopped cli-ai-gateway (forced)."
}

cmd_restart() {
  cmd_stop
  cmd_start
}

cmd_status() {
  local pid url
  pid="$(read_pid)"
  url="$(resolve_base_url)"
  if ! is_running "$pid"; then
    echo "Status : STOPPED"
    echo "Config : $CONFIG_PATH"
    return 1
  fi

  echo "Status : RUNNING (PID $pid)"
  echo "Config : $CONFIG_PATH"
  echo "URL    : $url"
  echo "Admin  : ${url}/admin/"
  echo "Log    : $LOG_FILE"

  if command -v curl >/dev/null 2>&1; then
    echo "--- /health ---"
    curl -sS "${url}/health" || true
    echo ""
    echo "--- /ready ---"
    curl -sS "${url}/ready" || true
    echo ""
  elif [[ -x "$BIN_PATH" ]]; then
    "$BIN_PATH" -config "$CONFIG_PATH" -healthcheck
  fi
}

cmd_probe() {
  local endpoint="$1"
  local url
  url="$(resolve_base_url)"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS "${url}/${endpoint}"
    echo ""
  elif [[ "$endpoint" == "health" && -x "$BIN_PATH" ]]; then
    "$BIN_PATH" -config "$CONFIG_PATH" -healthcheck
  else
    echo "Error: curl is required to probe /${endpoint}" >&2
    exit 1
  fi
}

cmd_logs() {
  if [[ ! -f "$LOG_FILE" ]]; then
    echo "Log file does not exist yet: $LOG_FILE"
    exit 1
  fi
  if [[ "${1:-}" == "--no-follow" ]]; then
    tail -n "${2:-100}" "$LOG_FILE"
  else
    tail -n 100 -f "$LOG_FILE"
  fi
}

usage() {
  cat <<EOF
Usage: $(basename "$0") <command>

Commands:
  start                 Build (if needed), start gateway daemon, wait for /health
  stop                  Send SIGTERM and wait for graceful shutdown
  restart               Stop running gateway and start it again
  status                Show PID, URLs, /health and /ready state
  health                Query HTTP /health endpoint
  ready                 Query HTTP /ready endpoint (executors & storage readiness)
  logs [--no-follow]    Stream or print gateway log output ($LOG_FILE)

Environment overrides:
  GATEWAY_CONFIG        Path to YAML config (default: ./config.yaml or ./configs/config.example.yaml)
  GATEWAY_BIN           Path to compiled binary (default: ./bin/cli-ai-gateway)
  GATEWAY_PID_FILE      Path to PID file (default: ./data/gateway.pid)
  GATEWAY_LOG_FILE      Path to log file (default: ./data/gateway.log)
  GATEWAY_REBUILD=1     Force go build on start
EOF
}

case "${1:-}" in
  start)   cmd_start ;;
  stop)    cmd_stop ;;
  restart) cmd_restart ;;
  status)  cmd_status ;;
  health)  cmd_probe "health" ;;
  ready)   cmd_probe "ready" ;;
  logs)    shift; cmd_logs "$@" ;;
  *)       usage; exit 1 ;;
esac
