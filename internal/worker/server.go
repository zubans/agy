package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cli-ai-gateway/internal/executor"
)

const (
	defaultWorkerListen    = ":9090"
	defaultMaxUploadBytes  = 100 * 1024 * 1024
	defaultShutdownTimeout = 30 * time.Second
	maxMetaPartBytes       = 1 * 1024 * 1024
)

// ServerConfig configures an isolated CLI worker HTTP service running inside a dedicated container.
type ServerConfig struct {
	Listen          string
	Token           string
	Provider        string
	Executable      string
	Executor        executor.Executor
	MaxUploadBytes  int64
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
}

// Server exposes a stateless single-agent worker API authenticated by Bearer token.
type Server struct {
	cfg       ServerConfig
	tokenHash [32]byte
	handler   http.Handler
}

// NewServer validates ServerConfig and constructs a Worker HTTP server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Executor == nil {
		return nil, errors.New("worker server requires non-nil executor")
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, errors.New("worker server requires GATEWAY_WORKER_TOKEN")
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		cfg.Listen = defaultWorkerListen
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = defaultMaxUploadBytes
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if cfg.Provider == "" {
		cfg.Provider = cfg.Executor.Name()
	}

	s := &Server{
		cfg:       cfg,
		tokenHash: sha256.Sum256([]byte(token)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LivezPath, s.handleLivez)
	mux.HandleFunc("GET "+HealthPath, s.withAuth(s.handleHealth))
	mux.HandleFunc("GET "+ModelsPath, s.withAuth(s.handleModels))
	mux.HandleFunc("POST "+ExecutePath, s.withAuth(s.handleExecute))
	s.handler = mux
	return s, nil
}

// Handler returns the HTTP handler for embedding or testing with httptest.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Run starts the HTTP worker server and shuts it down gracefully when ctx cancels.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.cfg.Logger.Info("cli worker listening",
			"addr", s.cfg.Listen,
			"provider", s.cfg.Provider,
			"executable", s.cfg.Executable,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.cfg.Logger.Info("worker shutdown signal received")
		shutCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !constantTimeBearerMatch(r.Header.Get("Authorization"), s.tokenHash) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized worker token"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"provider": s.cfg.Provider,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	err := s.cfg.Executor.Check(r.Context())
	resp := HealthResponse{
		Ready:        err == nil,
		Provider:     s.cfg.Provider,
		Executable:   s.cfg.Executable,
		Capabilities: s.cfg.Executor.Capabilities(),
	}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	resp := ModelsResponse{
		Provider: s.cfg.Provider,
		Models:   []executor.ModelDescriptor{},
	}
	lister, ok := s.cfg.Executor.(executor.ModelLister)
	if !ok {
		resp.Source = "unsupported"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	models, source, err := lister.ListModels(r.Context())
	resp.Source = source
	if models != nil {
		resp.Models = models
	}
	if err != nil {
		resp.Warning = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)

	workDir, err := os.MkdirTemp("", "cli-worker-run-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create worker temp dir failed"})
		return
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	meta, stagedFiles, err := unpackMultipartExecute(r, workDir, s.cfg.MaxUploadBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	timeout := time.Duration(meta.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	if timeout > 24*time.Hour {
		timeout = 24 * time.Hour
	}

	execCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	var writeMu sync.Mutex
	writeFrame := func(frame StreamFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		data, mErr := json.Marshal(frame)
		if mErr != nil {
			return mErr
		}
		data = append(data, '\n')
		if _, wErr := w.Write(data); wErr != nil {
			return wErr
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	execReq := executor.ExecuteRequest{
		RunID:         meta.RunID,
		Model:         meta.Model,
		ProviderModel: meta.ProviderModel,
		WorkDir:       workDir,
		Files:         stagedFiles,
		Instructions:  meta.Instructions,
		Prompt:        meta.Prompt,
		Tools:         meta.Tools,
		ToolChoice:    meta.ToolChoice,
		MCP:           meta.MCP,
		Session: executor.SessionContext{
			GatewaySessionID:  meta.Session.GatewaySessionID,
			ProviderSessionID: meta.Session.ProviderSessionID,
			IsResume:          meta.Session.IsResume,
			HistoryFile:       meta.Session.HistoryFile,
		},
		Settings: executor.ExecutionSettings{
			Timeout:        timeout,
			MaxOutputBytes: meta.MaxOutputBytes,
			Stream:         meta.Stream,
			DiagnosticLog:  meta.DiagnosticLog,
		},
	}

	res, execErr := s.cfg.Executor.Execute(execCtx, execReq, func(ev executor.Event) error {
		evCopy := ev
		return writeFrame(StreamFrame{
			Type:  "event",
			Event: &evCopy,
		})
	})

	_ = os.RemoveAll(workDir)
	doneFrame := encodeDoneFrame(res, execErr)
	_ = writeFrame(doneFrame)
}

func unpackMultipartExecute(r *http.Request, workDir string, maxTotalBytes int64) (ExecuteMeta, []executor.ExecuteFile, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return ExecuteMeta{}, nil, fmt.Errorf("expected multipart/form-data request: %w", err)
	}

	var (
		meta        ExecuteMeta
		hasMeta     bool
		fileMap     = make(map[string]FileMeta)
		stagedByFld = make(map[string]executor.ExecuteFile)
		totalCopied int64
	)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ExecuteMeta{}, nil, fmt.Errorf("read multipart stream: %w", err)
		}

		name := part.FormName()
		if name == "meta" {
			if hasMeta {
				_ = part.Close()
				return ExecuteMeta{}, nil, errors.New("duplicate meta part")
			}
			rawMeta, rErr := io.ReadAll(io.LimitReader(part, maxMetaPartBytes+1))
			_ = part.Close()
			if rErr != nil {
				return ExecuteMeta{}, nil, fmt.Errorf("read meta part: %w", rErr)
			}
			if int64(len(rawMeta)) > maxMetaPartBytes {
				return ExecuteMeta{}, nil, errors.New("meta part exceeds size limit")
			}
			if jErr := json.Unmarshal(rawMeta, &meta); jErr != nil {
				return ExecuteMeta{}, nil, fmt.Errorf("invalid meta JSON: %w", jErr)
			}
			for _, fm := range meta.Files {
				if fm.FieldName == "" {
					return ExecuteMeta{}, nil, errors.New("file metadata missing field_name")
				}
				if _, dup := fileMap[fm.FieldName]; dup {
					return ExecuteMeta{}, nil, fmt.Errorf("duplicate file field_name %q", fm.FieldName)
				}
				fileMap[fm.FieldName] = fm
			}
			hasMeta = true
			continue
		}

		if !hasMeta {
			_ = part.Close()
			return ExecuteMeta{}, nil, errors.New("meta multipart field must precede file parts")
		}

		fm, ok := fileMap[name]
		if !ok {
			_ = part.Close()
			return ExecuteMeta{}, nil, fmt.Errorf("unexpected file field %q", name)
		}
		if _, dup := stagedByFld[name]; dup {
			_ = part.Close()
			return ExecuteMeta{}, nil, fmt.Errorf("duplicate file field %q", name)
		}

		cleanRel, pErr := CleanRelativePath(fm.RelativePath)
		if pErr != nil {
			_ = part.Close()
			return ExecuteMeta{}, nil, pErr
		}

		destPath := filepath.Join(workDir, filepath.FromSlash(cleanRel))
		if mkErr := os.MkdirAll(filepath.Dir(destPath), 0o700); mkErr != nil {
			_ = part.Close()
			return ExecuteMeta{}, nil, fmt.Errorf("create input parent dir: %w", mkErr)
		}

		remaining := maxTotalBytes - totalCopied
		written, wErr := writePartToFile(part, destPath, remaining)
		_ = part.Close()
		if wErr != nil {
			return ExecuteMeta{}, nil, wErr
		}
		totalCopied += written

		stagedByFld[name] = executor.ExecuteFile{
			ID:           fm.ID,
			Name:         fm.Name,
			Path:         destPath,
			RelativePath: fm.RelativePath,
			Size:         written,
			MediaKind:    fm.MediaKind,
			ContentType:  fm.ContentType,
		}
	}

	if !hasMeta {
		return ExecuteMeta{}, nil, errors.New("missing required meta multipart field")
	}
	stagedFiles := make([]executor.ExecuteFile, 0, len(meta.Files))
	for _, fm := range meta.Files {
		f, ok := stagedByFld[fm.FieldName]
		if !ok {
			return ExecuteMeta{}, nil, fmt.Errorf("missing uploaded file part %q (%s)", fm.FieldName, fm.RelativePath)
		}
		stagedFiles = append(stagedFiles, f)
	}
	return meta, stagedFiles, nil
}

func writePartToFile(part *multipart.Part, destPath string, maxBytes int64) (int64, error) {
	if maxBytes < 0 {
		return 0, errors.New("uploaded workspace exceeds max upload limit")
	}
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create staged file: %w", err)
	}
	defer func() { _ = out.Close() }()

	n, err := io.Copy(out, io.LimitReader(part, maxBytes+1))
	if err != nil {
		return 0, fmt.Errorf("write staged file: %w", err)
	}
	if n > maxBytes {
		return 0, errors.New("uploaded workspace exceeds max upload limit")
	}
	return n, nil
}

func encodeDoneFrame(res executor.ExecuteResult, execErr error) StreamFrame {
	payload := &ResultPayload{
		Answer:            res.Answer,
		ProviderSessionID: res.ProviderSessionID,
		Status:            res.Status,
		FinishReason:      res.FinishReason,
		ToolCalls:         res.ToolCalls,
		GeneratedFiles:    res.GeneratedFiles,
		Usage:             res.Usage,
		ExitCode:          res.ExitCode,
	}
	if res.Error != nil {
		payload.Error = &ErrorPayload{
			Kind:              res.Error.Kind,
			Code:              res.Error.Code,
			Message:           res.Error.Message,
			ExitCode:          res.Error.ExitCode,
			RetryAfterSeconds: res.Error.RetryAfterSeconds,
			CooldownSource:    res.Error.CooldownSource,
		}
	} else if execErr != nil {
		var ee *executor.ExecError
		if errors.As(execErr, &ee) && ee != nil {
			payload.Error = &ErrorPayload{
				Kind:              ee.Kind,
				Code:              ee.Code,
				Message:           ee.Message,
				ExitCode:          ee.ExitCode,
				RetryAfterSeconds: ee.RetryAfterSeconds,
				CooldownSource:    ee.CooldownSource,
			}
		} else {
			return StreamFrame{
				Type:   "done",
				Result: payload,
				Error:  execErr.Error(),
			}
		}
	}
	return StreamFrame{
		Type:   "done",
		Result: payload,
	}
}
