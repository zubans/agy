package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BuildFileInstruction creates a short, deterministic prompt directing the CLI to read staged input files
// without placing user-controlled data into CLI flags (Section 7.2, 7.3).
func BuildFileInstruction(req ExecuteRequest) string {
	var b strings.Builder

	hasPromptMD := false
	var extraFiles []string
	for _, f := range req.Files {
		switch f.RelativePath {
		case "./input/prompt.md":
			hasPromptMD = true
		case "./input/context.md":
			// Handled explicitly below so ordering is deterministic.
		default:
			extraFiles = append(extraFiles, f.RelativePath)
		}
	}

	if req.Session.HistoryFile != "" {
		b.WriteString("Read ")
		b.WriteString(req.Session.HistoryFile)
		b.WriteString(" for the prior conversation context.\n")
	}

	if hasPromptMD {
		b.WriteString("Read the file ./input/prompt.md.\n")
		b.WriteString("Follow the task instructions contained in it.\n")
	}

	if len(extraFiles) > 0 {
		b.WriteString("Also read the following workspace input files: ")
		b.WriteString(strings.Join(extraFiles, ", "))
		b.WriteString(".\n")
	}

	if req.Prompt != "" {
		b.WriteString("Additional request note: ")
		b.WriteString(req.Prompt)
		b.WriteString("\n")
	}

	b.WriteString("Return the final answer.")
	return b.String()
}

// CheckBinary verifies that an executable resolves cleanly and is runnable.
func CheckBinary(executable string) error {
	if strings.TrimSpace(executable) == "" {
		return errors.New("executable path is empty")
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return fmt.Errorf("executable %q not found: %w", executable, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat %q: %w", resolved, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory, not an executable", resolved)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%q is not executable", resolved)
	}
	return nil
}

// ClassifyCLIError converts process errors and stderr summaries into typed ExecError values (Section 12).
func ClassifyCLIError(provider string, stderr string, err error, exitCode int) *ExecError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &ExecError{
			Kind:     ErrTimeout,
			Code:     "cli_timeout",
			Message:  fmt.Sprintf("%s execution timed out", provider),
			ExitCode: exitCode,
			Cause:    err,
		}
	}
	if errors.Is(err, context.Canceled) {
		return &ExecError{
			Kind:     ErrCancelled,
			Code:     "request_cancelled",
			Message:  fmt.Sprintf("%s execution cancelled", provider),
			ExitCode: exitCode,
			Cause:    err,
		}
	}
	if errors.Is(err, exec.ErrNotFound) || os.IsNotExist(err) {
		return &ExecError{
			Kind:     ErrUnavailable,
			Code:     "cli_unavailable",
			Message:  fmt.Sprintf("%s CLI binary is unavailable", provider),
			ExitCode: exitCode,
			Cause:    err,
		}
	}

	lower := strings.ToLower(stderr)
	if err != nil {
		lower += " " + strings.ToLower(err.Error())
	}

	if containsAny(lower,
		"unauthenticated", "authentication failed", "unauthorized",
		"not logged in", "login required", "invalid api key",
		"expired token", "auth_failed", "code 401", "re-authenticate",
	) {
		return &ExecError{
			Kind:     ErrAuth,
			Code:     "cli_auth_expired",
			Message:  fmt.Sprintf("%s authentication failed or expired; re-authenticate the CLI", provider),
			ExitCode: exitCode,
			Cause:    err,
		}
	}

	if containsAny(lower,
		"quota", "resource_exhausted", "rate limit", "too many requests", "code 429",
	) {
		return &ExecError{
			Kind:     ErrRateLimit,
			Code:     "cli_rate_limited",
			Message:  fmt.Sprintf("%s rate or quota limit reached", provider),
			ExitCode: exitCode,
			Cause:    err,
		}
	}

	msg := fmt.Sprintf("%s execution failed (exit code %d)", provider, exitCode)
	if trimmed := firstUsefulLine(stderr); trimmed != "" {
		msg = fmt.Sprintf("%s execution failed: %s", provider, trimmed)
	}
	return &ExecError{
		Kind:     ErrExecution,
		Code:     "cli_execution_error",
		Message:  msg,
		ExitCode: exitCode,
		Cause:    err,
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func firstUsefulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 240 {
			line = line[:240] + "..."
		}
		return line
	}
	return ""
}

// ReadWorkDirFile safely reads a file strictly inside workDir, rejecting escapes and symlinks.
func ReadWorkDirFile(workDir, relPath string) ([]byte, error) {
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	cleanRel := filepath.Clean(relPath)
	if filepath.IsAbs(cleanRel) || strings.HasPrefix(cleanRel, "..") {
		return nil, fmt.Errorf("invalid relative path %q", relPath)
	}
	target := filepath.Join(absWork, cleanRel)
	relCheck, err := filepath.Rel(absWork, target)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return nil, fmt.Errorf("path %q escapes workdir", relPath)
	}
	info, err := os.Lstat(target)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symlinks are not allowed in workdir: %q", relPath)
	}
	return os.ReadFile(target)
}
