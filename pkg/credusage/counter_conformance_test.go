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

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
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
	addAggregate := func(k Key, nature Nature, backend string, cost float64, aggregate int64, when time.Time) {
		t.Helper()
		if err := c.AddSpend(ctx, when, Spend{
			Key: k, Nature: nature, Backend: backend,
			CostUSD: cost, AggregateTokens: aggregate,
		}); err != nil {
			t.Fatalf("AddSpend(%s): %v", k.Fingerprint, err)
		}
	}
	add(teamKey, NatureMetered, "claw", 1.25, 1000, 200, sept)
	add(teamKey, NatureMetered, "claw", 0.75, 500, 100, sept)
	// The SAME run's other half, on another credential and another
	// backend — the case a single RunTotals() figure cannot express.
	// A CLI delegate reports ONE count and no split (#992): it must land in
	// the aggregate on BOTH twins, leaving the directional pair at zero.
	addAggregate(forfait, NatureEstimate, "claude_code", 4.5, 9000, sept)
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

	// A spend carrying ONLY an aggregate is recordable: a delegation whose
	// price sources knew nothing still proves the credential was used.
	addAggregate(forfait, NatureEstimate, "claude_code", 0, 1000, sept)

	fRow, err := c.Usage(ctx, sept, forfait)
	if err != nil {
		t.Fatal(err)
	}
	if fRow.Nature != NatureEstimate {
		t.Fatalf("forfait nature = %q, want estimate — a subscription bills nothing per call", fRow.Nature)
	}
	if fRow.AggregateTokens != 10000 {
		t.Fatalf("forfait aggregate tokens = %d, want 10000 (9000 + the unpriced 1000)", fRow.AggregateTokens)
	}
	if fRow.InputTokens != 0 || fRow.OutputTokens != 0 {
		t.Fatalf("forfait row = in %d / out %d, want 0/0 — the delegate never reported a split (#992)", fRow.InputTokens, fRow.OutputTokens)
	}
	if fRow.Runs != 2 {
		t.Fatalf("forfait runs = %d, want 2 — an aggregate-only spend is not an empty one", fRow.Runs)
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

	// --- the repository dimension ---
	//
	// A repo-attributed spend is its own stored row. The listings that
	// predate the dimension therefore have to SUM the repositories back
	// together, or every figure an operator reads would drop on the day
	// their runs started naming a repo — a reporting outage that looks
	// exactly like a quiet month.
	const repoA, repoB = "SocialGouv/iterion", "SocialGouv/other"
	repoKey := func(repo string) Key {
		return Key{Fingerprint: "fp-team", Provider: "anthropic", Tier: TierTeam, TenantID: "team-a", RepoID: repo}
	}
	add(repoKey(repoA), NatureMetered, "claude_code", 1.0, 100, 20, sept)
	add(repoKey(repoB), NatureMetered, "claw", 0.5, 50, 10, sept)

	// The unattributed row is untouched: Usage addresses ONE row, and its
	// key names no repository.
	if own, _ := c.Usage(ctx, sept, teamKey); own.CostUSD != 2.0 || own.Runs != 2 {
		t.Fatalf("the repo-less row moved to $%.2f / %d runs when repo spend was recorded beside it", own.CostUSD, own.Runs)
	}
	if withRepo, _ := c.Usage(ctx, sept, repoKey(repoA)); withRepo.CostUSD != 1.0 || withRepo.RepoID != repoA {
		t.Fatalf("Usage(repo row) = $%.2f repo=%q, want $1.00 on %q", withRepo.CostUSD, withRepo.RepoID, repoA)
	}

	rows, err = c.List(ctx, sept, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("List(team-a) = %d rows, want the same 4 — repositories are summed into their credential, not listed beside it", len(rows))
	}
	var teamRow *MonthlyUsage
	for i := range rows {
		if rows[i].Fingerprint == "fp-team" && rows[i].Tier == TierTeam {
			teamRow = &rows[i]
		}
	}
	if teamRow == nil {
		t.Fatal("List(team-a) lost the fp-team/team row")
	}
	if teamRow.CostUSD != 3.5 {
		t.Fatalf("summed fp-team = $%.2f, want $3.50 ($2.00 unattributed + $1.00 + $0.50)", teamRow.CostUSD)
	}
	if teamRow.Runs != 4 || teamRow.InputTokens != 1650 || teamRow.OutputTokens != 330 {
		t.Fatalf("summed fp-team = %d runs / %d in / %d out, want 4 / 1650 / 330", teamRow.Runs, teamRow.InputTokens, teamRow.OutputTokens)
	}
	if teamRow.RepoID != "" {
		t.Fatalf("a summed row named repo %q — it is the total of several, so naming one of them is a wrong answer", teamRow.RepoID)
	}
	if len(teamRow.Backends) != 2 || teamRow.Backends[0] != "claude_code" || teamRow.Backends[1] != "claw" {
		t.Fatalf("summed backends = %v, want [claude_code claw] — the union, sorted", teamRow.Backends)
	}

	// The new query: one repository's month, rows kept apart.
	byRepo, err := c.ListByRepo(ctx, sept, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if len(byRepo) != 1 || byRepo[0].CostUSD != 1.0 || byRepo[0].RepoID != repoA {
		t.Fatalf("ListByRepo(%q) = %+v, want one $1.00 row carrying the repo", repoA, byRepo)
	}
	// An empty repo returns NOTHING rather than every unattributed row: a
	// caller asking a repository's spend must never be handed the
	// deployment's.
	if n, err := c.ListByRepo(ctx, sept, ""); err != nil || len(n) != 0 {
		t.Fatalf("ListByRepo(\"\") = %v, %v; want nothing", n, err)
	}
	if n, _ := c.ListByRepo(ctx, sept, "SocialGouv/never-ran"); len(n) != 0 {
		t.Fatalf("ListByRepo(unknown repo) = %v, want nothing", n)
	}

	// A second credential on the same repository joins it there, which is
	// what makes the row a REPO's bill rather than a credential's.
	add(Key{Fingerprint: "fp-plat", Provider: "openai", Tier: TierPlatform, TenantID: "team-a", RepoID: repoA},
		NatureEstimate, "codex", 0.25, 10, 2, sept)
	if byRepo, _ = c.ListByRepo(ctx, sept, repoA); len(byRepo) != 2 {
		t.Fatalf("ListByRepo(%q) = %d rows, want 2 (both credentials that served it)", repoA, len(byRepo))
	}

	// Order is deterministic: the summed sequence is what a client diffs
	// between two polls, and a map iteration would reshuffle ties.
	first, _ := c.List(ctx, sept, "team-a")
	second, _ := c.List(ctx, sept, "team-a")
	for i := range first {
		if first[i].Fingerprint != second[i].Fingerprint || first[i].Tier != second[i].Tier {
			t.Fatalf("two identical List calls disagreed at %d: %s/%s vs %s/%s",
				i, first[i].Fingerprint, first[i].Tier, second[i].Fingerprint, second[i].Tier)
		}
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
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_credusage_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runCounterConformance(t, NewMongoCounter(db))
}
