package mongo

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Two-client proofs of the capacity transit (ADR-167 C1/C2) on a real
// replica set: client A is a publisher replica admitting newcomers, client B
// the runner that picks a redelivery up and calls store.EnterCapacityTransit —
// the runner's own sequence. Interleavings run through the ledger's eviction
// seam (beforeCredSlotEvict) and the sequence's step seam
// (store.WithCapacityTransitStepSeam); time comes from instants written
// directly, never from sleeps. Skipped without ITERION_TEST_MONGO_URI.

const transitFP = "fp-transit"

func transitCtx() context.Context { return store.WithIdentity(context.Background(), "_test", "_test") }

// transitPair is the two clients on one database.
func transitPair(t *testing.T) (a, b *Store) {
	t.Helper()
	return preparedMongo(t)
}

// stampedRun creates a run in status, stamped with fps, through client s.
func stampedRun(t *testing.T, s *Store, id string, status store.RunStatus, fps ...string) {
	t.Helper()
	ctx := transitCtx()
	if _, err := s.CreateRun(ctx, id, "demo", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = status
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
	if len(fps) > 0 {
		if err := s.SetRunCredStamp(ctx, id, store.RunCredStamp{Fingerprints: fps}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", id, err)
		}
	}
}

// admittedThenParked is run-a as a redelivery finds it: admitted on the key
// (its entry's token returned), then parked failed_resumable by the engine —
// not in transit, so a reconcile may judge it stale.
func admittedThenParked(t *testing.T, a *Store, id string, max int) time.Time {
	t.Helper()
	stampedRun(t, a, id, store.RunStatusQueued, transitFP)
	adm, err := a.ReserveCredSlot(transitCtx(), transitFP, id, max)
	if err != nil || !adm.Granted {
		t.Fatalf("admit %s: %+v %v", id, adm, err)
	}
	r, err := a.LoadRun(transitCtx(), id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = store.RunStatusFailedResumable
	if err := a.SaveRun(transitCtx(), r); err != nil {
		t.Fatalf("park %s: %v", id, err)
	}
	return adm.At
}

func readRow(t *testing.T, s *Store) credSlotDoc {
	t.Helper()
	var row credSlotDoc
	if err := s.credSlots.FindOne(context.Background(), bson.M{"_id": transitFP}).Decode(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return row
}

// toStatus moves a run to status through a whole-document save.
func toStatus(t *testing.T, s *Store, id string, status store.RunStatus) {
	t.Helper()
	r, err := s.LoadRun(transitCtx(), id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = status
	if err := s.SaveRun(transitCtx(), r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
}

// A judges run-a stale (parked, not in transit) and, before its $pull, B
// enters run-a's transit: the re-assertion moved the entry A judged, so the
// pull misses and A is refused. Red when the re-assertion keeps the entry's
// instant: the pull takes it, and A runs beside run-a on a cap of one.
func TestCapacityTransit_EvictionJudgedBeforeTheCASMissesTheReassertedEntry(t *testing.T) {
	a, b := transitPair(t)
	token := admittedThenParked(t, a, "run-a", 1)
	stampedRun(t, a, "run-n", store.RunStatusPausedOperator, transitFP)
	var entry store.CapacityTransitEntry
	entered := false
	a.beforeCredSlotEvict = func(runID string) {
		if runID != "run-a" || entered {
			return
		}
		entered = true
		var err error
		entry, err = store.EnterCapacityTransit(transitCtx(), b, b, "run-a",
			[]store.CapacityHold{{Subject: transitFP, Token: token}},
			store.CapacityTransit{Until: time.Now().Add(5 * time.Minute)})
		if err != nil {
			t.Errorf("EnterCapacityTransit: %v", err)
		}
	}
	adm, err := a.ReserveCredSlot(transitCtx(), transitFP, "run-n", 1)
	if err != nil {
		t.Fatalf("ReserveCredSlot run-n: %v", err)
	}
	if !entered {
		t.Fatal("A never judged run-a stale: the interleaving under test did not happen")
	}
	if !entry.Started || entry.Lost != "" || len(entry.Held) != 1 {
		t.Fatalf("B's entry: %+v, want started and held", entry)
	}
	if adm.Granted {
		t.Fatalf("run-n admitted (%+v): A's pull took the entry run-a's redelivery just proved", adm)
	}
	if row := readRow(t, a); len(row.Holders) != 1 || !row.holds("run-a") {
		t.Fatalf("row %+v, want run-a alone", row.Holders)
	}
}

// Between the transit write and the re-assertion, A's reconcile reads run-a in
// transit — a holder — and keeps it; B then gets Held. Red when the sequence
// re-asserts before it writes the transit: A then reads run-a parked, judges
// the moved entry stale, evicts it and is granted.
func TestCapacityTransit_EvictionBetweenTheStepsSeesAHolder(t *testing.T) {
	a, b := transitPair(t)
	token := admittedThenParked(t, a, "run-a", 1)
	stampedRun(t, a, "run-n", store.RunStatusPausedOperator, transitFP)
	var adm store.CredSlotAdmission
	seamRan := false
	ctx := store.WithCapacityTransitStepSeam(transitCtx(), func() {
		seamRan = true
		var err error
		adm, err = a.ReserveCredSlot(transitCtx(), transitFP, "run-n", 1)
		if err != nil {
			t.Errorf("ReserveCredSlot run-n in the seam: %v", err)
		}
	})
	entry, err := store.EnterCapacityTransit(ctx, b, b, "run-a",
		[]store.CapacityHold{{Subject: transitFP, Token: token}},
		store.CapacityTransit{Until: time.Now().Add(5 * time.Minute)})
	if err != nil {
		t.Fatalf("EnterCapacityTransit: %v", err)
	}
	if !seamRan {
		t.Fatal("the step seam never ran")
	}
	if adm.Granted {
		t.Fatalf("run-n admitted between the steps (%+v): run-a read as no holder", adm)
	}
	if !entry.Started || entry.Lost != "" || len(entry.Held) != 1 {
		t.Fatalf("B's entry: %+v, want started and held", entry)
	}
}

// An entry pulled before the redelivery is not created again by its
// re-assertion: Held false, the row as it was. Red when the re-assertion
// pushes an absent entry.
func TestCapacityTransit_ReassertNeverCreates(t *testing.T) {
	a, b := transitPair(t)
	token := admittedThenParked(t, a, "run-a", 2)
	stampedRun(t, a, "run-other", store.RunStatusQueued, transitFP)
	if adm, err := a.ReserveCredSlot(transitCtx(), transitFP, "run-other", 2); err != nil || !adm.Granted {
		t.Fatalf("admit run-other: %+v %v", adm, err)
	}
	if err := a.ReleaseCredSlot(transitCtx(), transitFP, "run-a", token); err != nil {
		t.Fatalf("pull run-a: %v", err)
	}
	before := readRow(t, a)
	re, err := b.ReassertCredSlot(transitCtx(), transitFP, "run-a", token)
	if err != nil || re.Held {
		t.Fatalf("re-assertion of a pulled entry: %+v %v, want not held", re, err)
	}
	after := readRow(t, a)
	if len(after.Holders) != len(before.Holders) || after.holds("run-a") {
		t.Fatalf("row %+v after the re-assertion, want %+v unchanged", after.Holders, before.Holders)
	}
}

// A transit past its instant holds nothing: A's reconcile evicts run-a and
// the count reads no holder. Red when the transit arm ignores its instant.
func TestCapacityTransit_TransitHoldExpires(t *testing.T) {
	a, b := transitPair(t)
	admittedThenParked(t, a, "run-a", 1)
	if _, err := b.UpdateRunOutcome(transitCtx(), "run-a", store.RunStatusFailedResumable, "",
		store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending, ContinuationUntil: time.Now().Add(-time.Second)},
		[]store.RunStatus{store.RunStatusFailedResumable}); err != nil {
		t.Fatalf("promote run-a with an expired hold: %v", err)
	}
	if n, err := a.CountAliveRunsWithCredFingerprint(transitCtx(), transitFP, ""); err != nil || n != 0 {
		t.Fatalf("alive(%s) = %d/%v, want 0 — the hold expired", transitFP, n, err)
	}
	stampedRun(t, a, "run-n", store.RunStatusPausedOperator, transitFP)
	adm, err := a.ReserveCredSlot(transitCtx(), transitFP, "run-n", 1)
	if err != nil || !adm.Granted {
		t.Fatalf("run-n behind an expired transit: %+v %v, want granted", adm, err)
	}
}

// Problem A of the plan: run-a's place is taken while it is parked, then its
// redelivery writes the transit, and another admission adopts the transit run
// as a holder — by an entry with no admission identity. run-a's re-assertion
// by its own token is then not held, and it parks rather than run beside the
// newcomer on a cap of one. Red when the re-assertion matches on the run alone.
func TestCapacityTransit_TransitRunIsAdoptedButAdoptedEntryIsNotHeld(t *testing.T) {
	a, b := transitPair(t)
	token := admittedThenParked(t, a, "run-a", 1)
	stampedRun(t, a, "run-x", store.RunStatusPausedOperator, transitFP)
	if adm, err := a.ReserveCredSlot(transitCtx(), transitFP, "run-x", 1); err != nil || !adm.Granted {
		t.Fatalf("run-x takes run-a's place: %+v %v", adm, err)
	}
	toStatus(t, a, "run-x", store.RunStatusRunning)
	start, err := b.BeginCapacityTransit(transitCtx(), "run-a", store.CapacityTransit{Until: time.Now().Add(5 * time.Minute)})
	if err != nil || !start.Started {
		t.Fatalf("run-a's transit: %+v %v", start, err)
	}
	stampedRun(t, a, "run-y", store.RunStatusPausedOperator, transitFP)
	if adm, err := a.ReserveCredSlot(transitCtx(), transitFP, "run-y", 1); err != nil || adm.Granted || adm.Held != 2 {
		t.Fatalf("run-y: %+v %v, want refused with run-x and the adopted run-a", adm, err)
	}
	if re, err := b.ReassertCredSlot(transitCtx(), transitFP, "run-a", token); err != nil || re.Held {
		t.Fatalf("run-a's re-assertion of an adopted entry: %+v %v, want not held", re, err)
	}
	// The adoption wrote no identity on run-a's entry.
	if h, ok := readRow(t, a).holder("run-a"); !ok || h.AdmittedAt != nil {
		t.Fatalf("run-a's adopted entry %+v (present=%v), want one with no admitted_at", h, ok)
	}
}

// F6: a cap set while a run is in transit counts it. X runs, Y is in transit
// with no ledger entry (its key had no cap): the first admission on a cap of
// two adopts both and refuses the newcomer. Red when the adoption filter
// drops the transit arm.
func TestCapacityTransit_CapSetDuringTransitCountsTheTransitRun(t *testing.T) {
	a, b := transitPair(t)
	stampedRun(t, a, "run-x", store.RunStatusRunning, transitFP)
	stampedRun(t, a, "run-y", store.RunStatusFailedResumable, transitFP)
	if start, err := b.BeginCapacityTransit(transitCtx(), "run-y", store.CapacityTransit{Until: time.Now().Add(5 * time.Minute)}); err != nil || !start.Started {
		t.Fatalf("run-y's transit: %+v %v", start, err)
	}
	stampedRun(t, a, "run-n1", store.RunStatusPausedOperator, transitFP)
	adm, err := a.ReserveCredSlot(transitCtx(), transitFP, "run-n1", 2)
	if err != nil {
		t.Fatalf("ReserveCredSlot: %v", err)
	}
	if adm.Granted || adm.Held != 2 {
		t.Fatalf("run-n1 on a cap of two over a running and a transit run: %+v, want refused with both adopted", adm)
	}
}

// parkedTransit is run-a in a transit B owns, its prior code given.
func parkedTransit(t *testing.T, a, b *Store, id string, code store.FailureCode) time.Time {
	t.Helper()
	stampedRun(t, a, id, store.RunStatusRunning, transitFP)
	if err := a.FailRunResumable(transitCtx(), id, nil, "drained", code); err != nil {
		t.Fatalf("FailRunResumable %s: %v", id, err)
	}
	until := store.TransitInstant(time.Now().Add(5 * time.Minute))
	if start, err := b.BeginCapacityTransit(transitCtx(), id, store.CapacityTransit{Until: until}); err != nil || !start.Started {
		t.Fatalf("transit of %s: %+v %v", id, start, err)
	}
	return until
}

func capacityPark(until time.Time, charge bool, max int) store.CapacityPark {
	return store.CapacityPark{
		From: store.RunStatusFailedResumable, TransitUntil: until,
		Code: store.FailureCredSlotWait, Error: "parked: place lost", Reason: store.RetryReasonCredSlot,
		RetryAfter: time.Now().Add(store.CredSlotProbeInterval), Charge: charge, MaxAttempts: max,
	}
}

// The park is one write: fenced to the transit it ends (a foreign transit
// parks nothing and writes nothing), and on success the status, the code,
// the armed retry with scheduled_at stamped now, the continuation and the
// attempt land at one version. Red on a two-step park (two versions) and on
// a scheduled_at kept from an earlier arming.
func TestCapacityTransit_ParkIsOneWrite(t *testing.T) {
	a, b := transitPair(t)
	until := parkedTransit(t, a, b, "run-a", store.FailureExecutionFailed)
	// An earlier arming's scheduled_at, as a run that waited before carries.
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Millisecond)
	if _, err := a.runs.UpdateOne(context.Background(), bson.M{"_id": "run-a"}, bson.M{"$set": bson.M{"retry_state.scheduled_at": old}}); err != nil {
		t.Fatalf("seed scheduled_at: %v", err)
	}
	before, err := a.LoadRun(transitCtx(), "run-a")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if parked, _, err := b.ParkRunOnCapacity(transitCtx(), "run-a", capacityPark(until.Add(time.Second), true, 5)); err != nil || parked {
		t.Fatalf("park on a foreign transit: %v %v, want not parked", parked, err)
	}
	if same, _ := a.LoadRun(transitCtx(), "run-a"); same.CASVersion != before.CASVersion || same.ContinuationState != store.ContinuationRedeliveryPending {
		t.Fatalf("a refused park wrote: version %d→%d continuation %q", before.CASVersion, same.CASVersion, same.ContinuationState)
	}
	parked, attempt, err := b.ParkRunOnCapacity(transitCtx(), "run-a", capacityPark(until, true, 5))
	if err != nil || !parked || attempt != 1 {
		t.Fatalf("park: parked=%v attempt=%d err=%v, want parked at attempt 1", parked, attempt, err)
	}
	after, err := a.LoadRun(transitCtx(), "run-a")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if after.CASVersion != before.CASVersion+1 {
		t.Fatalf("version %d→%d, want one write", before.CASVersion, after.CASVersion)
	}
	rs := after.RetryState
	if after.Status != store.RunStatusFailedResumable || after.FailureCode != store.FailureCredSlotWait ||
		after.ContinuationState != store.ContinuationRetryArmed || after.ContinuationUntil != nil ||
		rs == nil || rs.RetryAfter == nil || rs.Attempts != 1 || rs.Reason != store.RetryReasonCredSlot || rs.ScheduledAt == nil {
		t.Fatalf("parked run: status=%s code=%s continuation=%q until=%v retry=%+v", after.Status, after.FailureCode, after.ContinuationState, after.ContinuationUntil, rs)
	}
	if rs.ScheduledAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("scheduled_at = %s, want now — an earlier arming's instant was kept", rs.ScheduledAt)
	}
}

// A charged park respects the run's retry budget; a free one does not, and
// leaves the attempts as they are. Red when the budget filter rides free
// parks too.
func TestCapacityTransit_ChargedParkRespectsBudgetFreeParkIgnoresIt(t *testing.T) {
	a, b := transitPair(t)
	for _, c := range []struct {
		name   string
		charge bool
		parked bool
	}{{"charged", true, false}, {"free", false, true}} {
		t.Run(c.name, func(t *testing.T) {
			id := "run-" + c.name
			until := parkedTransit(t, a, b, id, store.FailureInterrupted)
			if _, err := a.runs.UpdateOne(context.Background(), bson.M{"_id": id}, bson.M{"$set": bson.M{"retry_state.attempts": 5}}); err != nil {
				t.Fatalf("spend the budget: %v", err)
			}
			parked, attempt, err := b.ParkRunOnCapacity(transitCtx(), id, capacityPark(until, c.charge, 5))
			if err != nil || parked != c.parked {
				t.Fatalf("park with the budget spent: parked=%v err=%v, want parked=%v", parked, err, c.parked)
			}
			if c.parked && attempt != 5 {
				t.Fatalf("free park attempt=%d, want the spent budget left at 5", attempt)
			}
		})
	}
}

// The parked launch is one create: status, code, armed retry and
// continuation in the run's first version; a charged park on a spent budget
// writes nothing.
func TestCapacityTransit_SaveParkedLaunchRunArmsInTheCreate(t *testing.T) {
	a, _ := transitPair(t)
	now := time.Now().UTC()
	r := &store.Run{
		ID: "run-launch", WorkflowName: "demo", Status: store.RunStatusFailedResumable,
		FailureCode: store.FailureCredSlotWait, Error: "parked: key full",
		CreatedAt: now, UpdatedAt: now, QueuedAt: &now,
	}
	park := store.CapacityPark{Code: store.FailureCredSlotWait, Reason: store.RetryReasonCredSlot,
		RetryAfter: now.Add(store.CredSlotProbeInterval), Charge: true, MaxAttempts: 5}
	if err := a.SaveParkedLaunchRun(transitCtx(), r, park); err != nil {
		t.Fatalf("SaveParkedLaunchRun: %v", err)
	}
	got, err := a.LoadRun(transitCtx(), "run-launch")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.CASVersion != 1 || got.Status != store.RunStatusFailedResumable || got.FailureCode != store.FailureCredSlotWait || got.ContinuationState != store.ContinuationRetryArmed ||
		got.RetryState == nil || got.RetryState.RetryAfter == nil || got.RetryState.Attempts != 1 {
		t.Fatalf("parked launch: version=%d status=%s code=%q continuation=%q retry=%+v, want version 1 parked on CRED_SLOT_WAIT and armed", got.CASVersion, got.Status, got.FailureCode, got.ContinuationState, got.RetryState)
	}
	if _, err := a.runs.UpdateOne(context.Background(), bson.M{"_id": "run-launch"}, bson.M{"$set": bson.M{"retry_state.attempts": 5}}); err != nil {
		t.Fatalf("spend the budget: %v", err)
	}
	again, err := a.LoadRun(transitCtx(), "run-launch")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if err := a.SaveParkedLaunchRun(transitCtx(), again, park); !errors.Is(err, store.ErrCapacityParkBudgetSpent) {
		t.Fatalf("charged park on a spent budget: %v, want ErrCapacityParkBudgetSpent", err)
	}
}
