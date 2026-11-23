package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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

// Runner coordinates validation, priority target selection, cooldown failover, locking,
// file staging, concurrency control, server/client tools, and execution across /v1/responses and /v1/chat/completions.
type Runner struct {
	cfg      *config.Config
	registry *executor.Registry
	pools    *pools.Store
	files    *storage.FileStore
	sessions *session.Manager
	procMgr  *process.Manager
	tools    *tools.Registry
	mcp      *mcpserver.Server
}

func NewRunner(cfg *config.Config, registry *executor.Registry, poolStore *pools.Store, files *storage.FileStore, sessions *session.Manager, procMgr *process.Manager) *Runner {
	return &Runner{
		cfg:      cfg,
		registry: registry,
		pools:    poolStore,
		files:    files,
		sessions: sessions,
		procMgr:  procMgr,
	}
}

// SetTools attaches the server-side tool registry and per-run MCP server to the Runner.
func (rn *Runner) SetTools(toolReg *tools.Registry, mcpSrv *mcpserver.Server) {
	rn.tools = toolReg
	rn.mcp = mcpSrv
}

// StageInlineMedia saves an inline base64-decoded media payload (from image_url or input_audio)
// into FileStore for the duration of the request.
func (rn *Runner) StageInlineMedia(ctx context.Context, ownerID, filename, purpose string, data []byte) (storage.FileRecord, error) {
	return rn.files.Save(ctx, ownerID, filename, purpose, bytes.NewReader(data))
}

func (rn *Runner) mcpBaseURL() string {
	if u := strings.TrimRight(strings.TrimSpace(os.Getenv("GATEWAY_MCP_BASE_URL")), "/"); u != "" {
		return u
	}
	listen := strings.TrimSpace(rn.cfg.Server.Listen)
	if strings.HasPrefix(listen, ":") {
		return "http://127.0.0.1" + listen
	}
	if strings.HasPrefix(listen, "0.0.0.0:") {
		return "http://127.0.0.1:" + strings.TrimPrefix(listen, "0.0.0.0:")
	}
	if listen == "" {
		return "http://127.0.0.1:7070"
	}
	return "http://" + listen
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
	Tools         []executor.ToolDefinition
	ToolChoice    any
	ServerTools   []string
	Stream        bool
	OnStreamReady func(run session.RunRecord, prevRespID *string) error
	OnDelta       func(delta string) error
}

type PipelineOutput struct {
	Run    session.RunRecord
	Result executor.ExecuteResult
}

// Execute runs pre-flight checks, priority/cooldown target resolution, workspace staging,
// and automatic pre-stream failover across multiple provider targets.
func (rn *Runner) Execute(ctx context.Context, w http.ResponseWriter, in PipelineInput) (PipelineOutput, bool, error) {
	audit := AuditFromContext(ctx)

	targets, isPool, defaultCooldown, handled := rn.resolveTargets(ctx, w, audit, in)
	if handled {
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

	first := targets[0]
	audit.SetModel(in.ModelID, first.Info.Config.Provider)

	sess, isContinuation, err := rn.sessions.ResolveOrCreateSession(ctx, in.Principal.OwnerID, in.ModelID, first.Info.Config.Provider, in.PreviousResponseID)
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

	var (
		existingRespID string
		lastErr        error
		lastHTTPStatus int
		lastErrType    string
		lastErrCode    string
		lastErrMsg     string
		maxRetrySec    int64
	)

	for idx, cand := range targets {
		if ctx.Err() != nil {
			audit.SetErrorType("request_cancelled")
			return PipelineOutput{}, false, nil
		}

		if err := cand.Info.Executor.Check(ctx); err != nil {
			if isPool && rn.pools != nil {
				rn.recordCooldown(cand, "unavailable", err.Error(), 60, "default")
			}
			lastErr = err
			lastHTTPStatus = http.StatusServiceUnavailable
			lastErrType = "server_error"
			lastErrCode = "executor_unavailable"
			lastErrMsg = fmt.Sprintf("Executor for model %q (%s) is unavailable", in.ModelID, cand.ProviderID)
			if idx+1 < len(targets) {
				continue
			}
			break
		}

		out, streamCommitted, retryAfterSec, attemptErr, fatalSetupStatus, fatalSetupCode, fatalSetupMsg := rn.executeAttempt(
			ctx, w, audit, in, sess, isContinuation, fileRecords, totalFileBytes, existingRespID, cand, isPool, defaultCooldown,
		)
		if out.Run.ResponseID != "" {
			existingRespID = out.Run.ResponseID
		}
		if retryAfterSec > maxRetrySec {
			maxRetrySec = retryAfterSec
		}

		if fatalSetupCode != "" {
			if idx+1 < len(targets) && (fatalSetupStatus == http.StatusTooManyRequests || fatalSetupStatus == http.StatusServiceUnavailable) {
				lastHTTPStatus = fatalSetupStatus
				lastErrType = "server_error"
				if fatalSetupStatus == http.StatusTooManyRequests {
					lastErrType = "rate_limit_error"
				}
				lastErrCode = fatalSetupCode
				lastErrMsg = fatalSetupMsg
				continue
			}
			errType := "server_error"
			if fatalSetupStatus == http.StatusTooManyRequests {
				errType = "rate_limit_error"
			}
			writeError(w, fatalSetupStatus, errType, fatalSetupCode, fatalSetupMsg, nil)
			audit.SetErrorType(fatalSetupCode)
			return PipelineOutput{}, false, nil
		}

		if attemptErr == nil {
			if !in.Stream {
				w.Header().Set("X-Gateway-Target-Provider", cand.ProviderID)
				if cand.ProviderModel != "" {
					w.Header().Set("X-Gateway-Target-Model", cand.ProviderModel)
				}
			}
			return out, true, nil
		}

		if streamCommitted {
			return out, true, attemptErr
		}

		lastErr = attemptErr
		lastHTTPStatus, lastErrType, lastErrCode, lastErrMsg = mapExecutorHTTPStatus(attemptErr)

		if idx+1 < len(targets) && ctx.Err() == nil && shouldFailover(attemptErr) {
			continue
		}
		break
	}

	if lastHTTPStatus == 0 && lastErr != nil {
		lastHTTPStatus, lastErrType, lastErrCode, lastErrMsg = mapExecutorHTTPStatus(lastErr)
	}
	if lastHTTPStatus == 0 {
		lastHTTPStatus = http.StatusServiceUnavailable
		lastErrType = "server_error"
		lastErrCode = "executor_unavailable"
		lastErrMsg = fmt.Sprintf("No available executor for model %q", in.ModelID)
	}
	if maxRetrySec > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(maxRetrySec, 10))
	}
	audit.SetErrorType(lastErrCode)
	writeError(w, lastHTTPStatus, lastErrType, lastErrCode, lastErrMsg, nil)
	return PipelineOutput{}, false, nil
}

func (rn *Runner) resolveTargets(ctx context.Context, w http.ResponseWriter, audit *RequestAudit, in PipelineInput) ([]pools.ResolvedTarget, bool, time.Duration, bool) {
	const fallbackCooldown = 300 * time.Second

	if rn.pools != nil {
		pm, err := rn.pools.GetPublishedModel(ctx, in.ModelID)
		if err == nil {
			if !pm.Enabled {
				p := "model"
				writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("Model %q not found", in.ModelID), &p)
				audit.SetErrorType("model_not_found")
				return nil, true, fallbackCooldown, true
			}
			if !in.Principal.AllowsModel(pm.ID) {
				p := "model"
				writeError(w, http.StatusForbidden, "permission_error", "model_not_allowed", fmt.Sprintf("API key is not permitted to use model %q", in.ModelID), &p)
				audit.SetErrorType("model_not_allowed")
				return nil, true, fallbackCooldown, true
			}
			sel, selErr := rn.pools.ResolveCandidates(ctx, rn.registry, pm.ID)
			if selErr != nil {
				writeError(w, http.StatusInternalServerError, "server_error", "pool_resolve_error", "Failed to resolve model providers", nil)
				audit.SetErrorType("pool_resolve_error")
				return nil, true, fallbackCooldown, true
			}
			if len(sel.Candidates) == 0 {
				now := time.Now().Unix()
				if sel.EarliestReset > now {
					retrySec := sel.EarliestReset - now
					w.Header().Set("Retry-After", strconv.FormatInt(retrySec, 10))
					writeError(w, http.StatusServiceUnavailable, "rate_limit_error", "all_providers_cooling",
						fmt.Sprintf("All configured providers for model %q are cooling down; retry in %ds", in.ModelID, retrySec), nil)
					audit.SetErrorType("all_providers_cooling")
					return nil, true, fallbackCooldown, true
				}
				writeError(w, http.StatusServiceUnavailable, "server_error", "executor_unavailable",
					fmt.Sprintf("No enabled providers available for model %q", in.ModelID), nil)
				audit.SetErrorType("executor_unavailable")
				return nil, true, fallbackCooldown, true
			}
			defCD := time.Duration(pm.DefaultCooldownSeconds) * time.Second
			if defCD <= 0 {
				defCD = fallbackCooldown
			}
			return sel.Candidates, true, defCD, false
		} else if !errors.Is(err, pools.ErrModelNotFound) {
			writeError(w, http.StatusInternalServerError, "server_error", "pool_lookup_error", "Failed to inspect model catalog", nil)
			audit.SetErrorType("pool_lookup_error")
			return nil, true, fallbackCooldown, true
		}
	}

	modelInfo, ok := rn.registry.Get(in.ModelID)
	if !ok {
		p := "model"
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("Model %q not found", in.ModelID), &p)
		audit.SetErrorType("model_not_found")
		return nil, false, fallbackCooldown, true
	}
	if !in.Principal.AllowsModel(modelInfo.Config.ID) {
		p := "model"
		writeError(w, http.StatusForbidden, "permission_error", "model_not_allowed", fmt.Sprintf("API key is not permitted to use model %q", in.ModelID), &p)
		audit.SetErrorType("model_not_allowed")
		return nil, false, fallbackCooldown, true
	}

	if rn.pools != nil {
		cooldowns, err := rn.pools.ActiveCooldownMap(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "pool_resolve_error", "Failed to inspect cooldowns", nil)
			audit.SetErrorType("pool_resolve_error")
			return nil, false, fallbackCooldown, true
		}
		if cd, ok := cooldowns[pools.CooldownKey{ProviderID: modelInfo.Config.ID, ProviderModel: modelInfo.Config.Model}]; ok {
			w.Header().Set("Retry-After", strconv.FormatInt(cd.RemainingSeconds, 10))
			writeError(w, http.StatusServiceUnavailable, "rate_limit_error", "all_providers_cooling", "Model is cooling down", nil)
			audit.SetErrorType("all_providers_cooling")
			return nil, false, fallbackCooldown, true
		}
	}

	return []pools.ResolvedTarget{{
		PublishedModelID: modelInfo.Config.ID,
		ProviderID:       modelInfo.Config.ID,
		ProviderModel:    modelInfo.Config.Model,
		Priority:         1,
		Position:         0,
		Info:             modelInfo,
	}}, false, fallbackCooldown, false
}

func (rn *Runner) executeAttempt(
	ctx context.Context,
	w http.ResponseWriter,
	audit *RequestAudit,
	in PipelineInput,
	sess session.Session,
	isContinuation bool,
	fileRecords []storage.FileRecord,
	totalFileBytes int64,
	existingRespID string,
	cand pools.ResolvedTarget,
	isPool bool,
	defaultCooldown time.Duration,
) (
	out PipelineOutput,
	streamCommitted bool,
	retryAfterSec int64,
	execErr error,
	fatalSetupStatus int,
	fatalSetupCode string,
	fatalSetupMsg string,
) {
	lease, err := rn.procMgr.Acquire(ctx, cand.ProviderID, in.Principal.OwnerID, cand.Info.Config.MaxConcurrent, in.Principal.MaxConcurrent)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return PipelineOutput{}, false, 0, nil, 499, "request_cancelled", "Request cancelled"
		}
		return PipelineOutput{}, false, 0, nil, http.StatusTooManyRequests, "queue_limit_exceeded", err.Error()
	}
	defer lease.Release()

	run, err := rn.sessions.CreateRunAttempt(
		ctx, sess, in.PreviousResponseID, existingRespID,
		in.ModelID, cand.Info.Config.Provider, cand.ProviderID, cand.ProviderModel,
		totalFileBytes, in.FileIDs,
	)
	if err != nil {
		return PipelineOutput{}, false, 0, nil, http.StatusInternalServerError, "run_create_error", "Failed to initialize execution workspace"
	}

	stagedFiles, stagedBytes, promptSummary, err := rn.stageWorkspace(run, fileRecords, in)
	if err != nil {
		_ = rn.sessions.FinishRun(context.Background(), sess, run, promptSummary, executor.ExecuteResult{
			Status: "failed",
			Error:  &executor.ExecError{Kind: executor.ErrExecution, Code: "stage_error", Message: err.Error()},
		}, 0)
		return PipelineOutput{}, false, 0, nil, http.StatusInternalServerError, "workspace_stage_error", "Failed to prepare workspace files"
	}
	audit.SetModel(in.ModelID, cand.Info.Config.Provider)
	audit.SetInputBytes(stagedBytes)

	sessCtx, historyFile, err := rn.sessions.BuildSessionContext(sess, isContinuation, cand.Info.Config.Provider, cand.Info.Capabilities, run)
	if err != nil {
		_ = rn.sessions.FinishRun(context.Background(), sess, run, promptSummary, executor.ExecuteResult{
			Status: "failed",
			Error:  &executor.ExecError{Kind: executor.ErrExecution, Code: "session_context_error", Message: err.Error()},
		}, 0)
		return PipelineOutput{}, false, 0, nil, http.StatusInternalServerError, "session_context_error", "Failed to prepare session history"
	}
	if historyFile != nil {
		stagedFiles = append(stagedFiles, *historyFile)
	}

	var prevPtr *string
	if in.PreviousResponseID != "" {
		p := in.PreviousResponseID
		prevPtr = &p
	}

	timeout := time.Duration(cand.Info.Config.TimeoutSeconds) * time.Second
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	rn.sessions.RegisterCancel(run.ID, cancel)
	defer func() {
		rn.sessions.UnregisterCancel(run.ID)
		cancel()
	}()

	hasDeltas := false
	start := time.Now()
	execReq := executor.ExecuteRequest{
		RunID:         run.ID,
		Model:         in.ModelID,
		ProviderModel: cand.ProviderModel,
		WorkDir:       run.Dir,
		Files:         stagedFiles,
		Instructions:  in.Instructions,
		Prompt:        in.PromptNote,
		Tools:         in.Tools,
		ToolChoice:    in.ToolChoice,
		Session:       sessCtx,
		Settings: executor.ExecutionSettings{
			Timeout:        timeout,
			MaxOutputBytes: rn.cfg.Limits.MaxOutputBytes,
			Stream:         in.Stream,
			DiagnosticLog:  rn.cfg.Logging.Diagnostic,
		},
	}

	// Resolve server-side tools bound to this model (or explicitly requested).
	serverToolNames := in.ServerTools
	if len(serverToolNames) == 0 && rn.tools != nil && rn.tools.Store() != nil {
		if bound, bErr := rn.tools.Store().GetModelTools(ctx, in.ModelID); bErr == nil && len(bound) > 0 {
			serverToolNames = bound
		}
	}

	var (
		genMu          sync.Mutex
		generatedFiles []executor.GeneratedFileRef
	)
	runToolCtx := tools.RunContext{
		RunID:         run.ID,
		OwnerID:       in.Principal.OwnerID,
		WorkDir:       run.Dir,
		AttachedFiles: fileRecords,
		OnFileGenerated: func(ref executor.GeneratedFileRef) {
			genMu.Lock()
			generatedFiles = append(generatedFiles, ref)
			genMu.Unlock()
		},
	}

	var mcpSess *mcpserver.Session
	if cand.Info.Config.Provider != "ollama" && rn.mcp != nil && (len(serverToolNames) > 0 || len(in.Tools) > 0) {
		mcpSess = rn.mcp.RegisterSession(run.ID, runToolCtx, serverToolNames, in.Tools, func(_ executor.ToolCall) {
			cancel()
		})
		defer rn.mcp.UnregisterSession(run.ID)
		execReq.MCP = &executor.MCPConfig{
			URL:   rn.mcpBaseURL() + "/internal/mcp/" + run.ID,
			Token: mcpSess.Token,
		}
	}

	emitHandler := func(ev executor.Event) error {
		rn.sessions.AppendEvent(run, ev)
		if ev.Type == executor.EventTextDelta && ev.Text != "" && in.OnDelta != nil {
			if in.Stream && !streamCommitted {
				if in.OnStreamReady != nil {
					w.Header().Set("X-Gateway-Target-Provider", cand.ProviderID)
					if cand.ProviderModel != "" {
						w.Header().Set("X-Gateway-Target-Model", cand.ProviderModel)
					}
					if cbErr := in.OnStreamReady(run, prevPtr); cbErr != nil {
						return cbErr
					}
				}
				streamCommitted = true
			}
			hasDeltas = true
			return in.OnDelta(ev.Text)
		}
		return nil
	}

	var res executor.ExecuteResult
	if cand.Info.Config.Provider == "ollama" && rn.tools != nil && len(serverToolNames) > 0 {
		res, execErr = rn.executeOllamaToolLoop(execCtx, cand, execReq, serverToolNames, runToolCtx, emitHandler)
	} else {
		res, execErr = cand.Info.Executor.Execute(execCtx, execReq, emitHandler)
	}

	if mcpSess != nil {
		if captured := mcpSess.CapturedClientCalls(); len(captured) > 0 {
			res.ToolCalls = captured
			res.FinishReason = "tool_calls"
			res.Status = "completed"
			res.Error = nil
			execErr = nil
		}
	}
	genMu.Lock()
	if len(generatedFiles) > 0 {
		res.GeneratedFiles = append(res.GeneratedFiles, generatedFiles...)
	}
	genMu.Unlock()
	duration := time.Since(start)

	var errType string
	if res.Error != nil {
		errType = res.Error.Code
	} else if execErr != nil {
		errType = "cli_execution_error"
	}
	audit.SetResult(duration.Milliseconds(), res.ExitCode, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens, errType)

	_ = rn.sessions.FinishRun(context.Background(), sess, run, promptSummary, res, duration)

	if execErr != nil {
		var ee *executor.ExecError
		if errors.As(execErr, &ee) && ee != nil && rn.pools != nil {
			switch ee.Kind {
			case executor.ErrRateLimit:
				cdSec := ee.RetryAfterSeconds
				src := ee.CooldownSource
				if cdSec <= 0 {
					cdSec = int64(defaultCooldown.Seconds())
					src = "default"
				}
				if cdSec <= 0 {
					cdSec = 300
				}
				retryAfterSec = cdSec
				rn.recordCooldown(cand, "rate_limit", ee.Message, cdSec, src)
			case executor.ErrAuth:
				if isPool {
					rn.recordCooldown(cand, "auth", ee.Message, 600, "default")
				}
			case executor.ErrUnavailable:
				if isPool {
					rn.recordCooldown(cand, "unavailable", ee.Message, 60, "default")
				}
			}
		}
		return PipelineOutput{Run: run, Result: res}, streamCommitted, retryAfterSec, execErr, 0, "", ""
	}

	if in.Stream && !streamCommitted && in.OnStreamReady != nil {
		w.Header().Set("X-Gateway-Target-Provider", cand.ProviderID)
		if cand.ProviderModel != "" {
			w.Header().Set("X-Gateway-Target-Model", cand.ProviderModel)
		}
		if cbErr := in.OnStreamReady(run, prevPtr); cbErr != nil {
			return PipelineOutput{Run: run, Result: res}, true, 0, cbErr, 0, "", ""
		}
		streamCommitted = true
		if !hasDeltas && res.Answer != "" && in.OnDelta != nil {
			if cbErr := in.OnDelta(res.Answer); cbErr != nil {
				return PipelineOutput{Run: run, Result: res}, true, 0, cbErr, 0, "", ""
			}
		}
	}

	return PipelineOutput{Run: run, Result: res}, streamCommitted, 0, nil, 0, "", ""
}

func (rn *Runner) recordCooldown(cand pools.ResolvedTarget, reason, message string, seconds int64, source string) {
	if rn.pools == nil {
		return
	}
	if seconds <= 0 {
		seconds = 60
	}
	until := time.Now().Add(time.Duration(seconds) * time.Second)
	_ = rn.pools.SetCooldown(context.Background(), cand.ProviderID, cand.ProviderModel, reason, message, until, source)
}

func shouldFailover(err error) bool {
	var ee *executor.ExecError
	if !errors.As(err, &ee) || ee == nil {
		return false
	}
	switch ee.Kind {
	case executor.ErrRateLimit, executor.ErrAuth, executor.ErrUnavailable:
		return true
	default:
		return false
	}
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
			MediaKind:    fr.MediaKind,
			ContentType:  fr.ContentType,
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

// executeOllamaToolLoop runs a server-side agentic loop for Ollama when server tools are bound to the model.
// Client-defined tools are returned immediately to the caller as finish_reason="tool_calls".
func (rn *Runner) executeOllamaToolLoop(
	ctx context.Context,
	cand pools.ResolvedTarget,
	baseReq executor.ExecuteRequest,
	serverToolNames []string,
	runCtx tools.RunContext,
	emit executor.EventHandler,
) (executor.ExecuteResult, error) {
	serverDefs := rn.tools.Definitions(serverToolNames)
	serverSet := make(map[string]bool, len(serverDefs))
	for _, d := range serverDefs {
		serverSet[d.Name] = true
	}

	combinedTools := make([]executor.ToolDefinition, 0, len(serverDefs)+len(baseReq.Tools))
	combinedTools = append(combinedTools, serverDefs...)
	combinedTools = append(combinedTools, baseReq.Tools...)

	req := baseReq
	req.Tools = combinedTools

	const maxSteps = 8
	var totalUsage executor.Usage
	for step := 0; step < maxSteps; step++ {
		res, err := cand.Info.Executor.Execute(ctx, req, emit)
		totalUsage.InputTokens += res.Usage.InputTokens
		totalUsage.OutputTokens += res.Usage.OutputTokens
		totalUsage.TotalTokens += res.Usage.TotalTokens
		if res.Usage.Available {
			totalUsage.Available = true
		}
		if err != nil {
			res.Usage = totalUsage
			return res, err
		}
		if len(res.ToolCalls) == 0 {
			res.Usage = totalUsage
			return res, nil
		}

		// If any call targets a client-defined tool, return them directly to the HTTP caller.
		var clientCalls []executor.ToolCall
		var serverCalls []executor.ToolCall
		for _, tc := range res.ToolCalls {
			if serverSet[tc.Name] {
				serverCalls = append(serverCalls, tc)
			} else {
				clientCalls = append(clientCalls, tc)
			}
		}
		if len(clientCalls) > 0 {
			res.ToolCalls = clientCalls
			res.FinishReason = "tool_calls"
			res.Usage = totalUsage
			return res, nil
		}

		// Execute server tools locally and append results to the prompt for the next turn.
		var sb strings.Builder
		if req.Prompt != "" {
			sb.WriteString(req.Prompt)
			sb.WriteString("\n\n")
		}
		sb.WriteString("<tool_results>\n")
		for _, tc := range serverCalls {
			out, tErr := rn.tools.Execute(ctx, runCtx, tc.Name, json.RawMessage(tc.Arguments))
			text := out.Text
			if text == "" && tErr != nil {
				text = "Error: " + tErr.Error()
			}
			sb.WriteString(fmt.Sprintf("<tool_result name=%q call_id=%q>\n%s\n</tool_result>\n", tc.Name, tc.ID, text))
		}
		sb.WriteString("</tool_results>\nContinue and produce the final answer for the user.")
		req.Prompt = sb.String()
	}

	return executor.ExecuteResult{
		Answer: "Maximum server tool execution steps reached.",
		Status: "completed",
		Usage:  totalUsage,
	}, nil
}

// InvokePoolByCapability lets server tools (image_analyze, image_ocr, audio_transcribe)
// delegate a sub-task to the first available model supporting the requested capability ("image_in" or "audio_in").
func (rn *Runner) InvokePoolByCapability(ctx context.Context, ownerID, capability, prompt string, fileIDs []string) (string, error) {
	// Find a published model or direct executor that supports the requested capability.
	var targetModel string
	if rn.pools != nil && rn.tools != nil && rn.tools.Store() != nil {
		if pms, err := rn.pools.ListPublishedModels(ctx, true); err == nil {
			for _, pm := range pms {
				if !pm.Enabled {
					continue
				}
				caps, _ := rn.tools.Store().GetModelCapabilities(ctx, pm.ID)
				if (capability == "image_in" && caps.ImageIn) || (capability == "audio_in" && caps.AudioIn) {
					targetModel = pm.ID
					break
				}
			}
		}
	}
	if targetModel == "" {
		for _, info := range rn.registry.ListProviders() {
			if (capability == "image_in" && info.Capabilities.ImageIn) || (capability == "audio_in" && info.Capabilities.AudioIn) {
				if info.Executor.Check(ctx) == nil {
					targetModel = info.Config.ID
					break
				}
			}
		}
	}
	if targetModel == "" {
		return "", fmt.Errorf("no enabled model with capability %q available", capability)
	}

	info, ok := rn.registry.Get(targetModel)
	if !ok {
		// Resolve via pool if it's a published model.
		if rn.pools == nil {
			return "", fmt.Errorf("model %q unavailable", targetModel)
		}
		sel, err := rn.pools.ResolveCandidates(ctx, rn.registry, targetModel)
		if err != nil || len(sel.Candidates) == 0 {
			return "", fmt.Errorf("no available provider for %q", targetModel)
		}
		info = sel.Candidates[0].Info
	}

	fileRecs, err := rn.files.AcquireForRun(ctx, ownerID, fileIDs)
	if err != nil {
		return "", err
	}
	defer rn.files.ReleaseForRun(context.Background(), fileIDs)

	tmpDir, err := os.MkdirTemp("", "agy-delegate-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	inputDir := filepath.Join(tmpDir, "input")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		return "", err
	}

	staged := make([]executor.ExecuteFile, 0, len(fileRecs))
	for _, fr := range fileRecs {
		dest := filepath.Join(inputDir, fr.Filename)
		if err := rn.files.CopyToWorkspace(fr, dest); err != nil {
			return "", err
		}
		staged = append(staged, executor.ExecuteFile{
			ID:           fr.ID,
			Name:         fr.Filename,
			Path:         dest,
			RelativePath: "./input/" + fr.Filename,
			Size:         fr.Bytes,
			MediaKind:    fr.MediaKind,
			ContentType:  fr.ContentType,
		})
	}

	res, err := info.Executor.Execute(ctx, executor.ExecuteRequest{
		RunID:         "delegate_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Model:         targetModel,
		ProviderModel: info.Config.Model,
		WorkDir:       tmpDir,
		Files:         staged,
		Prompt:        prompt,
		Settings: executor.ExecutionSettings{
			Timeout:        2 * time.Minute,
			MaxOutputBytes: rn.cfg.Limits.MaxOutputBytes,
		},
	}, nil)
	if err != nil {
		return "", err
	}
	return res.Answer, nil
}
