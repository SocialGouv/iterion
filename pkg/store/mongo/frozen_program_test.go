package mongo

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// The frozen-program record on a real replica set (ADR-167 T4a): written by
// the capacity park's own single write, bound to the attempt marker that
// write found, untouched by every other writer of the run, and kept by a
// whole-document save. Skipped without ITERION_TEST_MONGO_URI.

// frozenRecord is the record a runner hands the park: everything but the
// attempt marker, which the park reads off the document.
func frozenRecord() *store.FrozenProgram {
	return &store.FrozenProgram{
		Key: "ir/run-a-bundle-program-d1.json", Digest: "d1", WorkflowHash: "h1",
		Source: store.FrozenProgramSourceCapacity, FrozenAt: time.Now().UTC().Truncate(time.Millisecond),
	}
}

// startedInTransit is a started run (a checkpoint, the hash h1, the attempt
// marker q1) parked by an interruption and in a transit client b owns.
func startedInTransit(t *testing.T, a, b *Store, id string) (until, q1 time.Time) {
	t.Helper()
	stampedRun(t, a, id, store.RunStatusRunning, transitFP)
	if err := a.FailRunResumable(transitCtx(), id, &store.Checkpoint{NodeID: "n1"}, "drained", store.FailureInterrupted); err != nil {
		t.Fatalf("FailRunResumable %s: %v", id, err)
	}
	q1 = time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if _, err := a.runs.UpdateOne(context.Background(), bson.M{"_id": id}, bson.M{"$set": bson.M{"queued_at": q1, "workflow_hash": "h1"}}); err != nil {
		t.Fatalf("seed queued_at and hash: %v", err)
	}
	until = store.TransitInstant(time.Now().Add(5 * time.Minute))
	if start, err := b.BeginCapacityTransit(transitCtx(), id, store.CapacityTransit{Until: until}); err != nil || !start.Started {
		t.Fatalf("transit of %s: %+v %v", id, start, err)
	}
	return until, q1
}

func frozenPark(until time.Time, fp *store.FrozenProgram) store.CapacityPark {
	p := capacityPark(until, false, 5)
	p.Frozen = fp
	return p
}

// F1. The park writes the record in its own single write: the record, the
// status, the code, the armed retry and the continuation land at one version,
// and the record's attempt marker is the document's queued_at — the caller
// hands none. Red when the record is written by a second update, or when the
// pipeline leaves queued_at out.
func TestFrozenProgram_ParkWritesTheRecordInItsOneWrite(t *testing.T) {
	a, b := transitPair(t)
	until, q1 := startedInTransit(t, a, b, "run-a")
	before, err := a.LoadRun(transitCtx(), "run-a")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	fp := frozenRecord()
	if parked, _, err := b.ParkRunOnCapacity(transitCtx(), "run-a", frozenPark(until, fp)); err != nil || !parked {
		t.Fatalf("park: parked=%v err=%v", parked, err)
	}
	after, err := a.LoadRun(transitCtx(), "run-a")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if after.CASVersion != before.CASVersion+1 {
		t.Fatalf("version %d→%d, want the record in the park's one write", before.CASVersion, after.CASVersion)
	}
	got := after.FrozenProgram
	if got == nil {
		t.Fatal("the park wrote no frozen program")
	}
	if !got.QueuedAt.Equal(q1) {
		t.Fatalf("record queued_at = %v, want the document's %v", got.QueuedAt, q1)
	}
	want := *fp
	want.QueuedAt = q1
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("record = %+v, want %+v", *got, want)
	}
	if after.ContinuationState != store.ContinuationRetryArmed || after.RetryState == nil || after.RetryState.RetryAfter == nil || after.FailureCode != store.FailureCredSlotWait {
		t.Fatalf("park: continuation %q retry %+v code %s", after.ContinuationState, after.RetryState, after.FailureCode)
	}
	if _, v := after.ReadmitFrozen(); v != store.FrozenBound {
		t.Fatalf("the parked run reads %s, want bound", v)
	}
}

// F2. A park that froze nothing removes the record an earlier park left: the
// run no longer carries a program to re-admit. Red when the park leaves the
// field alone on a nil program.
func TestFrozenProgram_ParkWithoutAProgramRemovesTheOldRecord(t *testing.T) {
	a, b := transitPair(t)
	until, q1 := startedInTransit(t, a, b, "run-a")
	old := frozenRecord()
	old.QueuedAt = q1
	if _, err := a.runs.UpdateOne(context.Background(), bson.M{"_id": "run-a"}, bson.M{"$set": bson.M{frozenProgramField: old}}); err != nil {
		t.Fatalf("seed an old record: %v", err)
	}
	if parked, _, err := b.ParkRunOnCapacity(transitCtx(), "run-a", frozenPark(until, nil)); err != nil || !parked {
		t.Fatalf("park: parked=%v err=%v", parked, err)
	}
	after, err := a.LoadRun(transitCtx(), "run-a")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if after.FrozenProgram != nil {
		t.Fatalf("a park with no program kept the old record %+v", after.FrozenProgram)
	}
}

// F3. Nothing but the park writes the record: every retry writer, the
// resume's flip and its revert, and a whole-document save — from a copy that
// carries none, or one that carries another — leave the stored record as the
// park wrote it. Red when ClearRunRetry unsets it, or when SaveRun lets its
// copy decide it (either half of its clause dropped).
func TestFrozenProgram_EveryOtherWriterKeepsTheRecord(t *testing.T) {
	a, b := transitPair(t)
	until, _ := startedInTransit(t, a, b, "run-a")
	if parked, _, err := b.ParkRunOnCapacity(transitCtx(), "run-a", frozenPark(until, frozenRecord())); err != nil || !parked {
		t.Fatalf("park: parked=%v err=%v", parked, err)
	}
	ctx := transitCtx()
	load := func(t *testing.T) *store.Run {
		t.Helper()
		r, err := a.LoadRun(ctx, "run-a")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		return r
	}
	want := *load(t).FrozenProgram
	retryAfter := func(t *testing.T) time.Time {
		t.Helper()
		r := load(t)
		if r.RetryState == nil || r.RetryState.RetryAfter == nil {
			t.Fatalf("run-a has no armed retry: %+v", r.RetryState)
		}
		return *r.RetryState.RetryAfter
	}
	landed := func(t *testing.T, what string, ok bool, err error) {
		t.Helper()
		if err != nil || !ok {
			t.Fatalf("%s did not land: ok=%v err=%v", what, ok, err)
		}
	}
	steps := []struct {
		name string
		do   func(t *testing.T)
	}{
		{"claim", func(t *testing.T) {
			ok, err := b.ClaimRunRetry(ctx, "run-a", retryAfter(t))
			landed(t, "ClaimRunRetry", ok, err)
		}},
		{"delay with a slot re-tag", func(t *testing.T) {
			ok, err := b.DelayRunRetry(ctx, "run-a", retryAfter(t), time.Now().Add(2*time.Minute), store.RetryReasonCredSlot, string(store.FailureCredSlotWait))
			landed(t, "DelayRunRetry", ok, err)
		}},
		{"pull", func(t *testing.T) {
			ok, err := b.PullRunRetry(ctx, "run-a", retryAfter(t), time.Now())
			landed(t, "PullRunRetry", ok, err)
		}},
		{"rearm probed", func(t *testing.T) {
			ok, err := b.RearmProbedRunRetry(ctx, "run-a", time.Now().Add(time.Hour), time.Time{})
			landed(t, "RearmProbedRunRetry", ok, err)
		}},
		{"schedule", func(t *testing.T) {
			ok, _, err := b.ScheduleRunRetry(ctx, "run-a", time.Now().Add(time.Hour), time.Time{}, store.RetryReasonCredSlot, string(store.FailureCredSlotWait), 10)
			landed(t, "ScheduleRunRetry", ok, err)
		}},
		{"clear", func(t *testing.T) {
			landed(t, "ClearRunRetry", true, b.ClearRunRetry(ctx, "run-a"))
		}},
		{"flip and revert", func(t *testing.T) {
			flip, ok, err := b.FlipToQueued(ctx, "run-a", store.RunStatusFailedResumable, time.Now())
			landed(t, "FlipToQueued", ok, err)
			ok, err = b.RevertQueuedFlip(ctx, "run-a", flip, "")
			landed(t, "RevertQueuedFlip", ok, err)
		}},
		{"save from a copy with no record", func(t *testing.T) {
			r := load(t)
			r.FrozenProgram = nil
			landed(t, "SaveRun", true, b.SaveRun(ctx, r))
		}},
		{"save from a copy with another record", func(t *testing.T) {
			r := load(t)
			r.FrozenProgram = &store.FrozenProgram{Key: "forged", Digest: "x", WorkflowHash: "h9", Source: "forged"}
			landed(t, "SaveRun", true, b.SaveRun(ctx, r))
		}},
		{"abandon", func(t *testing.T) {
			ok, err := b.AbandonRunRetry(ctx, "run-a", "test abandon", time.Time{})
			landed(t, "AbandonRunRetry", ok, err)
		}},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			st.do(t)
			got := load(t).FrozenProgram
			if got == nil || !reflect.DeepEqual(*got, want) {
				t.Fatalf("after %s the record reads %+v, want the park's %+v", st.name, got, want)
			}
		})
	}
}

// F4. The binding follows the attempt marker: the resume's flip moves
// queued_at and the record goes stale; the flip's revert puts the marker back
// and the record binds again. Red when the binding ignores queued_at.
func TestFrozenProgram_BindingFollowsTheFlip(t *testing.T) {
	a, b := transitPair(t)
	until, _ := startedInTransit(t, a, b, "run-a")
	if parked, _, err := b.ParkRunOnCapacity(transitCtx(), "run-a", frozenPark(until, frozenRecord())); err != nil || !parked {
		t.Fatalf("park: parked=%v err=%v", parked, err)
	}
	verdict := func(t *testing.T) store.FrozenVerdict {
		t.Helper()
		r, err := a.LoadRun(transitCtx(), "run-a")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		_, v := r.ReadmitFrozen()
		return v
	}
	if v := verdict(t); v != store.FrozenBound {
		t.Fatalf("parked: %s, want bound", v)
	}
	flip, ok, err := b.FlipToQueued(transitCtx(), "run-a", store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("FlipToQueued: %v %v", ok, err)
	}
	if v := verdict(t); v != store.FrozenStale {
		t.Fatalf("after the flip: %s, want stale (the attempt moved)", v)
	}
	if ok, err := b.RevertQueuedFlip(transitCtx(), "run-a", flip, "refused"); err != nil || !ok {
		t.Fatalf("RevertQueuedFlip: %v %v", ok, err)
	}
	if v := verdict(t); v != store.FrozenBound {
		t.Fatalf("after the revert: %s, want bound again", v)
	}
}

// F5. The envelope's key is mediated by its run's tenant like every IR blob,
// and swept with the run. Red when the key leaves the "-bundle-" key space:
// the mediation then finds no owning run and the sweep misses it.
func TestFrozenProgram_EnvelopeKeyIsTenantMediatedAndSweptWithTheRun(t *testing.T) {
	s, _ := preparedMongo(t)
	ctx := t.Context()
	if _, err := s.CreateRun(store.WithTenant(ctx, "tenant-a"), "run-a", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := s.CreateRun(store.WithTenant(ctx, "tenant-b"), "run-b", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	key, err := s.PutIRBlob(ctx, blob.FrozenProgramLogicalKey("run-a", "d1"), []byte(`{"v":1}`))
	if err != nil {
		t.Fatalf("PutIRBlob: %v", err)
	}
	if body, err := s.GetIRBlob(store.WithTenant(ctx, "tenant-a"), key); err != nil || string(body) != `{"v":1}` {
		t.Fatalf("owner read: %q %v", body, err)
	}
	if _, err := s.GetIRBlob(store.WithTenant(ctx, "tenant-b"), key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign-tenant read: %v, want os.ErrNotExist", err)
	}
	if err := s.DeleteRun(store.WithTenant(ctx, "tenant-a"), "run-a"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := s.blob.GetIRBlob(ctx, key); !errors.Is(err, blob.ErrArtifactNotFound) {
		t.Fatalf("after the run's deletion the envelope reads %v, want swept", err)
	}
}
