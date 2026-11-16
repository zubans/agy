package api

import (
	"net/http"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/pools"
)

// ModelsHandler serves GET /v1/models and GET /v1/executors (Section 6.1).
type ModelsHandler struct {
	registry *executor.Registry
	pools    *pools.Store
	started  int64
}

func NewModelsHandler(registry *executor.Registry, poolStore *pools.Store) *ModelsHandler {
	return &ModelsHandler{
		registry: registry,
		pools:    poolStore,
		started:  time.Now().Unix(),
	}
}

type openAIModel struct {
	ID            string   `json:"id"`
	Object        string   `json:"object"`
	Created       int64    `json:"created"`
	OwnedBy       string   `json:"owned_by"`
	Kind          string   `json:"kind,omitempty"`
	Strategy      string   `json:"strategy,omitempty"`
	Available     *bool    `json:"available,omitempty"`
	Providers     []string `json:"providers,omitempty"`
	CooldownUntil int64    `json:"cooldown_until,omitempty"`
}

func (h *ModelsHandler) List(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())

	data := make([]openAIModel, 0)
	seen := make(map[string]struct{})
	now := time.Now().Unix()

	var cooldowns map[pools.CooldownKey]pools.Cooldown
	if h.pools != nil {
		cooldowns, _ = h.pools.ActiveCooldownMap(r.Context())
		pubModels, err := h.pools.ListPublishedModels(r.Context(), false)
		if err == nil {
			for _, pm := range pubModels {
				seen[pm.ID] = struct{}{}
				if !pm.Enabled || !principal.AllowsModel(pm.ID) {
					continue
				}
				seen[pm.ID] = struct{}{}
				sel := h.pools.InspectAvailability(h.registry, pm, cooldowns, now)
				avail := len(sel.Candidates) > 0
				providers := make([]string, 0, len(pm.Targets))
				owner := "cli-gateway"
				for _, t := range pm.Targets {
					if !t.Enabled {
						continue
					}
					label := t.ProviderID
					if t.ProviderModel != "" {
						label += ":" + t.ProviderModel
					}
					providers = append(providers, label)
				}
				entry := openAIModel{
					ID:        pm.ID,
					Object:    "model",
					Created:   pm.CreatedAt,
					OwnedBy:   owner,
					Kind:      pm.Kind,
					Strategy:  string(pm.Strategy),
					Available: &avail,
					Providers: providers,
				}
				if !avail && sel.EarliestReset > now {
					entry.CooldownUntil = sel.EarliestReset
				}
				data = append(data, entry)
			}
		}
	}

	// Preserve legacy/standalone explicitly public entries not shadowed by a published model.
	for _, m := range h.registry.List() {
		if _, dup := seen[m.Config.ID]; dup {
			continue
		}
		if !principal.AllowsModel(m.Config.ID) {
			continue
		}
		avail := true
		var coolUntil int64
		if cooldowns != nil {
			if cd, ok := cooldowns[pools.CooldownKey{ProviderID: m.Config.ID, ProviderModel: m.Config.Model}]; ok && cd.CoolingUntil > now {
				avail = false
				coolUntil = cd.CoolingUntil
			}
		}
		data = append(data, openAIModel{
			ID:            m.Config.ID,
			Object:        "model",
			Created:       h.started,
			OwnedBy:       "cli-gateway",
			Kind:          "single",
			Available:     &avail,
			CooldownUntil: coolUntil,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// Capabilities returns diagnostic details about each published/public model's executor readiness (Section 6.1, 10).
func (h *ModelsHandler) Capabilities(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	report := h.registry.CheckAll(r.Context())

	type modelCapability struct {
		ID           string                `json:"id"`
		Provider     string                `json:"provider"`
		Kind         string                `json:"kind,omitempty"`
		Ready        bool                  `json:"ready"`
		Error        string                `json:"error,omitempty"`
		Capabilities executor.Capabilities `json:"capabilities"`
	}

	out := make([]modelCapability, 0)
	seen := make(map[string]struct{})
	now := time.Now().Unix()

	if h.pools != nil {
		cooldowns, _ := h.pools.ActiveCooldownMap(r.Context())
		if pubModels, err := h.pools.ListPublishedModels(r.Context(), false); err == nil {
			for _, pm := range pubModels {
				seen[pm.ID] = struct{}{}
				if !pm.Enabled || !principal.AllowsModel(pm.ID) {
					continue
				}
				seen[pm.ID] = struct{}{}
				sel := h.pools.InspectAvailability(h.registry, pm, cooldowns, now)
				var caps executor.Capabilities
				ready := false
				provName := pm.Kind
				for _, c := range sel.Candidates {
					if st, ok := report.Executors[c.ProviderID]; ok && st.Ready {
						ready = true
						caps = c.Info.Capabilities
						provName = c.Info.Config.Provider
						break
					}
				}
				errMsg := ""
				if !ready {
					if sel.EarliestReset > now {
						errMsg = "all provider targets are cooling down"
					} else {
						errMsg = "no ready provider targets available"
					}
				}
				out = append(out, modelCapability{
					ID:           pm.ID,
					Provider:     provName,
					Kind:         pm.Kind,
					Ready:        ready,
					Error:        errMsg,
					Capabilities: caps,
				})
			}
		}
	}

	for _, m := range h.registry.List() {
		if _, dup := seen[m.Config.ID]; dup {
			continue
		}
		if !principal.AllowsModel(m.Config.ID) {
			continue
		}
		st := report.Executors[m.Config.ID]
		out = append(out, modelCapability{
			ID:           m.Config.ID,
			Provider:     m.Config.Provider,
			Kind:         "single",
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
