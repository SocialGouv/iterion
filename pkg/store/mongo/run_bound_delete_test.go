package mongo

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestBoundDeleteRefusesASuccessorInsideTheTransaction: a delete bound to the
// run its caller observed compares the binding inside the transaction that
// tombstones the run, not only at its earlier reads. The original is replaced
// by a successor after those reads and before the transaction; the delete is
// refused, typed, and the successor keeps its document and its events.
func TestBoundDeleteRefusesASuccessorInsideTheTransaction(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	const id = "bound-delete-txn"
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	bound, a := bindMongoRun(t, s, ctx, id)
	var successor string
	fired := false
	t.Cleanup(func() { s.beforeValueDeletionTxn = nil })
	s.beforeValueDeletionTxn = func(runID string) {
		if runID != id || fired {
			return
		}
		fired = true
		if err := s.DeleteRun(ctx, id); err != nil {
			t.Fatalf("DeleteRun: %v", err)
		}
		if _, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("PruneDeletionMarkers: %v", err)
		}
		b, err := s.CreateRun(ctx, id, "wf", nil)
		if err != nil {
			t.Fatalf("recreate: %v", err)
		}
		if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		successor = b.BirthID
	}
	err := s.DeleteRun(bound, id)
	s.beforeValueDeletionTxn = nil
	if !fired {
		t.Fatal("the seam never ran")
	}
	if successor == "" || successor == a.BirthID {
		t.Fatalf("fixture: successor birth %q, original %q", successor, a.BirthID)
	}
	r, loadErr := s.LoadRun(ctx, id)
	if loadErr != nil {
		t.Fatalf("the successor does not load after a refused bound delete: %v", loadErr)
	}
	if r.BirthID != successor {
		t.Fatalf("%s holds birth %q after a refused bound delete, want the successor's %q", id, r.BirthID, successor)
	}
	if events, err := s.LoadEvents(ctx, id); err != nil || len(events) != 1 {
		t.Fatalf("the successor's events after a refused bound delete = %d, %v; want 1, nil", len(events), err)
	}
	if !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("delete bound to the original, successor born inside its window = %v, want ErrRunDataIdentityChanged", err)
	}
}

// boundDeleteOrigins are the shapes a bound delete can observe: the born
// ones (legacy, exact) and the historical ones (birth_id absent, or stored
// as an explicit empty string).
func boundDeleteOrigins() []boundMetadataOrigin {
	return append(boundMetadataOrigins(), boundMetadataOrigin{"explicit-empty", func(t *testing.T, s *Store, ctx context.Context, id string) {
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"birth_id": ""}}); err != nil {
			t.Fatal(err)
		}
	}})
}

// requireIDFree requires that no document holds id and that a run can be
// born under it again.
func requireIDFree(t *testing.T, s *Store, ctx context.Context, id string) {
	t.Helper()
	if n, err := s.runs.CountDocuments(ctx, bson.M{"_id": id}); err != nil || n != 0 {
		t.Fatalf("documents under %s after a refused bound delete = %d, %v; want none", id, n, err)
	}
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatalf("CreateRun %s after a refused bound delete: %v", id, err)
	}
}

// reapRun deletes id unbound and prunes its tombstone: the ID is free.
func reapRun(t *testing.T, s *Store, ctx context.Context, id string) {
	t.Helper()
	if err := s.DeleteRun(ctx, id); err != nil {
		t.Fatalf("DeleteRun %s: %v", id, err)
	}
	if _, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("PruneDeletionMarkers: %v", err)
	}
	if n, err := s.runs.CountDocuments(ctx, bson.M{"_id": id}); err != nil || n != 0 {
		t.Fatalf("fixture: %d documents under %s after the prune (%v)", n, id, err)
	}
}

// TestBoundDeleteOfAReapedRunWritesNothing: a delete bound to a run that was
// deleted and reaped since its caller observed it is refused, typed, and
// writes nothing — no tombstone is planted on the free ID, so a run can be
// born under it again. A historical observation answers ErrRunNotFound, the
// answer of the filesystem twin; a born one keeps the answer its bound read
// gives.
func TestBoundDeleteOfAReapedRunWritesNothing(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	for _, origin := range boundDeleteOrigins() {
		t.Run(origin.name, func(t *testing.T) {
			id := "bound-delete-reaped-" + origin.name
			origin.create(t, s, ctx, id)
			bound, a := bindMongoRun(t, s, ctx, id)
			reapRun(t, s, ctx, id)
			err := s.DeleteRun(bound, id)
			requireIDFree(t, s, ctx, id)
			if a.BirthID == "" && !errors.Is(err, store.ErrRunNotFound) {
				t.Fatalf("delete bound to a reaped historical run = %v, want ErrRunNotFound", err)
			}
			if a.BirthID != "" && !errors.Is(err, store.ErrRunNotFound) && !errors.Is(err, store.ErrRunDataIdentityChanged) {
				t.Fatalf("delete bound to a reaped born run = %v, want a typed refusal", err)
			}
		})
	}
}

// TestBoundDeleteOfARunReapedInsideItsWindow: the same refusal inside the
// transaction that tombstones the run. A historical run is reaped after the
// delete's earlier reads and before that transaction: the delete answers
// ErrRunNotFound and the ID stays free.
func TestBoundDeleteOfARunReapedInsideItsWindow(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	const id = "bound-delete-reaped-txn"
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	setStoredBirth(t, s, ctx, id, "")
	bound, _ := bindMongoRun(t, s, ctx, id)
	fired := false
	t.Cleanup(func() { s.beforeValueDeletionTxn = nil })
	s.beforeValueDeletionTxn = func(runID string) {
		if runID != id || fired {
			return
		}
		fired = true
		reapRun(t, s, ctx, id)
	}
	err := s.DeleteRun(bound, id)
	s.beforeValueDeletionTxn = nil
	if !fired {
		t.Fatal("the seam never ran")
	}
	requireIDFree(t, s, ctx, id)
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("delete bound to a historical run reaped inside its window = %v, want ErrRunNotFound", err)
	}
}

// TestUnboundDeleteOfAnAbsentRunStaysIdempotent: an unbound delete of an ID
// no run holds keeps its answer — nil, the ID tombstoned — whether the run
// never existed or was reaped.
func TestUnboundDeleteOfAnAbsentRunStaysIdempotent(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	for _, tc := range []struct {
		name   string
		reaped bool
	}{{"never-created", false}, {"reaped", true}} {
		t.Run(tc.name, func(t *testing.T) {
			id := "unbound-delete-absent-" + tc.name
			if tc.reaped {
				if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
					t.Fatal(err)
				}
				reapRun(t, s, ctx, id)
			}
			if err := s.DeleteRun(ctx, id); err != nil {
				t.Fatalf("unbound delete of an absent run = %v, want nil", err)
			}
			if _, err := s.LoadRun(ctx, id); !errors.Is(err, store.ErrRunDeleted) {
				t.Fatalf("LoadRun after an unbound delete of an absent run = %v, want ErrRunDeleted", err)
			}
		})
	}
}
