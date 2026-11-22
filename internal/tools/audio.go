package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/storage"
)

const (
	defaultAudioChunkSec   = 1500.0 // 25 minutes
	defaultAudioOverlapSec = 10.0   // 10 seconds overlap
)

type audioInfoTool struct{ reg *Registry }

func (t *audioInfoTool) Name() string     { return "audio_info" }
func (t *audioInfoTool) Category() string { return "audio" }
func (t *audioInfoTool) Description() string {
	return "Inspect audio file metadata (duration, codec, sample rate, channels, bitrate) using ffprobe."
}
func (t *audioInfoTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id": map[string]any{"type": "string", "description": "ID of the uploaded audio file"},
		},
		"required": []string{"file_id"},
	}
}
func (t *audioInfoTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	rec, path, err := t.reg.resolveOwnedAudioPath(ctx, runCtx.OwnerID, args.FileID)
	if err != nil {
		return ToolOutput{}, err
	}
	info, err := probeAudioFile(ctx, path)
	if err != nil {
		return ToolOutput{}, err
	}
	info["file_id"] = rec.ID
	info["filename"] = rec.Filename
	info["bytes"] = rec.Bytes
	info["content_type"] = rec.ContentType
	raw, _ := json.MarshalIndent(info, "", "  ")
	return ToolOutput{Text: string(raw), Data: info}, nil
}

type audioConvertTrimTool struct{ reg *Registry }

func (t *audioConvertTrimTool) Name() string     { return "audio_convert_trim" }
func (t *audioConvertTrimTool) Category() string { return "audio" }
func (t *audioConvertTrimTool) Description() string {
	return "Convert audio format (mp3, wav, ogg, m4a, flac), trim by start/end seconds, and optionally normalize loudness using ffmpeg."
}
func (t *audioConvertTrimTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id":   map[string]any{"type": "string", "description": "ID of the input audio file"},
			"format":    map[string]any{"type": "string", "enum": []string{"mp3", "wav", "ogg", "m4a", "flac"}, "description": "Target audio format"},
			"start_sec": map[string]any{"type": "number", "description": "Optional start timestamp in seconds"},
			"end_sec":   map[string]any{"type": "number", "description": "Optional end timestamp in seconds"},
			"normalize": map[string]any{"type": "boolean", "description": "Apply loudnorm loudness normalization"},
		},
		"required": []string{"file_id", "format"},
	}
}
func (t *audioConvertTrimTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID    string   `json:"file_id"`
		Format    string   `json:"format"`
		StartSec  *float64 `json:"start_sec"`
		EndSec    *float64 `json:"end_sec"`
		Normalize bool     `json:"normalize"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	format := strings.ToLower(strings.TrimSpace(args.Format))
	switch format {
	case "mp3", "wav", "ogg", "m4a", "flac":
	default:
		return ToolOutput{}, fmt.Errorf("unsupported target format %q", args.Format)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return ToolOutput{}, errors.New("ffmpeg is not installed in PATH")
	}
	rec, srcPath, err := t.reg.resolveOwnedAudioPath(ctx, runCtx.OwnerID, args.FileID)
	if err != nil {
		return ToolOutput{}, err
	}

	tmpDir, err := os.MkdirTemp("", "agy-audio-conv-*")
	if err != nil {
		return ToolOutput{}, err
	}
	defer os.RemoveAll(tmpDir)

	base := strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename))
	if base == "" {
		base = "audio"
	}
	outName := base + "." + format
	outPath := filepath.Join(tmpDir, "out."+format)

	ffArgs := []string{"-y", "-hide_banner", "-loglevel", "error"}
	if args.StartSec != nil && *args.StartSec > 0 {
		ffArgs = append(ffArgs, "-ss", fmt.Sprintf("%.3f", *args.StartSec))
	}
	ffArgs = append(ffArgs, "-i", srcPath)
	if args.EndSec != nil && *args.EndSec > 0 {
		if args.StartSec != nil && *args.StartSec > 0 && *args.EndSec > *args.StartSec {
			ffArgs = append(ffArgs, "-t", fmt.Sprintf("%.3f", *args.EndSec-*args.StartSec))
		} else {
			ffArgs = append(ffArgs, "-to", fmt.Sprintf("%.3f", *args.EndSec))
		}
	}
	ffArgs = append(ffArgs, "-vn")
	if args.Normalize {
		ffArgs = append(ffArgs, "-af", "loudnorm")
	}
	ffArgs = append(ffArgs, outPath)

	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "ffmpeg", ffArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ToolOutput{}, fmt.Errorf("ffmpeg failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return ToolOutput{}, err
	}
	saved, err := t.reg.files.SaveGenerated(ctx, runCtx.OwnerID, outName, data)
	if err != nil {
		return ToolOutput{}, err
	}
	ref := fileRecordToRef(saved)
	return ToolOutput{
		Text:           fmt.Sprintf("Converted audio saved as %q (file_id: %s, %d bytes, URL: %s)", saved.Filename, saved.ID, saved.Bytes, ref.URL),
		Data:           map[string]any{"file_id": saved.ID, "filename": saved.Filename, "bytes": saved.Bytes, "url": ref.URL},
		GeneratedFiles: []executor.GeneratedFileRef{ref},
	}, nil
}

type audioTranscribeTool struct{ reg *Registry }

func (t *audioTranscribeTool) Name() string     { return "audio_transcribe" }
func (t *audioTranscribeTool) Category() string { return "audio" }
func (t *audioTranscribeTool) Description() string {
	return "Transcribe an audio file to text (automatically chunks long recordings >25 min via ffmpeg with 10s overlap). Uses Whisper backend or an audio-capable pool model."
}
func (t *audioTranscribeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id":  map[string]any{"type": "string", "description": "ID of the uploaded audio file"},
			"language": map[string]any{"type": "string", "description": "Optional ISO-639-1 language hint (e.g. ru, en)"},
			"prompt":   map[string]any{"type": "string", "description": "Optional context prompt or vocabulary hint"},
		},
		"required": []string{"file_id"},
	}
}
func (t *audioTranscribeTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID   string `json:"file_id"`
		Language string `json:"language"`
		Prompt   string `json:"prompt"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	return t.reg.TranscribeAudioFile(ctx, runCtx.OwnerID, args.FileID, args.Language, args.Prompt)
}

// TranscribeAudioFile executes STT (callable both as a model tool and via POST /v1/audio/transcriptions).
func (r *Registry) TranscribeAudioFile(ctx context.Context, ownerID, fileID, language, prompt string) (ToolOutput, error) {
	rec, srcPath, err := r.resolveOwnedAudioPath(ctx, ownerID, fileID)
	if err != nil {
		return ToolOutput{}, err
	}

	backend, hasBackend := r.getBackend(ctx, "whisper")
	useWhisper := hasBackend && backend.Enabled && strings.TrimSpace(backend.BaseURL) != ""

	if !useWhisper {
		invoker := r.getInvoker()
		if invoker == nil {
			return ToolOutput{}, errors.New("audio transcription unavailable: configure whisper backend URL or an audio_in model in the pool")
		}
		q := "Transcribe this audio accurately."
		if language != "" {
			q += fmt.Sprintf(" Language: %s.", language)
		}
		if prompt != "" {
			q += " Context: " + prompt
		}
		text, err := invoker(ctx, ownerID, "audio_in", q, []string{rec.ID})
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{
			Text: text,
			Data: map[string]any{"file_id": rec.ID, "text": text, "backend": "pool_model"},
		}, nil
	}

	chunkPaths := []string{srcPath}
	var cleanupDir string
	if info, err := probeAudioFile(ctx, srcPath); err == nil {
		if dur, ok := info["duration_sec"].(float64); ok && dur > defaultAudioChunkSec {
			if _, ffErr := exec.LookPath("ffmpeg"); ffErr == nil {
				if dir, chunks, splitErr := splitAudioChunks(ctx, srcPath, dur, defaultAudioChunkSec, defaultAudioOverlapSec); splitErr == nil && len(chunks) > 0 {
					cleanupDir = dir
					chunkPaths = chunks
				}
			}
		}
	}
	if cleanupDir != "" {
		defer os.RemoveAll(cleanupDir)
	}

	var parts []string
	for _, cp := range chunkPaths {
		txt, err := r.callWhisperHTTP(ctx, backend, cp, rec.Filename, language, prompt)
		if err != nil {
			return ToolOutput{}, err
		}
		parts = append(parts, strings.TrimSpace(txt))
	}
	fullText := strings.Join(parts, "\n\n")
	return ToolOutput{
		Text: fullText,
		Data: map[string]any{"file_id": rec.ID, "text": fullText, "chunks": len(chunkPaths), "backend": "whisper"},
	}, nil
}

type audioTTSTool struct{ reg *Registry }

func (t *audioTTSTool) Name() string     { return "audio_tts" }
func (t *audioTTSTool) Category() string { return "audio" }
func (t *audioTTSTool) Description() string {
	return "Synthesize speech audio from text (TTS) via the configured OpenAI-compatible TTS backend and save it as a downloadable audio file."
}
func (t *audioTTSTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text":   map[string]any{"type": "string", "description": "Text to speak"},
			"voice":  map[string]any{"type": "string", "description": "Voice identifier (default alloy)"},
			"format": map[string]any{"type": "string", "enum": []string{"mp3", "wav", "ogg", "flac"}, "description": "Output audio format (default mp3)"},
			"speed":  map[string]any{"type": "number", "description": "Speech speed multiplier (0.25 to 4.0, default 1.0)"},
		},
		"required": []string{"text"},
	}
}
func (t *audioTTSTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		Text   string  `json:"text"`
		Voice  string  `json:"voice"`
		Format string  `json:"format"`
		Speed  float64 `json:"speed"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	return t.reg.SynthesizeSpeech(ctx, runCtx.OwnerID, args.Text, args.Voice, args.Format, args.Speed)
}

// SynthesizeSpeech calls the configured OpenAI-compatible /v1/audio/speech backend and persists the audio.
func (r *Registry) SynthesizeSpeech(ctx context.Context, ownerID, text, voice, format string, speed float64) (ToolOutput, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return ToolOutput{}, errors.New("text is required")
	}
	backend, ok := r.getBackend(ctx, "tts")
	if !ok || !backend.Enabled || strings.TrimSpace(backend.BaseURL) == "" {
		return ToolOutput{}, errors.New("TTS unavailable: configure tts backend URL in Admin -> Tools or GATEWAY_TTS_URL")
	}
	if voice == "" {
		voice = "alloy"
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "mp3"
	}
	responseFormat := format
	if format == "ogg" {
		responseFormat = "opus"
	}
	if speed <= 0 {
		speed = 1.0
	}
	model := backend.Model
	if model == "" {
		model = "tts-1"
	}

	bodyBytes, _ := json.Marshal(map[string]any{
		"model":           model,
		"input":           text,
		"voice":           voice,
		"response_format": responseFormat,
		"speed":           speed,
	})

	endpoint := strings.TrimRight(backend.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/audio/speech") {
		endpoint += "/v1/audio/speech"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return ToolOutput{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("tts request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ToolOutput{}, fmt.Errorf("tts backend HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	audioBytes, err := io.ReadAll(io.LimitReader(resp.Body, storage.MaxAudioFileBytes+1))
	if err != nil {
		return ToolOutput{}, err
	}
	if int64(len(audioBytes)) > storage.MaxAudioFileBytes {
		return ToolOutput{}, errors.New("tts output exceeds max audio size")
	}
	if r.files == nil {
		return ToolOutput{}, errors.New("file store unavailable")
	}

	filename := fmt.Sprintf("speech_%d.%s", time.Now().Unix(), format)
	saved, err := r.files.SaveGenerated(ctx, ownerID, filename, audioBytes)
	if err != nil {
		return ToolOutput{}, err
	}
	ref := fileRecordToRef(saved)
	return ToolOutput{
		Text:           fmt.Sprintf("Synthesized speech saved as %q (file_id: %s, %d bytes, URL: %s)", saved.Filename, saved.ID, saved.Bytes, ref.URL),
		Data:           map[string]any{"file_id": saved.ID, "filename": saved.Filename, "bytes": saved.Bytes, "url": ref.URL},
		GeneratedFiles: []executor.GeneratedFileRef{ref},
	}, nil
}

func (r *Registry) resolveOwnedAudioPath(ctx context.Context, ownerID, fileID string) (storage.FileRecord, string, error) {
	if r.files == nil {
		return storage.FileRecord{}, "", errors.New("file store unavailable")
	}
	rec, err := r.files.Get(ctx, ownerID, strings.TrimSpace(fileID))
	if err != nil {
		return storage.FileRecord{}, "", err
	}
	if rec.MediaKind != storage.MediaKindAudio {
		return rec, "", fmt.Errorf("file %s is not an audio file (media_kind=%q)", rec.ID, rec.MediaKind)
	}
	p, err := r.files.PathForRecord(rec)
	if err != nil {
		return rec, "", err
	}
	return rec, p, nil
}

func probeAudioFile(ctx context.Context, path string) (map[string]any, error) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil, errors.New("ffprobe is not installed in PATH")
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w", err)
	}
	var parsed struct {
		Format struct {
			Duration   string `json:"duration"`
			BitRate    string `json:"bit_rate"`
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	res := map[string]any{"format": parsed.Format.FormatName}
	if d, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil {
		res["duration_sec"] = d
	}
	if br, err := strconv.ParseInt(parsed.Format.BitRate, 10, 64); err == nil {
		res["bit_rate"] = br
	}
	for _, st := range parsed.Streams {
		if st.CodecType == "audio" {
			res["codec"] = st.CodecName
			res["channels"] = st.Channels
			if sr, err := strconv.Atoi(st.SampleRate); err == nil {
				res["sample_rate"] = sr
			}
			break
		}
	}
	return res, nil
}

func splitAudioChunks(ctx context.Context, srcPath string, totalDur, chunkSec, overlapSec float64) (string, []string, error) {
	tmpDir, err := os.MkdirTemp("", "agy-audio-chunks-*")
	if err != nil {
		return "", nil, err
	}
	var chunks []string
	idx := 0
	for start := 0.0; start < totalDur; start += chunkSec - overlapSec {
		outPath := filepath.Join(tmpDir, fmt.Sprintf("chunk_%03d.mp3", idx))
		cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cmd := exec.CommandContext(cmdCtx, "ffmpeg",
			"-y", "-hide_banner", "-loglevel", "error",
			"-ss", fmt.Sprintf("%.3f", start),
			"-t", fmt.Sprintf("%.3f", chunkSec),
			"-i", srcPath,
			"-vn", "-ac", "1", "-ar", "16000", "-b:a", "64k",
			outPath,
		)
		err := cmd.Run()
		cancel()
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", nil, err
		}
		chunks = append(chunks, outPath)
		idx++
		if start+chunkSec >= totalDur {
			break
		}
	}
	return tmpDir, chunks, nil
}

func (r *Registry) callWhisperHTTP(ctx context.Context, backend BackendConfig, filePath, displayFilename, language, prompt string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if displayFilename == "" {
		displayFilename = filepath.Base(filePath)
	}
	part, err := mw.CreateFormFile("file", displayFilename)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	model := backend.Model
	if model == "" {
		model = "whisper-1"
	}
	_ = mw.WriteField("model", model)
	if language != "" {
		_ = mw.WriteField("language", language)
	}
	if prompt != "" {
		_ = mw.WriteField("prompt", prompt)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	endpoint := strings.TrimRight(backend.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/audio/transcriptions") {
		endpoint += "/v1/audio/transcriptions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper backend HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Text != "" {
		return parsed.Text, nil
	}
	return strings.TrimSpace(string(raw)), nil
}
