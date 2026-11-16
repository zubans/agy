package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/process"
)

// Executor adapts Google Antigravity CLI (`agy`) using stream-json input/output.
type Executor struct {
	cfg     config.ModelConfig
	procMgr *process.Manager
}

// New creates an Antigravity CLI executor adapter.
func New(cfg config.ModelConfig, procMgr *process.Manager) *Executor {
	return &Executor{cfg: cfg, procMgr: procMgr}
}

func (e *Executor) Name() string { return "antigravity" }

func (e *Executor) Capabilities() executor.Capabilities {
	return executor.Capabilities{
		Streaming:        true,
		SessionResume:    true,
		ModelSelection:   true,
		FileInput:        true,
		SupportedFormats: []string{"markdown", "text"},
		TokenUsage:       true,
	}
}

func (e *Executor) Check(_ context.Context) error {
	return executor.CheckBinary(e.cfg.Executable)
}

var reAgyParenModel = regexp.MustCompile(`^([^(]+?)\s*\(([^)]+)\)\s*$`)

// ListModels probes `agy models` inside the runtime environment to discover available models.
func (e *Executor) ListModels(ctx context.Context) ([]executor.ModelDescriptor, string, error) {
	if err := e.Check(ctx); err != nil {
		return nil, "unavailable", err
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var stdout strings.Builder
	res, runErr := e.procMgr.RunStream(probeCtx, process.RunSpec{
		Executable:     e.cfg.Executable,
		Args:           []string{"models"},
		WorkDir:        os.TempDir(),
		HomeDir:        e.cfg.HomeDir,
		ExtraEnv:       e.cfg.Env,
		MaxOutputBytes: 512 * 1024,
		OnLine: func(line []byte) error {
			stdout.Write(line)
			stdout.WriteByte('\n')
			return nil
		},
	})

	combined := stdout.String() + "\n" + res.Stderr
	lower := strings.ToLower(combined)
	if strings.Contains(lower, "sign in") || strings.Contains(lower, "not logged in") || strings.Contains(lower, "unauthenticated") {
		return nil, "auth_required", fmt.Errorf("agy requires sign-in to list models (run make agy-login): %s", firstNonEmptyLine(combined))
	}

	parsed := parseAgyModelsOutput(stdout.String())
	if len(parsed) == 0 {
		parsed = parseAgyModelsOutput(res.Stderr)
	}
	if len(parsed) == 0 && e.cfg.Model != "" {
		parsed = append(parsed, executor.ModelDescriptor{ID: e.cfg.Model, Name: e.cfg.Model, Default: true})
	}
	if runErr != nil && len(parsed) == 0 {
		return nil, "agy_models", fmt.Errorf("agy models failed: %w (%s)", runErr, firstNonEmptyLine(combined))
	}
	if e.cfg.Model != "" {
		parsed = mergeConfiguredModel(parsed, e.cfg.Model)
	}
	return parsed, "agy_models", nil
}

func parseAgyModelsOutput(raw string) []executor.ModelDescriptor {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		var arr []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Default     bool   `json:"default"`
		}
		if json.Unmarshal([]byte(trimmed), &arr) == nil && len(arr) > 0 {
			out := make([]executor.ModelDescriptor, 0, len(arr))
			for _, it := range arr {
				id := strings.TrimSpace(it.ID)
				if id == "" {
					id = strings.TrimSpace(it.Name)
				}
				if id == "" {
					continue
				}
				name := strings.TrimSpace(it.DisplayName)
				if name == "" {
					name = strings.TrimSpace(it.Name)
				}
				out = append(out, executor.ModelDescriptor{ID: id, Name: name, Description: strings.TrimSpace(it.Description), Default: it.Default})
			}
			return out
		}
		var wrapped struct {
			Models []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
				Description string `json:"description"`
				Default     bool   `json:"default"`
			} `json:"models"`
		}
		if json.Unmarshal([]byte(trimmed), &wrapped) == nil && len(wrapped.Models) > 0 {
			out := make([]executor.ModelDescriptor, 0, len(wrapped.Models))
			for _, it := range wrapped.Models {
				id := strings.TrimSpace(it.ID)
				if id == "" {
					id = strings.TrimSpace(it.Name)
				}
				if id == "" {
					continue
				}
				name := strings.TrimSpace(it.DisplayName)
				if name == "" {
					name = strings.TrimSpace(it.Name)
				}
				out = append(out, executor.ModelDescriptor{ID: id, Name: name, Description: strings.TrimSpace(it.Description), Default: it.Default})
			}
			return out
		}
	}

	seen := make(map[string]struct{})
	var out []executor.ModelDescriptor
	for _, line := range strings.Split(raw, "\n") {
		s := strings.TrimSpace(line)
		s = strings.TrimLeft(s, "*-• ")
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		low := strings.ToLower(s)
		if strings.HasPrefix(low, "fetching") || strings.HasPrefix(low, "available") || strings.HasPrefix(low, "error:") || strings.HasPrefix(low, "usage:") || strings.HasPrefix(low, "---") || strings.HasPrefix(low, "===") || strings.HasPrefix(low, "note:") {
			continue
		}
		id := s
		name := s
		isDefault := strings.Contains(low, "(default)") || strings.Contains(low, "[default]")
		if m := reAgyParenModel.FindStringSubmatch(s); len(m) == 3 {
			left := strings.TrimSpace(m[1])
			right := strings.TrimSpace(m[2])
			// Support both "model-id (Display Name)" and "Display Name (model-id)"
			if strings.Contains(left, " ") && !strings.Contains(right, " ") && strings.ContainsAny(right, "-_.") {
				id = right
				name = left
			} else if !strings.Contains(left, " ") {
				id = left
				name = right
			}
		} else if parts := strings.SplitN(s, " - ", 2); len(parts) == 2 {
			id = strings.TrimSpace(parts[0])
			name = strings.TrimSpace(parts[1])
		}
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, executor.ModelDescriptor{ID: id, Name: name, Default: isDefault})
	}
	return out
}

func mergeConfiguredModel(list []executor.ModelDescriptor, cfgModel string) []executor.ModelDescriptor {
	for _, m := range list {
		if m.ID == cfgModel {
			return list
		}
	}
	return append([]executor.ModelDescriptor{{ID: cfgModel, Name: cfgModel, Default: true}}, list...)
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func (e *Executor) Execute(ctx context.Context, req executor.ExecuteRequest, emit executor.EventHandler) (executor.ExecuteResult, error) {
	if err := e.Check(ctx); err != nil {
		execErr := &executor.ExecError{
			Kind:    executor.ErrUnavailable,
			Code:    "cli_unavailable",
			Message: err.Error(),
			Cause:   err,
		}
		return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
	}

	instruction := executor.BuildFileInstruction(req)
	if req.Instructions != "" {
		instruction = "<system>\n" + req.Instructions + "\n</system>\n\n" + instruction
	}

	stdinPayload, err := json.Marshal(map[string]any{
		"event": "user",
		"message": map[string]any{
			"content": instruction,
		},
	})
	if err != nil {
		return executor.ExecuteResult{Status: "failed", ExitCode: -1}, err
	}
	stdinPayload = append(stdinPayload, '\n')

	args := e.buildArgs(req)
	var (
		convID   = req.Session.ProviderSessionID
		streamed strings.Builder
		result   *agyResultPayload
		started  bool
	)

	res, runErr := e.procMgr.RunStream(ctx, process.RunSpec{
		Executable:     e.cfg.Executable,
		Args:           args,
		WorkDir:        req.WorkDir,
		HomeDir:        e.cfg.HomeDir,
		ExtraEnv:       e.cfg.Env,
		Stdin:          stdinPayload,
		MaxOutputBytes: req.Settings.MaxOutputBytes,
		OnLine: func(line []byte) error {
			var ev agyEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				return nil
			}
			switch ev.Event {
			case "step_update":
				if !started && emit != nil {
					started = true
					if err := emit(executor.Event{
						Type:      executor.EventStarted,
						SessionID: convID,
						RawType:   ev.Event,
						Timestamp: time.Now(),
					}); err != nil {
						return err
					}
				}
				if ev.StepUpdate != nil && ev.StepUpdate.StepType == "agent_response" && ev.StepUpdate.TextDelta != "" {
					streamed.WriteString(ev.StepUpdate.TextDelta)
					if emit != nil {
						return emit(executor.Event{
							Type:      executor.EventTextDelta,
							Text:      ev.StepUpdate.TextDelta,
							RawType:   ev.Event,
							Timestamp: time.Now(),
						})
					}
				}
			case "result":
				result = ev.Result
				if result != nil && result.ConversationID != "" {
					convID = result.ConversationID
				}
			}
			return nil
		},
	})

	if result != nil && result.Status != "SUCCESS" {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = result.Status
		}
		execErr := executor.ClassifyCLIError(e.Name(), process.SanitizeOutput(errMsg+"\n"+res.Stderr), runErr, res.ExitCode)
		if emit != nil {
			_ = emit(executor.Event{Type: executor.EventError, Text: execErr.Message, Timestamp: time.Now()})
		}
		return executor.ExecuteResult{
			ProviderSessionID: convID,
			Status:            "failed",
			ExitCode:          res.ExitCode,
			Error:             execErr,
		}, execErr
	}

	if runErr != nil || result == nil {
		if runErr == nil {
			runErr = fmt.Errorf("antigravity exited without a result event (code %d)", res.ExitCode)
		}
		execErr := executor.ClassifyCLIError(e.Name(), res.Stderr, runErr, res.ExitCode)
		if emit != nil {
			_ = emit(executor.Event{Type: executor.EventError, Text: execErr.Message, Timestamp: time.Now()})
		}
		status := "failed"
		if execErr.Kind == executor.ErrTimeout {
			status = "timed_out"
		} else if execErr.Kind == executor.ErrCancelled {
			status = "cancelled"
		}
		return executor.ExecuteResult{
			ProviderSessionID: convID,
			Status:            status,
			ExitCode:          res.ExitCode,
			Error:             execErr,
		}, execErr
	}

	var usage executor.Usage
	if result.Usage != nil {
		usage.InputTokens = result.Usage.InputTokens
		usage.OutputTokens = result.Usage.OutputTokens + result.Usage.ThinkingTokens
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		usage.Available = true
	}

	answer := result.Response
	if strings.TrimSpace(answer) == "" {
		answer = streamed.String()
	}

	if emit != nil {
		if err := emit(executor.Event{
			Type:      executor.EventCompleted,
			SessionID: convID,
			Usage:     &usage,
			Timestamp: time.Now(),
		}); err != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: res.ExitCode}, err
		}
	}

	return executor.ExecuteResult{
		Answer:            answer,
		ProviderSessionID: convID,
		Status:            "completed",
		Usage:             usage,
		ExitCode:          res.ExitCode,
	}, nil
}

func (e *Executor) buildArgs(req executor.ExecuteRequest) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json"}
	modelName := req.ProviderModel
	if modelName == "" {
		modelName = e.cfg.Model
	}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	if req.Session.IsResume && req.Session.ProviderSessionID != "" {
		args = append(args, "--conversation", req.Session.ProviderSessionID)
	}
	return args
}

type agyEvent struct {
	Event      string            `json:"event"`
	StepUpdate *agyStepUpdate    `json:"step_update"`
	Result     *agyResultPayload `json:"result"`
}

type agyStepUpdate struct {
	StepType  string `json:"step_type"`
	TextDelta string `json:"text_delta"`
	ToolName  string `json:"tool_name"`
}

type agyResultPayload struct {
	Status         string    `json:"status"`
	Response       string    `json:"response"`
	Error          string    `json:"error"`
	ConversationID string    `json:"conversation_id"`
	Usage          *agyUsage `json:"usage"`
}

type agyUsage struct {
	InputTokens    int64 `json:"input_tokens"`
	OutputTokens   int64 `json:"output_tokens"`
	ThinkingTokens int64 `json:"thinking_tokens"`
}
