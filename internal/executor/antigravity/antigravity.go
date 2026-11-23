package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/process"
)

var agyMCPMu sync.Mutex

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
		ImageIn:          true,
		AudioIn:          true,
		Tools:            true,
		MCP:              true,
		SupportedFormats: []string{"markdown", "text", "image", "audio"},
		TokenUsage:       true,
	}
}

func (e *Executor) Check(_ context.Context) error {
	return executor.CheckBinary(e.cfg.Executable)
}

var (
	reAgyParenModel = regexp.MustCompile(`^([^(]+?)\s*\(([^)]+)\)\s*$`)
	reAgyColumns    = regexp.MustCompile(`\s{2,}`)
)

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
		desc := ""
		isDefault := strings.Contains(low, "(default)") || strings.Contains(low, "[default]")
		if strings.Contains(s, "\t") {
			rawCols := strings.Split(s, "\t")
			cols := make([]string, 0, len(rawCols))
			for _, c := range rawCols {
				if t := strings.TrimSpace(c); t != "" {
					cols = append(cols, t)
				}
			}
			if len(cols) == 0 {
				continue
			}
			firstLow := strings.ToLower(cols[0])
			if len(cols) >= 2 && (firstLow == "id" || firstLow == "model" || firstLow == "model id") {
				continue
			}
			id = cols[0]
			if len(cols) >= 2 {
				name = cols[1]
			} else {
				name = id
			}
			if len(cols) >= 3 {
				var extra []string
				for _, c := range cols[2:] {
					cLow := strings.ToLower(c)
					if cLow == "default" || cLow == "(default)" || cLow == "[default]" || cLow == "*" {
						isDefault = true
						continue
					}
					extra = append(extra, c)
				}
				desc = strings.Join(extra, " ")
			}
		} else if cols := reAgyColumns.Split(s, 2); len(cols) == 2 && !strings.Contains(strings.TrimSpace(cols[0]), " ") {
			left := strings.TrimSpace(cols[0])
			right := strings.TrimSpace(cols[1])
			leftLow := strings.ToLower(left)
			if leftLow == "id" || leftLow == "model" {
				continue
			}
			id = left
			name = right
		} else if parts := strings.SplitN(s, " - ", 2); len(parts) == 2 && !strings.Contains(strings.TrimSpace(parts[0]), " ") {
			id = strings.TrimSpace(parts[0])
			name = strings.TrimSpace(parts[1])
		} else if m := reAgyParenModel.FindStringSubmatch(s); len(m) == 3 {
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
		}
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, executor.ModelDescriptor{ID: id, Name: name, Description: desc, Default: isDefault})
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

	extraEnv := make(map[string]string, len(e.cfg.Env)+3)
	for k, v := range e.cfg.Env {
		extraEnv[k] = v
	}
	if req.MCP != nil && strings.TrimSpace(req.MCP.URL) != "" {
		mcpPath, mcpErr := executor.WriteMCPConfigFile(req.WorkDir, req.MCP)
		if mcpErr != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: -1}, mcpErr
		}
		extraEnv["AGY_MCP_CONFIG"] = mcpPath
		extraEnv["AGY_MCP_URL"] = req.MCP.URL
		extraEnv["AGY_MCP_TOKEN"] = req.MCP.Token

		homeDir := strings.TrimSpace(e.cfg.HomeDir)
		if homeDir == "" {
			homeDir, _ = os.UserHomeDir()
		}
		if homeDir != "" {
			if cleanup, regErr := registerAgyHomeMCP(homeDir, "gateway", req.MCP); regErr == nil {
				defer cleanup()
			}
		}
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
		ExtraEnv:       extraEnv,
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

func normalizeModelFlag(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.IndexByte(model, '\t'); idx >= 0 {
		model = strings.TrimSpace(model[:idx])
	}
	return model
}

func (e *Executor) buildArgs(req executor.ExecuteRequest) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json"}
	modelName := normalizeModelFlag(req.ProviderModel)
	if modelName == "" {
		modelName = normalizeModelFlag(e.cfg.Model)
	}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	if req.Session.IsResume && req.Session.ProviderSessionID != "" {
		args = append(args, "--conversation", req.Session.ProviderSessionID)
	}
	if req.MCP != nil && strings.TrimSpace(req.MCP.URL) != "" {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// registerAgyHomeMCP writes a scoped HTTP MCP server entry into <homeDir>/.gemini/config/mcp_config.json
// using the exact format expected by `agy` CLI ("serverUrl" + "headers"), and returns a cleanup callback.
func registerAgyHomeMCP(homeDir, serverName string, mcp *executor.MCPConfig) (func(), error) {
	cfgDir := filepath.Join(homeDir, ".gemini", "config")
	cfgPath := filepath.Join(cfgDir, "mcp_config.json")

	agyMCPMu.Lock()
	defer agyMCPMu.Unlock()

	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if raw, err := os.ReadFile(cfgPath); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &doc)
	}
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok || servers == nil {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	servers[serverName] = map[string]any{
		"disabled":  false,
		"serverUrl": mcp.URL,
		"headers": map[string]string{
			"Authorization": "Bearer " + mcp.Token,
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
		return nil, err
	}

	cleanup := func() {
		agyMCPMu.Lock()
		defer agyMCPMu.Unlock()
		cur := map[string]any{}
		if raw, err := os.ReadFile(cfgPath); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &cur)
		}
		if srvMap, ok := cur["mcpServers"].(map[string]any); ok {
			delete(srvMap, serverName)
			if b, err := json.MarshalIndent(cur, "", "  "); err == nil {
				_ = os.WriteFile(cfgPath, b, 0o600)
			}
		}
	}
	return cleanup, nil
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
