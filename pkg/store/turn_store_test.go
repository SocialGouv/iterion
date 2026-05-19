package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestTurnStore_WriteLoadList exercises the per-turn store on the
// filesystem backend: write three turns under one (node, iter),
// confirm LoadTurn returns each by exact coordinates, ListTurns
// preserves ascending TurnIndex order, and LatestTurn picks the
// highest-indexed entry.
func TestTurnStore_WriteLoadList(t *testing.T) {
	st, err := New(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	runID, nodeID := "run-turn", "agent_a"
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		tc := &TurnCheckpoint{
			RunID:        runID,
			NodeID:       nodeID,
			LoopIter:     0,
			TurnIndex:    i,
			Backend:      "claw",
			FinishReason: "tool_use",
			Messages:     json.RawMessage(`[]`),
			MessagesRef:  "ref",
			WrittenAt:    now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := st.WriteTurn(context.Background(), tc); err != nil {
			t.Fatalf("write turn %d: %v", i, err)
		}
	}

	for i := 0; i < 3; i++ {
		got, err := st.LoadTurn(context.Background(), runID, nodeID, 0, i)
		if err != nil {
			t.Fatalf("LoadTurn(%d): %v", i, err)
		}
		if got.TurnIndex != i {
			t.Errorf("LoadTurn(%d).TurnIndex = %d", i, got.TurnIndex)
		}
		if got.Backend != "claw" {
			t.Errorf("Backend = %q, want claw", got.Backend)
		}
	}

	rows, err := st.ListTurns(context.Background(), runID, nodeID, 0)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListTurns returned %d rows, want 3", len(rows))
	}
	for i, r := range rows {
		if r.TurnIndex != i {
			t.Errorf("rows[%d].TurnIndex = %d", i, r.TurnIndex)
		}
	}

	latest, err := st.LatestTurn(context.Background(), runID, nodeID)
	if err != nil {
		t.Fatalf("LatestTurn: %v", err)
	}
	if latest.TurnIndex != 2 {
		t.Errorf("LatestTurn.TurnIndex = %d, want 2", latest.TurnIndex)
	}

	// LoadTurnMessages reads the sibling blob persisted alongside
	// the TurnCheckpoint.
	body, err := st.LoadTurnMessages(context.Background(), runID, nodeID, 0, 0)
	if err != nil {
		t.Fatalf("LoadTurnMessages: %v", err)
	}
	if string(body) != "[]" {
		t.Errorf("LoadTurnMessages body = %q, want []", body)
	}
}

// TestTurnStore_NotFound returns ErrTurnNotFound for missing
// turns / nodes — the Fork path relies on this typed signal.
func TestTurnStore_NotFound(t *testing.T) {
	st, err := New(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	_, err = st.LoadTurn(context.Background(), "missing", "node", 0, 0)
	if !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("LoadTurn missing → %v, want ErrTurnNotFound", err)
	}
	_, err = st.LatestTurn(context.Background(), "missing", "node")
	if !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("LatestTurn missing → %v, want ErrTurnNotFound", err)
	}
}

// TestTurnStore_Index confirms the per-node index.json is maintained
// so LatestTurn doesn't need to scan the directory tree on every
// call. We don't exercise the fallback branch directly here — the
// happy path through index.json is what the runtime relies on.
func TestTurnStore_Index(t *testing.T) {
	st, err := New(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	runID, nodeID := "run-idx", "judge_a"
	// Mix two loop iterations to make sure the highest-iter row wins.
	for _, e := range []struct {
		iter, turn int
	}{
		{0, 0}, {0, 1}, {1, 0}, {1, 2}, {1, 1},
	} {
		if err := st.WriteTurn(context.Background(), &TurnCheckpoint{
			RunID:     runID,
			NodeID:    nodeID,
			LoopIter:  e.iter,
			TurnIndex: e.turn,
			Backend:   "claw",
			WrittenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("write turn iter=%d turn=%d: %v", e.iter, e.turn, err)
		}
	}
	latest, err := st.LatestTurn(context.Background(), runID, nodeID)
	if err != nil {
		t.Fatalf("LatestTurn: %v", err)
	}
	if latest.LoopIter != 1 || latest.TurnIndex != 2 {
		t.Errorf("LatestTurn = (iter=%d, turn=%d), want (1,2)", latest.LoopIter, latest.TurnIndex)
	}
}
