package worker

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"cli-ai-gateway/internal/executor"
)

const (
	// LivezPath is the unauthenticated process liveness endpoint on worker containers.
	LivezPath = "/livez"
	// HealthPath probes worker authentication and CLI binary readiness.
	HealthPath = "/worker/health"
	// ExecutePath streams task execution and staged workspace files to a worker container.
	ExecutePath = "/worker/execute"
)

// HealthResponse is returned by GET /worker/health.
type HealthResponse struct {
	Ready        bool                  `json:"ready"`
	Provider     string                `json:"provider"`
	Executable   string                `json:"executable,omitempty"`
	Error        string                `json:"error,omitempty"`
	Capabilities executor.Capabilities `json:"capabilities"`
}

// FileMeta describes one staged input file uploaded alongside a worker execute request.
type FileMeta struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name"`
	RelativePath string `json:"relative_path"`
	Size         int64  `json:"size"`
	FieldName    string `json:"field_name"`
}

// SessionPayload transfers gateway/provider session context to a worker.
type SessionPayload struct {
	GatewaySessionID  string `json:"gateway_session_id,omitempty"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	IsResume          bool   `json:"is_resume,omitempty"`
	HistoryFile       string `json:"history_file,omitempty"`
}

// ExecuteMeta is the JSON metadata part ("meta") of POST /worker/execute.
type ExecuteMeta struct {
	RunID          string         `json:"run_id"`
	Model          string         `json:"model"`
	ProviderModel  string         `json:"provider_model,omitempty"`
	Instructions   string         `json:"instructions,omitempty"`
	Prompt         string         `json:"prompt,omitempty"`
	Session        SessionPayload `json:"session"`
	Files          []FileMeta     `json:"files,omitempty"`
	TimeoutSeconds int64          `json:"timeout_seconds"`
	MaxOutputBytes int64          `json:"max_output_bytes"`
	Stream         bool           `json:"stream"`
	DiagnosticLog  bool           `json:"diagnostic_log"`
}

// ErrorPayload serializes executor.ExecError across HTTP boundaries.
type ErrorPayload struct {
	Kind     executor.ErrorKind `json:"kind"`
	Code     string             `json:"code"`
	Message  string             `json:"message"`
	ExitCode int                `json:"exit_code"`
}

// ResultPayload serializes executor.ExecuteResult in the terminal NDJSON frame.
type ResultPayload struct {
	Answer            string         `json:"answer"`
	ProviderSessionID string         `json:"provider_session_id,omitempty"`
	Status            string         `json:"status"`
	Usage             executor.Usage `json:"usage"`
	ExitCode          int            `json:"exit_code"`
	Error             *ErrorPayload  `json:"error,omitempty"`
}

// StreamFrame is one NDJSON line returned by POST /worker/execute.
type StreamFrame struct {
	Type   string          `json:"type"` // "event" | "done"
	Event  *executor.Event `json:"event,omitempty"`
	Result *ResultPayload  `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// CleanRelativePath validates and normalizes a workspace-relative file path so staged files cannot escape runWorkDir.
func CleanRelativePath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsRune(trimmed, 0) || strings.ContainsRune(trimmed, '\\') {
		return "", errors.New("invalid relative file path")
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "~") {
		return "", fmt.Errorf("relative path %q must not be absolute", raw)
	}
	// Reject Windows drive letter prefixes such as "C:foo".
	if len(trimmed) >= 2 && trimmed[1] == ':' {
		return "", fmt.Errorf("invalid path prefix in %q", raw)
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("relative path %q escapes workspace", raw)
	}
	return cleaned, nil
}

func constantTimeBearerMatch(authHeader string, expectedHash [32]byte) bool {
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return false
	}
	gotHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) == 1
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
