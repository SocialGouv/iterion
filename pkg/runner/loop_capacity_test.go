package runner

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The runner's capacity guard, routed on the filesystem store: the transit
// and the ledger are the store's own (both twins host them); the park is a
// fake honouring the Mongo park's fences (parkingStore). These tests prove
// what the runner asks and in which case; the order of the writes and the
// park's single write are proven on Mongo (pkg/store/mongo
// capacity_transit_test.go, and TestCapacityTransitPickupMongo here).

const capFP = "fp-guarded"

// parkingStore is the filesystem store with a capacity park: one park per
// call, fenced to the transit it ends, charged parks bounded by the budget.
type parkingStore struct {
	store.RunStore
	mu       sync.Mutex
	parks    []store.CapacityPark
	attempts map[string]int
}

func (s *parkingStore) Unwrap() store.RunStore { return s.RunStore }

func (s *parkingStore) ParkRunOnCapacity(ctx context.Context, runID string, p store.CapacityPark) (bool, int, error) {
	if err := p.Validate(); err != nil {
		return false, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parks = append(s.parks, p)
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		return false, 0, err
	}
	if r.Status != p.From || (p.From == store.RunStatusFailedResumable &&
		(r.ContinuationState != store.ContinuationRedeliveryPending || r.ContinuationUntil == nil || !r.ContinuationUntil.Equal(p.TransitUntil))) {
		return false, 0, nil
	}
	if s.attempts == nil {
		s.attempts = map[string]int{}
	}
	if p.Charge && s.attempts[runID] >= p.MaxAttempts {
		return false, 0, nil
	}
	if p.Charge {
		s.attempts[runID]++
	}
	if _, err := s.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, p.Error,
		store.RunOutcomeMeta{Code: p.Code, Continuation: store.ContinuationRetryArmed}, []store.RunStatus{p.From}); err != nil {
		return false, 0, err
	}
	return true, s.attempts[runID], nil
}

func (s *parkingStore) SaveParkedLaunchRun(ctx context.Context, r *store.Run, _ store.CapacityPark) error {
	return s.SaveRun(ctx, r)
}

func (s *parkingStore) recordedParks() []store.CapacityPark {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.CapacityPark(nil), s.parks...)
}

func idCtx() context.Context { return store.WithIdentity(context.Background(), "team-1", "u1") }

// guardFixture is a runner on a parking filesystem store with a run admitted
// on capFP (cap 1), then parked failed_resumable on code and promoted to its
// transit — a redelivery's starting point — and the message of that
// admission.
type guardFixture struct {
	r     *Runner
	fs    store.RunStore
	park  *parkingStore
	msg   *queue.RunMessage
	token time.Time
	reg   *metrics.Registry
}

func newGuardFixture(t *testing.T, runID string, code store.FailureCode, prior store.ContinuationState) *guardFixture {
	t.Helper()
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := idCtx()
	if _, err := fs.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.TenantID, r.OwnerID, r.Status = "team-1", "u1", store.RunStatusRunning
	r.Checkpoint = &store.Checkpoint{NodeID: "n1"}
	if err := fs.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := fs.SetRunCredStamp(ctx, runID, store.RunCredStamp{Fingerprints: []string{capFP}}); err != nil {
		t.Fatalf("SetRunCredStamp: %v", err)
	}
	adm, err := store.AsCredSlotStore(fs).ReserveCredSlot(ctx, capFP, runID, 1)
	if err != nil || !adm.Granted {
		t.Fatalf("admission: %+v %v", adm, err)
	}
	if err := fs.FailRunResumable(ctx, runID, r.Checkpoint, "drained", code); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	if prior != "" {
		meta := store.RunOutcomeMeta{Continuation: prior}
		if prior == store.ContinuationRedeliveryPending {
			meta.ContinuationUntil = time.Now().Add(5 * time.Minute)
		}
		if _, err := fs.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, "", meta, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
			t.Fatalf("state %s: %v", prior, err)
		}
	}
	park := &parkingStore{RunStore: fs}
	reg := metrics.New()
	return &guardFixture{
		r:    &Runner{cfg: Config{Store: park, Logger: iterlog.Nop(), Metrics: reg}, maxDeliverOverride: 8},
		fs:   fs,
		park: park,
		msg: &queue.RunMessage{
			RunID: runID, TenantID: "team-1", OwnerID: "u1",
			PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
			Capacity:       &queue.CapacityAdmission{Holds: []store.CapacityHold{{Subject: capFP, Token: adm.At}}},
		},
		token: adm.At,
		reg:   reg,
	}
}

func (f *guardFixture) load(t *testing.T) *store.Run {
	t.Helper()
	r, err := f.fs.LoadRun(idCtx(), f.msg.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return r
}

// entryAt is the instant of the run's entry on capFP, zero when it has none.
func (f *guardFixture) entryAt(t *testing.T) time.Time {
	t.Helper()
	fsStore := f.fs.(*store.FilesystemRunStore)
	re, err := fsStore.ReassertCredSlot(idCtx(), capFP, f.msg.RunID, f.token)
	if err != nil {
		t.Fatalf("probe the entry: %v", err)
	}
	return re.At
}

// evict takes the run's place: its entry pulled, as a reconcile that judged
// it stale while it was parked does.
func (f *guardFixture) evict(t *testing.T) {
	t.Helper()
	ledger := store.AsCredSlotStore(f.fs)
	re, err := ledger.ReassertCredSlot(idCtx(), capFP, f.msg.RunID, f.token)
	if err != nil || !re.Held {
		t.Fatalf("the run's entry before the eviction: %+v %v", re, err)
	}
	if err := ledger.ReleaseCredSlot(idCtx(), capFP, f.msg.RunID, re.At); err != nil {
		t.Fatalf("evict: %v", err)
	}
}

func (f *guardFixture) pickup(t *testing.T, delivered int) (*capacityGuard, preconditionOutcome, bool) {
	t.Helper()
	return f.r.capacityAtPickup(context.Background(), f.msg, nil, delivered, iterlog.Nop())
}

// A held place: the delivery proceeds, the run is in a transit this delivery
// owns, and the entry moved past the admission's instant while the message's
// token stays the admission's. Red when the re-assertion is skipped.
func TestCapacityPickup_HeldPlaceResumes(t *testing.T) {
	f := newGuardFixture(t, "cap-held", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	g, out, proceed := f.pickup(t, 2)
	if !proceed || g == nil {
		t.Fatalf("pickup: proceed=%v out=%+v, want the delivery to proceed", proceed, out)
	}
	run := f.load(t)
	owned, _, held := g.ownedTransit()
	if !run.InCapacityTransit(time.Now()) || run.ContinuationUntil == nil || !run.ContinuationUntil.Equal(owned) {
		t.Fatalf("run continuation=%q until=%v, want the transit this delivery owns (%s)", run.ContinuationState, run.ContinuationUntil, owned)
	}
	if len(held) != 1 || !held[0].At.After(f.token) {
		t.Fatalf("held %+v, want the entry moved past the admission's %s", held, f.token)
	}
	if got := f.msg.Capacity.Holds[0].Token; !got.Equal(f.token) {
		t.Fatalf("message token %s, want the admission's %s unchanged", got, f.token)
	}
	if n := counterValue(t, f.reg.CapacityReassertTotal.WithLabelValues(metrics.CapacityStageDelivery, metrics.CapacityHeld, metrics.CapacityKindKey)); n != 1 {
		t.Fatalf("held count %v, want 1", n)
	}
}

// A lost place: the delivery is acked without executing, the run parked on
// CRED_SLOT_WAIT with its retry armed from the transit this delivery owns,
// free for an infrastructure source, and the pool lease of the delivery that
// executed nothing closed. Red when the pickup does not run the delivery
// stage, or does not close the lease.
func TestCapacityPickup_LostPlaceParksAndClosesThePoolLease(t *testing.T) {
	f := newGuardFixture(t, "cap-lost", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	pool := newPoolHarnessForRun(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1}, f.msg.RunID)
	f.r.cfg.CredPool = pool.broker
	f.msg.PublishedAtRFC = time.Now().UTC().Add(time.Millisecond).Format(time.RFC3339Nano)
	f.evict(t)
	_, out, proceed := f.pickup(t, 2)
	if proceed || out.action != actionAck {
		t.Fatalf("pickup: proceed=%v action=%v, want an ack without executing", proceed, out.action)
	}
	parks := f.park.recordedParks()
	if len(parks) != 1 || parks[0].From != store.RunStatusFailedResumable || parks[0].TransitUntil.IsZero() || parks[0].Charge {
		t.Fatalf("parks %+v, want one free park from the delivery's transit", parks)
	}
	run := f.load(t)
	if run.Status != store.RunStatusFailedResumable || run.FailureCode != store.FailureCredSlotWait || run.ContinuationState != store.ContinuationRetryArmed {
		t.Fatalf("run %s/%s continuation %q, want parked CRED_SLOT_WAIT retry_armed", run.Status, run.FailureCode, run.ContinuationState)
	}
	leases, err := pool.leases.ListByDonor(context.Background(), "donor", 10)
	if err != nil || len(leases) != 1 || !leases[0].Closed {
		t.Fatalf("pool lease %+v (err %v), want closed: the delivery executed nothing and will not come back", leases, err)
	}
}

// A message whose kind is a resume is gated like a launch redelivered onto an
// executed run: the guard keys on the status the transit write finds. Red
// when the guard reads the message's kind.
func TestCapacityPickup_ResumeMessageIsGated(t *testing.T) {
	f := newGuardFixture(t, "cap-resume", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.msg.Resume = &queue.ResumeSpec{PriorStatus: store.RunStatusFailedResumable}
	f.evict(t)
	if _, out, proceed := f.pickup(t, 2); proceed || out.action != actionAck {
		t.Fatalf("pickup of a resume message: proceed=%v action=%v, want parked and acked", proceed, out.action)
	}
	if len(f.park.recordedParks()) != 1 {
		t.Fatalf("parks %+v, want one", f.park.recordedParks())
	}
}

// failingReassert answers every re-assertion with a store error.
type failingReassert struct {
	*parkingStore
}

func (s *failingReassert) Unwrap() store.RunStore { return s.parkingStore }
func (s *failingReassert) ReserveCredSlot(ctx context.Context, fp, runID string, max int) (store.CredSlotAdmission, error) {
	return store.AsCredSlotStore(s.parkingStore).ReserveCredSlot(ctx, fp, runID, max)
}
func (s *failingReassert) ReassertCredSlot(context.Context, string, string, time.Time) (store.CredSlotReassertion, error) {
	return store.CredSlotReassertion{}, errors.New("ledger unavailable")
}
func (s *failingReassert) ReleaseCredSlot(ctx context.Context, fp, runID string, at time.Time) error {
	return store.AsCredSlotStore(s.parkingStore).ReleaseCredSlot(ctx, fp, runID, at)
}
func (s *failingReassert) SettleCredSlotAdmission(ctx context.Context, runID string, at time.Time) error {
	return store.AsCredSlotStore(s.parkingStore).SettleCredSlotAdmission(ctx, runID, at)
}
func (s *failingReassert) ParkRunOnCapacity(ctx context.Context, runID string, p store.CapacityPark) (bool, int, error) {
	return s.parkingStore.ParkRunOnCapacity(ctx, runID, p)
}
func (s *failingReassert) SaveParkedLaunchRun(ctx context.Context, r *store.Run, p store.CapacityPark) error {
	return s.parkingStore.SaveParkedLaunchRun(ctx, r, p)
}

// A ledger that cannot answer re-offers the delivery after a delay with the
// run left in its transit, and parks it on the last delivery: it never
// resumes unproven. Red when the error reads as held, or naks into nothing on
// the last delivery.
func TestCapacityPickup_LedgerErrorNaksThenParksOnTheLastDelivery(t *testing.T) {
	f := newGuardFixture(t, "cap-ledger", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.r.cfg.Store = &failingReassert{parkingStore: f.park}
	_, out, proceed := f.pickup(t, 2)
	if proceed || out.action != actionNakDelayed || out.delay != capacityReassertNakDelay {
		t.Fatalf("pickup: proceed=%v action=%v delay=%s, want a delayed nak", proceed, out.action, out.delay)
	}
	if run := f.load(t); !run.InCapacityTransit(time.Now()) {
		t.Fatalf("run continuation=%q until=%v, want still in transit for the redelivery", run.ContinuationState, run.ContinuationUntil)
	}
	_, out, proceed = f.pickup(t, 8)
	if proceed || out.action != actionAck {
		t.Fatalf("last delivery: proceed=%v action=%v, want parked and acked", proceed, out.action)
	}
	if run := f.load(t); run.FailureCode != store.FailureCredSlotWait || run.ContinuationState != store.ContinuationRetryArmed {
		t.Fatalf("last delivery left %s/%q, want parked on CRED_SLOT_WAIT", run.FailureCode, run.ContinuationState)
	}
}

// An armed retry is the sweeper's: the delivery is acked, nothing resumes,
// and the document is not written. Red when the transit write ignores the
// continuation.
func TestCapacityPickup_RetryArmedIsAckedUntouched(t *testing.T) {
	f := newGuardFixture(t, "cap-armed", store.FailureCredSlotWait, store.ContinuationRetryArmed)
	before := f.load(t)
	_, out, proceed := f.pickup(t, 2)
	if proceed || out.action != actionAck {
		t.Fatalf("pickup on retry_armed: proceed=%v action=%v, want acked", proceed, out.action)
	}
	after := f.load(t)
	if after.CASVersion != before.CASVersion || after.ContinuationState != store.ContinuationRetryArmed || after.ContinuationUntil != nil {
		t.Fatalf("retry_armed run written: version %d→%d continuation %q until %v", before.CASVersion, after.CASVersion, after.ContinuationState, after.ContinuationUntil)
	}
	if len(f.park.recordedParks()) != 0 {
		t.Fatalf("parks %+v, want none", f.park.recordedParks())
	}
}

// The orphan road: the adoption under the lock writes the transit with the
// continuation, and the pickup that follows proves the places from it. Red
// when the adoption's promote writes no instant.
func TestCapacityPickup_OrphanAdoptionWritesTheTransit(t *testing.T) {
	f := newGuardFixture(t, "cap-orphan", store.FailureInterrupted, "")
	ctx := idCtx()
	run := f.load(t)
	run.Status = store.RunStatusRunning
	if err := f.fs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun running: %v", err)
	}
	pre := f.load(t)
	f.r.lockLivenessOverride = true
	out := f.r.adoptRunningUnderLock(f.msg, pre, &fakeDelivery{delivered: 2}, time.Now().Add(runningAdoptionFloor+time.Minute))
	if !out.proceed {
		t.Fatalf("adoption: %+v, want proceed", out)
	}
	if adopted := f.load(t); !adopted.InCapacityTransit(time.Now()) {
		t.Fatalf("adopted run continuation=%q until=%v, want in transit", adopted.ContinuationState, adopted.ContinuationUntil)
	}
	g, _, proceed := f.pickup(t, 2)
	if !proceed || g == nil {
		t.Fatal("pickup after the adoption did not proceed")
	}
	if _, _, held := g.ownedTransit(); len(held) != 1 {
		t.Fatalf("held %+v, want the place proven", held)
	}
}

// The claim guard on a place released between the pickup and the claim: the
// claim is refused with the place lost, the run left failed_resumable, and
// the delivery's after-engine step parks it. Red when the guard skips the
// re-assertion.
func TestCapacityClaimGuard_ReleasedPlaceIsRefusedThenParked(t *testing.T) {
	f := newGuardFixture(t, "cap-claim", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	g, _, proceed := f.pickup(t, 2)
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
	if run := f.load(t); run.Status != store.RunStatusFailedResumable || !run.InCapacityTransit(time.Now()) {
		t.Fatalf("run %s continuation %q, want failed_resumable in transit after a refused claim", run.Status, run.ContinuationState)
	}
	d := &fakeDelivery{delivered: 2}
	status, handled := g.afterEngine(context.Background(), err, d, nil)
	if !handled || d.acks != 1 || status != "capacity_parked" {
		t.Fatalf("after engine: handled=%v status=%q acks=%d, want parked and acked", handled, status, d.acks)
	}
}

// The claim guard writes nothing to the run document (F3: the quota claim
// fences on the version it reads). Red when the guard refreshes the transit.
func TestCapacityClaimGuard_WritesNothing(t *testing.T) {
	f := newGuardFixture(t, "cap-claim-nowrite", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	g, _, proceed := f.pickup(t, 2)
	if !proceed {
		t.Fatal("pickup did not proceed")
	}
	before := f.load(t)
	if err := g.claimGuard(context.Background(), before); err != nil {
		t.Fatalf("claim guard: %v", err)
	}
	if after := f.load(t); after.CASVersion != before.CASVersion {
		t.Fatalf("version %d→%d, want the claim guard to write nothing", before.CASVersion, after.CASVersion)
	}
}

// A hold within the claim margin of its end refuses the claim as expired.
func TestCapacityClaimGuard_HoldTooCloseToItsEndRefuses(t *testing.T) {
	f := newGuardFixture(t, "cap-claim-expired", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	g := f.r.newCapacityGuard(f.msg, 2, iterlog.Nop())
	run := f.load(t)
	soon := time.Now().Add(capacityClaimMargin / 2)
	run.ContinuationUntil = &soon
	if err := g.claimGuard(context.Background(), run); !errors.Is(err, store.ErrCapacityLedgerUnavailable) {
		t.Fatalf("claim guard on a hold ending in %s: %v, want refused", capacityClaimMargin/2, err)
	}
}

// A hold shorter than four claim margins is held to a quarter of itself: the
// claim right after the pickup that wrote it passes, a hold within that
// quarter of its end is still refused. Red when every hold is held to the
// fixed margin — no guarded redelivery under a short hold could ever claim.
func TestCapacityClaimGuard_AShortHoldIsHeldToAQuarterOfItself(t *testing.T) {
	f := newGuardFixture(t, "cap-claim-short", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.r.cfg.RedeliveryHoldTTL = 20 * time.Second
	g, out, proceed := f.pickup(t, 2)
	if !proceed {
		t.Fatalf("pickup: %+v, want the held place to proceed", out)
	}
	if err := g.claimGuard(context.Background(), f.load(t)); err != nil {
		t.Fatalf("claim right after the pickup under a 20s hold: %v, want it to pass", err)
	}
	run := f.load(t)
	soon := time.Now().Add(2 * time.Second)
	run.ContinuationUntil = &soon
	if err := g.claimGuard(context.Background(), run); !errors.Is(err, store.ErrCapacityLedgerUnavailable) {
		t.Fatalf("claim on a 20s hold ending in 2s: %v, want refused", err)
	}
}

// A delivery that resumed nothing ends the transit it owns: the continuation
// it replaced put back (final stays final, nothing stays nothing) and its
// places given back. Handed back to the queue, it ends nothing. Red when the
// end is dropped.
func TestCapacityEnd_RestoresWhatTheTransitReplaced(t *testing.T) {
	for _, prior := range []store.ContinuationState{store.ContinuationFinal, ""} {
		t.Run("from "+string(prior), func(t *testing.T) {
			f := newGuardFixture(t, "cap-end-"+string(prior), store.FailureInterrupted, prior)
			g, _, proceed := f.pickup(t, 2)
			if !proceed {
				t.Fatal("pickup did not proceed")
			}
			g.endUnlessRedelivered(true)
			if run := f.load(t); !run.InCapacityTransit(time.Now()) {
				t.Fatal("a delivery handed back to the queue ended its transit")
			}
			g.endUnlessRedelivered(false)
			run := f.load(t)
			if run.ContinuationState != prior || run.ContinuationUntil != nil {
				t.Fatalf("after the end: continuation=%q until=%v, want %q and no instant", run.ContinuationState, run.ContinuationUntil, prior)
			}
			if at := f.entryAt(t); !at.IsZero() {
				t.Fatalf("the place is still held (entry at %s) by a delivery that resumed nothing", at)
			}
			if n := counterValue(t, f.reg.CapacityTransitEndTotal.WithLabelValues(metrics.CapacityTransitRestored)); n != 1 {
				t.Fatalf("restored count %v, want 1", n)
			}
		})
	}
}

// The engine's park starts the hold: an interruption the queue will redeliver
// puts the run in transit at the park itself, so an admission during the
// runner's post-engine steps keeps it as a holder. Not on the last delivery,
// not for another code. Red when the hook writes nothing.
func TestCapacityParkHook_StartsTheHoldAtTheEnginePark(t *testing.T) {
	f := newGuardFixture(t, "cap-hook", store.FailureInterrupted, "")
	g := f.r.newCapacityGuard(f.msg, 2, iterlog.Nop())
	g.onResumablePark(context.Background(), store.FailureExecutionFailed)
	if run := f.load(t); run.InCapacityTransit(time.Now()) {
		t.Fatal("a non-interruption park started a transit")
	}
	last := f.r.newCapacityGuard(f.msg, 8, iterlog.Nop())
	last.onResumablePark(context.Background(), store.FailureInterrupted)
	if run := f.load(t); run.InCapacityTransit(time.Now()) {
		t.Fatal("the last delivery's park started a transit nothing redelivers")
	}
	g.onResumablePark(context.Background(), store.FailureInterrupted)
	if run := f.load(t); !run.InCapacityTransit(time.Now()) {
		t.Fatalf("run continuation=%q until=%v, want in transit from the engine's park", run.ContinuationState, run.ContinuationUntil)
	}
	ledger := store.AsCredSlotStore(f.fs)
	credSlotRunFS(t, f.fs, "cap-hook-newcomer")
	if adm, err := ledger.ReserveCredSlot(idCtx(), capFP, "cap-hook-newcomer", 1); err != nil || adm.Granted {
		t.Fatalf("an admission during the post-engine steps: %+v %v, want refused — the parked run holds its place", adm, err)
	}
}

// credSlotRunFS creates a waiting run stamped on capFP.
func credSlotRunFS(t *testing.T, fs store.RunStore, id string) {
	t.Helper()
	ctx := idCtx()
	if _, err := fs.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r, err := fs.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = store.RunStatusPausedOperator
	if err := fs.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
	if err := fs.SetRunCredStamp(ctx, id, store.RunCredStamp{Fingerprints: []string{capFP}}); err != nil {
		t.Fatalf("SetRunCredStamp %s: %v", id, err)
	}
}

// F14: a run that came back from an execution failure pays an attempt for
// each lost place, so a fail→lose→resume loop stops at its budget, the run
// left final; an infrastructure source parks free. Red when every park is
// free.
func TestCapacityPark_ChargedForExecutionFailures(t *testing.T) {
	f := newGuardFixture(t, "cap-charged", store.FailureExecutionFailed, store.ContinuationRedeliveryPending)
	for cycle := 1; cycle <= 6; cycle++ {
		f.evict(t)
		_, _, proceed := f.pickup(t, 2)
		if proceed {
			t.Fatalf("cycle %d: a lost place proceeded", cycle)
		}
		run := f.load(t)
		if cycle <= 5 {
			if run.ContinuationState != store.ContinuationRetryArmed {
				t.Fatalf("cycle %d: continuation %q, want parked and armed", cycle, run.ContinuationState)
			}
		} else if run.ContinuationState != store.ContinuationFinal {
			t.Fatalf("cycle %d: continuation %q, want final once the budget is spent", cycle, run.ContinuationState)
		}
		// The sweeper's re-admission resumes it, the engine fails it again,
		// and its redelivery finds its place gone once more.
		adm, err := store.AsCredSlotStore(f.fs).ReserveCredSlot(idCtx(), capFP, f.msg.RunID, 1)
		if err != nil || !adm.Granted {
			t.Fatalf("cycle %d: re-admission: %+v %v", cycle, adm, err)
		}
		f.token = adm.At
		f.msg.Capacity.Holds[0].Token = adm.At
		if _, err := f.fs.UpdateRunOutcome(idCtx(), f.msg.RunID, store.RunStatusFailedResumable, "failed again",
			store.RunOutcomeMeta{Code: store.FailureExecutionFailed, Continuation: store.ContinuationRedeliveryPending, ContinuationUntil: time.Now().Add(5 * time.Minute)},
			[]store.RunStatus{store.RunStatusFailedResumable}); err != nil {
			t.Fatalf("cycle %d: fail again: %v", cycle, err)
		}
	}
	parks := f.park.recordedParks()
	if len(parks) != 6 || !parks[0].Charge {
		t.Fatalf("parks %+v, want six charged parks", parks)
	}
	if !freeCapacityParkSource(store.FailureInterrupted) || freeCapacityParkSource(store.FailureExecutionFailed) {
		t.Fatal("the source classification moved")
	}
}

// A message from a publisher older than the guard proceeds unguarded and is
// counted legacy; an admission that reserved nothing proceeds unguarded and
// is not. Red when nil is read as empty.
func TestCapacityPickup_LegacyMessageIsCounted(t *testing.T) {
	f := newGuardFixture(t, "cap-legacy", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.msg.Capacity = nil
	if g, _, proceed := f.pickup(t, 2); g != nil || !proceed {
		t.Fatalf("legacy message: guard=%v proceed=%v, want unguarded", g != nil, proceed)
	}
	f.msg.Capacity = &queue.CapacityAdmission{}
	if g, _, proceed := f.pickup(t, 2); g != nil || !proceed {
		t.Fatalf("empty admission: guard=%v proceed=%v, want unguarded", g != nil, proceed)
	}
	if n := counterValue(t, f.reg.CapacityReassertTotal.WithLabelValues(metrics.CapacityStageDelivery, metrics.CapacityLegacy, "none")); n != 1 {
		t.Fatalf("legacy count %v, want 1 (the nil message only)", n)
	}
}

// F1 (the T4a residual, measured). A delivery that resumes a run outside its
// transit — the hold lapsed, or none was ever stamped — while the run's stamp
// names subjects its message holds no place for (a key or a gateway capped
// after the admission, or never capped) is counted unheld_out_of_transit,
// once per such subject and kind, and proceeds as before: nothing re-asserts
// those subjects. Both shapes are counted: an admission that reserved
// nothing (capacity:{}, unguarded) and one whose holds cover only part of
// the stamp (guarded, its holds proven). An operator-paused run redelivered
// on an admission that held nothing holds no place either (the pause is not
// a holder), and is counted the same. A run still in its transit, a queued
// launch, a legacy message and a stamp the holds cover are not. Red when the
// measurement keys on an empty admission alone (the partial shape is
// missed), ignores the transit (a delivery the hold still covers is
// counted), or keys on failed_resumable instead of the holder predicate (the
// paused run is missed).
func TestCapacityPickup_AnUnheldSubjectOutOfTransitIsCounted(t *testing.T) {
	const capped, gw = "fp-capped-after-the-admission", "gw:llm.example.test"
	lapsed := time.Now().Add(-time.Second)
	live := time.Now().Add(5 * time.Minute)
	for _, tc := range []struct {
		name string
		// empty: the admission reserved nothing (capacity:{}); otherwise it
		// holds capFP.
		empty   bool
		until   *time.Time
		legacy  bool
		queued  bool
		paused  bool
		stamp   []string
		subject []string
		want    float64
	}{
		{name: "empty admission, hold lapsed", empty: true, until: &lapsed, stamp: []string{capFP, capped}, want: 2},
		{name: "empty admission, in its transit", empty: true, until: &live, stamp: []string{capFP, capped}},
		{name: "partial holds, hold lapsed", until: &lapsed, stamp: []string{capFP, capped}, want: 1},
		{name: "partial holds, in its transit", until: &live, stamp: []string{capFP, capped}},
		{name: "partial holds, no hold ever stamped", stamp: []string{capFP, capped}, want: 1},
		{name: "a gateway subject, hold lapsed", until: &lapsed, stamp: []string{capFP}, subject: []string{gw}, want: 1},
		{name: "holds cover the stamp, hold lapsed", until: &lapsed, stamp: []string{capFP}},
		{name: "a queued launch", empty: true, queued: true, stamp: []string{capFP, capped}},
		{name: "an operator-paused run, empty admission", empty: true, paused: true, stamp: []string{capFP, capped}, want: 2},
		{name: "a legacy message", legacy: true, until: &lapsed, stamp: []string{capFP, capped}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "cap-unheld", store.FailureInterrupted, "")
			ctx := idCtx()
			if err := f.fs.SetRunCredStamp(ctx, f.msg.RunID, store.RunCredStamp{Fingerprints: tc.stamp, CapacitySubjects: tc.subject, CapacityStamped: true}); err != nil {
				t.Fatalf("SetRunCredStamp: %v", err)
			}
			meta := store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending}
			if tc.until != nil {
				meta.ContinuationUntil = *tc.until
			}
			if _, err := f.fs.UpdateRunOutcome(ctx, f.msg.RunID, store.RunStatusFailedResumable, "", meta, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
				t.Fatalf("park the run: %v", err)
			}
			if tc.queued || tc.paused {
				run := f.load(t)
				run.Status, run.ContinuationState, run.ContinuationUntil = store.RunStatusQueued, "", nil
				if tc.paused {
					run.Status = store.RunStatusPausedOperator
				}
				if err := f.fs.SaveRun(ctx, run); err != nil {
					t.Fatalf("move the run to %s: %v", run.Status, err)
				}
			}
			switch {
			case tc.legacy:
				f.msg.Capacity = nil
			case tc.empty:
				f.msg.Capacity = &queue.CapacityAdmission{}
			}
			pre := f.load(t)
			g, out, proceed := f.r.capacityAtPickup(context.Background(), f.msg, pre, 2, iterlog.Nop())
			if !proceed {
				t.Fatalf("pickup: %+v, want the delivery to proceed — the measurement changes nothing", out)
			}
			if guarded := g != nil; guarded != (!tc.empty && !tc.legacy) {
				t.Fatalf("guarded %v, want %v", guarded, !tc.empty && !tc.legacy)
			}
			var got float64
			for _, kind := range []string{metrics.CapacityKindKey, metrics.CapacityKindGateway} {
				got += counterValue(t, f.reg.CapacityReassertTotal.WithLabelValues(metrics.CapacityStageDelivery, metrics.CapacityUnheldOutOfTransit, kind))
			}
			if got != tc.want {
				t.Fatalf("unheld_out_of_transit %v, want %v", got, tc.want)
			}
			if len(tc.subject) > 0 && tc.want > 0 {
				if n := counterValue(t, f.reg.CapacityReassertTotal.WithLabelValues(metrics.CapacityStageDelivery, metrics.CapacityUnheldOutOfTransit, metrics.CapacityKindGateway)); n != tc.want {
					t.Fatalf("gateway count %v, want %v", n, tc.want)
				}
			}
		})
	}
}

// F2 (the hold's coverage, as the docs state it — store.RedeliveryHoldTTLFloor,
// docs/environment-variables.md, ADR-167's T4a amendment). The floor covers
// the delayed re-offers of a delivery that executed, the longest 3 minutes,
// plus the running-adoption floor. It does not cover the held-lease nak,
// which comes before the guard: with the defaults that nak stays within the
// default hold up to the fourth delivery, passes it from the fifth on, and
// reaches about an hour. Red when a re-offer or a floor moves past what the
// docs say the hold covers, or the held-lease nak's reach moves.
func TestRedeliveryHold_CoversThePostExecutionReoffersNotTheHeldLease(t *testing.T) {
	reoffers := []time.Duration{sandboxSetupTimeoutNakDelay, sandboxCapacityNakDelay, attachmentsFetchNakDelay}
	longest := slices.Max(reoffers)
	if longest != 3*time.Minute {
		t.Fatalf("the longest post-execution re-offer is %s, the docs say 3 min", longest)
	}
	if longest+runningAdoptionFloor > store.RedeliveryHoldTTLFloor {
		t.Fatalf("re-offer %s plus the adoption floor %s pass the hold's floor %s", longest, runningAdoptionFloor, store.RedeliveryHoldTTLFloor)
	}
	held := func(delivered int) time.Duration {
		return natsq.HeldLockRetryDelay(natsq.DefaultLockTTL, LeaseUnwindCeiling, natsq.DefaultStreamMaxRetry, delivered)
	}
	if d4, d5 := held(4), held(5); d4 > store.DefaultRedeliveryHoldTTL || d5 <= store.DefaultRedeliveryHoldTTL {
		t.Fatalf("held-lease naks %s (delivery 4) and %s (delivery 5), want the default hold %s passed from the fifth delivery on", d4, d5, store.DefaultRedeliveryHoldTTL)
	}
	if last := held(natsq.DefaultStreamMaxRetry - 1); last < 50*time.Minute || last > time.Hour {
		t.Fatalf("the longest held-lease nak is %s, the docs say about an hour", last)
	}
}

// The transit's end is the writer's clock plus the configured hold, in the
// precision every store keeps: the park hook's write lands until − now ≈ TTL.
// Red when the hold is scaled (a unit error).
func TestCapacityTransit_UntilIsNowPlusTheHold(t *testing.T) {
	f := newGuardFixture(t, "cap-ttl", store.FailureInterrupted, "")
	f.r.cfg.RedeliveryHoldTTL = 7 * time.Minute
	g := f.r.newCapacityGuard(f.msg, 2, iterlog.Nop())
	before := time.Now()
	g.onResumablePark(context.Background(), store.FailureInterrupted)
	after := time.Now()
	run := f.load(t)
	if run.ContinuationUntil == nil {
		t.Fatal("no transit written")
	}
	lo := before.Add(7 * time.Minute).Add(-time.Millisecond)
	hi := after.Add(7 * time.Minute).Add(time.Millisecond)
	if run.ContinuationUntil.Before(lo) || run.ContinuationUntil.After(hi) {
		t.Fatalf("until %s, want now + 7m (between %s and %s)", run.ContinuationUntil, lo, hi)
	}
}

// failingBegin answers every transit write with a store error.
type failingBegin struct {
	*parkingStore
}

func (s *failingBegin) Unwrap() store.RunStore { return s.parkingStore }
func (s *failingBegin) BeginCapacityTransit(context.Context, string, store.CapacityTransit) (store.CapacityTransitStart, error) {
	return store.CapacityTransitStart{}, errors.New("runs collection unavailable")
}
func (s *failingBegin) EndCapacityTransit(ctx context.Context, runID string, until time.Time, restore store.ContinuationState) (bool, error) {
	return store.AsCapacityTransitStore(s.parkingStore).EndCapacityTransit(ctx, runID, until, restore)
}

// A last delivery whose transit write fails has no transit to park from: it
// is acked, and the run's continuation says nobody resumes it rather than the
// redelivery_pending its previous delivery promised. Red when the stale
// continuation is left.
func TestCapacityPickup_UnprovenLastDeliveryWithNoTransitIsFinal(t *testing.T) {
	f := newGuardFixture(t, "cap-unowned", store.FailureInterrupted, store.ContinuationRedeliveryPending)
	f.r.cfg.Store = &failingBegin{parkingStore: f.park}
	_, out, proceed := f.pickup(t, 8)
	if proceed || out.action != actionAck {
		t.Fatalf("last delivery: proceed=%v action=%v, want acked", proceed, out.action)
	}
	if run := f.load(t); run.ContinuationState != store.ContinuationFinal || run.ContinuationUntil != nil {
		t.Fatalf("run continuation %q until %v, want final with no instant", run.ContinuationState, run.ContinuationUntil)
	}
}
