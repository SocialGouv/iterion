package dispatcher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Compile-time guard: *heartbeatStore MUST satisfy model.TurnWriter so
// the hook layer's `emitter.(TurnWriter)` capability probe matches and
// dispatcher-launched runs persist per-turn checkpoints. A signature
// drift on WriteTurn (the bug this guards against) breaks compilation.
var _ model.TurnWriter = (*heartbeatStore)(nil)

// fakeTurnRunStore embeds store.RunStore (nil — only WriteTurn is
// exercised) and records the forwarded turn, standing in for a
// FilesystemRunStore that satisfies the optional TurnStore capability.
type fakeTurnRunStore struct {
	store.RunStore
	gotTurn   *store.TurnCheckpoint
	turnCalls int
}

func (f *fakeTurnRunStore) WriteTurn(_ context.Context, t *store.TurnCheckpoint) error {
	f.turnCalls++
	f.gotTurn = t
	return nil
}

// noTurnRunStore embeds store.RunStore but does NOT implement the
// optional WriteTurn capability (mirrors a cloud Mongo store).
type noTurnRunStore struct{ store.RunStore }

func TestHeartbeatStoreForwardsTurnWrites(t *testing.T) {
	f := &fakeTurnRunStore{}
	hb := newHeartbeatStore(f, func(string) {})

	// The hook layer probes the wrapper via model.TurnWriter; this must
	// succeed (it silently didn't with the old 3-arg signature).
	tw, ok := any(hb).(model.TurnWriter)
	if !ok {
		t.Fatal("*heartbeatStore does not satisfy model.TurnWriter; dispatcher runs would drop turn checkpoints")
	}

	turn := &store.TurnCheckpoint{RunID: "run-1", NodeID: "node-a"}
	if err := tw.WriteTurn(context.Background(), turn); err != nil {
		t.Fatalf("WriteTurn: %v", err)
	}
	if f.turnCalls != 1 {
		t.Fatalf("wrapped store WriteTurn calls = %d; want 1 (wrapper did not forward)", f.turnCalls)
	}
	if f.gotTurn != turn {
		t.Fatalf("wrapped store received %+v; want the forwarded checkpoint", f.gotTurn)
	}
}

func TestHeartbeatStoreTurnWriteNoopWithoutCapability(t *testing.T) {
	hb := newHeartbeatStore(noTurnRunStore{}, func(string) {})
	// A store lacking the WriteTurn capability degrades to a silent
	// no-op (matching the hook layer's capability-missing skip), never
	// a panic or error.
	if err := hb.WriteTurn(context.Background(), &store.TurnCheckpoint{RunID: "r"}); err != nil {
		t.Fatalf("WriteTurn no-op should not error, got %v", err)
	}
}

// TestHeartbeatStoreForwardsCreateChildRun: the engine probes the store it
// is handed with store.AsParentedRunCreator to create a subbot child WITH
// its parent link in the create write. An embedded interface promotes only
// the RunStore methods, so without an explicit forward the wrapper hides
// the capability and every dispatched child is created parentless, linked
// only by the engine's later stamping write.
func TestHeartbeatStoreForwardsCreateChildRun(t *testing.T) {
	fs, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	hb := newHeartbeatStore(fs, func(string) {})
	pc := store.AsParentedRunCreator(hb)
	if pc == nil {
		t.Fatal("*heartbeatStore hides CreateChildRun; a dispatched subbot child is created with no ParentRunID")
	}
	ctx := context.Background()
	r, err := pc.CreateChildRun(ctx, "child", "wf", "parent", nil)
	if err != nil {
		t.Fatalf("CreateChildRun: %v", err)
	}
	if r.ParentRunID != "parent" {
		t.Fatalf("returned run parent = %q, want parent", r.ParentRunID)
	}
	loaded, err := fs.LoadRun(ctx, "child")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if loaded.ParentRunID != "parent" {
		t.Fatalf("persisted parent = %q, want parent in the create write", loaded.ParentRunID)
	}
}
