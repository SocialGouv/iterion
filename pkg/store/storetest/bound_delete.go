package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// bindRunOrigin binds ctx to r's observation as the server binds an admitted
// action: the run's header and the tenant its store recorded.
func bindRunOrigin(t *testing.T, ctx context.Context, r *store.Run) context.Context {
	t.Helper()
	header := store.RunDataHeader{RunID: r.ID, Format: store.ValueFormat(r.FormatVersion), Protocol: r.DataProtocol, BirthID: r.BirthID}
	o, err := store.CaptureRunObservation(header, r.TenantID, true)
	if err != nil {
		t.Fatalf("capture the observation of %s: %v", r.ID, err)
	}
	bound, err := store.BindRunObservation(ctx, o)
	if err != nil {
		t.Fatalf("bind to the observation of %s: %v", r.ID, err)
	}
	return bound
}

// seedBoundDeleteRun creates id with one event and returns the run as stored.
func seedBoundDeleteRun(t *testing.T, s store.RunStore, ctx context.Context, id string) *store.Run {
	t.Helper()
	r := seedMergeBirthRun(t, s, ctx, id, false)
	if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("AppendEvent %s: %v", id, err)
	}
	return r
}

// requireBoundDeleteRunIntact requires id to hold the run born as birth with
// exactly one event.
func requireBoundDeleteRunIntact(t *testing.T, s store.RunStore, ctx context.Context, id, birth string) {
	t.Helper()
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("the successor %s does not load after a refused delete: %v", id, err)
	}
	if r.BirthID != birth {
		t.Fatalf("%s holds birth %q after a refused delete, want the successor's %q", id, r.BirthID, birth)
	}
	events, err := s.LoadEvents(ctx, id)
	if err != nil || len(events) != 1 {
		t.Fatalf("the successor's events after a refused delete = %d, %v; want 1, nil", len(events), err)
	}
}

// testBoundDeleteRefusesASuccessor: a delete bound to the run its caller
// observed never removes a successor recreated under the same ID. The
// refusal is typed and leaves the successor whole; a delete bound to the
// successor itself lands. Unbound deletes and the prune keep their
// unanchored behaviour: they act on whatever run holds the ID.
func testBoundDeleteRefusesASuccessor(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	const id = "run-bound-delete"
	a := seedBoundDeleteRun(t, s, ctx, id)
	boundA := bindRunOrigin(t, ctx, a)
	b := replaceMergeBirthRun(t, s, ctx, a, false)
	if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("AppendEvent on the successor: %v", err)
	}

	err := s.DeleteRun(boundA, id)
	requireBoundDeleteRunIntact(t, s, ctx, id, b.BirthID)
	if !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("delete bound to the original on its successor = %v, want ErrRunDataIdentityChanged", err)
	}

	if err := s.DeleteRun(bindRunOrigin(t, ctx, b), id); err != nil {
		t.Fatalf("delete bound to the run's own observation = %v, want nil", err)
	}
	if _, err := s.LoadRun(ctx, id); !errors.Is(err, store.ErrRunDeleted) {
		t.Fatalf("LoadRun after a bound delete of the run's own observation = %v, want ErrRunDeleted", err)
	}

	// Legacy: an unbound delete removes whatever run holds the ID, and the
	// unbound prune reaps its tombstone so the ID can be born again.
	const legacyID = "run-bound-delete-legacy"
	original := seedBoundDeleteRun(t, s, ctx, legacyID)
	successor := replaceMergeBirthRun(t, s, ctx, original, false)
	if err := s.DeleteRun(ctx, legacyID); err != nil {
		t.Fatalf("unbound delete of a successor = %v, want nil", err)
	}
	if _, err := s.LoadRun(ctx, legacyID); !errors.Is(err, store.ErrRunDeleted) {
		t.Fatalf("LoadRun after an unbound delete of birth %q = %v, want ErrRunDeleted", successor.BirthID, err)
	}
	reaped, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour))
	if err != nil || reaped < 1 {
		t.Fatalf("unbound PruneDeletionMarkers = (%d, %v), want at least one tombstone reaped", reaped, err)
	}
	if _, err := s.LoadRun(ctx, legacyID); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("LoadRun after the prune = %v, want ErrRunNotFound", err)
	}
	if _, err := s.CreateRun(ctx, legacyID, "wf", nil); err != nil {
		t.Fatalf("CreateRun after the prune: %v", err)
	}
}
