package streaming

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"cli-ai-gateway/internal/executor"
)

// InitSSE prepares HTTP response headers for Server-Sent Events streaming.
func InitSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flushWriter(w)
}

func flushWriter(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func writeSSEEvent(w http.ResponseWriter, eventName string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if eventName != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", eventName); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}

// ResponsesSSE emits OpenAI Responses API SSE lifecycle events strictly (Section 9.1).
// Never substitutes Chat Completions chunk objects.
type ResponsesSSE struct {
	w          http.ResponseWriter
	mu         sync.Mutex
	responseID string
	itemID     string
	model      string
	createdAt  int64
	prevRespID *string
	seq        int
	started    bool
	itemAdded  bool
	textAdded  bool
	closed     bool
}

// NewResponsesSSE initializes an OpenAI Responses API SSE translator.
func NewResponsesSSE(w http.ResponseWriter, responseID, model string, createdAt int64, prevRespID *string) *ResponsesSSE {
	return &ResponsesSSE{
		w:          w,
		responseID: responseID,
		itemID:     randomID("msg"),
		model:      model,
		createdAt:  createdAt,
		prevRespID: prevRespID,
	}
}

func (s *ResponsesSSE) nextSeq() int {
	n := s.seq
	s.seq++
	return n
}

// Start writes initial response.created, response.in_progress, output_item.added, and content_part.added.
func (s *ResponsesSSE) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	s.started = true
	InitSSE(s.w)

	baseResp := map[string]any{
		"id":                   s.responseID,
		"object":               "response",
		"created_at":           s.createdAt,
		"status":               "in_progress",
		"model":                s.model,
		"previous_response_id": s.prevRespID,
		"output":               []any{},
	}
	if err := writeSSEEvent(s.w, "response.created", map[string]any{
		"type":            "response.created",
		"sequence_number": s.nextSeq(),
		"response":        baseResp,
	}); err != nil {
		return err
	}
	if err := writeSSEEvent(s.w, "response.in_progress", map[string]any{
		"type":            "response.in_progress",
		"sequence_number": s.nextSeq(),
		"response":        baseResp,
	}); err != nil {
		return err
	}

	s.itemAdded = true
	if err := writeSSEEvent(s.w, "response.output_item.added", map[string]any{
		"type":            "response.output_item.added",
		"sequence_number": s.nextSeq(),
		"output_index":    0,
		"item": map[string]any{
			"id":      s.itemID,
			"type":    "message",
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	}); err != nil {
		return err
	}

	s.textAdded = true
	return writeSSEEvent(s.w, "response.content_part.added", map[string]any{
		"type":            "response.content_part.added",
		"sequence_number": s.nextSeq(),
		"item_id":         s.itemID,
		"output_index":    0,
		"content_index":   0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
		},
	})
}

// Started reports whether SSE headers have already been flushed to the client.
func (s *ResponsesSSE) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Delta emits a response.output_text.delta event.
func (s *ResponsesSSE) Delta(text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return writeSSEEvent(s.w, "response.output_text.delta", map[string]any{
		"type":            "response.output_text.delta",
		"sequence_number": s.nextSeq(),
		"item_id":         s.itemID,
		"output_index":    0,
		"content_index":   0,
		"delta":           text,
	})
}

// Complete emits output_text.done, content_part.done, output_item.done, and response.completed.
func (s *ResponsesSSE) Complete(fullText string, usage executor.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	if err := writeSSEEvent(s.w, "response.output_text.done", map[string]any{
		"type":            "response.output_text.done",
		"sequence_number": s.nextSeq(),
		"item_id":         s.itemID,
		"output_index":    0,
		"content_index":   0,
		"text":            fullText,
	}); err != nil {
		return err
	}

	part := map[string]any{
		"type":        "output_text",
		"text":        fullText,
		"annotations": []any{},
	}
	if err := writeSSEEvent(s.w, "response.content_part.done", map[string]any{
		"type":            "response.content_part.done",
		"sequence_number": s.nextSeq(),
		"item_id":         s.itemID,
		"output_index":    0,
		"content_index":   0,
		"part":            part,
	}); err != nil {
		return err
	}

	item := map[string]any{
		"id":      s.itemID,
		"type":    "message",
		"status":  "completed",
		"role":    "assistant",
		"content": []any{part},
	}
	if err := writeSSEEvent(s.w, "response.output_item.done", map[string]any{
		"type":            "response.output_item.done",
		"sequence_number": s.nextSeq(),
		"output_index":    0,
		"item":            item,
	}); err != nil {
		return err
	}

	resp := map[string]any{
		"id":                   s.responseID,
		"object":               "response",
		"created_at":           s.createdAt,
		"status":               "completed",
		"model":                s.model,
		"previous_response_id": s.prevRespID,
		"output":               []any{item},
		"usage": map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  usage.TotalTokens,
		},
	}
	return writeSSEEvent(s.w, "response.completed", map[string]any{
		"type":            "response.completed",
		"sequence_number": s.nextSeq(),
		"response":        resp,
	})
}

// Fail emits an SSE error event and response.failed event when an error occurs mid-stream (Section 9.2).
func (s *ResponsesSSE) Fail(errType, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.started {
		return
	}
	s.closed = true
	_ = writeSSEEvent(s.w, "error", map[string]any{
		"type":            "error",
		"sequence_number": s.nextSeq(),
		"code":            code,
		"message":         message,
		"param":           nil,
	})
	_ = writeSSEEvent(s.w, "response.failed", map[string]any{
		"type":            "response.failed",
		"sequence_number": s.nextSeq(),
		"response": map[string]any{
			"id":                   s.responseID,
			"object":               "response",
			"created_at":           s.createdAt,
			"status":               "failed",
			"model":                s.model,
			"previous_response_id": s.prevRespID,
			"output":               []any{},
			"error": map[string]any{
				"code":    code,
				"message": message,
				"type":    errType,
			},
		},
	})
}

// ChatCompletionsSSE emits OpenAI `chat.completion.chunk` SSE events (Section 6.7 & 9.1).
type ChatCompletionsSSE struct {
	w       http.ResponseWriter
	mu      sync.Mutex
	id      string
	model   string
	created int64
	started bool
	closed  bool
}

// NewChatCompletionsSSE initializes a Chat Completions SSE translator.
func NewChatCompletionsSSE(w http.ResponseWriter, model string, created int64) *ChatCompletionsSSE {
	return &ChatCompletionsSSE{
		w:       w,
		id:      randomID("chatcmpl"),
		model:   model,
		created: created,
	}
}

func (c *ChatCompletionsSSE) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	c.started = true
	InitSSE(c.w)
	return writeSSEEvent(c.w, "", map[string]any{
		"id":      c.id,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{"role": "assistant"},
				"finish_reason": nil,
			},
		},
	})
}

func (c *ChatCompletionsSSE) Started() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

func (c *ChatCompletionsSSE) Delta(text string) error {
	if text == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return writeSSEEvent(c.w, "", map[string]any{
		"id":      c.id,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{"content": text},
				"finish_reason": nil,
			},
		},
	})
}

func (c *ChatCompletionsSSE) Complete(usage executor.Usage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	chunk := map[string]any{
		"id":      c.id,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	if usage.Available {
		chunk["usage"] = map[string]any{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	if err := writeSSEEvent(c.w, "", chunk); err != nil {
		return err
	}
	_, err := fmt.Fprint(c.w, "data: [DONE]\n\n")
	flushWriter(c.w)
	return err
}

func (c *ChatCompletionsSSE) Fail(errType, code, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.started {
		return
	}
	c.closed = true
	_ = writeSSEEvent(c.w, "error", map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    code,
		},
	})
	_, _ = fmt.Fprint(c.w, "data: [DONE]\n\n")
	flushWriter(c.w)
}

func randomID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
