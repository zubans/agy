# OpenAI API Compatibility

CLI AI Gateway implements a focused subset of the OpenAI HTTP API tailored for executing tasks through local CLI agents (`codex`, `antigravity`, `gemini`, `claude`) and local Ollama models.

> Full compatibility with every OpenAI endpoint and parameter is intentionally **not** claimed; unsupported parameters are explicitly rejected with `400 invalid_request_error` (`code: "unsupported_parameter"`).

## Authentication

- All `/v1/*` routes require `Authorization: Bearer <API_KEY>`.
- Invalid, missing, or revoked keys return `401 authentication_error`.
- Model allowlists are enforced per key; disallowed models return `403 permission_error`.

## Supported Endpoints

### `GET /v1/models`
- Returns `{"object": "list", "data": [...]}` containing configured models enabled for the caller's API key.
- Service diagnostics endpoint `GET /v1/executors` exposes per-model executor capabilities (`streaming`, `session_resume`, `model_selection`, `file_input`, `supported_formats`, `token_usage`) and live readiness without leaking secrets or binary arguments.

### `POST /v1/files`
- Accepts `multipart/form-data` with:
  - `purpose`: required, must equal `user_data`.
  - `file`: UTF-8 text/markdown/code file up to `storage.max_file_bytes` (default 50 MiB).
- Returns `201 Created` with OpenAI `file` object (`id`, `object`, `bytes`, `created_at`, `filename`, `purpose`).
- Ownership is bound to the API key's `owner_id`.

### `GET /v1/files/{file_id}` & `GET /v1/files/{file_id}/content`
- Returns metadata (`JSON`) or raw file contents to the owning client.
- Accessing another owner's file returns `403`. Missing files return `404`.

### `DELETE /v1/files/{file_id}`
- Deletes the file from disk and SQLite.
- If the file is currently referenced by a running task, returns `409 Conflict` (`code: "file_in_use"`).

### `POST /v1/responses`
Supported request fields:
- `model` (required `string`)
- `input` (required `string` or array of message/content items containing `input_file` and `input_text` / `text`)
- `stream` (optional `boolean`, SSE mode)
- `previous_response_id` (optional `string`, continues the owning client's session)
- `instructions` (optional `string`)

Unsupported top-level fields (such as `tools`, `temperature`, `top_p`, `reasoning`, `background`, `max_output_tokens`, `metadata`, `store`, `include`, etc.) and unsupported input part types (such as `input_image`, `input_audio`) are rejected with `400 invalid_request_error`.

Streaming (`stream: true`) emits OpenAI Responses API SSE events (`event:` + `data:`):
- `response.created`
- `response.in_progress`
- `response.output_item.added`
- `response.content_part.added`
- `response.output_text.delta`
- `response.output_text.done`
- `response.content_part.done`
- `response.output_item.done`
- `response.completed`
- On mid-stream CLI failure: `error` and `response.failed` (never `response.completed`).

### `POST /v1/chat/completions`
Supported request fields:
- `model` (required `string`)
- `messages` (required non-empty array of `{role, content}` with `system`, `developer`, `user`, `assistant`)
- `stream` (optional `boolean`)

Unsupported fields (`tools`, `functions`, `temperature`, `n`, `max_tokens`, etc.) are rejected with `400 invalid_request_error`.

Streaming (`stream: true`) emits `chat.completion.chunk` SSE events terminated by `data: [DONE]`.

## Official SDK Compatibility

Verified in `tests/compat_test.go` against the official OpenAI Go SDK (`github.com/openai/openai-go/v3`) with custom `WithBaseURL` and `WithAPIKey`:
- `client.Models.List`
- `client.Files.New`, `client.Files.Get`, `client.Files.Content`, `client.Files.Delete`
- `client.Responses.New` (with `input_file`, `input_text`, `previous_response_id`)
- `client.Responses.NewStreaming`
- `client.Chat.Completions.New` and `client.Chat.Completions.NewStreaming`
