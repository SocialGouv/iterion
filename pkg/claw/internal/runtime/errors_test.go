package runtime

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"os"
	"testing"
)

func TestSessionError_Kind(t *testing.T) {
	tests := []struct {
		name string
		err  *SessionError
		want SessionErrorKind
	}{
		{
			name: "IO error",
			err:  NewSessionIOError("read", "/tmp/session.json", os.ErrNotExist),
			want: SessionErrIO,
		},
		{
			name: "JSON error",
			err:  NewSessionJSONError("decode", fmt.Errorf("unexpected EOF")),
			want: SessionErrJSON,
		},
		{
			name: "InvalidFormat error",
			err:  NewSessionFormatError("missing version field"),
			want: SessionErrInvalidFormat,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", tt.err.Kind, tt.want)
			}
		})
	}
}

func TestSessionError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *SessionError
		want string
	}{
		{
			name: "with path",
			err:  NewSessionIOError("read", "/tmp/sess.json", os.ErrNotExist),
			want: "session read [/tmp/sess.json]: file does not exist",
		},
		{
			name: "without path",
			err:  NewSessionJSONError("decode", fmt.Errorf("unexpected EOF")),
			want: "session decode: unexpected EOF",
		},
		{
			name: "format error",
			err:  NewSessionFormatError("missing version field"),
			want: "session parse: missing version field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionError_Unwrap(t *testing.T) {
	cause := os.ErrPermission
	err := NewSessionIOError("write", "/tmp/sess.json", cause)

	if !errors.Is(err, os.ErrPermission) {
		t.Error("errors.Is should find os.ErrPermission in chain")
	}

	// Format error has no cause
	fmtErr := NewSessionFormatError("bad format")
	if fmtErr.Unwrap() != nil {
		t.Error("format error Unwrap should return nil")
	}
}

func TestSessionError_ErrorsAs(t *testing.T) {
	cause := fmt.Errorf("disk full")
	err := NewSessionIOError("write", "/data/session.json", cause)

	// Wrap it further
	wrapped := fmt.Errorf("operation failed: %w", err)

	var sessErr *SessionError
	if !errors.As(wrapped, &sessErr) {
		t.Fatal("errors.As should extract *SessionError from wrapped error")
	}
	if sessErr.Kind != SessionErrIO {
		t.Errorf("Kind = %v, want %v", sessErr.Kind, SessionErrIO)
	}
	if sessErr.Path != "/data/session.json" {
		t.Errorf("Path = %q, want %q", sessErr.Path, "/data/session.json")
	}
}

func TestSession_Fork(t *testing.T) {
	parent := NewSession()
	parent.Messages = []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hello"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
	}
	parent.PromptHistory = []PromptHistoryEntry{
		{TimestampMs: 1000, Text: "hello"},
	}
	parent.CompactionSummary = "compacted context"
	parent.CompactionCount = 2
	parent.TotalInputTokens = 100
	parent.TotalOutputTokens = 50
	parent.TotalTurns = 3

	child := parent.ForkSession("feature-branch")

	// New session ID.
	if child.ID == parent.ID {
		t.Error("fork should produce new session ID")
	}

	// Fork metadata.
	if child.Fork == nil {
		t.Fatal("fork metadata should be set")
	}
	if child.Fork.ParentSessionID != parent.ID {
		t.Errorf("ParentSessionID = %q, want %q", child.Fork.ParentSessionID, parent.ID)
	}
	if child.Fork.BranchName != "feature-branch" {
		t.Errorf("BranchName = %q, want %q", child.Fork.BranchName, "feature-branch")
	}

	// Messages deep-copied (mutate child, verify parent unchanged).
	if len(child.Messages) != 2 {
		t.Fatalf("child should have 2 messages, got %d", len(child.Messages))
	}
	child.Messages[0].Content[0].Text = "mutated"
	if parent.Messages[0].Content[0].Text != "hello" {
		t.Error("mutating child messages should not affect parent")
	}

	// PromptHistory deep-copied.
	if len(child.PromptHistory) != 1 {
		t.Fatalf("child should have 1 prompt history entry, got %d", len(child.PromptHistory))
	}
	child.PromptHistory[0].Text = "changed"
	if parent.PromptHistory[0].Text != "hello" {
		t.Error("mutating child prompt history should not affect parent")
	}

	// State copied.
	if child.CompactionSummary != "compacted context" {
		t.Error("compaction summary not copied")
	}
	if child.TotalInputTokens != 100 {
		t.Error("input tokens not copied")
	}
}

func TestSession_Fork_EmptyBranch(t *testing.T) {
	parent := NewSession()
	child := parent.ForkSession("")
	if child.Fork.BranchName != "" {
		t.Errorf("BranchName should be empty, got %q", child.Fork.BranchName)
	}
	if child.Fork.ParentSessionID != parent.ID {
		t.Error("ParentSessionID should be set even with empty branch name")
	}
}
