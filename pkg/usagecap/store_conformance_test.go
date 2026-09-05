package usagecap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// runStoreConformance is the contract both Store twins must honour, run
// against the in-process store here and the real Mongo one below. One
// suite, two backends: a behaviour that only the memory twin exhibits is
// a cloud hole, not a feature.
func runStoreConformance(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC)

	// The same credential metered under three keys: its tenant meter, the
	// platform meter (a lent or platform-tier credential serves everyone),
	// and a legacy fingerprint-less key that must NOT be touched by a
	// fingerprint delete (it names a slot, not a credential).
	tenantKey := Key("claude_code", TenantScope("team-a"), "fp-aaaa1111")
	platformKey := Key("claude_code", ScopePlatform, "fp-aaaa1111")
	otherKey := Key("claude_code", TenantScope("team-a"), "fp-bbbb2222")
	legacyKey := Key("claude_code", TenantScope("team-a"), "")

	rec := func(key string, w Window, util float64, observed time.Time) {
		t.Helper()
		if err := st.Record(ctx, key, Reading{Window: w, Utilization: util, Status: StatusAllowed,
			ResetsAt: observed.Add(72 * time.Hour), ObservedAt: observed}); err != nil {
			t.Fatalf("Record(%s, %s): %v", key, w, err)
		}
	}
	rec(tenantKey, WindowSevenDay, 0.95, now)
	rec(tenantKey, WindowFiveHour, 0.40, now)
	rec(platformKey, WindowSevenDay, 0.93, now)
	rec(otherKey, WindowSevenDay, 0.10, now)
	rec(legacyKey, WindowSevenDay, 0.99, now)

	// Newest-per-window: an older observation must not regress a window.
	rec(tenantKey, WindowSevenDay, 0.20, now.Add(-time.Hour))
	got, err := st.Latest(ctx, tenantKey)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Latest(tenant) = %d readings, want 2 (one per window)", len(got))
	}
	for _, r := range got {
		if r.Window == WindowSevenDay && r.Utilization != 0.95 {
			t.Fatalf("an older observation regressed the seven_day window to %.2f", r.Utilization)
		}
	}

	// An unknown key is "nothing learned yet", never an error.
	if got, err := st.Latest(ctx, Key("x", "y", "z")); err != nil || len(got) != 0 {
		t.Fatalf("Latest(unknown) = %v, %v; want empty, nil", got, err)
	}

	// #690 point 3 — the operator escape hatch. Deleting by fingerprint
	// forgets the credential under EVERY key it was metered with (tenant
	// and platform scope alike: the reset an operator witnessed applies to
	// the credential, not to one of its meters), and nothing else.
	n, err := st.DeleteByFingerprint(ctx, "fp-aaaa1111")
	if err != nil {
		t.Fatalf("DeleteByFingerprint: %v", err)
	}
	if n != 3 {
		t.Fatalf("DeleteByFingerprint dropped %d readings, want 3 (two windows under the tenant key + one under the platform key)", n)
	}
	for _, key := range []string{tenantKey, platformKey} {
		if got, err := st.Latest(ctx, key); err != nil || len(got) != 0 {
			t.Fatalf("Latest(%s) after delete = %v, %v; want empty", key, got, err)
		}
	}
	if got, _ := st.Latest(ctx, otherKey); len(got) != 1 {
		t.Fatalf("another credential's reading was deleted (%d left, want 1)", len(got))
	}
	if got, _ := st.Latest(ctx, legacyKey); len(got) != 1 {
		t.Fatalf("the fingerprint-less legacy key was deleted (%d left, want 1) — it names a slot, not this credential", len(got))
	}
	// A second delete is a no-op that reports zero, never an error.
	if n, err := st.DeleteByFingerprint(ctx, "fp-aaaa1111"); err != nil || n != 0 {
		t.Fatalf("second DeleteByFingerprint = %d, %v; want 0, nil", n, err)
	}
	// An empty fingerprint names nothing: it must not match the legacy key
	// (whose key carries no fp segment) nor anything else.
	if n, err := st.DeleteByFingerprint(ctx, ""); err == nil && n != 0 {
		t.Fatalf("DeleteByFingerprint(\"\") dropped %d readings — an empty fingerprint must match nothing", n)
	}
	// The credential can be metered again afterwards.
	rec(tenantKey, WindowSevenDay, 0.05, now.Add(time.Minute))
	if got, _ := st.Latest(ctx, tenantKey); len(got) != 1 || got[0].Utilization != 0.05 {
		t.Fatalf("re-record after delete = %+v, want the fresh 5%% reading", got)
	}
}

// runRefusalStreakConformance is the second half of the Store contract
// (#629 pt 2): the store, not the caller, counts how many times IN A ROW a
// credential was refused on a window with no reset instant. A caller is one
// pod that saw one refusal; only the ledger can tell a blip from an account
// frozen for days, and the escalating rest reads nothing else.
func runRefusalStreakConformance(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	key := Key("claude_code", TenantScope("team-streak"), "fp-streak")

	refuse := func(w Window, at time.Time) {
		t.Helper()
		if err := st.Record(ctx, key, Reading{Window: w, Status: StatusRejected, ObservedAt: at}); err != nil {
			t.Fatalf("Record(refusal): %v", err)
		}
	}
	countOf := func(w Window) int {
		t.Helper()
		got, err := st.Latest(ctx, key)
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		for _, r := range got {
			if r.Window == w {
				return r.Refusals
			}
		}
		t.Fatalf("no reading for %s", w)
		return 0
	}

	refuse(WindowAuth, now)
	if got := countOf(WindowAuth); got != 1 {
		t.Fatalf("first refusal counted %d, want 1", got)
	}
	refuse(WindowAuth, now.Add(time.Hour))
	refuse(WindowAuth, now.Add(2*time.Hour))
	if got := countOf(WindowAuth); got != 3 {
		t.Fatalf("three refusals in a row counted %d, want 3", got)
	}
	// Streaks are per window: a frequency refusal starts its own.
	refuse(WindowFrequency, now.Add(2*time.Hour))
	if got := countOf(WindowFrequency); got != 1 {
		t.Fatalf("a different window's first refusal counted %d, want 1", got)
	}
	if got := countOf(WindowAuth); got != 3 {
		t.Fatalf("the auth streak moved to %d when another window was refused", got)
	}
	// The credential answering ends the streak — that is what makes the
	// rest self-healing rather than a one-way lock.
	if err := st.Record(ctx, key, Reading{
		Window: WindowAuth, Status: StatusAllowed, ObservedAt: now.Add(3 * time.Hour),
	}); err != nil {
		t.Fatalf("Record(allowed): %v", err)
	}
	if got := countOf(WindowAuth); got != 0 {
		t.Fatalf("a served call left the streak at %d, want 0", got)
	}
	refuse(WindowAuth, now.Add(4*time.Hour))
	if got := countOf(WindowAuth); got != 1 {
		t.Fatalf("the streak restarted at %d, want 1", got)
	}
	// A reading that LOSES the newest-wins race must not bump the count:
	// the streak counts refusals the ledger accepted, not deliveries.
	refuse(WindowAuth, now.Add(time.Hour))
	if got := countOf(WindowAuth); got != 1 {
		t.Fatalf("an out-of-order refusal bumped the streak to %d, want 1", got)
	}
	// A DATED refusal expires at its own reset instant, so it needs no
	// escalation and carries no streak.
	if err := st.Record(ctx, key, Reading{
		Window: WindowSevenDay, Status: StatusRejected,
		ResetsAt: now.Add(72 * time.Hour), ObservedAt: now.Add(5 * time.Hour),
	}); err != nil {
		t.Fatalf("Record(dated refusal): %v", err)
	}
	if got := countOf(WindowSevenDay); got != 0 {
		t.Fatalf("a dated refusal counted %d, want 0 — it expires at its own reset", got)
	}
}

func TestMemStore_Conformance(t *testing.T) {
	runStoreConformance(t, NewMemStore())
	runRefusalStreakConformance(t, NewMemStore())
}

// TestMongoStore_Conformance runs the same contract against the real Mongo
// twin (same gating as the pkg/store/mongo harness).
func TestMongoStore_Conformance(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo usage-readings suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_usagecap_readings_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runStoreConformance(t, NewMongoStore(db))
	runRefusalStreakConformance(t, NewMongoStore(db))
}
