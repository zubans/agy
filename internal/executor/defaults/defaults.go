package defaults

import (
	"fmt"
	"strings"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/antigravity"
	"cli-ai-gateway/internal/executor/claude"
	"cli-ai-gateway/internal/executor/codex"
	"cli-ai-gateway/internal/executor/gemini"
	"cli-ai-gateway/internal/executor/ollama"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/worker"
)

// RegisterAll registers all built-in CLI, Ollama, and remote-worker adapters on registry.
func RegisterAll(registry *executor.Registry, procMgr *process.Manager) {
	for _, provider := range []string{"codex", "antigravity", "gemini", "claude", "ollama"} {
		p := provider
		registry.RegisterFactory(p, func(m config.ModelConfig) (executor.Executor, error) {
			local, err := NewLocalExecutor(m, procMgr)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(m.WorkerURL) != "" {
				return worker.NewExecutor(m, local.Capabilities(), "")
			}
			return local, nil
		})
	}
}

// NewLocalExecutor builds the direct in-process Executor (CLI adapter or Ollama HTTP adapter) for m.Provider.
func NewLocalExecutor(m config.ModelConfig, procMgr *process.Manager) (executor.Executor, error) {
	switch m.Provider {
	case "codex":
		return codex.New(m, procMgr), nil
	case "antigravity":
		return antigravity.New(m, procMgr), nil
	case "gemini":
		return gemini.New(m, procMgr), nil
	case "claude":
		return claude.New(m, procMgr), nil
	case "ollama":
		return ollama.New(m), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", m.Provider)
	}
}
