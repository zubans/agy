package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"cli-ai-gateway/internal/storage"
	"cli-ai-gateway/pkg/client"
)

func doJSONReq(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func uploadTestFile(t *testing.T, baseURL, token, filename, purpose, content string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("purpose", purpose); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/files", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func TestIntegration_HealthAndPartialReadiness(t *testing.T) {
	h := newTestHarness(t, harnessOptions{IncludeBrokenCLI: true})

	code, body := doJSONReq(t, http.MethodGet, h.URL+"/health", "", nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("/health unexpected: code=%d body=%s", code, body)
	}

	// Ready remains 200 degraded when 1 executor is missing while healthy executors work.
	code, body = doJSONReq(t, http.MethodGet, h.URL+"/ready", "", nil)
	if code != http.StatusOK {
		t.Fatalf("expected /ready 200 degraded with partial executors, got %d (%s)", code, body)
	}
	var ready struct {
		Status    string `json:"status"`
		Executors map[string]struct {
			Ready bool `json:"ready"`
		} `json:"executors"`
	}
	if err := json.Unmarshal(body, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.Status != "degraded" || !ready.Executors["codex-cli"].Ready || ready.Executors["broken-cli"].Ready {
		t.Fatalf("unexpected readiness report: %+v", ready)
	}

	// Broken executor rejects execution with 503 while codex-cli still works.
	code, _ = doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "broken-cli",
		"input": "hello",
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for broken-cli, got %d", code)
	}
	code, _ = doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "codex-cli",
		"input": "hello",
	})
	if code != http.StatusOK {
		t.Fatalf("expected healthy codex-cli 200, got %d", code)
	}
}

func TestIntegration_LargePromptFileMarkers(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})
	c := client.New(h.URL, "alice-secret")

	// Build a >1.2 MiB prompt.md with explicit start, middle, and end markers.
	pad := strings.Repeat("Data-line-0123456789abcdef-cyrillic-Проверка.\n", 13000)
	largeTask := "START_MARKER_Alpha\n" + pad + "MID_MARKER_Beta\n" + pad + "END_MARKER_Gamma\n"
	doc := client.PromptDocument{
		SystemInstructions: "Follow large spec cleanly.",
		Task:               largeTask,
		ExpectedOutput:     "Verify all three markers.",
	}
	if len(doc.RenderMarkdown()) < 1200*1024 {
		t.Fatalf("expected >1.2 MiB prompt, got %d bytes", len(doc.RenderMarkdown()))
	}

	ctx := context.Background()
	fObj, err := c.UploadPrompt(ctx, "prompt.md", doc)
	if err != nil {
		t.Fatalf("UploadPrompt failed: %v", err)
	}

	resp, err := c.CreateResponse(ctx, client.ResponseParams{
		Model:   "codex-cli",
		FileIDs: []string{fObj.ID},
		Text:    "Process the uploaded spec.",
	})
	if err != nil {
		t.Fatalf("CreateResponse with 1.2MiB prompt.md failed: %v", err)
	}
	outText := resp.OutputText()
	if !strings.Contains(outText, "start=true mid=true end=true") {
		t.Fatalf("mock codex did not observe all 3 markers in staged ./input/prompt.md: %s", outText)
	}

	// Verify run workspace input/ was cleaned up by default CleanupRunInput=true.
	runsDir := filepath.Join(h.DataDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected run directory in %s: err=%v", runsDir, err)
	}
	for _, e := range entries {
		inDir := filepath.Join(runsDir, e.Name(), "input")
		if _, err := os.Stat(inDir); !os.IsNotExist(err) {
			t.Fatalf("expected temporary run input dir %s to be cleaned up", inDir)
		}
	}
}

func TestIntegration_StreamingResponsesAndChatIsolation(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})

	// 1. /v1/responses streaming uses strictly Responses SSE events.
	reqBody, _ := json.Marshal(map[string]any{
		"model":  "codex-cli",
		"input":  "stream test",
		"stream": true,
	})
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer alice-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames := readSSEFrames(t, resp.Body)
	_ = resp.Body.Close()

	var seenCreated, seenDelta, seenCompleted bool
	for _, f := range frames {
		if strings.Contains(f.Data, "chat.completion.chunk") || f.Data == "[DONE]" {
			t.Fatalf("/v1/responses leaked chat completions SSE format: %+v", f)
		}
		switch f.Event {
		case "response.created":
			seenCreated = true
		case "response.output_text.delta":
			seenDelta = true
		case "response.completed":
			seenCompleted = true
		}
	}
	if !seenCreated || !seenDelta || !seenCompleted {
		t.Fatalf("missing Responses SSE lifecycle events: created=%v delta=%v completed=%v", seenCreated, seenDelta, seenCompleted)
	}

	// 2. /v1/responses mid-stream failure emits event: error + response.failed.
	failBody, _ := json.Marshal(map[string]any{
		"model":  "codex-cli",
		"input":  "TRIGGER_MIDSTREAM_FAIL",
		"stream": true,
	})
	reqFail, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/responses", bytes.NewReader(failBody))
	reqFail.Header.Set("Authorization", "Bearer alice-secret")
	reqFail.Header.Set("Content-Type", "application/json")
	respFail, err := http.DefaultClient.Do(reqFail)
	if err != nil {
		t.Fatal(err)
	}
	failFrames := readSSEFrames(t, respFail.Body)
	_ = respFail.Body.Close()
	var seenErrorEv, seenFailedEv bool
	for _, f := range failFrames {
		if f.Event == "error" {
			seenErrorEv = true
		}
		if f.Event == "response.failed" {
			seenFailedEv = true
		}
	}
	if !seenErrorEv || !seenFailedEv {
		t.Fatalf("expected mid-stream error & response.failed events, got %+v", failFrames)
	}

	// 3. /v1/chat/completions streaming uses strictly chat.completion.chunk + [DONE].
	chatBody, _ := json.Marshal(map[string]any{
		"model":  "codex-cli",
		"stream": true,
		"messages": []map[string]string{
			{"role": "system", "content": "You are helpful"},
			{"role": "user", "content": "Hello chat"},
		},
	})
	reqChat, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/chat/completions", bytes.NewReader(chatBody))
	reqChat.Header.Set("Authorization", "Bearer alice-secret")
	reqChat.Header.Set("Content-Type", "application/json")
	respChat, err := http.DefaultClient.Do(reqChat)
	if err != nil {
		t.Fatal(err)
	}
	chatFrames := readSSEFrames(t, respChat.Body)
	_ = respChat.Body.Close()
	var seenChunk, seenDone bool
	for _, f := range chatFrames {
		if strings.HasPrefix(f.Event, "response.") {
			t.Fatalf("/v1/chat/completions leaked Responses SSE event %q", f.Event)
		}
		if f.Data == "[DONE]" {
			seenDone = true
		} else if strings.Contains(f.Data, `"chat.completion.chunk"`) {
			seenChunk = true
		}
	}
	if !seenChunk || !seenDone {
		t.Fatalf("missing chat completion SSE chunks or [DONE]: %+v", chatFrames)
	}
}

func TestIntegration_SessionContinuityAndExecutorSwitch(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})
	c := client.New(h.URL, "alice-secret")
	ctx := context.Background()

	// Turn 1: fresh session on codex-cli.
	r1, err := c.CreateResponse(ctx, client.ResponseParams{
		Model: "codex-cli",
		Text:  "Remember secret word MANGO.",
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if !strings.Contains(r1.OutputText(), "resume=false has_context=false") {
		t.Fatalf("turn 1 unexpected: %s", r1.OutputText())
	}

	// Turn 2: same provider via previous_response_id -> resumes CLI session WITHOUT context.md.
	r2, err := c.CreateResponse(ctx, client.ResponseParams{
		Model:              "codex-cli",
		PreviousResponseID: r1.ID,
		Text:               "What was the secret word?",
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !strings.Contains(r2.OutputText(), "resume=true has_context=false") {
		t.Fatalf("turn 2 expected CLI resume without context.md: %s", r2.OutputText())
	}

	// Turn 3: switch from codex-cli to ollama-local -> restores history via context.md!
	r3, err := c.CreateResponse(ctx, client.ResponseParams{
		Model:              "ollama-local",
		PreviousResponseID: r2.ID,
		Text:               "Continue in Ollama.",
	})
	if err != nil {
		t.Fatalf("turn 3 ollama switch: %v", err)
	}
	if !strings.Contains(r3.OutputText(), "provider=ollama has_context=true") {
		t.Fatalf("turn 3 expected Ollama with restored context: %s", r3.OutputText())
	}
	reqs := h.OllamaReqs()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Messages[len(reqs[0].Messages)-1]["content"], "MANGO") {
		t.Fatalf("expected Turn 1 history word MANGO restored into Ollama prompt, got %+v", reqs)
	}

	// Turn 4: switch from ollama-local to claude-cli -> fresh Claude session WITH ./input/context.md.
	r4, err := c.CreateResponse(ctx, client.ResponseParams{
		Model:              "claude-cli",
		PreviousResponseID: r3.ID,
		Text:               "Now continue in Claude.",
	})
	if err != nil {
		t.Fatalf("turn 4 claude switch: %v", err)
	}
	if !strings.Contains(r4.OutputText(), "provider=claude has_context=true") {
		t.Fatalf("turn 4 expected Claude with restored context.md: %s", r4.OutputText())
	}
}

func TestIntegration_SameSessionSequentialVsBusyConflict409(t *testing.T) {
	// Short session lock timeout (1s) to test 409 when locked by a slow task.
	h := newTestHarness(t, harnessOptions{SessionLockTimeoutSeconds: 1, DefaultTimeoutSeconds: 10})
	c := client.New(h.URL, "alice-secret")
	ctx := context.Background()

	seed, err := c.CreateResponse(ctx, client.ResponseParams{
		Model: "codex-cli",
		Text:  "seed session",
	})
	if err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(t.TempDir(), "slow.pid")
	var wg sync.WaitGroup
	wg.Add(1)
	slowCtx, cancelSlow := context.WithCancel(ctx)
	defer cancelSlow()

	go func() {
		defer wg.Done()
		_, _ = c.CreateResponse(slowCtx, client.ResponseParams{
			Model:              "codex-cli",
			PreviousResponseID: seed.ID,
			Text:               "TRIGGER_SLEEP_AND_CHILD CHILD_PID_FILE=" + pidFile,
		})
	}()

	// Wait until slow child has started so we know the session lock is held.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Concurrent request on SAME session must block until 1s lock timeout and return 409.
	code, body := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model":                "codex-cli",
		"previous_response_id": seed.ID,
		"input":                "competing same session",
	})
	if code != http.StatusConflict || !strings.Contains(string(body), "session_conflict") {
		t.Fatalf("expected 409 session_conflict, got %d (%s)", code, body)
	}

	// Concurrent request on DIFFERENT session succeeds immediately in parallel!
	otherCode, _ := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "codex-cli",
		"input": "independent session",
	})
	if otherCode != http.StatusOK {
		t.Fatalf("expected independent session 200 while other session locked, got %d", otherCode)
	}

	cancelSlow()
	wg.Wait()
}

func TestIntegration_TimeoutAndDisconnectKillProcessGroup(t *testing.T) {
	h := newTestHarness(t, harnessOptions{DefaultTimeoutSeconds: 1})

	// 1. Execution timeout kills child PID and returns 504.
	pidFile1 := filepath.Join(t.TempDir(), "timeout.pid")
	code, _ := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
		"model": "codex-cli",
		"input": "TRIGGER_SLEEP_AND_CHILD CHILD_PID_FILE=" + pidFile1,
	})
	if code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 timeout, got %d", code)
	}
	assertPIDDead(t, pidFile1)

	// 2. Client disconnect during streaming kills child PID.
	pidFile2 := filepath.Join(t.TempDir(), "disconnect.pid")
	reqCtx, cancelReq := context.WithCancel(context.Background())
	rawBody, _ := json.Marshal(map[string]any{
		"model":  "codex-cli",
		"input":  "TRIGGER_SLEEP_AND_CHILD CHILD_PID_FILE=" + pidFile2,
		"stream": true,
	})
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, h.URL+"/v1/responses", bytes.NewReader(rawBody))
	req.Header.Set("Authorization", "Bearer alice-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if strings.Contains(line, "started child=") {
			break
		}
		if err != nil {
			t.Fatalf("stream ended before child started: %v", err)
		}
	}
	cancelReq()
	_ = resp.Body.Close()
	assertPIDDead(t, pidFile2)
}

func assertPIDDead(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file %s: %v", pidFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid pid %q", string(raw))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("child pid %d remained alive after timeout/cancel", pid)
}

func TestIntegration_ActiveFileDeleteConflict409AndQueue429(t *testing.T) {
	// Test active file 409 conflict & release.
	h := newTestHarness(t, harnessOptions{})
	code, fMeta := uploadTestFile(t, h.URL, "alice-secret", "slow.md", "user_data", "TRIGGER_SLOW_FILE slow payload")
	if code != http.StatusCreated {
		t.Fatalf("upload failed: %d %+v", code, fMeta)
	}
	fileID := fMeta["id"].(string)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, _ := doJSONReq(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{
			"model": "codex-cli",
			"input": []map[string]any{
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "input_file", "file_id": fileID},
					},
				},
			},
		})
		if c != http.StatusOK {
			t.Errorf("expected 200 on slow file run, got %d", c)
		}
	}()

	// Wait until the active run acquires the file (active_refs == 1), then verify DELETE -> 409.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var refs int
		if err := h.DB.SQL().QueryRow(`SELECT active_refs FROM files WHERE id = ?`, fileID).Scan(&refs); err == nil && refs > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	delCode, delBody := doJSONReq(t, http.MethodDelete, h.URL+"/v1/files/"+fileID, "alice-secret", nil)
	if delCode != http.StatusConflict {
		wg.Wait()
		t.Fatalf("expected 409 Conflict when deleting file during active run, got %d (%s)", delCode, delBody)
	}
	wg.Wait()
	delAfterCode, _ := doJSONReq(t, http.MethodDelete, h.URL+"/v1/files/"+fileID, "alice-secret", nil)
	if delAfterCode != http.StatusOK {
		t.Fatalf("expected 200 delete after run completed, got %d", delAfterCode)
	}

	// Test concurrency queue full -> 429.
	qh := newTestHarness(t, harnessOptions{
		MaxGlobalProcesses:      1,
		MaxQueueSize:            1,
		QueueWaitTimeoutSeconds: 5,
		DefaultTimeoutSeconds:   10,
	})
	pidFile := filepath.Join(t.TempDir(), "q.pid")
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	var qwg sync.WaitGroup
	qwg.Add(2)
	go func() {
		defer qwg.Done()
		raw, _ := json.Marshal(map[string]any{"model": "codex-cli", "input": "TRIGGER_SLEEP_AND_CHILD CHILD_PID_FILE=" + pidFile})
		req, _ := http.NewRequestWithContext(ctx1, http.MethodPost, qh.URL+"/v1/responses", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer alice-secret")
		_, _ = http.DefaultClient.Do(req)
	}()
	for i := 0; i < 40; i++ {
		if qh.ProcMgr.Stats().ActiveGlobal == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	go func() {
		defer qwg.Done()
		_, _ = doJSONReq(t, http.MethodPost, qh.URL+"/v1/responses", "bob-secret", map[string]any{"model": "codex-cli", "input": "queued"})
	}()
	for i := 0; i < 40; i++ {
		if qh.ProcMgr.Stats().Queued == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	code429, _ := doJSONReq(t, http.MethodPost, qh.URL+"/v1/responses", "bob-secret", map[string]any{"model": "codex-cli", "input": "overflow"})
	if code429 != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when global queue full, got %d", code429)
	}
	cancel1()
	qwg.Wait()
}

func TestIntegration_RestartPersistenceAndInterruptedRecovery(t *testing.T) {
	dataDir := t.TempDir()
	h1 := newTestHarness(t, harnessOptions{ExistingDataDir: dataDir})

	code, fMeta := uploadTestFile(t, h1.URL, "alice-secret", "keep.md", "user_data", "persist across restart")
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	fileID := fMeta["id"].(string)

	c1 := client.New(h1.URL, "alice-secret")
	resp1, err := c1.CreateResponse(context.Background(), client.ResponseParams{
		Model: "codex-cli",
		Text:  "remember RESTART_KEY_99",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted run row and dangling active_refs=1 before shutdown.
	_, err = h1.DB.SQL().Exec(`UPDATE files SET active_refs = 1 WHERE id = ?`, fileID)
	if err != nil {
		t.Fatal(err)
	}
	var sessID string
	if err := h1.DB.SQL().QueryRow(`SELECT session_id FROM responses WHERE id = ?`, resp1.ID).Scan(&sessID); err != nil {
		t.Fatal(err)
	}
	_, err = h1.DB.SQL().Exec(`
		INSERT INTO runs (id, owner_id, session_id, response_id, model, provider, status, created_at)
		VALUES ('run_crash_1', 'alice', ?, 'resp_crash_1', 'codex-cli', 'codex', 'running', ?)
	`, sessID, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}

	h1.Server.Close()
	_ = h1.DB.Close()

	// OpenSQLite runs RecoverInterrupted on startup; verify interrupted run and released active_refs.
	db2, err := storage.OpenSQLite(context.Background(), filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	var st string
	if err := db2.SQL().QueryRow(`SELECT status FROM runs WHERE id = 'run_crash_1'`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "interrupted" {
		t.Fatalf("expected crashed run status 'interrupted' on restart, got %q", st)
	}
	var refs int
	if err := db2.SQL().QueryRow(`SELECT active_refs FROM files WHERE id = ?`, fileID).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Fatalf("expected dangling active_refs reset to 0 on restart, got %d", refs)
	}
	_ = db2.Close()

	// Full harness on ExistingDataDir continues previous session seamlessly!
	h2 := newTestHarness(t, harnessOptions{ExistingDataDir: dataDir})
	c2 := client.New(h2.URL, "alice-secret")
	resp2, err := c2.CreateResponse(context.Background(), client.ResponseParams{
		Model:              "codex-cli",
		PreviousResponseID: resp1.ID,
		Text:               "after restart",
	})
	if err != nil {
		t.Fatalf("resume after restart failed: %v", err)
	}
	if !strings.Contains(resp2.OutputText(), "resume=true") {
		t.Fatalf("expected resume=true after restart, got %s", resp2.OutputText())
	}
}
