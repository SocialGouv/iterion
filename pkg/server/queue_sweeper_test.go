package server

import (
	"context"
	"sync"
	"testing"
	"time"

	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

type fakeStaleLister struct {
	refs map[string][]mongostore.StaleRunRef // status -> refs
}

func (f *fakeStaleLister) ListStaleActiveRuns(_ context.Context, statuses []store.RunStatus, _ time.Time, _ int) ([]mongostore.StaleRunRef, error) {
	var out []mongostore.StaleRunRef
	for _, st := range statuses {
		out = append(out, f.refs[string(st)]...)
	}
	return out, nil
}

type fakeLeases struct {
	locked map[string]bool
	err    error // returned for every probe when set
}

func (f *fakeLeases) IsRunLocked(_ context.Context, runID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.locked[runID], nil
}

// fakeWindowedLeases additionally reports a redelivery window, like the
// real natsq.Conn.
type fakeWindowedLeases struct {
	fakeLeases
	window time.Duration
}

func (f *fakeWindowedLeases) RedeliveryWindow() time.Duration { return f.window }

type fakeSweepStore struct {
	store.RunStore
	mu      sync.Mutex
	flipped map[string]store.RunStatus
}

func (f *fakeSweepStore) UpdateRunStatusIf(ctx context.Context, id string, status store.RunStatus, _ string, _ []store.RunStatus) (bool, error) {
	// The sweeper must stamp the run's tenant before the CAS — assert
	// the ctx carries one (the mongo store would panic otherwise).
	if tenant, ok := store.TenantFromContext(ctx); !ok || tenant == "" {
		panic("sweeper CAS without tenant ctx")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flipped == nil {
		f.flipped = map[string]store.RunStatus{}
	}
	f.flipped[id] = status
	return true, nil
}

func TestSweepOrphanRuns(t *testing.T) {
	s := newOrgTestServer(t)
	fs := &fakeSweepStore{}
	s.cfg.Store = fs
	lister := &fakeStaleLister{refs: map[string][]mongostore.StaleRunRef{
		"queued":  {{ID: "r-queued", TenantID: "t1", Status: "queued"}},
		"running": {{ID: "r-crashed", TenantID: "t2", Status: "running"}, {ID: "r-healthy", TenantID: "t3", Status: "running"}},
	}}
	leases := &fakeLeases{locked: map[string]bool{"r-healthy": true}}

	s.sweepOrphanRuns(context.Background(), lister, leases, time.Now().UTC())

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.flipped["r-queued"] != store.RunStatusFailedResumable {
		t.Fatalf("queued orphan not flipped: %+v", fs.flipped)
	}
	if fs.flipped["r-crashed"] != store.RunStatusFailedResumable {
		t.Fatalf("crashed running orphan not flipped: %+v", fs.flipped)
	}
	if _, ok := fs.flipped["r-healthy"]; ok {
		t.Fatal("leased (in-flight) run was flipped — the lease check must protect it")
	}
}

// TestSweepOrphanRuns_LeaseFaultIsVisible pins the degradation contract: a
// broken lease probe must (a) fail safe (flip nothing), (b) count on the
// stage metric, and (c) bracket the episode edge-triggered — one Warn on
// entry, none while it persists, recovery flips the flag back.
func TestSweepOrphanRuns_LeaseFaultIsVisible(t *testing.T) {
	s := newOrgTestServer(t)
	fs := &fakeSweepStore{}
	s.cfg.Store = fs
	s.cfg.Metrics = cloudmetrics.New()
	lister := &fakeStaleLister{refs: map[string][]mongostore.StaleRunRef{
		"running": {{ID: "r-crashed", TenantID: "t1", Status: "running"}},
	}}
	broken := &fakeLeases{err: context.DeadlineExceeded}

	before := counterValue(t, s.cfg.Metrics.OrphanSweepErrors.WithLabelValues("lease"))
	s.sweepOrphanRuns(context.Background(), lister, broken, time.Now().UTC())

	fs.mu.Lock()
	if len(fs.flipped) != 0 {
		fs.mu.Unlock()
		t.Fatalf("lease-unknown candidates were flipped: %+v", fs.flipped)
	}
	fs.mu.Unlock()
	if got := counterValue(t, s.cfg.Metrics.OrphanSweepErrors.WithLabelValues("lease")); got != before+1 {
		t.Fatalf("lease error counter = %v, want %v — a disarmed sweeper must be measurable", got, before+1)
	}
	if !s.sweepDegraded {
		t.Fatal("sweepDegraded not set — the episode Warn would re-fire every tick or never")
	}

	// An EMPTY pass proves nothing: no candidate was probed, so the episode
	// must stay open instead of flapping "back to healthy" while NATS-KV is
	// still down (most minutes have no stale run at all).
	s.sweepOrphanRuns(context.Background(), &fakeStaleLister{}, &fakeLeases{}, time.Now().UTC())
	if !s.sweepDegraded {
		t.Fatal("an empty pass closed the degradation episode — false 'back to healthy'")
	}

	// A pass that actually probed a candidate cleanly closes it.
	s.sweepOrphanRuns(context.Background(), lister, &fakeLeases{}, time.Now().UTC())
	if s.sweepDegraded {
		t.Fatal("sweepDegraded still set after a clean probing pass")
	}
}

// TestSweepOrphanRuns_DeadScanOpensTheEpisode: a failing scan is orphan
// recovery 100% disabled — it must open the degraded episode, not just tick
// a per-minute Warn.
func TestSweepOrphanRuns_DeadScanOpensTheEpisode(t *testing.T) {
	s := newOrgTestServer(t)
	s.cfg.Store = &fakeSweepStore{}
	s.cfg.Metrics = cloudmetrics.New()
	s.sweepOrphanRuns(context.Background(), failingLister{}, &fakeLeases{}, time.Now().UTC())
	if !s.sweepDegraded {
		t.Fatal("a dead scan did not open the degradation episode")
	}

	// A SCAN episode closes on any pass whose scans return — an empty
	// healthy minute IS positive evidence for the scan stage. Without this
	// the flag latches forever (probed stays 0 on a healthy deployment)
	// and the edge-triggered Warn goes mute for every LATER outage.
	s.sweepOrphanRuns(context.Background(), &fakeStaleLister{}, &fakeLeases{}, time.Now().UTC())
	if s.sweepDegraded {
		t.Fatal("a recovered scan + empty healthy pass did not close the episode — the next outage would be silent")
	}
}

type failingLister struct{}

func (failingLister) ListStaleActiveRuns(context.Context, []store.RunStatus, time.Time, int) ([]mongostore.StaleRunRef, error) {
	return nil, context.DeadlineExceeded
}

func TestQueuedSweepCutoff(t *testing.T) {
	// Lease checker exposing the queue's redelivery window → window + margin.
	windowed := &fakeWindowedLeases{window: 80 * time.Minute}
	if got, want := queuedSweepCutoff(windowed), 90*time.Minute; got != want {
		t.Fatalf("windowed cutoff = %v, want %v", got, want)
	}
	// A zero window (misconfigured queue) must not collapse the cutoff.
	windowed.window = 0
	if got := queuedSweepCutoff(windowed); got != sweepQueuedFallback {
		t.Fatalf("zero-window cutoff = %v, want fallback %v", got, sweepQueuedFallback)
	}
	// Plain lease checker (no capability) → conservative fallback.
	if got := queuedSweepCutoff(&fakeLeases{}); got != sweepQueuedFallback {
		t.Fatalf("plain cutoff = %v, want fallback %v", got, sweepQueuedFallback)
	}
	// The fallback itself must exceed the shipped defaults' redelivery
	// envelope — this is the drift the derivation exists to prevent
	// (the old 20m constant silently fell behind a MaxDeliver/AckWait bump).
	envelope := time.Duration(natsq.DefaultStreamMaxRetry) * natsq.DefaultAckWait
	if sweepQueuedFallback <= envelope {
		t.Fatalf("fallback %v does not exceed the default MaxDeliver × AckWait envelope (%v)", sweepQueuedFallback, envelope)
	}
}
