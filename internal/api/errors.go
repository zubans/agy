package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
)

// OpenAIErrorEnvelope is the standard OpenAI error payload (Section 12).
type OpenAIErrorEnvelope struct {
	Error OpenAIError `json:"error"`
}

// OpenAIError contains structured error metadata without leaking raw CLI secrets.
type OpenAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, errType, code, message string, param *string) {
	writeJSON(w, status, OpenAIErrorEnvelope{
		Error: OpenAIError{
			Message: message,
			Type:    errType,
			Param:   param,
			Code:    code,
		},
	})
}

func writeInvalidRequest(w http.ResponseWriter, code, message string, param string) {
	var p *string
	if param != "" {
		p = &param
	}
	writeError(w, http.StatusBadRequest, "invalid_request_error", code, message, p)
}

func mapStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrFileNotFound):
		writeError(w, http.StatusNotFound, "invalid_request_error", "file_not_found", "File not found", nil)
	case errors.Is(err, storage.ErrFileForbidden):
		writeError(w, http.StatusForbidden, "permission_error", "forbidden", "You do not have access to this file", nil)
	case errors.Is(err, storage.ErrFileTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large", "Uploaded file exceeds maximum allowed size", nil)
	case errors.Is(err, storage.ErrStorageQuota):
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "storage_quota_exceeded", "Gateway storage quota exceeded", nil)
	case errors.Is(err, storage.ErrInvalidFileType), errors.Is(err, storage.ErrInvalidFilename):
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_file", err.Error(), nil)
	case errors.Is(err, storage.ErrFileInUse):
		writeError(w, http.StatusConflict, "conflict_error", "file_in_use", "File is currently used by an active task", nil)
	default:
		writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "Internal storage error", nil)
	}
}

func mapSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrResponseNotFound):
		p := "previous_response_id"
		writeError(w, http.StatusNotFound, "invalid_request_error", "response_not_found", "Previous response not found", &p)
	case errors.Is(err, session.ErrResponseForbidden):
		p := "previous_response_id"
		writeError(w, http.StatusForbidden, "permission_error", "forbidden", "You do not have access to the referenced response", &p)
	case errors.Is(err, session.ErrSessionBusy):
		writeError(w, http.StatusConflict, "conflict_error", "session_conflict", "Another task is currently running in this session", nil)
	default:
		writeError(w, http.StatusInternalServerError, "server_error", "session_error", "Failed to manage session state", nil)
	}
}

func mapExecutorHTTPStatus(err error) (status int, errType, code, msg string) {
	var execErr *executor.ExecError
	if errors.As(err, &execErr) {
		switch execErr.Kind {
		case executor.ErrUnavailable:
			return http.StatusServiceUnavailable, "server_error", execErr.Code, execErr.Message
		case executor.ErrTimeout:
			return http.StatusGatewayTimeout, "server_error", execErr.Code, execErr.Message
		case executor.ErrRateLimit:
			return http.StatusTooManyRequests, "rate_limit_error", execErr.Code, execErr.Message
		case executor.ErrAuth:
			return http.StatusBadGateway, "server_error", execErr.Code, execErr.Message
		case executor.ErrCancelled:
			return 499, "client_closed_request", execErr.Code, execErr.Message
		default:
			return http.StatusBadGateway, "server_error", execErr.Code, execErr.Message
		}
	}
	if errors.Is(err, process.ErrQueueFull) || errors.Is(err, process.ErrQueueTimeout) {
		return http.StatusTooManyRequests, "rate_limit_error", "queue_limit_exceeded", err.Error()
	}
	return http.StatusBadGateway, "server_error", "cli_execution_error", "Executor failed to complete request"
}
