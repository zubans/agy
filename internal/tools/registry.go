package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/storage"
)

// ModelInvoker allows server tools (such as image_analyze or audio_transcribe fallback)
// to delegate work to a capable model inside the gateway pool.
type ModelInvoker func(ctx context.Context, ownerID, capability, prompt string, fileIDs []string) (string, error)

// RunContext carries per-run isolation state into every tool invocation.
type RunContext struct {
	RunID           string
	OwnerID         string
	WorkDir         string
	AttachedFiles   []storage.FileRecord
	OnFileGenerated func(ref executor.GeneratedFileRef)
}

// ToolOutput is the structured result returned by a server-side tool.
type ToolOutput struct {
	Text           string                      `json:"text"`
	Data           map[string]any              `json:"data,omitempty"`
	GeneratedFiles []executor.GeneratedFileRef `json:"generated_files,omitempty"`
	IsError        bool                        `json:"is_error,omitempty"`
}

// Tool defines one server-side tool callable by models via MCP or native tool calling.
type Tool interface {
	Name() string
	Category() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error)
}

// ToolInfo is the administrative summary of a registered server tool.
type ToolInfo struct {
	Name        string         `json:"name"`
	Category    string         `json:"category"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Available   bool           `json:"available"`
	Requirement string         `json:"requirement,omitempty"`
}

// BackendConfig stores connection settings for an external/sidecar media backend.
type BackendConfig struct {
	Key       string            `json:"key"`
	Name      string            `json:"name"`
	BaseURL   string            `json:"base_url"`
	APIKey    string            `json:"api_key,omitempty"`
	HasAPIKey bool              `json:"has_api_key,omitempty"`
	Model     string            `json:"model,omitempty"`
	Enabled   bool              `json:"enabled"`
	Extra     map[string]string `json:"extra,omitempty"`
	UpdatedAt int64             `json:"updated_at,omitempty"`
}

// ProbeResult reports live readiness of a backend or local binary.
type ProbeResult struct {
	Key     string `json:"key"`
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

// Registry manages all built-in server-side tools and their backend configurations.
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	files   *storage.FileStore
	store   *BackendStore
	invoker ModelInvoker
	client  *http.Client
}

// NewRegistry creates a Registry preloaded with all built-in workspace, audio, and image tools.
func NewRegistry(files *storage.FileStore, store *BackendStore) *Registry {
	r := &Registry{
		tools: make(map[string]Tool),
		files: files,
		store: store,
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
	r.registerDefaults()
	return r
}

// SetModelInvoker wires a callback so tools like image_analyze / audio_transcribe can call pool models.
func (r *Registry) SetModelInvoker(fn ModelInvoker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invoker = fn
}

func (r *Registry) getInvoker() ModelInvoker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.invoker
}

// Store returns the underlying SQLite backend/binding store (may be nil in isolated unit tests).
func (r *Registry) Store() *BackendStore {
	return r.store
}

// Register adds or replaces a tool in the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// AllNames returns sorted names of all registered server tools.
func (r *Registry) AllNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Definitions returns OpenAI/MCP tool definitions for the given allowlist (empty = none; ["*"] = all).
func (r *Registry) Definitions(allowlist []string) []executor.ToolDefinition {
	if len(allowlist) == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	allowAll := false
	allowed := make(map[string]bool, len(allowlist))
	for _, name := range allowlist {
		n := strings.TrimSpace(name)
		if n == "*" {
			allowAll = true
		} else if n != "" {
			allowed[n] = true
		}
	}

	var defs []executor.ToolDefinition
	for _, name := range r.sortedNamesLocked() {
		if !allowAll && !allowed[name] {
			continue
		}
		t := r.tools[name]
		defs = append(defs, executor.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return defs
}

// ListInfo returns metadata and availability for all registered tools (used by Admin API).
func (r *Registry) ListInfo(ctx context.Context) []ToolInfo {
	r.mu.RLock()
	names := r.sortedNamesLocked()
	toolsCopy := make([]Tool, 0, len(names))
	for _, n := range names {
		toolsCopy = append(toolsCopy, r.tools[n])
	}
	r.mu.RUnlock()

	out := make([]ToolInfo, 0, len(toolsCopy))
	for _, t := range toolsCopy {
		avail, req := r.checkToolAvailability(ctx, t.Name())
		out = append(out, ToolInfo{
			Name:        t.Name(),
			Category:    t.Category(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
			Available:   avail,
			Requirement: req,
		})
	}
	return out
}

// Execute invokes a named server tool with timeout and records any generated files.
func (r *Registry) Execute(ctx context.Context, runCtx RunContext, name string, rawArgs json.RawMessage) (ToolOutput, error) {
	t, ok := r.Get(name)
	if !ok {
		return ToolOutput{Text: fmt.Sprintf("unknown server tool %q", name), IsError: true}, fmt.Errorf("unknown server tool %q", name)
	}
	if len(rawArgs) == 0 {
		rawArgs = json.RawMessage(`{}`)
	}
	out, err := t.Execute(ctx, runCtx, rawArgs)
	if err != nil {
		if out.Text == "" {
			out.Text = err.Error()
		}
		out.IsError = true
		return out, err
	}
	if runCtx.OnFileGenerated != nil {
		for _, ref := range out.GeneratedFiles {
			runCtx.OnFileGenerated(ref)
		}
	}
	return out, nil
}

// ProbeAll checks local binaries (ffmpeg, ffprobe, tesseract, mmdc) and configured HTTP backends.
func (r *Registry) ProbeAll(ctx context.Context) []ProbeResult {
	var results []ProbeResult
	for _, bin := range []string{"ffmpeg", "ffprobe", "tesseract", "mmdc"} {
		if p, err := exec.LookPath(bin); err == nil {
			results = append(results, ProbeResult{Key: "bin:" + bin, Ready: true, Message: p})
		} else {
			results = append(results, ProbeResult{Key: "bin:" + bin, Ready: false, Message: "not installed in PATH"})
		}
	}
	if r.store != nil {
		backends, err := r.store.ListBackends(ctx)
		if err == nil {
			for _, b := range backends {
				results = append(results, r.probeBackend(ctx, b))
			}
		}
	}
	return results
}

func (r *Registry) probeBackend(ctx context.Context, b BackendConfig) ProbeResult {
	if !b.Enabled || strings.TrimSpace(b.BaseURL) == "" {
		return ProbeResult{Key: b.Key, Ready: false, Message: "disabled or base_url not configured"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	url := strings.TrimRight(b.BaseURL, "/")
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeResult{Key: b.Key, Ready: false, Message: err.Error()}
	}
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return ProbeResult{Key: b.Key, Ready: false, Message: err.Error()}
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		return ProbeResult{Key: b.Key, Ready: false, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return ProbeResult{Key: b.Key, Ready: true, Message: fmt.Sprintf("reachable (HTTP %d)", resp.StatusCode)}
}

func (r *Registry) checkToolAvailability(ctx context.Context, name string) (bool, string) {
	switch name {
	case "workspace_list_files", "workspace_read_text", "workspace_save_output", "image_info", "image_transform":
		return true, ""
	case "audio_info":
		if _, err := exec.LookPath("ffprobe"); err == nil {
			return true, ""
		}
		return false, "requires ffprobe in PATH"
	case "audio_convert_trim":
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			return true, ""
		}
		return false, "requires ffmpeg in PATH"
	case "audio_transcribe":
		if b, ok := r.getBackend(ctx, "whisper"); ok && b.Enabled && b.BaseURL != "" {
			return true, ""
		}
		if r.getInvoker() != nil {
			return true, "uses pool audio_in model (no Whisper URL configured)"
		}
		return false, "configure whisper backend URL or audio_in model"
	case "audio_tts":
		if b, ok := r.getBackend(ctx, "tts"); ok && b.Enabled && b.BaseURL != "" {
			return true, ""
		}
		return false, "configure tts backend URL"
	case "image_analyze":
		if r.getInvoker() != nil {
			return true, ""
		}
		return false, "requires vision model in pool"
	case "image_ocr":
		if _, err := exec.LookPath("tesseract"); err == nil {
			return true, ""
		}
		if r.getInvoker() != nil {
			return true, "fallback to vision model (tesseract not installed)"
		}
		return false, "requires tesseract in PATH or vision model"
	case "image_generate":
		if b, ok := r.getBackend(ctx, "image_gen"); ok && b.Enabled && b.BaseURL != "" {
			return true, ""
		}
		return false, "configure image_gen backend URL"
	case "image_render_diagram":
		if _, err := exec.LookPath("mmdc"); err == nil {
			return true, ""
		}
		return true, "HTML/SVG output ready (install mmdc for PNG Mermaid rendering)"
	default:
		return true, ""
	}
}

func (r *Registry) getBackend(ctx context.Context, key string) (BackendConfig, bool) {
	if r.store == nil {
		return BackendConfig{}, false
	}
	return r.store.GetBackend(ctx, key)
}

func (r *Registry) sortedNamesLocked() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) registerDefaults() {
	r.Register(&workspaceListTool{reg: r})
	r.Register(&workspaceReadTextTool{reg: r})
	r.Register(&workspaceSaveOutputTool{reg: r})

	r.Register(&audioInfoTool{reg: r})
	r.Register(&audioConvertTrimTool{reg: r})
	r.Register(&audioTranscribeTool{reg: r})
	r.Register(&audioTTSTool{reg: r})

	r.Register(&imageInfoTool{reg: r})
	r.Register(&imageTransformTool{reg: r})
	r.Register(&imageAnalyzeTool{reg: r})
	r.Register(&imageOCRTool{reg: r})
	r.Register(&imageGenerateTool{reg: r})
	r.Register(&imageRenderDiagramTool{reg: r})
}

func fileRecordToRef(rec storage.FileRecord) executor.GeneratedFileRef {
	return executor.GeneratedFileRef{
		FileID:      rec.ID,
		Filename:    rec.Filename,
		MediaKind:   rec.MediaKind,
		ContentType: rec.ContentType,
		Bytes:       rec.Bytes,
		URL:         "/v1/files/" + rec.ID + "/content",
	}
}
