package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"cli-ai-gateway/internal/executor"
)

type inlineMediaBlob struct {
	Filename string
	Purpose  string // "vision" | "audio"
	Data     []byte
}

// parseOpenAITools parses both Chat Completions format:
//   {"type": "function", "function": {"name": "...", "description": "...", "parameters": {...}}}
// and Responses API format:
//   {"type": "function", "name": "...", "description": "...", "parameters": {...}}
func parseOpenAITools(raw json.RawMessage) ([]executor.ToolDefinition, string, string) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, "invalid_parameter", "Parameter 'tools' must be an array of tool definitions"
	}
	defs := make([]executor.ToolDefinition, 0, len(items))
	for _, it := range items {
		var tType string
		if rawType, ok := it["type"]; ok {
			_ = json.Unmarshal(rawType, &tType)
		}
		if tType != "function" {
			return nil, "unsupported_parameter", fmt.Sprintf("Unsupported tool type %q (only 'function' is supported)", tType)
		}

		// Nested Chat Completions format: {"type":"function","function":{...}}
		var def executor.ToolDefinition
		if fnRaw, ok := it["function"]; ok && !isJSONNull(fnRaw) {
			if err := json.Unmarshal(fnRaw, &def); err != nil {
				return nil, "invalid_parameter", "Tool 'function' must be an object with 'name'"
			}
		} else {
			// Flat Responses API format: {"type":"function","name":"...","parameters":{...}}
			if nRaw, ok := it["name"]; ok {
				_ = json.Unmarshal(nRaw, &def.Name)
			}
			if dRaw, ok := it["description"]; ok {
				_ = json.Unmarshal(dRaw, &def.Description)
			}
			if pRaw, ok := it["parameters"]; ok && !isJSONNull(pRaw) {
				_ = json.Unmarshal(pRaw, &def.Parameters)
			}
		}
		def.Name = strings.TrimSpace(def.Name)
		if def.Name == "" {
			return nil, "invalid_parameter", "Each function tool requires a non-empty 'name'"
		}
		defs = append(defs, def)
	}
	return defs, "", ""
}

// parseDataURIImage decodes "data:image/<fmt>;base64,<payload>" into an inlineMediaBlob.
func parseDataURIImage(rawURL string, idx int) (inlineMediaBlob, error) {
	u := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(u, "data:image/") {
		return inlineMediaBlob{}, fmt.Errorf("image_url must be a base64 data URI (data:image/...;base64,...) or file_id")
	}
	comma := strings.IndexByte(u, ',')
	if comma < 0 {
		return inlineMediaBlob{}, fmt.Errorf("malformed image data URI")
	}
	header := u[:comma]
	if !strings.Contains(header, ";base64") {
		return inlineMediaBlob{}, fmt.Errorf("image data URI must be base64-encoded")
	}
	mime := strings.TrimPrefix(strings.Split(header, ";")[0], "data:")
	ext := "png"
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		ext = "jpg"
	case "image/webp":
		ext = "webp"
	case "image/gif":
		ext = "gif"
	case "image/png":
		ext = "png"
	default:
		return inlineMediaBlob{}, fmt.Errorf("unsupported image mime type %q", mime)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(u[comma+1:]))
	if err != nil {
		return inlineMediaBlob{}, fmt.Errorf("invalid base64 image data: %w", err)
	}
	return inlineMediaBlob{
		Filename: fmt.Sprintf("inline_image_%d.%s", idx+1, ext),
		Purpose:  "vision",
		Data:     decoded,
	}, nil
}

// parseBase64Audio decodes OpenAI input_audio {data, format} into an inlineMediaBlob.
func parseBase64Audio(b64Data, format string, idx int) (inlineMediaBlob, error) {
	b64Data = strings.TrimSpace(b64Data)
	if b64Data == "" {
		return inlineMediaBlob{}, fmt.Errorf("input_audio requires non-empty base64 'data'")
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "wav"
	}
	ext := format
	switch format {
	case "mp3", "mpeg":
		ext = "mp3"
	case "wav":
		ext = "wav"
	case "ogg", "opus":
		ext = "ogg"
	case "m4a", "mp4":
		ext = "m4a"
	case "flac":
		ext = "flac"
	case "webm":
		ext = "webm"
	default:
		return inlineMediaBlob{}, fmt.Errorf("unsupported audio format %q", format)
	}
	decoded, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return inlineMediaBlob{}, fmt.Errorf("invalid base64 audio data: %w", err)
	}
	return inlineMediaBlob{
		Filename: fmt.Sprintf("inline_audio_%d.%s", idx+1, ext),
		Purpose:  "audio",
		Data:     decoded,
	}, nil
}
