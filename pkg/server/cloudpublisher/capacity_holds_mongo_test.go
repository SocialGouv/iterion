package cloudpublisher

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The launch's capacity writes on the real Mongo store (ADR-167 D8, C3):
// skipped without ITERION_TEST_MONGO_URI.

// capacityMongo opens two clients on one throwaway database: two publisher
// replicas.
func capacityMongo(t *testing.T) (*mongostore.Store, *mongostore.Store) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the launch capacity Mongo tests")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	db := "iterion_launch_capacity_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
	a, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: db, Blob: nopBlobClient{}})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	b, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: db, Blob: nopBlobClient{}})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = a.DB().Drop(dctx)
		_ = b.Close(dctx)
		_ = a.Close(dctx)
	})
	return a, b
}

// A launch the cap refuses is persisted parked with its retry armed in its
// first version: no reader sees the park without its retry. Red on the
// two-step park (the save, then the arming: version 2).
func TestCapacityHolds_LaunchParkIsOneWriteMongo(t *testing.T) {
	st, _ := capacityMongo(t)
	f := newCappedFixture(t, 1)
	p := cappedKeyPublisher(st, f.keys, f.sealer, f.caps, f.published)
	if err := f.launch(t, p, "run-holder", nil); err != nil {
		t.Fatalf("launch run-holder: %v", err)
	}
	sub, err := p.SubmitLaunch(f.ctx, "run-parked", launchSpec(nil), cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if err != nil {
		t.Fatalf("launch run-parked: %v", err)
	}
	if sub.Status != store.RunStatusFailedResumable || sub.FailureCode != store.FailureCredSlotWait {
		t.Fatalf("submission %+v, want parked", sub)
	}
	r, err := st.LoadRun(f.ctx, "run-parked")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.CASVersion != 1 || r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureCredSlotWait ||
		r.ContinuationState != store.ContinuationRetryArmed || r.RetryState == nil || r.RetryState.RetryAfter == nil ||
		r.RetryState.Reason != store.RetryReasonCredSlot || r.RetryState.Attempts != 1 {
		t.Fatalf("parked launch: version=%d %s/%s continuation=%q retry=%+v, want version 1 parked and armed", r.CASVersion, r.Status, r.FailureCode, r.ContinuationState, r.RetryState)
	}
}

// stallPastTheGrace is the launch F5 names, between its admission and its
// persist: it stalls past the ledger's grace, another replica's admission (M)
// evicts its entry — it has no document yet — and takes the place; once the
// launch persists queued, a third admission (N) adopts it, by an entry that
// carries no identity of the launch's admission.
type stallPastTheGrace struct {
	store.RunStore
	raw   *mongostore.Store
	other *mongostore.Store
	runID string
	ctx   context.Context
	t     *testing.T
	mu    sync.Mutex
	done  bool
}

func (s *stallPastTheGrace) Unwrap() store.RunStore { return s.RunStore }

func (s *stallPastTheGrace) SaveRun(ctx context.Context, r *store.Run) error {
	s.mu.Lock()
	stall := r.ID == s.runID && !s.done
	s.done = s.done || stall
	s.mu.Unlock()
	if !stall {
		return s.RunStore.SaveRun(ctx, r)
	}
	// The stall: the launch's entry ages past the grace.
	past := time.Now().UTC().Add(-store.CredSlotGrace - time.Minute)
	if _, err := s.raw.DB().Collection("cred_slots").UpdateOne(context.Background(),
		bson.M{"_id": cappedFP, "holders.run_id": s.runID}, bson.M{"$set": bson.M{"holders.$.at": past}}); err != nil {
		s.t.Fatalf("age the launch's entry: %v", err)
	}
	// M, on another replica: its reconcile evicts the launch (no document,
	// past the grace) and takes the place; M then runs.
	seedSpendingRun(s.t, s.other, s.ctx, "run-m", store.RunStatusPausedOperator)
	if adm, err := s.other.ReserveCredSlot(s.ctx, cappedFP, "run-m", 1); err != nil || !adm.Granted {
		s.t.Fatalf("M takes the place: %+v %v", adm, err)
	}
	setStatus(s.t, s.other, s.ctx, "run-m", store.RunStatusRunning)
	if err := s.RunStore.SaveRun(ctx, r); err != nil {
		return err
	}
	// N, on the other replica: the launch is queued and spends the key, so
	// N's admission adopts it.
	seedSpendingRun(s.t, s.other, s.ctx, "run-n", store.RunStatusPausedOperator)
	if adm, err := s.other.ReserveCredSlot(s.ctx, cappedFP, "run-n", 1); err != nil || adm.Granted {
		s.t.Fatalf("N: %+v %v, want refused with M and the adopted launch", adm, err)
	}
	return nil
}

func seedSpendingRun(t *testing.T, s *mongostore.Store, ctx context.Context, id string, status store.RunStatus) {
	t.Helper()
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	setStatus(t, s, ctx, id, status)
	if err := s.SetRunCredStamp(ctx, id, store.RunCredStamp{Fingerprints: []string{cappedFP}}); err != nil {
		t.Fatalf("SetRunCredStamp %s: %v", id, err)
	}
}

func setStatus(t *testing.T, s *mongostore.Store, ctx context.Context, id string, status store.RunStatus) {
	t.Helper()
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = status
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
}

// F5 caught by C3: the launch's re-assertion before it publishes finds its
// entry gone — the one in the row now is an adoption's, with no identity of
// its admission — asks again, is refused, and parks with zero publishes,
// instead of running beside M on a cap of one. Red when C3 is skipped, when
// it accepts the adopted entry, or when it skips the re-reservation.
func TestCapacityHolds_LaunchStalledPastTheGraceIsCaughtAfterItsWriteMongo(t *testing.T) {
	a, b := capacityMongo(t)
	f := newCappedFixture(t, 1)
	stall := &stallPastTheGrace{RunStore: a, raw: a, other: b, runID: "run-l", ctx: f.ctx, t: t}
	p := cappedKeyPublisher(stall, f.keys, f.sealer, f.caps, f.published)
	sub, err := p.SubmitLaunch(f.ctx, "run-l", launchSpec(nil), cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if !stall.done {
		t.Fatal("the stall never ran")
	}
	if got := f.published.ids(); len(got) != 0 {
		t.Fatalf("published %v: the launch runs beside M on a cap of one", got)
	}
	if sub.Status != store.RunStatusFailedResumable || sub.FailureCode != store.FailureCredSlotWait {
		t.Fatalf("submission %+v, want parked", sub)
	}
	r, err := a.LoadRun(f.ctx, "run-l")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureCredSlotWait || r.ContinuationState != store.ContinuationRetryArmed {
		t.Fatalf("run-l: %s/%s continuation %q, want parked and armed", r.Status, r.FailureCode, r.ContinuationState)
	}
}
