package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/antigravity"
	"cli-ai-gateway/internal/executor/claude"
	"cli-ai-gateway/internal/executor/codex"
	"cli-ai-gateway/internal/executor/gemini"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/worker"
)

type mockWorkerContainer struct {
	ID      string
	HomeDir string
	URL     string
}

func startMockWorkerContainer(t *testing.T, h *testHarness, workerToken, id, provider string) mockWorkerContainer {
	t.Helper()

	selfExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binName := map[string]string{
		"antigravity": "mock-agy",
		"codex":       "mock-codex",
		"claude":      "mock-claude",
		"gemini":      "mock-gemini",
	}[provider]
	if binName == "" {
		t.Fatalf("unsupported mock worker provider %q", provider)
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, binName)
	if err := os.Symlink(selfExe, binPath); err != nil {
		t.Fatal(err)
	}

	homeDir := t.TempDir()
	procMgr := process.NewManager(process.Limits{
		MaxGlobal:        4,
		MaxPerExecutor:   4,
		MaxPerUser:       4,
		MaxQueueSize:     8,
		QueueWaitTimeout: 5 * time.Second,
		DefaultMaxOutput: 1024 * 1024,
	})

	cfg := config.ModelConfig{
		ID:          id,
		Provider:    provider,
		Executable:  binPath,
		HomeDir:     homeDir,
		SandboxMode: "read-only",
		FSAccess:    "workspace-read",
	}

	var execAdapter executor.Executor
	switch provider {
	case "antigravity":
		execAdapter = antigravity.New(cfg, procMgr)
	case "codex":
		execAdapter = codex.New(cfg, procMgr)
	case "claude":
		execAdapter = claude.New(cfg, procMgr)
	case "gemini":
		execAdapter = gemini.New(cfg, procMgr)
	}

	wSrv, err := worker.NewServer(worker.ServerConfig{
		Token:      workerToken,
		Provider:   provider,
		Executable: binPath,
		Executor:   execAdapter,
	})
	if err != nil {
		t.Fatalf("worker.NewServer(%s): %v", id, err)
	}
	ts := httptest.NewServer(wSrv.Handler())
	t.Cleanup(ts.Close)

	resp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/models", "admin-secret", nil, false, map[string]any{
		"id":         id,
		"provider":   provider,
		"worker_url": ts.URL,
		"public":     false,
	})
	body := decodeJSONMap(t, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register hidden worker %q failed (%d): %+v", id, resp.StatusCode, body)
	}

	return mockWorkerContainer{
		ID:      id,
		HomeDir: homeDir,
		URL:     ts.URL,
	}
}

func doJSONReqWithHeaders(t *testing.T, method, url, token string, body any) (int, http.Header, []byte) {
	t.Helper()
	var payload string
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = string(raw)
	}
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequest(method, url, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, rErr := resp.Body.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if rErr != nil {
			break
		}
	}
	return resp.StatusCode, resp.Header.Clone(), []byte(buf.String())
}

func setMockQuotaFail(t *testing.T, homeDir, content string) {
	t.Helper()
	path := filepath.Join(homeDir, ".mock_fail_quota")
	if content == "" {
		_ = os.Remove(path)
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func extractResponseOutputText(t *testing.T, raw []byte) string {
	t.Helper()
	var payload struct {
		ID     string `json:"id"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal response %s: %v", string(raw), err)
	}
	if len(payload.Output) == 0 || len(payload.Output[0].Content) == 0 {
		t.Fatalf("empty output in response: %s", string(raw))
	}
	return payload.Output[0].Content[0].Text
}

func extractResponseID(t *testing.T, raw []byte) string {
	t.Helper()
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.ID == "" {
		t.Fatalf("expected response id in %s: %v", string(raw), err)
	}
	return payload.ID
}

func TestPools_HiddenProvidersDiscoveryAndStandalonePublish(t *testing.T) {
	const workerToken = "pool-worker-secret"
	t.Setenv("GATEWAY_WORKER_TOKEN", workerToken)

	h := newTestHarness(t, harnessOptions{})
	startMockWorkerContainer(t, h, workerToken, "agy-1", "antigravity")

	// 1. /ready includes hidden provider agy-1 as ready, while /v1/models omits it.
	readyStatus, _, readyBody := doJSONReqWithHeaders(t, http.MethodGet, h.URL+"/ready", "", nil)
	if readyStatus != http.StatusOK || !strings.Contains(string(readyBody), `"agy-1"`) {
		t.Fatalf("expected /ready to include ready hidden worker agy-1, got status=%d body=%s", readyStatus, string(readyBody))
	}

	modelsStatus, _, modelsBody := doJSONReqWithHeaders(t, http.MethodGet, h.URL+"/v1/models", "alice-secret", nil)
	if modelsStatus != http.StatusOK {
		t.Fatalf("GET /v1/models failed: %d", modelsStatus)
	}
	if strings.Contains(string(modelsBody), `"id":"agy-1"`) {
		t.Fatalf("hidden provider agy-1 leaked into /v1/models: %s", string(modelsBody))
	}

	directStatus, _, _ := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "agy-1",
		"input": "Should not be directly addressable",
	})
	if directStatus != http.StatusNotFound {
		t.Fatalf("expected 404 when calling non-public provider agy-1 directly, got %d", directStatus)
	}

	// 2. Refresh discovery for remote agy-1 worker and Ollama provider.
	discResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/providers/agy-1/models?refresh=1", "admin-secret", nil, false, nil)
	discMap := decodeJSONMap(t, discResp.Body)
	discResp.Body.Close()
	if discResp.StatusCode != http.StatusOK {
		t.Fatalf("discover agy-1 models failed (%d): %+v", discResp.StatusCode, discMap)
	}
	modelsList, _ := discMap["models"].([]any)
	if len(modelsList) < 3 {
		t.Fatalf("expected >= 3 discovered models from remote agy-1 worker, got %+v", discMap)
	}

	ollamaDisc := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/providers/ollama-local/models?refresh=1", "admin-secret", nil, false, nil)
	ollamaMap := decodeJSONMap(t, ollamaDisc.Body)
	ollamaDisc.Body.Close()
	if ollamaDisc.StatusCode != http.StatusOK || ollamaMap["source"] != "ollama_tags" {
		t.Fatalf("expected ollama_tags discovery, got %d %+v", ollamaDisc.StatusCode, ollamaMap)
	}

	// 3. Add a custom sub-model and publish gemini-2.5-pro from agy-1 as standalone model.
	customResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/providers/agy-1/models", "admin-secret", nil, false, map[string]any{
		"model_name":   "custom-model-x",
		"display_name": "Custom X",
	})
	customResp.Body.Close()
	if customResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 adding custom provider model, got %d", customResp.StatusCode)
	}

	pubResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/providers/agy-1/publish", "admin-secret", nil, false, map[string]any{
		"items": []map[string]any{
			{
				"id":            "agy-gemini-2.5-pro",
				"provider_model": "gemini-2.5-pro",
				"display_name":  "AGY Gemini 2.5 Pro",
			},
		},
	})
	pubResp.Body.Close()
	if pubResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 publishing standalone model, got %d", pubResp.StatusCode)
	}

	// 4. Verify published standalone model appears in /v1/models and passes --model gemini-2.5-pro to agy-1.
	_, _, updatedModels := doJSONReqWithHeaders(t, http.MethodGet, h.URL+"/v1/models", "alice-secret", nil)
	if !strings.Contains(string(updatedModels), `"agy-gemini-2.5-pro"`) || strings.Contains(string(updatedModels), `"id":"agy-1"`) {
		t.Fatalf("expected agy-gemini-2.5-pro published and agy-1 hidden in %s", string(updatedModels))
	}

	callStatus, callHdrs, callBody := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "agy-gemini-2.5-pro",
		"input": "Hello standalone published model",
	})
	if callStatus != http.StatusOK {
		t.Fatalf("POST /v1/responses failed (%d): %s", callStatus, string(callBody))
	}
	if got := callHdrs.Get("X-Gateway-Target-Provider"); got != "agy-1" {
		t.Fatalf("expected X-Gateway-Target-Provider=agy-1, got %q", got)
	}
	if got := callHdrs.Get("X-Gateway-Target-Model"); got != "gemini-2.5-pro" {
		t.Fatalf("expected X-Gateway-Target-Model=gemini-2.5-pro, got %q", got)
	}
	outText := extractResponseOutputText(t, callBody)
	if !strings.Contains(outText, "--model gemini-2.5-pro") {
		t.Fatalf("expected worker args to contain --model gemini-2.5-pro, got %q", outText)
	}
}

func TestPools_ThreeAgyFailoverPerModelCooldownAndRelease(t *testing.T) {
	const workerToken = "pool-worker-secret"
	t.Setenv("GATEWAY_WORKER_TOKEN", workerToken)

	h := newTestHarness(t, harnessOptions{})
	w1 := startMockWorkerContainer(t, h, workerToken, "agy-1", "antigravity")
	w2 := startMockWorkerContainer(t, h, workerToken, "agy-2", "antigravity")
	w3 := startMockWorkerContainer(t, h, workerToken, "agy-3", "antigravity")

	// Assemble 3x agy composite pool targeting gemini-2.5-pro.
	catResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/catalog", "admin-secret", nil, false, map[string]any{
		"id":                       "agy-pro-pool",
		"display_name":             "AGY Pro Pool (3x)",
		"kind":                     "composite",
		"strategy":                 "priority_order",
		"default_cooldown_seconds": 300,
		"targets": []map[string]any{
			{"provider_id": "agy-1", "provider_model": "gemini-2.5-pro", "priority": 1, "position": 0, "enabled": true},
			{"provider_id": "agy-2", "provider_model": "gemini-2.5-pro", "priority": 1, "position": 1, "enabled": true},
			{"provider_id": "agy-3", "provider_model": "gemini-2.5-pro", "priority": 1, "position": 2, "enabled": true},
		},
	})
	catResp.Body.Close()
	if catResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating composite pool, got %d", catResp.StatusCode)
	}

	// Also publish claude-sonnet-4-5 on agy-1 separately to verify per-(provider_id, provider_model) cooldown isolation.
	pubResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/providers/agy-1/publish", "admin-secret", nil, false, map[string]any{
		"items": []map[string]any{
			{"id": "agy-1-claude", "provider_model": "claude-sonnet-4-5"},
		},
	})
	pubResp.Body.Close()
	if pubResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 publishing agy-1-claude, got %d", pubResp.StatusCode)
	}

	// Trigger parsed quota limit specifically for gemini-2.5-pro on agy-1.
	setMockQuotaFail(t, w1.HomeDir, "model=gemini-2.5-pro|Error: Individual quota reached for Gemini 2.5 Pro. Resets in 2h.")

	// 1. Sync request on agy-pro-pool automatically fails over from agy-1 -> agy-2.
	status, hdrs, body := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "agy-pro-pool",
		"input": "Hello pool failover",
	})
	if status != http.StatusOK {
		t.Fatalf("expected transparent failover 200 OK, got %d: %s", status, string(body))
	}
	if hdrs.Get("X-Gateway-Target-Provider") != "agy-2" || hdrs.Get("X-Gateway-Target-Model") != "gemini-2.5-pro" {
		t.Fatalf("expected failover target agy-2 / gemini-2.5-pro, got provider=%q model=%q",
			hdrs.Get("X-Gateway-Target-Provider"), hdrs.Get("X-Gateway-Target-Model"))
	}

	// 2. Verify cooldown recorded with parsed ~7200s TTL for ("agy-1", "gemini-2.5-pro").
	cdResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/cooldowns", "admin-secret", nil, false, nil)
	var cdPayload struct {
		Data []struct {
			ProviderID       string `json:"provider_id"`
			ProviderModel    string `json:"provider_model"`
			RemainingSeconds int64  `json:"remaining_seconds"`
			Source           string `json:"source"`
		} `json:"data"`
	}
	if err := json.NewDecoder(cdResp.Body).Decode(&cdPayload); err != nil {
		t.Fatalf("decode cooldowns: %v", err)
	}
	cdResp.Body.Close()
	if len(cdPayload.Data) != 1 || cdPayload.Data[0].ProviderID != "agy-1" || cdPayload.Data[0].ProviderModel != "gemini-2.5-pro" {
		t.Fatalf("unexpected cooldown records: %+v", cdPayload.Data)
	}
	if cdPayload.Data[0].Source != "parsed" || cdPayload.Data[0].RemainingSeconds < 7100 || cdPayload.Data[0].RemainingSeconds > 7210 {
		t.Fatalf("expected parsed ~7200s cooldown, got %+v", cdPayload.Data[0])
	}

	// 3. Different sub-model on agy-1 (claude-sonnet-4-5) MUST remain unaffected by gemini-2.5-pro cooldown.
	cStatus, cHdrs, cBody := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "agy-1-claude",
		"input": "Check per-model cooldown isolation",
	})
	if cStatus != http.StatusOK || cHdrs.Get("X-Gateway-Target-Provider") != "agy-1" {
		t.Fatalf("expected agy-1-claude to still execute on agy-1, got status=%d provider=%q body=%s",
			cStatus, cHdrs.Get("X-Gateway-Target-Provider"), string(cBody))
	}

	// 4. Streaming failover: trigger quota on agy-2, stream request must seamlessly fail over to agy-3!
	setMockQuotaFail(t, w2.HomeDir, "model=gemini-2.5-pro|RESOURCE_EXHAUSTED: Quota exceeded. Retry in 45m.")
	streamReq, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/responses", strings.NewReader(`{"model":"agy-pro-pool","input":"Stream failover test","stream":true}`))
	streamReq.Header.Set("Authorization", "Bearer alice-secret")
	streamReq.Header.Set("Content-Type", "application/json")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	frames := readSSEFrames(t, streamResp.Body)
	streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("expected streaming failover 200 OK, got %d", streamResp.StatusCode)
	}
	if streamResp.Header.Get("X-Gateway-Target-Provider") != "agy-3" {
		t.Fatalf("expected SSE failover to agy-3, got %q", streamResp.Header.Get("X-Gateway-Target-Provider"))
	}
	if len(frames) == 0 || frames[len(frames)-1].Event != "response.completed" {
		t.Fatalf("expected response.completed SSE frame, got %+v", frames)
	}

	// 5. Exhaust remaining agy-3 provider -> gateway returns 503 all_providers_cooling + Retry-After header.
	setMockQuotaFail(t, w3.HomeDir, "model=gemini-2.5-pro|rate limit hit, retry in 10m")
	exhaustStatus, exhaustHdrs, exhaustBody := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "agy-pro-pool",
		"input": "Should fail with all cooling",
	})
	if exhaustStatus != http.StatusTooManyRequests || !strings.Contains(string(exhaustBody), "rate_limit_error") {
		t.Fatalf("expected 429 on last attempt rate limit, got status=%d body=%s", exhaustStatus, string(exhaustBody))
	}
	if retryVal, err := strconv.Atoi(exhaustHdrs.Get("Retry-After")); err != nil || retryVal <= 0 {
		t.Fatalf("expected positive Retry-After header on exhausted pool, got %q", exhaustHdrs.Get("Retry-After"))
	}

	// Subsequent call when all 3 are already in cooldown short-circuits immediately with all_providers_cooling.
	shortStatus, shortHdrs, shortBody := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "agy-pro-pool",
		"input": "Fast 503 check",
	})
	if shortStatus != http.StatusServiceUnavailable || !strings.Contains(string(shortBody), "all_providers_cooling") {
		t.Fatalf("expected 503 all_providers_cooling, got %d body=%s", shortStatus, string(shortBody))
	}
	if retryVal, err := strconv.Atoi(shortHdrs.Get("Retry-After")); err != nil || retryVal <= 0 {
		t.Fatalf("expected positive Retry-After header on all_providers_cooling, got %q", shortHdrs.Get("Retry-After"))
	}

	// 6. Release cooldown on agy-1 via Admin API and clear mock failure -> agy-1 immediately takes traffic again.
	setMockQuotaFail(t, w1.HomeDir, "")
	relResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/cooldowns/release", "admin-secret", nil, false, map[string]any{
		"provider_id":    "agy-1",
		"provider_model": "gemini-2.5-pro",
	})
	relResp.Body.Close()
	if relResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 releasing cooldown, got %d", relResp.StatusCode)
	}

	recStatus, recHdrs, recBody := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "agy-pro-pool",
		"input": "After manual release",
	})
	if recStatus != http.StatusOK || recHdrs.Get("X-Gateway-Target-Provider") != "agy-1" {
		t.Fatalf("expected agy-1 recovery after release, got status=%d provider=%q body=%s",
			recStatus, recHdrs.Get("X-Gateway-Target-Provider"), string(recBody))
	}
}

func TestPools_MixedPriorityFailoverSessionContinuityAndRecovery(t *testing.T) {
	const workerToken = "pool-worker-secret"
	t.Setenv("GATEWAY_WORKER_TOKEN", workerToken)

	h := newTestHarness(t, harnessOptions{})
	codexW := startMockWorkerContainer(t, h, workerToken, "codex-p1", "codex")
	agyW := startMockWorkerContainer(t, h, workerToken, "agy-p2", "antigravity")
	_ = startMockWorkerContainer(t, h, workerToken, "claude-p3", "claude")

	catResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/catalog", "admin-secret", nil, false, map[string]any{
		"id":           "smart-mix",
		"display_name": "Codex -> AGY -> Claude",
		"kind":         "composite",
		"strategy":     "priority_order",
		"targets": []map[string]any{
			{"provider_id": "codex-p1", "provider_model": "gpt-5.4", "priority": 1, "position": 0, "enabled": true},
			{"provider_id": "agy-p2", "provider_model": "gemini-2.5-pro", "priority": 2, "position": 0, "enabled": true},
			{"provider_id": "claude-p3", "provider_model": "sonnet", "priority": 3, "position": 0, "enabled": true},
		},
	})
	catResp.Body.Close()
	if catResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating mixed priority pool, got %d", catResp.StatusCode)
	}

	// Turn 1: hits Priority 1 (codex-p1).
	st1, hdr1, body1 := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "smart-mix",
		"input": "Remember alpha-123",
	})
	if st1 != http.StatusOK || hdr1.Get("X-Gateway-Target-Provider") != "codex-p1" {
		t.Fatalf("expected turn 1 on codex-p1, got status=%d provider=%q body=%s", st1, hdr1.Get("X-Gateway-Target-Provider"), string(body1))
	}
	respID1 := extractResponseID(t, body1)

	// Trip quota on Priority 1 -> Turn 2 with previous_response_id must fall back to Priority 2 (agy-p2) WITH context.md!
	setMockQuotaFail(t, codexW.HomeDir, "Usage limit hit. Try again in 1h.")
	st2, hdr2, body2 := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model":                "smart-mix",
		"previous_response_id": respID1,
		"input":                "Continue on backup provider",
	})
	if st2 != http.StatusOK || hdr2.Get("X-Gateway-Target-Provider") != "agy-p2" {
		t.Fatalf("expected turn 2 failover to agy-p2, got status=%d provider=%q body=%s", st2, hdr2.Get("X-Gateway-Target-Provider"), string(body2))
	}
	if txt2 := extractResponseOutputText(t, body2); !strings.Contains(txt2, "has_context=true") {
		t.Fatalf("expected context.md continuity on cross-provider failover, got %q", txt2)
	}
	respID2 := extractResponseID(t, body2)

	// Trip Priority 2 too -> Turn 3 falls back to Priority 3 (claude-p3) with context.md.
	setMockQuotaFail(t, agyW.HomeDir, "Individual quota reached. Resets in 30m.")
	st3, hdr3, body3 := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model":                "smart-mix",
		"previous_response_id": respID2,
		"input":                "Continue on claude tier",
	})
	if st3 != http.StatusOK || hdr3.Get("X-Gateway-Target-Provider") != "claude-p3" {
		t.Fatalf("expected turn 3 failover to claude-p3, got status=%d provider=%q body=%s", st3, hdr3.Get("X-Gateway-Target-Provider"), string(body3))
	}
	if txt3 := extractResponseOutputText(t, body3); !strings.Contains(txt3, "has_context=true") {
		t.Fatalf("expected context.md continuity on P3 failover, got %q", txt3)
	}
	respID3 := extractResponseID(t, body3)

	// Clear & release Priority 1 cooldown -> Turn 4 immediately returns to Priority 1 (codex-p1) and gets context.md!
	setMockQuotaFail(t, codexW.HomeDir, "")
	relResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/cooldowns/release", "admin-secret", nil, false, map[string]any{
		"provider_id":    "codex-p1",
		"provider_model": "gpt-5.4",
	})
	relResp.Body.Close()
	if relResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 releasing codex-p1 cooldown, got %d", relResp.StatusCode)
	}

	st4, hdr4, body4 := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model":                "smart-mix",
		"previous_response_id": respID3,
		"input":                "Verify strict priority return to P1",
	})
	if st4 != http.StatusOK || hdr4.Get("X-Gateway-Target-Provider") != "codex-p1" {
		t.Fatalf("expected strict priority return to codex-p1, got status=%d provider=%q body=%s", st4, hdr4.Get("X-Gateway-Target-Provider"), string(body4))
	}
	if txt4 := extractResponseOutputText(t, body4); !strings.Contains(txt4, "has_context=true") {
		t.Fatalf("expected context.md continuity when returning from P3 to P1, got %q", txt4)
	}
}
