package mongo

import (
	"errors"
	"testing"
	"time"

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
