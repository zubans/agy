package pools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cli-ai-gateway/internal/executor"
)

var (
	ErrModelNotFound      = errors.New("published model not found")
	ErrInvalidPoolConfig  = errors.New("invalid published model configuration")
	ErrDuplicateTarget    = errors.New("duplicate (provider_id, provider_model) target in published model")
)

type Strategy string

const (
	StrategyPriorityOrder Strategy = "priority_order"
	StrategyRoundRobin    Strategy = "round_robin"
)

type PublishedTarget struct {
	ID            int64  `json:"id,omitempty"`
	ProviderID    string `json:"provider_id"`
	ProviderModel string `json:"provider_model"`
	Priority      int    `json:"priority"`
	Position      int    `json:"position"`
	Enabled       bool   `json:"enabled"`
}

type PublishedModel struct {
	ID                     string            `json:"id"`
	DisplayName            string            `json:"display_name,omitempty"`
	Kind                   string            `json:"kind"` // "single" | "composite"
	Strategy               Strategy          `json:"strategy"`
	Enabled                bool              `json:"enabled"`
	DefaultCooldownSeconds int               `json:"default_cooldown_seconds"`
	Targets                []PublishedTarget `json:"targets"`
	CreatedAt              int64             `json:"created_at"`
	UpdatedAt              int64             `json:"updated_at"`
}

type CooldownKey struct {
	ProviderID    string
	ProviderModel string
}

type Cooldown struct {
	ProviderID       string `json:"provider_id"`
	ProviderModel    string `json:"provider_model"`
	Reason           string `json:"reason"`
	Message          string `json:"message,omitempty"`
	CoolingUntil     int64  `json:"cooling_until"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	Source           string `json:"source"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

type CachedProviderModel struct {
	ProviderID  string `json:"provider_id"`
	ModelName   string `json:"model_name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	IsDefault   bool   `json:"is_default,omitempty"`
	UpdatedAt   int64  `json:"updated_at"`
}

// ResolvedTarget is a concrete runnable target ordered by priority tier & cooldown readiness.
type ResolvedTarget struct {
	PublishedModelID string
	ProviderID       string
	ProviderModel    string
	Priority         int
	Position         int
	Info             executor.ModelInfo
}

type SelectionResult struct {
	Model          PublishedModel
	Candidates     []ResolvedTarget
	EarliestReset  int64
	TotalEnabled   int
	MissingBackend int
}

// Store persists published model definitions, target priorities, discovered sub-models,
// and per-(provider_id, provider_model) cooldown state in SQLite.
type Store struct {
	db       *sql.DB
	cursorMu sync.Mutex
	cursors  map[string]int // key "<model_id>:<priority>" -> next round-robin offset
}

func NewStore(db *sql.DB) *Store {
	return &Store{
		db:      db,
		cursors: make(map[string]int),
	}
}

func ValidatePublishedModelID(id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" || len(trimmed) > 128 || strings.ContainsAny(trimmed, "/\\?# \t\r\n") {
		return fmt.Errorf("%w: invalid model id %q (must be 1..128 chars without spaces, '/', '\\', '?', '#')", ErrInvalidPoolConfig, id)
	}
	return nil
}

// SaveProviderModels caches discovered models for providerID, preserving manually added custom models.
func (s *Store) SaveProviderModels(ctx context.Context, providerID string, source string, models []executor.ModelDescriptor) error {
	if s == nil || s.db == nil {
		return nil
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if source != "manual" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM provider_models_cache WHERE provider_id = ? AND source != 'manual'`, providerID); err != nil {
			return err
		}
	}
	for _, m := range models {
		name := strings.TrimSpace(m.ID)
		if name == "" {
			continue
		}
		disp := strings.TrimSpace(m.Name)
		if disp == "" {
			disp = name
		}
		defInt := 0
		if m.Default {
			defInt = 1
		}
		src := source
		if src == "" {
			src = "discovered"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_models_cache (provider_id, model_name, display_name, description, source, is_default, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(provider_id, model_name) DO UPDATE SET
				display_name = excluded.display_name,
				description = excluded.description,
				source = CASE WHEN provider_models_cache.source = 'manual' THEN 'manual' ELSE excluded.source END,
				is_default = excluded.is_default,
				updated_at = excluded.updated_at
		`, providerID, name, disp, strings.TrimSpace(m.Description), src, defInt, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddCustomProviderModel saves a user-entered custom model name under providerID.
func (s *Store) AddCustomProviderModel(ctx context.Context, providerID, modelName, displayName string) (CachedProviderModel, error) {
	modelName = strings.TrimSpace(modelName)
	if providerID == "" || modelName == "" {
		return CachedProviderModel{}, fmt.Errorf("%w: provider_id and model_name are required", ErrInvalidPoolConfig)
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = modelName
	}
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO provider_models_cache (provider_id, model_name, display_name, description, source, is_default, updated_at)
		VALUES (?, ?, ?, '', 'manual', 0, ?)
		ON CONFLICT(provider_id, model_name) DO UPDATE SET
			display_name = excluded.display_name,
			source = 'manual',
			updated_at = excluded.updated_at
	`, providerID, modelName, displayName, now); err != nil {
		return CachedProviderModel{}, err
	}
	return CachedProviderModel{
		ProviderID:  providerID,
		ModelName:   modelName,
		DisplayName: displayName,
		Source:      "manual",
		UpdatedAt:   now,
	}, nil
}

// ListProviderModels returns all cached & manual models known for providerID.
func (s *Store) ListProviderModels(ctx context.Context, providerID string) ([]CachedProviderModel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider_id, model_name, display_name, description, source, is_default, updated_at
		FROM provider_models_cache
		WHERE provider_id = ?
		ORDER BY is_default DESC, CASE WHEN source = 'manual' THEN 0 ELSE 1 END ASC, model_name ASC
	`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CachedProviderModel
	for rows.Next() {
		var item CachedProviderModel
		var isDef int
		if err := rows.Scan(&item.ProviderID, &item.ModelName, &item.DisplayName, &item.Description, &item.Source, &isDef, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.IsDefault = isDef != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

// UpsertPublishedModel validates and persists a single or composite published model and its prioritized targets.
func (s *Store) UpsertPublishedModel(ctx context.Context, pm PublishedModel) (PublishedModel, error) {
	pm.ID = strings.TrimSpace(pm.ID)
	if err := ValidatePublishedModelID(pm.ID); err != nil {
		return PublishedModel{}, err
	}
	pm.DisplayName = strings.TrimSpace(pm.DisplayName)
	if pm.DisplayName == "" {
		pm.DisplayName = pm.ID
	}
	if pm.Strategy == "" {
		pm.Strategy = StrategyPriorityOrder
	}
	if pm.Strategy != StrategyPriorityOrder && pm.Strategy != StrategyRoundRobin {
		return PublishedModel{}, fmt.Errorf("%w: unsupported strategy %q", ErrInvalidPoolConfig, pm.Strategy)
	}
	if pm.DefaultCooldownSeconds <= 0 {
		pm.DefaultCooldownSeconds = 300
	}
	if len(pm.Targets) == 0 {
		return PublishedModel{}, fmt.Errorf("%w: published model requires at least one provider target", ErrInvalidPoolConfig)
	}

	seen := make(map[CooldownKey]struct{}, len(pm.Targets))
	cleanTargets := make([]PublishedTarget, 0, len(pm.Targets))
	for idx, t := range pm.Targets {
		t.ProviderID = strings.TrimSpace(t.ProviderID)
		t.ProviderModel = strings.TrimSpace(t.ProviderModel)
		if t.ProviderID == "" {
			return PublishedModel{}, fmt.Errorf("%w: targets[%d].provider_id is required", ErrInvalidPoolConfig, idx)
		}
		key := CooldownKey{ProviderID: t.ProviderID, ProviderModel: t.ProviderModel}
		if _, dup := seen[key]; dup {
			return PublishedModel{}, fmt.Errorf("%w: (%s, %s)", ErrDuplicateTarget, t.ProviderID, t.ProviderModel)
		}
		seen[key] = struct{}{}
		if t.Priority <= 0 {
			t.Priority = 1
		}
		if t.Position < 0 {
			t.Position = idx
		}
		cleanTargets = append(cleanTargets, t)
	}
	if pm.Kind == "" {
		if len(cleanTargets) == 1 {
			pm.Kind = "single"
		} else {
			pm.Kind = "composite"
		}
	}
	if pm.Kind != "single" && pm.Kind != "composite" {
		return PublishedModel{}, fmt.Errorf("%w: invalid kind %q", ErrInvalidPoolConfig, pm.Kind)
	}
	if len(cleanTargets) > 1 {
		pm.Kind = "composite"
	}

	now := time.Now().Unix()
	enInt := 0
	if pm.Enabled {
		enInt = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PublishedModel{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingCreated int64
	_ = tx.QueryRowContext(ctx, `SELECT created_at FROM published_models WHERE id = ?`, pm.ID).Scan(&existingCreated)
	createdAt := now
	if existingCreated > 0 {
		createdAt = existingCreated
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO published_models (id, display_name, kind, strategy, enabled, default_cooldown_seconds, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name = excluded.display_name,
			kind = excluded.kind,
			strategy = excluded.strategy,
			enabled = excluded.enabled,
			default_cooldown_seconds = excluded.default_cooldown_seconds,
			updated_at = excluded.updated_at
	`, pm.ID, pm.DisplayName, pm.Kind, string(pm.Strategy), enInt, pm.DefaultCooldownSeconds, createdAt, now); err != nil {
		return PublishedModel{}, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM published_model_targets WHERE published_model_id = ?`, pm.ID); err != nil {
		return PublishedModel{}, err
	}

	for idx, t := range cleanTargets {
		tEn := 0
		if t.Enabled {
			tEn = 1
		}
		pos := idx
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO published_model_targets (published_model_id, provider_id, provider_model, priority, position, enabled)
			VALUES (?, ?, ?, ?, ?, ?)
		`, pm.ID, t.ProviderID, t.ProviderModel, t.Priority, pos, tEn); err != nil {
			return PublishedModel{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return PublishedModel{}, err
	}
	return s.GetPublishedModel(ctx, pm.ID)
}

func (s *Store) SetPublishedModelEnabled(ctx context.Context, id string, enabled bool) (PublishedModel, error) {
	enInt := 0
	if enabled {
		enInt = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE published_models SET enabled = ?, updated_at = ? WHERE id = ?`, enInt, time.Now().Unix(), id)
	if err != nil {
		return PublishedModel{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return PublishedModel{}, ErrModelNotFound
	}
	return s.GetPublishedModel(ctx, id)
}

func (s *Store) DeletePublishedModel(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM published_models WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrModelNotFound
	}
	return nil
}

func (s *Store) GetPublishedModel(ctx context.Context, id string) (PublishedModel, error) {
	var m PublishedModel
	var enInt int
	var strat string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, kind, strategy, enabled, default_cooldown_seconds, created_at, updated_at
		FROM published_models WHERE id = ?
	`, id).Scan(&m.ID, &m.DisplayName, &m.Kind, &strat, &enInt, &m.DefaultCooldownSeconds, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedModel{}, ErrModelNotFound
	}
	if err != nil {
		return PublishedModel{}, err
	}
	m.Enabled = enInt != 0
	m.Strategy = Strategy(strat)

	targets, err := s.listTargetsForModel(ctx, m.ID)
	if err != nil {
		return PublishedModel{}, err
	}
	m.Targets = targets
	return m, nil
}

func (s *Store) ListPublishedModels(ctx context.Context, onlyEnabled bool) ([]PublishedModel, error) {
	query := `
		SELECT id, display_name, kind, strategy, enabled, default_cooldown_seconds, created_at, updated_at
		FROM published_models
	`
	if onlyEnabled {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PublishedModel
	for rows.Next() {
		var m PublishedModel
		var enInt int
		var strat string
		if err := rows.Scan(&m.ID, &m.DisplayName, &m.Kind, &strat, &enInt, &m.DefaultCooldownSeconds, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Enabled = enInt != 0
		m.Strategy = Strategy(strat)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		targets, err := s.listTargetsForModel(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Targets = targets
	}
	return out, nil
}

func (s *Store) listTargetsForModel(ctx context.Context, modelID string) ([]PublishedTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider_id, provider_model, priority, position, enabled
		FROM published_model_targets
		WHERE published_model_id = ?
		ORDER BY priority ASC, position ASC, id ASC
	`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PublishedTarget
	for rows.Next() {
		var t PublishedTarget
		var enInt int
		if err := rows.Scan(&t.ID, &t.ProviderID, &t.ProviderModel, &t.Priority, &t.Position, &enInt); err != nil {
			return nil, err
		}
		t.Enabled = enInt != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetCooldown records an active cooldown window for a specific (provider_id, provider_model) pair.
func (s *Store) SetCooldown(ctx context.Context, providerID, providerModel, reason, message string, until time.Time, source string) error {
	if s == nil || s.db == nil {
		return nil
	}
	now := time.Now().Unix()
	untilUnix := until.Unix()
	if untilUnix <= now {
		untilUnix = now + 1
	}
	if source == "" {
		source = "default"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO provider_model_cooldowns (provider_id, provider_model, reason, message, cooling_until, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, provider_model) DO UPDATE SET
			reason = excluded.reason,
			message = excluded.message,
			cooling_until = excluded.cooling_until,
			source = excluded.source,
			updated_at = excluded.updated_at
	`, providerID, providerModel, reason, message, untilUnix, source, now, now)
	return err
}

// ReleaseCooldown immediately clears cooldown for a (provider_id, provider_model) pair.
func (s *Store) ReleaseCooldown(ctx context.Context, providerID, providerModel string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM provider_model_cooldowns WHERE provider_id = ? AND provider_model = ?`, providerID, providerModel)
	return err
}

// ListCooldowns returns all currently active cooldowns (purging expired rows cleanly).
func (s *Store) ListCooldowns(ctx context.Context) ([]Cooldown, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	now := time.Now().Unix()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM provider_model_cooldowns WHERE cooling_until <= ?`, now)

	rows, err := s.db.QueryContext(ctx, `
		SELECT provider_id, provider_model, reason, message, cooling_until, source, created_at, updated_at
		FROM provider_model_cooldowns
		WHERE cooling_until > ?
		ORDER BY cooling_until ASC, provider_id ASC, provider_model ASC
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Cooldown
	for rows.Next() {
		var c Cooldown
		if err := rows.Scan(&c.ProviderID, &c.ProviderModel, &c.Reason, &c.Message, &c.CoolingUntil, &c.Source, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.RemainingSeconds = c.CoolingUntil - now
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveCooldownMap indexes all currently active cooldowns keyed by (provider_id, provider_model).
func (s *Store) ActiveCooldownMap(ctx context.Context) (map[CooldownKey]Cooldown, error) {
	list, err := s.ListCooldowns(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[CooldownKey]Cooldown, len(list))
	for _, c := range list {
		m[CooldownKey{ProviderID: c.ProviderID, ProviderModel: c.ProviderModel}] = c
	}
	return m, nil
}

// ResolveCandidates orders ready non-cooling targets for publishedModelID strictly by priority tier,
// applying strategy (priority_order or round_robin) within equal priority tiers.
func (s *Store) ResolveCandidates(ctx context.Context, registry *executor.Registry, publishedModelID string) (SelectionResult, error) {
	pm, err := s.GetPublishedModel(ctx, publishedModelID)
	if err != nil {
		return SelectionResult{}, err
	}
	cooldowns, err := s.ActiveCooldownMap(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	return s.resolveFromLoaded(registry, pm, cooldowns, time.Now().Unix(), true), nil
}

// InspectAvailability computes non-mutating availability status for a published model (for GET /v1/models and Admin UI).
func (s *Store) InspectAvailability(registry *executor.Registry, pm PublishedModel, cooldowns map[CooldownKey]Cooldown, nowUnix int64) SelectionResult {
	return s.resolveFromLoaded(registry, pm, cooldowns, nowUnix, false)
}

func (s *Store) resolveFromLoaded(registry *executor.Registry, pm PublishedModel, cooldowns map[CooldownKey]Cooldown, nowUnix int64, advanceCursor bool) SelectionResult {
	res := SelectionResult{Model: pm}
	var ready []ResolvedTarget
	var earliestReset int64

	for _, t := range pm.Targets {
		if !t.Enabled {
			continue
		}
		res.TotalEnabled++
		backend, ok := registry.GetProvider(t.ProviderID)
		if !ok {
			res.MissingBackend++
			continue
		}
		effModel := strings.TrimSpace(t.ProviderModel)
		if effModel == "" {
			effModel = strings.TrimSpace(backend.Config.Model)
		}
		if cd, isCooling := cooldowns[CooldownKey{ProviderID: t.ProviderID, ProviderModel: effModel}]; isCooling && cd.CoolingUntil > nowUnix {
			if earliestReset == 0 || cd.CoolingUntil < earliestReset {
				earliestReset = cd.CoolingUntil
			}
			continue
		}
		ready = append(ready, ResolvedTarget{
			PublishedModelID: pm.ID,
			ProviderID:       t.ProviderID,
			ProviderModel:    effModel,
			Priority:         t.Priority,
			Position:         t.Position,
			Info:             backend,
		})
	}
	res.EarliestReset = earliestReset
	if len(ready) == 0 {
		return res
	}

	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority < ready[j].Priority
		}
		return ready[i].Position < ready[j].Position
	})

	ordered := make([]ResolvedTarget, 0, len(ready))
	for i := 0; i < len(ready); {
		j := i + 1
		for j < len(ready) && ready[j].Priority == ready[i].Priority {
			j++
		}
		tier := ready[i:j]
		if pm.Strategy == StrategyRoundRobin && len(tier) > 1 {
			key := fmt.Sprintf("%s:%d", pm.ID, tier[0].Priority)
			s.cursorMu.Lock()
			offset := s.cursors[key] % len(tier)
			if advanceCursor {
				s.cursors[key] = (offset + 1) % len(tier)
			}
			s.cursorMu.Unlock()
			ordered = append(ordered, tier[offset:]...)
			ordered = append(ordered, tier[:offset]...)
		} else {
			ordered = append(ordered, tier...)
		}
		i = j
	}
	res.Candidates = ordered
	return res
}
