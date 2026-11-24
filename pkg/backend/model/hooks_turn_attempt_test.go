package model

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2806 — the ticket's race driven through the real hook path:
// NewStoreEventHooks on a filesystem store, two delegate calls of one
// node (a first attempt that died on budget_exceeded, then the resumed
// run's re-execution), each firing its turn capture with Step: 1 — the
// executor passes Step: 1 for every delegate call
// (executor.go), so both attempts land on the same
// (LoopIter, TurnIndex). The store must keep both checkpoints: the
// usage baseline sums 23 + 31, and the coordinate's canonical record is
// the LAST attempt — the one a fork resumes.
func TestARetriedAttemptKeepsItsOwnTurnCheckpoint(t *testing.T) {
	t.Parallel()
	const runID = "run-retry-turn"
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.Nop(), nil, nil)
	if hooks.OnLLMTurnCapture == nil {
		t.Fatal("no OnLLMTurnCapture hook: fork anchoring has nothing to persist")
	}

	// Attempt 1, then the retried attempt — same step, same iteration,
	// different session and usage, exactly as the ticket's run recorded.
	hooks.OnLLMTurnCapture("writer", LLMTurnCaptureInfo{
		Step: 1, Backend: "claude_code", SessionID: "sess-a",
		Text: "a first draft", FinishReason: "stop",
		InputTokens: 1200, OutputTokens: 23,
	})
	hooks.OnLLMTurnCapture("writer", LLMTurnCaptureInfo{
		Step: 1, Backend: "claude_code", SessionID: "sess-b",
		Text: "the resumed draft", FinishReason: "stop",
		InputTokens: 2100, OutputTokens: 31,
	})

	ts := store.AsTurnStore(st)
	rows, err := ts.ListTurns(ctx, runID, "writer", 0)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListTurns = %d rows, want 2: the retried attempt overwrote the first attempt's checkpoint", len(rows))
	}
	if rows[0].Usage.OutputTokens != 23 || !rows[0].Superseded {
		t.Errorf("rows[0] = %+v; want the first attempt (23 output tokens), superseded", rows[0].Usage)
	}
	if rows[1].Usage.OutputTokens != 31 || rows[1].Superseded {
		t.Errorf("rows[1] = %+v; want the retried attempt (31 output tokens), canonical", rows[1].Usage)
	}

	// The fork anchor: turn 0 of iteration 0 resumes the LAST attempt's
	// session — and the record says an earlier attempt exists.
	turn, err := ts.LoadTurn(ctx, runID, "writer", 0, 0)
	if err != nil {
		t.Fatalf("LoadTurn: %v", err)
	}
	if turn.SessionID != "sess-b" || turn.Attempt != 2 {
		t.Errorf("the fork anchor = session %q attempt %d; want sess-b, attempt 2", turn.SessionID, turn.Attempt)
	}
}
