package worker_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/worker"
)

type stubExecutor struct {
	checkErr      error
	lastReq       executor.ExecuteRequest
	lastPromptMD  string
	lastExtra     string
	waitForCancel bool
	events        []executor.Event
	result        executor.ExecuteResult
	execErr       error
}

func (s *stubExecutor) Name() string { return "antigravity" }

func (s *stubExecutor) Capabilities() executor.Capabilities {
	return executor.Capabilities{
		Streaming:        true,
		SessionResume:    true,
		FileInput:        true,
		SupportedFormats: []string{"markdown", "text"},
		TokenUsage:       true,
	}
}

func (s *stubExecutor) Check(_ context.Context) error {
	return s.checkErr
}

func (s *stubExecutor) Execute(ctx context.Context, req executor.ExecuteRequest, emit executor.EventHandler) (executor.ExecuteResult, error) {
	s.lastReq = req
	for _, f := range req.Files {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return executor.ExecuteResult{Status: "failed", ExitCode: -1}, err
		}
		if f.RelativePath == "./input/prompt.md" {
			s.lastPromptMD = string(b)
		} else if f.RelativePath == "./input/notes.txt" {
			s.lastExtra = string(b)
		}
	}

	if s.waitForCancel {
		<-ctx.Done()
		err := &executor.ExecError{
			Kind:     executor.ErrCancelled,
			Code:     "cancelled",
			Message:  "execution cancelled",
			ExitCode: -1,
			Cause:    ctx.Err(),
		}
		return executor.ExecuteResult{Status: "cancelled", ExitCode: -1, Error: err}, err
	}

	for _, ev := range s.events {
		if emit != nil {
			if err := emit(ev); err != nil {
				return executor.ExecuteResult{Status: "cancelled", ExitCode: -1}, err
			}
		}
	}
	return s.result, s.execErr
}

func TestCleanRelativePath(t *testing.T) {
	valid := []string{"./input/prompt.md", "input/context.md", "notes.txt", "nested/dir/file.json"}
	for _, in := range valid {
		got, err := worker.CleanRelativePath(in)
		if err != nil || got == "" {
			t.Fatalf("expected valid path for %q, got %q, err=%v", in, got, err)
		}
	}

	invalid := []string{"", ".", "..", "../secret", "input/../../etc/passwd", "/etc/passwd", `C:\foo`, `a\b`, "bad\x00path", "~/secret"}
	for _, in := range invalid {
		if got, err := worker.CleanRelativePath(in); err == nil {
			t.Fatalf("expected error for unsafe path %q, got %q", in, got)
		}
	}
}

func TestWorkerServerAndRemoteExecutor(t *testing.T) {
	stub := &stubExecutor{
		events: []executor.Event{
			{Type: executor.EventStarted, SessionID: "prov-sess-1", Timestamp: time.Now()},
			{Type: executor.EventTextDelta, Text: "Hello ", Timestamp: time.Now()},
			{Type: executor.EventTextDelta, Text: "from container!", Timestamp: time.Now()},
		},
		result: executor.ExecuteResult{
			Answer:            "Hello from container!",
			ProviderSessionID: "prov-sess-1",
			Status:            "completed",
			Usage:             executor.Usage{InputTokens: 12, OutputTokens: 5, TotalTokens: 17, Available: true},
			ExitCode:          0,
		},
	}

	srv, err := worker.NewServer(worker.ServerConfig{
		Token:      "secret-worker-token",
		Provider:   "antigravity",
		Executable: "/usr/local/bin/agy",
		Executor:   stub,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	badClient, err := worker.NewExecutor(config.ModelConfig{
		ID:          "agy-1",
		Provider:    "antigravity",
		WorkerURL:   ts.URL,
		WorkerToken: "wrong-token",
	}, stub.Capabilities(), "")
	if err != nil {
		t.Fatalf("NewExecutor badClient: %v", err)
	}
	if err := badClient.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 auth failure, got %v", err)
	}

	client, err := worker.NewExecutor(config.ModelConfig{
		ID:          "agy-1",
		Provider:    "antigravity",
		WorkerURL:   ts.URL,
		WorkerToken: "secret-worker-token",
	}, stub.Capabilities(), "")
	if err != nil {
		t.Fatalf("NewExecutor client: %v", err)
	}
	if err := client.Check(context.Background()); err != nil {
		t.Fatalf("expected Check success, got %v", err)
	}

	// Stage two local files matching gateway workspace structure.
	localWork := t.TempDir()
	inDir := filepath.Join(localWork, "input")
	if err := os.MkdirAll(inDir, 0o700); err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(inDir, "prompt.md")
	notesPath := filepath.Join(inDir, "notes.txt")
	if err := os.WriteFile(promptPath, []byte("Summarize attached notes."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notesPath, []byte("container worker payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	var streamed strings.Builder
	res, err := client.Execute(context.Background(), executor.ExecuteRequest{
		RunID:         "run_1",
		Model:         "agy-1",
		ProviderModel: "gemini-3-pro",
		WorkDir:       localWork,
		Instructions:  "Be concise.",
		Prompt:        "Focus on key points.",
		Files: []executor.ExecuteFile{
			{Name: "prompt.md", Path: promptPath, RelativePath: "./input/prompt.md", Size: 25},
			{ID: "file_1", Name: "notes.txt", Path: notesPath, RelativePath: "./input/notes.txt", Size: 24},
		},
		Session: executor.SessionContext{
			GatewaySessionID:  "sess_1",
			ProviderSessionID: "prov-0",
			IsResume:          true,
		},
		Settings: executor.ExecutionSettings{
			Timeout:        30 * time.Second,
			MaxOutputBytes: 1024 * 1024,
			Stream:         true,
		},
	}, func(ev executor.Event) error {
		if ev.Type == executor.EventTextDelta {
			streamed.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.Answer != "Hello from container!" || streamed.String() != "Hello from container!" {
		t.Fatalf("unexpected answer/stream: res=%q streamed=%q", res.Answer, streamed.String())
	}
	if res.Usage.TotalTokens != 17 || res.ProviderSessionID != "prov-sess-1" {
		t.Fatalf("unexpected metadata: %+v", res)
	}
	if stub.lastPromptMD != "Summarize attached notes." || stub.lastExtra != "container worker payload" {
		t.Fatalf("staged file content mismatch: prompt=%q extra=%q", stub.lastPromptMD, stub.lastExtra)
	}
	if stub.lastReq.WorkDir == localWork || stub.lastReq.WorkDir == "" {
		t.Fatalf("expected isolated temp workDir on worker, got %q", stub.lastReq.WorkDir)
	}
	if _, err := os.Stat(stub.lastReq.WorkDir); !os.IsNotExist(err) {
		t.Fatalf("expected worker temp workDir %q to be cleaned up, err=%v", stub.lastReq.WorkDir, err)
	}
}

func TestWorkerLargeNDJSONLine(t *testing.T) {
	bigChunk := strings.Repeat("A", 180*1024) // > 64 KiB bufio.Scanner default limit
	stub := &stubExecutor{
		events: []executor.Event{
			{Type: executor.EventTextDelta, Text: bigChunk, Timestamp: time.Now()},
		},
		result: executor.ExecuteResult{
			Answer:   bigChunk,
			Status:   "completed",
			ExitCode: 0,
		},
	}

	srv, err := worker.NewServer(worker.ServerConfig{
		Token:    "tok",
		Provider: "claude",
		Executor: stub,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client, err := worker.NewExecutor(config.ModelConfig{
		ID:          "claude",
		Provider:    "claude",
		WorkerURL:   ts.URL,
		WorkerToken: "tok",
	}, stub.Capabilities(), "")
	if err != nil {
		t.Fatal(err)
	}

	var got strings.Builder
	res, err := client.Execute(context.Background(), executor.ExecuteRequest{
		RunID: "run_big",
		Model: "claude",
		Settings: executor.ExecutionSettings{
			Timeout: 10 * time.Second,
			Stream:  true,
		},
	}, func(ev executor.Event) error {
		got.WriteString(ev.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("Execute failed on large frame: %v", err)
	}
	if len(res.Answer) != len(bigChunk) || len(got.String()) != len(bigChunk) {
		t.Fatalf("unexpected chunk lengths: answer=%d streamed=%d", len(res.Answer), len(got.String()))
	}
}

func TestWorkerExecuteCancellationPropagates(t *testing.T) {
	stub := &stubExecutor{waitForCancel: true}
	srv, err := worker.NewServer(worker.ServerConfig{
		Token:    "tok",
		Provider: "codex",
		Executor: stub,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client, err := worker.NewExecutor(config.ModelConfig{
		ID:          "codex",
		Provider:    "codex",
		WorkerURL:   ts.URL,
		WorkerToken: "tok",
	}, stub.Capabilities(), "")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	res, err := client.Execute(ctx, executor.ExecuteRequest{
		RunID: "run_cancel",
		Model: "codex",
		Settings: executor.ExecutionSettings{
			Timeout: 10 * time.Second,
		},
	}, nil)
	if err == nil || res.Error == nil || res.Error.Kind != executor.ErrTimeout {
		t.Fatalf("expected timeout error on cancelled worker call, got res=%+v err=%v", res, err)
	}
}
