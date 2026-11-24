package mongo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TR9 (R54 slice 3, F10): the observed twin contract over Mongo —
// AppendObservedEvent carries the captured registration's tenant, refuses a
// successor, and the explicit no-filter path admits tenant-less runs.
// Skipped unless ITERION_TEST_MONGO_URI is set (the conformance fixture).
func TestAppendObservedEventCarriesTheCapturedTenant(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo twin contract")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_twin_" + bsonNonce(t)
	s, err := New(ctx, Config{
		URI:                uri,
		Database:           dbName,
		Blob:               newInMemoryBlob(),
		RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch"),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})

	// A run born under a tenant: its observation carries the tenant, and
	// the observed append succeeds under the binding.
	captureOrigin := func(ctx context.Context, id string) store.RunObservation {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		obs, err := store.CaptureRunObservation(store.RunDataHeader{
			RunID: r.ID, Format: store.ValueFormat(r.FormatVersion), Protocol: r.DataProtocol, BirthID: r.BirthID,
		}, r.TenantID, true)
		if err != nil {
			t.Fatalf("capture %s: %v", id, err)
		}
		return obs
	}

	if _, err := s.CreateRun(store.WithTenant(ctx, "team-a"), "twin", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	origin := captureOrigin(store.WithTenant(ctx, "team-a"), "twin")
	if origin.TenantID() != "team-a" {
		t.Fatalf("origin tenant = %q, want team-a", origin.TenantID())
	}
	bound, err := store.BindRunObservation(store.WithTenant(ctx, "team-a"), origin)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.AppendObservedEvent(bound, s, "twin", store.Event{
		Type: store.EventRunHealth, RunID: "twin", NodeID: "n", Data: map[string]any{"kind": "stall"},
	})
	if err != nil {
		t.Fatalf("observed append: %v", err)
	}
	if got, ok := persisted.EventObservation(); !ok || got.TenantID() != "team-a" {
		t.Fatalf("persisted observation tenant = %q (%t), want team-a", got.TenantID(), ok)
	}

	// A tenant-less run under the explicit no-filter path.
	if _, err := s.CreateRun(store.WithoutTenantFilter(ctx), "local", "wf", nil); err != nil {
		t.Fatalf("CreateRun local: %v", err)
	}
	localOrigin := captureOrigin(store.WithoutTenantFilter(ctx), "local")
	localBound, err := store.BindRunObservation(store.WithoutTenantFilter(ctx), localOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendObservedEvent(localBound, s, "local", store.Event{
		Type: store.EventRunHealth, RunID: "local",
	}); err != nil {
		t.Fatalf("tenant-less observed append: %v", err)
	}

	// A successor under the same ID refuses the predecessor's registration.
	tenanted := store.WithTenant(ctx, "team-a")
	if err := s.UpdateRunStatus(tenanted, "twin", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(tenanted, "twin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneDeletionMarkers(store.WithoutTenantFilter(ctx), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(store.WithTenant(ctx, "team-a"), "twin", "wf", nil); err != nil {
		t.Fatalf("CreateRun successor: %v", err)
	}
	if _, err := store.AppendObservedEvent(bound, s, "twin", store.Event{
		Type: store.EventRunHealth, RunID: "twin",
	}); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("successor append err = %v, want ErrRunDataIdentityChanged", err)
	}

	// The refused append wrote NOTHING to the successor: the persisted rows
	// are read back under the expected tenant and carry no run_health
	// (codex-F7: the error alone does not prove the successor's file clean).
	successor, err := s.LoadEvents(tenanted, "twin")
	if err != nil {
		t.Fatalf("read the successor's events: %v", err)
	}
	for _, ev := range successor {
		if ev.Type == store.EventRunHealth {
			t.Fatalf("the refused append landed a run_health in the successor: %+v", ev)
		}
	}

}
