package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestHeartbeatStoreForwardsReliabilityCapabilities(t *testing.T) {
	fs, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := fs.CreateRun(ctx, "run", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hb := newHeartbeatStore(fs, func(string) {})

	once := store.AsQueuedMessageInsertOnceStore(hb)
	if once == nil {
		t.Fatal("*heartbeatStore hides QueuedMessageInsertOnceStore")
	}
	msg := store.QueuedUserMessage{ID: "msg_stable", Text: "fix it"}
	inserted, err := once.AppendQueuedMessageOnce(ctx, "run", msg)
	if err != nil || !inserted {
		t.Fatalf("first AppendQueuedMessageOnce = (%t, %v), want inserted", inserted, err)
	}
	inserted, err = once.AppendQueuedMessageOnce(ctx, "run", msg)
	if err != nil || inserted {
		t.Fatalf("replayed AppendQueuedMessageOnce = (%t, %v), want no-op", inserted, err)
	}

	cursors := store.AsWatcherCursorStore(hb)
	if cursors == nil {
		t.Fatal("*heartbeatStore hides WatcherCursorStore")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := cursors.SetWatcherCursor(ctx, "run", "supervisor:safe", store.WatcherCursor{WatcherID: "supervisor:safe", LastProgressAt: now}); err != nil {
		t.Fatalf("SetWatcherCursor: %v", err)
	}

	corrections := store.AsOutputCorrectionStore(hb)
	if corrections == nil {
		t.Fatal("*heartbeatStore hides OutputCorrectionStore")
	}
	if err := corrections.SetRunOutputCorrection(ctx, "run", "agent_root", store.OutputCorrectionEpisode{EpisodeID: "agent/root", Status: "active", UpdatedAt: now}); err != nil {
		t.Fatalf("SetRunOutputCorrection: %v", err)
	}

	loaded, err := fs.LoadRun(ctx, "run")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	queued, err := fs.ListQueuedMessages(ctx, "run")
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued messages = (%d, %v), want one", len(queued), err)
	}
	if got := loaded.WatcherCursors["supervisor:safe"].WatcherID; got != "supervisor:safe" {
		t.Fatalf("persisted watcher id = %q", got)
	}
	if got := loaded.OutputCorrections["agent_root"].EpisodeID; got != "agent/root" {
		t.Fatalf("persisted correction episode = %q", got)
	}
}

// TestHeartbeatStoreHidesNoCapabilityTheEngineProbes: every optional store
// capability the engine probes (store.As* in pkg/runtime) that the wrapped
// store has is visible through the heartbeat wrapper. A hidden one degrades
// in silence: a dispatcher run parks without banking its scratch, persists
// no packed CLI session, mounts no run-files directory.
func TestHeartbeatStoreHidesNoCapabilityTheEngineProbes(t *testing.T) {
	probes := map[string]func(store.RunStore) bool{
		"AsBackendSessionStore":   func(s store.RunStore) bool { return store.AsBackendSessionStore(s) != nil },
		"AsOutputCorrectionStore": func(s store.RunStore) bool { return store.AsOutputCorrectionStore(s) != nil },
		"AsParentedRunCreator":    func(s store.RunStore) bool { return store.AsParentedRunCreator(s) != nil },
		"AsQueuedAttemptMover":    func(s store.RunStore) bool { return store.AsQueuedAttemptMover(s) != nil },
		"AsRunFilesStore":         func(s store.RunStore) bool { return store.AsRunFilesStore(s) != nil },
		"AsScratchBankStore":      func(s store.RunStore) bool { return store.AsScratchBankStore(s) != nil },
		"AsSpendStore":            func(s store.RunStore) bool { return store.AsSpendStore(s) != nil },
	}
	files, err := filepath.Glob(filepath.Join("..", "runtime", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	probed := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`store\.(As[A-Z][A-Za-z]*)\(`).FindAllStringSubmatch(string(src), -1) {
			probed[m[1]] = true
		}
	}
	if len(probed) < 5 {
		t.Fatalf("found %d store probes in pkg/runtime, want the engine's ~6 — the scan no longer reads them", len(probed))
	}
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hb := newHeartbeatStore(base, func(string) {})
	for name := range probed {
		probe, ok := probes[name]
		if !ok {
			t.Errorf("the engine probes store.%s: add it to this table", name)
			continue
		}
		if probe(base) && !probe(hb) {
			t.Errorf("store.%s finds the capability on the dispatcher's store but not through the heartbeat wrapper", name)
		}
	}
	if !probes["AsScratchBankStore"](hb) {
		t.Fatal("the dispatcher's store keeps no scratch bank — the check above proves nothing for it")
	}
}
