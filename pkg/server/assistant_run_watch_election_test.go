package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fleet shape of the assistant run-watch sweep (#2636): several replicas
// start the worker on one shared lease store; a replica sweeps only inside a
// term it was elected to, and when the holder stops, another takes over while
// the stopped one sweeps no more. Every replica sweeping was the production
// cost the lease removes — each pass lists the watchers and loads, walks and
// delivers for every one of them, multiplied by the replica count.
//
// The invariant is read from each replica's own "elected" lines, not from a
// count of sweepers: a hand-over is the lease working — one more term, logged
// — not two sweeps. That only one replica holds the lease at a time is the
// store's contract, pinned by the conformance suite in pkg/lease.
func TestAssistantWatchSweep_OneReplicaSweepsAndASuccessorTakesOver(t *testing.T) {
	shared := lease.NewMemoryStore()
	type replica struct {
		s      *Server
		log    *lockedBuffer
		sweeps *countingRunStore
	}
	const replicas = 3
	rs := make([]*replica, replicas)
	for i := range rs {
		base, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		counting := &countingRunStore{RunStore: base}
		svc := newTestRunviewService(t, "", runview.WithStore(counting))
		// The service's boot reconcile walks a filesystem store once (it has
		// lock authority there); the sweep under test starts after it.
		counting.listRuns.Store(0)
		counting.listWatching.Store(0)
		ws := runwatch.NewFSStore(t.TempDir())
		if err := ws.EnsureSchema(context.Background()); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		log := &lockedBuffer{}
		s := &Server{
			runs:                    svc,
			assistantWatches:        ws,
			logger:                  iterlog.New(iterlog.LevelInfo, log),
			leases:                  shared,
			replicaID:               fmt.Sprintf("replica-%d", i),
			assistantWatchLeasePace: 100 * time.Millisecond,
		}
		rs[i] = &replica{s: s, log: log, sweeps: counting}
		s.startAssistantRunWatches()
		if s.assistantWatch == nil || s.assistantWatchCancel == nil {
			t.Fatalf("replica %d: the worker did not start", i)
		}
	}
	stop := func(r *replica) {
		if r.s.assistantWatchCancel == nil {
			return
		}
		r.s.assistantWatchCancel(context.Background())
		<-r.s.assistantWatchDone
		r.s.assistantWatchCancel = nil
	}
	stopAll := sync.OnceFunc(func() {
		for _, r := range rs {
			stop(r)
		}
	})
	t.Cleanup(stopAll)

	// A sweep's one store pass is its watcher listing; ListRuns, the
	// whole-store walk the fix removed, must stay at zero on every replica.
	swept := func(r *replica) int64 { return r.sweeps.listWatching.Load() }
	elected := func(r *replica) int {
		return strings.Count(r.log.String(), fmt.Sprintf("lease %q: %s elected", leaseAssistantWatchSweep, r.s.replicaID))
	}
	total := func() (n int64) {
		for _, r := range rs {
			n += swept(r)
		}
		return n
	}
	sweptUnelected := func() []int {
		var out []int
		for i, r := range rs {
			if swept(r) > 0 && elected(r) == 0 {
				out = append(out, i)
			}
		}
		return out
	}

	waitForCond(t, 10*time.Second, "the elected replica's first sweep", func() bool { return total() >= 1 })
	// Give every follower several lease paces to sweep wrongly before judging.
	time.Sleep(10 * 100 * time.Millisecond)
	if bad := sweptUnelected(); len(bad) > 0 {
		t.Fatalf("replicas %v swept without ever being elected — every replica sweeping multiplies the store load by the replica count (#2636)", bad)
	}
	for i, r := range rs {
		if n := r.sweeps.listRuns.Load(); n != 0 {
			t.Fatalf("replica %d walked the whole store %d times — the sweep asks for the watchers, never for every run", i, n)
		}
	}

	var leader *replica
	for _, r := range rs {
		if elected(r) > 0 {
			leader = r
			break
		}
	}
	if leader == nil {
		t.Fatal("a sweep ran but no replica logged its election")
	}
	stop(leader)
	frozen := swept(leader)

	waitForCond(t, 10*time.Second, "a successor's election and first sweep", func() bool {
		for _, r := range rs {
			if r != leader && elected(r) > 0 && swept(r) > 0 {
				return true
			}
		}
		return false
	})
	if bad := sweptUnelected(); len(bad) > 0 {
		t.Fatalf("replicas %v swept without ever being elected after the hand-over", bad)
	}
	time.Sleep(5 * 100 * time.Millisecond)
	if n := swept(leader); n != frozen {
		t.Fatalf("the stopped holder kept sweeping (%d → %d passes) — a replica that stepped down must sweep no more", frozen, n)
	}
}

// A worker with no lease store cannot elect anybody, so it must not sweep —
// and must SAY so. A silent skip is the invisible outage: veilles armed by
// nobody, episodes delivered by nobody, nothing anywhere naming the sweep that
// never ran. Same rule as the forge→board sync and the merge-gate sweeper.
func TestAssistantWatchSweep_AnUnusableLeaseRefusesLoudly(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	counting := &countingRunStore{RunStore: base}
	svc := newTestRunviewService(t, "", runview.WithStore(counting))
	// The boot reconcile's own walk is the service's, not the sweep's.
	counting.listRuns.Store(0)
	counting.listWatching.Store(0)
	ws := runwatch.NewFSStore(t.TempDir())
	if err := ws.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	log := &lockedBuffer{}
	s := &Server{
		runs:             svc,
		assistantWatches: ws,
		logger:           iterlog.New(iterlog.LevelInfo, log),
		leases:           nil, // what a wiring regression would leave behind
		replicaID:        "replica-0",
	}
	s.startAssistantRunWatches()
	if s.assistantWatchDone == nil {
		t.Fatal("the worker did not start")
	}
	select {
	case <-s.assistantWatchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker kept running with no lease store — it cannot have been elected")
	}
	if n := counting.listWatching.Load() + counting.listRuns.Load(); n != 0 {
		t.Errorf("store passes = %d with no lease store, want 0 — an unelected sweep is what the lease exists to refuse", n)
	}
	if out := log.String(); !strings.Contains(out, "assistant run watch sweep NOT started") {
		t.Fatalf("log = %q, want the refusal named: a net that cannot start and says nothing is an invisible outage", out)
	}
}

// electedWorker starts one replica's watch worker on shared, paced for a
// test, and returns the server with its counting store and log.
func electedWorker(t *testing.T, shared lease.Store, replicaID string) (*Server, *countingRunStore, *lockedBuffer) {
	t.Helper()
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	counting := &countingRunStore{RunStore: base}
	svc := newTestRunviewService(t, "", runview.WithStore(counting))
	counting.listRuns.Store(0)
	counting.listWatching.Store(0)
	ws := runwatch.NewFSStore(t.TempDir())
	if err := ws.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	log := &lockedBuffer{}
	s := &Server{
		runs:                    svc,
		assistantWatches:        ws,
		logger:                  iterlog.New(iterlog.LevelInfo, log),
		leases:                  shared,
		replicaID:               replicaID,
		assistantWatchLeasePace: 100 * time.Millisecond,
	}
	s.startAssistantRunWatches()
	if s.assistantWatch == nil || s.assistantWatchCancel == nil {
		t.Fatalf("%s: the worker did not start", replicaID)
	}
	t.Cleanup(func() {
		if s.assistantWatchCancel != nil {
			s.assistantWatchCancel(context.Background())
			<-s.assistantWatchDone
		}
	})
	return s, counting, log
}

// A holder deposed while its process lives on — a successor took the lease
// after a lost renewal — must stop sweeping: its term's context is what the
// loop runs on, and a nudge after the loss must not sweep either. Mirrors
// TestGateSweeper_ADeposedHolderStopsSweeping; the election test above ends
// its holder through the worker cancel only, which this shape never reaches.
func TestAssistantWatchSweep_ADeposedHolderStopsSweeping(t *testing.T) {
	shared := lease.NewMemoryStore()
	s, counting, log := electedWorker(t, shared, "holder")
	waitForCond(t, 10*time.Second, "the holder's first sweep", func() bool { return counting.listWatching.Load() >= 1 })
	waitForCond(t, 10*time.Second, "elected in the heartbeat", func() bool { return s.assistantWatch.heartbeat().Elected })

	if ok, err := shared.Acquire(context.Background(), leaseAssistantWatchSweep, "successor", time.Now().Add(time.Hour), time.Hour); err != nil || !ok {
		t.Fatalf("successor Acquire = %v, %v", ok, err)
	}
	waitForCond(t, 10*time.Second, "the holder to see the loss", func() bool { return strings.Contains(log.String(), "lost the lease") })
	// The loss is logged before the term's context is cancelled; give the
	// loop that cancellation, then count what nudges still buy.
	time.Sleep(200 * time.Millisecond)
	before := counting.listWatching.Load()
	for range 20 {
		s.assistantWatch.nudge()
		time.Sleep(20 * time.Millisecond)
	}
	if after := counting.listWatching.Load(); after != before {
		t.Fatalf("a deposed holder swept %d more times after a successor took the lease — two replicas sweep", after-before)
	}
	waitForCond(t, 10*time.Second, "the heartbeat to drop elected", func() bool { return !s.assistantWatch.heartbeat().Elected })
	if hb := s.assistantWatch.heartbeat(); hb.Stale {
		t.Fatalf("a replica that holds no lease reports stale: %+v — only the holder owes a sweep", hb)
	}
}

// The heartbeat an operator reads: Elected while this replica holds the
// lease, and Stale judged against the replica's own sweeps only then.
func TestAssistantWatchSweep_HeartbeatReportsTheHolder(t *testing.T) {
	shared := lease.NewMemoryStore()
	// A follower first: the lease is taken by someone else before it starts.
	if ok, err := shared.Acquire(context.Background(), leaseAssistantWatchSweep, "someone-else", time.Now(), time.Hour); err != nil || !ok {
		t.Fatalf("Acquire = %v, %v", ok, err)
	}
	follower, counting, _ := electedWorker(t, shared, "follower")
	time.Sleep(5 * 100 * time.Millisecond)
	if n := counting.listWatching.Load(); n != 0 {
		t.Fatalf("a follower swept %d times", n)
	}
	if hb := follower.assistantWatch.heartbeat(); !hb.Present || hb.Elected || hb.Stale {
		t.Fatalf("follower heartbeat = %+v, want present, not elected, not stale", hb)
	}

	holder, _, _ := electedWorker(t, lease.NewMemoryStore(), "holder")
	waitForCond(t, 10*time.Second, "the holder elected in its heartbeat", func() bool { return holder.assistantWatch.heartbeat().Elected })
	waitForCond(t, 10*time.Second, "the holder's first sweep completed", func() bool {
		hb := holder.assistantWatch.heartbeat()
		return !hb.CompletedAt.IsZero() && !hb.Stale
	})
}

// A shutdown signal that fired before the worker started (the boot/shutdown
// overlap) ends the worker: the loop returns on the signal, and the campaign
// must not take that as a term's end and re-campaign — one sweep per retry
// until the process exits.
func TestAssistantWatchSweep_AShutdownAlreadySignalledEndsTheWorker(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	counting := &countingRunStore{RunStore: base}
	svc := newTestRunviewService(t, "", runview.WithStore(counting))
	counting.listRuns.Store(0)
	counting.listWatching.Store(0)
	ws := runwatch.NewFSStore(t.TempDir())
	if err := ws.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	shutdown := make(chan struct{})
	close(shutdown)
	s := &Server{
		runs:                    svc,
		assistantWatches:        ws,
		shutdown:                shutdown,
		logger:                  iterlog.New(iterlog.LevelError, &lockedBuffer{}),
		leases:                  lease.NewMemoryStore(),
		replicaID:               "late",
		assistantWatchLeasePace: 50 * time.Millisecond,
	}
	s.startAssistantRunWatches()
	if s.assistantWatchDone == nil {
		t.Fatal("the worker did not start")
	}
	select {
	case <-s.assistantWatchDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("the worker kept campaigning after the shutdown signal: %d sweeps so far", counting.listWatching.Load())
	}
	if n := counting.listWatching.Load(); n > 1 {
		t.Fatalf("the worker swept %d times after a shutdown already signalled, want at most the pass the loop opens with", n)
	}
}

// The local server — no injected run store, its own filesystem store under
// the work dir, one process — elects itself quietly: the sweep campaigns on
// an in-memory lease store, and the multi-replica warning (which would name
// the assistant sweep, the one leased net that also runs locally) stays off.
func TestLeaseStoreFor_TheLocalServerElectsItselfQuietly(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc := newTestRunviewService(t, "", runview.WithStore(base))
	ws := runwatch.NewFSStore(t.TempDir())
	var warnings []string
	warnf := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	local := &Server{runs: svc, assistantWatches: ws}
	if !local.assistantWatchStarts() {
		t.Fatal("precondition: the assistant sweep is wired on this server")
	}
	if st := leaseStoreFor(local, warnf); st == nil {
		t.Fatal("nil lease store")
	} else if _, ok := st.(*lease.MemoryStore); !ok {
		t.Errorf("the local server got %T, want the in-memory store", st)
	}
	if len(warnings) != 0 {
		t.Fatalf("the local server warned about replicas it does not have: %q", warnings)
	}
}
