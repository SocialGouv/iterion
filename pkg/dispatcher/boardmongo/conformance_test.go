package boardmongo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// runBoardStoreSuite exercises the native.BoardStore contract. It runs against
// both the filesystem native.Store (always — proving the suite) and the Mongo
// store (gated on ITERION_TEST_MONGO_URI), so the two implementations are held
// to an identical bar.
func runBoardStoreSuite(t *testing.T, store native.BoardStore) {
	t.Helper()

	// Create: title required.
	if _, err := store.Create(native.Issue{}); err == nil {
		t.Error("Create without title should fail")
	}

	// Create defaults the state to the board's first state (inbox).
	created, err := store.Create(native.Issue{Title: "first", Labels: []string{"x"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.State != native.StateInbox {
		t.Errorf("default state: want %q, got %q", native.StateInbox, created.State)
	}
	if !strings.HasPrefix(created.ID, "native:") || created.CreatedAt.IsZero() {
		t.Errorf("created issue id/timestamps: %+v", created)
	}

	// Get found + not-found.
	if got, err := store.Get(created.ID); err != nil || got.Title != "first" {
		t.Errorf("Get: %+v err=%v", got, err)
	}
	if _, err := store.Get("native:00000000-0000-0000-0000-000000000000"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Get missing: want ErrNotFound, got %v", err)
	}

	// Resolve by bare uuid (no native: prefix).
	bare := strings.TrimPrefix(created.ID, "native:")
	if full, err := store.Resolve(bare); err != nil || full != created.ID {
		t.Errorf("Resolve(%q): %q err=%v", bare, full, err)
	}

	// Update: patch fields + no-op.
	pr := 5
	updated, err := store.Update(created.ID, native.Patch{Priority: &pr})
	if err != nil || updated.Priority != 5 {
		t.Errorf("Update priority: %+v err=%v", updated, err)
	}
	if _, err := store.Update(created.ID, native.Patch{Priority: &pr}); err != nil {
		t.Errorf("Update no-op: %v", err)
	}

	// set_bot via Update.Bot.
	bot := "feature-dev"
	if u, err := store.Update(created.ID, native.Patch{Bot: &bot}); err != nil || u.Bot != "feature-dev" {
		t.Errorf("Update bot: %+v err=%v", u, err)
	}

	// SetState: valid transition, unknown rejected, no-op same.
	if u, err := store.SetState(created.ID, native.StateReady); err != nil || u.State != native.StateReady {
		t.Errorf("SetState: %+v err=%v", u, err)
	}
	if _, err := store.SetState(created.ID, "does-not-exist"); !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Errorf("SetState unknown: want ErrTransitionRejected, got %v", err)
	}
	if _, err := store.SetState(created.ID, native.StateReady); err != nil {
		t.Errorf("SetState no-op: %v", err)
	}

	// Claim: idempotent same marker; conflict on a different marker; release.
	if err := store.Claim(created.ID, "runner-A"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Claim(created.ID, "runner-A"); err != nil {
		t.Errorf("Claim idempotent: %v", err)
	}
	if err := store.Claim(created.ID, "runner-B"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Errorf("Claim conflict: want ErrClaimConflict, got %v", err)
	}
	if err := store.Release(created.ID, "runner-B"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Errorf("Release by wrong marker: want ErrClaimConflict, got %v", err)
	}
	if err := store.Release(created.ID, "runner-A"); err != nil {
		t.Errorf("Release: %v", err)
	}
	if err := store.Release(created.ID, "runner-A"); err != nil {
		t.Errorf("Release unclaimed no-op: %v", err)
	}

	// SetLastRun stamps the single pointer AND appends dedup'd run history.
	if err := store.SetLastRun(created.ID, "run-1", "/tmp/wd"); err != nil {
		t.Errorf("SetLastRun: %v", err)
	}
	if got, _ := store.Get(created.ID); got.LastRunID != "run-1" {
		t.Errorf("SetLastRun not persisted: %+v", got)
	}
	// A second, distinct run id appends a second RunRef (newest-last).
	if err := store.SetLastRun(created.ID, "run-2", "/tmp/wd2"); err != nil {
		t.Errorf("SetLastRun run-2: %v", err)
	}
	if got, _ := store.Get(created.ID); len(got.Runs) != 2 ||
		got.Runs[0].RunID != "run-1" || got.Runs[1].RunID != "run-2" {
		t.Errorf("run history not appended newest-last: %+v", got.Runs)
	}
	// Re-stamping an existing run id updates it in place, no growth.
	if err := store.SetLastRun(created.ID, "run-1", "/tmp/wd-moved"); err != nil {
		t.Errorf("SetLastRun run-1 re-stamp: %v", err)
	}
	if got, _ := store.Get(created.ID); len(got.Runs) != 2 || got.Runs[0].Workdir != "/tmp/wd-moved" {
		t.Errorf("run history dedup-update failed: %+v", got.Runs)
	}

	// SetAwaitingInput denormalizes the pause hint onto the card; set true,
	// clear false, with parity to the native store (idempotent, tagged).
	if err := store.SetAwaitingInput(created.ID, true); err != nil {
		t.Errorf("SetAwaitingInput(true): %v", err)
	}
	if got, _ := store.Get(created.ID); !got.AwaitingInput {
		t.Errorf("SetAwaitingInput(true) not persisted: %+v", got)
	}
	if err := store.SetAwaitingInput(created.ID, false); err != nil {
		t.Errorf("SetAwaitingInput(false): %v", err)
	}
	if got, _ := store.Get(created.ID); got.AwaitingInput {
		t.Errorf("SetAwaitingInput(false) not cleared: %+v", got)
	}

	// SetGaveUp records / clears the dispatcher's give-up stamp — the record
	// that distinguishes an automatic give-up from an operator filing the
	// same terminal state by hand. Both stores must round-trip the nested
	// value, or the pipeline board's needs-attention lane is cloud-blind.
	current, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before SetGaveUp: %v", err)
	}
	if err := store.SetGaveUp(created.ID, &native.GiveUp{RunID: "run-1", State: current.State, Attempts: 3}); err != nil {
		t.Errorf("SetGaveUp: %v", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Errorf("Get after SetGaveUp: %v", err)
	} else if got.GaveUp == nil || got.GaveUp.RunID != "run-1" || got.GaveUp.Attempts != 3 || got.GaveUp.At.IsZero() {
		t.Errorf("give-up stamp not persisted: %+v", got.GaveUp)
	} else if !got.GaveUp.Current(got.State, "run-1") {
		t.Errorf("stamp does not describe the issue it was written on: state=%q stamp=%+v", got.State, got.GaveUp)
	}
	// Moving the ticket expires the stamp for good — both stores enforce it
	// on their write path, or a returning ticket would resurrect a give-up.
	if _, err := store.SetState(created.ID, native.StateBlocked); err != nil {
		t.Errorf("SetState(blocked): %v", err)
	}
	if got, _ := store.Get(created.ID); got.GaveUp != nil {
		t.Errorf("stamp survived the ticket moving: %+v", got.GaveUp)
	}
	if _, err := store.SetState(created.ID, current.State); err != nil {
		t.Errorf("SetState(back): %v", err)
	}
	if got, _ := store.Get(created.ID); got.GaveUp != nil {
		t.Errorf("stamp came back when the ticket returned to its state: %+v", got.GaveUp)
	}
	// And an explicit clear is a no-op on an unstamped issue.
	if err := store.SetGaveUp(created.ID, nil); err != nil {
		t.Errorf("SetGaveUp(nil): %v", err)
	}
	if got, _ := store.Get(created.ID); got.GaveUp != nil {
		t.Errorf("SetGaveUp(nil) did not clear: %+v", got.GaveUp)
	}

	// List: filter by state + assignee; sort by priority.
	_, _ = store.Create(native.Issue{Title: "second", State: native.StateReady, Priority: 9})
	ready, err := store.List(native.ListFilter{States: []string{native.StateReady}})
	if err != nil || len(ready) != 2 {
		t.Errorf("List by state: got %d err=%v", len(ready), err)
	}
	if len(ready) == 2 && ready[0].Priority < ready[1].Priority {
		t.Errorf("List should sort by priority desc: %d then %d", ready[0].Priority, ready[1].Priority)
	}

	// AggregateLabels.
	labels := store.AggregateLabels()
	found := false
	for _, l := range labels {
		if l.Label == "x" && l.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("AggregateLabels missing label x: %+v", labels)
	}

	// ScanEvents: at least the create/update/state events landed.
	var n int
	if err := store.ScanEvents(func(*native.Event) bool { n++; return true }); err != nil {
		t.Errorf("ScanEvents: %v", err)
	}
	if n == 0 {
		t.Error("ScanEvents returned no events")
	}

	// Delete.
	if err := store.Delete(created.ID); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, err := store.Get(created.ID); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Get after delete: want ErrNotFound, got %v", err)
	}
	if err := store.Delete(created.ID); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Delete missing: want ErrNotFound, got %v", err)
	}
}

// runBoardAdminSuite exercises the native.BoardAdmin config-mutation surface
// (columns, fields, views, label vocabulary) plus the cascades to issues. It
// runs against both the filesystem native.Store and the Mongo store so the
// two implementations are held to an identical bar — same validation, same
// sentinel errors, same touched counts. `store` and `admin` are the SAME
// backing store viewed through both interfaces.
func runBoardAdminSuite(t *testing.T, store native.BoardStore, admin native.BoardAdmin) {
	t.Helper()

	// --- states (columns) ---

	if err := admin.AddState(native.State{Name: "triage", Display: "Triage"}); err != nil {
		t.Fatalf("AddState: %v", err)
	}
	if store.Board().StateByName("triage") == nil {
		t.Fatal("AddState: triage not persisted")
	}
	if err := admin.AddState(native.State{Name: "triage"}); err == nil {
		t.Error("AddState duplicate should fail")
	}
	if err := admin.AddState(native.State{Name: ""}); err == nil {
		t.Error("AddState empty name should fail")
	}

	// UpdateState: flip eligible + set display; never renames.
	yes := true
	if err := admin.UpdateState("triage", native.StatePatch{Eligible: &yes, Display: ptr("Triage!")}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if st := store.Board().StateByName("triage"); st == nil || !st.Eligible || st.Display != "Triage!" {
		t.Errorf("UpdateState not applied: %+v", st)
	}
	if err := admin.UpdateState("nope", native.StatePatch{Display: ptr("x")}); err == nil {
		t.Error("UpdateState unknown should fail")
	}

	// Park an issue in triage so RenameState/DeleteState cascade.
	parked, err := store.Create(native.Issue{Title: "parked", State: "triage"})
	if err != nil {
		t.Fatalf("Create parked: %v", err)
	}
	// RenameState cascades the card.
	n, err := admin.RenameState("triage", "triaging")
	if err != nil || n != 1 {
		t.Errorf("RenameState: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(parked.ID); got.State != "triaging" {
		t.Errorf("RenameState cascade: parked state=%q want triaging", got.State)
	}
	if store.Board().StateByName("triage") != nil {
		t.Error("RenameState: old column still present")
	}
	if _, err := admin.RenameState("triaging", native.StateInbox); err == nil {
		t.Error("RenameState onto existing column should fail")
	}
	if n, err := admin.RenameState("triaging", "triaging"); err != nil || n != 0 {
		t.Errorf("RenameState self no-op: touched=%d err=%v", n, err)
	}

	// DeleteState: non-empty without migrate target → ErrStateNotEmpty.
	if _, err := admin.DeleteState("triaging", ""); !errors.Is(err, native.ErrStateNotEmpty) {
		t.Errorf("DeleteState non-empty: want ErrStateNotEmpty, got %v", err)
	}
	// DeleteState with migrate target moves the card and drops the column.
	n, err = admin.DeleteState("triaging", native.StateBacklog)
	if err != nil || n != 1 {
		t.Errorf("DeleteState migrate: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(parked.ID); got.State != native.StateBacklog {
		t.Errorf("DeleteState migrate: parked state=%q want backlog", got.State)
	}
	if store.Board().StateByName("triaging") != nil {
		t.Error("DeleteState: column still present")
	}
	if _, err := admin.DeleteState("ghost", ""); err == nil {
		t.Error("DeleteState unknown should fail")
	}

	// ReorderStates: permutation only.
	cur := store.Board()
	names := make([]string, len(cur.States))
	for i, st := range cur.States {
		names[i] = st.Name
	}
	if len(names) >= 2 {
		swapped := append([]string(nil), names...)
		swapped[0], swapped[1] = swapped[1], swapped[0]
		if err := admin.ReorderStates(swapped); err != nil {
			t.Errorf("ReorderStates: %v", err)
		}
		if store.Board().States[0].Name != swapped[0] {
			t.Errorf("ReorderStates not applied: %+v", store.Board().States)
		}
	}
	if err := admin.ReorderStates([]string{"only-one"}); err == nil {
		t.Error("ReorderStates non-permutation should fail")
	}

	// --- fields ---

	if err := admin.AddField(native.Field{Name: "severity", Type: native.FieldText}); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if store.Board().FieldByName("severity") == nil {
		t.Fatal("AddField: severity not persisted")
	}
	if err := admin.AddField(native.Field{Name: "severity", Type: native.FieldText}); err == nil {
		t.Error("AddField duplicate should fail")
	}
	if err := admin.AddField(native.Field{Name: "bad", Type: native.FieldEnum}); err == nil {
		t.Error("AddField enum without values should fail board validation")
	}

	// UpdateField: change display/required in place.
	if err := admin.UpdateField("severity", native.FieldPatch{Display: ptr("Severity")}); err != nil {
		t.Errorf("UpdateField: %v", err)
	}
	if f := store.Board().FieldByName("severity"); f == nil || f.Display != "Severity" {
		t.Errorf("UpdateField not applied: %+v", f)
	}
	if err := admin.UpdateField("nope", native.FieldPatch{Display: ptr("x")}); err == nil {
		t.Error("UpdateField unknown should fail")
	}

	// Put a value on an issue so RenameField/DeleteField cascade.
	withField, err := store.Create(native.Issue{Title: "has-field", Fields: map[string]any{"severity": "high"}})
	if err != nil {
		t.Fatalf("Create withField: %v", err)
	}
	// RenameField cascades the key.
	n, err = admin.RenameField("severity", "sev")
	if err != nil || n != 1 {
		t.Errorf("RenameField: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(withField.ID); got.Fields["sev"] != "high" || got.Fields["severity"] != nil {
		t.Errorf("RenameField cascade: fields=%+v", got.Fields)
	}
	if store.Board().FieldByName("severity") != nil {
		t.Error("RenameField: old field def still present")
	}
	if _, err := admin.RenameField("sev", "bot_args"); err == nil {
		t.Error("RenameField onto existing field should fail")
	}
	// DeleteField strips the key.
	n, err = admin.DeleteField("sev")
	if err != nil || n != 1 {
		t.Errorf("DeleteField: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(withField.ID); got.Fields["sev"] != nil {
		t.Errorf("DeleteField cascade: key not stripped: %+v", got.Fields)
	}
	if store.Board().FieldByName("sev") != nil {
		t.Error("DeleteField: field def still present")
	}
	if _, err := admin.DeleteField("ghost"); err == nil {
		t.Error("DeleteField unknown should fail")
	}

	// ReorderFields: add a second field, then permute.
	if err := admin.AddField(native.Field{Name: "owner", Type: native.FieldText}); err != nil {
		t.Fatalf("AddField owner: %v", err)
	}
	fcur := store.Board()
	fnames := make([]string, len(fcur.Fields))
	for i, f := range fcur.Fields {
		fnames[i] = f.Name
	}
	if len(fnames) >= 2 {
		rev := make([]string, len(fnames))
		for i := range fnames {
			rev[i] = fnames[len(fnames)-1-i]
		}
		if err := admin.ReorderFields(rev); err != nil {
			t.Errorf("ReorderFields: %v", err)
		}
		if store.Board().Fields[0].Name != rev[0] {
			t.Errorf("ReorderFields not applied: %+v", store.Board().Fields)
		}
	}
	if err := admin.ReorderFields([]string{"x"}); err == nil {
		t.Error("ReorderFields non-permutation should fail")
	}

	// --- views ---

	if err := admin.SaveView(native.View{Name: "mine", Assignee: "me"}); err != nil {
		t.Fatalf("SaveView: %v", err)
	}
	if err := admin.SaveView(native.View{Name: "mine", Assignee: "you"}); err != nil {
		t.Fatalf("SaveView upsert: %v", err)
	}
	if vs := store.Board().Views; len(vs) != 1 || vs[0].Assignee != "you" {
		t.Errorf("SaveView upsert by name: %+v", vs)
	}
	if err := admin.SaveView(native.View{Name: ""}); err == nil {
		t.Error("SaveView empty name should fail")
	}
	if err := admin.DeleteView("mine"); err != nil {
		t.Errorf("DeleteView: %v", err)
	}
	if len(store.Board().Views) != 0 {
		t.Errorf("DeleteView: view still present: %+v", store.Board().Views)
	}
	if err := admin.DeleteView("ghost"); err == nil {
		t.Error("DeleteView unknown should fail")
	}

	// --- labels ---

	a, err := store.Create(native.Issue{Title: "la", Labels: []string{"bug", "p1"}})
	if err != nil {
		t.Fatalf("Create la: %v", err)
	}
	b, err := store.Create(native.Issue{Title: "lb", Labels: []string{"bug"}})
	if err != nil {
		t.Fatalf("Create lb: %v", err)
	}
	// RenameLabel cascades to both bug-carrying issues.
	n, err = admin.RenameLabel("bug", "defect")
	if err != nil || n != 2 {
		t.Errorf("RenameLabel: touched=%d err=%v (want 2, nil)", n, err)
	}
	if got, _ := store.Get(a.ID); !contains(got.Labels, "defect") || contains(got.Labels, "bug") {
		t.Errorf("RenameLabel cascade a: %+v", got.Labels)
	}
	if _, err := admin.RenameLabel("", "x"); !errors.Is(err, native.ErrLabelEmpty) {
		t.Errorf("RenameLabel empty: want ErrLabelEmpty, got %v", err)
	}
	if n, err := admin.RenameLabel("defect", "defect"); err != nil || n != 0 {
		t.Errorf("RenameLabel self no-op: touched=%d err=%v", n, err)
	}
	// MergeLabels: merge p1 into defect on issue a (dedupe — a already has defect).
	n, err = admin.MergeLabels("p1", "defect")
	if err != nil || n != 1 {
		t.Errorf("MergeLabels: touched=%d err=%v (want 1, nil)", n, err)
	}
	if got, _ := store.Get(a.ID); contains(got.Labels, "p1") || labelCount(got.Labels, "defect") != 1 {
		t.Errorf("MergeLabels dedupe a: %+v", got.Labels)
	}
	// DeleteLabel strips defect from both.
	n, err = admin.DeleteLabel("defect")
	if err != nil || n != 2 {
		t.Errorf("DeleteLabel: touched=%d err=%v (want 2, nil)", n, err)
	}
	if got, _ := store.Get(b.ID); contains(got.Labels, "defect") {
		t.Errorf("DeleteLabel cascade b: %+v", got.Labels)
	}
	if _, err := admin.DeleteLabel(""); !errors.Is(err, native.ErrLabelEmpty) {
		t.Errorf("DeleteLabel empty: want ErrLabelEmpty, got %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func labelCount(ss []string, want string) int {
	c := 0
	for _, s := range ss {
		if s == want {
			c++
		}
	}
	return c
}

// TestNativeStore_Conformance proves the suite against the reference
// filesystem implementation (always runs).
func TestNativeStore_Conformance(t *testing.T) {
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	runBoardStoreSuite(t, store)

	admin, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore (admin): %v", err)
	}
	runBoardAdminSuite(t, admin, admin)
}

// TestMongoStore_Conformance runs the same suite against the Mongo store.
func TestMongoStore_Conformance(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// Idempotent re-run.
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema (second): %v", err)
	}
	runBoardStoreSuite(t, boardmongo.New(db, "tenant-1"))

	// The same Mongo store must satisfy native.BoardAdmin identically to the
	// filesystem store (its own tenant so the cascades don't collide with the
	// BoardStore suite's tenant-1 issues).
	adminStore := boardmongo.New(db, "admin-tenant")
	runBoardAdminSuite(t, adminStore, adminStore)

	// The Mongo store must also drive the dispatcher as a tracker.Tracker via
	// the shared native.Adapter (eligible + unclaimed + blocker-free filtering).
	runTrackerSuite(t, boardmongo.New(db, "tracker-tenant"))

	// The Coordinator's cross-tenant ListEligible must find ready+unclaimed
	// cards across tenants (verifies the issue.state / issue.claim BSON paths).
	coord := boardmongo.NewCoordinator(db)
	for _, tc := range []struct {
		tenant, title, state string
		claim                bool
	}{
		{"ca", "ready-a", native.StateReady, false},
		{"cb", "ready-b", native.StateReady, false},
		{"ca", "parked", native.StateInbox, false}, // not eligible
		{"cb", "claimed", native.StateReady, true}, // eligible state but claimed
	} {
		st := coord.StoreFor(tc.tenant)
		iss, cerr := st.Create(native.Issue{Title: tc.title, State: tc.state})
		if cerr != nil {
			t.Fatalf("coord create %s: %v", tc.title, cerr)
		}
		if tc.claim {
			if cerr := st.Claim(iss.ID, "someone"); cerr != nil {
				t.Fatalf("claim: %v", cerr)
			}
		}
	}
	elig, eerr := coord.ListEligible(ctx, []string{native.StateReady}, 50, false)
	if eerr != nil {
		t.Fatalf("ListEligible: %v", eerr)
	}
	gotTitles := map[string]string{}
	for _, c := range elig {
		gotTitles[c.Issue.Title] = c.Tenant
	}
	if gotTitles["ready-a"] != "ca" || gotTitles["ready-b"] != "cb" {
		t.Errorf("cross-tenant ListEligible should return ready-a + ready-b: %v", gotTitles)
	}
	if _, ok := gotTitles["parked"]; ok {
		t.Error("inbox card must not be eligible")
	}
	if _, ok := gotTitles["claimed"]; ok {
		t.Error("claimed card must not be eligible")
	}

	// Ordering contract: the dispatch tick lists oldest-updated first (FIFO
	// fairness); the stranded-card sweep lists NEWEST first, so a capped
	// window always contains the freshest strandings on a saturated board
	// (R0544a9). ready-a was created before ready-b, so their UpdatedAt
	// order is creation order.
	if len(elig) == 2 && !(elig[0].Issue.Title == "ready-a" && elig[1].Issue.Title == "ready-b") {
		t.Errorf("oldest-first order = [%s, %s], want [ready-a, ready-b]", elig[0].Issue.Title, elig[1].Issue.Title)
	}
	desc, derr := coord.ListEligible(ctx, []string{native.StateReady}, 1, true)
	if derr != nil {
		t.Fatalf("ListEligible newest-first: %v", derr)
	}
	if len(desc) != 1 || desc[0].Issue.Title != "ready-b" {
		t.Errorf("newest-first capped window = %v, want the freshest card ready-b", desc)
	}
}

// runTrackerSuite exercises the tracker.Tracker view (native.Adapter) over a
// board store — the path the cloud dispatcher uses.
func runTrackerSuite(t *testing.T, store native.BoardStore) {
	t.Helper()
	trk := native.NewAdapter(store)
	ctx := context.Background()

	// An inbox issue is NOT a candidate (inbox is not eligible); a ready issue
	// IS (ready is eligible on the default board).
	_, _ = store.Create(native.Issue{Title: "parked", State: native.StateInbox})
	ready, err := store.Create(native.Issue{Title: "do me", State: native.StateReady})
	if err != nil {
		t.Fatalf("create ready: %v", err)
	}
	cands, err := trk.ListCandidates(ctx)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != ready.ID {
		t.Fatalf("candidates: want [%s], got %+v", ready.ID, cands)
	}

	// Claim removes it from the candidate set.
	if err := trk.Claim(ctx, ready.ID, "runner-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	cands, _ = trk.ListCandidates(ctx)
	if len(cands) != 0 {
		t.Errorf("claimed issue must not be a candidate, got %+v", cands)
	}

	// UpdateState + RefreshStates round-trip.
	if err := trk.UpdateState(ctx, ready.ID, native.StateDone); err != nil {
		t.Errorf("UpdateState: %v", err)
	}
	states, _ := trk.RefreshStates(ctx, []string{ready.ID})
	if states[ready.ID] != native.StateDone {
		t.Errorf("RefreshStates: %v", states)
	}
}
