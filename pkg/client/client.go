package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is a reference Go SDK client for CLI AI Gateway (Section 5 & Section 20).
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client

	cacheMu   sync.Mutex
	fileCache map[string]string // content sha256 -> file_id
}

// New creates a gateway Client pointing at baseURL (e.g. "http://127.0.0.1:7070").
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Minute},
		fileCache:  make(map[string]string),
	}
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type FileObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

type ResponseObject struct {
	ID                 string   `json:"id"`
	Object             string   `json:"object"`
	CreatedAt          int64    `json:"created_at"`
	Status             string   `json:"status"`
	Model              string   `json:"model"`
	PreviousResponseID *string  `json:"previous_response_id"`
	Output             []Output `json:"output"`
	Usage              Usage    `json:"usage"`
}

type Output struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Status  string       `json:"status"`
	Role    string       `json:"role"`
	Content []OutputPart `json:"content"`
}

type OutputPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

func (r ResponseObject) OutputText() string {
	var b strings.Builder
	for _, out := range r.Output {
		for _, part := range out.Content {
			if part.Type == "output_text" {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}

// ListModels calls GET /v1/models.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, decodeAPIError(resp)
	}
	var out struct {
		Data []Model `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// UploadPrompt uploads a PromptDocument in-memory as prompt.md via POST /v1/files,
// reusing cached file_ids for identical unchanged documents (Section 5.3).
func (c *Client) UploadPrompt(ctx context.Context, filename string, doc PromptDocument) (FileObject, error) {
	if filename == "" {
		filename = "prompt.md"
	}
	digest := doc.Digest()
	c.cacheMu.Lock()
	cachedID := c.fileCache[digest]
	c.cacheMu.Unlock()
	if cachedID != "" {
		if existing, err := c.GetFile(ctx, cachedID); err == nil {
			return existing, nil
		}
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("purpose", "user_data")
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return FileObject{}, err
	}
	if _, err := io.WriteString(fw, doc.RenderMarkdown()); err != nil {
		return FileObject{}, err
	}
	if err := mw.Close(); err != nil {
		return FileObject{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/files", &body)
	if err != nil {
		return FileObject{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return FileObject{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return FileObject{}, decodeAPIError(resp)
	}
	var fileObj FileObject
	if err := json.NewDecoder(resp.Body).Decode(&fileObj); err != nil {
		return FileObject{}, err
	}

	c.cacheMu.Lock()
	c.fileCache[digest] = fileObj.ID
	c.cacheMu.Unlock()
	return fileObj, nil
}

func (c *Client) GetFile(ctx context.Context, fileID string) (FileObject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/files/"+fileID, nil)
	if err != nil {
		return FileObject{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return FileObject{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return FileObject{}, decodeAPIError(resp)
	}
	var fileObj FileObject
	if err := json.NewDecoder(resp.Body).Decode(&fileObj); err != nil {
		return FileObject{}, err
	}
	return fileObj, nil
}

type ResponseParams struct {
	Model              string
	FileIDs            []string
	Text               string
	Instructions       string
	PreviousResponseID string
}

// CreateResponse calls POST /v1/responses synchronously.
func (c *Client) CreateResponse(ctx context.Context, params ResponseParams) (ResponseObject, error) {
	payload := buildResponsePayload(params, false)
	raw, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/responses", bytes.NewReader(raw))
	if err != nil {
		return ResponseObject{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ResponseObject{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ResponseObject{}, decodeAPIError(resp)
	}
	var out ResponseObject
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ResponseObject{}, err
	}
	return out, nil
}

// StreamResponse calls POST /v1/responses with stream=true and emits deltas in real time.
func (c *Client) StreamResponse(ctx context.Context, params ResponseParams, onDelta func(string)) (ResponseObject, error) {
	payload := buildResponsePayload(params, true)
	raw, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/responses", bytes.NewReader(raw))
	if err != nil {
		return ResponseObject{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ResponseObject{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ResponseObject{}, decodeAPIError(resp)
	}

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	var (
		eventName string
		finalResp ResponseObject
	)
	for {
		line, readErr := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "event: ") {
			eventName = strings.TrimPrefix(trimmed, "event: ")
		} else if strings.HasPrefix(trimmed, "data: ") {
			data := strings.TrimPrefix(trimmed, "data: ")
			switch eventName {
			case "response.output_text.delta":
				var ev struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal([]byte(data), &ev) == nil && onDelta != nil {
					onDelta(ev.Delta)
				}
			case "response.completed":
				var ev struct {
					Response ResponseObject `json:"response"`
				}
				if json.Unmarshal([]byte(data), &ev) == nil {
					finalResp = ev.Response
				}
			case "error":
				var ev struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				_ = json.Unmarshal([]byte(data), &ev)
				return ResponseObject{}, fmt.Errorf("stream error (%s): %s", ev.Code, ev.Message)
			}
		} else if trimmed == "" {
			eventName = ""
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return ResponseObject{}, readErr
		}
	}
	return finalResp, nil
}

func buildResponsePayload(params ResponseParams, stream bool) map[string]any {
	content := make([]map[string]any, 0, len(params.FileIDs)+1)
	for _, fid := range params.FileIDs {
		content = append(content, map[string]any{
			"type":    "input_file",
			"file_id": fid,
		})
	}
	if params.Text != "" {
		content = append(content, map[string]any{
			"type": "input_text",
			"text": params.Text,
		})
	}
	payload := map[string]any{
		"model": params.Model,
		"input": []map[string]any{
			{
				"role":    "user",
				"content": content,
			},
		},
		"stream": stream,
	}
	if params.PreviousResponseID != "" {
		payload["previous_response_id"] = params.PreviousResponseID
	}
	if params.Instructions != "" {
		payload["instructions"] = params.Instructions
	}
	return payload
}

func decodeAPIError(resp *http.Response) error {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		return fmt.Errorf("HTTP %d (%s/%s): %s", resp.StatusCode, env.Error.Type, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
