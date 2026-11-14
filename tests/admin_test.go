package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func doAdminRequest(t *testing.T, method, url, bearer string, cookie *http.Cookie, csrfHeader bool, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrfHeader {
		req.Header.Set("X-Admin-UI", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP %s %s: %v", method, url, err)
	}
	return resp
}

func decodeJSONMap(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return out
}

func TestAdminAuthSecurityAndConfigRedaction(t *testing.T) {
	disabled := newTestHarness(t, harnessOptions{DisableAdmin: true})
	resp := doAdminRequest(t, http.MethodGet, disabled.URL+"/admin/", "", nil, false, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 when admin disabled, got %d", resp.StatusCode)
	}

	h := newTestHarness(t, harnessOptions{})

	// 1. HTML SPA renders at /admin/ and redirects /admin -> /admin/.
	uiResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/", "", nil, false, nil)
	uiBody, _ := io.ReadAll(uiResp.Body)
	uiResp.Body.Close()
	if uiResp.StatusCode != http.StatusOK || !strings.Contains(string(uiBody), "CLI AI Gateway") {
		t.Fatalf("expected Web Admin HTML at /admin/, got status=%d len=%d", uiResp.StatusCode, len(uiBody))
	}

	// 2. Client API keys must never grant access to admin APIs.
	clientResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/status", "alice-secret", nil, false, nil)
	clientResp.Body.Close()
	if clientResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 when using client API key on admin API, got %d", clientResp.StatusCode)
	}

	// 3. Login via /admin/api/login sets HttpOnly + SameSite=Strict cookie.
	badLogin := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/login", "", nil, true, map[string]string{"token": "wrong-token"})
	badLogin.Body.Close()
	if badLogin.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on bad admin login, got %d", badLogin.StatusCode)
	}

	loginResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/login", "", nil, true, map[string]string{"token": "admin-secret"})
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on admin login, got %d", loginResp.StatusCode)
	}
	var sessionCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == "agy_admin_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("expected HttpOnly SameSite=Strict agy_admin_session cookie, got %+v", sessionCookie)
	}

	// 4. Session cookie works for GET and enforces X-Admin-UI CSRF guard on mutating methods.
	statusResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/status", "", sessionCookie, false, nil)
	statusBody := decodeJSONMap(t, statusResp.Body)
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /admin/api/status with cookie, got %d", statusResp.StatusCode)
	}
	counts, _ := statusBody["counts"].(map[string]any)
	if int(counts["total_models"].(float64)) != 5 {
		t.Fatalf("expected 5 total_models in status, got %v", counts)
	}

	csrfBlock := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/cleanup", "", sessionCookie, false, nil)
	csrfBlock.Body.Close()
	if csrfBlock.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on cookie POST without X-Admin-UI header, got %d", csrfBlock.StatusCode)
	}

	csrfOK := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/cleanup", "", sessionCookie, true, nil)
	csrfOK.Body.Close()
	if csrfOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on cookie POST with X-Admin-UI header, got %d", csrfOK.StatusCode)
	}

	// 5. /admin/api/config must redact secrets (never return plaintext keys or admin token).
	cfgResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/config", "admin-secret", nil, false, nil)
	cfgBytes, _ := io.ReadAll(cfgResp.Body)
	cfgResp.Body.Close()
	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /admin/api/config, got %d", cfgResp.StatusCode)
	}
	cfgText := string(cfgBytes)
	if strings.Contains(cfgText, "alice-secret") || strings.Contains(cfgText, "admin-secret") {
		t.Fatalf("plaintext secret leaked in /admin/api/config: %s", cfgText)
	}
	if !strings.Contains(cfgText, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker in /admin/api/config")
	}
}

func TestAdminModelManipulationAndPersistence(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})

	// 1. Disable claude-cli via Web Admin -> vanishes from /v1/models and returns 404 on /v1/responses.
	patchResp := doAdminRequest(t, http.MethodPatch, h.URL+"/admin/api/models/claude-cli", "admin-secret", nil, false, map[string]any{
		"enabled": false,
	})
	patchData := decodeJSONMap(t, patchResp.Body)
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK || patchData["enabled"] != false {
		t.Fatalf("expected claude-cli disabled, got status=%d body=%v", patchResp.StatusCode, patchData)
	}

	v1ModelsResp := doAdminRequest(t, http.MethodGet, h.URL+"/v1/models", "alice-secret", nil, false, nil)
	v1ModelsBytes, _ := io.ReadAll(v1ModelsResp.Body)
	v1ModelsResp.Body.Close()
	if strings.Contains(string(v1ModelsBytes), `"claude-cli"`) {
		t.Fatalf("disabled model claude-cli still listed in /v1/models: %s", string(v1ModelsBytes))
	}

	execDisabled := doAdminRequest(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", nil, false, map[string]any{
		"model": "claude-cli",
		"input": "hello",
	})
	execDisabled.Body.Close()
	if execDisabled.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for disabled model, got %d", execDisabled.StatusCode)
	}

	// Re-enable and run explicit CLI check.
	reenable := doAdminRequest(t, http.MethodPatch, h.URL+"/admin/api/models/claude-cli", "admin-secret", nil, false, map[string]any{
		"enabled":         true,
		"timeout_seconds": 45,
	})
	reenable.Body.Close()
	if reenable.StatusCode != http.StatusOK {
		t.Fatalf("re-enable claude-cli failed: %d", reenable.StatusCode)
	}

	checkResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/models/codex-cli/check", "admin-secret", nil, false, nil)
	checkBody := decodeJSONMap(t, checkResp.Body)
	checkResp.Body.Close()
	if checkResp.StatusCode != http.StatusOK || checkBody["ready"] != true {
		t.Fatalf("expected codex-cli check ready=true, got %v", checkBody)
	}

	// 2. Anti-RCE guard: executable patch and non-ollama dynamic creation are forbidden.
	badPatch := doAdminRequest(t, http.MethodPatch, h.URL+"/admin/api/models/codex-cli", "admin-secret", nil, false, map[string]any{
		"executable": "/bin/sh",
	})
	badPatch.Body.Close()
	if badPatch.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when attempting to alter executable path, got %d", badPatch.StatusCode)
	}

	badCreate := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/models", "admin-secret", nil, false, map[string]any{
		"id":       "evil-cli",
		"provider": "codex",
	})
	badCreate.Body.Close()
	if badCreate.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when creating non-ollama model dynamically, got %d", badCreate.StatusCode)
	}

	// 3. Add dynamic Ollama model, invoke it, verify SQLite persistence across restart, then delete it.
	createResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/models", "admin-secret", nil, false, map[string]any{
		"id":              "qwen-dyn",
		"provider":        "ollama",
		"base_url":        h.OllamaURL,
		"model":           "qwen3:8b",
		"timeout_seconds": 30,
		"max_concurrent":  2,
	})
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating dynamic ollama model, got %d", createResp.StatusCode)
	}

	// Restart using same DataDir -> model_overrides must restore disabled/dynamic state.
	_ = h.DB.Close()
	h2 := newTestHarness(t, harnessOptions{ExistingDataDir: h.DataDir})

	// Update dynamic ollama URL to h2's mock server to confirm dynamic model is registered on boot.
	modelsAfterRestart := doAdminRequest(t, http.MethodGet, h2.URL+"/admin/api/models", "admin-secret", nil, false, nil)
	modelsBytes, _ := io.ReadAll(modelsAfterRestart.Body)
	modelsAfterRestart.Body.Close()
	if !strings.Contains(string(modelsBytes), `"qwen-dyn"`) {
		t.Fatalf("expected dynamic model qwen-dyn to persist across restart, got %s", string(modelsBytes))
	}

	patchDynURL := doAdminRequest(t, http.MethodPatch, h2.URL+"/admin/api/models/qwen-dyn", "admin-secret", nil, false, map[string]any{
		"base_url": h2.OllamaURL,
	})
	patchDynURL.Body.Close()
	if patchDynURL.StatusCode != http.StatusOK {
		t.Fatalf("failed updating dynamic base_url: %d", patchDynURL.StatusCode)
	}

	chatResp := doAdminRequest(t, http.MethodPost, h2.URL+"/v1/chat/completions", "alice-secret", nil, false, map[string]any{
		"model": "qwen-dyn",
		"messages": []map[string]any{
			{"role": "user", "content": "Dynamic ping"},
		},
	})
	chatBody, _ := io.ReadAll(chatResp.Body)
	chatResp.Body.Close()
	if chatResp.StatusCode != http.StatusOK || !strings.Contains(string(chatBody), "provider=ollama") {
		t.Fatalf("expected dynamic ollama model response, got %d: %s", chatResp.StatusCode, string(chatBody))
	}
	reqs := h2.OllamaReqs()
	if len(reqs) == 0 || reqs[len(reqs)-1].Model != "qwen3:8b" {
		t.Fatalf("expected Ollama backend request with model qwen3:8b, got %+v", reqs)
	}

	delStatic := doAdminRequest(t, http.MethodDelete, h2.URL+"/admin/api/models/codex-cli", "admin-secret", nil, false, nil)
	delStatic.Body.Close()
	if delStatic.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 deleting YAML-configured model, got %d", delStatic.StatusCode)
	}

	delDyn := doAdminRequest(t, http.MethodDelete, h2.URL+"/admin/api/models/qwen-dyn", "admin-secret", nil, false, nil)
	delDyn.Body.Close()
	if delDyn.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 deleting dynamic model, got %d", delDyn.StatusCode)
	}
}

func TestAdminAPIKeyLifecycle(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})

	createResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/keys", "admin-secret", nil, false, map[string]any{
		"owner_id":       "frank",
		"name":           "CI Runner",
		"allowed_models": []string{"codex-cli"},
		"max_concurrent": 3,
	})
	created := decodeJSONMap(t, createResp.Body)
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating API key, got %d: %v", createResp.StatusCode, created)
	}

	token, _ := created["token"].(string)
	keyObj, _ := created["key"].(map[string]any)
	keyID, _ := keyObj["id"].(string)
	if !strings.HasPrefix(token, "gw-") || keyID == "" {
		t.Fatalf("expected gw- prefixed token and key ID, got %v", created)
	}

	// Allowed on codex-cli, forbidden on ollama-local.
	okResp := doAdminRequest(t, http.MethodPost, h.URL+"/v1/responses", token, nil, false, map[string]any{
		"model": "codex-cli",
		"input": "hello from new key",
	})
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("expected new key to invoke codex-cli, got %d", okResp.StatusCode)
	}

	forbidResp := doAdminRequest(t, http.MethodPost, h.URL+"/v1/responses", token, nil, false, map[string]any{
		"model": "ollama-local",
		"input": "should be blocked",
	})
	forbidResp.Body.Close()
	if forbidResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for disallowed model, got %d", forbidResp.StatusCode)
	}

	// Revoke -> immediate 401.
	revokeResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/keys/"+keyID+"/revoke", "admin-secret", nil, false, nil)
	revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 revoking key, got %d", revokeResp.StatusCode)
	}

	afterRevoke := doAdminRequest(t, http.MethodGet, h.URL+"/v1/models", token, nil, false, nil)
	afterRevoke.Body.Close()
	if afterRevoke.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after revocation, got %d", afterRevoke.StatusCode)
	}

	// Unrevoke + update allowed_models -> works everywhere.
	unrevoke := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/keys/"+keyID+"/unrevoke", "admin-secret", nil, false, nil)
	unrevoke.Body.Close()
	if unrevoke.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 unrevoking key, got %d", unrevoke.StatusCode)
	}

	upd := doAdminRequest(t, http.MethodPatch, h.URL+"/admin/api/keys/"+keyID, "admin-secret", nil, false, map[string]any{
		"allowed_models": []string{"*"},
		"max_concurrent": 5,
	})
	upd.Body.Close()
	if upd.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 updating key permissions, got %d", upd.StatusCode)
	}

	okOllama := doAdminRequest(t, http.MethodPost, h.URL+"/v1/chat/completions", token, nil, false, map[string]any{
		"model": "ollama-local",
		"messages": []map[string]any{
			{"role": "user", "content": "now allowed"},
		},
	})
	okOllama.Body.Close()
	if okOllama.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after expanding allowed_models, got %d", okOllama.StatusCode)
	}

	// Delete key -> 401 again.
	del := doAdminRequest(t, http.MethodDelete, h.URL+"/admin/api/keys/"+keyID, "admin-secret", nil, false, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 deleting key, got %d", del.StatusCode)
	}

	afterDel := doAdminRequest(t, http.MethodGet, h.URL+"/v1/models", token, nil, false, nil)
	afterDel.Body.Close()
	if afterDel.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after key deletion, got %d", afterDel.StatusCode)
	}
}

func TestAdminFilesPlaygroundSessionsAndRunCancel(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})

	// 1. Upload a file via Admin multipart endpoint, inspect content, and run Playground SSE with it.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("owner_id", "admin")
	part, err := mw.CreateFormFile("file", "notes.md")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = part.Write([]byte("START_MARKER_Alpha\nAdmin playground payload\nEND_MARKER_Gamma"))
	_ = mw.Close()

	upReq, _ := http.NewRequest(http.MethodPost, h.URL+"/admin/api/files", &buf)
	upReq.Header.Set("Authorization", "Bearer admin-secret")
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upResp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		t.Fatalf("upload file: %v", err)
	}
	upBody := decodeJSONMap(t, upResp.Body)
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 uploading file via admin, got %d: %v", upResp.StatusCode, upBody)
	}
	fileID, _ := upBody["id"].(string)

	contentResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/files/"+fileID+"/content", "admin-secret", nil, false, nil)
	contentBytes, _ := io.ReadAll(contentResp.Body)
	contentResp.Body.Close()
	if contentResp.StatusCode != http.StatusOK || !strings.Contains(string(contentBytes), "Admin playground payload") {
		t.Fatalf("unexpected file content response %d: %s", contentResp.StatusCode, string(contentBytes))
	}

	// Execute live SSE Playground prompt referencing the uploaded file.
	pgResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/playground", "admin-secret", nil, false, map[string]any{
		"owner_id":     "admin",
		"model":        "codex-cli",
		"instructions": "Be concise",
		"prompt":       "Inspect attached notes",
		"file_ids":     []string{fileID},
		"stream":       true,
	})
	frames := readSSEFrames(t, pgResp.Body)
	pgResp.Body.Close()
	if pgResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 SSE playground response, got %d", pgResp.StatusCode)
	}
	var sawCompleted bool
	var playgroundSessionID string
	for _, fr := range frames {
		if fr.Event == "response.completed" {
			sawCompleted = true
			if !strings.Contains(fr.Data, "notes.md") || !strings.Contains(fr.Data, "provider=codex") {
				t.Fatalf("playground completed event missing expected mock output: %s", fr.Data)
			}
		}
	}
	if !sawCompleted {
		t.Fatalf("expected response.completed SSE event in playground, got %+v", frames)
	}

	// 2. Inspect sessions list & detail, then delete the session.
	sessListResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/sessions", "admin-secret", nil, false, nil)
	sessList := decodeJSONMap(t, sessListResp.Body)
	sessListResp.Body.Close()
	sessItems, _ := sessList["data"].([]any)
	if len(sessItems) == 0 {
		t.Fatalf("expected at least 1 session after playground run")
	}
	firstSess, _ := sessItems[0].(map[string]any)
	playgroundSessionID, _ = firstSess["gateway_session_id"].(string)
	if playgroundSessionID == "" {
		t.Fatalf("expected non-empty gateway_session_id in %v", firstSess)
	}

	sessDetailResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/sessions/"+playgroundSessionID, "admin-secret", nil, false, nil)
	sessDetailBytes, _ := io.ReadAll(sessDetailResp.Body)
	sessDetailResp.Body.Close()
	if sessDetailResp.StatusCode != http.StatusOK || !strings.Contains(string(sessDetailBytes), "Inspect attached notes") {
		t.Fatalf("expected history entry in session detail, got %d: %s", sessDetailResp.StatusCode, string(sessDetailBytes))
	}

	delSess := doAdminRequest(t, http.MethodDelete, h.URL+"/admin/api/sessions/"+playgroundSessionID, "admin-secret", nil, false, nil)
	delSess.Body.Close()
	if delSess.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 deleting session, got %d", delSess.StatusCode)
	}

	// File is now unreferenced (active_refs == 0), so admin delete must succeed.
	delFile := doAdminRequest(t, http.MethodDelete, h.URL+"/admin/api/files/"+fileID, "admin-secret", nil, false, nil)
	delFile.Body.Close()
	if delFile.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 deleting unreferenced file, got %d", delFile.StatusCode)
	}

	// 3. Start a slow child-spawning run via /v1/responses and cancel it through /admin/api/runs/{id}/cancel.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	slowDone := make(chan int, 1)
	go func() {
		resp := doAdminRequest(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", nil, false, map[string]any{
			"model": "codex-cli",
			"input": "TRIGGER_SLEEP_AND_CHILD CHILD_PID_FILE=" + pidFile,
		})
		resp.Body.Close()
		slowDone <- resp.StatusCode
	}()

	var runningRunID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runsResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/runs?status=running", "admin-secret", nil, false, nil)
		runsBody := decodeJSONMap(t, runsResp.Body)
		runsResp.Body.Close()
		items, _ := runsBody["data"].([]any)
		pidBytes, err := os.ReadFile(pidFile)
		if len(items) > 0 && err == nil && len(bytes.TrimSpace(pidBytes)) > 0 {
			r0, _ := items[0].(map[string]any)
			runningRunID, _ = r0["id"].(string)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if runningRunID == "" {
		t.Fatalf("timed out waiting for active running task and child PID file")
	}

	childPIDBytes, _ := os.ReadFile(pidFile)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childPIDBytes)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child PID: %q (%v)", string(childPIDBytes), err)
	}

	cancelResp := doAdminRequest(t, http.MethodPost, h.URL+"/admin/api/runs/"+runningRunID+"/cancel", "admin-secret", nil, false, nil)
	cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 cancelling active run %s, got %d", runningRunID, cancelResp.StatusCode)
	}

	select {
	case <-slowDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("cancelled request did not finish in time")
	}

	// Verify run recorded cancelled and spawned grandchild PID was killed via process-group signal.
	runDetailResp := doAdminRequest(t, http.MethodGet, h.URL+"/admin/api/runs/"+runningRunID, "admin-secret", nil, false, nil)
	runDetail := decodeJSONMap(t, runDetailResp.Body)
	runDetailResp.Body.Close()
	if runDetail["status"] != "cancelled" {
		t.Fatalf("expected cancelled run status, got %v", runDetail["status"])
	}

	killWait := time.Now().Add(2 * time.Second)
	childDead := false
	for time.Now().Before(killWait) {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			childDead = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !childDead {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("expected child process %d to be killed on admin cancel", childPID)
	}
}
