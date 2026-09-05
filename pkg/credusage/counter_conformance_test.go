package credusage

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

// runCounterConformance is the contract both Counter twins must honour. One
// suite, two backends: a behaviour only the memory twin exhibits is a cloud
// hole, not a feature.
func runCounterConformance(t *testing.T, c Counter) {
	t.Helper()
	ctx := context.Background()
	sept := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	teamKey := Key{Fingerprint: "fp-team", Provider: "anthropic", Tier: TierTeam, TenantID: "team-a"}
	forfait := Key{Fingerprint: "fp-forfait", Provider: "claude_code", Tier: TierTeam, TenantID: "team-a"}
	platform := Key{Fingerprint: "fp-plat", Provider: "openai", Tier: TierPlatform, TenantID: "team-a"}

	// Nothing recorded yet: a zero view, never an error.
	got, err := c.Usage(ctx, sept, teamKey)
	if err != nil {
		t.Fatalf("Usage on an empty month: %v", err)
	}
	if got.Month != "2026-09" || got.CostUSD != 0 || got.Runs != 0 {
		t.Fatalf("empty month = %+v, want a zero view for 2026-09", got)
	}

	add := func(k Key, nature Nature, backend string, cost float64, in, out int64, when time.Time) {
		t.Helper()
		if err := c.AddSpend(ctx, when, Spend{
			Key: k, Nature: nature, Backend: backend,
			CostUSD: cost, InputTokens: in, OutputTokens: out,
		}); err != nil {
			t.Fatalf("AddSpend(%s): %v", k.Fingerprint, err)
		}
	}
	add(teamKey, NatureMetered, "claw", 1.25, 1000, 200, sept)
	add(teamKey, NatureMetered, "claw", 0.75, 500, 100, sept)
	// The SAME run's other half, on another credential and another
	// backend — the case a single RunTotals() figure cannot express.
	add(forfait, NatureEstimate, "claude_code", 4.5, 9000, 1200, sept)
	add(platform, NatureEstimate, "codex", 2.0, 300, 60, sept)

	got, err = c.Usage(ctx, sept, teamKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.CostUSD != 2.0 || got.InputTokens != 1500 || got.OutputTokens != 300 || got.Runs != 2 {
		t.Fatalf("team key month = %+v, want $2.00 / 1500 in / 300 out / 2 runs", got)
	}
	if got.Nature != NatureMetered {
		t.Fatalf("nature = %q, want metered — a key's figure is an invoice", got.Nature)
	}
	if len(got.Backends) != 1 || got.Backends[0] != "claw" {
		t.Fatalf("backends = %v, want [claw]", got.Backends)
	}

	fRow, err := c.Usage(ctx, sept, forfait)
	if err != nil {
		t.Fatal(err)
	}
	if fRow.Nature != NatureEstimate {
		t.Fatalf("forfait nature = %q, want estimate — a subscription bills nothing per call", fRow.Nature)
	}

	// A tenant listing carries every credential that served it, biggest
	// spend first.
	rows, err := c.List(ctx, sept, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("List(team-a) = %d rows, want 3 (one per credential+tier)", len(rows))
	}
	if rows[0].Fingerprint != "fp-forfait" || rows[1].Fingerprint != "fp-plat" || rows[2].Fingerprint != "fp-team" {
		t.Fatalf("List order = %s/%s/%s, want biggest spend first", rows[0].Fingerprint, rows[1].Fingerprint, rows[2].Fingerprint)
	}
	if n, err := c.List(ctx, sept, "team-b"); err != nil || len(n) != 0 {
		t.Fatalf("List(other tenant) = %v, %v; want empty", n, err)
	}

	// A platform credential serving a SECOND tenant is a separate row
	// under the same fingerprint: "what did this key cost us" and "what
	// did this key cost" are both answerable.
	add(Key{Fingerprint: "fp-plat", Provider: "openai", Tier: TierPlatform, TenantID: "team-b"},
		NatureEstimate, "codex", 3.0, 100, 20, sept)
	byFP, err := c.ListByFingerprint(ctx, sept, "fp-plat")
	if err != nil {
		t.Fatal(err)
	}
	if len(byFP) != 2 {
		t.Fatalf("ListByFingerprint = %d rows, want 2 (one per tenant it served)", len(byFP))
	}
	total := 0.0
	for _, r := range byFP {
		total += r.CostUSD
	}
	if total != 5.0 {
		t.Fatalf("fp-plat across tenants = $%.2f, want $5.00", total)
	}
	if n, err := c.ListByFingerprint(ctx, sept, ""); err != nil || len(n) != 0 {
		t.Fatalf("ListByFingerprint(\"\") = %v, %v; want nothing", n, err)
	}

	// The platform tier's own month is asked by TIER, not by tenant: its
	// rows live under the tenants it served.
	plat, err := c.ListByTier(ctx, sept, TierPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if len(plat) != 2 {
		t.Fatalf("ListByTier(platform) = %d rows, want 2 (team-a + team-b)", len(plat))
	}
	for _, row := range plat {
		if row.Tier != TierPlatform {
			t.Fatalf("ListByTier(platform) returned a %s row", row.Tier)
		}
	}
	if n, err := c.ListByTier(ctx, sept, ""); err != nil || len(n) != 0 {
		t.Fatalf("ListByTier(\"\") = %v, %v; want nothing", n, err)
	}

	// The TIER is part of the identity: the same key lent through the pool
	// is a different economic fact from the same key used by its owner.
	add(Key{Fingerprint: "fp-team", Provider: "anthropic", Tier: TierPool, TenantID: "team-a"},
		NatureMetered, "claw", 0.5, 10, 2, sept)
	if own, _ := c.Usage(ctx, sept, teamKey); own.CostUSD != 2.0 {
		t.Fatalf("the owner's row moved to $%.2f when the same key was lent", own.CostUSD)
	}

	// Months do not bleed.
	if next, _ := c.Usage(ctx, oct, teamKey); next.CostUSD != 0 || next.Month != "2026-10" {
		t.Fatalf("october = %+v, want a fresh month", next)
	}

	// Un-meterable spends are dropped quietly: metering must never turn a
	// finished run into a failed one.
	for _, bad := range []Spend{
		{Key: Key{Provider: "anthropic", Tier: TierTeam}, Nature: NatureMetered, CostUSD: 9},                                 // no fingerprint: a slot, not an account
		{Key: Key{Fingerprint: "fp-x", Tier: TierTeam}, Nature: NatureMetered, CostUSD: 9},                                   // no provider
		{Key: Key{Fingerprint: "fp-x", Provider: "anthropic", Tier: TierTeam}, CostUSD: 9},                                   // no nature
		{Key: Key{Fingerprint: "fp-team", Provider: "anthropic", Tier: TierTeam, TenantID: "team-a"}, Nature: NatureMetered}, // nothing spent
	} {
		if err := c.AddSpend(ctx, sept, bad); err != nil {
			t.Fatalf("AddSpend(%+v) errored: %v", bad, err)
		}
	}
	if after, _ := c.Usage(ctx, sept, teamKey); after.Runs != 2 {
		t.Fatalf("a zero-amount spend bumped the run count to %d", after.Runs)
	}
	if rows, _ := c.List(ctx, sept, "team-a"); len(rows) != 4 {
		t.Fatalf("List(team-a) = %d rows after the invalid spends, want 4", len(rows))
	}
}

func TestMemoryCounter_Conformance(t *testing.T) {
	runCounterConformance(t, NewMemoryCounter())
}

// TestMongoCounter_Conformance runs the same contract against the real Mongo
// twin (same gating as the other conformance suites).
func TestMongoCounter_Conformance(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo credusage suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_credusage_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runCounterConformance(t, NewMongoCounter(db))
}
