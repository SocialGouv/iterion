package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

// countingRunStore counts the store passes. ListRuns is the one call whose
// price scales with the size of the store rather than with the number of
// live veilles, so the sweep owes ZERO of them (#2636): the pass it makes
// asks for the watchers (ListDeliverableWatchers), an indexed question whose
// price follows the answer.
type countingRunStore struct {
	store.RunStore
	listRuns     atomic.Int64
	listWatching atomic.Int64
	loadRun      atomic.Int64
	// loadProjected counts the reads that went through the listing
	// projection (store.RunListingStore) instead of the whole document.
	loadProjected atomic.Int64
}

func (c *countingRunStore) ListRuns(ctx context.Context) ([]string, error) {
	c.listRuns.Add(1)
	return c.RunStore.ListRuns(ctx)
}

func (c *countingRunStore) ListDeliverableWatchers(ctx context.Context) ([]string, error) {
	c.listWatching.Add(1)
	return c.RunStore.ListDeliverableWatchers(ctx)
}

func (c *countingRunStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	c.loadRun.Add(1)
	return c.RunStore.LoadRun(ctx, id)
}

// LoadRunForListing makes the counting store a RunListingStore, so a path
// that asks for the projection is seen asking for it.
func (c *countingRunStore) LoadRunForListing(ctx context.Context, id string) (*store.Run, error) {
	c.loadProjected.Add(1)
	lister := store.AsRunListingStore(c.RunStore)
	if lister == nil {
		return c.RunStore.LoadRun(ctx, id)
	}
	return lister.LoadRunForListing(ctx, id)
}

func newCountingArmFixture(t *testing.T) (*armFixture, *countingRunStore) {
	t.Helper()
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	counting := &countingRunStore{RunStore: base}
	svc := newTestRunviewService(t, "", runview.WithStore(counting))

	botsRoot := t.TempDir()
	dir := filepath.Join(botsRoot, "chatbot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow chatbot:\n  entry: chat\n"), 0o644); err != nil {
		t.Fatalf("write main.bot: %v", err)
	}
	manifest := "name: chatbot\nchat:\n  nodes:\n    chat:\n      kind: human\n      text_field: message\n      host_event_field: host_event\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ws := runwatch.NewFSStore(t.TempDir())
	if err := ws.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	srv := &Server{runs: svc, assistantWatches: ws, logger: iterlog.New(iterlog.LevelError, os.Stderr)}
	srv.cfg.Bots.Paths = []string{botsRoot}
	return &armFixture{
		srv:   srv,
		coord: testAssistantWatchCoordinator(srv, ws, "test"),
		rs:    counting,
		ws:    ws,
	}, counting
}

// reconcileArmedWatches runs every 20s and on every run-outcome event. Its
// own comment promises a cost bounded by the number of live veilles; the
// code did the opposite — the outer loop listed and loaded every run, and
// each terminal target it found triggered ANOTHER full list-and-load inside
// armWatchesForTarget. One pass per target is what turns a long-lived store
// into continuous server load.
func TestReconcileArmedWatchesMakesOneStorePassWhateverTheTargetCount(t *testing.T) {
	ctx := context.Background()
	f, counting := newCountingArmFixture(t)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	const targets = 6
	for i := 0; i < targets; i++ {
		f.target(t, fmt.Sprintf("run-target-%d", i), issueID, assistant.CreatedAt.Add(time.Duration(i+1)*time.Minute))
	}

	counting.listRuns.Store(0)
	counting.listWatching.Store(0)
	counting.loadRun.Store(0)
	f.coord.reconcileArmedWatches(ctx)

	if got := counting.listRuns.Load(); got != 0 {
		t.Fatalf("reconcileArmedWatches listed the whole store %d times over %d targets, want 0 — the sweep asks for the watchers, never for every run (#2636)", got, targets)
	}
	if got := counting.listWatching.Load(); got != 1 {
		t.Fatalf("reconcileArmedWatches asked for the watchers %d times over %d targets, want exactly 1", got, targets)
	}

	// And it still did the work: every terminal target the watched card
	// produced is linked to the assistant.
	for i := 0; i < targets; i++ {
		id := fmt.Sprintf("run-target-%d", i)
		watches, err := f.ws.ListActiveByTarget(ctx, "", id)
		if err != nil {
			t.Fatalf("ListActiveByTarget(%s): %v", id, err)
		}
		if len(watches) != 1 || watches[0].AssistantRunID != assistant.ID {
			t.Fatalf("%s got %d watches (%+v), want 1 pointing at %s", id, len(watches), watches, assistant.ID)
		}
	}
}

// The watchers a sweep discovers are read through the listing projection,
// never whole: a watcher is used for its identity, cards, status and bot,
// and its recorded workflow source — hundreds of kilobytes for a real bot —
// is the one field no consumer of the sweep reads. A fleet of many live
// watchers then costs their headers per pass, not their sources (#2636, the
// gate's own question on the ticket's "projected loads" criterion).
func TestListIssueWatchersReadsTheProjection(t *testing.T) {
	ctx := context.Background()
	f, counting := newCountingArmFixture(t)
	const issueID = "native:card"
	const watchers = 3
	for i := range watchers {
		f.assistant(t, fmt.Sprintf("run-assistant-%d", i), issueID)
	}

	counting.loadRun.Store(0)
	counting.loadProjected.Store(0)
	got, err := f.coord.listIssueWatchers(ctx)
	if err != nil {
		t.Fatalf("listIssueWatchers: %v", err)
	}
	if len(got) != watchers {
		t.Fatalf("listIssueWatchers returned %d watchers, want %d", len(got), watchers)
	}
	for _, w := range got {
		if !w.SourceOmitted {
			t.Fatalf("watcher %s came back with its recorded source — the sweep must read the projection", w.ID)
		}
	}
	if n := counting.loadRun.Load(); n != 0 {
		t.Fatalf("listIssueWatchers loaded %d whole documents, want 0 — the projection exists for this read", n)
	}
	if n := counting.loadProjected.Load(); n < watchers {
		t.Fatalf("listIssueWatchers made %d projected reads for %d watchers, want at least one each", n, watchers)
	}
}

// A target reached through several watchers of the same card must be armed
// and observed ONCE per pass, not once per watcher. Both operations are
// idempotent, so this was only ever repeated cost — but it is cost that
// multiplies with the number of assistants watching a card, which is
// exactly the direction the feature grows in.
//
// Measured as a delta rather than an absolute: LoadRun is reached by
// several downstream helpers, so only the SHAPE of the growth is
// meaningful. Adding a second watcher must cost about one more run to
// enumerate, never a second full walk of the target.
func TestReconcileArmedWatchesDoesNotRepeatATargetPerWatcher(t *testing.T) {
	ctx := context.Background()
	const issueID = "native:card"

	cost := func(watchers int) int64 {
		f, counting := newCountingArmFixture(t)
		var first *store.Run
		for i := 0; i < watchers; i++ {
			a := f.assistant(t, fmt.Sprintf("run-assistant-%d", i), issueID)
			if first == nil {
				first = a
			}
		}
		f.target(t, "run-target", issueID, first.CreatedAt.Add(time.Minute))
		counting.loadRun.Store(0)
		f.coord.reconcileArmedWatches(ctx)
		return counting.loadRun.Load()
	}

	one, four := cost(1), cost(4)
	// Three extra watchers: three extra enumerations, plus each one's own
	// arm attempt against the single target. A repeated per-watcher walk of
	// the target would add its load + snapshot + observe chain on top.
	if four > one+12 {
		t.Fatalf("1 watcher costs %d run loads, 4 cost %d — the target is being re-walked per watcher", one, four)
	}
}
