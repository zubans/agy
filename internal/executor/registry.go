package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cli-ai-gateway/internal/config"
)

var (
	ErrModelNotFound      = errors.New("model not found")
	ErrModelAlreadyExists = errors.New("model already exists")
	ErrInvalidModelConfig = errors.New("invalid model configuration")
)

// Factory creates a configured Executor for a model entry.
type Factory func(model config.ModelConfig) (Executor, error)

// ModelInfo combines configured model details with executor capabilities and health.
type ModelInfo struct {
	Config       config.ModelConfig
	Executor     Executor
	Capabilities Capabilities
	Dynamic      bool
}

// Registry stores model-to-executor mappings and provider factories without coupling to HTTP handlers.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	models    map[string]ModelInfo
	order     []string
}

// NewRegistry creates an empty executor registry.
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]Factory),
		models:    make(map[string]ModelInfo),
	}
}

// RegisterFactory registers a provider factory (e.g., "codex", "antigravity", "gemini", "claude", "ollama").
func (r *Registry) RegisterFactory(provider string, factory Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[provider] = factory
}

// RegisterModels builds and binds Executor instances for all configured models.
func (r *Registry) RegisterModels(models []config.ModelConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, m := range models {
		factory, ok := r.factories[m.Provider]
		if !ok {
			return fmt.Errorf("no executor factory registered for provider %q (model %q)", m.Provider, m.ID)
		}
		exec, err := factory(m)
		if err != nil {
			return fmt.Errorf("initialize executor for model %q (%s): %w", m.ID, m.Provider, err)
		}
		if _, exists := r.models[m.ID]; !exists {
			r.order = append(r.order, m.ID)
		}
		r.models[m.ID] = ModelInfo{
			Config:       m,
			Executor:     exec,
			Capabilities: exec.Capabilities(),
			Dynamic:      false,
		}
	}
	return nil
}

// Get returns the enabled and publicly exposed standalone ModelInfo by model ID.
func (r *Registry) Get(modelID string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.models[modelID]
	if !ok || !info.Config.EffectiveEnabled() || !info.Config.EffectivePublic() {
		return ModelInfo{}, false
	}
	return info, true
}

// GetProvider returns any enabled backend provider/worker by ID regardless of public catalog visibility.
func (r *Registry) GetProvider(providerID string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.models[providerID]
	if !ok || !info.Config.EffectiveEnabled() {
		return ModelInfo{}, false
	}
	return info, true
}

// GetAny returns any registered backend provider (enabled or disabled) by ID.
func (r *Registry) GetAny(providerID string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.models[providerID]
	return info, ok
}

// List returns all enabled, publicly visible standalone models in deterministic registration order.
func (r *Registry) List() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelInfo, 0, len(r.order))
	for _, id := range r.order {
		if info, ok := r.models[id]; ok && info.Config.EffectiveEnabled() && info.Config.EffectivePublic() {
			out = append(out, info)
		}
	}
	return out
}

// ListProviders returns all enabled backend providers/workers (including hidden ones with public=false).
func (r *Registry) ListProviders() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelInfo, 0, len(r.order))
	for _, id := range r.order {
		if info, ok := r.models[id]; ok && info.Config.EffectiveEnabled() {
			out = append(out, info)
		}
	}
	return out
}

// DiscoverModels queries a configured backend provider for its available selectable sub-models.
func (r *Registry) DiscoverModels(ctx context.Context, providerID string) ([]ModelDescriptor, string, error) {
	r.mu.RLock()
	info, ok := r.models[providerID]
	r.mu.RUnlock()
	if !ok {
		return nil, "", ErrModelNotFound
	}
	if lister, ok := info.Executor.(ModelLister); ok {
		return lister.ListModels(ctx)
	}
	if strings.TrimSpace(info.Config.Model) != "" {
		return []ModelDescriptor{{ID: info.Config.Model, Name: info.Config.Model, Default: true}}, "configured", nil
	}
	return nil, "unsupported", nil
}

// ListAll returns all registered models (including disabled ones) in deterministic order.
func (r *Registry) ListAll() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelInfo, 0, len(r.order))
	for _, id := range r.order {
		if info, ok := r.models[id]; ok {
			out = append(out, info)
		}
	}
	return out
}

// CheckOne probes a single registered model (enabled or disabled) by ID.
func (r *Registry) CheckOne(ctx context.Context, modelID string) (ExecutorStatus, bool) {
	r.mu.RLock()
	info, ok := r.models[modelID]
	r.mu.RUnlock()
	if !ok {
		return ExecutorStatus{}, false
	}
	err := info.Executor.Check(ctx)
	st := ExecutorStatus{
		Provider:     info.Config.Provider,
		Ready:        err == nil,
		Capabilities: info.Capabilities,
	}
	if err != nil {
		st.Error = err.Error()
	}
	return st, true
}

// ModelPatch describes safe runtime changes permitted via Web Admin without mutating local CLI binary paths.
type ModelPatch struct {
	Enabled        *bool
	Public         *bool
	TimeoutSeconds *int
	MaxConcurrent  *int
	Model          *string
	BaseURL        *string
	WorkerURL      *string
}

// ApplyOverrides restores persisted runtime overrides and dynamically added models from SQLite.
func (r *Registry) ApplyOverrides(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, provider, base_url, worker_url, model, enabled, public, timeout_seconds, max_concurrent, dynamic
		FROM model_overrides
		WHERE deleted = 0
		ORDER BY updated_at ASC
	`)
	if err != nil {
		return fmt.Errorf("load model overrides: %w", err)
	}
	defer rows.Close()

	type rowData struct {
		id, provider, baseURL, workerURL, model string
		enabled, public, timeoutSec, maxConcur  sql.NullInt64
		dynamic                                 int
	}
	var loaded []rowData
	for rows.Next() {
		var it rowData
		if err := rows.Scan(&it.id, &it.provider, &it.baseURL, &it.workerURL, &it.model, &it.enabled, &it.public, &it.timeoutSec, &it.maxConcur, &it.dynamic); err != nil {
			return err
		}
		loaded = append(loaded, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, it := range loaded {
		existing, exists := r.models[it.id]
		if !exists {
			if it.dynamic == 0 {
				continue
			}
			en := true
			if it.enabled.Valid {
				en = it.enabled.Int64 != 0
			}
			var pubPtr *bool
			if it.public.Valid {
				pub := it.public.Int64 != 0
				pubPtr = &pub
			}
			timeout := 600
			if it.timeoutSec.Valid && it.timeoutSec.Int64 > 0 {
				timeout = int(it.timeoutSec.Int64)
			}
			maxC := 4
			if it.maxConcur.Valid && it.maxConcur.Int64 > 0 {
				maxC = int(it.maxConcur.Int64)
			}
			var cfg config.ModelConfig
			switch it.provider {
			case "ollama":
				if strings.TrimSpace(it.baseURL) == "" || strings.TrimSpace(it.model) == "" {
					continue
				}
				cfg = config.ModelConfig{
					ID:             it.id,
					Provider:       "ollama",
					BaseURL:        it.baseURL,
					Model:          it.model,
					TimeoutSeconds: timeout,
					MaxConcurrent:  maxC,
					FSAccess:       "workspace-read",
					Enabled:        &en,
					Public:         pubPtr,
				}
			case "codex", "antigravity", "gemini", "claude":
				if strings.TrimSpace(it.workerURL) == "" {
					continue
				}
				cfg = config.ModelConfig{
					ID:             it.id,
					Provider:       it.provider,
					WorkerURL:      it.workerURL,
					WorkerToken:    strings.TrimSpace(os.Getenv("GATEWAY_WORKER_TOKEN")),
					Model:          it.model,
					TimeoutSeconds: timeout,
					MaxConcurrent:  maxC,
					FSAccess:       "workspace-read",
					Enabled:        &en,
					Public:         pubPtr,
				}
			default:
				continue
			}
			if err := r.registerSingleLocked(cfg, true); err != nil {
				continue
			}
			continue
		}

		cfg := existing.Config
		if it.enabled.Valid {
			en := it.enabled.Int64 != 0
			cfg.Enabled = &en
		}
		if it.public.Valid {
			pub := it.public.Int64 != 0
			cfg.Public = &pub
		}
		if it.timeoutSec.Valid && it.timeoutSec.Int64 > 0 {
			cfg.TimeoutSeconds = int(it.timeoutSec.Int64)
		}
		if it.maxConcur.Valid && it.maxConcur.Int64 > 0 {
			cfg.MaxConcurrent = int(it.maxConcur.Int64)
		}
		if it.model != "" {
			cfg.Model = it.model
		}
		if it.baseURL != "" && cfg.Provider == "ollama" {
			cfg.BaseURL = it.baseURL
		}
		if it.workerURL != "" && cfg.WorkerURL != "" {
			cfg.WorkerURL = it.workerURL
		}
		_ = r.registerSingleLocked(cfg, existing.Dynamic)
	}
	return nil
}

// UpdateRuntimeModel applies a safe runtime patch to an existing model and persists it to SQLite.
func (r *Registry) UpdateRuntimeModel(ctx context.Context, db *sql.DB, id string, patch ModelPatch) (ModelInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.models[id]
	if !ok {
		return ModelInfo{}, ErrModelNotFound
	}
	cfg := existing.Config
	if patch.Enabled != nil {
		en := *patch.Enabled
		cfg.Enabled = &en
	}
	if patch.Public != nil {
		pub := *patch.Public
		cfg.Public = &pub
	}
	if patch.TimeoutSeconds != nil {
		if *patch.TimeoutSeconds < 1 || *patch.TimeoutSeconds > 86400 {
			return ModelInfo{}, fmt.Errorf("%w: timeout_seconds must be in [1, 86400]", ErrInvalidModelConfig)
		}
		cfg.TimeoutSeconds = *patch.TimeoutSeconds
	}
	if patch.MaxConcurrent != nil {
		if *patch.MaxConcurrent < 1 || *patch.MaxConcurrent > 256 {
			return ModelInfo{}, fmt.Errorf("%w: max_concurrent must be in [1, 256]", ErrInvalidModelConfig)
		}
		cfg.MaxConcurrent = *patch.MaxConcurrent
	}
	if patch.Model != nil {
		trimmed := strings.TrimSpace(*patch.Model)
		if cfg.Provider == "ollama" && trimmed == "" {
			return ModelInfo{}, fmt.Errorf("%w: model name cannot be empty for ollama", ErrInvalidModelConfig)
		}
		cfg.Model = trimmed
	}
	if patch.BaseURL != nil {
		if cfg.Provider != "ollama" {
			return ModelInfo{}, fmt.Errorf("%w: base_url can only be changed for ollama models", ErrInvalidModelConfig)
		}
		if err := validateHTTPURL("base_url", *patch.BaseURL); err != nil {
			return ModelInfo{}, err
		}
		cfg.BaseURL = strings.TrimSpace(*patch.BaseURL)
	}
	if patch.WorkerURL != nil {
		if strings.TrimSpace(cfg.WorkerURL) == "" {
			return ModelInfo{}, fmt.Errorf("%w: worker_url can only be changed for remote worker models", ErrInvalidModelConfig)
		}
		if err := validateHTTPURL("worker_url", *patch.WorkerURL); err != nil {
			return ModelInfo{}, err
		}
		cfg.WorkerURL = strings.TrimSpace(*patch.WorkerURL)
	}

	if err := r.registerSingleLocked(cfg, existing.Dynamic); err != nil {
		return ModelInfo{}, err
	}
	if err := persistModelOverride(ctx, db, cfg, existing.Dynamic, false); err != nil {
		return ModelInfo{}, err
	}
	return r.models[id], nil
}

// AddDynamicOllamaModel validates and registers a dynamic Ollama model (compatibility wrapper around AddDynamicModel).
func (r *Registry) AddDynamicOllamaModel(ctx context.Context, db *sql.DB, cfg config.ModelConfig) (ModelInfo, error) {
	cfg.Provider = "ollama"
	return r.AddDynamicModel(ctx, db, cfg)
}

// AddDynamicModel validates and registers a dynamic Ollama model or remote worker CLI model at runtime.
func (r *Registry) AddDynamicModel(ctx context.Context, db *sql.DB, cfg config.ModelConfig) (ModelInfo, error) {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.Provider = strings.TrimSpace(cfg.Provider)
	if cfg.Provider == "" {
		cfg.Provider = "ollama"
	}
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.WorkerURL = strings.TrimSpace(cfg.WorkerURL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Executable = ""
	cfg.HomeDir = ""

	if cfg.ID == "" || strings.ContainsAny(cfg.ID, "/\\ \t\n") {
		return ModelInfo{}, fmt.Errorf("%w: invalid model id", ErrInvalidModelConfig)
	}

	switch cfg.Provider {
	case "ollama":
		cfg.WorkerURL = ""
		if err := validateHTTPURL("base_url", cfg.BaseURL); err != nil {
			return ModelInfo{}, err
		}
		if cfg.Model == "" {
			return ModelInfo{}, fmt.Errorf("%w: ollama model name is required", ErrInvalidModelConfig)
		}
	case "codex", "antigravity", "gemini", "claude":
		cfg.BaseURL = ""
		if err := validateHTTPURL("worker_url", cfg.WorkerURL); err != nil {
			return ModelInfo{}, err
		}
		if strings.TrimSpace(cfg.WorkerToken) == "" {
			cfg.WorkerToken = strings.TrimSpace(os.Getenv("GATEWAY_WORKER_TOKEN"))
		}
	default:
		return ModelInfo{}, fmt.Errorf("%w: dynamic registration only supports ollama or remote CLI worker providers", ErrInvalidModelConfig)
	}

	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 600
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.FSAccess == "" {
		cfg.FSAccess = "workspace-read"
	}
	if cfg.Enabled == nil {
		en := true
		cfg.Enabled = &en
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.models[cfg.ID]; exists {
		return ModelInfo{}, ErrModelAlreadyExists
	}
	if err := r.registerSingleLocked(cfg, true); err != nil {
		return ModelInfo{}, err
	}
	if err := persistModelOverride(ctx, db, cfg, true, false); err != nil {
		return ModelInfo{}, err
	}
	return r.models[cfg.ID], nil
}

// DeleteDynamicModel deletes a Web Admin dynamic Ollama model. Static YAML models cannot be deleted via UI.
func (r *Registry) DeleteDynamicModel(ctx context.Context, db *sql.DB, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.models[id]
	if !ok {
		return ErrModelNotFound
	}
	if !existing.Dynamic {
		return fmt.Errorf("%w: statically configured models can only be disabled, not deleted", ErrInvalidModelConfig)
	}

	delete(r.models, id)
	filtered := r.order[:0]
	for _, itemID := range r.order {
		if itemID != id {
			filtered = append(filtered, itemID)
		}
	}
	r.order = filtered

	if db != nil {
		_, err := db.ExecContext(ctx, `UPDATE model_overrides SET deleted = 1, updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
		return err
	}
	return nil
}

func (r *Registry) registerSingleLocked(cfg config.ModelConfig, dynamic bool) error {
	factory, ok := r.factories[cfg.Provider]
	if !ok {
		return fmt.Errorf("no executor factory registered for provider %q", cfg.Provider)
	}
	exec, err := factory(cfg)
	if err != nil {
		return err
	}
	if _, exists := r.models[cfg.ID]; !exists {
		r.order = append(r.order, cfg.ID)
	}
	r.models[cfg.ID] = ModelInfo{
		Config:       cfg,
		Executor:     exec,
		Capabilities: exec.Capabilities(),
		Dynamic:      dynamic,
	}
	return nil
}

func validateHTTPURL(field, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: invalid http/https %s %q", ErrInvalidModelConfig, field, raw)
	}
	return nil
}

func persistModelOverride(ctx context.Context, db *sql.DB, cfg config.ModelConfig, dynamic, deleted bool) error {
	if db == nil {
		return nil
	}
	enInt := 0
	if cfg.EffectiveEnabled() {
		enInt = 1
	}
	dynInt := 0
	if dynamic {
		dynInt = 1
	}
	delInt := 0
	if deleted {
		delInt = 1
	}
	pubInt := 0
	if cfg.EffectivePublic() {
		pubInt = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO model_overrides (id, provider, base_url, worker_url, model, enabled, public, timeout_seconds, max_concurrent, dynamic, deleted, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider = excluded.provider,
			base_url = excluded.base_url,
			worker_url = excluded.worker_url,
			model = excluded.model,
			enabled = excluded.enabled,
			public = excluded.public,
			timeout_seconds = excluded.timeout_seconds,
			max_concurrent = excluded.max_concurrent,
			dynamic = excluded.dynamic,
			deleted = excluded.deleted,
			updated_at = excluded.updated_at
	`, cfg.ID, cfg.Provider, cfg.BaseURL, cfg.WorkerURL, cfg.Model, enInt, pubInt, cfg.TimeoutSeconds, cfg.MaxConcurrent, dynInt, delInt, time.Now().Unix())
	return err
}

// ReadinessReport holds non-fatal per-executor readiness status (Section 6.9).
type ReadinessReport struct {
	ReadyCount int                       `json:"ready_count"`
	TotalCount int                       `json:"total_count"`
	Executors  map[string]ExecutorStatus `json:"executors"`
}

// ExecutorStatus describes the health of a single configured model executor.
type ExecutorStatus struct {
	Provider     string       `json:"provider"`
	Ready        bool         `json:"ready"`
	Error        string       `json:"error,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

// CheckAll probes all registered enabled backend executors independently so one broken CLI does not block others.
func (r *Registry) CheckAll(ctx context.Context) ReadinessReport {
	items := r.ListProviders()
	report := ReadinessReport{
		TotalCount: len(items),
		Executors:  make(map[string]ExecutorStatus, len(items)),
	}
	type itemRes struct {
		id     string
		status ExecutorStatus
	}
	ch := make(chan itemRes, len(items))
	for _, item := range items {
		go func(m ModelInfo) {
			err := m.Executor.Check(ctx)
			st := ExecutorStatus{
				Provider:     m.Config.Provider,
				Ready:        err == nil,
				Capabilities: m.Capabilities,
			}
			if err != nil {
				st.Error = err.Error()
			}
			ch <- itemRes{id: m.Config.ID, status: st}
		}(item)
	}
	results := make([]itemRes, 0, len(items))
	for range items {
		results = append(results, <-ch)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].id < results[j].id })
	for _, res := range results {
		if res.status.Ready {
			report.ReadyCount++
		}
		report.Executors[res.id] = res.status
	}
	return report
}
