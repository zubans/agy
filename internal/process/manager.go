package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrQueueFull is returned when the concurrency wait queue exceeds MaxQueueSize.
	ErrQueueFull = errors.New("execution queue is full")
	// ErrQueueTimeout is returned when waiting for a process slot exceeds QueueWaitTimeout.
	ErrQueueTimeout = errors.New("timed out waiting for execution slot")
	// ErrOutputLimit is returned when stdout or stderr exceeds MaxOutputBytes.
	ErrOutputLimit = errors.New("process output limit exceeded")
)

// Limits configures global, per-executor, and per-user concurrency controls (Section 13).
type Limits struct {
	MaxGlobal        int
	MaxPerExecutor   int
	MaxPerUser       int
	MaxQueueSize     int
	QueueWaitTimeout time.Duration
	DefaultMaxOutput int64
}

// Manager enforces process concurrency, queue bounds, environment isolation, and clean process-group shutdown.
type Manager struct {
	limits Limits

	mu             sync.Mutex
	notify         chan struct{}
	queued         int
	activeGlobal   int
	activeExecutor map[string]int
	activeUser     map[string]int
}

// NewManager creates a process Manager with the given resource limits.
func NewManager(limits Limits) *Manager {
	if limits.MaxGlobal <= 0 {
		limits.MaxGlobal = 16
	}
	if limits.MaxPerExecutor <= 0 {
		limits.MaxPerExecutor = 4
	}
	if limits.MaxPerUser <= 0 {
		limits.MaxPerUser = 4
	}
	if limits.MaxQueueSize <= 0 {
		limits.MaxQueueSize = 32
	}
	if limits.QueueWaitTimeout <= 0 {
		limits.QueueWaitTimeout = 30 * time.Second
	}
	if limits.DefaultMaxOutput <= 0 {
		limits.DefaultMaxOutput = 16 * 1024 * 1024
	}
	return &Manager{
		limits:         limits,
		notify:         make(chan struct{}),
		activeExecutor: make(map[string]int),
		activeUser:     make(map[string]int),
	}
}

// Stats returns a snapshot of current process manager usage.
type Stats struct {
	ActiveGlobal int `json:"active_global"`
	Queued       int `json:"queued"`
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{
		ActiveGlobal: m.activeGlobal,
		Queued:       m.queued,
	}
}

// Lease represents an acquired execution slot that must be released.
type Lease struct {
	manager  *Manager
	executor string
	user     string
	once     sync.Once
}

// Release frees the execution slot and wakes queued waiters.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.manager.release(l.executor, l.user)
	})
}

// Acquire reserves global, per-executor, and per-user capacity, waiting up to QueueWaitTimeout.
// execLimit and userLimit override the defaults when > 0.
func (m *Manager) Acquire(ctx context.Context, executorID, userID string, execLimit, userLimit int) (*Lease, error) {
	if execLimit <= 0 {
		execLimit = m.limits.MaxPerExecutor
	}
	if userLimit <= 0 {
		userLimit = m.limits.MaxPerUser
	}

	waitCtx, cancel := context.WithTimeout(ctx, m.limits.QueueWaitTimeout)
	defer cancel()

	m.mu.Lock()
	if m.canAcquireLocked(executorID, userID, execLimit, userLimit) {
		m.takeLocked(executorID, userID)
		m.mu.Unlock()
		return &Lease{manager: m, executor: executorID, user: userID}, nil
	}
	if m.queued >= m.limits.MaxQueueSize {
		m.mu.Unlock()
		return nil, ErrQueueFull
	}
	m.queued++
	defer func() {
		m.mu.Lock()
		m.queued--
		m.mu.Unlock()
	}()

	for {
		ch := m.notify
		if m.canAcquireLocked(executorID, userID, execLimit, userLimit) {
			m.takeLocked(executorID, userID)
			m.mu.Unlock()
			return &Lease{manager: m, executor: executorID, user: userID}, nil
		}
		m.mu.Unlock()

		select {
		case <-ch:
			m.mu.Lock()
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrQueueTimeout
		}
	}
}

func (m *Manager) canAcquireLocked(executorID, userID string, execLimit, userLimit int) bool {
	if m.activeGlobal >= m.limits.MaxGlobal {
		return false
	}
	if m.activeExecutor[executorID] >= execLimit {
		return false
	}
	if m.activeUser[userID] >= userLimit {
		return false
	}
	return true
}

func (m *Manager) takeLocked(executorID, userID string) {
	m.activeGlobal++
	m.activeExecutor[executorID]++
	m.activeUser[userID]++
}

func (m *Manager) release(executorID, userID string) {
	m.mu.Lock()
	if m.activeGlobal > 0 {
		m.activeGlobal--
	}
	if m.activeExecutor[executorID] > 1 {
		m.activeExecutor[executorID]--
	} else {
		delete(m.activeExecutor, executorID)
	}
	if m.activeUser[userID] > 1 {
		m.activeUser[userID]--
	} else {
		delete(m.activeUser, userID)
	}
	close(m.notify)
	m.notify = make(chan struct{})
	m.mu.Unlock()
}

// RunSpec configures an isolated child process invocation.
type RunSpec struct {
	Executable     string
	Args           []string
	WorkDir        string
	HomeDir        string
	ExtraEnv       map[string]string
	Stdin          []byte
	MaxOutputBytes int64
	OnLine         func(line []byte) error
}

// RunResult holds process completion metadata.
type RunResult struct {
	ExitCode int
	Duration time.Duration
	Stderr   string
}

// RunStream executes a binary directly (never via a shell), isolates env and process group,
// streams stdout line-by-line without fixed line size limits, and terminates the whole process group on cancellation.
func (m *Manager) RunStream(ctx context.Context, spec RunSpec) (RunResult, error) {
	start := time.Now()
	maxOut := spec.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = m.limits.DefaultMaxOutput
	}

	if strings.TrimSpace(spec.Executable) == "" {
		return RunResult{ExitCode: -1}, errors.New("empty executable")
	}

	cmd := exec.Command(spec.Executable, spec.Args...)
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	cmd.Env = BuildSanitizedEnv(spec.HomeDir, spec.WorkDir, spec.ExtraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	} else {
		cmd.Stdin = nil
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{ExitCode: -1}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return RunResult{ExitCode: -1}, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return RunResult{ExitCode: -1, Duration: time.Since(start)}, fmt.Errorf("start %s: %w", filepath.Base(spec.Executable), err)
	}

	pid := cmd.Process.Pid

	// Monitor context cancellation and kill entire process group cleanly.
	doneCh := make(chan struct{})
	var killOnce sync.Once
	killGroup := func() {
		killOnce.Do(func() {
			if pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		})
	}

	go func() {
		select {
		case <-ctx.Done():
			killGroup()
		case <-doneCh:
		}
	}()

	var stderrBuf cappedBuffer
	stderrBuf.max = minInt64(maxOut, 512*1024)
	var errWG sync.WaitGroup
	errWG.Add(1)
	go func() {
		defer errWG.Done()
		_, _ = io.Copy(&stderrBuf, io.LimitReader(stderrPipe, stderrBuf.max+1))
	}()

	stdoutErr := readLinesUnbounded(stdoutPipe, maxOut, func(line []byte) error {
		if spec.OnLine != nil {
			return spec.OnLine(line)
		}
		return nil
	})
	if stdoutErr != nil {
		killGroup()
	}

	errWG.Wait()
	waitErr := cmd.Wait()
	close(doneCh)

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	res := RunResult{
		ExitCode: exitCode,
		Duration: time.Since(start),
		Stderr:   SanitizeOutput(stderrBuf.String()),
	}

	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if stdoutErr != nil {
		return res, stdoutErr
	}
	if waitErr != nil {
		return res, waitErr
	}
	return res, nil
}

// readLinesUnbounded reads newline-delimited lines of arbitrary length up to maxBytes total.
func readLinesUnbounded(r io.Reader, maxBytes int64, fn func([]byte) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var total int64
	var lineBuf []byte

	for {
		frag, err := br.ReadSlice('\n')
		total += int64(len(frag))
		if total > maxBytes {
			return ErrOutputLimit
		}
		if len(frag) > 0 {
			lineBuf = append(lineBuf, frag...)
		}
		if err == nil || errors.Is(err, io.EOF) {
			trimmed := bytes.TrimRight(lineBuf, "\r\n")
			if len(trimmed) > 0 {
				if cbErr := fn(trimmed); cbErr != nil {
					return cbErr
				}
			}
			lineBuf = lineBuf[:0]
			if errors.Is(err, io.EOF) {
				return nil
			}
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

type cappedBuffer struct {
	buf bytes.Buffer
	max int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	rem := c.max - int64(c.buf.Len())
	if rem > 0 {
		if int64(len(p)) > rem {
			_, _ = c.buf.Write(p[:rem])
		} else {
			_, _ = c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// BuildSanitizedEnv creates a minimal environment scoped strictly to one executor.
// Host secrets, gateway API keys, and other executors' credentials are excluded (Section 11.3).
func BuildSanitizedEnv(homeDir, workDir string, extra map[string]string) []string {
	envMap := make(map[string]string)

	for _, key := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if val, ok := os.LookupEnv(key); ok && val != "" {
			envMap[key] = val
		}
	}
	if envMap["PATH"] == "" {
		envMap["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	if workDir != "" {
		tmpDir := filepath.Join(workDir, ".tmp")
		_ = os.MkdirAll(tmpDir, 0o700)
		envMap["TMPDIR"] = tmpDir
	}

	if homeDir != "" {
		envMap["HOME"] = homeDir
	} else if curHome, ok := os.LookupEnv("HOME"); ok && curHome != "" {
		envMap["HOME"] = curHome
	}

	for k, v := range extra {
		if k == "" {
			continue
		}
		envMap[k] = v
	}

	out := make([]string, 0, len(envMap))
	for k, v := range envMap {
		out = append(out, k+"="+v)
	}
	return out
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{12,}`),
	regexp.MustCompile(`AIza[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`ya29\.[A-Za-z0-9._\-]{16,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|oauth[_-]?token|secret|password|authorization)\s*[:=]\s*["']?[^\s"']{6,}`),
}

// SanitizeOutput strips common token/secret patterns before logging or returning errors (Section 12 & 14).
func SanitizeOutput(s string) string {
	out := s
	for i, re := range secretPatterns {
		if i == 0 {
			out = re.ReplaceAllString(out, "${1}[REDACTED]")
		} else {
			out = re.ReplaceAllString(out, "[REDACTED]")
		}
	}
	if len(out) > 2048 {
		out = out[:2048] + "...(truncated)"
	}
	return strings.TrimSpace(out)
}
