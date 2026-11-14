package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"cli-ai-gateway/internal/auth"
)

type ctxKey string

const (
	ctxKeyPrincipal ctxKey = "principal"
	ctxKeyRequestID ctxKey = "request_id"
	ctxKeyAudit     ctxKey = "audit"
)

// RequestAudit holds non-secret per-request metrics logged at request completion (Section 14).
type RequestAudit struct {
	mu             sync.Mutex
	Model          string
	Executor       string
	CLIDurationMS  int64
	ExitCode       *int
	InputFileBytes int64
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	ErrorType      string
}

func (a *RequestAudit) SetModel(model, execName string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Model = model
	a.Executor = execName
	a.mu.Unlock()
}

func (a *RequestAudit) SetInputBytes(n int64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.InputFileBytes = n
	a.mu.Unlock()
}

func (a *RequestAudit) SetResult(cliDurationMS int64, exitCode int, inTok, outTok, totTok int64, errType string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.CLIDurationMS = cliDurationMS
	a.ExitCode = &exitCode
	a.InputTokens = inTok
	a.OutputTokens = outTok
	a.TotalTokens = totTok
	a.ErrorType = errType
	a.mu.Unlock()
}

func (a *RequestAudit) SetErrorType(errType string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.ErrorType = errType
	a.mu.Unlock()
}

func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	v, ok := ctx.Value(ctxKeyPrincipal).(auth.Principal)
	return v, ok
}

func AuditFromContext(ctx context.Context) *RequestAudit {
	v, _ := ctx.Value(ctxKeyAudit).(*RequestAudit)
	return v
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if fl, ok := s.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// LoggingAndRecoveryMiddleware attaches request IDs, catches panics, and emits structured JSON logs
// without ever logging API keys, OAuth tokens, or prompt payloads (Section 14).
func LoggingAndRecoveryMiddleware(logger *slog.Logger, diagnostic bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now().UTC()
		reqID := sanitizeRequestID(r.Header.Get("X-Request-Id"))
		if reqID == "" {
			reqID = randomRequestID()
		}

		audit := &RequestAudit{}
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, reqID)
		ctx = context.WithValue(ctx, ctxKeyAudit, audit)
		r = r.WithContext(ctx)

		w.Header().Set("X-Request-Id", reqID)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if pv := recover(); pv != nil {
				logger.Error("http panic recovered",
					"request_id", reqID,
					"method", r.Method,
					"path", r.URL.Path,
					"panic", pv,
				)
				if rec.status == http.StatusOK {
					writeError(rec, http.StatusInternalServerError, "server_error", "internal_error", "Internal server error", nil)
				}
			}

			finishedAt := time.Now().UTC()
			audit.mu.Lock()
			attrs := []any{
				slog.String("request_id", reqID),
				slog.Time("started_at", startedAt),
				slog.Time("finished_at", finishedAt),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", finishedAt.Sub(startedAt).Milliseconds()),
			}
			if audit.Model != "" {
				attrs = append(attrs, slog.String("model", audit.Model))
			}
			if audit.Executor != "" {
				attrs = append(attrs, slog.String("executor", audit.Executor))
			}
			if audit.CLIDurationMS > 0 {
				attrs = append(attrs, slog.Int64("cli_duration_ms", audit.CLIDurationMS))
			}
			if audit.ExitCode != nil {
				attrs = append(attrs, slog.Int("exit_code", *audit.ExitCode))
			}
			if audit.InputFileBytes > 0 {
				attrs = append(attrs, slog.Int64("input_file_bytes", audit.InputFileBytes))
			}
			if audit.TotalTokens > 0 || audit.InputTokens > 0 || audit.OutputTokens > 0 {
				attrs = append(attrs,
					slog.Int64("input_tokens", audit.InputTokens),
					slog.Int64("output_tokens", audit.OutputTokens),
					slog.Int64("total_tokens", audit.TotalTokens),
				)
			}
			if audit.ErrorType != "" {
				attrs = append(attrs, slog.String("error_type", audit.ErrorType))
			}
			if diagnostic {
				attrs = append(attrs, slog.String("remote_addr", r.RemoteAddr))
			}
			audit.mu.Unlock()

			level := slog.LevelInfo
			if r.URL.Path == "/health" || r.URL.Path == "/ready" {
				level = slog.LevelDebug
			}
			logger.Log(ctx, level, "http_request", attrs...)
		}()

		next.ServeHTTP(rec, r)
	})
}

// RequireAuth validates Bearer API key tokens on all /v1/* endpoints (Section 6 & 11.1).
func RequireAuth(authStore *auth.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := authStore.AuthenticateBearer(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			if audit := AuditFromContext(r.Context()); audit != nil {
				audit.SetErrorType("invalid_api_key")
			}
			code := "invalid_api_key"
			msg := "Invalid or missing Bearer API key"
			if errors.Is(err, auth.ErrKeyRevoked) {
				code = "revoked_api_key"
				msg = "API key has been revoked"
			}
			writeError(w, http.StatusUnauthorized, "authentication_error", code, msg, nil)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func randomRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 64 {
		return ""
	}
	for _, r := range raw {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return ""
		}
	}
	return raw
}
