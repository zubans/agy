package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

// ModelCapabilities stores declared multimodal/tool flags on a published model.
type ModelCapabilities struct {
	ImageIn  bool `json:"image_in"`
	AudioIn  bool `json:"audio_in"`
	ImageOut bool `json:"image_out"`
	AudioOut bool `json:"audio_out"`
	Tools    bool `json:"tools"`
}

// BackendStore persists tool_backends, published_model_tools, and published_models.capabilities_json.
type BackendStore struct {
	db *sql.DB
}

func NewBackendStore(db *sql.DB) *BackendStore {
	return &BackendStore{db: db}
}

var defaultBackends = []BackendConfig{
	{Key: "whisper", Name: "Whisper / STT (OpenAI-compatible /v1/audio/transcriptions)", Model: "whisper-1"},
	{Key: "tts", Name: "Text-to-Speech (OpenAI-compatible /v1/audio/speech)", Model: "tts-1"},
	{Key: "image_gen", Name: "Image Generation (OpenAI-compatible /v1/images/generations)", Model: "gpt-image-1"},
	{Key: "media_tools", Name: "Media Tools Sidecar (optional remote ffmpeg/OCR/render)"},
}

// ListBackends returns all known tool backends merged with env defaults and SQLite overrides.
func (s *BackendStore) ListBackends(ctx context.Context) ([]BackendConfig, error) {
	byKey := make(map[string]BackendConfig, len(defaultBackends))
	order := make([]string, 0, len(defaultBackends))
	for _, d := range defaultBackends {
		cfg := applyEnvBackendDefaults(d)
		byKey[cfg.Key] = cfg
		order = append(order, cfg.Key)
	}

	if s != nil && s.db != nil {
		rows, err := s.db.QueryContext(ctx, `
			SELECT key, name, base_url, api_key, model, enabled, extra_json, updated_at
			FROM tool_backends
			ORDER BY key ASC
		`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				b         BackendConfig
				enabledI  int
				extraJSON string
			)
			if err := rows.Scan(&b.Key, &b.Name, &b.BaseURL, &b.APIKey, &b.Model, &enabledI, &extraJSON, &b.UpdatedAt); err != nil {
				return nil, err
			}
			b.Enabled = enabledI != 0
			if extraJSON != "" && extraJSON != "{}" {
				_ = json.Unmarshal([]byte(extraJSON), &b.Extra)
			}
			b.HasAPIKey = b.APIKey != ""
			if _, exists := byKey[b.Key]; !exists {
				order = append(order, b.Key)
			}
			byKey[b.Key] = b
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	out := make([]BackendConfig, 0, len(order))
	for _, k := range order {
		cfg := byKey[k]
		cfg.HasAPIKey = cfg.APIKey != ""
		out = append(out, cfg)
	}
	return out, nil
}

// GetBackend returns one backend configuration (including API key for internal HTTP calls).
func (s *BackendStore) GetBackend(ctx context.Context, key string) (BackendConfig, bool) {
	list, err := s.ListBackends(ctx)
	if err != nil {
		return BackendConfig{}, false
	}
	for _, b := range list {
		if b.Key == key {
			return b, true
		}
	}
	return BackendConfig{}, false
}

// UpsertBackend saves a backend configuration to SQLite. Empty api_key preserves existing stored key.
func (s *BackendStore) UpsertBackend(ctx context.Context, b BackendConfig) (BackendConfig, error) {
	if s == nil || s.db == nil {
		return BackendConfig{}, errors.New("backend store not initialized")
	}
	b.Key = strings.TrimSpace(b.Key)
	if b.Key == "" {
		return BackendConfig{}, errors.New("backend key is required")
	}
	b.BaseURL = strings.TrimRight(strings.TrimSpace(b.BaseURL), "/")
	if b.Name == "" {
		b.Name = b.Key
	}
	if b.APIKey == "" {
		if existing, ok := s.GetBackend(ctx, b.Key); ok && existing.APIKey != "" {
			b.APIKey = existing.APIKey
		}
	}
	extraBytes := []byte("{}")
	if len(b.Extra) > 0 {
		if raw, err := json.Marshal(b.Extra); err == nil {
			extraBytes = raw
		}
	}
	now := time.Now().Unix()
	enabledI := 0
	if b.Enabled {
		enabledI = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_backends (key, name, base_url, api_key, model, enabled, extra_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			name = excluded.name,
			base_url = excluded.base_url,
			api_key = excluded.api_key,
			model = excluded.model,
			enabled = excluded.enabled,
			extra_json = excluded.extra_json,
			updated_at = excluded.updated_at
	`, b.Key, b.Name, b.BaseURL, b.APIKey, b.Model, enabledI, string(extraBytes), now)
	if err != nil {
		return BackendConfig{}, err
	}
	b.UpdatedAt = now
	b.HasAPIKey = b.APIKey != ""
	return b, nil
}

// GetModelTools returns the enabled server tool names attached to a published model.
func (s *BackendStore) GetModelTools(ctx context.Context, publishedModelID string) ([]string, error) {
	if s == nil || s.db == nil || strings.TrimSpace(publishedModelID) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT tool_name
		FROM published_model_tools
		WHERE published_model_id = ? AND enabled = 1
		ORDER BY tool_name ASC
	`, publishedModelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil && name != "" {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// SetModelTools replaces the allowlist of server tools bound to a published model.
func (s *BackendStore) SetModelTools(ctx context.Context, publishedModelID string, toolNames []string) error {
	if s == nil || s.db == nil {
		return errors.New("backend store not initialized")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM published_model_tools WHERE published_model_id = ?`, publishedModelID); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(toolNames))
	for _, raw := range toolNames {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO published_model_tools (published_model_id, tool_name, enabled) VALUES (?, ?, 1)
		`, publishedModelID, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetModelCapabilities reads capabilities_json for a published model.
func (s *BackendStore) GetModelCapabilities(ctx context.Context, publishedModelID string) (ModelCapabilities, error) {
	if s == nil || s.db == nil || strings.TrimSpace(publishedModelID) == "" {
		return ModelCapabilities{}, nil
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT capabilities_json FROM published_models WHERE id = ?`, publishedModelID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelCapabilities{}, nil
	}
	if err != nil {
		return ModelCapabilities{}, err
	}
	var caps ModelCapabilities
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &caps)
	}
	return caps, nil
}

// SetModelCapabilities updates capabilities_json on a published model.
func (s *BackendStore) SetModelCapabilities(ctx context.Context, publishedModelID string, caps ModelCapabilities) error {
	if s == nil || s.db == nil {
		return errors.New("backend store not initialized")
	}
	raw, err := json.Marshal(caps)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE published_models SET capabilities_json = ? WHERE id = ?`, string(raw), publishedModelID)
	return err
}

func applyEnvBackendDefaults(b BackendConfig) BackendConfig {
	switch b.Key {
	case "whisper":
		if u := strings.TrimRight(strings.TrimSpace(os.Getenv("GATEWAY_WHISPER_URL")), "/"); u != "" {
			b.BaseURL = u
			b.Enabled = true
		}
		if k := strings.TrimSpace(os.Getenv("GATEWAY_WHISPER_API_KEY")); k != "" {
			b.APIKey = k
		}
		if m := strings.TrimSpace(os.Getenv("GATEWAY_WHISPER_MODEL")); m != "" {
			b.Model = m
		}
	case "tts":
		if u := strings.TrimRight(strings.TrimSpace(os.Getenv("GATEWAY_TTS_URL")), "/"); u != "" {
			b.BaseURL = u
			b.Enabled = true
		}
		if k := strings.TrimSpace(os.Getenv("GATEWAY_TTS_API_KEY")); k != "" {
			b.APIKey = k
		}
		if m := strings.TrimSpace(os.Getenv("GATEWAY_TTS_MODEL")); m != "" {
			b.Model = m
		}
	case "image_gen":
		if u := strings.TrimRight(strings.TrimSpace(os.Getenv("GATEWAY_IMAGE_GEN_URL")), "/"); u != "" {
			b.BaseURL = u
			b.Enabled = true
		}
		if k := strings.TrimSpace(os.Getenv("GATEWAY_IMAGE_GEN_API_KEY")); k != "" {
			b.APIKey = k
		}
		if m := strings.TrimSpace(os.Getenv("GATEWAY_IMAGE_GEN_MODEL")); m != "" {
			b.Model = m
		}
	case "media_tools":
		if u := strings.TrimRight(strings.TrimSpace(os.Getenv("GATEWAY_MEDIA_TOOLS_URL")), "/"); u != "" {
			b.BaseURL = u
			b.Enabled = true
		}
	}
	b.HasAPIKey = b.APIKey != ""
	return b
}
