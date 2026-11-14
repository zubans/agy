package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the complete gateway configuration loaded from YAML and environment overrides.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	Limits  LimitsConfig  `yaml:"limits"`
	Logging LoggingConfig `yaml:"logging"`
	Auth    AuthConfig    `yaml:"auth"`
	Admin   AdminConfig   `yaml:"admin"`
	Models  []ModelConfig `yaml:"models"`
}

type AdminConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Token            string `yaml:"token"`
	TokenSHA256      string `yaml:"token_sha256"`
	AllowWithoutAuth bool   `yaml:"allow_without_auth"`
}

// EffectiveHash returns the lowercase SHA-256 hex digest of the configured admin token.
func (a AdminConfig) EffectiveHash() string {
	if strings.TrimSpace(a.TokenSHA256) != "" {
		return strings.ToLower(strings.TrimSpace(a.TokenSHA256))
	}
	if a.Token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(a.Token))
	return hex.EncodeToString(sum[:])
}

type ServerConfig struct {
	Listen                   string `yaml:"listen"`
	ReadHeaderTimeoutSeconds int    `yaml:"read_header_timeout_seconds"`
	ShutdownTimeoutSeconds   int    `yaml:"shutdown_timeout_seconds"`
}

type StorageConfig struct {
	Path                   string `yaml:"path"`
	SQLitePath             string `yaml:"sqlite_path"`
	MaxFileBytes           int64  `yaml:"max_file_bytes"`
	QuotaBytes             int64  `yaml:"quota_bytes"`
	FileTTLSeconds         int    `yaml:"file_ttl_seconds"`
	RunTTLSeconds          int    `yaml:"run_ttl_seconds"`
	SessionTTLSeconds      int    `yaml:"session_ttl_seconds"`
	CleanupIntervalSeconds int    `yaml:"cleanup_interval_seconds"`
	CleanupRunInput        *bool  `yaml:"cleanup_run_input"`
}

type LimitsConfig struct {
	MaxGlobalProcesses        int   `yaml:"max_global_processes"`
	MaxExecutorProcesses      int   `yaml:"max_executor_processes"`
	MaxUserProcesses          int   `yaml:"max_user_processes"`
	MaxQueueSize              int   `yaml:"max_queue_size"`
	QueueWaitTimeoutSeconds   int   `yaml:"queue_wait_timeout_seconds"`
	SessionLockTimeoutSeconds int   `yaml:"session_lock_timeout_seconds"`
	MaxOutputBytes            int64 `yaml:"max_output_bytes"`
	MaxFilesPerTask           int   `yaml:"max_files_per_task"`
	DefaultTimeoutSeconds     int   `yaml:"default_timeout_seconds"`
	MaxTimeoutSeconds         int   `yaml:"max_timeout_seconds"`
}

type LoggingConfig struct {
	Level      string `yaml:"level"`
	Diagnostic bool   `yaml:"diagnostic"`
}

type AuthConfig struct {
	Keys []APIKeyConfig `yaml:"keys"`
}

type APIKeyConfig struct {
	ID            string   `yaml:"id"`
	OwnerID       string   `yaml:"owner_id"`
	Name          string   `yaml:"name"`
	Key           string   `yaml:"key"`
	KeySHA256     string   `yaml:"key_sha256"`
	AllowedModels []string `yaml:"allowed_models"`
	MaxConcurrent int      `yaml:"max_concurrent"`
	Revoked       bool     `yaml:"revoked"`
}

type ModelConfig struct {
	ID             string            `yaml:"id"`
	Provider       string            `yaml:"provider"`
	Executable     string            `yaml:"executable"`
	BaseURL        string            `yaml:"base_url"`
	WorkerURL      string            `yaml:"worker_url"`
	WorkerToken    string            `yaml:"worker_token"`
	Model          string            `yaml:"model"`
	TimeoutSeconds int               `yaml:"timeout_seconds"`
	HomeDir        string            `yaml:"home_dir"`
	Env            map[string]string `yaml:"env"`
	SandboxMode    string            `yaml:"sandbox_mode"`
	AllowedTools   []string          `yaml:"allowed_tools"`
	AllowCommands  bool              `yaml:"allow_commands"`
	FSAccess       string            `yaml:"fs_access"`
	MaxConcurrent  int               `yaml:"max_concurrent"`
	Enabled        *bool             `yaml:"enabled"`
}

// EffectiveEnabled returns whether the model is enabled (defaults to true).
func (m ModelConfig) EffectiveEnabled() bool {
	if m.Enabled == nil {
		return true
	}
	return *m.Enabled
}

// EffectiveCleanupRunInput returns whether temporary run input dirs are purged on finish.
func (s StorageConfig) EffectiveCleanupRunInput() bool {
	if s.CleanupRunInput == nil {
		return true
	}
	return *s.CleanupRunInput
}

// EffectiveHash returns the lowercase hex SHA-256 hash for an API key entry.
func (k APIKeyConfig) EffectiveHash() string {
	if strings.TrimSpace(k.KeySHA256) != "" {
		return strings.ToLower(strings.TrimSpace(k.KeySHA256))
	}
	sum := sha256.Sum256([]byte(k.Key))
	return hex.EncodeToString(sum[:])
}

// Load reads YAML configuration from path, applies defaults and env overrides, and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return Parse(data)
}

// Parse decodes YAML bytes into a validated Config.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}
	applyDefaults(&cfg)
	applyEnvOverrides(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "127.0.0.1:8080"
	}
	if cfg.Server.ReadHeaderTimeoutSeconds <= 0 {
		cfg.Server.ReadHeaderTimeoutSeconds = 15
	}
	if cfg.Server.ShutdownTimeoutSeconds <= 0 {
		cfg.Server.ShutdownTimeoutSeconds = 30
	}

	if cfg.Storage.Path == "" {
		cfg.Storage.Path = "./data"
	}
	if cfg.Storage.SQLitePath == "" {
		cfg.Storage.SQLitePath = filepath.Join(cfg.Storage.Path, "gateway.db")
	}
	if cfg.Storage.MaxFileBytes <= 0 {
		cfg.Storage.MaxFileBytes = 50 * 1024 * 1024 // 50 MiB per Section 6.2
	}
	if cfg.Storage.QuotaBytes <= 0 {
		cfg.Storage.QuotaBytes = 2 * 1024 * 1024 * 1024 // 2 GiB default quota
	}
	if cfg.Storage.FileTTLSeconds <= 0 {
		cfg.Storage.FileTTLSeconds = 24 * 3600
	}
	if cfg.Storage.RunTTLSeconds <= 0 {
		cfg.Storage.RunTTLSeconds = 7 * 24 * 3600
	}
	if cfg.Storage.SessionTTLSeconds <= 0 {
		cfg.Storage.SessionTTLSeconds = 30 * 24 * 3600
	}
	if cfg.Storage.CleanupIntervalSeconds <= 0 {
		cfg.Storage.CleanupIntervalSeconds = 300
	}

	if cfg.Limits.MaxGlobalProcesses <= 0 {
		cfg.Limits.MaxGlobalProcesses = 16
	}
	if cfg.Limits.MaxExecutorProcesses <= 0 {
		cfg.Limits.MaxExecutorProcesses = 4
	}
	if cfg.Limits.MaxUserProcesses <= 0 {
		cfg.Limits.MaxUserProcesses = 4
	}
	if cfg.Limits.MaxQueueSize <= 0 {
		cfg.Limits.MaxQueueSize = 32
	}
	if cfg.Limits.QueueWaitTimeoutSeconds <= 0 {
		cfg.Limits.QueueWaitTimeoutSeconds = 30
	}
	if cfg.Limits.SessionLockTimeoutSeconds <= 0 {
		cfg.Limits.SessionLockTimeoutSeconds = 120
	}
	if cfg.Limits.MaxOutputBytes <= 0 {
		cfg.Limits.MaxOutputBytes = 16 * 1024 * 1024
	}
	if cfg.Limits.MaxFilesPerTask <= 0 {
		cfg.Limits.MaxFilesPerTask = 10
	}
	if cfg.Limits.DefaultTimeoutSeconds <= 0 {
		cfg.Limits.DefaultTimeoutSeconds = 600
	}
	if cfg.Limits.MaxTimeoutSeconds <= 0 {
		cfg.Limits.MaxTimeoutSeconds = 1800
	}

	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}

	for i := range cfg.Models {
		if cfg.Models[i].TimeoutSeconds <= 0 {
			cfg.Models[i].TimeoutSeconds = cfg.Limits.DefaultTimeoutSeconds
		}
		if cfg.Models[i].FSAccess == "" {
			cfg.Models[i].FSAccess = "workspace-read"
		}
		if cfg.Models[i].MaxConcurrent <= 0 {
			cfg.Models[i].MaxConcurrent = cfg.Limits.MaxExecutorProcesses
		}
	}

	for i := range cfg.Auth.Keys {
		if len(cfg.Auth.Keys[i].AllowedModels) == 0 {
			cfg.Auth.Keys[i].AllowedModels = []string{"*"}
		}
		if cfg.Auth.Keys[i].MaxConcurrent <= 0 {
			cfg.Auth.Keys[i].MaxConcurrent = cfg.Limits.MaxUserProcesses
		}
		if cfg.Auth.Keys[i].Name == "" {
			cfg.Auth.Keys[i].Name = cfg.Auth.Keys[i].ID
		}
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("GATEWAY_LISTEN")); v != "" {
		cfg.Server.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_STORAGE_PATH")); v != "" {
		cfg.Storage.Path = v
		if os.Getenv("GATEWAY_SQLITE_PATH") == "" {
			cfg.Storage.SQLitePath = filepath.Join(v, "gateway.db")
		}
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_SQLITE_PATH")); v != "" {
		cfg.Storage.SQLitePath = v
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_LOG_LEVEL")); v != "" {
		cfg.Logging.Level = v
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_DIAGNOSTIC_LOG")); v == "1" || strings.EqualFold(v, "true") {
		cfg.Logging.Diagnostic = true
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_TOKEN")); v != "" {
		cfg.Admin.Token = v
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_ENABLED")); v != "" {
		if v == "1" || strings.EqualFold(v, "true") {
			cfg.Admin.Enabled = true
		} else if v == "0" || strings.EqualFold(v, "false") {
			cfg.Admin.Enabled = false
		}
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY")); v != "" && len(cfg.Auth.Keys) > 0 {
		cfg.Auth.Keys[0].Key = v
		cfg.Auth.Keys[0].KeySHA256 = ""
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_WORKER_TOKEN")); v != "" {
		for i := range cfg.Models {
			if strings.TrimSpace(cfg.Models[i].WorkerURL) != "" && strings.TrimSpace(cfg.Models[i].WorkerToken) == "" {
				cfg.Models[i].WorkerToken = v
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_OLLAMA_BASE_URL")); v != "" {
		en := true
		for i := range cfg.Models {
			if cfg.Models[i].Provider == "ollama" {
				cfg.Models[i].BaseURL = v
				cfg.Models[i].Enabled = &en
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("GATEWAY_OLLAMA_MODEL")); v != "" {
		for i := range cfg.Models {
			if cfg.Models[i].Provider == "ollama" {
				cfg.Models[i].Model = v
			}
		}
	}
}

// Validate enforces structural and security invariants at startup (Section 10).
func (c *Config) Validate() error {
	var errs []error

	if strings.TrimSpace(c.Server.Listen) == "" {
		errs = append(errs, errors.New("server.listen must not be empty"))
	}
	if strings.TrimSpace(c.Storage.Path) == "" {
		errs = append(errs, errors.New("storage.path must not be empty"))
	}
	if c.Storage.MaxFileBytes <= 0 {
		errs = append(errs, errors.New("storage.max_file_bytes must be positive"))
	}
	if c.Storage.QuotaBytes < c.Storage.MaxFileBytes {
		errs = append(errs, errors.New("storage.quota_bytes must be >= storage.max_file_bytes"))
	}
	if c.Limits.MaxGlobalProcesses < 1 {
		errs = append(errs, errors.New("limits.max_global_processes must be >= 1"))
	}
	if c.Limits.MaxExecutorProcesses < 1 || c.Limits.MaxUserProcesses < 1 {
		errs = append(errs, errors.New("limits.max_executor_processes and max_user_processes must be >= 1"))
	}
	if c.Limits.MaxTimeoutSeconds < c.Limits.DefaultTimeoutSeconds {
		errs = append(errs, errors.New("limits.max_timeout_seconds must be >= limits.default_timeout_seconds"))
	}

	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("logging.level %q must be debug|info|warn|error", c.Logging.Level))
	}

	seenKeys := make(map[string]struct{})
	seenHashes := make(map[string]struct{})
	for i, k := range c.Auth.Keys {
		if strings.TrimSpace(k.ID) == "" {
			errs = append(errs, fmt.Errorf("auth.keys[%d].id must not be empty", i))
		} else if _, dup := seenKeys[k.ID]; dup {
			errs = append(errs, fmt.Errorf("duplicate auth.keys id %q", k.ID))
		} else {
			seenKeys[k.ID] = struct{}{}
		}
		if strings.TrimSpace(k.OwnerID) == "" {
			errs = append(errs, fmt.Errorf("auth.keys[%d].owner_id must not be empty", i))
		}
		if strings.TrimSpace(k.Key) == "" && strings.TrimSpace(k.KeySHA256) == "" {
			errs = append(errs, fmt.Errorf("auth.keys[%d] requires key or key_sha256", i))
		}
		if k.KeySHA256 != "" {
			if len(strings.TrimSpace(k.KeySHA256)) != 64 {
				errs = append(errs, fmt.Errorf("auth.keys[%d].key_sha256 must be 64 hex chars", i))
			} else if _, err := hex.DecodeString(strings.TrimSpace(k.KeySHA256)); err != nil {
				errs = append(errs, fmt.Errorf("auth.keys[%d].key_sha256 invalid hex: %w", i, err))
			}
		}
		h := k.EffectiveHash()
		if _, dup := seenHashes[h]; dup {
			errs = append(errs, fmt.Errorf("duplicate API key hash at auth.keys[%d]", i))
		}
		seenHashes[h] = struct{}{}
	}

	if len(c.Models) == 0 {
		errs = append(errs, errors.New("at least one model must be configured in models"))
	}

	if c.Admin.TokenSHA256 != "" {
		if len(strings.TrimSpace(c.Admin.TokenSHA256)) != 64 {
			errs = append(errs, errors.New("admin.token_sha256 must be 64 hex chars"))
		} else if _, err := hex.DecodeString(strings.TrimSpace(c.Admin.TokenSHA256)); err != nil {
			errs = append(errs, fmt.Errorf("admin.token_sha256 invalid hex: %w", err))
		}
	}
	if c.Admin.Enabled && strings.TrimSpace(c.Admin.Token) == "" && strings.TrimSpace(c.Admin.TokenSHA256) == "" && !c.Admin.AllowWithoutAuth {
		errs = append(errs, errors.New("admin.enabled requires admin.token, admin.token_sha256, or admin.allow_without_auth: true"))
	}

	seenModels := make(map[string]struct{})
	for i, m := range c.Models {
		if strings.TrimSpace(m.ID) == "" {
			errs = append(errs, fmt.Errorf("models[%d].id must not be empty", i))
		} else {
			if _, dup := seenModels[m.ID]; dup {
				errs = append(errs, fmt.Errorf("duplicate model id %q", m.ID))
			}
			seenModels[m.ID] = struct{}{}
		}
		switch m.Provider {
		case "codex", "antigravity", "gemini", "claude", "mock":
			hasWorker := strings.TrimSpace(m.WorkerURL) != ""
			hasExec := strings.TrimSpace(m.Executable) != ""
			if !hasWorker && !hasExec {
				errs = append(errs, fmt.Errorf("models[%d] (%s): executable is required (unless worker_url is set)", i, m.ID))
			}
			if hasWorker {
				if u, err := url.Parse(strings.TrimSpace(m.WorkerURL)); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					errs = append(errs, fmt.Errorf("models[%d] (%s): invalid worker_url %q", i, m.ID, m.WorkerURL))
				}
			}
		case "ollama":
			if strings.TrimSpace(m.BaseURL) == "" {
				errs = append(errs, fmt.Errorf("models[%d] (%s): base_url is required for ollama", i, m.ID))
			} else if u, err := url.Parse(m.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("models[%d] (%s): invalid base_url %q", i, m.ID, m.BaseURL))
			}
			if strings.TrimSpace(m.Model) == "" {
				errs = append(errs, fmt.Errorf("models[%d] (%s): model is required for ollama", i, m.ID))
			}
			if strings.TrimSpace(m.WorkerURL) != "" {
				if u, err := url.Parse(strings.TrimSpace(m.WorkerURL)); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					errs = append(errs, fmt.Errorf("models[%d] (%s): invalid worker_url %q", i, m.ID, m.WorkerURL))
				}
			}
		default:
			errs = append(errs, fmt.Errorf("models[%d] (%s): unsupported provider %q", i, m.ID, m.Provider))
		}

		switch m.FSAccess {
		case "none", "workspace-read", "workspace-write":
		default:
			errs = append(errs, fmt.Errorf("models[%d] (%s): invalid fs_access %q", i, m.ID, m.FSAccess))
		}

		if m.TimeoutSeconds <= 0 || m.TimeoutSeconds > c.Limits.MaxTimeoutSeconds {
			errs = append(errs, fmt.Errorf("models[%d] (%s): timeout_seconds %d out of range [1, %d]", i, m.ID, m.TimeoutSeconds, c.Limits.MaxTimeoutSeconds))
		}
	}

	return errors.Join(errs...)
}

// ModelByID looks up an enabled configured model.
func (c *Config) ModelByID(id string) (ModelConfig, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return ModelConfig{}, false
}

// Durations helpers.
func (s StorageConfig) FileTTL() time.Duration { return time.Duration(s.FileTTLSeconds) * time.Second }
func (s StorageConfig) RunTTL() time.Duration  { return time.Duration(s.RunTTLSeconds) * time.Second }
func (s StorageConfig) SessionTTL() time.Duration {
	return time.Duration(s.SessionTTLSeconds) * time.Second
}
func (s StorageConfig) CleanupInterval() time.Duration {
	return time.Duration(s.CleanupIntervalSeconds) * time.Second
}
func (l LimitsConfig) QueueWaitTimeout() time.Duration {
	return time.Duration(l.QueueWaitTimeoutSeconds) * time.Second
}
func (l LimitsConfig) SessionLockTimeout() time.Duration {
	return time.Duration(l.SessionLockTimeoutSeconds) * time.Second
}
