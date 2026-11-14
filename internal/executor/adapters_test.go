package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/antigravity"
	"cli-ai-gateway/internal/executor/claude"
	"cli-ai-gateway/internal/executor/codex"
	"cli-ai-gateway/internal/executor/gemini"
	"cli-ai-gateway/internal/executor/ollama"
	"cli-ai-gateway/internal/process"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "mock-codex":
		runMockCodex()
	case "mock-agy":
		runMockAgy()
	case "mock-gemini":
		runMockGemini()
	case "mock-claude":
		runMockClaude()
	default:
		os.Exit(m.Run())
	}
}

func runMockCodex() {
	stdinBytes, _ := io.ReadAll(os.Stdin)
	stdin := string(stdinBytes)
	joinedArgs := strings.Join(os.Args[1:], " ")

	if strings.Contains(stdin, "TRIGGER_RATE_LIMIT") {
		fmt.Println(`{"type":"thread.started","thread_id":"thr_rate_1"}`)
		fmt.Println(`{"type":"turn.failed","error":{"message":"rate limit 429 exceeded"}}`)
		os.Exit(1)
	}

	threadID := "thr_codex_100"
	if strings.HasPrefix(joinedArgs, "exec resume ") {
		threadID = "thr_codex_resumed"
	}
	fmt.Printf(`{"type":"thread.started","thread_id":%q}`+"\n", threadID)
	fmt.Println(`{"type":"item.delta","delta":"codex-part1 "}`)
	msg := fmt.Sprintf("args=[%s] stdin=[%s]", joinedArgs, strings.TrimSpace(stdin))
	outItem, _ := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type": "agent_message",
			"text": "codex-part1 " + msg,
		},
	})
	fmt.Println(string(outItem))
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":12,"cached_input_tokens":3,"output_tokens":7,"reasoning_output_tokens":2}}`)
	os.Exit(0)
}

func runMockAgy() {
	stdinBytes, _ := io.ReadAll(os.Stdin)
	var msg struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(stdinBytes, &msg)

	if strings.Contains(msg.Message.Content, "TRIGGER_AUTH_ERR") {
		ev, _ := json.Marshal(map[string]any{
			"event": "result",
			"result": map[string]any{
				"status":          "FAILED",
				"error":           "authentication failed: please run agy login",
				"conversation_id": "conv_err_1",
			},
		})
		fmt.Println(string(ev))
		os.Exit(1)
	}

	fmt.Println(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"agy-delta "}}`)
	joinedArgs := strings.Join(os.Args[1:], " ")
	res, _ := json.Marshal(map[string]any{
		"event": "result",
		"result": map[string]any{
			"status":          "SUCCESS",
			"response":        "agy-delta args=[" + joinedArgs + "] content=[" + msg.Message.Content + "]",
			"conversation_id": "conv_agy_77",
			"usage": map[string]any{
				"input_tokens":    20,
				"output_tokens":   10,
				"thinking_tokens": 5,
			},
		},
	})
	fmt.Println(string(res))
	os.Exit(0)
}

func runMockGemini() {
	joinedArgs := strings.Join(os.Args[1:], " ")
	fmt.Println(`{"type":"init","session_id":"gem_sess_42"}`)
	fmt.Println(`{"type":"message","role":"assistant","delta":"gem-chunk "}`)
	res, _ := json.Marshal(map[string]any{
		"type":       "result",
		"status":     "success",
		"session_id": "gem_sess_42",
		"response":   "gem-chunk args=[" + joinedArgs + "]",
		"stats": map[string]any{
			"tokens": map[string]any{
				"input_tokens":  11,
				"output_tokens": 9,
				"total_tokens":  20,
			},
		},
	})
	fmt.Println(string(res))
	os.Exit(0)
}

func runMockClaude() {
	stdinBytes, _ := io.ReadAll(os.Stdin)
	var in struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(stdinBytes, &in)
	promptText := ""
	if len(in.Message.Content) > 0 {
		promptText = in.Message.Content[0].Text
	}
	joinedArgs := strings.Join(os.Args[1:], " ")

	fmt.Println(`{"type":"system","subtype":"init","session_id":"claude_sess_99"}`)
	fmt.Println(`{"type":"stream_event","session_id":"claude_sess_99","event":{"delta":{"type":"text_delta","text":"claude-delta "}}}`)
	res, _ := json.Marshal(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"session_id": "claude_sess_99",
		"result":     "claude-delta args=[" + joinedArgs + "] prompt=[" + promptText + "]",
		"usage": map[string]any{
			"input_tokens":  30,
			"output_tokens": 14,
		},
	})
	fmt.Println(string(res))
	os.Exit(0)
}

func linkMock(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), name)
	if err := os.Link(self, target); err != nil {
		if err := os.Symlink(self, target); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestCodexAdapter(t *testing.T) {
	mgr := process.NewManager(process.Limits{})
	exe := codex.New(config.ModelConfig{
		ID:          "codex-cli",
		Provider:    "codex",
		Executable:  linkMock(t, "mock-codex"),
		Model:       "gpt-5.3-codex",
		SandboxMode: "read-only",
	}, mgr)

	var events []executor.Event
	res, err := exe.Execute(context.Background(), executor.ExecuteRequest{
		RunID:        "run_1",
		WorkDir:      t.TempDir(),
		Instructions: "Be concise.",
		Files: []executor.ExecuteFile{
			{RelativePath: "./input/prompt.md"},
		},
		Session: executor.SessionContext{GatewaySessionID: "sess_1"},
	}, func(ev executor.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("codex Execute failed: %v", err)
	}
	if res.ProviderSessionID != "thr_codex_100" {
		t.Fatalf("unexpected thread ID: %q", res.ProviderSessionID)
	}
	if res.Usage.InputTokens != 15 || res.Usage.OutputTokens != 9 || res.Usage.TotalTokens != 24 {
		t.Fatalf("unexpected usage: %+v", res.Usage)
	}
	if !strings.Contains(res.Answer, "--ignore-rules") || !strings.Contains(res.Answer, "./input/prompt.md") {
		t.Fatalf("expected CLI flags & file instruction in mock echo, got %q", res.Answer)
	}
	if len(events) < 3 {
		t.Fatalf("expected started, text_delta, completed events, got %d", len(events))
	}

	// Resume invocation uses exec resume <providerSessionID> -
	res2, err := exe.Execute(context.Background(), executor.ExecuteRequest{
		RunID:   "run_2",
		WorkDir: t.TempDir(),
		Prompt:  "follow up",
		Session: executor.SessionContext{
			GatewaySessionID:  "sess_1",
			ProviderSessionID: "thr_codex_100",
			IsResume:          true,
		},
	}, nil)
	if err != nil {
		t.Fatalf("codex resume Execute failed: %v", err)
	}
	if !strings.Contains(res2.Answer, "exec resume") || !strings.Contains(res2.Answer, "thr_codex_100 -") {
		t.Fatalf("expected resume args, got %q", res2.Answer)
	}

	// Rate limit error classification.
	_, err = exe.Execute(context.Background(), executor.ExecuteRequest{
		RunID:   "run_3",
		WorkDir: t.TempDir(),
		Prompt:  "TRIGGER_RATE_LIMIT",
	}, nil)
	var execErr *executor.ExecError
	if !errors.As(err, &execErr) || execErr.Kind != executor.ErrRateLimit {
		t.Fatalf("expected ErrRateLimit, got %v", err)
	}
}

func TestAntigravityGeminiClaudeAdapters(t *testing.T) {
	mgr := process.NewManager(process.Limits{})

	// Antigravity
	agyExe := antigravity.New(config.ModelConfig{
		ID:         "agy",
		Provider:   "antigravity",
		Executable: linkMock(t, "mock-agy"),
		Model:      "gemini-3-pro",
	}, mgr)
	resAgy, err := agyExe.Execute(context.Background(), executor.ExecuteRequest{
		WorkDir:      t.TempDir(),
		Instructions: "sys",
		Prompt:       "hello agy",
		Session: executor.SessionContext{
			ProviderSessionID: "conv_prev",
			IsResume:          true,
		},
	}, nil)
	if err != nil {
		t.Fatalf("agy Execute: %v", err)
	}
	if resAgy.ProviderSessionID != "conv_agy_77" || resAgy.Usage.TotalTokens != 35 {
		t.Fatalf("unexpected agy result: %+v", resAgy)
	}
	if !strings.Contains(resAgy.Answer, "--conversation conv_prev") {
		t.Fatalf("expected --conversation resume flag, got %q", resAgy.Answer)
	}
	_, err = agyExe.Execute(context.Background(), executor.ExecuteRequest{
		WorkDir: t.TempDir(),
		Prompt:  "TRIGGER_AUTH_ERR",
	}, nil)
	var execErr *executor.ExecError
	if !errors.As(err, &execErr) || execErr.Kind != executor.ErrAuth {
		t.Fatalf("expected ErrAuth from agy, got %v", err)
	}

	// Gemini
	gemExe := gemini.New(config.ModelConfig{
		ID:           "gemini",
		Provider:     "gemini",
		Executable:   linkMock(t, "mock-gemini"),
		Model:        "gemini-2.5-pro",
		AllowedTools: []string{"read_file"},
	}, mgr)
	resGem, err := gemExe.Execute(context.Background(), executor.ExecuteRequest{
		WorkDir: t.TempDir(),
		Prompt:  "hello gem",
		Session: executor.SessionContext{
			ProviderSessionID: "gem_old",
			IsResume:          true,
		},
	}, nil)
	if err != nil {
		t.Fatalf("gemini Execute: %v", err)
	}
	if resGem.ProviderSessionID != "gem_sess_42" || resGem.Usage.TotalTokens != 20 {
		t.Fatalf("unexpected gemini result: %+v", resGem)
	}
	if !strings.Contains(resGem.Answer, "--resume gem_old") || !strings.Contains(resGem.Answer, "--allowed-tools read_file") {
		t.Fatalf("unexpected gemini args: %q", resGem.Answer)
	}

	// Claude
	clExe := claude.New(config.ModelConfig{
		ID:         "claude",
		Provider:   "claude",
		Executable: linkMock(t, "mock-claude"),
		Model:      "sonnet",
	}, mgr)
	resCl, err := clExe.Execute(context.Background(), executor.ExecuteRequest{
		WorkDir:      t.TempDir(),
		Instructions: "Follow rules",
		Prompt:       "hello claude",
		Session: executor.SessionContext{
			GatewaySessionID: "gw_1",
		},
	}, nil)
	if err != nil {
		t.Fatalf("claude Execute: %v", err)
	}
	if resCl.ProviderSessionID != "claude_sess_99" || resCl.Usage.TotalTokens != 44 {
		t.Fatalf("unexpected claude result: %+v", resCl)
	}
	if !strings.Contains(resCl.Answer, "--system-prompt-file") || !strings.Contains(resCl.Answer, "--session-id") {
		t.Fatalf("unexpected claude args: %q", resCl.Answer)
	}
}

func TestOllamaAdapterCheckAndStream(t *testing.T) {
	var gotBody struct {
		Model    string              `json:"model"`
		Stream   bool                `json:"stream"`
		Messages []map[string]string `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5-coder:7b"}]}`))
		case "/api/chat":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			flusher, _ := w.(http.Flusher)
			chunks := []string{
				`{"message":{"role":"assistant","content":"Hello "},"done":false}`,
				`{"message":{"role":"assistant","content":"from Ollama"},"done":true,"prompt_eval_count":18,"eval_count":6}`,
			}
			for _, c := range chunks {
				_, _ = fmt.Fprintln(w, c)
				if flusher != nil {
					flusher.Flush()
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	exe := ollama.New(config.ModelConfig{
		ID:             "local-ollama",
		Provider:       "ollama",
		BaseURL:        srv.URL,
		Model:          "qwen2.5-coder:7b",
		TimeoutSeconds: 10,
	})

	if exe.Capabilities().SessionResume {
		t.Fatal("ollama Capabilities().SessionResume must be false")
	}
	if err := exe.Check(context.Background()); err != nil {
		t.Fatalf("ollama Check failed: %v", err)
	}

	workDir := t.TempDir()
	inDir := filepath.Join(workDir, "input")
	if err := os.MkdirAll(inDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inDir, "context.md"), []byte("User: hi\nAssistant: hey"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inDir, "prompt.md"), []byte("# Task\nCheck staged content."), 0o600); err != nil {
		t.Fatal(err)
	}

	var streamed strings.Builder
	res, err := exe.Execute(context.Background(), executor.ExecuteRequest{
		WorkDir:      workDir,
		Instructions: "System directive",
		Prompt:       "extra",
		Files: []executor.ExecuteFile{
			{ID: "f1", Name: "prompt.md", RelativePath: "./input/prompt.md"},
		},
		Session: executor.SessionContext{
			HistoryFile: "./input/context.md",
		},
		Settings: executor.ExecutionSettings{Timeout: 5 * time.Second, Stream: true},
	}, func(ev executor.Event) error {
		if ev.Type == executor.EventTextDelta {
			streamed.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ollama Execute failed: %v", err)
	}
	if res.Answer != "Hello from Ollama" || streamed.String() != "Hello from Ollama" {
		t.Fatalf("unexpected answer=%q streamed=%q", res.Answer, streamed.String())
	}
	if res.Usage.InputTokens != 18 || res.Usage.OutputTokens != 6 || res.Usage.TotalTokens != 24 {
		t.Fatalf("unexpected ollama usage: %+v", res.Usage)
	}
	if len(gotBody.Messages) != 2 || gotBody.Messages[0]["role"] != "system" {
		t.Fatalf("unexpected ollama messages: %+v", gotBody.Messages)
	}
	userMsg := gotBody.Messages[1]["content"]
	if !strings.Contains(userMsg, "Assistant: hey") || !strings.Contains(userMsg, "Check staged content.") {
		t.Fatalf("expected staged context.md and prompt.md in user message, got %q", userMsg)
	}
}
