package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor/antigravity"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/worker"
)

func TestRemoteContainerWorkerAdminAndResponses(t *testing.T) {
	// Spin up a standalone HTTP worker simulating an isolated `agy-4` container wrapping `mock-agy`.
	selfExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	mockAgyBin := filepath.Join(binDir, "mock-agy")
	if err := os.Symlink(selfExe, mockAgyBin); err != nil {
		t.Fatal(err)
	}

	procMgr := process.NewManager(process.Limits{
		MaxGlobal:        4,
		MaxPerExecutor:   4,
		MaxPerUser:       4,
		MaxQueueSize:     8,
		QueueWaitTimeout: 5 * time.Second,
		DefaultMaxOutput: 1024 * 1024,
	})
	agyHome := t.TempDir()
	localExec := antigravity.New(config.ModelConfig{
		ID:          "agy-4",
		Provider:    "antigravity",
		Executable:  mockAgyBin,
		HomeDir:     agyHome,
		SandboxMode: "strict",
		FSAccess:    "workspace-read",
	}, procMgr)

	const workerToken = "integration-worker-secret"
	t.Setenv("GATEWAY_WORKER_TOKEN", workerToken)

	wSrv, err := worker.NewServer(worker.ServerConfig{
		Token:      workerToken,
		Provider:   "antigravity",
		Executable: mockAgyBin,
		Executor:   localExec,
	})
	if err != nil {
		t.Fatalf("worker.NewServer: %v", err)
	}
	workerTS := httptest.NewServer(wSrv.Handler())
	defer workerTS.Close()

	h := newTestHarness(t, harnessOptions{})

	// 1. Web Admin must allow registering a remote worker model via worker_url (and reject local executable).
	badResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/models", "admin-secret", nil, false, map[string]any{
		"id":         "bad-local-cli",
		"provider":   "antigravity",
		"executable": "/bin/sh",
	})
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting local executable via admin UI, got %d", badResp.StatusCode)
	}

	createResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/models", "admin-secret", nil, false, map[string]any{
		"id":         "agy-4",
		"provider":   "antigravity",
		"worker_url": workerTS.URL,
		"model":      "gemini-3-pro",
	})
	created := decodeJSONMap(t, createResp.Body)
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating worker model, got %d (%v)", createResp.StatusCode, created)
	}
	if created["ready"] != true || created["worker_url"] != workerTS.URL {
		t.Fatalf("expected ready worker model, got %+v", created)
	}

	// Verify /admin/api/config redacts worker token while exposing worker_url.
	cfgResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/config", "admin-secret", nil, false, nil)
	cfgBytes, _ := io.ReadAll(cfgResp.Body)
	cfgResp.Body.Close()
	if strings.Contains(string(cfgBytes), workerToken) {
		t.Fatalf("worker token leaked in /admin/api/config")
	}

	// 2. Execute /v1/responses against the containerized worker model `agy-4` and verify response.
	execResp := doAdminRequest(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", nil, false, map[string]any{
		"model": "agy-4",
		"input": "Hello remote agy worker container!",
	})
	execBody := decodeJSONMap(t, execResp.Body)
	execResp.Body.Close()
	if execResp.StatusCode != http.StatusOK {
		raw, _ := json.Marshal(execBody)
		t.Fatalf("expected 200 executing remote worker model, got %d: %s", execResp.StatusCode, string(raw))
	}
	if execBody["model"] != "agy-4" {
		t.Fatalf("unexpected model in response: %v", execBody["model"])
	}

	// 3. Re-open Registry with ApplyOverrides to prove dynamic worker_url persists across gateway restarts.
	if err := h.Registry.ApplyOverrides(context.Background(), h.DB.SQL()); err != nil {
		t.Fatalf("ApplyOverrides reload failed: %v", err)
	}
	if info, ok := h.Registry.Get("agy-4"); !ok || info.Config.WorkerURL != workerTS.URL {
		t.Fatalf("expected persisted agy-4 worker model after reload, got ok=%v info=%+v", ok, info.Config)
	}
}
