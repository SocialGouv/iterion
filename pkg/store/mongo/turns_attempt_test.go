package mongo

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newTurnTestStore builds a Mongo store against a throwaway database, or
// skips when ITERION_TEST_MONGO_URI is unset — same gate as the note
// store test.
func newTurnTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo turn-store test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_turns_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	return s
}

// Revi on #2806, MOYEN: the embedded `prior` list was unbounded and each
// prior carries its inline Messages blob — a node retried past the BSON
// 16 MiB ceiling made every further write on the coordinate FAIL. The
// store keeps the most recent superseded attempts (the canonical record
// is never dropped) and SAYS the cut: the canonical checkpoint carries
// DroppedAttempts, so a reader summing the node's usage knows the
// baseline is partial instead of silently exact.
func TestWriteTurn_PriorListIsBoundedAndSaysTheCut(t *testing.T) {
	s := newTurnTestStore(t)
	ctx := store.WithoutTenantFilter(context.Background())
	const runID, nodeID = "run-many-attempts", "writer"

	// N+4 attempts: the first is the canonical write, the N+3 retries
	// stack N+3 priors — three past the bound.
	total := maxRetainedTurnAttempts + 4
	for i := 1; i <= total; i++ {
		err := s.WriteTurn(ctx, &store.TurnCheckpoint{
			RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
			Backend: "claude_code", SessionID: fmt.Sprintf("sess-%02d", i),
			Usage:    store.TurnUsage{OutputTokens: i},
			Messages: []byte(fmt.Sprintf(`[{"role":"user","content":"attempt %d"}]`, i)),
		})
		if err != nil {
			t.Fatalf("WriteTurn(attempt %d): %v — the coordinate must stay writable past the bound", i, err)
		}
	}

	rows, err := s.ListTurns(ctx, runID, nodeID, 0)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if want := maxRetainedTurnAttempts + 1; len(rows) != want {
		t.Fatalf("ListTurns = %d rows, want %d (the bound retains the most recent superseded attempts plus the canonical)", len(rows), want)
	}
	// The retained priors are the MOST RECENT ones, in ascending attempt
	// order; the canonical record is the last row.
	if rows[0].Attempt != 4 || !rows[0].Superseded {
		t.Errorf("rows[0] = attempt %d superseded %v; want the oldest RETAINED attempt (4), superseded", rows[0].Attempt, rows[0].Superseded)
	}
	latest := rows[len(rows)-1]
	if latest.Attempt != total || latest.Superseded || latest.SessionID != fmt.Sprintf("sess-%02d", total) {
		t.Errorf("canonical = attempt %d superseded %v session %q; want attempt %d, canonical, sess-%02d",
			latest.Attempt, latest.Superseded, latest.SessionID, total, total)
	}
	if latest.DroppedAttempts != 3 {
		t.Errorf("canonical.DroppedAttempts = %d, want 3 — the cut must be said, or a usage sum reads as exact what is partial", latest.DroppedAttempts)
	}
	// The coordinate stays writable once the cut has fallen.
	if err := s.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-final",
		Usage: store.TurnUsage{OutputTokens: 99},
	}); err != nil {
		t.Fatalf("WriteTurn past the cut: %v", err)
	}
	if got, err := s.LoadTurn(ctx, runID, nodeID, 0, 0); err != nil || got.Attempt != total+1 || got.DroppedAttempts != 4 {
		t.Errorf("after one more retry LoadTurn = attempt %d dropped %d (%v); want %d/4", got.Attempt, got.DroppedAttempts, err, total+1)
	}
}

// Revi on #2806, MINEUR 2: the fs twin ignores Messages when recognising
// an attempt (the blob lands in the sidecar; a redelivery that could not
// re-marshal it is still the same attempt), so the mongo twin must read
// the same input the same way: a byte-identical checkpoint rewritten
// WITHOUT Messages is the same attempt — no new prior, and the canonical
// keeps its conversation.
func TestWriteTurn_SameAttemptWithoutMessagesKeepsTheBlob(t *testing.T) {
	s := newTurnTestStore(t)
	ctx := store.WithoutTenantFilter(context.Background())
	const runID, nodeID = "run-redelivery", "writer"

	full := &store.TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-a",
		Usage:    store.TurnUsage{OutputTokens: 23},
		Messages: []byte(`[{"role":"user","content":"draft"}]`),
	}
	if err := s.WriteTurn(ctx, full); err != nil {
		t.Fatalf("WriteTurn: %v", err)
	}
	// The same attempt, redelivered by a path that has no conversation
	// snapshot to carry.
	bare := &store.TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", SessionID: "sess-a",
		Usage: store.TurnUsage{OutputTokens: 23},
	}
	if err := s.WriteTurn(ctx, bare); err != nil {
		t.Fatalf("WriteTurn(bare redelivery): %v", err)
	}
	rows, err := s.ListTurns(ctx, runID, nodeID, 0)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListTurns = %d rows, want 1 — a bare redelivery of the same attempt must not stack a prior", len(rows))
	}
	msgs, err := s.LoadTurnMessages(ctx, runID, nodeID, 0, 0)
	if err != nil {
		t.Fatalf("LoadTurnMessages after the bare redelivery: %v — the canonical lost its conversation", err)
	}
	if string(msgs) != `[{"role":"user","content":"draft"}]` {
		t.Errorf("messages = %q, want the first write's blob", msgs)
	}
}
