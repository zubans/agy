package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
)

// RemoteExecutor adapts a containerized worker HTTP endpoint as an executor.Executor.
type RemoteExecutor struct {
	cfg     config.ModelConfig
	baseURL string
	token   string
	caps    executor.Capabilities
	client  *http.Client
}

// NewExecutor constructs a RemoteExecutor pointing at cfg.WorkerURL with Bearer authentication.
func NewExecutor(cfg config.ModelConfig, caps executor.Capabilities, defaultToken string) (*RemoteExecutor, error) {
	rawURL := strings.TrimSpace(cfg.WorkerURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid worker_url %q for model %q", cfg.WorkerURL, cfg.ID)
	}

	token := strings.TrimSpace(cfg.WorkerToken)
	if token == "" {
		token = strings.TrimSpace(defaultToken)
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GATEWAY_WORKER_TOKEN"))
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}

	return &RemoteExecutor{
		cfg:     cfg,
		baseURL: strings.TrimRight(rawURL, "/"),
		token:   token,
		caps:    caps,
		client: &http.Client{
			Transport: transport,
		},
	}, nil
}

func (e *RemoteExecutor) Name() string {
	return e.cfg.Provider
}

func (e *RemoteExecutor) Capabilities() executor.Capabilities {
	return e.caps
}

func (e *RemoteExecutor) Check(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, e.baseURL+HealthPath, nil)
	if err != nil {
		return err
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("worker unreachable at %s: %w", e.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("worker authentication rejected (%d) at %s", resp.StatusCode, e.baseURL)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("worker healthcheck HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var health HealthResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&health); err != nil {
		return fmt.Errorf("invalid worker health JSON: %w", err)
	}
	if !health.Ready {
		if health.Error != "" {
			return fmt.Errorf("worker unavailable: %s", health.Error)
		}
		return fmt.Errorf("worker %s reported not ready", e.baseURL)
	}
	return nil
}

// ListModels queries GET /worker/models on the remote worker container.
func (e *RemoteExecutor) ListModels(ctx context.Context) ([]executor.ModelDescriptor, string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, e.baseURL+ModelsPath, nil)
	if err != nil {
		return nil, "worker", err
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, "worker", fmt.Errorf("worker unreachable at %s: %w", e.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, "worker", fmt.Errorf("worker /worker/models HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded ModelsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&decoded); err != nil {
		return nil, "worker", fmt.Errorf("decode worker models JSON: %w", err)
	}
	var warnErr error
	if strings.TrimSpace(decoded.Warning) != "" {
		warnErr = errors.New(decoded.Warning)
	}
	return decoded.Models, decoded.Source, warnErr
}

func (e *RemoteExecutor) Execute(ctx context.Context, req executor.ExecuteRequest, emit executor.EventHandler) (executor.ExecuteResult, error) {
	meta := buildExecuteMeta(req, e.cfg)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	go writeMultipartRequest(mw, pw, meta, req.Files)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+ExecutePath, pr)
	if err != nil {
		_ = pr.Close()
		execErr := &executor.ExecError{
			Kind:    executor.ErrExecution,
			Code:    "worker_request_build_failed",
			Message: err.Error(),
			Cause:   err,
		}
		return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
	}
	httpReq.Header.Set("Content-Type", contentType)
	if e.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.token)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return mapContextOrTransportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return mapHTTPError(resp)
	}

	return readNDJSONStream(ctx, resp.Body, emit)
}

func buildExecuteMeta(req executor.ExecuteRequest, cfg config.ModelConfig) ExecuteMeta {
	providerModel := req.ProviderModel
	if providerModel == "" {
		providerModel = cfg.Model
	}
	timeoutSec := int64(req.Settings.Timeout.Seconds())
	if timeoutSec <= 0 {
		timeoutSec = int64(cfg.TimeoutSeconds)
	}

	files := make([]FileMeta, 0, len(req.Files))
	for i, f := range req.Files {
		files = append(files, FileMeta{
			ID:           f.ID,
			Name:         f.Name,
			RelativePath: f.RelativePath,
			Size:         f.Size,
			FieldName:    fmt.Sprintf("file_%d", i),
		})
	}

	return ExecuteMeta{
		RunID:         req.RunID,
		Model:         req.Model,
		ProviderModel: providerModel,
		Instructions:  req.Instructions,
		Prompt:        req.Prompt,
		Session: SessionPayload{
			GatewaySessionID:  req.Session.GatewaySessionID,
			ProviderSessionID: req.Session.ProviderSessionID,
			IsResume:          req.Session.IsResume,
			HistoryFile:       req.Session.HistoryFile,
		},
		Files:          files,
		TimeoutSeconds: timeoutSec,
		MaxOutputBytes: req.Settings.MaxOutputBytes,
		Stream:         req.Settings.Stream,
		DiagnosticLog:  req.Settings.DiagnosticLog,
	}
}

func writeMultipartRequest(mw *multipart.Writer, pw *io.PipeWriter, meta ExecuteMeta, files []executor.ExecuteFile) {
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		_ = mw.Close()
		_ = pw.CloseWithError(err)
		return
	}
	metaPart, err := mw.CreateFormField("meta")
	if err != nil {
		_ = mw.Close()
		_ = pw.CloseWithError(err)
		return
	}
	if _, err := metaPart.Write(metaBytes); err != nil {
		_ = mw.Close()
		_ = pw.CloseWithError(err)
		return
	}

	for i, f := range files {
		if err := copyFileToMultipart(mw, meta.Files[i].FieldName, f); err != nil {
			_ = mw.Close()
			_ = pw.CloseWithError(err)
			return
		}
	}

	if err := mw.Close(); err != nil {
		_ = pw.CloseWithError(err)
		return
	}
	_ = pw.Close()
}

func copyFileToMultipart(mw *multipart.Writer, fieldName string, f executor.ExecuteFile) error {
	part, err := mw.CreateFormFile(fieldName, f.Name)
	if err != nil {
		return err
	}
	src, err := os.Open(f.Path)
	if err != nil {
		return fmt.Errorf("open staged file %q: %w", f.RelativePath, err)
	}
	defer func() { _ = src.Close() }()
	if _, err := io.Copy(part, src); err != nil {
		return fmt.Errorf("stream staged file %q: %w", f.RelativePath, err)
	}
	return nil
}

func readNDJSONStream(ctx context.Context, body io.Reader, emit executor.EventHandler) (executor.ExecuteResult, error) {
	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := readLineUnbounded(reader)
		if len(bytes.TrimSpace(line)) > 0 {
			var frame StreamFrame
			if decErr := json.Unmarshal(line, &frame); decErr != nil {
				execErr := &executor.ExecError{
					Kind:    executor.ErrExecution,
					Code:    "invalid_worker_frame",
					Message: fmt.Sprintf("decode worker frame: %v", decErr),
					Cause:   decErr,
				}
				return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
			}
			switch frame.Type {
			case "event":
				if frame.Event != nil && emit != nil {
					if cbErr := emit(*frame.Event); cbErr != nil {
						execErr := &executor.ExecError{
							Kind:    executor.ErrCancelled,
							Code:    "stream_aborted",
							Message: cbErr.Error(),
							Cause:   cbErr,
						}
						return executor.ExecuteResult{Status: "cancelled", ExitCode: -1, Error: execErr}, execErr
					}
				}
			case "done":
				return decodeDoneFrame(frame)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return mapContextOrTransportError(ctx, err)
		}
	}

	if ctx.Err() != nil {
		return mapContextOrTransportError(ctx, ctx.Err())
	}
	execErr := &executor.ExecError{
		Kind:     executor.ErrUnavailable,
		Code:     "worker_stream_eof",
		Message:  "worker connection closed before completion",
		ExitCode: -1,
	}
	return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
}

func decodeDoneFrame(frame StreamFrame) (executor.ExecuteResult, error) {
	if frame.Result == nil {
		msg := frame.Error
		if msg == "" {
			msg = "worker finished without result payload"
		}
		execErr := &executor.ExecError{
			Kind:     executor.ErrExecution,
			Code:     "worker_missing_result",
			Message:  msg,
			ExitCode: -1,
		}
		return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
	}

	res := executor.ExecuteResult{
		Answer:            frame.Result.Answer,
		ProviderSessionID: frame.Result.ProviderSessionID,
		Status:            frame.Result.Status,
		Usage:             frame.Result.Usage,
		ExitCode:          frame.Result.ExitCode,
	}
	if frame.Result.Error != nil {
		res.Error = &executor.ExecError{
			Kind:              frame.Result.Error.Kind,
			Code:              frame.Result.Error.Code,
			Message:           frame.Result.Error.Message,
			ExitCode:          frame.Result.Error.ExitCode,
			RetryAfterSeconds: frame.Result.Error.RetryAfterSeconds,
			CooldownSource:    frame.Result.Error.CooldownSource,
		}
		return res, res.Error
	}
	if frame.Error != "" {
		res.Error = &executor.ExecError{
			Kind:     executor.ErrExecution,
			Code:     "worker_execution_error",
			Message:  frame.Error,
			ExitCode: res.ExitCode,
		}
		return res, res.Error
	}
	return res, nil
}

func readLineUnbounded(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil || !errors.Is(err, bufio.ErrBufferFull) {
			return buf, err
		}
	}
}

func mapHTTPError(resp *http.Response) (executor.ExecuteResult, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = fmt.Sprintf("worker returned HTTP %d", resp.StatusCode)
	}
	kind := executor.ErrExecution
	code := "worker_http_error"
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = executor.ErrAuth
		code = "worker_auth_failed"
	case http.StatusTooManyRequests:
		kind = executor.ErrRateLimit
		code = "worker_rate_limited"
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		kind = executor.ErrUnavailable
		code = "worker_unavailable"
	}
	execErr := &executor.ExecError{
		Kind:     kind,
		Code:     code,
		Message:  msg,
		ExitCode: -1,
	}
	if kind == executor.ErrRateLimit {
		if d, src := executor.ParseCooldown(msg, resp.Header, 0); d > 0 && src == "parsed" {
			execErr.RetryAfterSeconds = int64(math.Ceil(d.Seconds()))
			execErr.CooldownSource = src
		}
	}
	return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
}

func mapContextOrTransportError(ctx context.Context, err error) (executor.ExecuteResult, error) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		execErr := &executor.ExecError{
			Kind:     executor.ErrTimeout,
			Code:     "worker_timeout",
			Message:  "worker execution timed out",
			ExitCode: -1,
			Cause:    ctx.Err(),
		}
		return executor.ExecuteResult{Status: "timeout", ExitCode: -1, Error: execErr}, execErr
	case errors.Is(ctx.Err(), context.Canceled):
		execErr := &executor.ExecError{
			Kind:     executor.ErrCancelled,
			Code:     "worker_cancelled",
			Message:  "worker execution cancelled",
			ExitCode: -1,
			Cause:    ctx.Err(),
		}
		return executor.ExecuteResult{Status: "cancelled", ExitCode: -1, Error: execErr}, execErr
	default:
		execErr := &executor.ExecError{
			Kind:     executor.ErrUnavailable,
			Code:     "worker_transport_error",
			Message:  err.Error(),
			ExitCode: -1,
			Cause:    err,
		}
		return executor.ExecuteResult{Status: "failed", ExitCode: -1, Error: execErr}, execErr
	}
}
