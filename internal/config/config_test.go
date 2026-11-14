package config_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cli-ai-gateway/internal/config"
)

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestParseAndLoadDefaults(t *testing.T) {
	yamlCfg := `
auth:
  keys:
    - id: dev-key
      owner_id: alice
      key: secret-token-1
models:
  - id: codex-cli
    provider: codex
    executable: /usr/local/bin/codex
  - id: local-ollama
    provider: ollama
    base_url: http://127.0.0.1:11434
    model: qwen2.5-coder:7b
`
	tmp := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmp, []byte(yamlCfg), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := config.Load(tmp)
	if err != nil {
		t.Fatalf("Load valid config failed: %v", err)
	}

	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("unexpected default listen: %q", cfg.Server.Listen)
	}
	if cfg.Storage.MaxFileBytes != 50*1024*1024 {
		t.Fatalf("unexpected default MaxFileBytes: %d", cfg.Storage.MaxFileBytes)
	}
	if !cfg.Storage.EffectiveCleanupRunInput() {
		t.Fatalf("expected CleanupRunInput to default to true")
	}
	if cfg.Limits.MaxFilesPerTask != 10 {
		t.Fatalf("unexpected default MaxFilesPerTask: %d", cfg.Limits.MaxFilesPerTask)
	}
	if cfg.Auth.Keys[0].EffectiveHash() != shaHex("secret-token-1") {
		t.Fatalf("unexpected key hash")
	}
	if !cfg.Models[0].EffectiveEnabled() {
		t.Fatalf("expected model to be enabled by default")
	}
	if cfg.Models[0].FSAccess != "workspace-read" {
		t.Fatalf("unexpected default fs_access: %q", cfg.Models[0].FSAccess)
	}
}

func TestEnvOverrides(t *testing.T) {
	yamlCfg := `
server:
  listen: 127.0.0.1:8080
storage:
  path: /tmp/original
models:
  - id: ollama-1
    provider: ollama
    base_url: http://127.0.0.1:11434
    model: llama3.2
`
	t.Setenv("GATEWAY_LISTEN", "127.0.0.1:9090")
	t.Setenv("GATEWAY_STORAGE_PATH", "/tmp/override-data")
	t.Setenv("GATEWAY_LOG_LEVEL", "debug")
	t.Setenv("GATEWAY_DIAGNOSTIC_LOG", "true")

	cfg, err := config.Parse([]byte(yamlCfg))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if cfg.Server.Listen != "127.0.0.1:9090" {
		t.Fatalf("listen override not applied: %q", cfg.Server.Listen)
	}
	if cfg.Storage.Path != "/tmp/override-data" {
		t.Fatalf("storage path override not applied: %q", cfg.Storage.Path)
	}
	if cfg.Storage.SQLitePath != "/tmp/override-data/gateway.db" {
		t.Fatalf("sqlite path should follow overridden storage path, got %q", cfg.Storage.SQLitePath)
	}
	if cfg.Logging.Level != "debug" || !cfg.Logging.Diagnostic {
		t.Fatalf("logging overrides not applied: %+v", cfg.Logging)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	yamlCfg := `
server:
  listen: 127.0.0.1:8080
  unknown_field: 123
models:
  - id: ollama-1
    provider: ollama
    base_url: http://127.0.0.1:11434
    model: llama3.2
`
	if _, err := config.Parse([]byte(yamlCfg)); err == nil {
		t.Fatal("expected error for unknown YAML field")
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantMatch string
	}{
		{
			name:      "missing models",
			yaml:      `server: { listen: "127.0.0.1:8080" }`,
			wantMatch: "at least one model",
		},
		{
			name: "duplicate model ids",
			yaml: `
models:
  - id: m1
    provider: codex
    executable: codex
  - id: m1
    provider: gemini
    executable: gemini
`,
			wantMatch: "duplicate model id",
		},
		{
			name: "unsupported provider",
			yaml: `
models:
  - id: m1
    provider: unknown_vendor
    executable: x
`,
			wantMatch: "unsupported provider",
		},
		{
			name: "missing cli executable",
			yaml: `
models:
  - id: m1
    provider: claude
`,
			wantMatch: "executable is required",
		},
		{
			name: "invalid ollama url",
			yaml: `
models:
  - id: m1
    provider: ollama
    base_url: ftp://bad-scheme
    model: qwen
`,
			wantMatch: "invalid base_url",
		},
		{
			name: "invalid timeout exceeds max",
			yaml: `
limits:
  default_timeout_seconds: 10
  max_timeout_seconds: 30
models:
  - id: m1
    provider: codex
    executable: codex
    timeout_seconds: 60
`,
			wantMatch: "out of range",
		},
		{
			name: "duplicate auth hash",
			yaml: `
auth:
  keys:
    - id: k1
      owner_id: alice
      key: same-secret
    - id: k2
      owner_id: bob
      key_sha256: ` + shaHex("same-secret") + `
models:
  - id: m1
    provider: codex
    executable: codex
`,
			wantMatch: "duplicate API key hash",
		},
		{
			name: "bad sha256 hex",
			yaml: `
auth:
  keys:
    - id: k1
      owner_id: alice
      key_sha256: not-64-hex
models:
  - id: m1
    provider: codex
    executable: codex
`,
			wantMatch: "64 hex chars",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.wantMatch)
			}
			if !strings.Contains(err.Error(), tc.wantMatch) {
				t.Fatalf("expected error containing %q, got %v", tc.wantMatch, err)
			}
		})
	}
}

func TestAdminConfigValidationAndEnv(t *testing.T) {
	t.Run("valid token and env overrides", func(t *testing.T) {
		yamlCfg := `
admin:
  enabled: true
  token: yaml-admin-secret
models:
  - id: m1
    provider: codex
    executable: codex
`
		t.Setenv("GATEWAY_ADMIN_TOKEN", "env-admin-secret")
		cfg, err := config.Parse([]byte(yamlCfg))
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if !cfg.Admin.Enabled {
			t.Fatalf("expected admin enabled")
		}
		if cfg.Admin.EffectiveHash() != shaHex("env-admin-secret") {
			t.Fatalf("expected GATEWAY_ADMIN_TOKEN override to take effect")
		}
	})

	t.Run("token_sha256 accepted", func(t *testing.T) {
		yamlCfg := `
admin:
  enabled: true
  token_sha256: ` + shaHex("hashed-admin-secret") + `
models:
  - id: m1
    provider: ollama
    base_url: http://127.0.0.1:11434
    model: qwen3
`
		cfg, err := config.Parse([]byte(yamlCfg))
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if cfg.Admin.EffectiveHash() != shaHex("hashed-admin-secret") {
			t.Fatalf("unexpected EffectiveHash")
		}
	})

	t.Run("enabled via env without credentials rejected", func(t *testing.T) {
		yamlCfg := `
models:
  - id: m1
    provider: codex
    executable: codex
`
		t.Setenv("GATEWAY_ADMIN_ENABLED", "true")
		_, err := config.Parse([]byte(yamlCfg))
		if err == nil || !strings.Contains(err.Error(), "admin.token") {
			t.Fatalf("expected missing admin credentials error, got %v", err)
		}
	})

	t.Run("bad token_sha256 rejected", func(t *testing.T) {
		yamlCfg := `
admin:
  enabled: true
  token_sha256: not-a-hex-digest
models:
  - id: m1
    provider: codex
    executable: codex
`
		_, err := config.Parse([]byte(yamlCfg))
		if err == nil || !strings.Contains(err.Error(), "64 hex chars") {
			t.Fatalf("expected 64 hex chars error, got %v", err)
		}
	})
}
