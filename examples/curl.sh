#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${GATEWAY_URL:-http://127.0.0.1:7070}"
API_KEY="${GATEWAY_API_KEY:-gw-dev-change-me}"
MODEL="${GATEWAY_MODEL:-codex}"

echo "==> 1. Health & Readiness"
curl -fsS "${BASE_URL}/health"
echo
curl -fsS "${BASE_URL}/ready"
echo

echo "==> 2. List Models"
curl -fsS "${BASE_URL}/v1/models" \
  -H "Authorization: Bearer ${API_KEY}"
echo

echo "==> 3. Upload prompt.md via OpenAI Files API"
TMP_PROMPT="$(mktemp /tmp/prompt-XXXXXX.md)"
trap 'rm -f "${TMP_PROMPT}"' EXIT
cat > "${TMP_PROMPT}" <<'EOF'
# System Instructions

You are a concise engineering assistant.

# Conversation Context

(none)

# Task

Confirm that you read this prompt file and summarize its sections.

# Additional Context

Gateway file-based task test.

# Expected Output

A brief 2-sentence confirmation.
EOF

FILE_JSON="$(curl -fsS "${BASE_URL}/v1/files" \
  -H "Authorization: Bearer ${API_KEY}" \
  -F "purpose=user_data" \
  -F "file=@${TMP_PROMPT};filename=prompt.md")"
echo "${FILE_JSON}"

FILE_ID="$(printf '%s' "${FILE_JSON}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')"

echo "==> 4. Inspect uploaded file metadata"
curl -fsS "${BASE_URL}/v1/files/${FILE_ID}" \
  -H "Authorization: Bearer ${API_KEY}"
echo

echo "==> 5. Execute Responses API (sync)"
RESP_JSON="$(curl -fsS "${BASE_URL}/v1/responses" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"${MODEL}\",
    \"input\": [
      {
        \"role\": \"user\",
        \"content\": [
          {\"type\": \"input_file\", \"file_id\": \"${FILE_ID}\"},
          {\"type\": \"input_text\", \"text\": \"Execute the task in prompt.md\"}
        ]
      }
    ],
    \"stream\": false
  }")"
echo "${RESP_JSON}"

RESP_ID="$(printf '%s' "${RESP_JSON}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')"

echo "==> 6. Continue dialogue using previous_response_id (streaming SSE)"
curl -N -fsS "${BASE_URL}/v1/responses" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"${MODEL}\",
    \"previous_response_id\": \"${RESP_ID}\",
    \"input\": \"Summarize your previous answer in 5 words.\",
    \"stream\": true
  }"
echo

echo "==> 7. Chat Completions compatibility"
curl -fsS "${BASE_URL}/v1/chat/completions" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"${MODEL}\",
    \"messages\": [
      {\"role\": \"user\", \"content\": \"Say ok.\"}
    ],
    \"stream\": false
  }"
echo

echo "==> 8. Delete uploaded file"
curl -fsS -X DELETE "${BASE_URL}/v1/files/${FILE_ID}" \
  -H "Authorization: Bearer ${API_KEY}"
echo
