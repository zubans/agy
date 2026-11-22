package tools_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/storage"
	"cli-ai-gateway/internal/tools"
)

func setupToolsEnv(t *testing.T) (*storage.FileStore, *tools.Registry) {
	t.Helper()
	root := t.TempDir()
	db, err := storage.OpenSQLite(context.Background(), filepath.Join(root, "gw.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fs, err := storage.NewFileStore(db, root, 50*1024*1024, 200*1024*1024, time.Hour)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	reg := tools.NewRegistry(fs, tools.NewBackendStore(db.SQL()))
	return fs, reg
}

func makeSamplePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 120, G: 200, B: 80, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func makeSampleWAV() []byte {
	// Minimal valid RIFF/WAVE header (44 bytes)
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

func TestImageInfoTransformAndDiagramTools(t *testing.T) {
	ctx := context.Background()
	fs, reg := setupToolsEnv(t)

	pngBytes := makeSamplePNG(t, 40, 20)
	rec, err := fs.Save(ctx, "alice", "chart.png", "vision", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Save PNG: %v", err)
	}
	if rec.MediaKind != storage.MediaKindImage || rec.Width != 40 || rec.Height != 20 {
		t.Fatalf("unexpected PNG metadata: %+v", rec)
	}

	var generated []executor.GeneratedFileRef
	runCtx := tools.RunContext{
		RunID:         "run_1",
		OwnerID:       "alice",
		AttachedFiles: []storage.FileRecord{rec},
		OnFileGenerated: func(ref executor.GeneratedFileRef) {
			generated = append(generated, ref)
		},
	}

	// 1. workspace_list_files
	listOut, err := reg.Execute(ctx, runCtx, "workspace_list_files", nil)
	if err != nil || !strings.Contains(listOut.Text, rec.ID) {
		t.Fatalf("workspace_list_files failed: err=%v out=%+v", err, listOut)
	}

	// 2. image_info
	infoArgs, _ := json.Marshal(map[string]any{"file_id": rec.ID})
	infoOut, err := reg.Execute(ctx, runCtx, "image_info", infoArgs)
	if err != nil || infoOut.Data["width"] != 40 || infoOut.Data["height"] != 20 {
		t.Fatalf("image_info failed: err=%v data=%+v", err, infoOut.Data)
	}

	// 3. image_transform (resize to 16x8 JPEG)
	txArgs, _ := json.Marshal(map[string]any{"file_id": rec.ID, "width": 16, "height": 8, "format": "jpeg", "quality": 80})
	txOut, err := reg.Execute(ctx, runCtx, "image_transform", txArgs)
	if err != nil {
		t.Fatalf("image_transform failed: %v", err)
	}
	if len(txOut.GeneratedFiles) != 1 || txOut.Data["width"] != 16 || txOut.Data["height"] != 8 {
		t.Fatalf("unexpected image_transform result: %+v", txOut)
	}

	// 4. image_render_diagram (HTML widget)
	diagArgs, _ := json.Marshal(map[string]any{
		"kind":     "html",
		"source":   "<!doctype html><html><body><h1>Chart</h1></body></html>",
		"filename": "widget",
	})
	diagOut, err := reg.Execute(ctx, runCtx, "image_render_diagram", diagArgs)
	if err != nil || len(diagOut.GeneratedFiles) != 1 {
		t.Fatalf("image_render_diagram failed: err=%v out=%+v", err, diagOut)
	}
	if len(generated) != 2 {
		t.Fatalf("expected 2 OnFileGenerated callbacks, got %d", len(generated))
	}
}

func TestAudioSTT_TTS_AndImageGenBackends(t *testing.T) {
	ctx := context.Background()
	fs, reg := setupToolsEnv(t)

	samplePNG := makeSamplePNG(t, 12, 12)
	sampleWAV := makeSampleWAV()

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/transcriptions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"text":"Привет, это тестовая расшифровка"}`))
		case "/v1/audio/speech":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(sampleWAV)
		case "/v1/images/generations":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"b64_json": base64.StdEncoding.EncodeToString(samplePNG)},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockSrv.Close()

	for _, key := range []string{"whisper", "tts", "image_gen"} {
		if _, err := reg.Store().UpsertBackend(ctx, tools.BackendConfig{
			Key:     key,
			Name:    key,
			BaseURL: mockSrv.URL,
			Enabled: true,
		}); err != nil {
			t.Fatalf("UpsertBackend(%s): %v", key, err)
		}
	}

	audioRec, err := fs.Save(ctx, "bob", "voice.wav", "audio", bytes.NewReader(sampleWAV))
	if err != nil {
		t.Fatalf("Save WAV: %v", err)
	}
	if audioRec.MediaKind != storage.MediaKindAudio {
		t.Fatalf("expected audio media_kind, got %q", audioRec.MediaKind)
	}

	runCtx := tools.RunContext{RunID: "run_2", OwnerID: "bob"}

	// audio_transcribe
	sttArgs, _ := json.Marshal(map[string]any{"file_id": audioRec.ID, "language": "ru"})
	sttOut, err := reg.Execute(ctx, runCtx, "audio_transcribe", sttArgs)
	if err != nil || !strings.Contains(sttOut.Text, "тестовая расшифровка") {
		t.Fatalf("audio_transcribe failed: err=%v out=%+v", err, sttOut)
	}

	// audio_tts
	ttsArgs, _ := json.Marshal(map[string]any{"text": "Hello world", "format": "wav"})
	ttsOut, err := reg.Execute(ctx, runCtx, "audio_tts", ttsArgs)
	if err != nil || len(ttsOut.GeneratedFiles) != 1 || ttsOut.GeneratedFiles[0].MediaKind != storage.MediaKindAudio {
		t.Fatalf("audio_tts failed: err=%v out=%+v", err, ttsOut)
	}

	// image_generate
	imgArgs, _ := json.Marshal(map[string]any{"prompt": "A green square"})
	imgOut, err := reg.Execute(ctx, runCtx, "image_generate", imgArgs)
	if err != nil || len(imgOut.GeneratedFiles) != 1 || imgOut.GeneratedFiles[0].MediaKind != storage.MediaKindImage {
		t.Fatalf("image_generate failed: err=%v out=%+v", err, imgOut)
	}
}
