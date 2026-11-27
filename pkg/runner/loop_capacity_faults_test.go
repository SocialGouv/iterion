package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The capacity park under store faults: a store error on the park's road
// re-offers the delivery while the queue redelivers, its transit kept, and
// ends the run final on the last delivery; the park's store budget starts
// once the freeze returned; a final end the store refuses is written again by
// the delivery's own end.

var errStoreBlip = errors.New("server selection error: context deadline exceeded")

// faultyParkStore is a capacity store whose park road fails on demand: the
// LoadRun calls loadFailAt names (1-based) fail, the park write fails with
// parkErr after parkDelay — after landing when parkLands is set, as a write
// the server applied before the client's deadline — the first endFails
// transit ends fail. beforePark runs just before the park write. Each park
// and end records the deadline it was handed.
type faultyParkStore struct {
	store.RunStore
	mu           sync.Mutex
	loadCalls    int
	loadFailAt   map[int]bool
	parkErr      error
	parkLands    bool
	parkDelay    time.Duration
	parkDeadline time.Time
	parkReturned time.Time
	beforePark   func()
	endFails     int
	ends         int
	endDeadlines []time.Time
}

func (s *faultyParkStore) Unwrap() store.RunStore { return s.RunStore }

func (s *faultyParkStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.mu.Lock()
	s.loadCalls++
	fail := s.loadFailAt[s.loadCalls]
	s.mu.Unlock()
	if fail {
		return nil, errStoreBlip
	}
	return s.RunStore.LoadRun(ctx, id)
}

func (s *faultyParkStore) ParkRunOnCapacity(ctx context.Context, runID string, p store.CapacityPark) (bool, int, error) {
	dl, _ := ctx.Deadline()
	s.mu.Lock()
	s.parkDeadline = dl
	perr, lands, delay, before := s.parkErr, s.parkLands, s.parkDelay, s.beforePark
	s.mu.Unlock()
	if before != nil {
		before()
	}
	time.Sleep(delay)
	defer func() {
		s.mu.Lock()
		s.parkReturned = time.Now()
		s.mu.Unlock()
	}()
	if perr != nil && lands {
		if _, _, err := store.AsCapacityParker(s.RunStore).ParkRunOnCapacity(ctx, runID, p); err != nil {
			return false, 0, err
		}
	}
	if perr != nil {
		return false, 0, perr
	}
	return store.AsCapacityParker(s.RunStore).ParkRunOnCapacity(ctx, runID, p)
}

func (s *faultyParkStore) SaveParkedLaunchRun(ctx context.Context, r *store.Run, p store.CapacityPark) error {
	return store.AsCapacityParker(s.RunStore).SaveParkedLaunchRun(ctx, r, p)
}

func (s *faultyParkStore) BeginCapacityTransit(ctx context.Context, runID string, t store.CapacityTransit) (store.CapacityTransitStart, error) {
	return store.AsCapacityTransitStore(s.RunStore).BeginCapacityTransit(ctx, runID, t)
}

func (s *faultyParkStore) EndCapacityTransit(ctx context.Context, runID string, until time.Time, restore store.ContinuationState) (bool, error) {
	dl, _ := ctx.Deadline()
	s.mu.Lock()
	s.ends++
	s.endDeadlines = append(s.endDeadlines, dl)
	fail := s.endFails > 0
	if fail {
		s.endFails--
	}
	s.mu.Unlock()
	if fail {
		return false, errStoreBlip
	}
	return store.AsCapacityTransitStore(s.RunStore).EndCapacityTransit(ctx, runID, until, restore)
}

// deliver runs one guarded pickup as processOne does: the disposition
// dispatched through the nak recorder, then the delivery's end.
func (f *guardFixture) deliver(t *testing.T, delivered int) (preconditionOutcome, *fakeDelivery) {
	t.Helper()
	d := &fakeDelivery{delivered: delivered}
	rec := &nakRecorder{jsDelivery: d}
	g, out, proceed := f.r.capacityAtPickup(context.Background(), f.msg, nil, delivered, iterlog.Nop())
	if proceed {
		t.Fatalf("delivery %d proceeded, want a lost place handled at pickup", delivered)
	}
	dispatchPrecondition(iterlog.Nop(), rec, out, f.msg.RunID)
	if g != nil {
		g.endUnlessRedelivered(rec.nakked && f.r.maxDeliver() > 0 && delivered < f.r.maxDeliver())
	}
	return out, d
}

func (f *guardFixture) hasEvent(t *testing.T, typ store.EventType) bool {
	t.Helper()
	evs, err := f.fs.LoadEvents(idCtx(), f.msg.RunID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, ev := range evs {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// The park's read of the run fails on a delivery the queue will hand back:
// the delivery is re-offered after a delay with nothing written, the run in
// the transit, the pool lease open for the redelivery — and that redelivery
// parks it. Red when the store error acks the delivery with the run left with
// no continuation and no retry.
func TestCapacityPark_ReadErrorReoffersThenTheRedeliveryParks(t *testing.T) {
	f := newGuardFixture(t, "cap-park-read", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	pool := newPoolHarnessForRun(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1}, f.msg.RunID)
	f.r.cfg.CredPool = pool.broker
	f.msg.PublishedAtRFC = time.Now().UTC().Add(time.Millisecond).Format(time.RFC3339Nano)
	f.evict(t)
	w := &faultyParkStore{RunStore: f.park, loadFailAt: map[int]bool{1: true}}
	f.r.cfg.Store = w
	out, d := f.deliver(t, 2)
	if out.action != actionNakDelayed || out.delay != capacityReassertNakDelay || len(d.nakDelays) != 1 || d.acks != 0 {
		t.Fatalf("delivery 2: action=%v delay=%s acks=%d naks=%v, want a delayed nak", out.action, out.delay, d.acks, d.nakDelays)
	}
	if run := f.load(t); !run.InCapacityTransit(time.Now()) || run.RetryState != nil {
		t.Fatalf("run continuation=%q until=%v retry=%+v, want still in its transit for the redelivery", run.ContinuationState, run.ContinuationUntil, run.RetryState)
	}
	if parks := f.park.recordedParks(); len(parks) != 0 {
		t.Fatalf("parks %+v, want none on a read error", parks)
	}
	if leases, err := pool.leases.ListByDonor(context.Background(), "donor", 10); err != nil || len(leases) != 1 || leases[0].Closed {
		t.Fatalf("pool lease %+v (err %v), want open: the redelivery reuses it", leases, err)
	}
	out, d = f.deliver(t, 3)
	if out.action != actionAck || d.acks != 1 {
		t.Fatalf("delivery 3: action=%v acks=%d, want parked and acked", out.action, d.acks)
	}
	run := f.load(t)
	if run.FailureCode != store.FailureCredSlotWait || run.ContinuationState != store.ContinuationRetryArmed {
		t.Fatalf("delivery 3 left %s/%q, want parked on CRED_SLOT_WAIT retry_armed", run.FailureCode, run.ContinuationState)
	}
}

// The same read error on the claim road: the after-engine step re-offers the
// delivery instead of acking it.
func TestCapacityPark_ClaimRoadReadErrorReoffers(t *testing.T) {
	f := newGuardFixture(t, "cap-park-claim-read", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	g, _, proceed := f.pickup(t, 3)
	if !proceed {
		t.Fatal("pickup did not proceed")
	}
	_, _, held := g.ownedTransit()
	if err := store.AsCredSlotStore(f.fs).ReleaseCredSlot(idCtx(), capFP, f.msg.RunID, held[0].At); err != nil {
		t.Fatalf("release: %v", err)
	}
	err := g.claimGuard(context.Background(), f.load(t))
	if !errors.Is(err, store.ErrCapacityPlaceLost) {
		t.Fatalf("claim guard: %v, want the place lost", err)
	}
	f.r.cfg.Store = &faultyParkStore{RunStore: f.park, loadFailAt: map[int]bool{1: true}}
	d := &fakeDelivery{delivered: 3}
	status, handled := g.afterEngine(context.Background(), err, d, nil)
	if !handled || d.acks != 0 || len(d.nakDelays) != 1 || d.nakDelays[0] != capacityReassertNakDelay {
		t.Fatalf("after engine: handled=%v status=%q acks=%d naks=%v, want a delayed nak", handled, status, d.acks, d.nakDelays)
	}
	if run := f.load(t); !run.InCapacityTransit(time.Now()) {
		t.Fatalf("run continuation=%q until=%v, want still in its transit", run.ContinuationState, run.ContinuationUntil)
	}
}

// The park write fails on a delivery the queue will hand back: re-offered,
// the transit kept. Red when the failed write ends the run final.
func TestCapacityPark_WriteErrorReoffers(t *testing.T) {
	f := newGuardFixture(t, "cap-park-write", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.evict(t)
	f.r.cfg.Store = &faultyParkStore{RunStore: f.park, parkErr: errStoreBlip}
	out, d := f.deliver(t, 2)
	if out.action != actionNakDelayed || d.acks != 0 {
		t.Fatalf("delivery 2: action=%v acks=%d, want a delayed nak", out.action, d.acks)
	}
	if run := f.load(t); !run.InCapacityTransit(time.Now()) {
		t.Fatalf("run continuation=%q until=%v, want still in its transit for the redelivery", run.ContinuationState, run.ContinuationUntil)
	}
}

// A charged park refused for a spent budget, whose re-read fails: re-offered
// rather than read as a run that moved. Red when the read error is taken for
// a move and the delivery acked.
func TestCapacityPark_ChargedRereadErrorReoffers(t *testing.T) {
	f := newGuardFixture(t, "cap-park-reread", store.FailureExecutionFailed, store.ContinuationRedeliveryPending)
	f.park.attempts = map[string]int{f.msg.RunID: retrypolicy.DefaultMaxAttempts}
	f.evict(t)
	f.r.cfg.Store = &faultyParkStore{RunStore: f.park, loadFailAt: map[int]bool{2: true}}
	out, d := f.deliver(t, 2)
	if out.action != actionNakDelayed || d.acks != 0 {
		t.Fatalf("delivery 2: action=%v status=%s acks=%d, want a delayed nak", out.action, out.finalStatus, d.acks)
	}
	if run := f.load(t); !run.InCapacityTransit(time.Now()) {
		t.Fatalf("run continuation=%q until=%v, want still in its transit", run.ContinuationState, run.ContinuationUntil)
	}
}

// The park's read fails on the last delivery: nothing hands the run back, so
// it is acked and left final with the cause on its timeline. Red when it is
// left with no continuation and nothing said.
func TestCapacityPark_ReadErrorOnTheLastDeliveryIsFinal(t *testing.T) {
	f := newGuardFixture(t, "cap-park-read-last", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.evict(t)
	f.r.cfg.Store = &faultyParkStore{RunStore: f.park, loadFailAt: map[int]bool{1: true}}
	out, d := f.deliver(t, 8)
	if out.action != actionAck || d.acks != 1 {
		t.Fatalf("last delivery: action=%v acks=%d, want acked", out.action, d.acks)
	}
	if run := f.load(t); run.ContinuationState != store.ContinuationFinal || run.ContinuationUntil != nil {
		t.Fatalf("run continuation=%q until=%v, want final with no instant", run.ContinuationState, run.ContinuationUntil)
	}
	if !f.hasEvent(t, store.EventRunRetrySkipped) {
		t.Fatal("no run_retry_skipped event: the cause is not on the run's timeline")
	}
}

// A final end that matches nothing leaves the run to whoever moved it,
// nothing on its timeline, and the delivery reports it moved: on the last
// delivery, a park whose write landed although the store answered an error
// (the run is armed for the retry sweeper), and a charged park refused
// because an operator's resume moved the run, whose re-read then fails (the
// run is running). Red when the final end emits run_retry_skipped — "resume
// it by hand" on a run the sweeper or its owner resumes — whatever it
// matched.
func TestCapacityPark_AFinalEndThatMatchesNothingSaysNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   store.FailureCode
		setup  func(f *guardFixture, w *faultyParkStore)
		status store.RunStatus
		cont   store.ContinuationState
	}{
		{"a park that landed despite its error", store.FailureInterrupted, func(_ *guardFixture, w *faultyParkStore) {
			w.parkErr, w.parkLands = errStoreBlip, true
		}, store.RunStatusFailedResumable, store.ContinuationRetryArmed},
		{"a charged park refused for a move whose re-read fails", store.FailureExecutionFailed, func(f *guardFixture, w *faultyParkStore) {
			w.loadFailAt = map[int]bool{2: true}
			w.beforePark = func() {
				run, err := f.fs.LoadRun(idCtx(), f.msg.RunID)
				if err != nil {
					t.Errorf("load before the move: %v", err)
					return
				}
				run.Status, run.ContinuationState, run.ContinuationUntil = store.RunStatusRunning, "", nil
				if err := f.fs.SaveRun(idCtx(), run); err != nil {
					t.Errorf("move the run: %v", err)
				}
			}
		}, store.RunStatusRunning, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "cap-park-moved", tc.code, store.ContinuationRedeliveryPending)
			f.evict(t)
			w := &faultyParkStore{RunStore: f.park}
			tc.setup(f, w)
			f.r.cfg.Store = w
			out, d := f.deliver(t, 8)
			if out.action != actionAck || d.acks != 1 {
				t.Fatalf("last delivery: action=%v acks=%d, want acked", out.action, d.acks)
			}
			if out.finalStatus != "capacity_place_lost_moved" {
				t.Fatalf("final status %q, want capacity_place_lost_moved", out.finalStatus)
			}
			if run := f.load(t); run.Status != tc.status || run.ContinuationState != tc.cont {
				t.Fatalf("run %s/%q, want %s/%q: left to whoever moved it", run.Status, run.ContinuationState, tc.status, tc.cont)
			}
			if f.hasEvent(t, store.EventRunRetrySkipped) {
				t.Fatal("run_retry_skipped on the timeline of a run the final end did not match")
			}
		})
	}
}

// The park write fails on the last delivery after a slow write, and the store
// refuses the first final end: the end is made on a store budget of its own,
// and the delivery's end writes it again — the run is left final, not in a
// transit no redelivery honours. Red when the refused end disowns the
// transit, or runs on the caller's context.
func TestCapacityPark_ARefusedFinalEndIsWrittenByTheDeliveryEnd(t *testing.T) {
	f := newGuardFixture(t, "cap-park-end", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.evict(t)
	w := &faultyParkStore{RunStore: f.park, parkErr: errStoreBlip, parkDelay: 100 * time.Millisecond, endFails: 1}
	f.r.cfg.Store = w
	if out, _ := f.deliver(t, 8); out.action != actionAck {
		t.Fatalf("last delivery: action=%v, want acked", out.action)
	}
	if w.ends != 2 {
		t.Fatalf("transit ends %d, want 2: the refused final end written again by the delivery's end", w.ends)
	}
	if run := f.load(t); run.ContinuationState != store.ContinuationFinal || run.ContinuationUntil != nil {
		t.Fatalf("run continuation=%q until=%v, want final with no instant", run.ContinuationState, run.ContinuationUntil)
	}
	first := w.endDeadlines[0]
	if first.IsZero() || first.Add(-capacityStoreTimeout).Before(w.parkReturned) {
		t.Fatalf("the final end's deadline %v is not a budget opened after the park returned (%v)", first, w.parkReturned)
	}
}

// The park of a started run waits on the freeze of its program (up to
// programFreezeTimeout): its store budget starts once the freeze returned, so
// a slow freeze never hands the park an expired context. Red when the park's
// context is opened before the freeze.
func TestCapacityPark_TheParkBudgetStartsAfterTheFreeze(t *testing.T) {
	f, blobs, _ := newFrozenFixture(t, "cap-park-budget", startedShape{name: "checkpoint", checkpoint: true})
	blobs.putDelay = 150 * time.Millisecond
	w := &faultyParkStore{RunStore: f.r.cfg.Store}
	f.r.cfg.Store = w
	out, _ := f.deliver(t, 2)
	if out.finalStatus != "capacity_parked" {
		t.Fatalf("delivery 2: %s, want capacity_parked", out.finalStatus)
	}
	parks := f.park.recordedParks()
	if len(parks) != 1 || parks[0].Frozen == nil {
		t.Fatalf("parks %+v, want one carrying the frozen program", parks)
	}
	if w.parkDeadline.IsZero() || w.parkDeadline.Add(-capacityStoreTimeout).Before(blobs.putDone) {
		t.Fatalf("the park's deadline %v opened its %s budget before the freeze's last write returned (%v)", w.parkDeadline, capacityStoreTimeout, blobs.putDone)
	}
}
