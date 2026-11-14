package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
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
