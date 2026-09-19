package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// blockingRepoIntegrationStore parks ListSyncEnabledForRepo until release is
// signalled — the shape of "webhook lands, projection begins its store read,
// SIGTERM arrives, projection still executing". Delegates every other method
// to the memory store so the surface stays complete.
type blockingRepoIntegrationStore struct {
	*forge.MemoryRepoIntegrationStore
	entered chan struct{}
	release chan struct{}
}

func newBlockingRepoIntegrationStore() *blockingRepoIntegrationStore {
	return &blockingRepoIntegrationStore{
		MemoryRepoIntegrationStore: forge.NewMemoryRepoIntegrationStore(),
		entered:                    make(chan struct{}, 1),
		release:                    make(chan struct{}),
	}
}

func (b *blockingRepoIntegrationStore) ListSyncEnabledForRepo(ctx context.Context, repo string) ([]forge.RepoIntegration, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	// Deliberately parks on release only — a projection that cooperates with
	// ctx.Done would exit on the join budget and mask a broken join. The
	// property under test is that Shutdown WAITS on the registered worker;
	// only a runaway handler proves the wait actually happens.
	<-b.release
	return b.MemoryRepoIntegrationStore.ListSyncEnabledForRepo(ctx, repo)
}

// TestScheduleForgeBoardProjectionRegistersForShutdownJoin is the reproducer
// for #1345: before the fix, scheduleForgeBoardProjection detached a
// goroutine on context.Background(), and up to 16 store writes could be cut
// by SIGTERM on a rolling deploy — invisible in the operator's log because
// nobody was waiting.
//
// The property is stated as SEAM registration: the projection goes through
// tryGoUntilShutdown, so the shutdown JOIN — already proven to wait for its
// registered loops by TestShutdownJoinsTheLoopsItCancels — covers this path
// too. Mutation the test defends against: removing the goUntilShutdown wrap
// (a bare `go func()` again) leaves bgWorkers without the entry and the
// test reddens on the very next line.
func TestScheduleForgeBoardProjectionRegistersForShutdownJoin(t *testing.T) {
	srv := newMissionTestServer(t)
	// Wire the two guards scheduleForgeBoardProjection checks: without them
	// the projection short-circuits and never registers a worker (the
	// self-hosted-mode branch, deliberately silent).
	srv.cfg.CloudBoardFor = func(string) native.BoardStore { return nil }
	store := newBlockingRepoIntegrationStore()
	srv.forgeIntegrations = store

	srv.scheduleForgeBoardProjection("owner/repo")

	// The projection is registered under a stable, greppable name. If the
	// production code regressed to a bare `go func()`, no entry lands.
	srv.stateMu.Lock()
	var registered bool
	for _, w := range srv.bgWorkers {
		if w.name == "server.forgeBoardProjection" {
			registered = true
			break
		}
	}
	srv.stateMu.Unlock()
	if !registered {
		t.Fatal("scheduleForgeBoardProjection did not register a bgWorker — the fix for #1345 regressed to a bare goroutine")
	}

	// Wait for the projection to actually be inside the store read (proves
	// the fn was invoked, not just registered). Then release.
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("projection never entered its store read")
	}
	close(store.release)
}

// TestScheduleForgeBoardProjectionShutdownWaitsForInFlight extends the seam
// proof: with a projection actually executing its store read, a concurrent
// Shutdown WAITS on it — the join budget covers the whole burst. Without
// the goUntilShutdown wrap, Shutdown would exit while the goroutine was
// still writing (before, it ran on context.Background()).
func TestScheduleForgeBoardProjectionShutdownWaitsForInFlight(t *testing.T) {
	srv := newMissionTestServer(t)
	// The join budget must be well above the wait for the projection to
	// return once its ctx is cancelled; the projection observes ctx via
	// the outer tryGoUntilShutdown context, so cancelling it makes the
	// store's blocking Select return within microseconds.
	srv.bgJoinBudget = 30 * time.Second
	srv.cfg.CloudBoardFor = func(string) native.BoardStore { return nil }
	store := newBlockingRepoIntegrationStore()
	srv.forgeIntegrations = store

	srv.scheduleForgeBoardProjection("owner/repo")

	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("projection never entered its store read")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	shutdownReturned := make(chan error, 1)
	go func() { shutdownReturned <- srv.Shutdown(ctx) }()

	// Shutdown must not return while the projection is still parked in the
	// blocking store read. The pre-fix bug: Background() ctx, no join → the
	// projection's write outlived Shutdown by whatever the ctx budget was.
	select {
	case err := <-shutdownReturned:
		t.Fatalf("Shutdown returned (err=%v) while projection was still parked — #1345 regressed", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Release the projection. The projection's outer ctx (from
	// tryGoUntilShutdown) is now cancelled by Shutdown, so its store read
	// exits on the ctx.Done branch; but for full realism release too, so
	// the exit path is deterministic on either arm.
	close(store.release)

	select {
	case err := <-shutdownReturned:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown never returned once the projection had")
	}
}

// TestScheduleForgeBoardProjectionReleasesSemOnRefused pins the semaphore
// leak the tryGoUntilShutdown handshake was added to prevent: when
// registration is refused (the shutdown has already joined its background
// loops), the slot the caller reserved is released back to the pool. The
// pre-fix design — call goUntilShutdown, defer <-sem inside fn — leaked one
// slot per late webhook, permanently narrowing the concurrency cap on any
// process kept alive by a hung Shutdown.
func TestScheduleForgeBoardProjectionReleasesSemOnRefused(t *testing.T) {
	srv := newMissionTestServer(t)
	srv.cfg.CloudBoardFor = func(string) native.BoardStore { return nil }
	srv.forgeIntegrations = newBlockingRepoIntegrationStore()

	// Drive the server to the "already joined" state.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("initial shutdown: %v", err)
	}

	// The semaphore is per-Server now (#1477 follow-up Q4), so this snapshot
	// is not polluted by any other test.
	sem := srv.forgeProjSem
	before := len(sem)

	// Fire a burst of projections; each acquires a slot but the registration
	// is refused. Each must release the slot before returning.
	for i := 0; i < 16; i++ {
		srv.scheduleForgeBoardProjection("owner/repo")
	}

	after := len(sem)
	if after != before {
		t.Fatalf("forgeProjectionSem grew by %d slots on refused registrations; slots leaked", after-before)
	}
}
