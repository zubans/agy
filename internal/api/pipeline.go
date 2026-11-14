package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
)

// Runner coordinates validation, locking, file staging, concurrency control, and execution
// across both /v1/responses and /v1/chat/completions without duplicating pipeline logic.
type Runner struct {
	cfg      *config.Config
	registry *executor.Registry
	files    *storage.FileStore
	sessions *session.Manager
	procMgr  *process.Manager
}

func NewRunner(cfg *config.Config, registry *executor.Registry, files *storage.FileStore, sessions *session.Manager, procMgr *process.Manager) *Runner {
	return &Runner{
		cfg:      cfg,
		registry: registry,
		files:    files,
		sessions: sessions,
		procMgr:  procMgr,
	}
}

type PipelineInput struct {
	Principal          auth.Principal
	ModelID            string
	PreviousResponseID string
	Instructions       string
	FileIDs            []string
	// InlinePromptMD is written to ./input/prompt.md when no uploaded file is named prompt.md.
	InlinePromptMD string
	// PromptNote carries short inline text when prompt.md is already supplied via input_file.
	PromptNote    string
	HistoryEntry  string
	Stream        bool
	OnStreamReady func(run session.RunRecord, prevRespID *string) error
	OnDelta       func(delta string) error
}

type PipelineOutput struct {
	Run    session.RunRecord
	Result executor.ExecuteResult
}

// Execute runs the full pre-flight -> workspace staging -> executor pipeline.
// If an error happens BEFORE OnStreamReady is called, it writes the appropriate JSON HTTP error
// and returns (PipelineOutput{}, false, nil) or returns an error when post-stream failure occurs.
func (rn *Runner) Execute(ctx context.Context, w http.ResponseWriter, in PipelineInput) (PipelineOutput, bool, error) {
	audit := AuditFromContext(ctx)

	modelInfo, ok := rn.registry.Get(in.ModelID)
	if !ok {
		p := "model"
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("Model %q not found", in.ModelID), &p)
		audit.SetErrorType("model_not_found")
		return PipelineOutput{}, false, nil
	}
	audit.SetModel(modelInfo.Config.ID, modelInfo.Config.Provider)

	if !in.Principal.AllowsModel(modelInfo.Config.ID) {
		p := "model"
		writeError(w, http.StatusForbidden, "permission_error", "model_not_allowed", fmt.Sprintf("API key is not permitted to use model %q", in.ModelID), &p)
		audit.SetErrorType("model_not_allowed")
		return PipelineOutput{}, false, nil
	}

	if err := modelInfo.Executor.Check(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "executor_unavailable", fmt.Sprintf("Executor for model %q is unavailable", in.ModelID), nil)
		audit.SetErrorType("executor_unavailable")
		return PipelineOutput{}, false, nil
	}

	if len(in.FileIDs) > rn.cfg.Limits.MaxFilesPerTask {
		writeInvalidRequest(w, "too_many_files", fmt.Sprintf("Maximum %d files allowed per request", rn.cfg.Limits.MaxFilesPerTask), "input")
		audit.SetErrorType("too_many_files")
		return PipelineOutput{}, false, nil
	}

	// Acquire files before taking session lock so invalid/forbidden file_ids fail fast.
	fileRecords, err := rn.files.AcquireForRun(ctx, in.Principal.OwnerID, in.FileIDs)
	if err != nil {
		mapStorageError(w, err)
		audit.SetErrorType("invalid_file")
		return PipelineOutput{}, false, nil
	}
	defer rn.files.ReleaseForRun(context.Background(), in.FileIDs)

	var totalFileBytes int64
	for _, fr := range fileRecords {
		totalFileBytes += fr.Bytes
	}

	sess, isContinuation, err := rn.sessions.ResolveOrCreateSession(ctx, in.Principal.OwnerID, modelInfo.Config.ID, modelInfo.Config.Provider, in.PreviousResponseID)
	if err != nil {
		mapSessionError(w, err)
		audit.SetErrorType("session_error")
		return PipelineOutput{}, false, nil
	}

	unlockSession, err := rn.sessions.AcquireSessionLock(ctx, sess.ID)
	if err != nil {
		mapSessionError(w, err)
		audit.SetErrorType("session_conflict")
		return PipelineOutput{}, false, nil
	}
	defer unlockSession()

	// Acquire process concurrency slot before starting SSE so queue rejections return HTTP 429 JSON.
	lease, err := rn.procMgr.Acquire(ctx, modelInfo.Config.ID, in.Principal.OwnerID, modelInfo.Config.MaxConcurrent, in.Principal.MaxConcurrent)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			audit.SetErrorType("request_cancelled")
			return PipelineOutput{}, false, nil
		}
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "queue_limit_exceeded", err.Error(), nil)
		audit.SetErrorType("queue_limit_exceeded")
		return PipelineOutput{}, false, nil
	}
	defer lease.Release()

	run, err := rn.sessions.CreateRun(ctx, sess, in.PreviousResponseID, modelInfo.Config.ID, modelInfo.Config.Provider, totalFileBytes, in.FileIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "run_create_error", "Failed to initialize execution workspace", nil)
		audit.SetErrorType("run_create_error")
		return PipelineOutput{}, false, nil
	}

	stagedFiles, stagedBytes, promptSummary, err := rn.stageWorkspace(run, fileRecords, in)
	if err != nil {
		_ = rn.sessions.FinishRun(context.Background(), sess, run, promptSummary, executor.ExecuteResult{
			Status: "failed",
			Error:  &executor.ExecError{Kind: executor.ErrExecution, Code: "stage_error", Message: err.Error()},
		}, 0)
		writeError(w, http.StatusInternalServerError, "server_error", "workspace_stage_error", "Failed to prepare workspace files", nil)
		audit.SetErrorType("workspace_stage_error")
		return PipelineOutput{}, false, nil
	}
	audit.SetInputBytes(stagedBytes)

	sessCtx, historyFile, err := rn.sessions.BuildSessionContext(sess, isContinuation, modelInfo.Config.Provider, modelInfo.Capabilities, run)
	if err != nil {
		_ = rn.sessions.FinishRun(context.Background(), sess, run, promptSummary, executor.ExecuteResult{
			Status: "failed",
			Error:  &executor.ExecError{Kind: executor.ErrExecution, Code: "session_context_error", Message: err.Error()},
		}, 0)
		writeError(w, http.StatusInternalServerError, "server_error", "session_context_error", "Failed to prepare session history", nil)
		audit.SetErrorType("session_context_error")
		return PipelineOutput{}, false, nil
	}
	if historyFile != nil {
		stagedFiles = append(stagedFiles, *historyFile)
	}

	if in.Stream && in.OnStreamReady != nil {
		var prevPtr *string
		if in.PreviousResponseID != "" {
			p := in.PreviousResponseID
			prevPtr = &p
		}
		if err := in.OnStreamReady(run, prevPtr); err != nil {
			_ = rn.sessions.FinishRun(context.Background(), sess, run, promptSummary, executor.ExecuteResult{
				Status: "cancelled",
				Error:  &executor.ExecError{Kind: executor.ErrCancelled, Code: "stream_init_failed", Message: err.Error()},
			}, 0)
			return PipelineOutput{}, true, err
		}
	}

	timeout := time.Duration(modelInfo.Config.TimeoutSeconds) * time.Second
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	rn.sessions.RegisterCancel(run.ID, cancel)
	defer func() {
		rn.sessions.UnregisterCancel(run.ID)
		cancel()
	}()

	start := time.Now()
	execReq := executor.ExecuteRequest{
		RunID:         run.ID,
		Model:         modelInfo.Config.ID,
		ProviderModel: modelInfo.Config.Model,
		WorkDir:       run.Dir,
		Files:         stagedFiles,
		Instructions:  in.Instructions,
		Prompt:        in.PromptNote,
		Session:       sessCtx,
		Settings: executor.ExecutionSettings{
			Timeout:        timeout,
			MaxOutputBytes: rn.cfg.Limits.MaxOutputBytes,
			Stream:         in.Stream,
			DiagnosticLog:  rn.cfg.Logging.Diagnostic,
		},
	}

	res, execErr := modelInfo.Executor.Execute(execCtx, execReq, func(ev executor.Event) error {
		rn.sessions.AppendEvent(run, ev)
		if ev.Type == executor.EventTextDelta && in.OnDelta != nil {
			return in.OnDelta(ev.Text)
		}
		return nil
	})
	duration := time.Since(start)

	var errType string
	if res.Error != nil {
		errType = res.Error.Code
	} else if execErr != nil {
		errType = "cli_execution_error"
	}
	audit.SetResult(duration.Milliseconds(), res.ExitCode, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens, errType)

	// Persist metadata & run artifacts even if client disconnected mid-request.
	_ = rn.sessions.FinishRun(context.Background(), sess, run, promptSummary, res, duration)

	if execErr != nil {
		if !in.Stream {
			status, eType, code, msg := mapExecutorHTTPStatus(execErr)
			writeError(w, status, eType, code, msg, nil)
			return PipelineOutput{}, false, nil
		}
		return PipelineOutput{Run: run, Result: res}, true, execErr
	}

	return PipelineOutput{Run: run, Result: res}, true, nil
}

func (rn *Runner) stageWorkspace(run session.RunRecord, files []storage.FileRecord, in PipelineInput) ([]executor.ExecuteFile, int64, string, error) {
	usedNames := make(map[string]int)
	staged := make([]executor.ExecuteFile, 0, len(files)+1)
	var totalBytes int64
	hasPromptMD := false

	for _, fr := range files {
		name := fr.Filename
		if count := usedNames[name]; count > 0 {
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			name = fmt.Sprintf("%s_%d%s", stem, count+1, ext)
		}
		usedNames[fr.Filename]++
		if name == "prompt.md" {
			hasPromptMD = true
		}

		destPath := filepath.Join(run.InputDir, name)
		if err := rn.files.CopyToWorkspace(fr, destPath); err != nil {
			return nil, 0, "", err
		}
		totalBytes += fr.Bytes
		staged = append(staged, executor.ExecuteFile{
			ID:           fr.ID,
			Name:         name,
			Path:         destPath,
			RelativePath: "./input/" + name,
			Size:         fr.Bytes,
		})
	}

	if !hasPromptMD && strings.TrimSpace(in.InlinePromptMD) != "" {
		promptPath := filepath.Join(run.InputDir, "prompt.md")
		data := []byte(in.InlinePromptMD)
		if err := os.WriteFile(promptPath, data, 0o600); err != nil {
			return nil, 0, "", err
		}
		totalBytes += int64(len(data))
		staged = append(staged, executor.ExecuteFile{
			Name:         "prompt.md",
			Path:         promptPath,
			RelativePath: "./input/prompt.md",
			Size:         int64(len(data)),
		})
	}

	summary := in.HistoryEntry
	if summary == "" {
		summary = in.PromptNote
	}
	if summary == "" && len(files) > 0 {
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, fmt.Sprintf("%s (%s)", f.Filename, f.ID))
		}
		summary = "Files: " + strings.Join(names, ", ")
	}
	return staged, totalBytes, summary, nil
}
