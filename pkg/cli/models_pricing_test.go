package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// The audit reports on a price, which is a budget decision, and it had no test
// at all — so every verdict it printed was asserted by nobody. These pin the
// three it can produce, hermetically: the fixture registry feeds BOTH sides of
// the comparison (model.ResolveSpec's published column and, through the
// estimator's aggregator tier, the effective one), and claw's live registry is
// switched off so the host's cache cannot answer instead.
func runAudit(t *testing.T, table map[string]modelspecs.Spec) (PricingResult, string) {
	t.Helper()
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(table)))

	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}
	if err := RunModelsPricing(context.Background(), PricingOptions{}, p); err != nil {
		t.Fatalf("RunModelsPricing: %v", err)
	}

	// Re-run in JSON to get the structured rows the human table renders.
	var jbuf bytes.Buffer
	jp := &Printer{W: &jbuf, Format: OutputJSON}
	_ = RunModelsPricing(context.Background(), PricingOptions{}, jp)
	return decodeResult(t, jbuf.Bytes()), buf.String()
}

func decodeResult(t *testing.T, data []byte) PricingResult {
	t.Helper()
	var r PricingResult
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode audit JSON: %v\n%s", err, data)
	}
	return r
}

func rowFor(t *testing.T, r PricingResult, spec string) PricingRow {
	t.Helper()
	for _, row := range r.Rows {
		if row.Spec == spec {
			return row
		}
	}
	t.Fatalf("no audit row for %q", spec)
	return PricingRow{}
}

// A published pair the estimator now consumes must read as agreement, not as
// drift. Before the estimator gained its aggregator tier this same fixture
// produced DISAGREES for every model whose table entry differed from the
// published rate.
func TestRunModelsPricing_PublishedPairAgreesWithTheChargedRate(t *testing.T) {
	res, out := runAudit(t, map[string]modelspecs.Spec{
		// Deliberately unlike the committed haiku entry (1/5): if the
		// estimator were still table-only, this would be flagged.
		"anthropic/claude-haiku-4-5": {InputCostPerM: 9, OutputCostPerM: 45},
	})

	row := rowFor(t, res, "anthropic/claude-haiku-4-5")
	if !row.HasFetched || row.FetchedIn != 9 || row.FetchedOut != 45 {
		t.Fatalf("published column = %v/%v (has=%v), want 9/45", row.FetchedIn, row.FetchedOut, row.HasFetched)
	}
	if row.EffectiveIn != 9 || row.EffectiveOut != 45 {
		t.Errorf("charged rate = %v/%v, want the published 9/45 — the estimator's aggregator tier",
			row.EffectiveIn, row.EffectiveOut)
	}
	if row.Disagrees {
		t.Error("a published pair the estimator uses must not be reported as drift")
	}
	if strings.Contains(out, "DISAGREES") {
		t.Errorf("human output reports DISAGREES:\n%s", out)
	}
}

// IGNORED narrows to the half-published pair — the one shape the estimator
// still refuses, because pricing the missing half at zero reads as a bargain
// and is simply wrong.
func TestRunModelsPricing_HalfPublishedPairIsIgnoredAndCounted(t *testing.T) {
	res, out := runAudit(t, map[string]modelspecs.Spec{
		// Input only. The static table has no glm entry, so nothing else can
		// price it and the effective rate stays absent.
		"anthropic/glm-5.2": {InputCostPerM: 0.6},
	})

	row := rowFor(t, res, "anthropic/glm-5.2")
	if !row.HasFetched {
		t.Fatal("half a published pair must still count as published")
	}
	if row.HasEffective {
		t.Errorf("charged rate = %v/%v, want none: a half pair is refused whole",
			row.EffectiveIn, row.EffectiveOut)
	}
	if !row.OnlyFetched {
		t.Error("verdict should be IGNORED — published, and a run reports nothing")
	}
	if res.DriftCount < 1 {
		t.Error("an ignored published price must count toward drift")
	}
	if !strings.Contains(out, "IGNORED") {
		t.Errorf("human output omits the IGNORED verdict:\n%s", out)
	}
}

// Nothing published: the committed table is the answer and the audit says so
// rather than inventing drift. This is the brand-new-model case the table
// exists for.
func TestRunModelsPricing_TableOnlyWhenNothingIsPublished(t *testing.T) {
	res, _ := runAudit(t, nil)

	row := rowFor(t, res, "anthropic/claude-haiku-4-5")
	if row.HasFetched {
		t.Fatal("fixture is empty; nothing may be reported as published")
	}
	if !row.HasStatic || row.StaticIn != 1 || row.StaticOut != 5 {
		t.Fatalf("committed haiku rate = %v/%v, want 1/5", row.StaticIn, row.StaticOut)
	}
	if !row.OnlyStatic || row.Disagrees {
		t.Errorf("verdict = onlyStatic:%v disagrees:%v, want table-only", row.OnlyStatic, row.Disagrees)
	}
	if res.DriftCount != 0 {
		t.Errorf("DriftCount = %d, want 0 — an unpublished price is not drift", res.DriftCount)
	}
}

// --check is the CI gate, so its exit condition is pinned: drift errors, and
// the message names what an operator must go decide.
func TestRunModelsPricing_CheckFailsOnDrift(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"anthropic/glm-5.2": {InputCostPerM: 0.6}, // half a pair → IGNORED
	})))

	var buf bytes.Buffer
	err := RunModelsPricing(context.Background(), PricingOptions{Check: true},
		&Printer{W: &buf, Format: OutputHuman})
	if err == nil {
		t.Fatal("--check returned nil despite an ignored published price")
	}
	if !strings.Contains(err.Error(), "pricing drift") {
		t.Errorf("error = %q, want it to name the drift", err)
	}

	// And it stays green when nothing drifted.
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(nil)))
	buf.Reset()
	if err := RunModelsPricing(context.Background(), PricingOptions{Check: true},
		&Printer{W: &buf, Format: OutputHuman}); err != nil {
		t.Errorf("--check with no drift = %v, want nil", err)
	}
}
