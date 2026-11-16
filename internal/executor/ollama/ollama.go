package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
)

// Executor adapts a local Ollama server via its native HTTP API (/api/chat, /api/tags).
type Executor struct {
	cfg        config.ModelConfig
	httpClient *http.Client
}

// New creates an Ollama HTTP API adapter.
func New(cfg config.ModelConfig) *Executor {
	return &Executor{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
}

func (e *Executor) Name() string { return "ollama" }

// Capabilities honestly reports that Ollama HTTP chat has no CLI-side session resume;
// multi-turn continuity is restored by the gateway from stored session history files (Section 8.2).
func (e *Executor) Capabilities() executor.Capabilities {
	return executor.Capabilities{
		Streaming:        true,
		SessionResume:    false,
		ModelSelection:   true,
		FileInput:        true,
		SupportedFormats: []string{"markdown", "text"},
		TokenUsage:       true,
	}
}

func (e *Executor) Check(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(e.cfg.BaseURL, "/")+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama unreachable at %s: %w", e.cfg.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama /api/tags returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// ListModels fetches available pulled models from Ollama GET /api/tags.
func (e *Executor) ListModels(ctx context.Context) ([]executor.ModelDescriptor, string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(e.cfg.BaseURL, "/")+"/api/tags", nil)
	if err != nil {
		return nil, "ollama_tags", err
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		if e.cfg.Model != "" {
			return []executor.ModelDescriptor{{ID: e.cfg.Model, Name: e.cfg.Model, Default: true}}, "configured", err
		}
		return nil, "ollama_tags", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "ollama_tags", fmt.Errorf("ollama /api/tags returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model"`
			ModifiedAt string `json:"modified_at"`
			Size       int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&payload); err != nil {
		return nil, "ollama_tags", err
	}

	seen := make(map[string]struct{})
	var out []executor.ModelDescriptor
	if strings.TrimSpace(e.cfg.Model) != "" {
		seen[e.cfg.Model] = struct{}{}
		out = append(out, executor.ModelDescriptor{ID: e.cfg.Model, Name: e.cfg.Model, Default: true})
	}
	for _, m := range payload.Models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = strings.TrimSpace(m.Model)
		}
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, executor.ModelDescriptor{ID: name, Name: name})
	}
	return out, "ollama_tags", nil
}

func (e *Executor) Execute(ctx context.Context, req executor.ExecuteRequest, emit executor.EventHandler) (executor.ExecuteResult, error) {
	messages, err := e.buildMessages(req)
	if err != nil {
		execErr := &executor.ExecError{
			Kind:    executor.ErrExecution,
			Code:    "file_read_error",
			Message: err.Error(),
			Cause:   err,
		}
		return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
	}

	modelName := req.ProviderModel
	if modelName == "" {
		modelName = e.cfg.Model
	}

	bodyBytes, err := json.Marshal(map[string]any{
		"model":    modelName,
		"messages": messages,
		"stream":   true,
	})
	if err != nil {
		return executor.ExecuteResult{Status: "failed", ExitCode: -1}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.cfg.BaseURL, "/")+"/api/chat", bytes.NewReader(bodyBytes))
	if err != nil {
		return executor.ExecuteResult{Status: "failed", ExitCode: -1}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		execErr := classifyOllamaErr(err)
		if emit != nil {
			_ = emit(executor.Event{Type: executor.EventError, Text: execErr.Message, Timestamp: time.Now()})
		}
		status := "failed"
		if execErr.Kind == executor.ErrTimeout {
			status = "timed_out"
		} else if execErr.Kind == executor.ErrCancelled {
			status = "cancelled"
		}
		return executor.ExecuteResult{Status: status, ExitCode: -1, Error: execErr}, execErr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		bodyText := strings.TrimSpace(string(snippet))
		execErr := &executor.ExecError{
			Kind:    executor.ErrExecution,
			Code:    "ollama_error",
			Message: fmt.Sprintf("ollama returned HTTP %d: %s", resp.StatusCode, bodyText),
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			execErr.Kind = executor.ErrRateLimit
			execErr.Code = "ollama_rate_limited"
			if d, src := executor.ParseCooldown(bodyText, resp.Header, 0); d > 0 && src == "parsed" {
				execErr.RetryAfterSeconds = int64(math.Ceil(d.Seconds()))
				execErr.CooldownSource = src
			}
		} else if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusBadGateway {
			execErr.Kind = executor.ErrUnavailable
			execErr.Code = "executor_unavailable"
		}
		if emit != nil {
			_ = emit(executor.Event{Type: executor.EventError, Text: execErr.Message, Timestamp: time.Now()})
		}
		return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
	}

	if emit != nil {
		if err := emit(executor.Event{Type: executor.EventStarted, Timestamp: time.Now()}); err != nil {
			return executor.ExecuteResult{Status: "failed"}, err
		}
	}

	var (
		answer strings.Builder
		usage  executor.Usage
	)

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	var lineBuf []byte
	var totalRead int64
	maxOut := req.Settings.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = 16 * 1024 * 1024
	}

	for {
		frag, readErr := br.ReadSlice('\n')
		totalRead += int64(len(frag))
		if totalRead > maxOut {
			execErr := &executor.ExecError{
				Kind:    executor.ErrExecution,
				Code:    "output_limit_exceeded",
				Message: "ollama response exceeded output limit",
			}
			return executor.ExecuteResult{Status: "failed", Error: execErr}, execErr
		}
		lineBuf = append(lineBuf, frag...)
		if readErr == nil || errors.Is(readErr, io.EOF) {
			trimmed := bytes.TrimSpace(lineBuf)
			lineBuf = lineBuf[:0]
			if len(trimmed) > 0 {
				var chunk ollamaChatChunk
				if err := json.Unmarshal(trimmed, &chunk); err == nil {
					if chunk.Error != "" {
						execErr := &executor.ExecError{
							Kind:    executor.ErrExecution,
							Code:    "ollama_error",
							Message: chunk.Error,
						}
						if emit != nil {
							_ = emit(executor.Event{Type: executor.EventError, Text: execErr.Message, Timestamp: time.Now()})
						}
						return executor.ExecuteResult{Status: "failed", Error: execErr}, execErr
					}
					if chunk.Message.Content != "" {
						answer.WriteString(chunk.Message.Content)
						if emit != nil {
							if err := emit(executor.Event{
								Type:      executor.EventTextDelta,
								Text:      chunk.Message.Content,
								RawType:   "ollama_chunk",
								Timestamp: time.Now(),
							}); err != nil {
								return executor.ExecuteResult{Status: "failed"}, err
							}
						}
					}
					if chunk.Done {
						usage = executor.Usage{
							InputTokens:  chunk.PromptEvalCount,
							OutputTokens: chunk.EvalCount,
							TotalTokens:  chunk.PromptEvalCount + chunk.EvalCount,
							Available:    chunk.PromptEvalCount > 0 || chunk.EvalCount > 0,
						}
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		execErr := classifyOllamaErr(readErr)
		return executor.ExecuteResult{Status: "failed", Error: execErr}, execErr
	}

	if emit != nil {
		if err := emit(executor.Event{
			Type:      executor.EventCompleted,
			Usage:     &usage,
			Timestamp: time.Now(),
		}); err != nil {
			return executor.ExecuteResult{Status: "failed"}, err
		}
	}

	return executor.ExecuteResult{
		Answer: answer.String(),
		Status: "completed",
		Usage:  usage,
	}, nil
}

func (e *Executor) buildMessages(req executor.ExecuteRequest) ([]map[string]string, error) {
	var msgs []map[string]string
	if req.Instructions != "" {
		msgs = append(msgs, map[string]string{
			"role":    "system",
			"content": req.Instructions,
		})
	}

	var userContent strings.Builder
	if req.Session.HistoryFile != "" {
		data, err := executor.ReadWorkDirFile(req.WorkDir, req.Session.HistoryFile)
		if err != nil {
			return nil, fmt.Errorf("read history context: %w", err)
		}
		userContent.WriteString("<conversation_context>\n")
		userContent.Write(data)
		userContent.WriteString("\n</conversation_context>\n\n")
	}

	for _, f := range req.Files {
		if f.RelativePath == req.Session.HistoryFile {
			continue
		}
		data, err := executor.ReadWorkDirFile(req.WorkDir, f.RelativePath)
		if err != nil {
			return nil, fmt.Errorf("read file %s: %w", f.Name, err)
		}
		userContent.WriteString(fmt.Sprintf("<input_file name=%q>\n", f.Name))
		userContent.Write(data)
		userContent.WriteString("\n</input_file>\n\n")
	}

	if req.Prompt != "" {
		userContent.WriteString(req.Prompt)
	}

	msgs = append(msgs, map[string]string{
		"role":    "user",
		"content": strings.TrimSpace(userContent.String()),
	})
	return msgs, nil
}

func classifyOllamaErr(err error) *executor.ExecError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &executor.ExecError{
			Kind:    executor.ErrTimeout,
			Code:    "ollama_timeout",
			Message: "ollama request timed out",
			Cause:   err,
		}
	}
	if errors.Is(err, context.Canceled) {
		return &executor.ExecError{
			Kind:    executor.ErrCancelled,
			Code:    "request_cancelled",
			Message: "request cancelled",
			Cause:   err,
		}
	}
	return &executor.ExecError{
		Kind:    executor.ErrUnavailable,
		Code:    "ollama_unavailable",
		Message: fmt.Sprintf("ollama connection error: %v", err),
		Cause:   err,
	}
}

type ollamaChatChunk struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	Error           string `json:"error"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
}
