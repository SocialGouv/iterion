package storetest

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	capacityTeam    = "capacity-team"
	capacityForeign = "capacity-foreign-team"
	// capacityReader is the tenant the listing is read under: neither team
	// that owns a seeded run. A listing scoped by the context's tenant
	// returns nothing for it, which is the defect the row exists to catch.
	capacityReader = "capacity-reader-team"
)

// seedCapacityRun creates a run of tenant in status, shaped by shape before
// the one whole-document write that persists it.
func seedCapacityRun(t *testing.T, s store.RunStore, tenant, id string, status store.RunStatus, shape func(*store.Run)) {
	t.Helper()
	ctx := store.WithIdentity(context.Background(), tenant, "_test")
	r, err := s.CreateRun(ctx, id, "capacity", nil)
	if err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r.TenantID = tenant
	r.Status = status
	if shape != nil {
		shape(r)
	}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
}

// onGateway stamps a run the way a resolution that sealed a gateway key does:
// its subjects read off the bundle, and the stamp says so.
func onGateway(fp string, subjects ...string) func(*store.Run) {
	return func(r *store.Run) {
		store.RunCredStamp{Fingerprints: []string{fp}, CapacitySubjects: subjects, CapacityStamped: true}.Apply(r)
		r.BotID = "bot-" + r.ID
	}
}

// seedCapacityListing seeds every shape the alive listing must keep or drop
// and returns, by id, the projection the listing must answer for each run it
// keeps. Each expected value is written out field by field rather than
// computed through store.ProjectForCapacity: the row witnesses the store's
// own projection.
func seedCapacityListing(t *testing.T, s store.RunStore) map[string]store.Run {
	t.Helper()
	gwA := store.GatewayCapacitySubject("a.example")
	gwB := store.GatewayCapacitySubject("b.example")
	want := map[string]store.Run{}
	keep := func(id, tenant string, status store.RunStatus, fps, subjects []string) {
		r := store.Run{ID: id, TenantID: tenant, BotID: "bot-" + id, Status: status, CredFingerprints: fps, CapacitySubjects: subjects}
		if len(fps) > 0 {
			// onGateway's stamp read its subjects: the listing carries its marker.
			r.CapacityStampedFor = store.CapacityStampDigest(fps, subjects)
		}
		want[id] = r
	}

	seedCapacityRun(t, s, capacityTeam, "cap-queued", store.RunStatusQueued, onGateway("fp-a", gwA))
	keep("cap-queued", capacityTeam, store.RunStatusQueued, []string{"fp-a"}, []string{gwA})

	seedCapacityRun(t, s, capacityTeam, "cap-running", store.RunStatusRunning, onGateway("fp-b", gwA, gwB))
	keep("cap-running", capacityTeam, store.RunStatusRunning, []string{"fp-b"}, []string{gwA, gwB})

	// Model-idle releases nothing: the run still holds its stamp.
	seedCapacityRun(t, s, capacityTeam, "cap-idle", store.RunStatusRunning, func(r *store.Run) {
		onGateway("fp-a", gwA)(r)
		idle := time.Now().UTC().Truncate(time.Millisecond)
		r.LLMIdleSince = &idle
	})
	keep("cap-idle", capacityTeam, store.RunStatusRunning, []string{"fp-a"}, []string{gwA})

	// A resume whose own admission has not decided yet still spends its stamp.
	seedCapacityRun(t, s, capacityTeam, "cap-pending", store.RunStatusQueued, func(r *store.Run) {
		onGateway("fp-a", gwA)(r)
		r.CredAdmissionPending = true
	})
	keep("cap-pending", capacityTeam, store.RunStatusQueued, []string{"fp-a"}, []string{gwA})

	// Alive with no stamp at all: listed — the readings decide what it holds.
	seedCapacityRun(t, s, capacityTeam, "cap-unstamped", store.RunStatusRunning, func(r *store.Run) { r.BotID = "bot-cap-unstamped" })
	keep("cap-unstamped", capacityTeam, store.RunStatusRunning, nil, nil)

	// A subbot child as an in-process child is written: its parent's content
	// boundary, no stamp of its own.
	seedCapacityRun(t, s, capacityTeam, "cap-child", store.RunStatusRunning, func(r *store.Run) {
		r.BotID = "bot-cap-child"
		r.ParentRunID = "cap-running"
		r.ParentNodeID = "review"
		r.ContentBoundary = &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "key-gw", GatewayHost: "a.example"}
	})
	child := store.Run{ID: "cap-child", TenantID: capacityTeam, BotID: "bot-cap-child", Status: store.RunStatusRunning,
		ParentRunID: "cap-running", ParentNodeID: "review", ContentBoundary: &store.RunContentBoundary{KeyID: "key-gw"}}
	want["cap-child"] = child

	// Another team's run on the same gateway: the count is every team's.
	seedCapacityRun(t, s, capacityForeign, "cap-foreign", store.RunStatusRunning, onGateway("fp-c", gwA))
	keep("cap-foreign", capacityForeign, store.RunStatusRunning, []string{"fp-c"}, []string{gwA})

	// A run in its redelivery transit holds its stamp until the instant its
	// writer stamped, and the listing carries the two fields that say so.
	until := store.TransitInstant(time.Now().Add(5 * time.Minute))
	seedCapacityRun(t, s, capacityTeam, "cap-transit", store.RunStatusFailedResumable, onGateway("fp-a", gwA))
	teamCtx := store.WithIdentity(context.Background(), capacityTeam, "_test")
	promoteTransitIn(t, s, teamCtx, "cap-transit", until)
	keep("cap-transit", capacityTeam, store.RunStatusFailedResumable, []string{"fp-a"}, []string{gwA})
	transit := want["cap-transit"]
	transit.ContinuationState = store.ContinuationRedeliveryPending
	transit.ContinuationUntil = &until
	want["cap-transit"] = transit
	// Past its instant, the transit holds nothing.
	seedCapacityRun(t, s, capacityTeam, "cap-transit-expired", store.RunStatusFailedResumable, onGateway("fp-a", gwA))
	promoteTransitIn(t, s, teamCtx, "cap-transit-expired", time.Now().Add(-time.Minute))

	// Neither parked, paused, ended nor deleted runs hold anything.
	seedCapacityRun(t, s, capacityTeam, "cap-parked", store.RunStatusFailedResumable, onGateway("fp-a", gwA))
	seedCapacityRun(t, s, capacityTeam, "cap-paused", store.RunStatusPausedWaitingHuman, onGateway("fp-a", gwA))
	seedCapacityRun(t, s, capacityTeam, "cap-finished", store.RunStatusFinished, onGateway("fp-a", gwA))
	seedCapacityRun(t, s, capacityTeam, "cap-cancelled", store.RunStatusCancelled, onGateway("fp-a", gwA))
	seedCapacityRun(t, s, capacityTeam, "cap-deleted", store.RunStatusRunning, onGateway("fp-a", gwA))
	if err := s.DeleteRun(store.WithIdentity(context.Background(), capacityTeam, "_test"), "cap-deleted"); err != nil {
		t.Fatalf("DeleteRun cap-deleted: %v", err)
	}
	return want
}

// testAliveCapacityListing pins store.AliveCapacityLister on both twins: the
// alive runs of every tenant whatever the context's tenant — queued, running,
// model-idle, admission-pending, unstamped and subbot children alike — and
// nothing parked, paused, ended or deleted, each projected to the fields the
// per-subject readings need.
func testAliveCapacityListing(t *testing.T, s store.RunStore) {
	t.Helper()
	lister := store.AsAliveCapacityLister(s)
	if lister == nil {
		t.Fatal("the store must host the alive capacity listing (store.AliveCapacityLister)")
	}
	want := seedCapacityListing(t, s)
	got, err := lister.ListAliveRunsForCapacity(store.WithTenant(context.Background(), capacityReader))
	if err != nil {
		t.Fatalf("ListAliveRunsForCapacity: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	wantIDs := make([]string, 0, len(want))
	for id := range want {
		wantIDs = append(wantIDs, id)
	}
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("listed %v under another tenant's context, want %v — every tenant's queued and running runs (idle, admission-pending, unstamped and subbot children included), nothing parked, paused, ended or deleted", ids, wantIDs)
	}
	for _, r := range got {
		if w := want[r.ID]; !reflect.DeepEqual(r, w) {
			t.Errorf("run %s listed as %s, want %s — the projection the readings depend on", r.ID, capacityView(r), capacityView(w))
		}
	}
}

// testAliveCapacityListingMirrorsTheCount pins that the listing grouped in Go
// answers the ledger's own count for every stamped value: the listing's status
// set and the count's query filter are one list. On the Mongo twin this
// compares two queries; on the filesystem twin both sides read
// Run.SpendsCredential, so only the Mongo run witnesses the mirror — the
// filesystem run states that the shared seed agrees with itself.
func testAliveCapacityListingMirrorsTheCount(t *testing.T, s store.RunStore) {
	t.Helper()
	lister := store.AsAliveCapacityLister(s)
	if lister == nil {
		t.Fatal("the store must host the alive capacity listing (store.AliveCapacityLister)")
	}
	seedCapacityListing(t, s)
	list, err := lister.ListAliveRunsForCapacity(store.WithTenant(context.Background(), capacityReader))
	if err != nil {
		t.Fatalf("ListAliveRunsForCapacity: %v", err)
	}
	grouped := store.CountCapacityHolders(list, "")
	ctx := testCtx()
	for _, subject := range []string{
		store.GatewayCapacitySubject("a.example"), store.GatewayCapacitySubject("b.example"),
		"fp-a", "fp-b", "fp-c",
	} {
		n, err := s.CountAliveRunsWithCredFingerprint(ctx, subject, "")
		if err != nil {
			t.Fatalf("CountAliveRunsWithCredFingerprint(%s): %v", subject, err)
		}
		if grouped[subject].Runs != n {
			t.Errorf("%s: the listing grouped in Go holds %d run(s), the ledger's count %d — one status set must serve both", subject, grouped[subject].Runs, n)
		}
	}
}

// capacityView renders the projected fields of a listed run.
func capacityView(r store.Run) string {
	key := "<no boundary>"
	if r.ContentBoundary != nil {
		key = fmt.Sprintf("%+v", *r.ContentBoundary)
	}
	return fmt.Sprintf("{tenant=%s bot=%s status=%s continuation=%q until=%v parent=%s node=%s fps=%v subjects=%v marker=%q boundary=%s}",
		r.TenantID, r.BotID, r.Status, r.ContinuationState, r.ContinuationUntil, r.ParentRunID, r.ParentNodeID, r.CredFingerprints, r.CapacitySubjects, r.CapacityStampedFor, key)
}
