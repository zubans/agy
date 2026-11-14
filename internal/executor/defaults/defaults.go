package defaults

import (
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/antigravity"
	"cli-ai-gateway/internal/executor/claude"
	"cli-ai-gateway/internal/executor/codex"
	"cli-ai-gateway/internal/executor/gemini"
	"cli-ai-gateway/internal/executor/ollama"
	"cli-ai-gateway/internal/process"
)

// RegisterAll registers all built-in CLI and local engine adapters on registry.
func RegisterAll(registry *executor.Registry, procMgr *process.Manager) {
	registry.RegisterFactory("codex", func(m config.ModelConfig) (executor.Executor, error) {
		return codex.New(m, procMgr), nil
	})
	registry.RegisterFactory("antigravity", func(m config.ModelConfig) (executor.Executor, error) {
		return antigravity.New(m, procMgr), nil
	})
	registry.RegisterFactory("gemini", func(m config.ModelConfig) (executor.Executor, error) {
		return gemini.New(m, procMgr), nil
	})
	registry.RegisterFactory("claude", func(m config.ModelConfig) (executor.Executor, error) {
		return claude.New(m, procMgr), nil
	})
	registry.RegisterFactory("ollama", func(m config.ModelConfig) (executor.Executor, error) {
		return ollama.New(m), nil
	})
}
