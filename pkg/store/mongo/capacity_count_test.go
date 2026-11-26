package mongo

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A tombstoned document is no holder whatever status it carries: the listing
// keeps the deleted_at exclusion the ledger's count keeps (notDeleted), and a
// tombstone that still reads `running` — which the store's own DeleteRun never
// writes, so the shared conformance row cannot seed it — is the one shape that
// witnesses it.
func TestAliveCapacityListingMongoSkipsATombstoneWhateverItsStatus(t *testing.T) {
	s := aliveTestStore(t)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	subject := store.GatewayCapacitySubject("a.example")
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, doc := range []bson.M{
		{"_id": "cap-live", "tenant_id": "team-a", "status": string(store.RunStatusRunning), "capacity_subjects": bson.A{subject}},
		{"_id": "cap-tombstone", "tenant_id": "team-a", "status": string(store.RunStatusRunning), "capacity_subjects": bson.A{subject}, "deleted_at": now},
	} {
		if _, err := s.runs.InsertOne(ctx, doc); err != nil {
			t.Fatalf("insert %v: %v", doc["_id"], err)
		}
	}
	got, err := s.ListAliveRunsForCapacity(store.WithTenant(context.Background(), "team-reader"))
	if err != nil {
		t.Fatalf("ListAliveRunsForCapacity: %v", err)
	}
	if len(got) != 1 || got[0].ID != "cap-live" {
		ids := make([]string, 0, len(got))
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		t.Fatalf("listed %v, want [cap-live] — a tombstone holds nothing, whatever its status says", ids)
	}
}

// t1SetRunCredStamp is the update the T1 release's SetRunCredStamp writes
// (ADR-167 slice T1, verbatim): what a replica not yet on T2 runs when it
// resumes a run during the rolling deploy. It writes the fingerprints and the
// subjects of a stamp that carries none — it clears them — and knows nothing
// of capacity_stamped_for, which it leaves in place.
func t1SetRunCredStamp(ctx context.Context, s *Store, id string, stamp store.RunCredStamp) error {
	set := bson.M{
		"cred_fingerprints": stamp.Fingerprints,
		"credential_tiers":  stamp.Tiers,
		"updated_at":        time.Now().UTC(),
	}
	unset := bson.M{"llm_idle_since": ""}
	if len(stamp.CapacitySubjects) > 0 {
		set["capacity_subjects"] = stamp.CapacitySubjects
	} else {
		unset["capacity_subjects"] = ""
	}
	if stamp.SkippedReopensAt != nil {
		set["skipped_cred_reopens_at"] = stamp.SkippedReopensAt.UTC()
	} else {
		unset["skipped_cred_reopens_at"] = ""
	}
	res, err := s.updateRunMetadata(ctx, id, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), versionRunUpdate(bson.M{"$set": set, "$unset": unset}))
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return store.ErrRunNotFound
	}
	return nil
}

// A T2 stamp marks its subjects read; a T1 replica's resume during the
// rolling deploy then clears the subjects around a marker it does not know.
// The run must not read as one whose latest resolution sealed no gateway key
// — it still dials the gateway — whether the T1 resume sealed the same key or
// another: through LoadRun and through the alive listing the gateway readings
// group. This is the twin a T1 write reaches with the marker intact; a T1
// filesystem binary drops the field it cannot decode when it rewrites the run.
func TestCapacityStampedForMongoAT1ReStampLeavesNoReadMarker(t *testing.T) {
	s := aliveTestStore(t)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	tctx := store.WithTenant(ctx, "team-a")
	subject := store.GatewayCapacitySubject("a.example")
	for _, c := range []struct {
		id  string
		fps []string
	}{{"t1-same-key", []string{"fp-gw"}}, {"t1-other-key", []string{"fp-gw-v2"}}} {
		if _, err := s.CreateRun(tctx, c.id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", c.id, err)
		}
		if err := s.UpdateRunStatus(tctx, c.id, store.RunStatusRunning, ""); err != nil {
			t.Fatalf("UpdateRunStatus %s: %v", c.id, err)
		}
		if err := s.SetRunCredStamp(tctx, c.id, store.RunCredStamp{Fingerprints: []string{"fp-gw"}, CapacitySubjects: []string{subject}, CapacityStamped: true}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", c.id, err)
		}
		r, err := s.LoadRun(tctx, c.id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", c.id, err)
		}
		marker := r.CapacityStampedFor
		if !r.CapacitySubjectsRead() {
			t.Fatalf("%s premise: the T2 stamp does not read as read (marker %q)", c.id, marker)
		}
		if err := t1SetRunCredStamp(tctx, s, c.id, store.RunCredStamp{Fingerprints: c.fps}); err != nil {
			t.Fatalf("T1 SetRunCredStamp %s: %v", c.id, err)
		}
		r, err = s.LoadRun(tctx, c.id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", c.id, err)
		}
		if r.CapacityStampedFor != marker || len(r.CapacitySubjects) != 0 {
			t.Fatalf("%s premise: after the T1 write marker %q (was %q), subjects %v — want the marker left and the subjects cleared, the shape a T1 replica writes", c.id, r.CapacityStampedFor, marker, r.CapacitySubjects)
		}
		if r.CapacitySubjectsRead() {
			t.Fatalf("%s: a T1 re-stamp cleared the subjects and the run still reads as one whose latest stamp read them — it would read as off the gateway it dials", c.id)
		}
	}
	listed, err := s.ListAliveRunsForCapacity(ctx)
	if err != nil {
		t.Fatalf("ListAliveRunsForCapacity: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d runs, want the 2 seeded", len(listed))
	}
	for _, r := range listed {
		if r.CapacityStampedFor == "" || r.CapacitySubjectsRead() {
			t.Fatalf("listed %s: marker %q, CapacitySubjectsRead() = %v — the listing must carry the marker, and a T1-rewritten stamp must not read as read", r.ID, r.CapacityStampedFor, r.CapacitySubjectsRead())
		}
	}
}
