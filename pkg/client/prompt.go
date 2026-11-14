package client

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PromptDocument builds deterministic UTF-8 prompt.md payloads according to Section 5.2.
type PromptDocument struct {
	SystemInstructions  string
	ConversationContext string
	Task                string
	AdditionalContext   string
	ExpectedOutput      string
}

// RenderMarkdown renders the prompt in deterministic section order, separating and escaping
// user-provided blocks so embedded headings cannot collide with top-level instruction sections.
func (d PromptDocument) RenderMarkdown() string {
	var b strings.Builder

	writeSection := func(title, body string, untrusted bool) {
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
		trimmed := strings.TrimSpace(body)
		if trimmed == "" {
			b.WriteString("(none)\n\n")
			return
		}
		if untrusted {
			b.WriteString("```markdown\n")
			b.WriteString(escapeFence(trimmed))
			b.WriteString("\n```\n\n")
		} else {
			b.WriteString(trimmed)
			b.WriteString("\n\n")
		}
	}

	writeSection("System Instructions", d.SystemInstructions, false)
	writeSection("Conversation Context", d.ConversationContext, true)
	writeSection("Task", d.Task, true)
	writeSection("Additional Context", d.AdditionalContext, true)
	writeSection("Expected Output", d.ExpectedOutput, false)

	return strings.TrimRight(b.String(), "\n") + "\n"
}

// Digest returns the SHA-256 hex digest of the rendered prompt so clients can reuse unchanged file_ids (Section 5.3).
func (d PromptDocument) Digest() string {
	sum := sha256.Sum256([]byte(d.RenderMarkdown()))
	return hex.EncodeToString(sum[:])
}

func escapeFence(s string) string {
	return strings.ReplaceAll(s, "```", "\\`\\`\\`")
}
