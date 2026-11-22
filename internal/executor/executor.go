package executor

import (
	"context"
	"fmt"
	"time"
)

// EventType identifies a normalized streaming event emitted by an Executor.
type EventType string

const (
	EventStarted   EventType = "started"
	EventTextDelta EventType = "text_delta"
	EventStatus    EventType = "status"
	EventCompleted EventType = "completed"
	EventError     EventType = "error"
)

// Capabilities honestly describes what an executor supports (Section 7.4, ADR-004).
// Adapters must not fake capabilities absent from the underlying CLI/engine.
type Capabilities struct {
	Streaming        bool     `json:"streaming"`
	SessionResume    bool     `json:"session_resume"`
	ModelSelection   bool     `json:"model_selection"`
	FileInput        bool     `json:"file_input"`
	ImageIn          bool     `json:"image_in"`
	AudioIn          bool     `json:"audio_in"`
	ImageOut         bool     `json:"image_out"`
	AudioOut         bool     `json:"audio_out"`
	Tools            bool     `json:"tools"`
	MCP              bool     `json:"mcp"`
	SupportedFormats []string `json:"supported_formats"`
	TokenUsage       bool     `json:"token_usage"`
}

// ToolDefinition describes a JSON-Schema function tool (OpenAI / MCP compatible).
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolCall represents a single model-initiated tool invocation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON object string
}

// GeneratedFileRef references a file artifact created by a server tool during a run.
type GeneratedFileRef struct {
	FileID      string `json:"file_id"`
	Filename    string `json:"filename"`
	MediaKind   string `json:"media_kind"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	URL         string `json:"url"`
}

// MCPConfig carries scoped per-run MCP server connection info for CLI workers.
type MCPConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// ExecuteFile describes an authorized file staged inside the run workspace.
type ExecuteFile struct {
	ID           string
	Name         string
	Path         string
	RelativePath string
	Size         int64
	MediaKind    string
	ContentType  string
}

// SessionContext carries gateway and provider session continuity state.
type SessionContext struct {
	GatewaySessionID  string
	ProviderSessionID string
	IsResume          bool
	HistoryFile       string
}

// ExecutionSettings holds per-run operational limits.
type ExecutionSettings struct {
	Timeout        time.Duration
	MaxOutputBytes int64
	Stream         bool
	DiagnosticLog  bool
}

// ExecuteRequest is the unified input contract passed to every Executor.
type ExecuteRequest struct {
	RunID         string
	Model         string
	ProviderModel string
	WorkDir       string
	Files         []ExecuteFile
	Instructions  string
	Prompt        string
	Tools         []ToolDefinition
	ToolChoice    any
	MCP           *MCPConfig
	Session       SessionContext
	Settings      ExecutionSettings
}

// Usage contains token accounting reported by an executor.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Available    bool  `json:"available"`
}

// Event is a normalized lifecycle or text chunk event emitted during Execute.
type Event struct {
	Type      EventType `json:"type"`
	Text      string    `json:"text,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Usage     *Usage    `json:"usage,omitempty"`
	RawType   string    `json:"raw_type,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// EventHandler receives normalized events as they arrive from an executor.
type EventHandler func(Event) error

// ErrorKind classifies typed internal executor failures (Section 12).
type ErrorKind string

const (
	ErrUnavailable ErrorKind = "unavailable"
	ErrTimeout     ErrorKind = "timeout"
	ErrExecution   ErrorKind = "execution"
	ErrAuth        ErrorKind = "auth"
	ErrRateLimit   ErrorKind = "rate_limit"
	ErrCancelled   ErrorKind = "cancelled"
)

// ExecError represents a sanitized, typed executor failure.
type ExecError struct {
	Kind              ErrorKind
	Code              string
	Message           string
	ExitCode          int
	RetryAfterSeconds int64
	CooldownSource    string
	Cause             error
}

func (e *ExecError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return fmt.Sprintf("executor error (%s)", e.Kind)
}

func (e *ExecError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ExecuteResult captures the final outcome of an executor run.
type ExecuteResult struct {
	Answer            string
	ProviderSessionID string
	Status            string
	FinishReason      string
	ToolCalls         []ToolCall
	GeneratedFiles    []GeneratedFileRef
	Usage             Usage
	ExitCode          int
	Error             *ExecError
}

// ModelDescriptor describes a selectable model offered by an executor provider.
type ModelDescriptor struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

// ModelLister is optionally implemented by executors capable of enumerating selectable sub-models.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelDescriptor, string, error)
}

// Executor defines the unified contract for CLI adapters and local engines (Section 7.4).
type Executor interface {
	Name() string
	Capabilities() Capabilities
	Check(ctx context.Context) error
	Execute(
		ctx context.Context,
		request ExecuteRequest,
		emit EventHandler,
	) (ExecuteResult, error)
}
