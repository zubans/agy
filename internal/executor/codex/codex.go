package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/process"
)

// Executor adapts the official OpenAI Codex CLI (`codex exec`) to the gateway Executor interface.
type Executor struct {
	cfg     config.ModelConfig
	procMgr *process.Manager
}

// New creates a Codex CLI executor adapter.
func New(cfg config.ModelConfig, procMgr *process.Manager) *Executor {
	return &Executor{cfg: cfg, procMgr: procMgr}
}

func (e *Executor) Name() string { return "codex" }

func (e *Executor) Capabilities() executor.Capabilities {
	return executor.Capabilities{
		Streaming:        true,
		SessionResume:    true,
		ModelSelection:   true,
		FileInput:        true,
		ImageIn:          true,
		Tools:            true,
		MCP:              true,
		SupportedFormats: []string{"markdown", "text", "image"},
		TokenUsage:       true,
	}
}

func (e *Executor) Check(_ context.Context) error {
	return executor.CheckBinary(e.cfg.Executable)
}

// ListModels reads $CODEX_HOME/models_cache.json if present and merges curated Codex CLI models.
func (e *Executor) ListModels(_ context.Context) ([]executor.ModelDescriptor, string, error) {
	seen := make(map[string]struct{})
	var out []executor.ModelDescriptor
	add := func(d executor.ModelDescriptor) {
		id := strings.TrimSpace(d.ID)
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		d.ID = id
		if d.Name == "" {
			d.Name = id
		}
		out = append(out, d)
	}

	source := "curated_defaults"
	if e.cfg.Model != "" {
		add(executor.ModelDescriptor{ID: e.cfg.Model, Name: e.cfg.Model, Default: true})
	}

	home := strings.TrimSpace(e.cfg.HomeDir)
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home != "" {
		cacheCandidates := []string{
			filepath.Join(home, "models_cache.json"),
			filepath.Join(home, ".codex", "models_cache.json"),
		}
		for _, p := range cacheCandidates {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var cache struct {
				Models []struct {
					Slug        string `json:"slug"`
					ID          string `json:"id"`
					DisplayName string `json:"display_name"`
					Description string `json:"description"`
					Visibility  string `json:"visibility"`
				} `json:"models"`
			}
			if json.Unmarshal(raw, &cache) == nil && len(cache.Models) > 0 {
				source = "codex_models_cache"
				for _, m := range cache.Models {
					if m.Visibility == "hidden" {
						continue
					}
					id := m.Slug
					if id == "" {
						id = m.ID
					}
					add(executor.ModelDescriptor{ID: id, Name: m.DisplayName, Description: m.Description})
				}
				break
			}
		}
	}

	for _, preset := range []executor.ModelDescriptor{
		{ID: "gpt-5.4", Name: "gpt-5.4"},
		{ID: "gpt-5.3-codex", Name: "gpt-5.3-codex"},
		{ID: "gpt-5.2-codex", Name: "gpt-5.2-codex"},
		{ID: "gpt-5.1-codex", Name: "gpt-5.1-codex"},
		{ID: "o3", Name: "o3"},
		{ID: "o4-mini", Name: "o4-mini"},
	} {
		add(preset)
	}

	return out, source, nil
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

	if req.MCP != nil && strings.TrimSpace(req.MCP.URL) != "" {
		if _, mcpErr := executor.WriteMCPConfigFile(req.WorkDir, req.MCP); mcpErr != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: -1}, mcpErr
		}
	}

	instruction := executor.BuildFileInstruction(req)
	if req.Instructions != "" {
		instruction = "<system_instructions>\n" + req.Instructions + "\n</system_instructions>\n\n" + instruction
	}

	args := e.buildArgs(req)
	var (
		threadID = req.Session.ProviderSessionID
		answer   strings.Builder
		streamed strings.Builder
		usage    executor.Usage
		failure  string
	)

	res, runErr := e.procMgr.RunStream(ctx, process.RunSpec{
		Executable:     e.cfg.Executable,
		Args:           args,
		WorkDir:        req.WorkDir,
		HomeDir:        e.cfg.HomeDir,
		ExtraEnv:       e.buildEnv(),
		Stdin:          []byte(instruction),
		MaxOutputBytes: req.Settings.MaxOutputBytes,
		OnLine: func(line []byte) error {
			var ev codexEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				// Ignore non-JSON preamble/status lines safely.
				return nil
			}
			switch ev.Type {
			case "thread.started":
				if ev.ThreadID != "" {
					threadID = ev.ThreadID
				}
				if emit != nil {
					return emit(executor.Event{
						Type:      executor.EventStarted,
						SessionID: threadID,
						RawType:   ev.Type,
						Timestamp: time.Now(),
					})
				}
			case "item.delta", "item.updated":
				if ev.Delta != "" {
					streamed.WriteString(ev.Delta)
					if emit != nil {
						return emit(executor.Event{
							Type:      executor.EventTextDelta,
							Text:      ev.Delta,
							RawType:   ev.Type,
							Timestamp: time.Now(),
						})
					}
				}
			case "item.completed":
				if ev.Item != nil && ev.Item.Type == "agent_message" {
					txt := extractItemText(ev.Item)
					if txt != "" {
						answer.Reset()
						answer.WriteString(txt)
						if streamed.Len() == 0 && emit != nil {
							streamed.WriteString(txt)
							return emit(executor.Event{
								Type:      executor.EventTextDelta,
								Text:      txt,
								RawType:   ev.Type,
								Timestamp: time.Now(),
							})
						}
					}
				}
			case "turn.completed":
				if ev.Usage != nil {
					usage.InputTokens = ev.Usage.InputTokens + ev.Usage.CachedInputTokens
					usage.OutputTokens = ev.Usage.OutputTokens + ev.Usage.ReasoningOutputTokens
					usage.TotalTokens = usage.InputTokens + usage.OutputTokens
					usage.Available = true
				}
			case "turn.failed", "error":
				failure = extractError(ev)
			}
			return nil
		},
	})

	if evUsage := res; evUsage.ExitCode != 0 && runErr == nil {
		runErr = fmt.Errorf("codex exited with status %d", res.ExitCode)
	}

	if runErr != nil || failure != "" {
		errText := res.Stderr
		if failure != "" {
			errText = failure + "\n" + errText
		}
		execErr := executor.ClassifyCLIError(e.Name(), process.SanitizeOutput(errText), runErr, res.ExitCode)
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
			ProviderSessionID: threadID,
			Status:            status,
			Usage:             usage,
			ExitCode:          res.ExitCode,
			Error:             execErr,
		}, execErr
	}

	finalText := answer.String()
	if strings.TrimSpace(finalText) == "" {
		finalText = streamed.String()
	}

	if emit != nil {
		if err := emit(executor.Event{
			Type:      executor.EventCompleted,
			SessionID: threadID,
			Usage:     &usage,
			Timestamp: time.Now(),
		}); err != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: res.ExitCode}, err
		}
	}

	return executor.ExecuteResult{
		Answer:            finalText,
		ProviderSessionID: threadID,
		Status:            "completed",
		Usage:             usage,
		ExitCode:          res.ExitCode,
	}, nil
}

func (e *Executor) buildArgs(req executor.ExecuteRequest) []string {
	modelName := req.ProviderModel
	if modelName == "" {
		modelName = e.cfg.Model
	}
	if req.Session.IsResume && req.Session.ProviderSessionID != "" {
		args := []string{
			"exec", "resume",
			"--json",
			"--skip-git-repo-check",
			"--ignore-rules",
			"--ignore-user-config",
		}
		if modelName != "" {
			args = append(args, "--model", modelName)
		}
		args = append(args, req.Session.ProviderSessionID, "-")
		return args
	}

	sandboxMode := e.cfg.SandboxMode
	if sandboxMode != "read-only" && sandboxMode != "workspace-write" {
		sandboxMode = "read-only"
	}
	args := []string{
		"exec",
		"--json",
		"--color", "never",
		"--sandbox", sandboxMode,
		"--skip-git-repo-check",
		"--ignore-rules",
		"--ignore-user-config",
		"--cd", req.WorkDir,
	}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	if req.Session.GatewaySessionID == "" {
		args = append(args, "--ephemeral")
	}
	args = append(args, "-")
	return args
}

func (e *Executor) buildEnv() map[string]string {
	env := make(map[string]string, len(e.cfg.Env)+1)
	if e.cfg.HomeDir != "" {
		env["CODEX_HOME"] = e.cfg.HomeDir
	}
	for k, v := range e.cfg.Env {
		env[k] = v
	}
	return env
}

type codexEvent struct {
	Type     string      `json:"type"`
	ThreadID string      `json:"thread_id"`
	Delta    string      `json:"delta"`
	Item     *codexItem  `json:"item"`
	Usage    *codexUsage `json:"usage"`
	Error    any         `json:"error"`
	Message  string      `json:"message"`
}

type codexItem struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Content []any  `json:"content"`
}

type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

func extractItemText(item *codexItem) string {
	if item.Text != "" {
		return item.Text
	}
	var b strings.Builder
	for _, part := range item.Content {
		switch v := part.(type) {
		case string:
			b.WriteString(v)
		case map[string]any:
			if t, ok := v["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

func extractError(ev codexEvent) string {
	if ev.Message != "" {
		return ev.Message
	}
	switch v := ev.Error.(type) {
	case string:
		return v
	case map[string]any:
		if m, ok := v["message"].(string); ok {
			return m
		}
	}
	return "codex execution failed"
}
