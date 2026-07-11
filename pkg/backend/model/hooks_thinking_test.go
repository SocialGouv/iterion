package model

import (
	"bytes"
	"context"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newThinkingHooks builds store-backed event hooks whose run.log output is
// captured in the returned buffer.
func newThinkingHooks(t *testing.T, runID string) (EventHooks, *bytes.Buffer) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &logBuf)
	return NewStoreEventHooks(context.Background(), st, runID, logger, nil), &logBuf
}

// TestStoreEventHooks_ThinkingFoldsInRunLog proves a step's extended-thinking
// text reaches run.log as a foldable 🧠 LogBlock (header + "│ "-indented
// body, the shape the studio's LogBlockRow collapses). The text is log-only
// by design — events.jsonl stays bounded to small payloads.
func TestStoreEventHooks_ThinkingFoldsInRunLog(t *testing.T) {
	hooks, logBuf := newThinkingHooks(t, "run-thinking")

	const thinking = "Let me reason about this carefully."
	hooks.OnLLMStepFinish("n1", LLMStepInfo{
		Number:          1,
		Thinking:        thinking,
		ReasoningTokens: 7,
		ThinkingMs:      120,
	})

	out := logBuf.String()
	if !strings.Contains(out, "🧠") || !strings.Contains(out, "thinking step 1 (~7 tok, 120ms):") {
		t.Errorf("run.log missing 🧠 thinking header:\n%s", out)
	}
	if !strings.Contains(out, "│ "+thinking) {
		t.Errorf("run.log missing folded thinking body (block-indent continuation):\n%s", out)
	}
}

// Without thinking text, the metrics-only 🧠 line must remain (fallback for
// backends that report counts but not content).
func TestStoreEventHooks_ThinkingMetricsOnlyFallback(t *testing.T) {
	hooks, logBuf := newThinkingHooks(t, "run-thinking-metrics")

	hooks.OnLLMStepFinish("n1", LLMStepInfo{Number: 2, ReasoningTokens: 9, ThinkingMs: 50})

	out := logBuf.String()
	if !strings.Contains(out, "step 2 thinking: ~9 tok, 50ms") {
		t.Errorf("run.log missing metrics-only thinking line:\n%s", out)
	}
	if strings.Contains(out, "│ ") {
		t.Errorf("unexpected folded block body without thinking text:\n%s", out)
	}
}
