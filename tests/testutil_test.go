package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cli-ai-gateway/internal/api"
	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/defaults"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "mock-codex":
		runMockCodex()
	case "mock-claude":
		runMockClaude()
	case "mock-gemini":
		runMockGemini()
	case "mock-agy":
		runMockAgy()
	default:
		os.Exit(m.Run())
	}
}

func readInputFiles(workDir string) (promptMD string, contextMD string, extraFiles []string) {
	inDir := filepath.Join(workDir, "input")
	entries, err := os.ReadDir(inDir)
	if err != nil {
		return "", "", nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(inDir, e.Name()))
		if err != nil {
			continue
		}
		switch e.Name() {
		case "prompt.md":
			promptMD = string(data)
		case "context.md":
			contextMD = string(data)
		default:
			extraFiles = append(extraFiles, e.Name()+"="+strconv.Itoa(len(data)))
		}
	}
	return promptMD, contextMD, extraFiles
}

func extractChildPIDFile(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "CHILD_PID_FILE="); idx >= 0 {
			rest := line[idx+len("CHILD_PID_FILE="):]
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func spawnSleepChild(pidFile string) int {
	if pidFile == "" {
		return 0
	}
	attr := &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}
	proc, err := os.StartProcess("/bin/sleep", []string{"/bin/sleep", "30"}, attr)
	if err != nil {
		// Fallback to re-exec self in child mode if /bin/sleep is unavailable.
		return 0
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(proc.Pid)), 0o600)
	return proc.Pid
}

func summarizeTextMarkers(s string) string {
	if s == "" {
		return "none"
	}
	hasStart := strings.Contains(s, "START_MARKER_Alpha")
	hasMid := strings.Contains(s, "MID_MARKER_Beta")
	hasEnd := strings.Contains(s, "END_MARKER_Gamma")
	return fmt.Sprintf("len=%d start=%v mid=%v end=%v", len(s), hasStart, hasMid, hasEnd)
}

func runMockCodex() {
	stdinBytes, _ := io.ReadAll(os.Stdin)
	stdin := string(stdinBytes)
	workDir, _ := os.Getwd()
	promptMD, contextMD, extraFiles := readInputFiles(workDir)
	combined := stdin + "\n" + promptMD
	joinedArgs := strings.Join(os.Args[1:], " ")

	if strings.Contains(combined, "TRIGGER_MIDSTREAM_FAIL") {
		fmt.Println(`{"type":"thread.started","thread_id":"thr_fail_1"}`)
		fmt.Println(`{"type":"item.delta","delta":"partial-before-error "}`)
		_ = os.Stdout.Sync()
		time.Sleep(30 * time.Millisecond)
		fmt.Println(`{"type":"turn.failed","error":{"message":"simulated mid-stream failure"}}`)
		os.Exit(1)
	}

	if strings.Contains(combined, "TRIGGER_SLEEP_AND_CHILD") {
		pidFile := extractChildPIDFile(combined)
		childPID := spawnSleepChild(pidFile)
		fmt.Printf(`{"type":"thread.started","thread_id":"thr_sleep_%d"}`+"\n", childPID)
		fmt.Printf(`{"type":"item.delta","delta":"started child=%d "}`+"\n", childPID)
		time.Sleep(15 * time.Second)
		os.Exit(0)
	}

	if strings.Contains(combined, "TRIGGER_SLOW_FILE") {
		time.Sleep(800 * time.Millisecond)
	}

	var answer string
	if strings.Contains(combined, "TRIGGER_ENV_DUMP") {
		answer = "ENV_DUMP:\n" + strings.Join(os.Environ(), "\n")
	} else {
		isResume := strings.HasPrefix(joinedArgs, "exec resume ")
		hasContext := contextMD != ""
		answer = fmt.Sprintf(
			"provider=codex resume=%v has_context=%v context_len=%d prompt_md=[%s] extra=[%s] args=[%s] stdin=[%s]",
			isResume,
			hasContext,
			len(contextMD),
			summarizeTextMarkers(promptMD),
			strings.Join(extraFiles, ","),
			joinedArgs,
			strings.ReplaceAll(strings.TrimSpace(stdin), "\n", "|"),
		)
	}

	thrID := "thr_codex_sess_1"
	if strings.HasPrefix(joinedArgs, "exec resume ") {
		thrID = "thr_codex_sess_1"
	}
	fmt.Printf(`{"type":"thread.started","thread_id":%q}`+"\n", thrID)
	half := len(answer) / 2
	if half > 0 {
		d1, _ := json.Marshal(map[string]any{"type": "item.delta", "delta": answer[:half]})
		fmt.Println(string(d1))
	}
	d2, _ := json.Marshal(map[string]any{"type": "item.delta", "delta": answer[half:]})
	fmt.Println(string(d2))
	item, _ := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type": "agent_message",
			"text": answer,
		},
	})
	fmt.Println(string(item))
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":15,"output_tokens":8}}`)
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
	workDir, _ := os.Getwd()
	promptMD, contextMD, _ := readInputFiles(workDir)
	joinedArgs := strings.Join(os.Args[1:], " ")

	answer := fmt.Sprintf(
		"provider=claude has_context=%v context_len=%d context_snippet=%q prompt_md=[%s] args=[%s] prompt=[%s]",
		contextMD != "",
		len(contextMD),
		truncate(contextMD, 120),
		summarizeTextMarkers(promptMD),
		joinedArgs,
		strings.ReplaceAll(promptText, "\n", "|"),
	)

	fmt.Println(`{"type":"system","subtype":"init","session_id":"claude_sess_1"}`)
	deltaEv, _ := json.Marshal(map[string]any{
		"type":       "stream_event",
		"session_id": "claude_sess_1",
		"event": map[string]any{
			"delta": map[string]any{"type": "text_delta", "text": answer},
		},
	})
	fmt.Println(string(deltaEv))
	resEv, _ := json.Marshal(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"session_id": "claude_sess_1",
		"result":     answer,
		"usage":      map[string]any{"input_tokens": 20, "output_tokens": 12},
	})
	fmt.Println(string(resEv))
	os.Exit(0)
}

func runMockGemini() {
	workDir, _ := os.Getwd()
	promptMD, contextMD, _ := readInputFiles(workDir)
	joinedArgs := strings.Join(os.Args[1:], " ")
	answer := fmt.Sprintf(
		"provider=gemini has_context=%v prompt_md=[%s] args=[%s]",
		contextMD != "",
		summarizeTextMarkers(promptMD),
		joinedArgs,
	)
	fmt.Println(`{"type":"init","session_id":"gem_sess_1"}`)
	msg, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "delta": answer})
	fmt.Println(string(msg))
	res, _ := json.Marshal(map[string]any{
		"type":       "result",
		"status":     "success",
		"session_id": "gem_sess_1",
		"response":   answer,
		"stats":      map[string]any{"tokens": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}},
	})
	fmt.Println(string(res))
	os.Exit(0)
}

func runMockAgy() {
	stdinBytes, _ := io.ReadAll(os.Stdin)
	var in struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(stdinBytes, &in)
	workDir, _ := os.Getwd()
	promptMD, contextMD, _ := readInputFiles(workDir)
	answer := fmt.Sprintf(
		"provider=antigravity has_context=%v prompt_md=[%s] content=[%s]",
		contextMD != "",
		summarizeTextMarkers(promptMD),
		strings.ReplaceAll(in.Message.Content, "\n", "|"),
	)
	fmt.Println(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"agy "}}`)
	res, _ := json.Marshal(map[string]any{
		"event": "result",
		"result": map[string]any{
			"status":          "SUCCESS",
			"response":        answer,
			"conversation_id": "agy_conv_1",
			"usage":           map[string]any{"input_tokens": 9, "output_tokens": 4},
		},
	})
	fmt.Println(string(res))
	os.Exit(0)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

type harnessOptions struct {
	MaxFileBytes              int64
	QuotaBytes                int64
	MaxGlobalProcesses        int
	MaxQueueSize              int
	QueueWaitTimeoutSeconds   int
	SessionLockTimeoutSeconds int
	DefaultTimeoutSeconds     int
	IncludeBrokenCLI          bool
	DisableAdmin              bool
	CleanupRunInput           *bool
	ExistingDataDir           string
}

type recordedOllamaReq struct {
	Model    string              `json:"model"`
	Messages []map[string]string `json:"messages"`
}

type testHarness struct {
	Server     *httptest.Server
	URL        string
	DataDir    string
	Config     *config.Config
	DB         *storage.DB
	Files      *storage.FileStore
	Sessions   *session.Manager
	Auth       *auth.Store
	Registry   *executor.Registry
	ProcMgr    *process.Manager
	OllamaURL  string
	OllamaReqs func() []recordedOllamaReq
}

func linkMockBin(t *testing.T, dir, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, name)
	_ = os.Remove(target)
	if err := os.Link(self, target); err != nil {
		if err := os.Symlink(self, target); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func newTestHarness(t *testing.T, opts harnessOptions) *testHarness {
	t.Helper()

	dataDir := opts.ExistingDataDir
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	binDir := t.TempDir()
	codexBin := linkMockBin(t, binDir, "mock-codex")
	claudeBin := linkMockBin(t, binDir, "mock-claude")
	geminiBin := linkMockBin(t, binDir, "mock-gemini")
	agyBin := linkMockBin(t, binDir, "mock-agy")

	var (
		mu         sync.Mutex
		ollamaReqs []recordedOllamaReq
	)
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5-coder:7b"}]}`))
		case "/api/chat":
			var req recordedOllamaReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			ollamaReqs = append(ollamaReqs, req)
			mu.Unlock()

			userContent := ""
			if len(req.Messages) > 0 {
				userContent = req.Messages[len(req.Messages)-1]["content"]
			}
			hasCtx := strings.Contains(userContent, "<conversation_context>")
			reply := fmt.Sprintf("provider=ollama has_context=%v chars=%d", hasCtx, len(userContent))
			w.Header().Set("Content-Type", "application/x-ndjson")
			chunk1, _ := json.Marshal(map[string]any{
				"message": map[string]string{"role": "assistant", "content": reply[:len(reply)/2]},
				"done":    false,
			})
			chunk2, _ := json.Marshal(map[string]any{
				"message":           map[string]string{"role": "assistant", "content": reply[len(reply)/2:]},
				"done":              true,
				"prompt_eval_count": 25,
				"eval_count":        10,
			})
			_, _ = fmt.Fprintln(w, string(chunk1))
			_, _ = fmt.Fprintln(w, string(chunk2))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ollamaSrv.Close)

	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 5 * 1024 * 1024
	}
	if opts.QuotaBytes <= 0 {
		opts.QuotaBytes = 50 * 1024 * 1024
	}
	if opts.MaxGlobalProcesses <= 0 {
		opts.MaxGlobalProcesses = 8
	}
	if opts.MaxQueueSize <= 0 {
		opts.MaxQueueSize = 8
	}
	if opts.QueueWaitTimeoutSeconds <= 0 {
		opts.QueueWaitTimeoutSeconds = 5
	}
	if opts.SessionLockTimeoutSeconds <= 0 {
		opts.SessionLockTimeoutSeconds = 5
	}
	if opts.DefaultTimeoutSeconds <= 0 {
		opts.DefaultTimeoutSeconds = 10
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:                   "127.0.0.1:0",
			ReadHeaderTimeoutSeconds: 10,
			ShutdownTimeoutSeconds:   5,
		},
		Storage: config.StorageConfig{
			Path:                   dataDir,
			SQLitePath:             filepath.Join(dataDir, "gateway.db"),
			MaxFileBytes:           opts.MaxFileBytes,
			QuotaBytes:             opts.QuotaBytes,
			FileTTLSeconds:         3600,
			RunTTLSeconds:          3600,
			SessionTTLSeconds:      3600,
			CleanupIntervalSeconds: 300,
			CleanupRunInput:        opts.CleanupRunInput,
		},
		Limits: config.LimitsConfig{
			MaxGlobalProcesses:        opts.MaxGlobalProcesses,
			MaxExecutorProcesses:      opts.MaxGlobalProcesses,
			MaxUserProcesses:          opts.MaxGlobalProcesses,
			MaxQueueSize:              opts.MaxQueueSize,
			QueueWaitTimeoutSeconds:   opts.QueueWaitTimeoutSeconds,
			SessionLockTimeoutSeconds: opts.SessionLockTimeoutSeconds,
			MaxOutputBytes:            2 * 1024 * 1024,
			MaxFilesPerTask:           5,
			DefaultTimeoutSeconds:     opts.DefaultTimeoutSeconds,
			MaxTimeoutSeconds:         60,
		},
		Logging: config.LoggingConfig{Level: "error"},
		Admin: config.AdminConfig{
			Enabled: !opts.DisableAdmin,
			Token:   "admin-secret",
		},
		Auth: config.AuthConfig{
			Keys: []config.APIKeyConfig{
				{ID: "key-alice", OwnerID: "alice", Key: "alice-secret", AllowedModels: []string{"*"}, MaxConcurrent: opts.MaxGlobalProcesses},
				{ID: "key-bob", OwnerID: "bob", Key: "bob-secret", AllowedModels: []string{"*"}, MaxConcurrent: opts.MaxGlobalProcesses},
				{ID: "key-restricted", OwnerID: "carol", Key: "carol-secret", AllowedModels: []string{"codex-cli"}, MaxConcurrent: opts.MaxGlobalProcesses},
				{ID: "key-revoked", OwnerID: "dave", Key: "dave-revoked", AllowedModels: []string{"*"}, Revoked: true},
			},
		},
		Models: []config.ModelConfig{
			{
				ID:             "codex-cli",
				Provider:       "codex",
				Executable:     codexBin,
				Model:          "gpt-5.3-codex",
				TimeoutSeconds: opts.DefaultTimeoutSeconds,
				SandboxMode:    "read-only",
				FSAccess:       "workspace-read",
				MaxConcurrent:  opts.MaxGlobalProcesses,
				Env:            map[string]string{"CODEX_ISOLATED_TOKEN": "codex-only-secret"},
			},
			{
				ID:             "claude-cli",
				Provider:       "claude",
				Executable:     claudeBin,
				Model:          "sonnet",
				TimeoutSeconds: opts.DefaultTimeoutSeconds,
				FSAccess:       "workspace-read",
				MaxConcurrent:  opts.MaxGlobalProcesses,
				Env:            map[string]string{"CLAUDE_ISOLATED_TOKEN": "claude-only-secret"},
			},
			{
				ID:             "gemini-cli",
				Provider:       "gemini",
				Executable:     geminiBin,
				Model:          "gemini-2.5-pro",
				TimeoutSeconds: opts.DefaultTimeoutSeconds,
				FSAccess:       "workspace-read",
				MaxConcurrent:  opts.MaxGlobalProcesses,
			},
			{
				ID:             "agy-cli",
				Provider:       "antigravity",
				Executable:     agyBin,
				Model:          "gemini-3-pro",
				TimeoutSeconds: opts.DefaultTimeoutSeconds,
				FSAccess:       "workspace-read",
				MaxConcurrent:  opts.MaxGlobalProcesses,
			},
			{
				ID:             "ollama-local",
				Provider:       "ollama",
				BaseURL:        ollamaSrv.URL,
				Model:          "qwen2.5-coder:7b",
				TimeoutSeconds: opts.DefaultTimeoutSeconds,
				FSAccess:       "workspace-read",
				MaxConcurrent:  opts.MaxGlobalProcesses,
			},
		},
	}

	if opts.IncludeBrokenCLI {
		cfg.Models = append(cfg.Models, config.ModelConfig{
			ID:             "broken-cli",
			Provider:       "codex",
			Executable:     "/nonexistent/binary/does-not-exist",
			TimeoutSeconds: opts.DefaultTimeoutSeconds,
			FSAccess:       "workspace-read",
			MaxConcurrent:  1,
		})
	}

	ctx := context.Background()
	db, err := storage.OpenSQLite(ctx, cfg.Storage.SQLitePath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	authStore, err := auth.NewStore(ctx, db, cfg.Auth.Keys)
	if err != nil {
		t.Fatalf("auth.NewStore: %v", err)
	}
	fileStore, err := storage.NewFileStore(db, dataDir, cfg.Storage.MaxFileBytes, cfg.Storage.QuotaBytes, cfg.Storage.FileTTL())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessMgr, err := session.NewManager(db, dataDir, cfg.Limits.SessionLockTimeout(), cfg.Storage.RunTTL(), cfg.Storage.SessionTTL(), cfg.Storage.EffectiveCleanupRunInput())
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	procMgr := process.NewManager(process.Limits{
		MaxGlobal:        cfg.Limits.MaxGlobalProcesses,
		MaxPerExecutor:   cfg.Limits.MaxExecutorProcesses,
		MaxPerUser:       cfg.Limits.MaxUserProcesses,
		MaxQueueSize:     cfg.Limits.MaxQueueSize,
		QueueWaitTimeout: cfg.Limits.QueueWaitTimeout(),
		DefaultMaxOutput: cfg.Limits.MaxOutputBytes,
	})
	registry := executor.NewRegistry()
	defaults.RegisterAll(registry, procMgr)
	if err := registry.RegisterModels(cfg.Models); err != nil {
		t.Fatalf("RegisterModels: %v", err)
	}
	if err := registry.ApplyOverrides(ctx, db.SQL()); err != nil {
		t.Fatalf("ApplyOverrides: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	handler := api.NewRouter(api.Dependencies{
		Config:    cfg,
		StartedAt: time.Now(),
		Logger:    logger,
		DB:        db,
		Auth:      authStore,
		Files:     fileStore,
		Sessions:  sessMgr,
		Registry:  registry,
		ProcMgr:   procMgr,
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return &testHarness{
		Server:    ts,
		URL:       ts.URL,
		DataDir:   dataDir,
		Config:    cfg,
		DB:        db,
		Files:     fileStore,
		Sessions:  sessMgr,
		Auth:      authStore,
		Registry:  registry,
		ProcMgr:   procMgr,
		OllamaURL: ollamaSrv.URL,
		OllamaReqs: func() []recordedOllamaReq {
			mu.Lock()
			defer mu.Unlock()
			cp := make([]recordedOllamaReq, len(ollamaReqs))
			copy(cp, ollamaReqs)
			return cp
		},
	}
}

type sseFrame struct {
	Event string
	Data  string
}

func readSSEFrames(t *testing.T, r io.Reader) []sseFrame {
	t.Helper()
	br := bufio.NewReader(r)
	var frames []sseFrame
	var curEvent string
	var curData []string
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if curEvent != "" || len(curData) > 0 {
					frames = append(frames, sseFrame{
						Event: curEvent,
						Data:  strings.Join(curData, "\n"),
					})
					curEvent = ""
					curData = nil
				}
			} else if strings.HasPrefix(trimmed, "event:") {
				curEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			} else if strings.HasPrefix(trimmed, "data:") {
				curData = append(curData, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			}
		}
		if err != nil {
			if err == io.EOF {
				if curEvent != "" || len(curData) > 0 {
					frames = append(frames, sseFrame{
						Event: curEvent,
						Data:  strings.Join(curData, "\n"),
					})
				}
				break
			}
			t.Fatalf("read SSE: %v", err)
		}
	}
	return frames
}
