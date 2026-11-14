package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/storage"
)

var (
	ErrResponseNotFound  = errors.New("previous response not found")
	ErrResponseForbidden = errors.New("forbidden access to previous response")
	ErrSessionBusy       = errors.New("session is currently executing another task")
)

// Session represents gateway session metadata stored in SQLite and sessions/<id>/metadata.json.
type Session struct {
	ID                string `json:"gateway_session_id"`
	OwnerID           string `json:"owner_id"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	ProviderSessionID string `json:"provider_session_id"`
	LastResponseID    string `json:"last_response_id"`
	TurnCount         int    `json:"turn_count"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// HistoryEntry is one turn appended to sessions/<id>/history.jsonl.
type HistoryEntry struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ResponseID string `json:"response_id,omitempty"`
	Model      string `json:"model,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Timestamp  int64  `json:"timestamp"`
}

// RunRecord holds run workspace paths and metadata.
type RunRecord struct {
	ID         string
	ResponseID string
	SessionID  string
	OwnerID    string
	Model      string
	Provider   string
	Dir        string
	InputDir   string
	EventsPath string
	ResultPath string
	CreatedAt  int64
}

// Manager coordinates sessions, sequential per-session locking, run workspaces, and filesystem history (Section 8).
type Manager struct {
	db              *storage.DB
	rootDir         string
	sessionsDir     string
	runsDir         string
	lockTimeout     time.Duration
	runTTL          time.Duration
	sessionTTL      time.Duration
	cleanupRunInput bool

	mu      sync.Mutex
	locks   map[string]chan struct{}
	cancels map[string]context.CancelFunc
}

// NewManager creates a session & run manager rooted at rootDir.
func NewManager(db *storage.DB, rootDir string, lockTimeout, runTTL, sessionTTL time.Duration, cleanupRunInput bool) (*Manager, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	sessionsDir := filepath.Join(absRoot, "sessions")
	runsDir := filepath.Join(absRoot, "runs")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return nil, err
	}
	return &Manager{
		db:              db,
		rootDir:         absRoot,
		sessionsDir:     sessionsDir,
		runsDir:         runsDir,
		lockTimeout:     lockTimeout,
		runTTL:          runTTL,
		sessionTTL:      sessionTTL,
		cleanupRunInput: cleanupRunInput,
		locks:           make(map[string]chan struct{}),
		cancels:         make(map[string]context.CancelFunc),
	}, nil
}

// ResolveOrCreateSession either looks up the session owning previousResponseID (verifying ownership)
// or creates a new gateway session.
func (m *Manager) ResolveOrCreateSession(ctx context.Context, ownerID, modelID, provider, previousResponseID string) (Session, bool, error) {
	if previousResponseID == "" {
		now := time.Now().Unix()
		sess := Session{
			ID:        newID("sess"),
			OwnerID:   ownerID,
			Provider:  provider,
			Model:     modelID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if _, err := m.db.SQL().ExecContext(ctx, `
			INSERT INTO sessions (id, owner_id, provider, model, provider_session_id, last_response_id, active_run_id, turn_count, created_at, updated_at)
			VALUES (?, ?, ?, ?, '', '', '', 0, ?, ?)
		`, sess.ID, sess.OwnerID, sess.Provider, sess.Model, sess.CreatedAt, sess.UpdatedAt); err != nil {
			return Session{}, false, fmt.Errorf("create session: %w", err)
		}
		if err := m.writeSessionMetadata(sess); err != nil {
			return Session{}, false, err
		}
		return sess, false, nil
	}

	var respOwner, sessionID string
	err := m.db.SQL().QueryRowContext(ctx, `
		SELECT owner_id, session_id FROM responses WHERE id = ?
	`, previousResponseID).Scan(&respOwner, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, ErrResponseNotFound
	}
	if err != nil {
		return Session{}, false, err
	}
	if respOwner != ownerID {
		return Session{}, false, ErrResponseForbidden
	}

	sess, err := m.GetSession(ctx, sessionID)
	if err != nil {
		return Session{}, false, err
	}
	return sess, true, nil
}

// GetSession retrieves session metadata from SQLite.
func (m *Manager) GetSession(ctx context.Context, sessionID string) (Session, error) {
	var sess Session
	err := m.db.SQL().QueryRowContext(ctx, `
		SELECT id, owner_id, provider, model, provider_session_id, last_response_id, turn_count, created_at, updated_at
		FROM sessions WHERE id = ?
	`, sessionID).Scan(
		&sess.ID, &sess.OwnerID, &sess.Provider, &sess.Model,
		&sess.ProviderSessionID, &sess.LastResponseID, &sess.TurnCount, &sess.CreatedAt, &sess.UpdatedAt,
	)
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// AcquireSessionLock ensures only one state-mutating task runs per session at a time (Section 8.4).
// Parallel tasks across independent sessions never block each other.
func (m *Manager) AcquireSessionLock(ctx context.Context, sessionID string) (func(), error) {
	m.mu.Lock()
	ch, ok := m.locks[sessionID]
	if !ok {
		ch = make(chan struct{}, 1)
		m.locks[sessionID] = ch
	}
	m.mu.Unlock()

	timer := time.NewTimer(m.lockTimeout)
	defer timer.Stop()

	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-ch
			})
		}, nil
	case <-timer.C:
		return nil, ErrSessionBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CreateRun allocates an isolated run directory and records queued/running metadata in SQLite.
func (m *Manager) CreateRun(ctx context.Context, sess Session, previousResponseID, modelID, provider string, inputBytes int64, fileIDs []string) (RunRecord, error) {
	runID := newID("run")
	respID := newID("resp")
	now := time.Now().Unix()

	runDir := filepath.Join(m.runsDir, runID)
	inputDir := filepath.Join(runDir, "input")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		return RunRecord{}, fmt.Errorf("create run input dir: %w", err)
	}

	fileIDsJSON, _ := json.Marshal(fileIDs)

	tx, err := m.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		_ = os.RemoveAll(runDir)
		return RunRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runs (id, owner_id, session_id, response_id, model, provider, status, input_bytes, file_ids_json, created_at, started_at)
		VALUES (?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?)
	`, runID, sess.OwnerID, sess.ID, respID, modelID, provider, inputBytes, string(fileIDsJSON), now, now); err != nil {
		_ = os.RemoveAll(runDir)
		return RunRecord{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO responses (id, owner_id, session_id, previous_response_id, run_id, model, provider, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'in_progress', ?)
	`, respID, sess.OwnerID, sess.ID, previousResponseID, runID, modelID, provider, now); err != nil {
		_ = os.RemoveAll(runDir)
		return RunRecord{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET active_run_id = ?, updated_at = ? WHERE id = ?
	`, runID, now, sess.ID); err != nil {
		_ = os.RemoveAll(runDir)
		return RunRecord{}, err
	}

	if err := tx.Commit(); err != nil {
		_ = os.RemoveAll(runDir)
		return RunRecord{}, err
	}

	return RunRecord{
		ID:         runID,
		ResponseID: respID,
		SessionID:  sess.ID,
		OwnerID:    sess.OwnerID,
		Model:      modelID,
		Provider:   provider,
		Dir:        runDir,
		InputDir:   inputDir,
		EventsPath: filepath.Join(runDir, "events.jsonl"),
		ResultPath: filepath.Join(runDir, "result.json"),
		CreatedAt:  now,
	}, nil
}

// BuildSessionContext determines whether to resume the CLI session directly or restore file history (Section 8.2-8.3).
// When switching providers or when the executor does not support SessionResume, it writes ./input/context.md
// from sessions/<id>/history.jsonl and returns IsResume=false.
func (m *Manager) BuildSessionContext(sess Session, isContinuation bool, targetProvider string, caps executor.Capabilities, run RunRecord) (executor.SessionContext, *executor.ExecuteFile, error) {
	if !isContinuation {
		return executor.SessionContext{
			GatewaySessionID: sess.ID,
			IsResume:         false,
		}, nil, nil
	}

	if sess.Provider == targetProvider && caps.SessionResume && sess.ProviderSessionID != "" {
		return executor.SessionContext{
			GatewaySessionID:  sess.ID,
			ProviderSessionID: sess.ProviderSessionID,
			IsResume:          true,
		}, nil, nil
	}

	// Fallback / provider switch: restore context from filesystem history (Section 8.2 step 5, 8.3).
	history, err := m.LoadHistory(sess.ID)
	if err != nil {
		return executor.SessionContext{}, nil, fmt.Errorf("load session history: %w", err)
	}
	if len(history) == 0 {
		return executor.SessionContext{
			GatewaySessionID: sess.ID,
			IsResume:         false,
		}, nil, nil
	}

	var b strings.Builder
	b.WriteString("# Previous Conversation History\n\n")
	for _, entry := range history {
		roleTitle := strings.ToUpper(entry.Role)
		b.WriteString("## ")
		b.WriteString(roleTitle)
		if entry.Model != "" {
			b.WriteString(" (")
			b.WriteString(entry.Model)
			b.WriteString(")")
		}
		b.WriteString("\n\n")
		b.WriteString(entry.Content)
		b.WriteString("\n\n")
	}

	contextPath := filepath.Join(run.InputDir, "context.md")
	data := []byte(b.String())
	if err := os.WriteFile(contextPath, data, 0o600); err != nil {
		return executor.SessionContext{}, nil, fmt.Errorf("write context.md: %w", err)
	}

	file := &executor.ExecuteFile{
		Name:         "context.md",
		Path:         contextPath,
		RelativePath: "./input/context.md",
		Size:         int64(len(data)),
	}
	return executor.SessionContext{
		GatewaySessionID: sess.ID,
		IsResume:         false,
		HistoryFile:      "./input/context.md",
	}, file, nil
}

// AppendEvent logs one normalized event line to runs/<run_id>/events.jsonl.
func (m *Manager) AppendEvent(run RunRecord, ev executor.Event) {
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(run.EventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// FinishRun persists run results, updates SQLite metadata, appends conversation history on success,
// writes runs/<id>/result.json and sessions/<id>/metadata.json, and optionally removes temp input/.
func (m *Manager) FinishRun(ctx context.Context, sess Session, run RunRecord, userPromptSummary string, res executor.ExecuteResult, duration time.Duration) error {
	now := time.Now().Unix()
	runStatus := res.Status
	if runStatus == "" {
		runStatus = "failed"
	}
	respStatus := "failed"
	switch runStatus {
	case "completed":
		respStatus = "completed"
	case "cancelled":
		respStatus = "cancelled"
	case "timed_out":
		respStatus = "failed"
	}

	var errType, errMsg string
	if res.Error != nil {
		errType = res.Error.Code
		errMsg = res.Error.Message
	}

	resultPayload := map[string]any{
		"run_id":              run.ID,
		"response_id":         run.ResponseID,
		"gateway_session_id":  sess.ID,
		"provider":            run.Provider,
		"model":               run.Model,
		"provider_session_id": res.ProviderSessionID,
		"status":              runStatus,
		"answer":              res.Answer,
		"usage":               res.Usage,
		"duration_ms":         duration.Milliseconds(),
		"exit_code":           res.ExitCode,
		"error_type":          errType,
		"error_message":       errMsg,
		"finished_at":         now,
	}
	if rawResult, err := json.MarshalIndent(resultPayload, "", "  "); err == nil {
		_ = os.WriteFile(run.ResultPath, rawResult, 0o600)
	}

	tx, err := m.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, exit_code = ?, duration_ms = ?, error_type = ?, error_message = ?, finished_at = ?
		WHERE id = ?
	`, runStatus, res.ExitCode, duration.Milliseconds(), errType, errMsg, now, run.ID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE responses
		SET status = ?, input_tokens = ?, output_tokens = ?, total_tokens = ?, error_code = ?, error_message = ?, completed_at = ?
		WHERE id = ?
	`, respStatus, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens, errType, errMsg, now, run.ResponseID); err != nil {
		return err
	}

	if runStatus == "completed" {
		sess.Provider = run.Provider
		sess.Model = run.Model
		if res.ProviderSessionID != "" {
			sess.ProviderSessionID = res.ProviderSessionID
		} else if sess.Provider != run.Provider {
			sess.ProviderSessionID = ""
		}
		sess.LastResponseID = run.ResponseID
		sess.TurnCount++
		sess.UpdatedAt = now

		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET provider = ?, model = ?, provider_session_id = ?, last_response_id = ?, active_run_id = '', turn_count = ?, updated_at = ?
			WHERE id = ?
		`, sess.Provider, sess.Model, sess.ProviderSessionID, sess.LastResponseID, sess.TurnCount, sess.UpdatedAt, sess.ID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET active_run_id = '', updated_at = ? WHERE id = ?
		`, now, sess.ID); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if runStatus == "completed" {
		_ = m.appendHistory(sess.ID, HistoryEntry{
			Role:      "user",
			Content:   userPromptSummary,
			Model:     run.Model,
			Provider:  run.Provider,
			Timestamp: run.CreatedAt,
		})
		_ = m.appendHistory(sess.ID, HistoryEntry{
			Role:       "assistant",
			Content:    res.Answer,
			ResponseID: run.ResponseID,
			Model:      run.Model,
			Provider:   run.Provider,
			Timestamp:  now,
		})
		_ = m.writeSessionMetadata(sess)
		_ = m.writeSessionSummary(sess, userPromptSummary, res.Answer)
	}

	if m.cleanupRunInput {
		_ = os.RemoveAll(run.InputDir)
	}
	return nil
}

// LoadHistory reads history.jsonl from sessions/<id>/history.jsonl.
func (m *Manager) LoadHistory(sessionID string) ([]HistoryEntry, error) {
	path := filepath.Join(m.sessionsDir, sessionID, "history.jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []HistoryEntry
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			var e HistoryEntry
			if jsonErr := json.Unmarshal([]byte(trimmed), &e); jsonErr == nil {
				entries = append(entries, e)
			}
		}
		if err != nil {
			break
		}
	}
	return entries, nil
}

func (m *Manager) appendHistory(sessionID string, entry HistoryEntry) error {
	dir := filepath.Join(m.sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "history.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func (m *Manager) writeSessionMetadata(sess Session) error {
	dir := filepath.Join(m.sessionsDir, sess.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "metadata.json"), raw, 0o600)
}

func (m *Manager) writeSessionSummary(sess Session, userSummary, assistantAnswer string) error {
	dir := filepath.Join(m.sessionsDir, sess.ID)
	summaryPath := filepath.Join(dir, "summary.md")
	snipUser := truncateString(userSummary, 500)
	snipAns := truncateString(assistantAnswer, 500)
	block := fmt.Sprintf("## Turn %d (%s / %s)\n- User: %s\n- Assistant: %s\n\n",
		sess.TurnCount, sess.Provider, sess.Model, snipUser, snipAns)
	f, err := os.OpenFile(summaryPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(block)
	return err
}

// CleanupExpired purges expired runs and idle expired sessions from SQLite and disk (Section 13.3).
func (m *Manager) CleanupExpired(ctx context.Context) (int, error) {
	now := time.Now().Unix()
	removed := 0

	if m.runTTL > 0 {
		cutoff := now - int64(m.runTTL.Seconds())
		rows, err := m.db.SQL().QueryContext(ctx, `
			SELECT id FROM runs WHERE status NOT IN ('queued', 'running') AND created_at < ?
		`, cutoff)
		if err == nil {
			var ids []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			_ = rows.Close()
			for _, id := range ids {
				if _, err := m.db.SQL().ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id); err == nil {
					_ = os.RemoveAll(filepath.Join(m.runsDir, id))
					removed++
				}
			}
		}
	}

	if m.sessionTTL > 0 {
		cutoff := now - int64(m.sessionTTL.Seconds())
		rows, err := m.db.SQL().QueryContext(ctx, `
			SELECT id FROM sessions WHERE active_run_id = '' AND updated_at < ?
		`, cutoff)
		if err == nil {
			var ids []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			_ = rows.Close()
			for _, id := range ids {
				if _, err := m.db.SQL().ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err == nil {
					_ = os.RemoveAll(filepath.Join(m.sessionsDir, id))
					removed++
				}
			}
		}
	}
	return removed, nil
}

// RegisterCancel tracks an active run's cancel function for live Admin termination.
func (m *Manager) RegisterCancel(runID string, cancel context.CancelFunc) {
	if runID == "" || cancel == nil {
		return
	}
	m.mu.Lock()
	m.cancels[runID] = cancel
	m.mu.Unlock()
}

// UnregisterCancel removes a finished run's cancel handle.
func (m *Manager) UnregisterCancel(runID string) {
	m.mu.Lock()
	delete(m.cancels, runID)
	m.mu.Unlock()
}

// CancelRun immediately cancels an active in-process run (sending SIGKILL to its CLI process group).
func (m *Manager) CancelRun(runID string) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[runID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// RunSummary represents stored run metadata for Admin monitoring & control.
type RunSummary struct {
	ID           string   `json:"id"`
	OwnerID      string   `json:"owner_id"`
	SessionID    string   `json:"session_id"`
	ResponseID   string   `json:"response_id"`
	Model        string   `json:"model"`
	Provider     string   `json:"provider"`
	Status       string   `json:"status"`
	InputBytes   int64    `json:"input_bytes"`
	FileIDs      []string `json:"file_ids"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	DurationMS   int64    `json:"duration_ms"`
	ErrorType    string   `json:"error_type,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
	CreatedAt    int64    `json:"created_at"`
	StartedAt    *int64   `json:"started_at,omitempty"`
	FinishedAt   *int64   `json:"finished_at,omitempty"`
	LiveActive   bool     `json:"live_active"`
}

// RunDetail enriches RunSummary with disk result/event tails when available.
type RunDetail struct {
	RunSummary
	ResultJSON string `json:"result_json,omitempty"`
	EventsTail string `json:"events_tail,omitempty"`
}

// ListRuns returns recent runs optionally filtered by status.
func (m *Manager) ListRuns(ctx context.Context, statusFilter string, limit int) ([]RunSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if statusFilter != "" && statusFilter != "all" {
		rows, err = m.db.SQL().QueryContext(ctx, `
			SELECT id, owner_id, session_id, response_id, model, provider, status,
			       input_bytes, file_ids_json, exit_code, duration_ms, error_type, error_message,
			       created_at, started_at, finished_at
			FROM runs WHERE status = ?
			ORDER BY created_at DESC LIMIT ?
		`, statusFilter, limit)
	} else {
		rows, err = m.db.SQL().QueryContext(ctx, `
			SELECT id, owner_id, session_id, response_id, model, provider, status,
			       input_bytes, file_ids_json, exit_code, duration_ms, error_type, error_message,
			       created_at, started_at, finished_at
			FROM runs
			ORDER BY created_at DESC LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m.mu.Lock()
	activeSet := make(map[string]bool, len(m.cancels))
	for id := range m.cancels {
		activeSet[id] = true
	}
	m.mu.Unlock()

	var out []RunSummary
	for rows.Next() {
		rs, scanErr := scanRunRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		rs.LiveActive = activeSet[rs.ID]
		out = append(out, rs)
	}
	return out, rows.Err()
}

// GetRunDetail returns full run metadata plus bounded disk event/result diagnostics.
func (m *Manager) GetRunDetail(ctx context.Context, runID string) (RunDetail, error) {
	if !isSafeStorageID(runID, "run_") {
		return RunDetail{}, sql.ErrNoRows
	}
	row := m.db.SQL().QueryRowContext(ctx, `
		SELECT id, owner_id, session_id, response_id, model, provider, status,
		       input_bytes, file_ids_json, exit_code, duration_ms, error_type, error_message,
		       created_at, started_at, finished_at
		FROM runs WHERE id = ?
	`, runID)
	rs, err := scanRunRow(row)
	if err != nil {
		return RunDetail{}, err
	}
	m.mu.Lock()
	_, rs.LiveActive = m.cancels[runID]
	m.mu.Unlock()

	detail := RunDetail{RunSummary: rs}
	runDir := filepath.Join(m.runsDir, runID)
	if raw, err := readBoundedTail(filepath.Join(runDir, "result.json"), 64*1024); err == nil {
		detail.ResultJSON = raw
	}
	if raw, err := readBoundedTail(filepath.Join(runDir, "events.jsonl"), 64*1024); err == nil {
		detail.EventsTail = raw
	}
	return detail, nil
}

// SessionSummary extends Session metadata with active_run_id visibility for Admin.
type SessionSummary struct {
	Session
	ActiveRunID string `json:"active_run_id,omitempty"`
}

// SessionDetail returns session metadata, history entries, and recent runs.
type SessionDetail struct {
	SessionSummary
	History []HistoryEntry `json:"history"`
	Runs    []RunSummary   `json:"runs"`
}

// ListSessions returns stored sessions ordered by latest update.
func (m *Manager) ListSessions(ctx context.Context, limit int) ([]SessionSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := m.db.SQL().QueryContext(ctx, `
		SELECT id, owner_id, provider, model, provider_session_id, last_response_id, active_run_id, turn_count, created_at, updated_at
		FROM sessions
		ORDER BY updated_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionSummary
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(
			&s.ID, &s.OwnerID, &s.Provider, &s.Model, &s.ProviderSessionID,
			&s.LastResponseID, &s.ActiveRunID, &s.TurnCount, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSessionDetail returns session metadata, stored transcript turns, and associated runs.
func (m *Manager) GetSessionDetail(ctx context.Context, sessionID string) (SessionDetail, error) {
	if !isSafeStorageID(sessionID, "sess_") {
		return SessionDetail{}, sql.ErrNoRows
	}
	var s SessionSummary
	err := m.db.SQL().QueryRowContext(ctx, `
		SELECT id, owner_id, provider, model, provider_session_id, last_response_id, active_run_id, turn_count, created_at, updated_at
		FROM sessions WHERE id = ?
	`, sessionID).Scan(
		&s.ID, &s.OwnerID, &s.Provider, &s.Model, &s.ProviderSessionID,
		&s.LastResponseID, &s.ActiveRunID, &s.TurnCount, &s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return SessionDetail{}, err
	}

	history, err := m.LoadHistory(sessionID)
	if err != nil {
		history = nil
	} else if len(history) > 50 {
		history = history[len(history)-50:]
	}

	rows, err := m.db.SQL().QueryContext(ctx, `
		SELECT id, owner_id, session_id, response_id, model, provider, status,
		       input_bytes, file_ids_json, exit_code, duration_ms, error_type, error_message,
		       created_at, started_at, finished_at
		FROM runs WHERE session_id = ?
		ORDER BY created_at DESC LIMIT 50
	`, sessionID)
	var runs []RunSummary
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			if rs, scanErr := scanRunRow(rows); scanErr == nil {
				runs = append(runs, rs)
			}
		}
	}

	return SessionDetail{
		SessionSummary: s,
		History:        history,
		Runs:           runs,
	}, nil
}

// DeleteSession deletes an idle session and its runs/history from SQLite and filesystem.
func (m *Manager) DeleteSession(ctx context.Context, sessionID string) error {
	if !isSafeStorageID(sessionID, "sess_") {
		return sql.ErrNoRows
	}
	var activeRun string
	err := m.db.SQL().QueryRowContext(ctx, `SELECT active_run_id FROM sessions WHERE id = ?`, sessionID).Scan(&activeRun)
	if err != nil {
		return err
	}
	if activeRun != "" {
		return ErrSessionBusy
	}

	var runCount int
	if err := m.db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM runs WHERE session_id = ? AND status IN ('queued', 'running')`, sessionID).Scan(&runCount); err == nil && runCount > 0 {
		return ErrSessionBusy
	}

	runRows, err := m.db.SQL().QueryContext(ctx, `SELECT id FROM runs WHERE session_id = ?`, sessionID)
	var runIDs []string
	if err == nil {
		for runRows.Next() {
			var rid string
			if runRows.Scan(&rid) == nil {
				runIDs = append(runIDs, rid)
			}
		}
		_ = runRows.Close()
	}

	if _, err := m.db.SQL().ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(m.sessionsDir, sessionID))
	for _, rid := range runIDs {
		if isSafeStorageID(rid, "run_") {
			_ = os.RemoveAll(filepath.Join(m.runsDir, rid))
		}
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRunRow(s rowScanner) (RunSummary, error) {
	var (
		rs          RunSummary
		fileIDsJSON string
		exitCode    sql.NullInt64
		startedAt   sql.NullInt64
		finishedAt  sql.NullInt64
	)
	if err := s.Scan(
		&rs.ID, &rs.OwnerID, &rs.SessionID, &rs.ResponseID, &rs.Model, &rs.Provider, &rs.Status,
		&rs.InputBytes, &fileIDsJSON, &exitCode, &rs.DurationMS, &rs.ErrorType, &rs.ErrorMessage,
		&rs.CreatedAt, &startedAt, &finishedAt,
	); err != nil {
		return RunSummary{}, err
	}
	_ = json.Unmarshal([]byte(fileIDsJSON), &rs.FileIDs)
	if rs.FileIDs == nil {
		rs.FileIDs = []string{}
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		rs.ExitCode = &v
	}
	if startedAt.Valid {
		v := startedAt.Int64
		rs.StartedAt = &v
	}
	if finishedAt.Valid {
		v := finishedAt.Int64
		rs.FinishedAt = &v
	}
	return rs, nil
}

func isSafeStorageID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) || len(id) > 64 {
		return false
	}
	for _, r := range id[len(prefix):] {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func readBoundedTail(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	if size > maxBytes {
		if _, err := f.Seek(size-maxBytes, 0); err != nil {
			return "", err
		}
	}
	buf := make([]byte, min(size, maxBytes))
	n, _ := f.Read(buf)
	if size > maxBytes {
		return "...(truncated)\n" + string(buf[:n]), nil
	}
	return string(buf[:n]), nil
}

func truncateString(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
