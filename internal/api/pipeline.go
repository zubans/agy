package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/pools"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
)

// Runner coordinates validation, priority target selection, cooldown failover, locking,
// file staging, concurrency control, and execution across /v1/responses and /v1/chat/completions.
type Runner struct {
	cfg      *config.Config
	registry *executor.Registry
	pools    *pools.Store
	files    *storage.FileStore
	sessions *session.Manager
	procMgr  *process.Manager
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
		Session:       sessCtx,
		Settings: executor.ExecutionSettings{
			Timeout:        timeout,
			MaxOutputBytes: rn.cfg.Limits.MaxOutputBytes,
			Stream:         in.Stream,
			DiagnosticLog:  rn.cfg.Logging.Diagnostic,
		},
	}

	res, execErr := cand.Info.Executor.Execute(execCtx, execReq, func(ev executor.Event) error {
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
	})
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
