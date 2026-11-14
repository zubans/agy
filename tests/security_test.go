package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecurity_AuthAuthorizationAndOwnership(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})

	// 1. Missing, wrong, and revoked tokens -> 401.
	for _, tok := range []string{"", "wrong-token", "dave-revoked"} {
		code, _ := doJSONReq(t, http.MethodGet, h.URL+"/v1/models", tok, nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("token %q expected 401, got %d", tok, code)
		}
	}

	// 2. Disallowed model for key -> 403.
	code, body := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "carol-secret", map[string]any{
		"model": "claude-cli",
		"input": "hello",
	})
	if code != http.StatusForbidden || !strings.Contains(string(body), "model_not_allowed") {
		t.Fatalf("expected 403 model_not_allowed, got %d (%s)", code, body)
	}

	// 3. Cross-user file isolation (alice uploads, bob attempts Get/Content/Delete/Use).
	upCode, fMeta := uploadTestFile(t, h.URL, "alice-secret", "secret.md", "user_data", "alice confidential")
	if upCode != http.StatusCreated {
		t.Fatalf("alice upload failed: %d", upCode)
	}
	fileID := fMeta["id"].(string)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/files/" + fileID},
		{http.MethodGet, "/v1/files/" + fileID + "/content"},
		{http.MethodDelete, "/v1/files/" + fileID},
	} {
		c, _ := doJSONReq(t, tc.method, h.URL+tc.path, "bob-secret", nil)
		if c != http.StatusForbidden {
			t.Fatalf("%s %s by bob expected 403, got %d", tc.method, tc.path, c)
		}
	}

	useCode, _ := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "bob-secret", map[string]any{
		"model": "codex-cli",
		"input": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "input_file", "file_id": fileID}}},
		},
	})
	if useCode != http.StatusForbidden {
		t.Fatalf("bob using alice file_id expected 403, got %d", useCode)
	}

	// 4. Cross-user previous_response_id -> 403.
	c1, rBody := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "codex-cli",
		"input": "alice session",
	})
	if c1 != http.StatusOK {
		t.Fatalf("alice response failed: %d", c1)
	}
	rID := extractJSONField(t, rBody, "id")
	c2, _ := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "bob-secret", map[string]any{
		"model":                "codex-cli",
		"previous_response_id": rID,
		"input":                "bob hijack attempt",
	})
	if c2 != http.StatusForbidden {
		t.Fatalf("bob resuming alice previous_response_id expected 403, got %d", c2)
	}
}

func TestSecurity_TraversalInjectionValidationAndSizeLimits(t *testing.T) {
	h := newTestHarness(t, harnessOptions{MaxFileBytes: 1024})

	// 1. Path traversal filename -> 400.
	c, _ := uploadTestFile(t, h.URL, "alice-secret", "../../etc/passwd", "user_data", "payload")
	if c != http.StatusBadRequest {
		t.Fatalf("expected 400 for path traversal filename, got %d", c)
	}

	// 2. Spoofed file_id -> 400 or 404.
	c, _ = doJSONReq(t, http.MethodGet, h.URL+"/v1/files/..%2F..%2Fetc%2Fpasswd", "alice-secret", nil)
	if c != http.StatusNotFound && c != http.StatusBadRequest {
		t.Fatalf("expected 404/400 for traversal file_id, got %d", c)
	}

	// 3. Oversized file -> 413 Payload Too Large.
	c, _ = uploadTestFile(t, h.URL, "alice-secret", "big.txt", "user_data", strings.Repeat("X", 2048))
	if c != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for >MaxFileBytes upload, got %d", c)
	}

	// 4. Binary NUL upload -> 400.
	c, _ = uploadTestFile(t, h.URL, "alice-secret", "bin.txt", "user_data", "abc\x00def")
	if c != http.StatusBadRequest {
		t.Fatalf("expected 400 for binary NUL upload, got %d", c)
	}

	// 5. Unsupported OpenAI parameter rejected strictly -> 400.
	c, b := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model":       "codex-cli",
		"input":       "hi",
		"temperature": 0.5,
	})
	if c != http.StatusBadRequest || !strings.Contains(string(b), "unsupported_parameter") {
		t.Fatalf("expected 400 unsupported_parameter, got %d (%s)", c, b)
	}

	// 6. Shell injection & CLI flag injection stay inert in prompt text, never executed or promoted to args.
	pwnMarker := filepath.Join(t.TempDir(), "should_not_exist")
	injPrompt := fmt.Sprintf("$(touch %s) ; touch %s && --dangerously-bypass-approvals-and-sandbox --cd /etc", pwnMarker, pwnMarker)
	c, respBytes := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "codex-cli",
		"input": injPrompt,
	})
	if c != http.StatusOK {
		t.Fatalf("expected 200 (safe inert handling), got %d (%s)", c, respBytes)
	}
	if _, err := os.Stat(pwnMarker); !os.IsNotExist(err) {
		t.Fatalf("shell injection marker file %s was created!", pwnMarker)
	}
	// Extract the args=[...] portion returned by mock-codex and ensure injected flags are NOT in argv.
	outStr := string(respBytes)
	if idx := strings.Index(outStr, "args=["); idx >= 0 {
		argsSection := outStr[idx:]
		if end := strings.Index(argsSection, "]"); end > 0 {
			argsSection = argsSection[:end]
		}
		if strings.Contains(argsSection, "dangerously-bypass") || strings.Contains(argsSection, "/etc") {
			t.Fatalf("CLI flag injection reached argv: %s", argsSection)
		}
	}
}

func TestSecurity_EnvAndSecretIsolationAcrossExecutors(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-parent-host-openai-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-parent-host-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-parent-secret")
	t.Setenv("GATEWAY_ADMIN_SECRET", "gateway-internal-secret")

	h := newTestHarness(t, harnessOptions{})

	code, body := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "codex-cli",
		"input": "TRIGGER_ENV_DUMP",
	})
	if code != http.StatusOK {
		t.Fatalf("env dump run failed: %d (%s)", code, body)
	}

	s := string(body)
	if !strings.Contains(s, "CODEX_ISOLATED_TOKEN=codex-only-secret") {
		t.Fatalf("expected codex-specific env var in codex process, got: %s", s)
	}
	for _, leaked := range []string{
		"CLAUDE_ISOLATED_TOKEN",
		"claude-only-secret",
		"sk-parent-host-openai-secret",
		"sk-ant-parent-host-secret",
		"aws-parent-secret",
		"gateway-internal-secret",
		"alice-secret",
		"bob-secret",
	} {
		if strings.Contains(s, leaked) {
			t.Fatalf("executor environment leaked forbidden secret %q: %s", leaked, s)
		}
	}
}

func extractJSONField(t *testing.T, body []byte, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	v, _ := m[field].(string)
	return v
}
