package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cli-ai-gateway/internal/api"
	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/defaults"
	"cli-ai-gateway/internal/pools"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/storage"
	"cli-ai-gateway/internal/tools"
)

func testPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 50, G: 130, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func testWAVBytes() []byte {
	b := make([]byte, 44)
	copy(b[0:4], "RIFF")
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	b[16] = 16
	b[20] = 1
	b[22] = 1
	copy(b[36:40], "data")
	return b
}

func TestMultimodalAndToolsEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// 1. Mock Ollama server supporting vision images, client tool calls, and server tool loop.
	var (
		ollamaMu         sync.Mutex
		sawImageInOllama bool
		ollamaStep       int
	)
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "qwen-vl:latest"}},
			})
			return
		}
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages []struct {
				Role    string   `json:"role"`
				Content string   `json:"content"`
				Images  []string `json:"images"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		var lastContent string
		for _, m := range body.Messages {
			if len(m.Images) > 0 {
				ollamaMu.Lock()
				sawImageInOllama = true
				ollamaMu.Unlock()
			}
			lastContent = m.Content
		}

		// If client tool "get_weather" is requested, return a native Ollama tool_call.
		for _, tl := range body.Tools {
			if tl.Function.Name == "get_weather" && !strings.Contains(lastContent, "<tool_results>") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": "",
						"tool_calls": []map[string]any{
							{
								"id": "call_weather_1",
								"function": map[string]any{
									"name":      "get_weather",
									"arguments": map[string]any{"city": "Moscow"},
								},
							},
						},
					},
					"done":              true,
					"prompt_eval_count": 10,
					"eval_count":        5,
				})
				return
			}
		}

		// If server tool "workspace_save_output" is available and not yet executed, call it first!
		hasSaveTool := false
		for _, tl := range body.Tools {
			if tl.Function.Name == "workspace_save_output" {
				hasSaveTool = true
			}
		}
		ollamaMu.Lock()
		step := ollamaStep
		if hasSaveTool && !strings.Contains(lastContent, "<tool_results>") {
			ollamaStep++
		}
		ollamaMu.Unlock()

		if hasSaveTool && !strings.Contains(lastContent, "<tool_results>") && step == 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{
						{
							"id": "call_srv_save_1",
							"function": map[string]any{
								"name":      "workspace_save_output",
								"arguments": map[string]any{"filename": "summary.md", "content": "# Done"},
							},
						},
					},
				},
				"done":              true,
				"prompt_eval_count": 12,
				"eval_count":        6,
			})
			return
		}

		reply := "OK"
		if strings.Contains(lastContent, "<tool_results>") {
			reply = "Saved summary.md via server tool!"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]any{"role": "assistant", "content": reply},
			"done":              true,
			"prompt_eval_count": 15,
			"eval_count":        7,
		})
	}))
	defer ollamaSrv.Close()

	// 2. Mock external media backends (Whisper, TTS, ImageGen).
	samplePNG := testPNGBytes(t, 24, 16)
	sampleWAV := testWAVBytes()
	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/transcriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"text": "recognized audio text"})
		case "/v1/audio/speech":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(sampleWAV)
		case "/v1/images/generations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(samplePNG)}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mediaSrv.Close()

	// 3. Spin up full gateway router.
	db, err := storage.OpenSQLite(ctx, filepath.Join(root, "gw.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer db.Close()

	enabledTrue := true
	cfg := &config.Config{
		Server: config.ServerConfig{Listen: "127.0.0.1:0", ReadHeaderTimeoutSeconds: 10},
		Storage: config.StorageConfig{
			Path:         root,
			SQLitePath:   filepath.Join(root, "gw.db"),
			MaxFileBytes: 50 * 1024 * 1024,
			QuotaBytes:   200 * 1024 * 1024,
		},
		Limits: config.LimitsConfig{
			MaxGlobalProcesses:   4,
			MaxExecutorProcesses: 4,
			MaxUserProcesses:     4,
			MaxQueueSize:         8,
			MaxFilesPerTask:      10,
			MaxOutputBytes:       4 * 1024 * 1024,
		},
		Admin: config.AdminConfig{Enabled: true, Token: "admin-secret"},
		Auth: config.AuthConfig{
			Keys: []config.APIKeyConfig{{
				ID:            "k1",
				OwnerID:       "alice",
				Name:          "Alice",
				Key:           "alice-token",
				AllowedModels: []string{"*"},
				MaxConcurrent: 4,
			}},
		},
		Models: []config.ModelConfig{{
			ID:             "ollama-vl",
			Provider:       "ollama",
			BaseURL:        ollamaSrv.URL,
			Model:          "qwen-vl:latest",
			Enabled:        &enabledTrue,
			TimeoutSeconds: 30,
			MaxConcurrent:  4,
		}},
	}

	authStore, err := auth.NewStore(ctx, db, cfg.Auth.Keys)
	if err != nil {
		t.Fatalf("auth.NewStore: %v", err)
	}
	fileStore, err := storage.NewFileStore(db, root, cfg.Storage.MaxFileBytes, cfg.Storage.QuotaBytes, time.Hour)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	sessMgr, err := session.NewManager(db, root, 10*time.Second, time.Hour, time.Hour, true)
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	procMgr := process.NewManager(process.Limits{MaxGlobal: 4, MaxPerExecutor: 4, MaxPerUser: 4, MaxQueueSize: 8, QueueWaitTimeout: 10 * time.Second})
	reg := executor.NewRegistry()
	defaults.RegisterAll(reg, procMgr)
	if err := reg.RegisterModels(cfg.Models); err != nil {
		t.Fatalf("RegisterModels: %v", err)
	}
	poolStore := pools.NewStore(db.SQL())
	toolReg := tools.NewRegistry(fileStore, tools.NewBackendStore(db.SQL()))

	for _, key := range []string{"whisper", "tts", "image_gen"} {
		if _, err := toolReg.Store().UpsertBackend(ctx, tools.BackendConfig{
			Key:     key,
			Name:    key,
			BaseURL: mediaSrv.URL,
			Enabled: true,
		}); err != nil {
			t.Fatalf("UpsertBackend: %v", err)
		}
	}

	router := api.NewRouter(api.Dependencies{
		Config:    cfg,
		StartedAt: time.Now(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:        db,
		Auth:      authStore,
		Files:     fileStore,
		Sessions:  sessMgr,
		Registry:  reg,
		Pools:     poolStore,
		Tools:     toolReg,
		ProcMgr:   procMgr,
	})
	gw := httptest.NewServer(router)
	defer gw.Close()

	// Test A: Upload binary PNG via /v1/files with purpose="vision".
	var mbuf bytes.Buffer
	mw := multipart.NewWriter(&mbuf)
	_ = mw.WriteField("purpose", "vision")
	fw, _ := mw.CreateFormFile("file", "pic.png")
	_, _ = fw.Write(samplePNG)
	_ = mw.Close()

	upReq, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/files", &mbuf)
	upReq.Header.Set("Authorization", "Bearer alice-token")
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upResp, err := http.DefaultClient.Do(upReq)
	if err != nil || upResp.StatusCode != http.StatusCreated {
		t.Fatalf("upload PNG failed: err=%v status=%v", err, upResp.StatusCode)
	}
	var uploadedFile storage.FileRecord
	_ = json.NewDecoder(upResp.Body).Decode(&uploadedFile)
	_ = upResp.Body.Close()
	if uploadedFile.MediaKind != "image" || uploadedFile.Width != 24 || uploadedFile.Height != 16 {
		t.Fatalf("unexpected uploaded PNG metadata: %+v", uploadedFile)
	}

	// Test B: Chat Completions with inline base64 image_url + client tool call -> finish_reason="tool_calls".
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(samplePNG)
	status, rawResp := doJSONReq(t, http.MethodPost, gw.URL+"/v1/chat/completions", "alice-token", map[string]any{
		"model": "ollama-vl",
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "What is the weather in Moscow?"},
					{"type": "image_url", "image_url": map[string]any{"url": dataURI}},
				},
			},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "Weather lookup",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
				},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("chat completions with tools failed: %d %s", status, rawResp)
	}
	if !strings.Contains(string(rawResp), `"finish_reason":"tool_calls"`) || !strings.Contains(string(rawResp), `"get_weather"`) {
		t.Fatalf("expected tool_calls in response, got: %s", rawResp)
	}
	ollamaMu.Lock()
	gotImg := sawImageInOllama
	ollamaMu.Unlock()
	if !gotImg {
		t.Fatal("expected Ollama to receive base64 image in /api/chat messages[].images")
	}

	// Test C: Bind server tool workspace_save_output to a published model and verify autonomous server tool loop!
	if _, err := poolStore.UpsertPublishedModel(ctx, pools.PublishedModel{
		ID:       "smart-tools",
		Kind:     "single",
		Strategy: pools.StrategyPriorityOrder,
		Enabled:  true,
		Targets:  []pools.PublishedTarget{{ProviderID: "ollama-vl", ProviderModel: "qwen-vl:latest", Priority: 1, Enabled: true}},
	}); err != nil {
		t.Fatalf("UpsertPublishedModel: %v", err)
	}
	if err := toolReg.Store().SetModelTools(ctx, "smart-tools", []string{"workspace_save_output"}); err != nil {
		t.Fatalf("SetModelTools: %v", err)
	}

	status, rawResp = doJSONReq(t, http.MethodPost, gw.URL+"/v1/chat/completions", "alice-token", map[string]any{
		"model":    "smart-tools",
		"messages": []map[string]any{{"role": "user", "content": "Save a summary file"}},
	})
	if status != http.StatusOK {
		t.Fatalf("server tool loop failed: %d %s", status, rawResp)
	}
	if !strings.Contains(string(rawResp), "Saved summary.md via server tool!") || !strings.Contains(string(rawResp), "generated_files") {
		t.Fatalf("expected server tool loop answer + generated_files, got: %s", rawResp)
	}

	// Test D: OpenAI media facades (/v1/audio/transcriptions, /v1/audio/speech, /v1/images/generations).
	var abuf bytes.Buffer
	amw := multipart.NewWriter(&abuf)
	afw, _ := amw.CreateFormFile("file", "clip.wav")
	_, _ = afw.Write(sampleWAV)
	_ = amw.Close()
	sttReq, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/audio/transcriptions", &abuf)
	sttReq.Header.Set("Authorization", "Bearer alice-token")
	sttReq.Header.Set("Content-Type", amw.FormDataContentType())
	sttResp, err := http.DefaultClient.Do(sttReq)
	if err != nil || sttResp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/audio/transcriptions failed: err=%v status=%v", err, sttResp.StatusCode)
	}
	sttBody, _ := io.ReadAll(sttResp.Body)
	_ = sttResp.Body.Close()
	if !strings.Contains(string(sttBody), "recognized audio text") {
		t.Fatalf("unexpected STT body: %s", sttBody)
	}

	ttsStatus, ttsBody := doJSONReq(t, http.MethodPost, gw.URL+"/v1/audio/speech", "alice-token", map[string]any{
		"model": "tts-1", "input": "Hello", "voice": "alloy", "response_format": "wav",
	})
	if ttsStatus != http.StatusOK || !bytes.HasPrefix(ttsBody, []byte("RIFF")) {
		t.Fatalf("/v1/audio/speech failed: %d (len=%d)", ttsStatus, len(ttsBody))
	}

	imgStatus, imgBody := doJSONReq(t, http.MethodPost, gw.URL+"/v1/images/generations", "alice-token", map[string]any{
		"model": "gpt-image-1", "prompt": "Blue sky",
	})
	if imgStatus != http.StatusOK || !strings.Contains(string(imgBody), `"b64_json"`) {
		t.Fatalf("/v1/images/generations failed: %d %s", imgStatus, imgBody)
	}
}
