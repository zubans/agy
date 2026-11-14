package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/streaming"
)

// ChatHandler implements POST /v1/chat/completions (Section 6.7).
type ChatHandler struct {
	runner *Runner
}

func NewChatHandler(runner *Runner) *ChatHandler {
	return &ChatHandler{runner: runner}
}

type chatMessage struct {
	Role    string
	Content string
}

type parsedChatReq struct {
	Model    string
	Stream   bool
	Messages []chatMessage
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
		docBuilder.WriteString("\n\n")
		docBuilder.WriteString(m.Content)
		docBuilder.WriteString("\n\n")
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
		InlinePromptMD: docBuilder.String(),
		HistoryEntry:   lastUser,
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
			_ = sse.Complete(out.Result.Usage)
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      randomChatID(),
		"object":  "chat.completion",
		"created": out.Run.CreatedAt,
		"model":   out.Run.Model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": out.Result.Answer,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     out.Result.Usage.InputTokens,
			"completion_tokens": out.Result.Usage.OutputTokens,
			"total_tokens":      out.Result.Usage.TotalTokens,
		},
	})
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
		"model":    true,
		"messages": true,
		"stream":   true,
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
		case "system", "developer", "user", "assistant":
		default:
			return out, "messages", "invalid_parameter", fmt.Sprintf("Unsupported message role %q", role)
		}

		cRaw, hasContent := item["content"]
		if !hasContent || isJSONNull(cRaw) {
			return out, "messages", "invalid_parameter", "Each message requires 'content'"
		}

		var contentText string
		if err := json.Unmarshal(cRaw, &contentText); err != nil {
			var parts []map[string]json.RawMessage
			if err2 := json.Unmarshal(cRaw, &parts); err2 != nil {
				return out, "messages", "invalid_parameter", "Message 'content' must be a string or array of text parts"
			}
			var sb strings.Builder
			for _, p := range parts {
				var pType, txt string
				if tRaw, ok := p["type"]; ok {
					_ = json.Unmarshal(tRaw, &pType)
				}
				if pType != "text" && pType != "input_text" {
					return out, "messages", "unsupported_input_type", fmt.Sprintf("Unsupported message content type %q in chat completions", pType)
				}
				if txtRaw, ok := p["text"]; !ok || json.Unmarshal(txtRaw, &txt) != nil {
					return out, "messages", "invalid_parameter", "Text part requires string field 'text'"
				}
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(txt)
			}
			contentText = sb.String()
		}
		out.Messages = append(out.Messages, chatMessage{Role: role, Content: contentText})
	}
	return out, "", "", ""
}

func randomChatID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}
