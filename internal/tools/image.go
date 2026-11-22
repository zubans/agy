package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/storage"
)

// image_info
type imageInfoTool struct{ reg *Registry }

func (t *imageInfoTool) Name() string     { return "image_info" }
func (t *imageInfoTool) Category() string { return "image" }
func (t *imageInfoTool) Description() string {
	return "Inspect image metadata (width, height, format, mime_type, bytes)."
}
func (t *imageInfoTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id": map[string]any{"type": "string", "description": "ID of the uploaded image file"},
		},
		"required": []string{"file_id"},
	}
}
func (t *imageInfoTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	rec, data, err := t.reg.resolveOwnedImageBytes(ctx, runCtx.OwnerID, args.FileID)
	if err != nil {
		return ToolOutput{}, err
	}
	w, h := rec.Width, rec.Height
	format := strings.TrimPrefix(rec.ContentType, "image/")
	if cfg, fmtName, decErr := image.DecodeConfig(bytes.NewReader(data)); decErr == nil {
		w, h = cfg.Width, cfg.Height
		if fmtName != "" {
			format = fmtName
		}
	}
	info := map[string]any{
		"file_id":      rec.ID,
		"filename":     rec.Filename,
		"format":       format,
		"content_type": rec.ContentType,
		"width":        w,
		"height":       h,
		"bytes":        rec.Bytes,
	}
	raw, _ := json.MarshalIndent(info, "", "  ")
	return ToolOutput{Text: string(raw), Data: info}, nil
}

// image_transform (pure Go resize/crop/convert — zero external binary dependencies)
type imageTransformTool struct{ reg *Registry }

func (t *imageTransformTool) Name() string     { return "image_transform" }
func (t *imageTransformTool) Category() string { return "image" }
func (t *imageTransformTool) Description() string {
	return "Resize, crop, or convert an image (png, jpeg, gif) and save the transformed result as a new downloadable file."
}
func (t *imageTransformTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id": map[string]any{"type": "string", "description": "ID of the input image file"},
			"width":   map[string]any{"type": "integer", "description": "Target width in pixels (0 preserves aspect ratio if height is set)"},
			"height":  map[string]any{"type": "integer", "description": "Target height in pixels (0 preserves aspect ratio if width is set)"},
			"crop": map[string]any{
				"type":        "object",
				"description": "Optional crop rectangle {x, y, width, height} applied before resize",
				"properties": map[string]any{
					"x":      map[string]any{"type": "integer"},
					"y":      map[string]any{"type": "integer"},
					"width":  map[string]any{"type": "integer"},
					"height": map[string]any{"type": "integer"},
				},
			},
			"format":  map[string]any{"type": "string", "enum": []string{"png", "jpeg", "gif"}, "description": "Output image format (default png)"},
			"quality": map[string]any{"type": "integer", "description": "JPEG quality 1-100 (default 88)"},
		},
		"required": []string{"file_id"},
	}
}
func (t *imageTransformTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID string `json:"file_id"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
		Crop   *struct {
			X      int `json:"x"`
			Y      int `json:"y"`
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"crop"`
		Format  string `json:"format"`
		Quality int    `json:"quality"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	rec, data, err := t.reg.resolveOwnedImageBytes(ctx, runCtx.OwnerID, args.FileID)
	if err != nil {
		return ToolOutput{}, err
	}

	src, srcFmt, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ToolOutput{}, fmt.Errorf("decode image %q: %w", rec.Filename, err)
	}

	cur := src
	if args.Crop != nil && args.Crop.Width > 0 && args.Crop.Height > 0 {
		cur = cropImage(cur, args.Crop.X, args.Crop.Y, args.Crop.Width, args.Crop.Height)
	}

	if args.Width > 0 || args.Height > 0 {
		b := cur.Bounds()
		sw, sh := b.Dx(), b.Dy()
		tw, th := args.Width, args.Height
		if tw <= 0 && sh > 0 {
			tw = max(1, sw*th/sh)
		}
		if th <= 0 && sw > 0 {
			th = max(1, sh*tw/sw)
		}
		if tw > 8192 || th > 8192 {
			return ToolOutput{}, errors.New("target dimensions exceed 8192px limit")
		}
		cur = resizeBilinear(cur, tw, th)
	}

	outFmt := strings.ToLower(strings.TrimSpace(args.Format))
	if outFmt == "" {
		outFmt = srcFmt
	}
	if outFmt == "jpg" {
		outFmt = "jpeg"
	}
	if outFmt != "png" && outFmt != "jpeg" && outFmt != "gif" {
		outFmt = "png"
	}

	var buf bytes.Buffer
	ext := outFmt
	switch outFmt {
	case "png":
		if err := png.Encode(&buf, cur); err != nil {
			return ToolOutput{}, err
		}
	case "jpeg":
		ext = "jpg"
		q := args.Quality
		if q <= 0 || q > 100 {
			q = 88
		}
		if err := jpeg.Encode(&buf, cur, &jpeg.Options{Quality: q}); err != nil {
			return ToolOutput{}, err
		}
	case "gif":
		if err := gif.Encode(&buf, cur, nil); err != nil {
			return ToolOutput{}, err
		}
	}

	base := strings.TrimSuffix(rec.Filename, filepath.Ext(rec.Filename))
	if base == "" {
		base = "image"
	}
	outName := fmt.Sprintf("%s_transformed.%s", base, ext)
	saved, err := t.reg.files.SaveGenerated(ctx, runCtx.OwnerID, outName, buf.Bytes())
	if err != nil {
		return ToolOutput{}, err
	}
	ref := fileRecordToRef(saved)
	return ToolOutput{
		Text: fmt.Sprintf("Transformed image saved as %q (%dx%d, file_id: %s, URL: %s)", saved.Filename, saved.Width, saved.Height, saved.ID, ref.URL),
		Data: map[string]any{
			"file_id":  saved.ID,
			"filename": saved.Filename,
			"width":    saved.Width,
			"height":   saved.Height,
			"bytes":    saved.Bytes,
			"url":      ref.URL,
		},
		GeneratedFiles: []executor.GeneratedFileRef{ref},
	}, nil
}

// image_analyze
type imageAnalyzeTool struct{ reg *Registry }

func (t *imageAnalyzeTool) Name() string     { return "image_analyze" }
func (t *imageAnalyzeTool) Category() string { return "image" }
func (t *imageAnalyzeTool) Description() string {
	return "Analyze or answer questions about an image by delegating to a vision-capable model in the gateway pool."
}
func (t *imageAnalyzeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id":  map[string]any{"type": "string", "description": "ID of the uploaded image file"},
			"question": map[string]any{"type": "string", "description": "What to inspect or describe in the image"},
		},
		"required": []string{"file_id", "question"},
	}
}
func (t *imageAnalyzeTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID   string `json:"file_id"`
		Question string `json:"question"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Question) == "" {
		return ToolOutput{}, errors.New("question is required")
	}
	rec, _, err := t.reg.resolveOwnedImagePath(ctx, runCtx.OwnerID, args.FileID)
	if err != nil {
		return ToolOutput{}, err
	}
	invoker := t.reg.getInvoker()
	if invoker == nil {
		return ToolOutput{}, errors.New("image_analyze unavailable: no vision model invoker configured")
	}
	answer, err := invoker(ctx, runCtx.OwnerID, "image_in", args.Question, []string{rec.ID})
	if err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{
		Text: answer,
		Data: map[string]any{"file_id": rec.ID, "answer": answer},
	}, nil
}

// image_ocr
type imageOCRTool struct{ reg *Registry }

func (t *imageOCRTool) Name() string     { return "image_ocr" }
func (t *imageOCRTool) Category() string { return "image" }
func (t *imageOCRTool) Description() string {
	return "Extract text from an image or screenshot using tesseract OCR (or falls back to a vision model in the pool)."
}
func (t *imageOCRTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_id": map[string]any{"type": "string", "description": "ID of the uploaded image file"},
			"lang":    map[string]any{"type": "string", "description": "Tesseract language packs (default rus+eng)"},
		},
		"required": []string{"file_id"},
	}
}
func (t *imageOCRTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		FileID string `json:"file_id"`
		Lang   string `json:"lang"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	rec, imgPath, err := t.reg.resolveOwnedImagePath(ctx, runCtx.OwnerID, args.FileID)
	if err != nil {
		return ToolOutput{}, err
	}
	lang := strings.TrimSpace(args.Lang)
	if lang == "" {
		lang = "rus+eng"
	}

	if _, err := exec.LookPath("tesseract"); err == nil {
		cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cmdCtx, "tesseract", imgPath, "stdout", "-l", lang)
		out, runErr := cmd.Output()
		if runErr == nil {
			txt := strings.TrimSpace(string(out))
			return ToolOutput{
				Text: txt,
				Data: map[string]any{"file_id": rec.ID, "text": txt, "engine": "tesseract"},
			}, nil
		}
	}

	invoker := t.reg.getInvoker()
	if invoker == nil {
		return ToolOutput{}, errors.New("image_ocr unavailable: install tesseract in PATH or configure a vision model")
	}
	txt, err := invoker(ctx, runCtx.OwnerID, "image_in", "Extract all visible text from this image verbatim without commentary.", []string{rec.ID})
	if err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{
		Text: txt,
		Data: map[string]any{"file_id": rec.ID, "text": txt, "engine": "vision_model"},
	}, nil
}

// image_generate
type imageGenerateTool struct{ reg *Registry }

func (t *imageGenerateTool) Name() string     { return "image_generate" }
func (t *imageGenerateTool) Category() string { return "image" }
func (t *imageGenerateTool) Description() string {
	return "Generate an image from a text prompt via the configured OpenAI-compatible image generation backend and save it as a downloadable file."
}
func (t *imageGenerateTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string", "description": "Detailed visual prompt describing the image to generate"},
			"size":   map[string]any{"type": "string", "description": "Image dimensions (e.g. 1024x1024, 1536x1024, 1024x1536)"},
		},
		"required": []string{"prompt"},
	}
}
func (t *imageGenerateTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		Prompt string `json:"prompt"`
		Size   string `json:"size"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	return t.reg.GenerateImage(ctx, runCtx.OwnerID, args.Prompt, args.Size)
}

// GenerateImage calls the configured OpenAI-compatible /v1/images/generations backend and persists the image.
func (r *Registry) GenerateImage(ctx context.Context, ownerID, prompt, size string) (ToolOutput, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ToolOutput{}, errors.New("prompt is required")
	}
	backend, ok := r.getBackend(ctx, "image_gen")
	if !ok || !backend.Enabled || strings.TrimSpace(backend.BaseURL) == "" {
		return ToolOutput{}, errors.New("image generation unavailable: configure image_gen backend URL in Admin -> Tools or GATEWAY_IMAGE_GEN_URL")
	}
	if size == "" {
		size = "1024x1024"
	}
	model := backend.Model
	if model == "" {
		model = "gpt-image-1"
	}

	bodyBytes, _ := json.Marshal(map[string]any{
		"model":           model,
		"prompt":          prompt,
		"size":            size,
		"n":               1,
		"response_format": "b64_json",
	})
	endpoint := strings.TrimRight(backend.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/images/generations") {
		endpoint += "/v1/images/generations"
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
		return ToolOutput{}, fmt.Errorf("image_gen request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return ToolOutput{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ToolOutput{}, fmt.Errorf("image_gen backend HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Data) == 0 {
		return ToolOutput{}, errors.New("image_gen backend returned empty data")
	}

	var imgBytes []byte
	if parsed.Data[0].B64JSON != "" {
		imgBytes, err = base64.StdEncoding.DecodeString(parsed.Data[0].B64JSON)
		if err != nil {
			return ToolOutput{}, fmt.Errorf("decode b64_json image: %w", err)
		}
	} else if parsed.Data[0].URL != "" {
		dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.Data[0].URL, nil)
		if err != nil {
			return ToolOutput{}, err
		}
		dlResp, err := r.client.Do(dlReq)
		if err != nil {
			return ToolOutput{}, err
		}
		defer dlResp.Body.Close()
		imgBytes, err = io.ReadAll(io.LimitReader(dlResp.Body, storage.MaxImageFileBytes+1))
		if err != nil {
			return ToolOutput{}, err
		}
	} else {
		return ToolOutput{}, errors.New("image_gen backend returned neither b64_json nor url")
	}

	ext := "png"
	if len(imgBytes) >= 3 && imgBytes[0] == 0xFF && imgBytes[1] == 0xD8 {
		ext = "jpg"
	} else if len(imgBytes) >= 12 && bytes.Equal(imgBytes[8:12], []byte("WEBP")) {
		ext = "webp"
	}

	filename := fmt.Sprintf("generated_%d.%s", time.Now().Unix(), ext)
	saved, err := r.files.SaveGenerated(ctx, ownerID, filename, imgBytes)
	if err != nil {
		return ToolOutput{}, err
	}
	ref := fileRecordToRef(saved)
	return ToolOutput{
		Text: fmt.Sprintf("Generated image saved as %q (%dx%d, file_id: %s, URL: %s)", saved.Filename, saved.Width, saved.Height, saved.ID, ref.URL),
		Data: map[string]any{
			"file_id":  saved.ID,
			"filename": saved.Filename,
			"width":    saved.Width,
			"height":   saved.Height,
			"bytes":    saved.Bytes,
			"url":      ref.URL,
		},
		GeneratedFiles: []executor.GeneratedFileRef{ref},
	}, nil
}

// image_render_diagram
type imageRenderDiagramTool struct{ reg *Registry }

func (t *imageRenderDiagramTool) Name() string     { return "image_render_diagram" }
func (t *imageRenderDiagramTool) Category() string { return "image" }
func (t *imageRenderDiagramTool) Description() string {
	return "Render a Mermaid diagram (to SVG/PNG via mmdc, or self-contained HTML fallback) or save a self-contained HTML widget."
}
func (t *imageRenderDiagramTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":     map[string]any{"type": "string", "enum": []string{"mermaid", "html"}, "description": "Diagram or widget kind"},
			"source":   map[string]any{"type": "string", "description": "Mermaid definition or self-contained HTML markup"},
			"format":   map[string]any{"type": "string", "enum": []string{"svg", "png", "html"}, "description": "Desired output format (default svg for mermaid, html for html)"},
			"filename": map[string]any{"type": "string", "description": "Optional base filename without extension"},
		},
		"required": []string{"kind", "source"},
	}
}
func (t *imageRenderDiagramTool) Execute(ctx context.Context, runCtx RunContext, rawArgs json.RawMessage) (ToolOutput, error) {
	var args struct {
		Kind     string `json:"kind"`
		Source   string `json:"source"`
		Format   string `json:"format"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ToolOutput{}, fmt.Errorf("invalid arguments: %w", err)
	}
	kind := strings.ToLower(strings.TrimSpace(args.Kind))
	src := strings.TrimSpace(args.Source)
	if src == "" {
		return ToolOutput{}, errors.New("source is required")
	}
	base := strings.TrimSpace(args.Filename)
	if base == "" {
		base = fmt.Sprintf("diagram_%d", time.Now().Unix())
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))

	if kind == "html" {
		saved, err := t.reg.files.SaveGenerated(ctx, runCtx.OwnerID, base+".html", []byte(src))
		if err != nil {
			return ToolOutput{}, err
		}
		ref := fileRecordToRef(saved)
		return ToolOutput{
			Text:           fmt.Sprintf("Saved HTML widget %q (file_id: %s, URL: %s)", saved.Filename, saved.ID, ref.URL),
			Data:           map[string]any{"file_id": saved.ID, "filename": saved.Filename, "url": ref.URL},
			GeneratedFiles: []executor.GeneratedFileRef{ref},
		}, nil
	}

	if kind != "mermaid" {
		return ToolOutput{}, fmt.Errorf("unsupported diagram kind %q", args.Kind)
	}
	format := strings.ToLower(strings.TrimSpace(args.Format))
	if format == "" {
		format = "svg"
	}

	if (format == "svg" || format == "png") {
		if _, err := exec.LookPath("mmdc"); err == nil {
			tmpDir, err := os.MkdirTemp("", "agy-mermaid-*")
			if err != nil {
				return ToolOutput{}, err
			}
			defer os.RemoveAll(tmpDir)
			inPath := filepath.Join(tmpDir, "in.mmd")
			outPath := filepath.Join(tmpDir, "out."+format)
			if err := os.WriteFile(inPath, []byte(src), 0o600); err != nil {
				return ToolOutput{}, err
			}
			cmdCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cmdCtx, "mmdc", "-i", inPath, "-o", outPath)
			if out, runErr := cmd.CombinedOutput(); runErr != nil {
				return ToolOutput{}, fmt.Errorf("mmdc render failed: %w (%s)", runErr, strings.TrimSpace(string(out)))
			}
			rendered, err := os.ReadFile(outPath)
			if err != nil {
				return ToolOutput{}, err
			}
			saved, err := t.reg.files.SaveGenerated(ctx, runCtx.OwnerID, base+"."+format, rendered)
			if err != nil {
				return ToolOutput{}, err
			}
			ref := fileRecordToRef(saved)
			return ToolOutput{
				Text:           fmt.Sprintf("Rendered Mermaid diagram %q (file_id: %s, URL: %s)", saved.Filename, saved.ID, ref.URL),
				Data:           map[string]any{"file_id": saved.ID, "filename": saved.Filename, "url": ref.URL, "format": format},
				GeneratedFiles: []executor.GeneratedFileRef{ref},
			}, nil
		}
	}

	// Fallback: save self-contained HTML page embedding the Mermaid source for browser viewing.
	escaped := html.EscapeString(src)
	page := fmt.Sprintf("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><title>%s</title></head><body><pre class=\"mermaid\">\n%s\n</pre></body></html>\n", html.EscapeString(base), escaped)
	saved, err := t.reg.files.SaveGenerated(ctx, runCtx.OwnerID, base+".html", []byte(page))
	if err != nil {
		return ToolOutput{}, err
	}
	ref := fileRecordToRef(saved)
	return ToolOutput{
		Text:           fmt.Sprintf("Saved Mermaid diagram as HTML %q (file_id: %s, URL: %s; install mmdc for direct SVG/PNG)", saved.Filename, saved.ID, ref.URL),
		Data:           map[string]any{"file_id": saved.ID, "filename": saved.Filename, "url": ref.URL, "format": "html"},
		GeneratedFiles: []executor.GeneratedFileRef{ref},
	}, nil
}

func (r *Registry) resolveOwnedImageBytes(ctx context.Context, ownerID, fileID string) (storage.FileRecord, []byte, error) {
	if r.files == nil {
		return storage.FileRecord{}, nil, errors.New("file store unavailable")
	}
	rec, data, err := r.files.ReadBytes(ctx, ownerID, strings.TrimSpace(fileID), storage.MaxImageFileBytes)
	if err != nil {
		return storage.FileRecord{}, nil, err
	}
	if rec.MediaKind != storage.MediaKindImage {
		return rec, nil, fmt.Errorf("file %s is not an image (media_kind=%q)", rec.ID, rec.MediaKind)
	}
	return rec, data, nil
}

func (r *Registry) resolveOwnedImagePath(ctx context.Context, ownerID, fileID string) (storage.FileRecord, string, error) {
	if r.files == nil {
		return storage.FileRecord{}, "", errors.New("file store unavailable")
	}
	rec, err := r.files.Get(ctx, ownerID, strings.TrimSpace(fileID))
	if err != nil {
		return storage.FileRecord{}, "", err
	}
	if rec.MediaKind != storage.MediaKindImage {
		return rec, "", fmt.Errorf("file %s is not an image (media_kind=%q)", rec.ID, rec.MediaKind)
	}
	p, err := r.files.PathForRecord(rec)
	if err != nil {
		return rec, "", err
	}
	return rec, p, nil
}

func cropImage(src image.Image, x, y, w, h int) image.Image {
	b := src.Bounds()
	x0 := max(b.Min.X, b.Min.X+x)
	y0 := max(b.Min.Y, b.Min.Y+y)
	x1 := min(b.Max.X, x0+w)
	y1 := min(b.Max.Y, y0+h)
	if x1 <= x0 || y1 <= y0 {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
	for yy := y0; yy < y1; yy++ {
		for xx := x0; xx < x1; xx++ {
			dst.Set(xx-x0, yy-y0, src.At(xx, yy))
		}
	}
	return dst
}

func resizeBilinear(src image.Image, dstW, dstH int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	if sw == 0 || sh == 0 || dstW == 0 || dstH == 0 {
		return dst
	}
	for y := 0; y < dstH; y++ {
		sy := (float64(y)+0.5)*float64(sh)/float64(dstH) - 0.5
		y0 := int(sy)
		if y0 < 0 {
			y0 = 0
		}
		y1 := y0 + 1
		if y1 >= sh {
			y1 = sh - 1
		}
		fy := sy - float64(y0)
		if fy < 0 {
			fy = 0
		}
		for x := 0; x < dstW; x++ {
			sx := (float64(x)+0.5)*float64(sw)/float64(dstW) - 0.5
			x0 := int(sx)
			if x0 < 0 {
				x0 = 0
			}
			x1 := x0 + 1
			if x1 >= sw {
				x1 = sw - 1
			}
			fx := sx - float64(x0)
			if fx < 0 {
				fx = 0
			}

			r00, g00, b00, a00 := src.At(sb.Min.X+x0, sb.Min.Y+y0).RGBA()
			r10, g10, b10, a10 := src.At(sb.Min.X+x1, sb.Min.Y+y0).RGBA()
			r01, g01, b01, a01 := src.At(sb.Min.X+x0, sb.Min.Y+y1).RGBA()
			r11, g11, b11, a11 := src.At(sb.Min.X+x1, sb.Min.Y+y1).RGBA()

			blend := func(c00, c10, c01, c11 uint32) uint8 {
				top := float64(c00)*(1-fx) + float64(c10)*fx
				bot := float64(c01)*(1-fx) + float64(c11)*fx
				v := (top*(1-fy) + bot*fy) / 257.0
				if v < 0 {
					return 0
				}
				if v > 255 {
					return 255
				}
				return uint8(v + 0.5)
			}
			off := dst.PixOffset(x, y)
			dst.Pix[off] = blend(r00, r10, r01, r11)
			dst.Pix[off+1] = blend(g00, g10, g01, g11)
			dst.Pix[off+2] = blend(b00, b10, b01, b11)
			dst.Pix[off+3] = blend(a00, a10, a01, a11)
		}
	}
	return dst
}
