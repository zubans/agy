package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
)

//go:embed ui/index.html
var uiAssets embed.FS

const (
	sessionCookieName = "agy_admin_session"
	sessionTTL        = 24 * time.Hour
)

// PlaygroundInput describes an interactive test run triggered from the Web Admin console.
type PlaygroundInput struct {
	OwnerID            string   `json:"owner_id"`
	ModelID            string   `json:"model"`
	Instructions       string   `json:"instructions"`
	Prompt             string   `json:"prompt"`
	PreviousResponseID string   `json:"previous_response_id"`
	FileIDs            []string `json:"file_ids"`
	Stream             bool     `json:"stream"`
}

// PlaygroundRunner executes an interactive test prompt using the gateway's standard pipeline.
type PlaygroundRunner interface {
	ExecutePlayground(ctx context.Context, w http.ResponseWriter, in PlaygroundInput)
}

// Dependencies supplies live runtime services manipulated by the Web Admin UI.
type Dependencies struct {
	Config     *config.Config
	StartedAt  time.Time
	DB         *storage.DB
	Auth       *auth.Store
	Files      *storage.FileStore
	Sessions   *session.Manager
	Registry   *executor.Registry
	ProcMgr    *process.Manager
	Playground PlaygroundRunner
}

// Handler serves the embedded Web Admin SPA and its management JSON APIs.
type Handler struct {
	deps       Dependencies
	hmacSecret []byte
	indexHTML  []byte
}

// NewHandler creates a Web Admin handler with an ephemeral HMAC session signing key.
func NewHandler(deps Dependencies) *Handler {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)

	if deps.StartedAt.IsZero() {
		deps.StartedAt = time.Now()
	}
	indexHTML, _ := uiAssets.ReadFile("ui/index.html")

	return &Handler{
		deps:       deps,
		hmacSecret: secret,
		indexHTML:  indexHTML,
	}
}

// RegisterRoutes mounts the Web Admin UI and API routes on mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /admin/", h.serveIndex)
	mux.HandleFunc("GET /admin/api/auth", h.handleAuthStatus)
	mux.HandleFunc("POST /admin/api/login", h.handleLogin)
	mux.HandleFunc("POST /admin/api/logout", h.handleLogout)

	mux.HandleFunc("GET /admin/api/status", h.requireAdmin(h.handleStatus))
	mux.HandleFunc("GET /admin/api/config", h.requireAdmin(h.handleConfig))

	mux.HandleFunc("GET /admin/api/models", h.requireAdmin(h.handleListModels))
	mux.HandleFunc("POST /admin/api/models", h.requireAdmin(h.handleCreateModel))
	mux.HandleFunc("POST /admin/api/models/check", h.requireAdmin(h.handleCheckAllModels))
	mux.HandleFunc("POST /admin/api/models/{model_id}/check", h.requireAdmin(h.handleCheckOneModel))
	mux.HandleFunc("PATCH /admin/api/models/{model_id}", h.requireAdmin(h.handlePatchModel))
	mux.HandleFunc("DELETE /admin/api/models/{model_id}", h.requireAdmin(h.handleDeleteModel))

	mux.HandleFunc("GET /admin/api/keys", h.requireAdmin(h.handleListKeys))
	mux.HandleFunc("POST /admin/api/keys", h.requireAdmin(h.handleCreateKey))
	mux.HandleFunc("PATCH /admin/api/keys/{key_id}", h.requireAdmin(h.handleUpdateKey))
	mux.HandleFunc("POST /admin/api/keys/{key_id}/revoke", h.requireAdmin(h.handleSetKeyRevoked(true)))
	mux.HandleFunc("POST /admin/api/keys/{key_id}/unrevoke", h.requireAdmin(h.handleSetKeyRevoked(false)))
	mux.HandleFunc("DELETE /admin/api/keys/{key_id}", h.requireAdmin(h.handleDeleteKey))

	mux.HandleFunc("GET /admin/api/runs", h.requireAdmin(h.handleListRuns))
	mux.HandleFunc("GET /admin/api/runs/{run_id}", h.requireAdmin(h.handleGetRun))
	mux.HandleFunc("POST /admin/api/runs/{run_id}/cancel", h.requireAdmin(h.handleCancelRun))

	mux.HandleFunc("GET /admin/api/sessions", h.requireAdmin(h.handleListSessions))
	mux.HandleFunc("GET /admin/api/sessions/{session_id}", h.requireAdmin(h.handleGetSession))
	mux.HandleFunc("DELETE /admin/api/sessions/{session_id}", h.requireAdmin(h.handleDeleteSession))

	mux.HandleFunc("GET /admin/api/files", h.requireAdmin(h.handleListFiles))
	mux.HandleFunc("POST /admin/api/files", h.requireAdmin(h.handleUploadFile))
	mux.HandleFunc("GET /admin/api/files/{file_id}/content", h.requireAdmin(h.handleFileContent))
	mux.HandleFunc("DELETE /admin/api/files/{file_id}", h.requireAdmin(h.handleDeleteFile))

	mux.HandleFunc("POST /admin/api/cleanup", h.requireAdmin(h.handleCleanup))
	mux.HandleFunc("POST /admin/api/playground", h.requireAdmin(h.handlePlayground))
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.indexHTML)
}

func (h *Handler) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	authRequired := !h.deps.Config.Admin.AllowWithoutAuth
	authenticated := !authRequired || h.isAuthorizedRequest(r, false)
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"authenticated": authenticated,
		"auth_required": authRequired,
	})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if h.deps.Config.Admin.AllowWithoutAuth {
		writeAdminJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON payload")
		return
	}
	if !h.verifyAdminToken(body.Token) {
		writeAdminError(w, http.StatusUnauthorized, "unauthorized", "Invalid admin token")
		return
	}

	exp := time.Now().Add(sessionTTL).Unix()
	cookieVal := h.signSession(exp)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    cookieVal,
		Path:     "/admin",
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeAdminJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (h *Handler) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeAdminJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (h *Handler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if !h.isAuthorizedRequest(r, true) {
			writeAdminError(w, http.StatusUnauthorized, "unauthorized", "Admin authentication required")
			return
		}
		next(w, r)
	}
}

func (h *Handler) isAuthorizedRequest(r *http.Request, enforceCSRF bool) bool {
	if h.deps.Config.Admin.AllowWithoutAuth {
		return !enforceCSRF || !isMutatingMethod(r.Method) || r.Header.Get("X-Admin-UI") == "1" || parseBearer(r.Header.Get("Authorization")) != ""
	}
	if tok := parseBearer(r.Header.Get("Authorization")); tok != "" {
		return h.verifyAdminToken(tok)
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || !h.verifySessionCookie(c.Value) {
		return false
	}
	if enforceCSRF && isMutatingMethod(r.Method) && r.Header.Get("X-Admin-UI") != "1" {
		return false
	}
	return true
}

func (h *Handler) verifyAdminToken(candidate string) bool {
	expectedHash := h.deps.Config.Admin.EffectiveHash()
	if expectedHash == "" || strings.TrimSpace(candidate) == "" {
		return false
	}
	sum := sha256.Sum256([]byte(candidate))
	candHash := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(candHash), []byte(expectedHash)) == 1
}

func (h *Handler) signSession(expUnix int64) string {
	payload := fmt.Sprintf("%d.%s", expUnix, h.deps.Config.Admin.EffectiveHash())
	mac := hmac.New(sha256.New, h.hmacSecret)
	_, _ = mac.Write([]byte(payload))
	return fmt.Sprintf("%d.%s", expUnix, hex.EncodeToString(mac.Sum(nil)))
}

func (h *Handler) verifySessionCookie(val string) bool {
	parts := strings.SplitN(val, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return false
	}
	expected := h.signSession(expUnix)
	return subtle.ConstantTimeCompare([]byte(val), []byte(expected)) == 1
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	report := h.deps.Registry.CheckAll(ctx)
	procStats := h.deps.ProcMgr.Stats()
	usedBytes, _ := h.deps.Files.TotalStoredBytes(ctx)
	dbErr := h.deps.DB.Ping(ctx)

	var activeRuns, totalRuns, totalSessions, activeKeys, totalFiles int
	_ = h.deps.DB.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM runs WHERE status IN ('queued','running')`).Scan(&activeRuns)
	_ = h.deps.DB.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM runs`).Scan(&totalRuns)
	_ = h.deps.DB.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM sessions`).Scan(&totalSessions)
	_ = h.deps.DB.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM api_keys WHERE revoked = 0`).Scan(&activeKeys)
	_ = h.deps.DB.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM files WHERE deleted = 0`).Scan(&totalFiles)

	statusStr := "ok"
	if dbErr != nil {
		statusStr = "down"
	} else if report.ReadyCount == 0 && report.TotalCount > 0 {
		statusStr = "unavailable"
	} else if report.ReadyCount < report.TotalCount {
		statusStr = "degraded"
	}

	writeAdminJSON(w, http.StatusOK, map[string]any{
		"status":         statusStr,
		"started_at":     h.deps.StartedAt.Unix(),
		"uptime_seconds": int64(time.Since(h.deps.StartedAt).Seconds()),
		"listen":         h.deps.Config.Server.Listen,
		"db_ok":          dbErr == nil,
		"process": map[string]any{
			"active_global":        procStats.ActiveGlobal,
			"queued":               procStats.Queued,
			"max_global_processes": h.deps.Config.Limits.MaxGlobalProcesses,
			"max_queue_size":       h.deps.Config.Limits.MaxQueueSize,
		},
		"storage": map[string]any{
			"path":           h.deps.Files.RootDir(),
			"used_bytes":     usedBytes,
			"quota_bytes":    h.deps.Config.Storage.QuotaBytes,
			"max_file_bytes": h.deps.Config.Storage.MaxFileBytes,
			"file_count":     totalFiles,
		},
		"counts": map[string]any{
			"active_runs":  activeRuns,
			"total_runs":   totalRuns,
			"sessions":     totalSessions,
			"active_keys":  activeKeys,
			"ready_models": report.ReadyCount,
			"total_models": len(h.deps.Registry.ListAll()),
		},
		"readiness": report,
	})
}

func (h *Handler) handleConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := h.deps.Config
	redactedKeys := make([]map[string]any, 0, len(cfg.Auth.Keys))
	for _, k := range cfg.Auth.Keys {
		redactedKeys = append(redactedKeys, map[string]any{
			"id":             k.ID,
			"owner_id":       k.OwnerID,
			"name":           k.Name,
			"allowed_models": k.AllowedModels,
			"max_concurrent": k.MaxConcurrent,
			"revoked":        k.Revoked,
			"key":            "[REDACTED]",
		})
	}
	models := h.deps.Registry.ListAll()
	redactedModels := make([]map[string]any, 0, len(models))
	for _, m := range models {
		envKeys := make([]string, 0, len(m.Config.Env))
		for k := range m.Config.Env {
			envKeys = append(envKeys, k)
		}
		redactedModels = append(redactedModels, map[string]any{
			"id":              m.Config.ID,
			"provider":        m.Config.Provider,
			"executable":      m.Config.Executable,
			"base_url":        m.Config.BaseURL,
			"model":           m.Config.Model,
			"timeout_seconds": m.Config.TimeoutSeconds,
			"sandbox_mode":    m.Config.SandboxMode,
			"fs_access":       m.Config.FSAccess,
			"allow_commands":  m.Config.AllowCommands,
			"max_concurrent":  m.Config.MaxConcurrent,
			"enabled":         m.Config.EffectiveEnabled(),
			"dynamic":         m.Dynamic,
			"env_keys":        envKeys,
		})
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"server":  cfg.Server,
		"storage": cfg.Storage,
		"limits":  cfg.Limits,
		"logging": cfg.Logging,
		"admin": map[string]any{
			"enabled":            cfg.Admin.Enabled,
			"allow_without_auth": cfg.Admin.AllowWithoutAuth,
			"token":              "[REDACTED]",
		},
		"auth": map[string]any{
			"configured_keys": redactedKeys,
		},
		"models": redactedModels,
	})
}

type adminModelView struct {
	ID             string                `json:"id"`
	Provider       string                `json:"provider"`
	Executable     string                `json:"executable,omitempty"`
	BaseURL        string                `json:"base_url,omitempty"`
	Model          string                `json:"model,omitempty"`
	TimeoutSeconds int                   `json:"timeout_seconds"`
	MaxConcurrent  int                   `json:"max_concurrent"`
	SandboxMode    string                `json:"sandbox_mode,omitempty"`
	FSAccess       string                `json:"fs_access,omitempty"`
	Enabled        bool                  `json:"enabled"`
	Dynamic        bool                  `json:"dynamic"`
	Ready          bool                  `json:"ready"`
	Error          string                `json:"error,omitempty"`
	Capabilities   executor.Capabilities `json:"capabilities"`
}

func (h *Handler) handleListModels(w http.ResponseWriter, r *http.Request) {
	items := h.deps.Registry.ListAll()
	views := make([]adminModelView, 0, len(items))
	for _, m := range items {
		st, _ := h.deps.Registry.CheckOne(r.Context(), m.Config.ID)
		views = append(views, adminModelView{
			ID:             m.Config.ID,
			Provider:       m.Config.Provider,
			Executable:     m.Config.Executable,
			BaseURL:        m.Config.BaseURL,
			Model:          m.Config.Model,
			TimeoutSeconds: m.Config.TimeoutSeconds,
			MaxConcurrent:  m.Config.MaxConcurrent,
			SandboxMode:    m.Config.SandboxMode,
			FSAccess:       m.Config.FSAccess,
			Enabled:        m.Config.EffectiveEnabled(),
			Dynamic:        m.Dynamic,
			Ready:          st.Ready,
			Error:          st.Error,
			Capabilities:   m.Capabilities,
		})
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": views})
}

func (h *Handler) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID             string `json:"id"`
		Provider       string `json:"provider"`
		BaseURL        string `json:"base_url"`
		Model          string `json:"model"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		MaxConcurrent  int    `json:"max_concurrent"`
		Enabled        *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	if body.Provider != "" && body.Provider != "ollama" {
		writeAdminError(w, http.StatusBadRequest, "invalid_provider", "Only dynamic 'ollama' models can be added via Web Admin for binary execution security")
		return
	}
	info, err := h.deps.Registry.AddDynamicOllamaModel(r.Context(), h.deps.DB.SQL(), config.ModelConfig{
		ID:             body.ID,
		Provider:       "ollama",
		BaseURL:        body.BaseURL,
		Model:          body.Model,
		TimeoutSeconds: body.TimeoutSeconds,
		MaxConcurrent:  body.MaxConcurrent,
		Enabled:        body.Enabled,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, executor.ErrModelAlreadyExists) {
			status = http.StatusConflict
		}
		writeAdminError(w, status, "model_create_failed", err.Error())
		return
	}
	st, _ := h.deps.Registry.CheckOne(r.Context(), info.Config.ID)
	writeAdminJSON(w, http.StatusCreated, adminModelView{
		ID:             info.Config.ID,
		Provider:       info.Config.Provider,
		BaseURL:        info.Config.BaseURL,
		Model:          info.Config.Model,
		TimeoutSeconds: info.Config.TimeoutSeconds,
		MaxConcurrent:  info.Config.MaxConcurrent,
		FSAccess:       info.Config.FSAccess,
		Enabled:        info.Config.EffectiveEnabled(),
		Dynamic:        info.Dynamic,
		Ready:          st.Ready,
		Error:          st.Error,
		Capabilities:   info.Capabilities,
	})
}

func (h *Handler) handleCheckAllModels(w http.ResponseWriter, r *http.Request) {
	h.handleListModels(w, r)
}

func (h *Handler) handleCheckOneModel(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("model_id")
	st, ok := h.deps.Registry.CheckOne(r.Context(), modelID)
	if !ok {
		writeAdminError(w, http.StatusNotFound, "model_not_found", "Model not found")
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"id":           modelID,
		"provider":     st.Provider,
		"ready":        st.Ready,
		"error":        st.Error,
		"capabilities": st.Capabilities,
	})
}

func (h *Handler) handlePatchModel(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("model_id")
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&raw); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	if _, hasExec := raw["executable"]; hasExec {
		writeAdminError(w, http.StatusBadRequest, "forbidden_field", "CLI executable path cannot be altered via Web Admin")
		return
	}

	var patch executor.ModelPatch
	if v, ok := raw["enabled"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_field", "enabled must be boolean")
			return
		}
		patch.Enabled = &b
	}
	if v, ok := raw["timeout_seconds"]; ok {
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_field", "timeout_seconds must be integer")
			return
		}
		patch.TimeoutSeconds = &n
	}
	if v, ok := raw["max_concurrent"]; ok {
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_field", "max_concurrent must be integer")
			return
		}
		patch.MaxConcurrent = &n
	}
	if v, ok := raw["model"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_field", "model must be string")
			return
		}
		patch.Model = &s
	}
	if v, ok := raw["base_url"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_field", "base_url must be string")
			return
		}
		patch.BaseURL = &s
	}

	info, err := h.deps.Registry.UpdateRuntimeModel(r.Context(), h.deps.DB.SQL(), modelID, patch)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, executor.ErrModelNotFound) {
			status = http.StatusNotFound
		}
		writeAdminError(w, status, "model_update_failed", err.Error())
		return
	}
	st, _ := h.deps.Registry.CheckOne(r.Context(), info.Config.ID)
	writeAdminJSON(w, http.StatusOK, adminModelView{
		ID:             info.Config.ID,
		Provider:       info.Config.Provider,
		Executable:     info.Config.Executable,
		BaseURL:        info.Config.BaseURL,
		Model:          info.Config.Model,
		TimeoutSeconds: info.Config.TimeoutSeconds,
		MaxConcurrent:  info.Config.MaxConcurrent,
		SandboxMode:    info.Config.SandboxMode,
		FSAccess:       info.Config.FSAccess,
		Enabled:        info.Config.EffectiveEnabled(),
		Dynamic:        info.Dynamic,
		Ready:          st.Ready,
		Error:          st.Error,
		Capabilities:   info.Capabilities,
	})
}

func (h *Handler) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("model_id")
	if err := h.deps.Registry.DeleteDynamicModel(r.Context(), h.deps.DB.SQL(), modelID); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, executor.ErrModelNotFound) {
			status = http.StatusNotFound
		}
		writeAdminError(w, status, "model_delete_failed", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"id": modelID, "deleted": true})
}

func (h *Handler) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.deps.Auth.ListKeys(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "list_keys_failed", err.Error())
		return
	}
	if keys == nil {
		keys = []auth.KeySummary{}
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": keys})
}

func (h *Handler) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OwnerID       string   `json:"owner_id"`
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		MaxConcurrent int      `json:"max_concurrent"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON payload")
		return
	}
	summary, token, err := h.deps.Auth.CreateKey(r.Context(), body.OwnerID, body.Name, body.AllowedModels, body.MaxConcurrent)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "create_key_failed", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusCreated, map[string]any{
		"key":   summary,
		"token": token,
	})
}

func (h *Handler) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("key_id")
	var body struct {
		Name          *string  `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		MaxConcurrent *int     `json:"max_concurrent"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON payload")
		return
	}
	summary, err := h.deps.Auth.UpdateKey(r.Context(), keyID, body.Name, body.AllowedModels, body.MaxConcurrent)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeAdminError(w, status, "update_key_failed", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, summary)
}

func (h *Handler) handleSetKeyRevoked(revoked bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID := r.PathValue("key_id")
		if err := h.deps.Auth.SetRevoked(r.Context(), keyID, revoked); err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			writeAdminError(w, status, "revoke_key_failed", err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"id": keyID, "revoked": revoked})
	}
}

func (h *Handler) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("key_id")
	if err := h.deps.Auth.DeleteKey(r.Context(), keyID); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeAdminError(w, status, "delete_key_failed", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"id": keyID, "deleted": true})
}

func (h *Handler) handleListRuns(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := h.deps.Sessions.ListRuns(r.Context(), statusFilter, limit)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "list_runs_failed", err.Error())
		return
	}
	if runs == nil {
		runs = []session.RunSummary{}
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": runs})
}

func (h *Handler) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	detail, err := h.deps.Sessions.GetRunDetail(r.Context(), runID)
	if errors.Is(err, sql.ErrNoRows) {
		writeAdminError(w, http.StatusNotFound, "run_not_found", "Run not found")
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "get_run_failed", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, detail)
}

func (h *Handler) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if !h.deps.Sessions.CancelRun(runID) {
		writeAdminError(w, http.StatusConflict, "run_not_active", "Run is not currently executing in this process")
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"id": runID, "cancelling": true})
}

func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	sessions, err := h.deps.Sessions.ListSessions(r.Context(), limit)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "list_sessions_failed", err.Error())
		return
	}
	if sessions == nil {
		sessions = []session.SessionSummary{}
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": sessions})
}

func (h *Handler) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	detail, err := h.deps.Sessions.GetSessionDetail(r.Context(), sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		writeAdminError(w, http.StatusNotFound, "session_not_found", "Session not found")
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "get_session_failed", err.Error())
		return
	}
	if detail.History == nil {
		detail.History = []session.HistoryEntry{}
	}
	if detail.Runs == nil {
		detail.Runs = []session.RunSummary{}
	}
	writeAdminJSON(w, http.StatusOK, detail)
}

func (h *Handler) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	err := h.deps.Sessions.DeleteSession(r.Context(), sessionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeAdminError(w, http.StatusNotFound, "session_not_found", "Session not found")
	case errors.Is(err, session.ErrSessionBusy):
		writeAdminError(w, http.StatusConflict, "session_busy", "Cannot delete session while a run is active")
	case err != nil:
		writeAdminError(w, http.StatusInternalServerError, "delete_session_failed", err.Error())
	default:
		writeAdminJSON(w, http.StatusOK, map[string]any{"id": sessionID, "deleted": true})
	}
}

func (h *Handler) handleListFiles(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	files, err := h.deps.Files.ListAll(r.Context(), limit)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "list_files_failed", err.Error())
		return
	}
	type adminFileView struct {
		storage.FileRecord
		OwnerID    string `json:"owner_id"`
		ActiveRefs int    `json:"active_refs"`
		ExpiresAt  int64  `json:"expires_at"`
	}
	out := make([]adminFileView, 0, len(files))
	for _, f := range files {
		out = append(out, adminFileView{
			FileRecord: f,
			OwnerID:    f.OwnerID,
			ActiveRefs: f.ActiveRefs,
			ExpiresAt:  f.ExpiresAt,
		})
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.deps.Config.Storage.MaxFileBytes+2*1024*1024)
	mr, err := r.MultipartReader()
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_multipart", "Expected multipart/form-data")
		return
	}
	ownerID := strings.TrimSpace(r.URL.Query().Get("owner_id"))
	if ownerID == "" {
		ownerID = "admin"
	}
	var uploaded storage.FileRecord
	var hasFile bool
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_multipart", err.Error())
			return
		}
		if part.FormName() == "owner_id" {
			b, _ := io.ReadAll(io.LimitReader(part, 128))
			_ = part.Close()
			if s := strings.TrimSpace(string(b)); s != "" {
				ownerID = s
			}
			continue
		}
		if part.FormName() == "file" {
			rawName := part.FileName()
			if _, params, perr := mime.ParseMediaType(part.Header.Get("Content-Disposition")); perr == nil && params["filename"] != "" {
				rawName = params["filename"]
			}
			rec, saveErr := h.deps.Files.Save(r.Context(), ownerID, rawName, "user_data", part)
			_ = part.Close()
			if saveErr != nil {
				status := http.StatusBadRequest
				if errors.Is(saveErr, storage.ErrFileTooLarge) || errors.Is(saveErr, storage.ErrStorageQuota) {
					status = http.StatusRequestEntityTooLarge
				}
				writeAdminError(w, status, "upload_failed", saveErr.Error())
				return
			}
			uploaded = rec
			hasFile = true
			continue
		}
		_ = part.Close()
	}
	if !hasFile {
		writeAdminError(w, http.StatusBadRequest, "missing_file", "Multipart field 'file' is required")
		return
	}
	writeAdminJSON(w, http.StatusCreated, map[string]any{
		"id":         uploaded.ID,
		"owner_id":   ownerID,
		"filename":   uploaded.Filename,
		"bytes":      uploaded.Bytes,
		"created_at": uploaded.CreatedAt,
	})
}

func (h *Handler) handleFileContent(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("file_id")
	rec, f, err := h.deps.Files.OpenContentByID(r.Context(), fileID)
	if errors.Is(err, storage.ErrFileNotFound) {
		writeAdminError(w, http.StatusNotFound, "file_not_found", "File not found")
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "file_read_failed", err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", rec.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, io.LimitReader(f, 2*1024*1024))
}

func (h *Handler) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("file_id")
	err := h.deps.Files.DeleteByID(r.Context(), fileID)
	switch {
	case errors.Is(err, storage.ErrFileNotFound):
		writeAdminError(w, http.StatusNotFound, "file_not_found", "File not found")
	case errors.Is(err, storage.ErrFileInUse):
		writeAdminError(w, http.StatusConflict, "file_in_use", "File is referenced by an active run")
	case err != nil:
		writeAdminError(w, http.StatusInternalServerError, "delete_file_failed", err.Error())
	default:
		writeAdminJSON(w, http.StatusOK, map[string]any{"id": fileID, "deleted": true})
	}
}

func (h *Handler) handleCleanup(w http.ResponseWriter, r *http.Request) {
	fRemoved, fErr := h.deps.Files.CleanupExpired(r.Context())
	sRemoved, sErr := h.deps.Sessions.CleanupExpired(r.Context())
	if fErr != nil || sErr != nil {
		writeAdminError(w, http.StatusInternalServerError, "cleanup_failed", fmt.Sprintf("files=%v sessions=%v", fErr, sErr))
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"expired_files_removed":         fRemoved,
		"expired_sessions_runs_removed": sRemoved,
	})
}

func (h *Handler) handlePlayground(w http.ResponseWriter, r *http.Request) {
	if h.deps.Playground == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "playground_unavailable", "Playground runner is not configured")
		return
	}
	var in PlaygroundInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4*1024*1024)).Decode(&in); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON payload")
		return
	}
	if strings.TrimSpace(in.ModelID) == "" {
		writeAdminError(w, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}
	if strings.TrimSpace(in.Prompt) == "" && len(in.FileIDs) == 0 {
		writeAdminError(w, http.StatusBadRequest, "missing_prompt", "Either 'prompt' or 'file_ids' must be supplied")
		return
	}
	if strings.TrimSpace(in.OwnerID) == "" {
		in.OwnerID = "admin"
	}
	h.deps.Playground.ExecutePlayground(r.Context(), w, in)
}

func parseBearer(header string) string {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func isMutatingMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut || method == http.MethodDelete
}

func writeAdminJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	writeAdminJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}
