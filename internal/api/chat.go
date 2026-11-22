package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/streaming"
)

// ChatHandler implements POST /v1/chat/completions (Section 6.7, ADR-004).
type ChatHandler struct {
	runner *Runner
}

func NewChatHandler(runner *Runner) *ChatHandler {
	return &ChatHandler{runner: runner}
}

type chatMessage struct {
	Role       string
	Content    string
	ToolCallID string
	ToolCalls  []executor.ToolCall
}

type parsedChatReq struct {
	Model       string
	Stream      bool
	Messages    []chatMessage
	Tools       []executor.ToolDefinition
	ToolChoice  any
	FileIDs     []string
	InlineMedia []inlineMediaBlob
}

func (h *ChatHandler) Create(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())

	req, errParam, errCode, errMsg := parseChatRequest(r.Body)
	if errCode != "" {
		writeInvalidRequest(w, errCode, errMsg, errParam)
		if audit := AuditFromContext(r.Context()); audit != nil {
			audit.SetErrorType(errCode)
		}
		return
	}

	// Stage any inline base64 images/audio into FileStore and clean them up after the request completes.
	allFileIDs := append([]string(nil), req.FileIDs...)
	var tempInlineIDs []string
	for _, blob := range req.InlineMedia {
		rec, err := h.runner.StageInlineMedia(r.Context(), principal.OwnerID, blob.Filename, blob.Purpose, blob.Data)
		if err != nil {
			for _, tid := range tempInlineIDs {
				_ = h.runner.files.Delete(context.Background(), principal.OwnerID, tid)
			}
			mapStorageError(w, err)
			return
		}
		tempInlineIDs = append(tempInlineIDs, rec.ID)
		allFileIDs = append(allFileIDs, rec.ID)
	}
	if len(tempInlineIDs) > 0 {
		defer func() {
			for _, tid := range tempInlineIDs {
				_ = h.runner.files.Delete(context.Background(), principal.OwnerID, tid)
			}
		}()
	}

	var sysParts []string
	var docBuilder strings.Builder
	docBuilder.WriteString("# Conversation Messages\n\n")
	var lastUser string
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			sysParts = append(sysParts, m.Content)
		}
		docBuilder.WriteString("## ")
		docBuilder.WriteString(strings.ToUpper(m.Role))
		if m.ToolCallID != "" {
			docBuilder.WriteString(" (tool_call_id=")
			docBuilder.WriteString(m.ToolCallID)
			docBuilder.WriteString(")")
		}
		docBuilder.WriteString("\n\n")
		if m.Content != "" {
			docBuilder.WriteString(m.Content)
			docBuilder.WriteString("\n\n")
		}
		for _, tc := range m.ToolCalls {
			docBuilder.WriteString(fmt.Sprintf("Tool call [%s]: %s(%s)\n\n", tc.ID, tc.Name, tc.Arguments))
		}
		if m.Role == "user" {
			lastUser = m.Content
		}
	}

	instructions := strings.Join(sysParts, "\n\n")
	var sse *streaming.ChatCompletionsSSE

	out, ok, runErr := h.runner.Execute(r.Context(), w, PipelineInput{
		Principal:      principal,
		ModelID:        req.Model,
		Instructions:   instructions,
		FileIDs:        dedupeStrings(allFileIDs),
		InlinePromptMD: docBuilder.String(),
		HistoryEntry:   lastUser,
		Tools:          req.Tools,
		ToolChoice:     req.ToolChoice,
		Stream:         req.Stream,
		OnStreamReady: func(run session.RunRecord, _ *string) error {
			sse = streaming.NewChatCompletionsSSE(w, run.Model, run.CreatedAt)
			return sse.Start()
		},
		OnDelta: func(delta string) error {
			if sse != nil {
				return sse.Delta(delta)
			}
			return nil
		},
	})
	if !ok {
		return
	}

	if req.Stream {
		if runErr != nil {
			_, errType, code, msg := mapExecutorHTTPStatus(runErr)
			if sse != nil && sse.Started() {
				sse.Fail(errType, code, msg)
			} else {
				status, _, _, _ := mapExecutorHTTPStatus(runErr)
				writeError(w, status, errType, code, msg, nil)
			}
			return
		}
		if sse != nil {
			_ = sse.CompleteWithTools(out.Result.ToolCalls, out.Result.Usage)
		}
		return
	}

	finishReason := "stop"
	msgObj := map[string]any{
		"role":    "assistant",
		"content": out.Result.Answer,
	}
	if len(out.Result.ToolCalls) > 0 {
		finishReason = "tool_calls"
		tcArr := make([]map[string]any, 0, len(out.Result.ToolCalls))
		for _, tc := range out.Result.ToolCalls {
			tcArr = append(tcArr, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			})
		}
		msgObj["tool_calls"] = tcArr
		if out.Result.Answer == "" {
			msgObj["content"] = nil
		}
	}

	respBody := map[string]any{
		"id":      randomChatID(),
		"object":  "chat.completion",
		"created": out.Run.CreatedAt,
		"model":   out.Run.Model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msgObj,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     out.Result.Usage.InputTokens,
			"completion_tokens": out.Result.Usage.OutputTokens,
			"total_tokens":      out.Result.Usage.TotalTokens,
		},
	}
	if len(out.Result.GeneratedFiles) > 0 {
		respBody["generated_files"] = out.Result.GeneratedFiles
	}
	writeJSON(w, http.StatusOK, respBody)
}

func parseChatRequest(body io.Reader) (parsedChatReq, string, string, string) {
	raw, err := io.ReadAll(io.LimitReader(body, 10*1024*1024+1))
	if err != nil {
		return parsedChatReq{}, "", "invalid_json", "Failed to read request body"
	}
	if len(raw) > 10*1024*1024 {
		return parsedChatReq{}, "", "request_too_large", "JSON request body exceeds 10 MiB limit"
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return parsedChatReq{}, "", "invalid_json", "Request body must be a valid JSON object"
	}

	allowed := map[string]bool{
		"model":               true,
		"messages":            true,
		"stream":              true,
		"tools":               true,
		"tool_choice":         true,
		"parallel_tool_calls": true,
	}
	for k := range fields {
		if !allowed[k] {
			return parsedChatReq{}, k, "unsupported_parameter", fmt.Sprintf("Unsupported parameter %q", k)
		}
	}

	var out parsedChatReq
	if rawModel, ok := fields["model"]; !ok || isJSONNull(rawModel) {
		return out, "model", "missing_required_parameter", "Parameter 'model' is required"
	} else if err := json.Unmarshal(rawModel, &out.Model); err != nil || strings.TrimSpace(out.Model) == "" {
		return out, "model", "invalid_parameter", "Parameter 'model' must be a non-empty string"
	}

	if rawStream, ok := fields["stream"]; ok && !isJSONNull(rawStream) {
		if err := json.Unmarshal(rawStream, &out.Stream); err != nil {
			return out, "stream", "invalid_parameter", "Parameter 'stream' must be a boolean"
		}
	}

	if rawTools, ok := fields["tools"]; ok && !isJSONNull(rawTools) {
		defs, tc, tm := parseOpenAITools(rawTools)
		if tc != "" {
			return out, "tools", tc, tm
		}
		out.Tools = defs
	}
	if rawChoice, ok := fields["tool_choice"]; ok && !isJSONNull(rawChoice) {
		var choice any
		if err := json.Unmarshal(rawChoice, &choice); err != nil {
			return out, "tool_choice", "invalid_parameter", "Parameter 'tool_choice' must be valid JSON"
		}
		out.ToolChoice = choice
	}

	rawMsgs, ok := fields["messages"]
	if !ok || isJSONNull(rawMsgs) {
		return out, "messages", "missing_required_parameter", "Parameter 'messages' is required"
	}

	var rawMsgList []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &rawMsgList); err != nil || len(rawMsgList) == 0 {
		return out, "messages", "invalid_parameter", "Parameter 'messages' must be a non-empty array"
	}

	for _, item := range rawMsgList {
		var role string
		if rRaw, ok := item["role"]; !ok || json.Unmarshal(rRaw, &role) != nil || strings.TrimSpace(role) == "" {
			return out, "messages", "invalid_parameter", "Each message requires a non-empty 'role'"
		}
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return out, "messages", "invalid_parameter", fmt.Sprintf("Unsupported message role %q", role)
		}

		msg := chatMessage{Role: role}
		if tcidRaw, ok := item["tool_call_id"]; ok && !isJSONNull(tcidRaw) {
			_ = json.Unmarshal(tcidRaw, &msg.ToolCallID)
		}
		if tcsRaw, ok := item["tool_calls"]; ok && !isJSONNull(tcsRaw) {
			var rawTCs []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			if err := json.Unmarshal(tcsRaw, &rawTCs); err == nil {
				for _, tc := range rawTCs {
					msg.ToolCalls = append(msg.ToolCalls, executor.ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					})
				}
			}
		}

		cRaw, hasContent := item["content"]
		if !hasContent || isJSONNull(cRaw) {
			if role == "assistant" && len(msg.ToolCalls) > 0 {
				out.Messages = append(out.Messages, msg)
				continue
			}
			return out, "messages", "invalid_parameter", "Each message requires 'content'"
		}

		var contentText string
		if err := json.Unmarshal(cRaw, &contentText); err != nil {
			var parts []map[string]json.RawMessage
			if err2 := json.Unmarshal(cRaw, &parts); err2 != nil {
				return out, "messages", "invalid_parameter", "Message 'content' must be a string or array of content parts"
			}
			var sb strings.Builder
			for _, p := range parts {
				var pType string
				if tRaw, ok := p["type"]; ok {
					_ = json.Unmarshal(tRaw, &pType)
				}
				switch pType {
				case "text", "input_text":
					var txt string
					if txtRaw, ok := p["text"]; !ok || json.Unmarshal(txtRaw, &txt) != nil {
						return out, "messages", "invalid_parameter", "Text part requires string field 'text'"
					}
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(txt)
				case "image_url", "input_image":
					var imgURL, fid string
					if uRaw, ok := p["image_url"]; ok && !isJSONNull(uRaw) {
						if err := json.Unmarshal(uRaw, &imgURL); err != nil {
							var obj struct {
								URL    string `json:"url"`
								FileID string `json:"file_id"`
							}
							if err2 := json.Unmarshal(uRaw, &obj); err2 == nil {
								imgURL = obj.URL
								fid = obj.FileID
							}
						}
					}
					if fRaw, ok := p["file_id"]; ok && !isJSONNull(fRaw) {
						_ = json.Unmarshal(fRaw, &fid)
					}
					if fid != "" {
						out.FileIDs = append(out.FileIDs, fid)
					} else if imgURL != "" {
						blob, bErr := parseDataURIImage(imgURL, len(out.InlineMedia))
						if bErr != nil {
							return out, "messages", "invalid_parameter", bErr.Error()
						}
						out.InlineMedia = append(out.InlineMedia, blob)
					} else {
						return out, "messages", "invalid_parameter", "image_url part requires 'image_url.url' or 'file_id'"
					}
				case "input_audio":
					var fid string
					if fRaw, ok := p["file_id"]; ok && !isJSONNull(fRaw) {
						_ = json.Unmarshal(fRaw, &fid)
					}
					if fid != "" {
						out.FileIDs = append(out.FileIDs, fid)
					} else if aRaw, ok := p["input_audio"]; ok && !isJSONNull(aRaw) {
						var aObj struct {
							Data   string `json:"data"`
							Format string `json:"format"`
							FileID string `json:"file_id"`
						}
						if err := json.Unmarshal(aRaw, &aObj); err != nil {
							return out, "messages", "invalid_parameter", "input_audio must be an object with 'data' and 'format'"
						}
						if aObj.FileID != "" {
							out.FileIDs = append(out.FileIDs, aObj.FileID)
						} else {
							blob, bErr := parseBase64Audio(aObj.Data, aObj.Format, len(out.InlineMedia))
							if bErr != nil {
								return out, "messages", "invalid_parameter", bErr.Error()
							}
							out.InlineMedia = append(out.InlineMedia, blob)
						}
					} else {
						return out, "messages", "invalid_parameter", "input_audio part requires 'input_audio' object or 'file_id'"
					}
				case "file", "input_file":
					var fid string
					if fRaw, ok := p["file_id"]; ok && !isJSONNull(fRaw) {
						_ = json.Unmarshal(fRaw, &fid)
					}
					if fObjRaw, ok := p["file"]; ok && !isJSONNull(fObjRaw) {
						var fObj struct {
							FileID string `json:"file_id"`
						}
						if json.Unmarshal(fObjRaw, &fObj) == nil && fObj.FileID != "" {
							fid = fObj.FileID
						}
					}
					if strings.TrimSpace(fid) == "" {
						return out, "messages", "invalid_parameter", "file part requires non-empty 'file_id'"
					}
					out.FileIDs = append(out.FileIDs, fid)
				default:
					return out, "messages", "unsupported_input_type", fmt.Sprintf("Unsupported message content type %q in chat completions", pType)
				}
			}
			contentText = sb.String()
		}
		msg.Content = contentText
		out.Messages = append(out.Messages, msg)
	}
	return out, "", "", ""
}

func randomChatID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}
