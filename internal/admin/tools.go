package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"cli-ai-gateway/internal/tools"
)

func (h *Handler) handleListTools(w http.ResponseWriter, r *http.Request) {
	if h.deps.Tools == nil {
		writeAdminJSON(w, http.StatusOK, map[string]any{
			"tools":    []any{},
			"backends": []any{},
		})
		return
	}
	toolList := h.deps.Tools.ListInfo(r.Context())
	var backends []tools.BackendConfig
	if h.deps.Tools.Store() != nil {
		if list, err := h.deps.Tools.Store().ListBackends(r.Context()); err == nil {
			for _, b := range list {
				b.APIKey = "" // never expose raw secret to Web Admin
				backends = append(backends, b)
			}
		}
	}
	if backends == nil {
		backends = []tools.BackendConfig{}
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"tools":    toolList,
		"backends": backends,
	})
}

func (h *Handler) handleProbeTools(w http.ResponseWriter, r *http.Request) {
	if h.deps.Tools == nil {
		writeAdminJSON(w, http.StatusOK, map[string]any{"probes": []any{}})
		return
	}
	probes := h.deps.Tools.ProbeAll(r.Context())
	writeAdminJSON(w, http.StatusOK, map[string]any{"probes": probes})
}

func (h *Handler) handleUpsertToolBackend(w http.ResponseWriter, r *http.Request) {
	if h.deps.Tools == nil || h.deps.Tools.Store() == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "tools_unavailable", "tool store unavailable")
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "backend key is required")
		return
	}
	var req struct {
		Name    string            `json:"name"`
		BaseURL string            `json:"base_url"`
		APIKey  string            `json:"api_key"`
		Model   string            `json:"model"`
		Enabled bool              `json:"enabled"`
		Extra   map[string]string `json:"extra"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "invalid JSON payload")
		return
	}
	saved, err := h.deps.Tools.Store().UpsertBackend(r.Context(), tools.BackendConfig{
		Key:     key,
		Name:    req.Name,
		BaseURL: req.BaseURL,
		APIKey:  req.APIKey,
		Model:   req.Model,
		Enabled: req.Enabled,
		Extra:   req.Extra,
	})
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "backend_save_error", err.Error())
		return
	}
	saved.APIKey = ""
	writeAdminJSON(w, http.StatusOK, saved)
}

func (h *Handler) handleGetCatalogModelTools(w http.ResponseWriter, r *http.Request) {
	if h.deps.Tools == nil || h.deps.Tools.Store() == nil {
		writeAdminJSON(w, http.StatusOK, map[string]any{
			"tools":        []string{},
			"capabilities": tools.ModelCapabilities{},
		})
		return
	}
	modelID := strings.TrimSpace(r.PathValue("model_id"))
	bound, err := h.deps.Tools.Store().GetModelTools(r.Context(), modelID)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if bound == nil {
		bound = []string{}
	}
	caps, err := h.deps.Tools.Store().GetModelCapabilities(r.Context(), modelID)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"model_id":     modelID,
		"tools":        bound,
		"capabilities": caps,
	})
}

func (h *Handler) handleSetCatalogModelTools(w http.ResponseWriter, r *http.Request) {
	if h.deps.Tools == nil || h.deps.Tools.Store() == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "tools_unavailable", "tool store unavailable")
		return
	}
	modelID := strings.TrimSpace(r.PathValue("model_id"))
	if modelID == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "model_id is required")
		return
	}
	var req struct {
		Tools        []string                 `json:"tools"`
		Capabilities *tools.ModelCapabilities `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "invalid JSON payload")
		return
	}
	if req.Tools != nil {
		if err := h.deps.Tools.Store().SetModelTools(r.Context(), modelID, req.Tools); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
	}
	if req.Capabilities != nil {
		if err := h.deps.Tools.Store().SetModelCapabilities(r.Context(), modelID, *req.Capabilities); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
	}
	h.handleGetCatalogModelTools(w, r)
}
