package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zendev-sh/goai/provider"
)

// charsPerToken is a rough heuristic for token estimation (~4 chars/token for
// English text). Can be off by 2x for code-heavy content, but only drives
// compaction thresholds, not billing.
const charsPerToken = 4

// Session holds a conversation's message history and compaction state.
type Session struct {
	ID        string             `json:"id"`
	Messages  []provider.Message `json:"messages"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`

	// Compaction state.
	CompactionSummary string `json:"compaction_summary,omitempty"`
	CompactionCount   int    `json:"compaction_count,omitempty"`

	// Cumulative usage.
	TotalUsage provider.Usage `json:"total_usage,omitempty"`
	TotalTurns int            `json:"total_turns,omitempty"`
}

// newSession creates a new session with a unique timestamp-based ID.
func newSession() *Session {
	now := time.Now()
	var suffix [4]byte
	rand.Read(suffix[:])
	return &Session{
		ID:        fmt.Sprintf("session-%d-%s", now.UnixNano(), hex.EncodeToString(suffix[:])),
		Messages:  []provider.Message{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// save persists the session to disk as JSON.
func (s *Session) save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("agent: create session dir: %w", err)
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("agent: marshal session: %w", err)
	}
	path := filepath.Join(dir, s.ID+".json")
	return os.WriteFile(path, data, 0o644)
}

// loadSession loads a session from disk by ID.
func loadSession(dir, id string) (*Session, error) {
	path := filepath.Join(dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: read session: %w", err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("agent: unmarshal session: %w", err)
	}
	return &s, nil
}

// estimateTokens roughly estimates the token count of a message slice.
func estimateTokens(messages []provider.Message) int {
	var total int
	for _, msg := range messages {
		for _, part := range msg.Content {
			total += len(part.Text) / charsPerToken
			total += len(part.ToolOutput) / charsPerToken
			total += len(part.ToolInput) / charsPerToken
		}
	}
	return total
}

// shouldCompact returns true when the session should be compacted.
func shouldCompact(inputTokens int, messages []provider.Message, maxTokens int, threshold float64) bool {
	if maxTokens <= 0 {
		return false
	}
	if inputTokens <= 0 {
		inputTokens = estimateTokens(messages)
	}
	limit := int(float64(maxTokens) * threshold)
	return inputTokens >= limit
}

// compact summarizes the session history using the model, then trims messages.
func compact(ctx context.Context, model provider.LanguageModel, session *Session, keepRecent int) error {
	if len(session.Messages) == 0 {
		return nil
	}

	transcript := buildTranscript(session.Messages)

	params := provider.GenerateParams{
		System: "You are a conversation summarizer. Provide a concise but thorough summary of the conversation. " +
			"Preserve all important technical details: file paths modified, commands run, " +
			"decisions made, errors encountered, and the current state of any ongoing work. " +
			"The summary will replace the conversation history and must be self-contained.",
		Messages: []provider.Message{
			{
				Role: provider.RoleUser,
				Content: []provider.Part{
					{Type: provider.PartText, Text: transcript},
				},
			},
		},
		MaxOutputTokens: 2048,
	}

	result, err := model.DoGenerate(ctx, params)
	if err != nil {
		return fmt.Errorf("agent: compact generate: %w", err)
	}

	summary := result.Text

	// Retain the most recent N messages.
	if keepRecent > len(session.Messages) {
		keepRecent = len(session.Messages)
	}
	recent := make([]provider.Message, keepRecent)
	copy(recent, session.Messages[len(session.Messages)-keepRecent:])

	session.CompactionSummary = summary
	session.CompactionCount++
	session.Messages = recent

	return nil
}

// buildTranscript constructs a plain-text transcript for summarization.
func buildTranscript(messages []provider.Message) string {
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation:\n\n")
	sb.WriteString("---CONVERSATION---\n")

	for _, msg := range messages {
		fmt.Fprintf(&sb, "\n[%s]:\n", strings.ToUpper(string(msg.Role)))
		for _, part := range msg.Content {
			switch part.Type {
			case provider.PartText:
				text := part.Text
				if len(text) > 1000 {
					text = text[:1000] + "... [truncated]"
				}
				sb.WriteString(text)
				sb.WriteString("\n")
			case provider.PartToolCall:
				fmt.Fprintf(&sb, "[Tool call: %s]\n", part.ToolName)
			case provider.PartToolResult:
				text := part.ToolOutput
				if len(text) > 300 {
					text = text[:300] + "... (truncated)"
				}
				fmt.Fprintf(&sb, "[Tool result: %s]\n", text)
			}
		}
	}
	sb.WriteString("\n---END CONVERSATION---\n")
	return sb.String()
}

// continuationMessage creates a synthetic user message announcing compaction.
func continuationMessage(summary string) provider.Message {
	text := fmt.Sprintf(
		"[System: Conversation history was automatically compacted to stay within context limits.\n\nSummary of prior context:\n%s\n\nContinuing from here.]",
		summary,
	)
	return provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Part{
			{Type: provider.PartText, Text: text},
		},
	}
}
