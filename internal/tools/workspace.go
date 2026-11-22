package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/storage"
)

type workspaceListTool struct{ reg *Registry }

func (t *workspaceListTool) Name() string     { return "workspace_list_files" }
func (t *workspaceListTool) Category() string { return "workspace" }
func (t *workspaceListTool) Description() string {
	return "List files attached to the current request (IDs, filenames, media kinds, sizes, image dimensions)."
}
func (t *workspaceListTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *workspaceListTool) Execute(_ context.Context, runCtx RunContext, _ json.RawMessage) (ToolOutput, error) {
	items := make([]map[string]any, 0, len(runCtx.AttachedFiles))
	for _, f := range runCtx.AttachedFiles {
		item := map[string]any{
			"file_id":      f.ID,
			"filename":     f.Filename,
			"media_kind":   f.MediaKind,
			"content_type": f.ContentType,
			"bytes":        f.Bytes,
		}
		if f.Width > 0 && f.Height > 0 {
			item["width"] = f.Width
			item["height"] = f.Height
		}
		if f.DurationSec > 0 {
			item["duration_sec"] = f.DurationSec
		}
		items = append(items, item)
	}
	raw, _ := json.MarshalIndent(items, "", "  ")
	return ToolOutput{Text: string(raw), Data: map[string]any{"files": items}}, nil
}

type workspaceReadTextTool struct{ reg *Registry }

func (t *workspaceReadTextTool) Name() string     { return "workspace_read_text" }
func (t *workspaceReadTextTool) Category() string { return "workspace" }
func (t *workspaceReadTextTool) Description() string {
	return "Read a UTF-8 text file attached to the run by file_id, optionally specifying byte offset and limit."
}
func (t *workspaceReadTextTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id": map[string]any{"type": "string", "description": "ID of the text file to read"},
			"offset":  map[string]any{"type": "integer", "description": "Optional starting byte offset (default 0)"},
			"limit":   map[string]any{"type": "integer", "description": "Optional max bytes to read (default 65536, max 262144)"},
		},
		"required": []string{"file_id"},
	}
}
func (t *workspaceReadTextTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID string `json:"file_id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.FileID) == "" {
		return ToolOutput{}, errors.New("file_id is required")
	}
	if t.reg.files == nil {
		return ToolOutput{}, errors.New("file store unavailable")
	}
	if args.Limit <= 0 {
		args.Limit = 65536
	}
	if args.Limit > 262144 {
		args.Limit = 262144
	}
	if args.Offset < 0 {
		args.Offset = 0
	}
	rec, data, err := t.reg.files.ReadBytes(ctx, runCtx.OwnerID, args.FileID, 8*1024*1024)
	if err != nil {
		return ToolOutput{}, err
	}
	if rec.MediaKind != "" && rec.MediaKind != storage.MediaKindText {
		return ToolOutput{}, fmt.Errorf("file %s has media_kind=%q and cannot be read as plain text", rec.ID, rec.MediaKind)
	}
	if args.Offset >= len(data) {
		return ToolOutput{Text: "", Data: map[string]any{"file_id": rec.ID, "eof": true, "total_bytes": len(data)}}, nil
	}
	end := args.Offset + args.Limit
	eof := false
	if end >= len(data) {
		end = len(data)
		eof = true
	}
	return ToolOutput{
		Text: string(data[args.Offset:end]),
		Data: map[string]any{
			"file_id":     rec.ID,
			"filename":    rec.Filename,
			"offset":      args.Offset,
			"returned":    end - args.Offset,
			"total_bytes": len(data),
			"eof":         eof,
		},
	}, nil
}

type workspaceSaveOutputTool struct{ reg *Registry }

func (t *workspaceSaveOutputTool) Name() string     { return "workspace_save_output" }
func (t *workspaceSaveOutputTool) Category() string { return "workspace" }
func (t *workspaceSaveOutputTool) Description() string {
	return "Save a text, markdown, JSON, HTML, or SVG artifact as a downloadable file owned by the requester."
}
func (t *workspaceSaveOutputTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filename": map[string]any{"type": "string", "description": "Target filename (e.g. report.md, widget.html, chart.svg)"},
			"content":  map[string]any{"type": "string", "description": "UTF-8 file content"},
		},
		"required": []string{"filename", "content"},
	}
}
func (t *workspaceSaveOutputTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Filename) == "" || args.Content == "" {
		return ToolOutput{}, errors.New("filename and non-empty content are required")
	}
	if t.reg.files == nil {
		return ToolOutput{}, errors.New("file store unavailable")
	}
	rec, err := t.reg.files.SaveGenerated(ctx, runCtx.OwnerID, args.Filename, []byte(args.Content))
	if err != nil {
		return ToolOutput{}, err
	}
	ref := fileRecordToRef(rec)
	return ToolOutput{
		Text:           fmt.Sprintf("Saved artifact %q as %s (%d bytes, URL: %s)", rec.Filename, rec.ID, rec.Bytes, ref.URL),
		Data:           map[string]any{"file_id": rec.ID, "filename": rec.Filename, "url": ref.URL, "bytes": rec.Bytes},
		GeneratedFiles: []executor.GeneratedFileRef{ref},
	}, nil
}
