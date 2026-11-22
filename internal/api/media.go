package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"cli-ai-gateway/internal/storage"
	"cli-ai-gateway/internal/tools"
)

// MediaHandler implements OpenAI-compatible audio & image facades backed by internal/tools.Registry:
//   - POST /v1/audio/transcriptions
//   - POST /v1/audio/speech
//   - POST /v1/images/generations
type MediaHandler struct {
	files *storage.FileStore
	tools *tools.Registry
}

func NewMediaHandler(files *storage.FileStore, toolReg *tools.Registry) *MediaHandler {
	return &MediaHandler{files: files, tools: toolReg}
}

func (h *MediaHandler) AudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	if h.tools == nil || h.files == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "tools_unavailable", "Audio tools are not configured", nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, storage.MaxAudioFileBytes+2*1024*1024)
	mr, err := r.MultipartReader()
	if err != nil {
		writeInvalidRequest(w, "invalid_multipart", "Content-Type must be multipart/form-data", "")
		return
	}

	var (
		uploaded storage.FileRecord
		hasFile  bool
		language string
		prompt   string
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if hasFile {
				_ = h.files.Delete(r.Context(), principal.OwnerID, uploaded.ID)
			}
			writeInvalidRequest(w, "invalid_multipart", "Malformed multipart body", "")
			return
		}
		switch part.FormName() {
		case "file":
			rawName := rawMultipartFilename(part)
			if rawName == "" {
				rawName = "audio.ogg"
			}
			rec, saveErr := h.files.Save(r.Context(), principal.OwnerID, rawName, "audio", part)
			_ = part.Close()
			if saveErr != nil {
				mapStorageError(w, saveErr)
				return
			}
			uploaded = rec
			hasFile = true
		case "language":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			_ = part.Close()
			language = strings.TrimSpace(string(b))
		case "prompt":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			_ = part.Close()
			prompt = strings.TrimSpace(string(b))
		default:
			_ = part.Close()
		}
	}
	if !hasFile {
		writeInvalidRequest(w, "missing_file", "Multipart field 'file' is required", "file")
		return
	}
	defer func() {
		_ = h.files.Delete(r.Context(), principal.OwnerID, uploaded.ID)
	}()

	out, err := h.tools.TranscribeAudioFile(r.Context(), principal.OwnerID, uploaded.ID, language, prompt)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "transcription_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text": out.Text,
	})
}

func (h *MediaHandler) AudioSpeech(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	if h.tools == nil || h.files == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "tools_unavailable", "Audio tools are not configured", nil)
		return
	}

	var req struct {
		Model          string  `json:"model"`
		Input          string  `json:"input"`
		Voice          string  `json:"voice"`
		ResponseFormat string  `json:"response_format"`
		Speed          float64 `json:"speed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1*1024*1024)).Decode(&req); err != nil {
		writeInvalidRequest(w, "invalid_json", "Request body must be valid JSON", "")
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeInvalidRequest(w, "missing_required_parameter", "Parameter 'input' is required", "input")
		return
	}
	format := req.ResponseFormat
	if format == "opus" {
		format = "ogg"
	}
	out, err := h.tools.SynthesizeSpeech(r.Context(), principal.OwnerID, req.Input, req.Voice, format, req.Speed)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "tts_failed", err.Error(), nil)
		return
	}
	if len(out.GeneratedFiles) == 0 {
		writeError(w, http.StatusInternalServerError, "server_error", "tts_empty", "TTS produced no audio file", nil)
		return
	}
	ref := out.GeneratedFiles[0]
	rec, data, err := h.files.ReadBytes(r.Context(), principal.OwnerID, ref.FileID, storage.MaxAudioFileBytes)
	if err != nil {
		mapStorageError(w, err)
		return
	}
	ct := rec.ContentType
	if ct == "" {
		ct = "audio/mpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Gateway-File-Id", rec.ID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *MediaHandler) ImageGenerations(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	if h.tools == nil || h.files == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "tools_unavailable", "Image tools are not configured", nil)
		return
	}

	var req struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		Size           string `json:"size"`
		ResponseFormat string `json:"response_format"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1*1024*1024)).Decode(&req); err != nil {
		writeInvalidRequest(w, "invalid_json", "Request body must be valid JSON", "")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeInvalidRequest(w, "missing_required_parameter", "Parameter 'prompt' is required", "prompt")
		return
	}

	out, err := h.tools.GenerateImage(r.Context(), principal.OwnerID, req.Prompt, req.Size)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "image_generation_failed", err.Error(), nil)
		return
	}
	if len(out.GeneratedFiles) == 0 {
		writeError(w, http.StatusInternalServerError, "server_error", "image_empty", "Image generator produced no file", nil)
		return
	}
	ref := out.GeneratedFiles[0]
	item := map[string]any{
		"file_id": ref.FileID,
		"url":     ref.URL,
	}
	if req.ResponseFormat == "" || req.ResponseFormat == "b64_json" {
		if _, data, readErr := h.files.ReadBytes(r.Context(), principal.OwnerID, ref.FileID, storage.MaxImageFileBytes); readErr == nil {
			item["b64_json"] = base64.StdEncoding.EncodeToString(data)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    []map[string]any{item},
	})
}
