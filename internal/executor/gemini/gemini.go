package gemini

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

// Executor adapts the official Gemini CLI (`gemini`) in headless stream-json mode.
type Executor struct {
	cfg     config.ModelConfig
	procMgr *process.Manager
}

// New creates a Gemini CLI executor adapter.
func New(cfg config.ModelConfig, procMgr *process.Manager) *Executor {
	return &Executor{cfg: cfg, procMgr: procMgr}
}

func (e *Executor) Name() string { return "gemini" }

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

var reGeminiListModel = regexp.MustCompile(`^([a-zA-Z0-9._:-]+)\s*\(([^)]+)\)\s*$`)

// ListModels queries `gemini --list-models` inside the runtime environment.
func (e *Executor) ListModels(ctx context.Context) ([]executor.ModelDescriptor, string, error) {
	if err := e.Check(ctx); err != nil {
		return nil, "unavailable", err
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var stdout strings.Builder
	res, runErr := e.procMgr.RunStream(probeCtx, process.RunSpec{
		Executable:     e.cfg.Executable,
		Args:           []string{"--list-models"},
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

	parsed := parseGeminiModelsOutput(stdout.String())
	if len(parsed) == 0 {
		parsed = parseGeminiModelsOutput(res.Stderr)
	}
	if len(parsed) == 0 {
		fallback := []executor.ModelDescriptor{
			{ID: "auto", Name: "Auto", Default: true},
			{ID: "gemini-2.5-pro", Name: "gemini-2.5-pro"},
			{ID: "gemini-3.5-flash", Name: "gemini-3.5-flash"},
			{ID: "gemini-3.1-flash-lite", Name: "gemini-3.1-flash-lite"},
		}
		if runErr != nil {
			return fallback, "curated_fallback", nil
		}
		return fallback, "curated_fallback", nil
	}
	if e.cfg.Model != "" {
		found := false
		for _, m := range parsed {
			if m.ID == e.cfg.Model {
				found = true
				break
			}
		}
		if !found {
			parsed = append([]executor.ModelDescriptor{{ID: e.cfg.Model, Name: e.cfg.Model}}, parsed...)
		}
	}
	return parsed, "gemini_list_models", nil
}

func parseGeminiModelsOutput(raw string) []executor.ModelDescriptor {
	var out []executor.ModelDescriptor
	var cur *executor.ModelDescriptor
	for _, rawLine := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(strings.ToLower(trimmed), "available ") {
			continue
		}
		isIndented := len(rawLine) > 0 && (rawLine[0] == ' ' || rawLine[0] == '\t')
		if !isIndented {
			if m := reGeminiListModel.FindStringSubmatch(trimmed); len(m) == 3 {
				if cur != nil {
					out = append(out, *cur)
				}
				cur = &executor.ModelDescriptor{
					ID:      strings.TrimSpace(m[1]),
					Name:    strings.TrimSpace(m[2]),
					Default: strings.EqualFold(strings.TrimSpace(m[1]), "auto"),
				}
				continue
			}
		}
		if cur != nil && isIndented {
			if strings.HasPrefix(trimmed, "Description:") {
				cur.Description = strings.TrimSpace(strings.TrimPrefix(trimmed, "Description:"))
			} else if strings.HasPrefix(trimmed, "Context limit:") {
				ctxInfo := strings.TrimSpace(trimmed)
				if cur.Description != "" {
					cur.Description += " | " + ctxInfo
				} else {
					cur.Description = ctxInfo
				}
			}
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
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
		instruction = "System instructions:\n" + req.Instructions + "\n\n" + instruction
	}

	args := e.buildArgs(req, instruction)
	var (
		sessionID = req.Session.ProviderSessionID
		streamed  strings.Builder
		finalResp string
		usage     executor.Usage
		failure   string
	)

	res, runErr := e.procMgr.RunStream(ctx, process.RunSpec{
		Executable:     e.cfg.Executable,
		Args:           args,
		WorkDir:        req.WorkDir,
		HomeDir:        e.cfg.HomeDir,
		ExtraEnv:       e.cfg.Env,
		MaxOutputBytes: req.Settings.MaxOutputBytes,
		OnLine: func(line []byte) error {
			var ev geminiEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				return nil
			}
			switch ev.Type {
			case "init":
				if ev.SessionID != "" {
					sessionID = ev.SessionID
				} else if ev.SessionIDSnake != "" {
					sessionID = ev.SessionIDSnake
				}
				if emit != nil {
					return emit(executor.Event{
						Type:      executor.EventStarted,
						SessionID: sessionID,
						RawType:   ev.Type,
						Timestamp: time.Now(),
					})
				}
			case "message", "delta":
				if ev.Role != "" && ev.Role != "assistant" {
					return nil
				}
				chunk := ev.Delta
				if chunk == "" {
					chunk = ev.Content
				}
				if chunk != "" {
					streamed.WriteString(chunk)
					if emit != nil {
						return emit(executor.Event{
							Type:      executor.EventTextDelta,
							Text:      chunk,
							RawType:   ev.Type,
							Timestamp: time.Now(),
						})
					}
				}
			case "error":
				failure = extractGeminiError(ev)
			case "result":
				if ev.Status == "error" {
					failure = extractGeminiError(ev)
				}
				if ev.Response != "" {
					finalResp = ev.Response
				}
				if ev.Stats != nil && ev.Stats.Tokens != nil {
					t := ev.Stats.Tokens
					in := firstNonZero(t.PromptTokens, t.InputTokens)
					out := firstNonZero(t.CandidatesTokens, t.OutputTokens)
					tot := firstNonZero(t.TotalTokens, in+out)
					usage = executor.Usage{
						InputTokens:  in,
						OutputTokens: out,
						TotalTokens:  tot,
						Available:    tot > 0 || in > 0 || out > 0,
					}
				}
			}
			return nil
		},
	})

	if runErr != nil || failure != "" {
		errText := res.Stderr
		if failure != "" {
			errText = failure + "\n" + errText
		}
		if runErr == nil && failure != "" {
			runErr = fmt.Errorf("gemini error: %s", failure)
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
			ProviderSessionID: sessionID,
			Status:            status,
			Usage:             usage,
			ExitCode:          res.ExitCode,
			Error:             execErr,
		}, execErr
	}

	answer := finalResp
	if strings.TrimSpace(answer) == "" {
		answer = streamed.String()
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

func (e *Executor) buildArgs(req executor.ExecuteRequest, instruction string) []string {
	args := []string{"--output-format", "stream-json"}
	modelName := req.ProviderModel
	if modelName == "" {
		modelName = e.cfg.Model
	}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	if req.Session.IsResume && req.Session.ProviderSessionID != "" {
		args = append(args, "--resume", req.Session.ProviderSessionID)
	}
	if e.cfg.SandboxMode == "yolo" && e.cfg.AllowCommands {
		args = append(args, "--yolo")
	} else if len(e.cfg.AllowedTools) > 0 {
		args = append(args, "--allowed-tools", strings.Join(e.cfg.AllowedTools, ","))
	}
	args = append(args, "-p", instruction)
	return args
}

type geminiEvent struct {
	Type           string       `json:"type"`
	SessionID      string       `json:"sessionId"`
	SessionIDSnake string       `json:"session_id"`
	Role           string       `json:"role"`
	Delta          string       `json:"delta"`
	Content        string       `json:"content"`
	Status         string       `json:"status"`
	Response       string       `json:"response"`
	Error          any          `json:"error"`
	Message        string       `json:"message"`
	Stats          *geminiStats `json:"stats"`
}

type geminiStats struct {
	Tokens *geminiTokens `json:"tokens"`
}

type geminiTokens struct {
	PromptTokens     int64 `json:"promptTokens"`
	CandidatesTokens int64 `json:"candidatesTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

func extractGeminiError(ev geminiEvent) string {
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
	return "gemini execution failed"
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
