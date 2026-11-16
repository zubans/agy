package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/pools"
)

func (h *Handler) ensurePools() *pools.Store {
	if h.deps.Pools != nil {
		return h.deps.Pools
	}
	if h.deps.DB != nil {
		return pools.NewStore(h.deps.DB.SQL())
	}
	return nil
}

func (h *Handler) handleListProviders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := h.ensurePools()
	items := h.deps.Registry.ListAll()
	cooldowns, _ := store.ListCooldowns(ctx)

	cdByProvider := make(map[string][]pools.Cooldown)
	for _, c := range cooldowns {
		cdByProvider[c.ProviderID] = append(cdByProvider[c.ProviderID], c)
	}

	type providerView struct {
		adminModelView
		CachedModels    []pools.CachedProviderModel `json:"cached_models"`
		ActiveCooldowns []pools.Cooldown            `json:"active_cooldowns"`
	}

	out := make([]providerView, 0, len(items))
	for _, m := range items {
		st, _ := h.deps.Registry.CheckOne(ctx, m.Config.ID)
		cached, _ := store.ListProviderModels(ctx, m.Config.ID)
		if cached == nil {
			cached = []pools.CachedProviderModel{}
		}
		cds := cdByProvider[m.Config.ID]
		if cds == nil {
			cds = []pools.Cooldown{}
		}
		out = append(out, providerView{
			adminModelView:  toAdminModelView(m, st),
			CachedModels:    cached,
			ActiveCooldowns: cds,
		})
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) handleDiscoverProviderModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	providerID := r.PathValue("provider_id")
	if _, ok := h.deps.Registry.GetAny(providerID); !ok {
		writeAdminError(w, http.StatusNotFound, "provider_not_found", "Provider not found")
		return
	}

	store := h.ensurePools()
	refresh := r.URL.Query().Get("refresh") == "1" || strings.EqualFold(r.URL.Query().Get("refresh"), "true")
	cached, err := store.ListProviderModels(ctx, providerID)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "provider_models_error", err.Error())
		return
	}

	var source, warning string
	if refresh || len(cached) == 0 {
		discovered, src, discErr := h.deps.Registry.DiscoverModels(ctx, providerID)
		source = src
		if discErr != nil {
			warning = discErr.Error()
		}
		if len(discovered) > 0 {
			if saveErr := store.SaveProviderModels(ctx, providerID, src, discovered); saveErr == nil {
				cached, _ = store.ListProviderModels(ctx, providerID)
			}
		}
	} else {
		source = "cache"
	}
	if cached == nil {
		cached = []pools.CachedProviderModel{}
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"provider_id": providerID,
		"source":      source,
		"warning":     warning,
		"models":      cached,
	})
}

func (h *Handler) handleAddCustomProviderModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	providerID := r.PathValue("provider_id")
	if _, ok := h.deps.Registry.GetAny(providerID); !ok {
		writeAdminError(w, http.StatusNotFound, "provider_not_found", "Provider not found")
		return
	}
	var body struct {
		ModelName   string `json:"model_name"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	item, err := h.ensurePools().AddCustomProviderModel(ctx, providerID, body.ModelName, body.DisplayName)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_provider_model", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusCreated, item)
}

func (h *Handler) handlePublishProviderModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	providerID := r.PathValue("provider_id")
	if _, ok := h.deps.Registry.GetAny(providerID); !ok {
		writeAdminError(w, http.StatusNotFound, "provider_not_found", "Provider not found")
		return
	}
	var body struct {
		Items []struct {
			ID                     string `json:"id"`
			ProviderModel          string `json:"provider_model"`
			DisplayName            string `json:"display_name"`
			DefaultCooldownSeconds int    `json:"default_cooldown_seconds"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	if len(body.Items) == 0 {
		writeAdminError(w, http.StatusBadRequest, "missing_items", "At least one model item must be selected")
		return
	}

	store := h.ensurePools()
	published := make([]pools.PublishedModel, 0, len(body.Items))
	for _, item := range body.Items {
		pmName := strings.TrimSpace(item.ProviderModel)
		pubID := strings.TrimSpace(item.ID)
		if pubID == "" {
			if pmName != "" {
				pubID = sanitizeModelID(providerID + "-" + pmName)
			} else {
				pubID = providerID
			}
		}
		if err := pools.ValidatePublishedModelID(pubID); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_model_id", err.Error())
			return
		}
		if pmName != "" {
			_, _ = store.AddCustomProviderModel(ctx, providerID, pmName, item.DisplayName)
		}
		saved, err := store.UpsertPublishedModel(ctx, pools.PublishedModel{
			ID:                     pubID,
			DisplayName:            item.DisplayName,
			Kind:                   "single",
			Strategy:               pools.StrategyPriorityOrder,
			Enabled:                true,
			DefaultCooldownSeconds: item.DefaultCooldownSeconds,
			Targets: []pools.PublishedTarget{
				{
					ProviderID:    providerID,
					ProviderModel: pmName,
					Priority:      1,
					Position:      0,
					Enabled:       true,
				},
			},
		})
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "publish_model_failed", err.Error())
			return
		}
		published = append(published, saved)
	}
	writeAdminJSON(w, http.StatusCreated, map[string]any{"data": published})
}

type adminTargetView struct {
	pools.PublishedTarget
	BackendProvider  string `json:"backend_provider,omitempty"`
	BackendEnabled   bool   `json:"backend_enabled"`
	BackendReady     bool   `json:"backend_ready"`
	CoolingUntil     int64  `json:"cooling_until,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds,omitempty"`
	CooldownReason   string `json:"cooldown_reason,omitempty"`
	CooldownSource   string `json:"cooldown_source,omitempty"`
}

type adminCatalogModelView struct {
	ID                     string               `json:"id"`
	DisplayName            string               `json:"display_name"`
	Kind                   string               `json:"kind"`
	Strategy               pools.Strategy       `json:"strategy"`
	Enabled                bool                 `json:"enabled"`
	DefaultCooldownSeconds int                  `json:"default_cooldown_seconds"`
	Available              bool                 `json:"available"`
	ReadyTargets           int                  `json:"ready_targets"`
	CoolingTargets         int                  `json:"cooling_targets"`
	EarliestReset          int64                `json:"earliest_reset,omitempty"`
	Targets                []adminTargetView    `json:"targets"`
	CreatedAt              int64                `json:"created_at"`
	UpdatedAt              int64                `json:"updated_at"`
}

func (h *Handler) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := h.ensurePools()
	items, err := store.ListPublishedModels(ctx, false)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "list_catalog_failed", err.Error())
		return
	}
	cooldowns, _ := store.ActiveCooldownMap(ctx)
	statusCache := make(map[string]executor.ExecutorStatus)
	now := time.Now().Unix()

	views := make([]adminCatalogModelView, 0, len(items))
	for _, pm := range items {
		views = append(views, h.buildCatalogView(ctx, pm, cooldowns, statusCache, now))
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": views})
}

func (h *Handler) buildCatalogView(
	ctx context.Context,
	pm pools.PublishedModel,
	cooldowns map[pools.CooldownKey]pools.Cooldown,
	statusCache map[string]executor.ExecutorStatus,
	now int64,
) adminCatalogModelView {
	view := adminCatalogModelView{
		ID:                     pm.ID,
		DisplayName:            pm.DisplayName,
		Kind:                   pm.Kind,
		Strategy:               pm.Strategy,
		Enabled:                pm.Enabled,
		DefaultCooldownSeconds: pm.DefaultCooldownSeconds,
		Targets:                make([]adminTargetView, 0, len(pm.Targets)),
		CreatedAt:              pm.CreatedAt,
		UpdatedAt:              pm.UpdatedAt,
	}

	var earliestReset int64
	for _, t := range pm.Targets {
		tv := adminTargetView{PublishedTarget: t}
		if backend, ok := h.deps.Registry.GetAny(t.ProviderID); ok {
			tv.BackendProvider = backend.Config.Provider
			tv.BackendEnabled = backend.Config.EffectiveEnabled()
			st, cached := statusCache[t.ProviderID]
			if !cached {
				st, _ = h.deps.Registry.CheckOne(ctx, t.ProviderID)
				statusCache[t.ProviderID] = st
			}
			tv.BackendReady = st.Ready
			effModel := strings.TrimSpace(t.ProviderModel)
			if effModel == "" {
				effModel = strings.TrimSpace(backend.Config.Model)
			}
			if cd, isCooling := cooldowns[pools.CooldownKey{ProviderID: t.ProviderID, ProviderModel: effModel}]; isCooling && cd.CoolingUntil > now {
				tv.CoolingUntil = cd.CoolingUntil
				tv.RemainingSeconds = cd.CoolingUntil - now
				tv.CooldownReason = cd.Reason
				tv.CooldownSource = cd.Source
				if t.Enabled && tv.BackendEnabled {
					view.CoolingTargets++
					if earliestReset == 0 || cd.CoolingUntil < earliestReset {
						earliestReset = cd.CoolingUntil
					}
				}
			} else if t.Enabled && tv.BackendEnabled {
				view.ReadyTargets++
			}
		}
		view.Targets = append(view.Targets, tv)
	}
	view.Available = pm.Enabled && view.ReadyTargets > 0
	if !view.Available && earliestReset > now {
		view.EarliestReset = earliestReset
	}
	return view
}

type catalogPayload struct {
	ID                     string                  `json:"id"`
	DisplayName            string                  `json:"display_name"`
	Kind                   string                  `json:"kind"`
	Strategy               pools.Strategy          `json:"strategy"`
	Enabled                *bool                   `json:"enabled"`
	DefaultCooldownSeconds int                     `json:"default_cooldown_seconds"`
	Targets                []pools.PublishedTarget `json:"targets"`
}

func (h *Handler) handleCreateCatalogModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body catalogPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	store := h.ensurePools()
	if _, err := store.GetPublishedModel(ctx, body.ID); err == nil {
		writeAdminError(w, http.StatusConflict, "model_already_exists", fmt.Sprintf("Model %q already exists", body.ID))
		return
	}
	h.saveCatalogPayload(ctx, w, http.StatusCreated, body)
}

func (h *Handler) handleUpdateCatalogModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID := r.PathValue("model_id")
	var body catalogPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	body.ID = modelID
	h.saveCatalogPayload(ctx, w, http.StatusOK, body)
}

func (h *Handler) saveCatalogPayload(ctx context.Context, w http.ResponseWriter, status int, body catalogPayload) {
	for i := range body.Targets {
		t := &body.Targets[i]
		if _, ok := h.deps.Registry.GetAny(strings.TrimSpace(t.ProviderID)); !ok {
			writeAdminError(w, http.StatusBadRequest, "unknown_provider", fmt.Sprintf("Provider %q does not exist", t.ProviderID))
			return
		}
	}
	en := true
	if body.Enabled != nil {
		en = *body.Enabled
	}
	saved, err := h.ensurePools().UpsertPublishedModel(ctx, pools.PublishedModel{
		ID:                     body.ID,
		DisplayName:            body.DisplayName,
		Kind:                   body.Kind,
		Strategy:               body.Strategy,
		Enabled:                en,
		DefaultCooldownSeconds: body.DefaultCooldownSeconds,
		Targets:                body.Targets,
	})
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "catalog_save_failed", err.Error())
		return
	}
	cooldowns, _ := h.ensurePools().ActiveCooldownMap(ctx)
	view := h.buildCatalogView(ctx, saved, cooldowns, make(map[string]executor.ExecutorStatus), time.Now().Unix())
	writeAdminJSON(w, status, view)
}

func (h *Handler) handlePatchCatalogModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID := r.PathValue("model_id")
	store := h.ensurePools()
	existing, err := store.GetPublishedModel(ctx, modelID)
	if errors.Is(err, pools.ErrModelNotFound) {
		writeAdminError(w, http.StatusNotFound, "model_not_found", "Published model not found")
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "catalog_lookup_failed", err.Error())
		return
	}
	var body struct {
		Enabled                *bool           `json:"enabled"`
		DisplayName            *string         `json:"display_name"`
		Strategy               *pools.Strategy `json:"strategy"`
		DefaultCooldownSeconds *int            `json:"default_cooldown_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
	}
	if body.DisplayName != nil {
		existing.DisplayName = *body.DisplayName
	}
	if body.Strategy != nil {
		existing.Strategy = *body.Strategy
	}
	if body.DefaultCooldownSeconds != nil {
		existing.DefaultCooldownSeconds = *body.DefaultCooldownSeconds
	}
	saved, err := store.UpsertPublishedModel(ctx, existing)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "catalog_update_failed", err.Error())
		return
	}
	cooldowns, _ := store.ActiveCooldownMap(ctx)
	view := h.buildCatalogView(ctx, saved, cooldowns, make(map[string]executor.ExecutorStatus), time.Now().Unix())
	writeAdminJSON(w, http.StatusOK, view)
}

func (h *Handler) handleDeleteCatalogModel(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("model_id")
	err := h.ensurePools().DeletePublishedModel(r.Context(), modelID)
	if errors.Is(err, pools.ErrModelNotFound) {
		writeAdminError(w, http.StatusNotFound, "model_not_found", "Published model not found")
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "catalog_delete_failed", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"id": modelID, "deleted": true})
}

func (h *Handler) handleListCooldowns(w http.ResponseWriter, r *http.Request) {
	cds, err := h.ensurePools().ListCooldowns(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "list_cooldowns_failed", err.Error())
		return
	}
	if cds == nil {
		cds = []pools.Cooldown{}
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"data": cds})
}

func (h *Handler) handleReleaseCooldown(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID    string `json:"provider_id"`
		ProviderModel string `json:"provider_model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8*1024)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	body.ProviderID = strings.TrimSpace(body.ProviderID)
	body.ProviderModel = strings.TrimSpace(body.ProviderModel)
	if body.ProviderID == "" {
		writeAdminError(w, http.StatusBadRequest, "missing_provider_id", "provider_id is required")
		return
	}
	if err := h.ensurePools().ReleaseCooldown(r.Context(), body.ProviderID, body.ProviderModel); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "release_cooldown_failed", err.Error())
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"provider_id":    body.ProviderID,
		"provider_model": body.ProviderModel,
		"released":       true,
	})
}

func sanitizeModelID(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':':
			b.WriteRune(r)
		case r == '/' || r == ' ' || r == '\t':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}
