package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/mcpserver"
	"cli-ai-gateway/internal/storage"
	"cli-ai-gateway/internal/tools"
)

func TestMCPServerScopedAuthServerToolsAndClientCapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := storage.OpenSQLite(ctx, filepath.Join(root, "mcp.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer db.Close()

	fs, err := storage.NewFileStore(db, root, 10*1024*1024, 50*1024*1024, time.Hour)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	reg := tools.NewRegistry(fs, tools.NewBackendStore(db.SQL()))
	srv := mcpserver.NewServer(reg)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/mcp/{run_id}", srv.HandleHTTP)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var capturedCalled bool
	sess := srv.RegisterSession(
		"run_abc",
		tools.RunContext{RunID: "run_abc", OwnerID: "alice"},
		[]string{"workspace_save_output"},
		[]executor.ToolDefinition{{
			Name:        "get_weather",
			Description: "Get weather for a city",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
		func(_ executor.ToolCall) { capturedCalled = true },
	)
	defer srv.UnregisterSession("run_abc")

	callRPC := func(token string, payload map[string]any) (int, map[string]any) {
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/internal/mcp/run_abc", bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("http Do: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Wrong token rejected.
	if status, _ := callRPC("wrong-token", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}); status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %d", status)
	}

	// tools/list returns both allowed server tool and client tool.
	status, res := callRPC(sess.Token, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if status != http.StatusOK {
		t.Fatalf("tools/list HTTP %d", status)
	}
	rawRes, _ := json.Marshal(res)
	if !strings.Contains(string(rawRes), "workspace_save_output") || !strings.Contains(string(rawRes), "get_weather") {
		t.Fatalf("tools/list missing expected tools: %s", rawRes)
	}

	// Server tool call succeeds and records generated file.
	_, res = callRPC(sess.Token, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "workspace_save_output",
			"arguments": map[string]any{"filename": "note.txt", "content": "hello from mcp"},
		},
	})
	if len(sess.GeneratedFiles()) != 1 {
		t.Fatalf("expected 1 generated file, got %d (resp=%+v)", len(sess.GeneratedFiles()), res)
	}

	// Disallowed server tool is rejected.
	_, res = callRPC(sess.Token, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params":  map[string]any{"name": "audio_tts", "arguments": map[string]any{"text": "nope"}},
	})
	if res["error"] == nil {
		t.Fatalf("expected JSON-RPC error for disallowed tool, got %+v", res)
	}

	// Client tool call is captured cleanly.
	_, _ = callRPC(sess.Token, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "get_weather",
			"arguments": map[string]any{"city": "Moscow"},
		},
	})
	if !capturedCalled || len(sess.CapturedClientCalls()) != 1 || sess.CapturedClientCalls()[0].Name != "get_weather" {
		t.Fatalf("expected client tool call capture, got calls=%+v", sess.CapturedClientCalls())
	}
}
