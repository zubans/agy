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

	"cli-ai-gateway/internal/admin"
	"cli-ai-gateway/internal/auth"
	"cli-ai-gateway/internal/session"
	"cli-ai-gateway/internal/streaming"
)

// ResponsesHandler implements POST /v1/responses (Section 6.6).
type ResponsesHandler struct {
	runner *Runner
}

func NewResponsesHandler(runner *Runner) *ResponsesHandler {
	return &ResponsesHandler{runner: runner}
}

type parsedResponsesReq struct {
	Model              string
	Stream             bool
	PreviousResponseID string
	Instructions       string
	FileIDs            []string
	TextParts          []string
}

func (h *ResponsesHandler) Create(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())

	req, errParam, errCode, errMsg := parseResponsesRequest(r.Body)
	if errCode != "" {
		writeInvalidRequest(w, errCode, errMsg, errParam)
		if audit := AuditFromContext(r.Context()); audit != nil {
			audit.SetErrorType(errCode)
		}
		return
	}

	inlineText := strings.Join(req.TextParts, "\n\n")
	executeResponseFlow(r.Context(), w, h.runner, principal, req.Model, req.PreviousResponseID, req.Instructions, req.FileIDs, inlineText, req.Stream)
}

// ExecutePlayground runs an interactive Web Admin playground request through the Responses pipeline.
func (rn *Runner) ExecutePlayground(ctx context.Context, w http.ResponseWriter, in admin.PlaygroundInput) {
	ownerID := strings.TrimSpace(in.OwnerID)
	if ownerID == "" {
		ownerID = "admin"
	}
	principal := auth.Principal{
		KeyID:         "admin",
		OwnerID:       ownerID,
		Name:          "Web Admin",
		AllowedModels: []string{"*"},
		MaxConcurrent: rn.cfg.Limits.MaxGlobalProcesses,
	}
	executeResponseFlow(ctx, w, rn, principal, in.ModelID, in.PreviousResponseID, in.Instructions, in.FileIDs, in.Prompt, in.Stream)
}

func executeResponseFlow(
	ctx context.Context,
	w http.ResponseWriter,
	runner *Runner,
	principal auth.Principal,
	modelID string,
	prevRespID string,
	instructions string,
	fileIDs []string,
	inlineText string,
	stream bool,
) {
	var inlinePromptMD, promptNote, historyEntry string
	if len(fileIDs) == 0 {
		inlinePromptMD = inlineText
		historyEntry = inlineText
	} else {
		promptNote = inlineText
		if inlineText != "" {
			historyEntry = inlineText
		}
	}

	var sse *streaming.ResponsesSSE
	out, ok, runErr := runner.Execute(ctx, w, PipelineInput{
		Principal:          principal,
		ModelID:            modelID,
		PreviousResponseID: prevRespID,
		Instructions:       instructions,
		FileIDs:            fileIDs,
		InlinePromptMD:     inlinePromptMD,
		PromptNote:         promptNote,
		HistoryEntry:       historyEntry,
		Stream:             stream,
		OnStreamReady: func(run session.RunRecord, prevPtr *string) error {
			sse = streaming.NewResponsesSSE(w, run.ResponseID, run.Model, run.CreatedAt, prevPtr)
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

	if stream {
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
			_ = sse.Complete(out.Result.Answer, out.Result.Usage)
		}
		return
	}

	var prevPtr *string
	if prevRespID != "" {
		p := prevRespID
		prevPtr = &p
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                   out.Run.ResponseID,
		"object":               "response",
		"created_at":           out.Run.CreatedAt,
		"status":               "completed",
		"model":                out.Run.Model,
		"previous_response_id": prevPtr,
		"output": []map[string]any{
			{
				"id":     randomMsgID(),
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{
					{
						"type":        "output_text",
						"text":        out.Result.Answer,
						"annotations": []any{},
					},
				},
			},
		},
		"usage": map[string]any{
			"input_tokens":  out.Result.Usage.InputTokens,
			"output_tokens": out.Result.Usage.OutputTokens,
			"total_tokens":  out.Result.Usage.TotalTokens,
		},
	})
}

func parseResponsesRequest(body io.Reader) (parsedResponsesReq, string, string, string) {
	raw, err := io.ReadAll(io.LimitReader(body, 10*1024*1024+1))
	if err != nil {
		return parsedResponsesReq{}, "", "invalid_json", "Failed to read request body"
	}
	if len(raw) > 10*1024*1024 {
		return parsedResponsesReq{}, "", "request_too_large", "JSON request body exceeds 10 MiB limit"
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return parsedResponsesReq{}, "", "invalid_json", "Request body must be valid JSON object"
	}

	allowed := map[string]bool{
		"model":                true,
		"input":                true,
		"stream":               true,
		"previous_response_id": true,
		"instructions":         true,
	}
	for k := range fields {
		if !allowed[k] {
			return parsedResponsesReq{}, k, "unsupported_parameter", fmt.Sprintf("Unsupported parameter %q", k)
		}
	}

	var out parsedResponsesReq
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

	if rawPrev, ok := fields["previous_response_id"]; ok && !isJSONNull(rawPrev) {
		if err := json.Unmarshal(rawPrev, &out.PreviousResponseID); err != nil {
			return out, "previous_response_id", "invalid_parameter", "Parameter 'previous_response_id' must be a string"
		}
	}

	if rawInstr, ok := fields["instructions"]; ok && !isJSONNull(rawInstr) {
		if err := json.Unmarshal(rawInstr, &out.Instructions); err != nil {
			return out, "instructions", "invalid_parameter", "Parameter 'instructions' must be a string"
		}
	}

	rawInput, ok := fields["input"]
	if !ok || isJSONNull(rawInput) {
		return out, "input", "missing_required_parameter", "Parameter 'input' is required"
	}

	// Accept string input directly or array of Responses API items.
	var asStr string
	if err := json.Unmarshal(rawInput, &asStr); err == nil {
		if strings.TrimSpace(asStr) == "" {
			return out, "input", "invalid_parameter", "Parameter 'input' must not be empty"
		}
		out.TextParts = append(out.TextParts, asStr)
		return out, "", "", ""
	}

	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil || len(items) == 0 {
		return out, "input", "invalid_parameter", "Parameter 'input' must be a string or a non-empty array of input items"
	}

	for _, itemRaw := range items {
		var itemObj map[string]json.RawMessage
		if err := json.Unmarshal(itemRaw, &itemObj); err != nil {
			return out, "input", "invalid_parameter", "Each element of 'input' must be an object"
		}

		var itemType string
		if tRaw, ok := itemObj["type"]; ok && !isJSONNull(tRaw) {
			_ = json.Unmarshal(tRaw, &itemType)
		}

		// Handle direct content items at top level of input array if supplied.
		if itemType == "input_text" || itemType == "input_file" {
			if errCode, errMsg := parseContentPart(itemObj, &out); errCode != "" {
				return out, "input", errCode, errMsg
			}
			continue
		}

		if itemType != "" && itemType != "message" {
			return out, "input", "unsupported_input_type", fmt.Sprintf("Unsupported input item type %q", itemType)
		}

		var role string
		if rRaw, ok := itemObj["role"]; ok && !isJSONNull(rRaw) {
			if err := json.Unmarshal(rRaw, &role); err != nil {
				return out, "input", "invalid_parameter", "Input item 'role' must be a string"
			}
		}

		cRaw, hasContent := itemObj["content"]
		if !hasContent || isJSONNull(cRaw) {
			return out, "input", "invalid_parameter", "Input message must include 'content'"
		}

		var contentStr string
		if err := json.Unmarshal(cRaw, &contentStr); err == nil {
			if role == "system" || role == "developer" {
				if out.Instructions != "" {
					out.Instructions += "\n\n" + contentStr
				} else {
					out.Instructions = contentStr
				}
			} else if contentStr != "" {
				out.TextParts = append(out.TextParts, contentStr)
			}
			continue
		}

		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(cRaw, &parts); err != nil {
			return out, "input", "invalid_parameter", "Message 'content' must be a string or array of content parts"
		}
		for _, part := range parts {
			if errCode, errMsg := parseContentPart(part, &out); errCode != "" {
				return out, "input", errCode, errMsg
			}
		}
	}

	if len(out.FileIDs) == 0 && len(out.TextParts) == 0 {
		return out, "input", "invalid_parameter", "Request must contain at least one input_text or input_file"
	}
	out.FileIDs = dedupeStrings(out.FileIDs)
	return out, "", "", ""
}

func dedupeStrings(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func parseContentPart(part map[string]json.RawMessage, out *parsedResponsesReq) (string, string) {
	var pType string
	if tRaw, ok := part["type"]; ok {
		_ = json.Unmarshal(tRaw, &pType)
	}
	switch pType {
	case "input_text", "text":
		var txt string
		if tRaw, ok := part["text"]; !ok || json.Unmarshal(tRaw, &txt) != nil {
			return "invalid_parameter", "input_text part requires string field 'text'"
		}
		if txt != "" {
			out.TextParts = append(out.TextParts, txt)
		}
	case "input_file":
		var fid string
		if fRaw, ok := part["file_id"]; !ok || json.Unmarshal(fRaw, &fid) != nil || strings.TrimSpace(fid) == "" {
			return "invalid_parameter", "input_file part requires non-empty 'file_id'"
		}
		out.FileIDs = append(out.FileIDs, fid)
	default:
		return "unsupported_input_type", fmt.Sprintf("Unsupported input content type %q", pType)
	}
	return "", ""
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func randomMsgID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}
