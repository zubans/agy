package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cli-ai-gateway/internal/api"
	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/defaults"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
	"cli-ai-gateway/internal/worker"
)

func main() {
	var configPath string
	var doHealthcheck bool
	var workerMode bool
	flag.StringVar(&configPath, "config", os.Getenv("GATEWAY_CONFIG"), "Path to YAML configuration file")
	flag.BoolVar(&doHealthcheck, "healthcheck", false, "Probe local /health endpoint and exit")
	flag.BoolVar(&workerMode, "worker", strings.EqualFold(os.Getenv("GATEWAY_MODE"), "worker"), "Run isolated single-CLI worker HTTP agent")
	flag.Parse()

	if doHealthcheck {
		os.Exit(runHealthcheck(configPath))
	}
	if workerMode {
		os.Exit(runWorker())
	}

	if configPath == "" {
		configPath = "configs/config.example.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(1)
	}

	logger := newJSONLogger(cfg.Logging.Level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := storage.OpenSQLite(ctx, cfg.Storage.SQLitePath)
	if err != nil {
		logger.Error("failed to initialize sqlite", "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	authStore, err := auth.NewStore(ctx, db, cfg.Auth.Keys)
	if err != nil {
		logger.Error("failed to initialize auth store", "error", err)
		os.Exit(1)
	}

	fileStore, err := storage.NewFileStore(db, cfg.Storage.Path, cfg.Storage.MaxFileBytes, cfg.Storage.QuotaBytes, cfg.Storage.FileTTL())
	if err != nil {
		logger.Error("failed to initialize file store", "error", err)
		os.Exit(1)
	}

	sessionMgr, err := session.NewManager(
		db,
		cfg.Storage.Path,
		cfg.Limits.SessionLockTimeout(),
		cfg.Storage.RunTTL(),
		cfg.Storage.SessionTTL(),
		cfg.Storage.EffectiveCleanupRunInput(),
	)
	if err != nil {
		logger.Error("failed to initialize session manager", "error", err)
		os.Exit(1)
	}

	procMgr := process.NewManager(process.Limits{
		MaxGlobal:        cfg.Limits.MaxGlobalProcesses,
		MaxPerExecutor:   cfg.Limits.MaxExecutorProcesses,
		MaxPerUser:       cfg.Limits.MaxUserProcesses,
		MaxQueueSize:     cfg.Limits.MaxQueueSize,
		QueueWaitTimeout: cfg.Limits.QueueWaitTimeout(),
		DefaultMaxOutput: cfg.Limits.MaxOutputBytes,
	})

	registry := executor.NewRegistry()
	defaults.RegisterAll(registry, procMgr)
	if err := registry.RegisterModels(cfg.Models); err != nil {
		logger.Error("failed to register models", "error", err)
		os.Exit(1)
	}
	if err := registry.ApplyOverrides(ctx, db.SQL()); err != nil {
		logger.Error("failed to apply model overrides", "error", err)
		os.Exit(1)
	}

	// Startup executor diagnostics (non-fatal if individual CLIs are missing, Section 6.9 & 10).
	report := registry.CheckAll(ctx)
	for modelID, st := range report.Executors {
		if st.Ready {
			logger.Info("executor ready", "model", modelID, "provider", st.Provider)
		} else {
			logger.Warn("executor unavailable at startup", "model", modelID, "provider", st.Provider, "error", st.Error)
		}
	}

	// Background storage retention cleanup loop (Section 13.3).
	go runCleanupLoop(ctx, logger, fileStore, sessionMgr, cfg.Storage.CleanupInterval())

	router := api.NewRouter(api.Dependencies{
		Config:    cfg,
		StartedAt: time.Now(),
		Logger:    logger,
		DB:        db,
		Auth:      authStore,
		Files:     fileStore,
		Sessions:  sessionMgr,
		Registry:  registry,
		ProcMgr:   procMgr,
	})

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           router,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeoutSeconds) * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("cli-ai-gateway listening", "addr", cfg.Server.Listen, "models", len(cfg.Models))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("http server error", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Server.ShutdownTimeoutSeconds)*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}
}

func runCleanupLoop(ctx context.Context, logger *slog.Logger, files *storage.FileStore, sessions *session.Manager, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fRemoved, err := files.CleanupExpired(ctx)
			if err != nil {
				logger.Warn("file cleanup failed", "error", err)
			}
			sRemoved, err := sessions.CleanupExpired(ctx)
			if err != nil {
				logger.Warn("session/run cleanup failed", "error", err)
			}
			if fRemoved > 0 || sRemoved > 0 {
				logger.Info("storage cleanup completed", "expired_files_removed", fRemoved, "expired_sessions_runs_removed", sRemoved)
			}
		}
	}
}

func newJSONLogger(levelStr string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func runHealthcheck(configPath string) int {
	target := strings.TrimSpace(os.Getenv("GATEWAY_HEALTHCHECK_URL"))
	if target == "" {
		listenAddr := strings.TrimSpace(os.Getenv("GATEWAY_LISTEN"))
		if listenAddr == "" && configPath != "" {
			if cfg, err := config.Load(configPath); err == nil {
				listenAddr = cfg.Server.Listen
			}
		}
		if listenAddr == "" || strings.HasPrefix(listenAddr, ":") || strings.HasPrefix(listenAddr, "0.0.0.0:") {
			port := "8080"
			if idx := strings.LastIndex(listenAddr, ":"); idx >= 0 && idx+1 < len(listenAddr) {
				port = listenAddr[idx+1:]
			}
			listenAddr = "127.0.0.1:" + port
		}
		target = "http://" + listenAddr + "/health"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck request error: %v\n", err)
		return 1
	}
	if tok := strings.TrimSpace(os.Getenv("GATEWAY_WORKER_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck error: %v\n", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck status: %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func runWorker() int {
	logger := newJSONLogger(os.Getenv("GATEWAY_LOG_LEVEL"))
	provider := strings.TrimSpace(os.Getenv("GATEWAY_WORKER_PROVIDER"))
	if provider == "" {
		logger.Error("GATEWAY_WORKER_PROVIDER is required in worker mode")
		return 1
	}

	executable := strings.TrimSpace(os.Getenv("GATEWAY_WORKER_EXECUTABLE"))
	if executable == "" {
		switch provider {
		case "antigravity":
			executable = "agy"
		default:
			executable = provider
		}
	}

	homeDir := strings.TrimSpace(os.Getenv("GATEWAY_WORKER_HOME"))
	if homeDir == "" {
		homeDir = os.Getenv("HOME")
	}

	fsAccess := strings.TrimSpace(os.Getenv("GATEWAY_WORKER_FS_ACCESS"))
	if fsAccess == "" {
		fsAccess = "workspace-read"
	}

	var allowedTools []string
	if raw := strings.TrimSpace(os.Getenv("GATEWAY_WORKER_ALLOWED_TOOLS")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(part); t != "" {
				allowedTools = append(allowedTools, t)
			}
		}
	}

	allowCommands := strings.EqualFold(os.Getenv("GATEWAY_WORKER_ALLOW_COMMANDS"), "true") || os.Getenv("GATEWAY_WORKER_ALLOW_COMMANDS") == "1"

	procMgr := process.NewManager(process.Limits{
		MaxGlobal:        16,
		MaxPerExecutor:   8,
		MaxPerUser:       8,
		MaxQueueSize:     32,
		QueueWaitTimeout: 30 * time.Second,
		DefaultMaxOutput: 16 * 1024 * 1024,
	})

	modelCfg := config.ModelConfig{
		ID:            provider,
		Provider:      provider,
		Executable:    executable,
		HomeDir:       homeDir,
		SandboxMode:   strings.TrimSpace(os.Getenv("GATEWAY_WORKER_SANDBOX_MODE")),
		AllowedTools:  allowedTools,
		AllowCommands: allowCommands,
		FSAccess:      fsAccess,
	}

	exec, err := defaults.NewLocalExecutor(modelCfg, procMgr)
	if err != nil {
		logger.Error("worker executor initialization failed", "error", err)
		return 1
	}

	srv, err := worker.NewServer(worker.ServerConfig{
		Listen:     os.Getenv("GATEWAY_WORKER_LISTEN"),
		Token:      os.Getenv("GATEWAY_WORKER_TOKEN"),
		Provider:   provider,
		Executable: executable,
		Executor:   exec,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("worker server initialization failed", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		logger.Error("worker server failed", "error", err)
		return 1
	}
	return 0
}
