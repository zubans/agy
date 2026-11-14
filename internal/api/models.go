package api

import (
	"net/http"
	"time"

	"cli-ai-gateway/internal/executor"
)

// ModelsHandler serves GET /v1/models and GET /v1/executors (Section 6.1).
type ModelsHandler struct {
	registry *executor.Registry
	started  int64
}

func NewModelsHandler(registry *executor.Registry) *ModelsHandler {
	return &ModelsHandler{
		registry: registry,
		started:  time.Now().Unix(),
	}
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (h *ModelsHandler) List(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())

	items := h.registry.List()
	data := make([]openAIModel, 0, len(items))
	for _, m := range items {
		if !principal.AllowsModel(m.Config.ID) {
			continue
		}
		data = append(data, openAIModel{
			ID:      m.Config.ID,
			Object:  "model",
			Created: h.started,
			OwnedBy: "cli-gateway",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// Capabilities returns diagnostic details about each allowed model's executor and readiness (Section 6.1, 10).
func (h *ModelsHandler) Capabilities(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	report := h.registry.CheckAll(r.Context())

	type modelCapability struct {
		ID           string                `json:"id"`
		Provider     string                `json:"provider"`
		Ready        bool                  `json:"ready"`
		Error        string                `json:"error,omitempty"`
		Capabilities executor.Capabilities `json:"capabilities"`
	}

	items := h.registry.List()
	out := make([]modelCapability, 0, len(items))
	for _, m := range items {
		if !principal.AllowsModel(m.Config.ID) {
			continue
		}
		st := report.Executors[m.Config.ID]
		out = append(out, modelCapability{
			ID:           m.Config.ID,
			Provider:     m.Config.Provider,
			Ready:        st.Ready,
			Error:        st.Error,
			Capabilities: m.Capabilities,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   out,
	})
}
