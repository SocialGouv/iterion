package model

import (
	"bytes"
	"context"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestStoreEventHooks_ThinkingFoldsInRunLog proves a step's extended-thinking
// text reaches both sinks: run.log as a foldable 🧠 LogBlock (header +
// "│ "-indented body, the shape the studio's LogBlockRow collapses), and the
// llm_step_finished event as data["thinking"].
func TestStoreEventHooks_ThinkingFoldsInRunLog(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-thinking"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &logBuf)
	hooks := NewStoreEventHooks(ctx, st, runID, logger, nil)

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

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	found := false
	for _, ev := range evts {
		if ev.Type == store.EventLLMStepFinished {
			found = true
			if got, _ := ev.Data["thinking"].(string); got != thinking {
				t.Errorf("event data[thinking] = %q, want %q", got, thinking)
			}
		}
	}
	if !found {
		t.Fatal("no llm_step_finished event persisted")
	}
}

// Without thinking text, the metrics-only 🧠 line must remain (fallback for
// backends that report counts but not content).
func TestStoreEventHooks_ThinkingMetricsOnlyFallback(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-thinking-metrics"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &logBuf)
	hooks := NewStoreEventHooks(ctx, st, runID, logger, nil)

	hooks.OnLLMStepFinish("n1", LLMStepInfo{Number: 2, ReasoningTokens: 9, ThinkingMs: 50})

	out := logBuf.String()
	if !strings.Contains(out, "step 2 thinking: ~9 tok, 50ms") {
		t.Errorf("run.log missing metrics-only thinking line:\n%s", out)
	}
	if strings.Contains(out, "│ ") {
		t.Errorf("unexpected folded block body without thinking text:\n%s", out)
	}
}
