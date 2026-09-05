package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// The reconciliation net (ADR-097 §10). A net nobody runs is a comment, and a
// net every replica runs is N times the API budget and N racing writers — so
// the two things worth proving are that it RUNS on schedule and that exactly
// ONE replica runs each pass.

func syncWorkerFixture(t *testing.T) (*forge.MemoryBoardBindingStore, native.BoardStore, *fakeBoardClient) {
	t.Helper()
	bindings := forge.NewMemoryBoardBindingStore()
	b := *testBinding()
	b.SyncEvery = time.Minute
	if err := bindings.Upsert(context.Background(), b); err != nil {
		t.Fatalf("Upsert binding: %v", err)
	}
	board := newTestBoard(t)
	bc := &fakeBoardClient{project: testProject()}
	return bindings, board, bc
}

func newTestSyncWorker(bindings forge.BoardBindingStore, board native.BoardStore, bc forge.BoardClient, now func() time.Time) *BoardSyncWorker {
	return &BoardSyncWorker{
		Bindings: bindings,
		Now:      now,
		BoardClientFor: func(context.Context, forge.BoardBinding) (forge.BoardClient, error) {
			return bc, nil
		},
		CardsFor: func(context.Context, string) (native.BoardStore, error) { return board, nil },
	}
}

func TestBoardSyncWorkerRunsADuePass(t *testing.T) {
	bindings, board, bc := syncWorkerFixture(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedCard(t, board, 613, native.StateInbox)
	bc.pages = [][]forge.ProjectItem{{item("PVTI_1", 613, statusValue("Planned", at))}}

	w := newTestSyncWorker(bindings, board, bc, func() time.Time { return at })
	n, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("passes = %d, want 1", n)
	}
	if got := mustGet(t, board, forgeCardID(forge.ProviderGitHub, "SocialGouv/iterion", 613)).State; got != native.StateReady {
		t.Errorf("the pass did not reconcile: state = %q", got)
	}
	// The watermark advanced, so the next tick inside the interval is a no-op.
	n2, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("passes = %d, want 0 inside the interval", n2)
	}
}

func TestBoardSyncWorkerSkipsDisabledBindings(t *testing.T) {
	bindings := forge.NewMemoryBoardBindingStore()
	b := *testBinding()
	b.SyncEvery = 0 // explicitly off
	if err := bindings.Upsert(context.Background(), b); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	board := newTestBoard(t)
	bc := &fakeBoardClient{project: testProject()}
	w := newTestSyncWorker(bindings, board, bc, time.Now)

	n, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 0 {
		t.Fatalf("sync_every=0 means OFF, got %d passes", n)
	}
}

// TestBoardSyncWorkerElectsOneReplica is the whole point of the CAS: N
// replicas tick the same due binding at the same instant, and exactly one runs
// the pass. Without it every replica reads and rewrites the same board.
func TestBoardSyncWorkerElectsOneReplica(t *testing.T) {
	bindings, board, bc := syncWorkerFixture(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedCard(t, board, 613, native.StateInbox)
	bc.pages = [][]forge.ProjectItem{{item("PVTI_1", 613, statusValue("Planned", at))}}

	const replicas = 8
	var mu sync.Mutex
	total := 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newTestSyncWorker(bindings, board, bc, func() time.Time { return at })
			<-start
			n, err := w.Tick(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Tick: %v", err)
			}
			total += n
		}()
	}
	close(start)
	wg.Wait()

	if total != 1 {
		t.Fatalf("%d replicas ran the pass, want exactly 1 — the CAS is what elects the runner", total)
	}
}

// TestBoardSyncWorkerOneTenantFailureDoesNotStopTheRest: an unreachable board
// for one team must not stop every other team's reconciliation.
func TestBoardSyncWorkerOneTenantFailureDoesNotStopTheRest(t *testing.T) {
	bindings := forge.NewMemoryBoardBindingStore()
	ok := *testBinding()
	ok.TenantID, ok.SyncEvery = "team-ok", time.Minute
	bad := *testBinding()
	bad.TenantID, bad.SyncEvery = "team-bad", time.Minute
	for _, b := range []forge.BoardBinding{bad, ok} {
		if err := bindings.Upsert(context.Background(), b); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedCard(t, board, 613, native.StateInbox)
	good := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{
		{item("PVTI_1", 613, statusValue("Planned", at))},
	}}

	w := &BoardSyncWorker{
		Bindings: bindings,
		Now:      func() time.Time { return at },
		BoardClientFor: func(_ context.Context, b forge.BoardBinding) (forge.BoardClient, error) {
			if b.TenantID == "team-bad" {
				return nil, errors.New("connection revoked")
			}
			return good, nil
		},
		CardsFor: func(context.Context, string) (native.BoardStore, error) { return board, nil },
	}
	n, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("a per-tenant failure must not fail the tick: %v", err)
	}
	if n != 1 {
		t.Errorf("passes = %d, want 1 (the healthy team still ran)", n)
	}
	if got := mustGet(t, board, forgeCardID(forge.ProviderGitHub, "SocialGouv/iterion", 613)).State; got != native.StateReady {
		t.Errorf("the healthy team was not reconciled: state = %q", got)
	}
}

func TestBoardSyncWorkerNeedsItsCollaborators(t *testing.T) {
	w := &BoardSyncWorker{}
	if _, err := w.Tick(context.Background()); err == nil {
		t.Fatal("a worker with no binding store must report it, not tick silently forever")
	}
}
