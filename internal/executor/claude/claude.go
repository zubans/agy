package claude

import (
	"context"
	"crypto/rand"
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

// Executor adapts Claude Code CLI (`claude -p`) in stream-json headless mode.
type Executor struct {
	cfg     config.ModelConfig
	procMgr *process.Manager
}

// New creates a Claude CLI executor adapter.
func New(cfg config.ModelConfig, procMgr *process.Manager) *Executor {
	return &Executor{cfg: cfg, procMgr: procMgr}
}

func (e *Executor) Name() string { return "claude" }

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

// ListModels inspects ~/.claude/settings.json and merges standard Claude Code CLI aliases/models.
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

	source := "curated_aliases"
	if e.cfg.Model != "" {
		add(executor.ModelDescriptor{ID: e.cfg.Model, Name: e.cfg.Model, Default: true})
	}
	if home := strings.TrimSpace(e.cfg.HomeDir); home != "" {
		if raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
			var s struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s.Model) != "" {
				source = "claude_settings"
				add(executor.ModelDescriptor{ID: s.Model, Name: s.Model, Description: "Configured in ~/.claude/settings.json", Default: true})
			}
		}
	}

	for _, preset := range []executor.ModelDescriptor{
		{ID: "sonnet", Name: "Sonnet (alias)", Description: "Latest Claude Sonnet via Claude Code CLI"},
		{ID: "opus", Name: "Opus (alias)", Description: "Latest Claude Opus via Claude Code CLI"},
		{ID: "haiku", Name: "Haiku (alias)", Description: "Latest Claude Haiku via Claude Code CLI"},
		{ID: "claude-sonnet-4-6", Name: "claude-sonnet-4-6"},
		{ID: "claude-opus-4-6", Name: "claude-opus-4-6"},
		{ID: "claude-haiku-4-5", Name: "claude-haiku-4-5"},
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

	var sysPromptFile string
	if req.Instructions != "" {
		tmpDir := filepath.Join(req.WorkDir, ".tmp")
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: -1}, err
		}
		sysPromptFile = filepath.Join(tmpDir, "system-prompt.md")
		if err := os.WriteFile(sysPromptFile, []byte(req.Instructions), 0o600); err != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: -1}, err
		}
	}

	sessionID := req.Session.ProviderSessionID
	if !req.Session.IsResume && req.Session.GatewaySessionID != "" {
		sessionID = newUUID()
	}

	var mcpConfigFile string
	if req.MCP != nil && strings.TrimSpace(req.MCP.URL) != "" {
		var mcpErr error
		mcpConfigFile, mcpErr = executor.WriteMCPConfigFile(req.WorkDir, req.MCP)
		if mcpErr != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: -1}, mcpErr
		}
	}

	args := e.buildArgs(req, sysPromptFile, mcpConfigFile, sessionID)

	instruction := executor.BuildFileInstruction(req)
	stdinPayload, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": instruction},
			},
		},
	})
	if err != nil {
		return executor.ExecuteResult{Status: "failed", ExitCode: -1}, err
	}
	stdinPayload = append(stdinPayload, '\n')

	var (
		streamed  strings.Builder
		assistant strings.Builder
		resultEv  *claudeResultEvent
		started   bool
	)

	res, runErr := e.procMgr.RunStream(ctx, process.RunSpec{
		Executable:     e.cfg.Executable,
		Args:           args,
		WorkDir:        req.WorkDir,
		HomeDir:        e.cfg.HomeDir,
		ExtraEnv:       e.buildEnv(),
		Stdin:          stdinPayload,
		MaxOutputBytes: req.Settings.MaxOutputBytes,
		OnLine: func(line []byte) error {
			var ev claudeEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				return nil
			}
			if ev.SessionID != "" {
				sessionID = ev.SessionID
			}
			switch ev.Type {
			case "system":
				if !started && emit != nil {
					started = true
					return emit(executor.Event{
						Type:      executor.EventStarted,
						SessionID: sessionID,
						RawType:   ev.Type,
						Timestamp: time.Now(),
					})
				}
			case "stream_event":
				if ev.Event != nil && ev.Event.Delta != nil && ev.Event.Delta.Type == "text_delta" && ev.Event.Delta.Text != "" {
					streamed.WriteString(ev.Event.Delta.Text)
					if emit != nil {
						return emit(executor.Event{
							Type:      executor.EventTextDelta,
							Text:      ev.Event.Delta.Text,
							RawType:   ev.Type,
							Timestamp: time.Now(),
						})
					}
				}
			case "assistant":
				if ev.Message != nil {
					for _, part := range ev.Message.Content {
						if part.Type == "text" && part.Text != "" {
							assistant.WriteString(part.Text)
						}
					}
				}
			case "result":
				resultEv = &claudeResultEvent{
					IsError:   ev.IsError,
					Result:    ev.Result,
					Subtype:   ev.Subtype,
					SessionID: ev.SessionID,
					Usage:     ev.Usage,
				}
				if ev.SessionID != "" {
					sessionID = ev.SessionID
				}
			}
			return nil
		},
	})

	if resultEv != nil && resultEv.IsError {
		errMsg := resultEv.Result
		if errMsg == "" {
			errMsg = resultEv.Subtype
		}
		execErr := executor.ClassifyCLIError(e.Name(), process.SanitizeOutput(errMsg+"\n"+res.Stderr), runErr, res.ExitCode)
		if emit != nil {
			_ = emit(executor.Event{Type: executor.EventError, Text: execErr.Message, Timestamp: time.Now()})
		}
		return executor.ExecuteResult{
			ProviderSessionID: sessionID,
			Status:            "failed",
			ExitCode:          res.ExitCode,
			Error:             execErr,
		}, execErr
	}

	if runErr != nil || resultEv == nil {
		if runErr == nil {
			runErr = fmt.Errorf("claude exited without a result event (code %d)", res.ExitCode)
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
			ProviderSessionID: sessionID,
			Status:            status,
			ExitCode:          res.ExitCode,
			Error:             execErr,
		}, execErr
	}

	var usage executor.Usage
	if resultEv.Usage != nil {
		usage.InputTokens = resultEv.Usage.InputTokens + resultEv.Usage.CacheReadInputTokens + resultEv.Usage.CacheCreationInputTokens
		usage.OutputTokens = resultEv.Usage.OutputTokens
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		usage.Available = true
	}

	answer := resultEv.Result
	if strings.TrimSpace(answer) == "" {
		answer = streamed.String()
	}
	if strings.TrimSpace(answer) == "" {
		answer = assistant.String()
	}

	if emit != nil {
		if err := emit(executor.Event{
			Type:      executor.EventCompleted,
			SessionID: sessionID,
			Usage:     &usage,
			Timestamp: time.Now(),
		}); err != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: res.ExitCode}, err
		}
	}

	return executor.ExecuteResult{
		Answer:            answer,
		ProviderSessionID: sessionID,
		Status:            "completed",
		Usage:             usage,
		ExitCode:          res.ExitCode,
	}, nil
}

func (e *Executor) buildArgs(req executor.ExecuteRequest, sysPromptFile, mcpConfigFile, sessionID string) []string {
	tools := "Read"
	if len(e.cfg.AllowedTools) > 0 {
		tools = strings.Join(e.cfg.AllowedTools, ",")
	}
	if mcpConfigFile != "" {
		tools += ",mcp__gateway__*"
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--strict-mcp-config",
		"--tools", tools,
	}
	if mcpConfigFile != "" {
		args = append(args, "--mcp-config", mcpConfigFile)
	}
	if !e.cfg.AllowCommands {
		args = append(args, "--restricted")
	}
	if e.cfg.SandboxMode != "" {
		args = append(args, "--permission-mode", e.cfg.SandboxMode)
	} else {
		args = append(args, "--permission-mode", "dontAsk")
	}
	modelName := req.ProviderModel
	if modelName == "" {
		modelName = e.cfg.Model
	}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	if sysPromptFile != "" {
		args = append(args, "--system-prompt-file", sysPromptFile)
	}
	if req.Session.IsResume && req.Session.ProviderSessionID != "" {
		args = append(args, "--resume", req.Session.ProviderSessionID)
	} else if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--no-session-persistence")
	}
	return args
}

func (e *Executor) buildEnv() map[string]string {
	env := map[string]string{
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
	for k, v := range e.cfg.Env {
		env[k] = v
	}
	return env
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type claudeEvent struct {
	Type      string             `json:"type"`
	Subtype   string             `json:"subtype"`
	SessionID string             `json:"session_id"`
	IsError   bool               `json:"is_error"`
	Result    string             `json:"result"`
	Usage     *claudeUsage       `json:"usage"`
	Event     *claudeStreamInner `json:"event"`
	Message   *claudeMsg         `json:"message"`
}

type claudeStreamInner struct {
	Delta *claudeDelta `json:"delta"`
}

type claudeDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeMsg struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

type claudeResultEvent struct {
	IsError   bool
	Result    string
	Subtype   string
	SessionID string
	Usage     *claudeUsage
}
