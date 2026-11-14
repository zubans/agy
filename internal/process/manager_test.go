package process_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"cli-ai-gateway/internal/process"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "mock-large-lines":
		// Emit a >128 KiB single JSON line to verify unbounded line reading.
		payload := strings.Repeat("Z", 140*1024)
		fmt.Printf("{\"chunk\":%q}\n", payload)
		fmt.Println("{\"done\":true}")
		os.Exit(0)
	case "mock-pgid-sleep":
		// Spawn a child subprocess in the same process group, report its PID, then wait.
		if len(os.Args) >= 3 && os.Args[1] == "--child" {
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
		pidFile := os.Getenv("MOCK_CHILD_PID_FILE")
		attr := &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		}
		proc, err := os.StartProcess(os.Args[0], []string{os.Args[0], "--child", "1"}, attr)
		if err != nil {
			os.Exit(2)
		}
		if pidFile != "" {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(proc.Pid)), 0o600)
		}
		fmt.Printf("STARTED child=%d\n", proc.Pid)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "mock-output-flood":
		chunk := strings.Repeat("A", 4096)
		for i := 0; i < 64; i++ {
			fmt.Println(chunk)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func mockBinary(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	target := filepath.Join(t.TempDir(), name)
	if err := os.Link(self, target); err != nil {
		if err := os.Symlink(self, target); err != nil {
			t.Fatalf("link/symlink mock binary: %v", err)
		}
	}
	return target
}

func TestManagerConcurrencyQueueAndTimeout(t *testing.T) {
	mgr := process.NewManager(process.Limits{
		MaxGlobal:        2,
		MaxPerExecutor:   1,
		MaxPerUser:       2,
		MaxQueueSize:     1,
		QueueWaitTimeout: 120 * time.Millisecond,
	})

	ctx := context.Background()
	lease1, err := mgr.Acquire(ctx, "exec-a", "user-1", 0, 0)
	if err != nil {
		t.Fatalf("Acquire lease1: %v", err)
	}

	// Second request on same executor must queue and succeed once lease1 releases.
	type res struct {
		lease *process.Lease
		err   error
	}
	ch2 := make(chan res, 1)
	go func() {
		l, e := mgr.Acquire(ctx, "exec-a", "user-1", 0, 0)
		ch2 <- res{l, e}
	}()

	// Wait briefly until queued == 1.
	deadline := time.Now().Add(time.Second)
	for mgr.Stats().Queued < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if mgr.Stats().Queued != 1 {
		t.Fatalf("expected Queued=1, got %+v", mgr.Stats())
	}

	// Third waiter exceeds MaxQueueSize=1 -> immediate ErrQueueFull.
	if _, err := mgr.Acquire(ctx, "exec-a", "user-1", 0, 0); !errors.Is(err, process.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}

	lease1.Release()
	r2 := <-ch2
	if r2.err != nil {
		t.Fatalf("queued Acquire failed after release: %v", r2.err)
	}

	// Holding r2 while waiting past QueueWaitTimeout must return ErrQueueTimeout.
	if _, err := mgr.Acquire(ctx, "exec-a", "user-1", 0, 0); !errors.Is(err, process.ErrQueueTimeout) {
		t.Fatalf("expected ErrQueueTimeout, got %v", err)
	}
	r2.lease.Release()
}

func TestBuildSanitizedEnvAndSanitizeOutput(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-secret-host-key-123456")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-host-key-123456")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("GATEWAY_ADMIN_SECRET", "gw-secret")

	workDir := t.TempDir()
	homeDir := filepath.Join(t.TempDir(), "codex-home")
	envList := process.BuildSanitizedEnv(homeDir, workDir, map[string]string{
		"CODEX_HOME": homeDir,
	})

	joined := strings.Join(envList, "\n")
	for _, forbidden := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "GATEWAY_ADMIN_SECRET", "sk-secret-host-key"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sanitized env leaked forbidden key/value %q", forbidden)
		}
	}
	if !strings.Contains(joined, "HOME="+homeDir) || !strings.Contains(joined, "CODEX_HOME="+homeDir) {
		t.Fatalf("expected isolated HOME and CODEX_HOME in env: %s", joined)
	}

	dirty := "failed with Bearer eyJhbGciOiJIUzI1NiJ9.abcdef and api_key=mysecretpassword123 and AIzaSyDummyKeyValue1234567890"
	clean := process.SanitizeOutput(dirty)
	if strings.Contains(clean, "eyJhbGciOiJIUzI1NiJ9") || strings.Contains(clean, "mysecretpassword123") || strings.Contains(clean, "AIzaSyDummyKeyValue") {
		t.Fatalf("SanitizeOutput failed to redact secrets: %q", clean)
	}
	if !strings.Contains(clean, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] placeholder in %q", clean)
	}
}

func TestRunStreamLargeLinesAndProcessGroupKill(t *testing.T) {
	mgr := process.NewManager(process.Limits{})

	// 1. >128 KiB line handling.
	largeBin := mockBinary(t, "mock-large-lines")
	var lines []string
	res, err := mgr.RunStream(context.Background(), process.RunSpec{
		Executable: largeBin,
		WorkDir:    t.TempDir(),
		OnLine: func(line []byte) error {
			lines = append(lines, string(line))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("RunStream large lines failed: %v", err)
	}
	if res.ExitCode != 0 || len(lines) != 2 || len(lines[0]) < 140*1024 {
		t.Fatalf("unexpected large lines result: exit=%d lines=%d firstLen=%d", res.ExitCode, len(lines), len(lines[0]))
	}

	// 2. Output limit enforcement.
	floodBin := mockBinary(t, "mock-output-flood")
	_, err = mgr.RunStream(context.Background(), process.RunSpec{
		Executable:     floodBin,
		WorkDir:        t.TempDir(),
		MaxOutputBytes: 16 * 1024,
	})
	if !errors.Is(err, process.ErrOutputLimit) {
		t.Fatalf("expected ErrOutputLimit, got %v", err)
	}

	// 3. Process-group SIGKILL cleanup on context cancel kills spawned grandchild too.
	sleepBin := mockBinary(t, "mock-pgid-sleep")
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startedCh := make(chan struct{})
	doneCh := make(chan error, 1)
	go func() {
		_, runErr := mgr.RunStream(runCtx, process.RunSpec{
			Executable: sleepBin,
			WorkDir:    t.TempDir(),
			ExtraEnv:   map[string]string{"MOCK_CHILD_PID_FILE": pidFile},
			OnLine: func(line []byte) error {
				if strings.HasPrefix(string(line), "STARTED") {
					select {
					case <-startedCh:
					default:
						close(startedCh)
					}
				}
				return nil
			},
		})
		doneCh <- runErr
	}()

	select {
	case <-startedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for mock-pgid-sleep child process startup")
	}

	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid file: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child pid %q: %v", string(rawPID), err)
	}

	cancel()
	runErr := <-doneCh
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected context.Canceled on abort, got %v", runErr)
	}

	// Verify child PID in process group was terminated.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild PID %d survived process-group SIGKILL", childPID)
}
