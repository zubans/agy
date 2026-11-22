package api

import (
	"log/slog"
	"net/http"
	"time"

	"cli-ai-gateway/internal/admin"
	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/mcpserver"
	"cli-ai-gateway/internal/pools"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
	"cli-ai-gateway/internal/tools"
)

// Dependencies bundles all services required to construct the HTTP router.
type Dependencies struct {
	Config    *config.Config
	StartedAt time.Time
	Logger    *slog.Logger
	DB        *storage.DB
	Auth      *auth.Store
	Files     *storage.FileStore
	Sessions  *session.Manager
	Registry  *executor.Registry
	Pools     *pools.Store
	Tools     *tools.Registry
	ProcMgr   *process.Manager
}

// NewRouter builds the net/http ServeMux with authentication, logging, and all public endpoints.
func NewRouter(deps Dependencies) http.Handler {
	if deps.Pools == nil && deps.DB != nil {
		deps.Pools = pools.NewStore(deps.DB.SQL())
	}
	if deps.Tools == nil {
		var bStore *tools.BackendStore
		if deps.DB != nil {
			bStore = tools.NewBackendStore(deps.DB.SQL())
		}
		deps.Tools = tools.NewRegistry(deps.Files, bStore)
	}
	mcpSrv := mcpserver.NewServer(deps.Tools)

	mux := http.NewServeMux()

	health := NewHealthHandler(deps.DB, deps.Files, deps.Registry)
	mux.HandleFunc("GET /health", health.Health)
	mux.HandleFunc("GET /ready", health.Ready)

	// Scoped per-run MCP server endpoint for CLI workers (protected by ephemeral per-run Bearer token).
	mux.HandleFunc("POST /internal/mcp/{run_id}", mcpSrv.HandleHTTP)

	models := NewModelsHandler(deps.Registry, deps.Pools)
	files := NewFilesHandler(deps.Files, deps.Config.Storage.MaxFileBytes)
	runner := NewRunner(deps.Config, deps.Registry, deps.Pools, deps.Files, deps.Sessions, deps.ProcMgr)
	runner.SetTools(deps.Tools, mcpSrv)
	deps.Tools.SetModelInvoker(runner.InvokePoolByCapability)

	responses := NewResponsesHandler(runner)
	chat := NewChatHandler(runner)
	media := NewMediaHandler(deps.Files, deps.Tools)

	v1Mux := http.NewServeMux()
	v1Mux.HandleFunc("GET /v1/models", models.List)
	v1Mux.HandleFunc("GET /v1/executors", models.Capabilities)
	v1Mux.HandleFunc("POST /v1/files", files.Upload)
	v1Mux.HandleFunc("GET /v1/files/{file_id}", files.Get)
	v1Mux.HandleFunc("GET /v1/files/{file_id}/content", files.Content)
	v1Mux.HandleFunc("DELETE /v1/files/{file_id}", files.Delete)
	v1Mux.HandleFunc("POST /v1/responses", responses.Create)
	v1Mux.HandleFunc("POST /v1/chat/completions", chat.Create)
	v1Mux.HandleFunc("POST /v1/audio/transcriptions", media.AudioTranscriptions)
	v1Mux.HandleFunc("POST /v1/audio/speech", media.AudioSpeech)
	v1Mux.HandleFunc("POST /v1/images/generations", media.ImageGenerations)

	mux.Handle("/v1/", RequireAuth(deps.Auth, v1Mux))

	if deps.Config.Admin.Enabled {
		adminHandler := admin.NewHandler(admin.Dependencies{
			Config:     deps.Config,
			StartedAt:  deps.StartedAt,
			DB:         deps.DB,
			Auth:       deps.Auth,
			Files:      deps.Files,
			Sessions:   deps.Sessions,
			Registry:   deps.Registry,
			Pools:      deps.Pools,
			Tools:      deps.Tools,
			ProcMgr:    deps.ProcMgr,
			Playground: runner,
		})
		adminHandler.RegisterRoutes(mux)
	}

	return LoggingAndRecoveryMiddleware(deps.Logger, deps.Config.Logging.Diagnostic, mux)
}
