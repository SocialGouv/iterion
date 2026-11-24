package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// #2806 — a retried or resumed attempt of a node re-derives the same
// (iter, turn) coordinate as the attempt before it: the derivation
// (model/hooks.go) knows the STEP, not the attempt, so both attempts of
// run live-feat-budget-resume-1786088951's node `writer` wrote turn 0 of
// iteration 0, and the second write erased the first — with its usage,
// which is how the bench reconciliation saw the hole (54 output tokens
// on the per-step feed, 31 on the checkpoints).
//
// WriteTurn must keep EVERY attempt: the differing previous checkpoint
// is preserved as a superseded record, the latest attempt stays the
// coordinate's canonical record (LoadTurn, LatestTurn, LoadTurnAtIndex —
// the fork anchors — resolve it), and ListTurns enumerates both so the
// baseline the reconciliation sums keeps the earlier attempt's spend.
func TestTurnStore_ARetriedAttemptSupersedesNeverErases(t *testing.T) {
	st, err := New(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	ctx := context.Background()
	runID, nodeID := "run-retry", "writer"

	attempt1 := &TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-a",
		Usage:       TurnUsage{InputTokens: 1200, OutputTokens: 23},
		Messages:    json.RawMessage(`[{"role":"user","content":"draft"}]`),
		MessagesRef: "writer/0/0.messages.json",
	}
	if err := st.WriteTurn(ctx, attempt1); err != nil {
		t.Fatalf("WriteTurn(attempt 1): %v", err)
	}
	// The retried attempt: same coordinate, different content.
	attempt2 := &TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-b",
		Usage: TurnUsage{InputTokens: 2100, OutputTokens: 31},
	}
	if err := st.WriteTurn(ctx, attempt2); err != nil {
		t.Fatalf("WriteTurn(attempt 2): %v", err)
	}

	// The coordinate resolves the LATEST attempt — a fork anchored here
	// replays the last attempt's session, as before.
	got, err := st.LoadTurn(ctx, runID, nodeID, 0, 0)
	if err != nil {
		t.Fatalf("LoadTurn(0,0): %v", err)
	}
	if got.SessionID != "sess-b" || got.Usage.OutputTokens != 31 {
		t.Errorf("LoadTurn(0,0) = session %q, %d output tokens; want the retried attempt (sess-b, 31)",
			got.SessionID, got.Usage.OutputTokens)
	}
	if got.Attempt != 2 || got.Superseded {
		t.Errorf("LoadTurn(0,0) = attempt %d superseded %v; want attempt 2, canonical", got.Attempt, got.Superseded)
	}
	at, err := st.LoadTurnAtIndex(ctx, runID, nodeID, 0)
	if err != nil || at.SessionID != "sess-b" {
		t.Errorf("LoadTurnAtIndex(0) = %+v (%v); want the latest attempt", at, err)
	}

	// Both attempts remain: the earlier one is a trace, not an overwrite.
	rows, err := st.ListTurns(ctx, runID, nodeID, 0)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListTurns returned %d rows, want 2 — the retried attempt erased the first one's checkpoint", len(rows))
	}
	first, latest := rows[0], rows[1]
	if first.Attempt != 1 || !first.Superseded || first.Usage.OutputTokens != 23 || first.SessionID != "sess-a" {
		t.Errorf("rows[0] = attempt %d superseded %v session %q out %d; want attempt 1, superseded, sess-a, 23",
			first.Attempt, first.Superseded, first.SessionID, first.Usage.OutputTokens)
	}
	if latest.Attempt != 2 || latest.Superseded || latest.Usage.OutputTokens != 31 {
		t.Errorf("rows[1] = attempt %d superseded %v out %d; want attempt 2, canonical, 31",
			latest.Attempt, latest.Superseded, latest.Usage.OutputTokens)
	}
	if sum := first.Usage.OutputTokens + latest.Usage.OutputTokens; sum != 54 {
		t.Errorf("the two attempts sum to %d output tokens, want 54 — the feed's own count", sum)
	}

	// The earlier attempt's conversation snapshot moved with its record:
	// the canonical sidecar belongs to the latest attempt, which carried
	// none — serving attempt 1's conversation as attempt 2's would
	// rehydrate a fork with the wrong session's bytes.
	if _, err := st.LoadTurnMessages(ctx, runID, nodeID, 0, 0); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("LoadTurnMessages(0,0) = %v; want ErrTurnNotFound — attempt 1's blob is not attempt 2's", err)
	}

	// An identical redelivery (same attempt, written twice) is still the
	// idempotent overwrite the contract promises — no phantom attempt.
	again := &TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-b",
		Usage: TurnUsage{InputTokens: 2100, OutputTokens: 31},
	}
	if err := st.WriteTurn(ctx, again); err != nil {
		t.Fatalf("WriteTurn(redelivery): %v", err)
	}
	rows, err = st.ListTurns(ctx, runID, nodeID, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("after an identical redelivery ListTurns = %d rows (%v), want 2", len(rows), err)
	}
	if got, err := st.LoadTurn(ctx, runID, nodeID, 0, 0); err != nil || got.Attempt != 2 {
		t.Errorf("after redelivery LoadTurn(0,0).Attempt = %d (%v), want 2", got.Attempt, err)
	}
}

// A THIRD attempt (a resumed run re-executing the node once more) keeps
// numbering monotonic: every superseded record lands on its own slot.
func TestTurnStore_AThirdAttemptStacksBehindTheSecond(t *testing.T) {
	st, err := New(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	ctx := context.Background()
	runID, nodeID := "run-retry3", "writer"
	for i, out := range []int{23, 31, 40} {
		if err := st.WriteTurn(ctx, &TurnCheckpoint{
			RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
			Backend: "claude_code", SessionID: "sess-" + string(rune('a'+i)),
			Usage: TurnUsage{OutputTokens: out},
		}); err != nil {
			t.Fatalf("WriteTurn(attempt %d): %v", i+1, err)
		}
	}
	rows, err := st.ListTurns(ctx, runID, nodeID, 0)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListTurns returned %d rows, want 3", len(rows))
	}
	for i, want := range []struct {
		attempt    int
		superseded bool
		out        int
	}{{1, true, 23}, {2, true, 31}, {3, false, 40}} {
		if rows[i].Attempt != want.attempt || rows[i].Superseded != want.superseded || rows[i].Usage.OutputTokens != want.out {
			t.Errorf("rows[%d] = attempt %d superseded %v out %d; want %d/%v/%d",
				i, rows[i].Attempt, rows[i].Superseded, rows[i].Usage.OutputTokens,
				want.attempt, want.superseded, want.out)
		}
	}
	if latest, err := st.LatestTurn(ctx, runID, nodeID); err != nil || latest.Attempt != 3 || latest.Usage.OutputTokens != 40 {
		t.Errorf("LatestTurn = attempt %d out %d (%v); want attempt 3, 40",
			latest.Attempt, latest.Usage.OutputTokens, err)
	}
}

// The parity pin for the mongo twin's same-attempt rule (revi on #2806):
// an identical checkpoint redelivered WITHOUT its Messages blob is the
// same attempt — the coordinate keeps one record and the conversation
// sidecar survives. The mongo twin reads the same input the same way.
func TestTurnStore_SameAttemptWithoutMessagesKeepsTheBlob(t *testing.T) {
	st, err := New(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	ctx := context.Background()
	runID, nodeID := "run-redelivery", "writer"
	full := &TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-a",
		Usage:    TurnUsage{OutputTokens: 23},
		Messages: json.RawMessage(`[{"role":"user","content":"draft"}]`),
	}
	if err := st.WriteTurn(ctx, full); err != nil {
		t.Fatalf("WriteTurn: %v", err)
	}
	bare := &TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-a",
		Usage: TurnUsage{OutputTokens: 23},
	}
	if err := st.WriteTurn(ctx, bare); err != nil {
		t.Fatalf("WriteTurn(bare redelivery): %v", err)
	}
	rows, err := st.ListTurns(ctx, runID, nodeID, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListTurns = %d rows (%v), want 1 — a bare redelivery must not stack an attempt", len(rows), err)
	}
	msgs, err := st.LoadTurnMessages(ctx, runID, nodeID, 0, 0)
	if err != nil || string(msgs) != `[{"role":"user","content":"draft"}]` {
		t.Errorf("LoadTurnMessages = %q (%v), want the first write's blob", msgs, err)
	}
}
