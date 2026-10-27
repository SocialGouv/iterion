package delegate

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestHandleAssistantMessage_ThinkingFoldsInRunLog proves a streamed
// ThinkingBlock's content lands in run.log as a foldable 🧠 LogBlock
// (header with metrics + "│ "-indented body — the shape the studio's
// LogBlockRow collapses), and that the best-effort metrics still
// accumulate on sessionMeta.
func TestHandleAssistantMessage_ThinkingFoldsInRunLog(t *testing.T) {
	var logBuf bytes.Buffer
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelInfo, &logBuf)}

	const thinking = "Let me reason about this carefully."
	m := &claudesdk.AssistantMessage{
		Message: &claudesdk.APIMessage{
			Content: []claudesdk.ContentBlock{
				&claudesdk.ThinkingBlock{Type: "thinking", Thinking: thinking},
			},
		},
	}

	meta := sessionMeta{}
	var lastText string
	err := b.handleAssistantMessage(m, Task{NodeID: "n1"}, map[string]string{},
		&meta, &lastText, time.Now(), func() {})
	if err != nil {
		t.Fatalf("handleAssistantMessage: %v", err)
	}

	out := logBuf.String()
	if !strings.Contains(out, "🧠") || !strings.Contains(out, "thinking ~") {
		t.Errorf("run.log missing 🧠 thinking header:\n%s", out)
	}
	if !strings.Contains(out, "│ "+thinking) {
		t.Errorf("run.log missing folded thinking body (block-indent continuation):\n%s", out)
	}
	if meta.thinkingTokens <= 0 {
		t.Errorf("meta.thinkingTokens = %d, want > 0", meta.thinkingTokens)
	}
}

// TestHandleAssistantMessage_ThinkingBlockBoundedOnRenderedSize proves the
// thinking block is bounded on what the logger WRITES: the model chooses
// where its newlines fall, so a body bounded on its own bytes still reaches
// the run log multiplied by the continuation prefix every rendered line
// carries. The witness body is single-byte lines with a unique tail.
func TestHandleAssistantMessage_ThinkingBlockBoundedOnRenderedSize(t *testing.T) {
	var logBuf bytes.Buffer
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelInfo, &logBuf)}

	thinking := strings.Repeat("ab\n", 2_000_000) + "ZE_TAIL_9Q\n"
	m := &claudesdk.AssistantMessage{
		Message: &claudesdk.APIMessage{
			Content: []claudesdk.ContentBlock{
				&claudesdk.ThinkingBlock{Type: "thinking", Thinking: thinking},
			},
		},
	}

	meta := sessionMeta{}
	var lastText string
	err := b.handleAssistantMessage(m, Task{NodeID: "n1"}, map[string]string{},
		&meta, &lastText, time.Now(), func() {})
	if err != nil {
		t.Fatalf("handleAssistantMessage: %v", err)
	}

	out := logBuf.String()
	if !strings.Contains(out, "...[truncated]") {
		t.Errorf("the thinking block carries no truncation marker — the bound is gone")
	}
	if !strings.Contains(out, "│ ab") {
		t.Errorf("the bound dropped the head of the thinking text instead of previewing it:\n%.200s", out)
	}
	if strings.Contains(out, "ZE_TAIL_9Q") {
		t.Errorf("the tail of a 6 MiB thinking body reached the run log — the block is unbounded")
	}
	if logBuf.Len() >= len(thinking)/2 {
		t.Errorf("the logger wrote %d bytes for a %d-byte thinking body — the rendered-size bound is gone", logBuf.Len(), len(thinking))
	}
	if meta.thinkingTokens <= 0 {
		t.Errorf("meta.thinkingTokens = %d, want > 0 (metrics are not log output)", meta.thinkingTokens)
	}
}

// TestHandleAssistantMessage_ThinkingBlockGateAsksNoPass proves the thinking
// block asks the sink before paying: a log that drops the block (level below
// info) must not run the task's redaction pass over its text — the rule
// storeHooks.logFarSideBlock owns on the claw side.
func TestHandleAssistantMessage_ThinkingBlockGateAsksNoPass(t *testing.T) {
	var logBuf bytes.Buffer
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &logBuf)}

	passes := 0
	task := Task{NodeID: "n1", RedactSecrets: func(s string) string {
		passes++
		return s
	}}
	m := &claudesdk.AssistantMessage{
		Message: &claudesdk.APIMessage{
			Content: []claudesdk.ContentBlock{
				&claudesdk.ThinkingBlock{Type: "thinking", Thinking: "Let me reason about this carefully."},
			},
		},
	}

	meta := sessionMeta{}
	var lastText string
	err := b.handleAssistantMessage(m, task, map[string]string{},
		&meta, &lastText, time.Now(), func() {})
	if err != nil {
		t.Fatalf("handleAssistantMessage: %v", err)
	}

	if out := logBuf.String(); strings.Contains(out, "🧠") {
		t.Errorf("a run log at level error was given the thinking block:\n%s", out)
	}
	if passes != 0 {
		t.Errorf("a log that drops the thinking block still paid %d secret pass(es) over its text", passes)
	}
}

// claudeCodeThinkingDisplay defaults to summarized; the env var overrides
// and "off" suppresses the flag for CLIs that predate it.
func TestClaudeCodeThinkingDisplay(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_THINKING_DISPLAY", "")
	if got := claudeCodeThinkingDisplay(); got != "summarized" {
		t.Errorf("default = %q, want summarized", got)
	}
	t.Setenv("ITERION_CLAUDE_CODE_THINKING_DISPLAY", "omitted")
	if got := claudeCodeThinkingDisplay(); got != "omitted" {
		t.Errorf("omitted override = %q, want omitted", got)
	}
	t.Setenv("ITERION_CLAUDE_CODE_THINKING_DISPLAY", "off")
	if got := claudeCodeThinkingDisplay(); got != "" {
		t.Errorf("off kill-switch = %q, want empty (flag suppressed)", got)
	}
}

// TestHandleAssistantMessage_RedactedThinkingSurfacesTiming: some models
// redact thinking client-side (claude-opus-4-8 streams the block with empty
// text + encrypted signature) when --thinking-display can't be applied.
// The signed-but-empty block must still surface a timing line instead of
// vanishing without a trace.
func TestHandleAssistantMessage_RedactedThinkingSurfacesTiming(t *testing.T) {
	var logBuf bytes.Buffer
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelInfo, &logBuf)}

	m := &claudesdk.AssistantMessage{
		Message: &claudesdk.APIMessage{
			Content: []claudesdk.ContentBlock{
				&claudesdk.ThinkingBlock{Type: "thinking", Thinking: "", Signature: "EpwICokB-opaque"},
			},
		},
	}

	meta := sessionMeta{}
	var lastText string
	err := b.handleAssistantMessage(m, Task{NodeID: "n1"}, map[string]string{},
		&meta, &lastText, time.Now().Add(-100*time.Millisecond), func() {})
	if err != nil {
		t.Fatalf("handleAssistantMessage: %v", err)
	}

	out := logBuf.String()
	if !strings.Contains(out, "🧠") || !strings.Contains(out, "content withheld by provider") {
		t.Errorf("run.log missing redacted-thinking timing line:\n%s", out)
	}
	if meta.thinkingMs < 100 {
		t.Errorf("meta.thinkingMs = %d, want >= 100 (gap since previous stream item)", meta.thinkingMs)
	}
	if meta.thinkingTokens != 0 {
		t.Errorf("meta.thinkingTokens = %d, want 0 (no content to count)", meta.thinkingTokens)
	}
}
