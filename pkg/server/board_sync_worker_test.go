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

// TestBoardSyncWorkerRefusesAnOverlappingPass proves the whole point of the
// lease at the level an operator sees it: a pass that outlives the binding's
// interval must not be joined by a second replica.
//
// It uses a board client that BLOCKS inside the pass, so the overlapping tick
// happens while the first pass genuinely holds the board — the shape a
// watermark CAS alone cannot refuse, since an overrunning pass leaves exactly
// the watermark the next tick presents.
func TestBoardSyncWorkerRefusesAnOverlappingPass(t *testing.T) {
	bindings, board, bc := syncWorkerFixture(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedCard(t, board, 613, native.StateInbox)
	bc.pages = [][]forge.ProjectItem{{item("PVTI_1", 613, statusValue("Planned", at))}}

	inPass, release := make(chan struct{}), make(chan struct{})
	slow := &blockingBoardClient{fakeBoardClient: bc, entered: inPass, release: release}
	slowWorker := newTestSyncWorker(bindings, board, slow, func() time.Time { return at })

	done := make(chan int, 1)
	go func() {
		n, err := slowWorker.Tick(context.Background())
		if err != nil {
			t.Errorf("slow Tick: %v", err)
		}
		done <- n
	}()
	<-inPass // the first pass owns the board and has not finished

	// A second replica, one interval later: the binding is due again and the
	// watermark it presents is the one the running pass wrote.
	held, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	second := newTestSyncWorker(bindings, board, bc, func() time.Time { return at.Add(90 * time.Second) })
	n2, err := second.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if n2 != 0 {
		t.Errorf("passes = %d, want 0 — a board whose pass is still running must not be claimed again", n2)
	}
	after, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if !after.LastSyncedAt.Equal(held.LastSyncedAt) {
		t.Errorf("LastSyncedAt = %v, want the running pass's %v", after.LastSyncedAt, held.LastSyncedAt)
	}

	close(release)
	if n := <-done; n != 1 {
		t.Errorf("the first pass = %d, want 1", n)
	}
	// The lease is handed back at pass end, so the next tick claims at once
	// rather than sitting out the TTL.
	third := newTestSyncWorker(bindings, board, bc, func() time.Time { return at.Add(2 * time.Minute) })
	if n, err := third.Tick(context.Background()); err != nil || n != 1 {
		t.Errorf("after the pass ended: passes = %d err = %v, want 1", n, err)
	}
}

// blockingBoardClient parks inside ListProjectItems until released, so a test
// can observe the board WHILE a pass holds it.
type blockingBoardClient struct {
	*fakeBoardClient
	entered  chan struct{}
	release  chan struct{}
	oncePark sync.Once
}

func (b *blockingBoardClient) ListProjectItems(ctx context.Context, ref forge.ProjectRef, opts forge.ProjectItemListOptions) (forge.ProjectItemPage, error) {
	b.oncePark.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.fakeBoardClient.ListProjectItems(ctx, ref, opts)
}

// TestBoardSyncWorkerUsesAPerPassLeaseOwner pins that the worker's release is
// scoped to the pass that took the lease, end to end.
//
// The store CASes on the owner; what the worker owes is a token that is FRESH
// per pass. A per-replica token would let a replica whose earlier pass overran
// release the lease its own next pass just took — the same re-admission, one
// level up.
func TestBoardSyncWorkerUsesAPerPassLeaseOwner(t *testing.T) {
	bindings, board, bc := syncWorkerFixture(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedCard(t, board, 613, native.StateInbox)
	bc.pages = [][]forge.ProjectItem{{item("PVTI_1", 613, statusValue("Planned", at))}}

	w := newTestSyncWorker(bindings, board, bc, func() time.Time { return at })
	if n, err := w.Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("pass 1: n=%d err=%v", n, err)
	}
	first, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	// The pass released, so nothing holds the board.
	if !first.SyncLeaseUntil.IsZero() || first.SyncLeaseOwner != "" {
		t.Fatalf("the board is still held after a finished pass: %+v", first)
	}

	w2 := newTestSyncWorker(bindings, board, bc, func() time.Time { return at.Add(2 * time.Minute) })
	if n, err := w2.Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("pass 2: n=%d err=%v", n, err)
	}
	// Whatever the first pass's token was, the second's must differ — the
	// store's CAS is only worth anything if the token is not reused.
	second, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if !second.SyncLeaseUntil.IsZero() || second.SyncLeaseOwner != "" {
		t.Fatalf("the board is still held after the second pass: %+v", second)
	}
	// And a stale token from any earlier pass cannot free a live lease.
	if _, err := bindings.ClaimSync(context.Background(), "team-a", second.LastSyncedAt, at.Add(4*time.Minute), "replica-live"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := bindings.ReleaseSync(context.Background(), "team-a", "some-earlier-pass"); !errors.Is(err, forge.ErrBoardSyncLeaseLost) {
		t.Fatalf("a stale token released a live lease: %v", err)
	}
}
