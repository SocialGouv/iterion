package runner

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// nopRunnerBlob stands in for the object store the capacity guard never
// touches.
type nopRunnerBlob struct{ blob.Client }

func (nopRunnerBlob) Ping(context.Context) error { return nil }
func (nopRunnerBlob) Close() error               { return nil }

// The runner's pickup against the real ledger and the real one-write park:
// a run admitted on a key of one place, parked by an interruption, then
// evicted by another run's admission while it was out of transit, is parked
// on CRED_SLOT_WAIT by its redelivery — the retry armed in the park's write —
// and never proceeds; a run whose place survived proceeds in a transit the
// redelivery owns. Skipped without ITERION_TEST_MONGO_URI. Red when the pickup
// does not run its delivery stage.
func TestCapacityTransitPickupMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the runner's Mongo capacity guard test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_runner_capacity_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
	st, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: dbName, Blob: nopRunnerBlob{}})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = st.DB().Drop(dctx)
		_ = st.Close(dctx)
	})
	rctx := idCtx()
	seed := func(id string, status store.RunStatus) {
		t.Helper()
		if _, err := st.CreateRun(rctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, err := st.LoadRun(rctx, id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		r.Status = status
		r.Checkpoint = &store.Checkpoint{NodeID: "n1"}
		if err := st.SaveRun(rctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
		if err := st.SetRunCredStamp(rctx, id, store.RunCredStamp{Fingerprints: []string{capFP}}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", id, err)
		}
	}
	// admitAndInterrupt admits id on the key and parks it as an interruption
	// leaves it, its hold past: the queue's redelivery is late.
	admitAndInterrupt := func(id string) *queue.RunMessage {
		t.Helper()
		seed(id, store.RunStatusRunning)
		adm, err := st.ReserveCredSlot(rctx, capFP, id, 1)
		if err != nil || !adm.Granted {
			t.Fatalf("admit %s: %+v %v", id, adm, err)
		}
		if err := st.FailRunResumable(rctx, id, &store.Checkpoint{NodeID: "n1"}, "drained", store.FailureInterrupted); err != nil {
			t.Fatalf("interrupt %s: %v", id, err)
		}
		if _, err := st.UpdateRunOutcome(rctx, id, store.RunStatusFailedResumable, "",
			store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending, ContinuationUntil: time.Now().Add(-time.Second)},
			[]store.RunStatus{store.RunStatusFailedResumable}); err != nil {
			t.Fatalf("promote %s: %v", id, err)
		}
		return &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1",
			PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
			Capacity:       &queue.CapacityAdmission{Holds: []store.CapacityHold{{Subject: capFP, Token: adm.At}}}}
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, maxDeliverOverride: 8}

	lostMsg := admitAndInterrupt("run-lost")
	seed("run-x", store.RunStatusPausedOperator)
	if adm, err := st.ReserveCredSlot(rctx, capFP, "run-x", 1); err != nil || !adm.Granted {
		t.Fatalf("run-x takes the place of the parked run: %+v %v", adm, err)
	}
	_, out, proceed := r.capacityAtPickup(context.Background(), lostMsg, nil, 2, iterlog.Nop())
	if proceed || out.action != actionAck {
		t.Fatalf("pickup of a run whose place was taken: proceed=%v action=%v, want parked and acked", proceed, out.action)
	}
	parked, err := st.LoadRun(rctx, "run-lost")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if parked.Status != store.RunStatusFailedResumable || parked.FailureCode != store.FailureCredSlotWait ||
		parked.ContinuationState != store.ContinuationRetryArmed || parked.ContinuationUntil != nil ||
		parked.RetryState == nil || parked.RetryState.RetryAfter == nil || parked.RetryState.Reason != store.RetryReasonCredSlot {
		t.Fatalf("run-lost: %s/%s continuation %q until %v retry %+v, want parked CRED_SLOT_WAIT with its retry armed", parked.Status, parked.FailureCode, parked.ContinuationState, parked.ContinuationUntil, parked.RetryState)
	}
	if parked.RetryState.Attempts != 0 {
		t.Fatalf("an interruption's park charged %d attempt(s), want free", parked.RetryState.Attempts)
	}

	// Another key: a run whose place survived its late redelivery resumes in
	// a transit the redelivery owns.
	r2 := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, maxDeliverOverride: 8}
	if _, err := st.DB().Collection("cred_slots").DeleteMany(ctx, map[string]any{}); err != nil {
		t.Fatalf("reset the ledger: %v", err)
	}
	heldMsg := admitAndInterrupt("run-held")
	g, _, proceed := r2.capacityAtPickup(context.Background(), heldMsg, nil, 2, iterlog.Nop())
	if !proceed || g == nil {
		t.Fatal("pickup of a run whose place survived did not proceed")
	}
	held, err := st.LoadRun(rctx, "run-held")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	owned, _, _ := g.ownedTransit()
	if !held.InCapacityTransit(time.Now()) || !held.ContinuationUntil.Equal(owned) {
		t.Fatalf("run-held continuation %q until %v, want the redelivery's transit until %s", held.ContinuationState, held.ContinuationUntil, owned)
	}
}

// irRunnerBlob keeps IR blobs in memory, keyed as S3 keys them; every other
// object is the capacity guard's to never touch.
type irRunnerBlob struct {
	nopRunnerBlob
	mu   sync.Mutex
	data map[string][]byte
}

func (b *irRunnerBlob) PutIRBlob(_ context.Context, runID string, body []byte) error {
	key, err := blob.IRBlobKey(runID)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data == nil {
		b.data = map[string][]byte{}
	}
	b.data[key] = append([]byte(nil), body...)
	return nil
}

func (b *irRunnerBlob) GetIRBlob(_ context.Context, storageKey string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.data[storageKey]
	if !ok {
		return nil, blob.ErrArtifactNotFound
	}
	return append([]byte(nil), body...), nil
}

// F32. The runner's capacity park of a started run, end to end against the
// real store: the redelivery finds its place taken, parks the run in one
// write that carries the frozen program, and the record binds the attempt
// the delivery belonged to — its queued_at and hash the run's, its envelope
// the delivered IR byte for byte, readable through the tenant-mediated blob
// read. Skipped without ITERION_TEST_MONGO_URI. Red when the park does not
// carry the frozen program.
func TestCapacityTransitFrozenParkMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the runner's Mongo frozen-park test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_runner_frozen_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
	blobs := &irRunnerBlob{}
	st, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: dbName, Blob: blobs})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = st.DB().Drop(dctx)
		_ = st.Close(dctx)
	})
	rctx := idCtx()
	body, hash := frozenTransport(t)
	q1 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	seed := func(id string, status store.RunStatus) {
		t.Helper()
		if _, err := st.CreateRun(rctx, id, "frozen", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, err := st.LoadRun(rctx, id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		r.Status = status
		r.Checkpoint = &store.Checkpoint{NodeID: "one"}
		if err := st.SaveRun(rctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
		if err := st.SetRunCredStamp(rctx, id, store.RunCredStamp{Fingerprints: []string{capFP}}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", id, err)
		}
	}
	seed("run-frozen", store.RunStatusRunning)
	adm, err := st.ReserveCredSlot(rctx, capFP, "run-frozen", 1)
	if err != nil || !adm.Granted {
		t.Fatalf("admit: %+v %v", adm, err)
	}
	if err := st.FailRunResumable(rctx, "run-frozen", &store.Checkpoint{NodeID: "one"}, "drained", store.FailureInterrupted); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if _, err := st.DB().Collection("runs").UpdateOne(ctx, map[string]any{"_id": "run-frozen"},
		map[string]any{"$set": map[string]any{"queued_at": q1, "workflow_hash": hash}}); err != nil {
		t.Fatalf("seed the attempt marker and the hash: %v", err)
	}
	if _, err := st.UpdateRunOutcome(rctx, "run-frozen", store.RunStatusFailedResumable, "",
		store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending, ContinuationUntil: time.Now().Add(-time.Second)},
		[]store.RunStatus{store.RunStatusFailedResumable}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	seed("run-x", store.RunStatusPausedOperator)
	if adm, err := st.ReserveCredSlot(rctx, capFP, "run-x", 1); err != nil || !adm.Granted {
		t.Fatalf("run-x takes the place: %+v %v", adm, err)
	}
	msg := &queue.RunMessage{RunID: "run-frozen", TenantID: "team-1", OwnerID: "u1",
		WorkflowName: "frozen", WorkflowHash: hash, IRCompiled: body,
		PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
		Capacity:       &queue.CapacityAdmission{Holds: []store.CapacityHold{{Subject: capFP, Token: adm.At}}}}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, maxDeliverOverride: 8}
	if _, out, proceed := r.capacityAtPickup(context.Background(), msg, nil, 2, iterlog.Nop()); proceed || out.action != actionAck {
		t.Fatalf("pickup: proceed=%v action=%v, want parked and acked", proceed, out.action)
	}
	parked, err := st.LoadRun(rctx, "run-frozen")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if parked.FailureCode != store.FailureCredSlotWait || parked.ContinuationState != store.ContinuationRetryArmed {
		t.Fatalf("run %s continuation %q, want parked CRED_SLOT_WAIT retry_armed", parked.FailureCode, parked.ContinuationState)
	}
	fp, verdict := parked.ReadmitFrozen()
	if verdict != store.FrozenBound || fp == nil {
		t.Fatalf("the parked run reads %s (%+v), want its program frozen and bound", verdict, fp)
	}
	if !fp.QueuedAt.Equal(q1) || fp.WorkflowHash != hash {
		t.Fatalf("record queued_at %v hash %s, want the delivery's attempt %v and the run's hash %s", fp.QueuedAt, fp.WorkflowHash, q1, hash)
	}
	env, err := runview.LoadFrozenProgram(rctx, st, "run-frozen", fp)
	if err != nil {
		t.Fatalf("LoadFrozenProgram: %v", err)
	}
	if string(env.IR) != string(body) || env.WorkflowHash != msg.WorkflowHash {
		t.Fatal("the envelope is not the delivered program")
	}
}
