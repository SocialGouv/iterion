package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The far-side block sites bound their bodies on what the LOGGER WRITES
// ([iterlog.BlockPreviewRendered] at the logFarSideBlock choke point): a
// body bounded on its own bytes still reaches the log multiplied by the
// continuation prefix every rendered line carries. Each witness drives a
// body of single-byte lines with a unique tail, then expects the marker,
// the head, no tail, and a buffer well under the body's size — the
// unbounded render of this body is ~7.5x the body, so a mutant that drops
// the bound reddens the size and tail assertions, and a mutant that drops
// the whole body reddens the marker and head assertions.

func boundWitnessBody() string {
	return strings.Repeat("ab\n", 2_000_000) + "ZE_TAIL_9Q\n"
}

func assertBlockBounded(t *testing.T, out string, written, bodyLen int, head string) {
	t.Helper()
	if !strings.Contains(out, "...[truncated]") {
		t.Errorf("the far-side block carries no truncation marker — the bound is gone")
	}
	if !strings.Contains(out, head) {
		t.Errorf("the bound dropped the head of the body instead of previewing it:\n%.200s", out)
	}
	if strings.Contains(out, "ZE_TAIL_9Q") {
		t.Errorf("the tail of a %.1f MiB body reached the run log — the block is unbounded", float64(bodyLen)/(1<<20))
	}
	if written >= bodyLen/2 {
		t.Errorf("the logger wrote %d bytes for a %d-byte body — the rendered-size bound is gone", written, bodyLen)
	}
}

func farSideBoundHooks(level iterlog.Level, logs *strings.Builder) *storeHooks {
	return &storeHooks{
		ctx:     context.Background(),
		emitter: &countingEmitter{},
		runID:   "run-far-side-bound",
		logger:  iterlog.New(level, logs),
		red:     func(s string) string { return s },
	}
}

func TestStoreEventHooks_ResponseTextBlockBoundedOnRenderedSize(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	h := farSideBoundHooks(iterlog.LevelInfo, &logs)

	body := boundWitnessBody()
	h.onLLMStepFinish("n1", LLMStepInfo{Number: 1, Text: body})

	assertBlockBounded(t, logs.String(), logs.Len(), len(body), "│ ab")
}

func TestStoreEventHooks_ToolInputBlocksBoundedOnRenderedSize(t *testing.T) {
	t.Parallel()
	body := boundWitnessBody()
	input, err := json.Marshal(map[string]string{"command": body})
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}

	t.Run("tool input block at info", func(t *testing.T) {
		var logs strings.Builder
		h := farSideBoundHooks(iterlog.LevelInfo, &logs)
		h.onLLMStepFinish("n1", LLMStepInfo{Number: 1, ToolCalls: []ToolCallEntry{{Name: "noop_tool", Input: input}}})
		assertBlockBounded(t, logs.String(), logs.Len(), len(body), "│ ab")
	})
	t.Run("raw input block at debug", func(t *testing.T) {
		var logs strings.Builder
		h := farSideBoundHooks(iterlog.LevelDebug, &logs)
		h.onLLMStepFinish("n1", LLMStepInfo{Number: 1, ToolCalls: []ToolCallEntry{{Name: "noop_tool", Input: input}}})
		assertBlockBounded(t, logs.String(), logs.Len(), len(body), `│ {"command"`)
	})
}

func TestStoreEventHooks_ThinkingBlockBoundedOnRenderedSize(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	h := farSideBoundHooks(iterlog.LevelInfo, &logs)

	body := boundWitnessBody()
	h.onLLMStepFinish("n1", LLMStepInfo{Number: 1, Thinking: body, ReasoningTokens: 3})

	assertBlockBounded(t, logs.String(), logs.Len(), len(body), "│ ab")
}
