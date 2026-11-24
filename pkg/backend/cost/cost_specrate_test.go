package cost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	clawapi "github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// TestMain pins both live tiers OFF for the whole package (this package sits
// below internal/pricingtest, which imports it, so it isolates itself). Every
// other test here asserts an exact dollar figure or an exact zero, and the
// real tiers answer from the host's caches and refresh them from the network
// — so without this the figures would be whatever the developer's machine
// last fetched. Both seams are stubbed (tests opt back in via withSpecs and
// withLivePricing), and claw's registry is also switched off on an empty
// cache this binary owns, for anything that reaches it past the seam.
func TestMain(m *testing.M) {
	specLookup = stubSpecLookup
	// The process registry too: anything reaching it past the seam — a stub
	// gone live, a test putting the real wiring back — finds a seeded table that
	// never fetches.
	modelspecs.SetDefault(modelspecs.NewSeeded(nil))
	livePricing = stubLivePricing
	cache, err := os.MkdirTemp("", "cost-claw-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cost TestMain:", err)
		os.Exit(1)
	}
	if err := os.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1"); err != nil {
		fmt.Fprintln(os.Stderr, "cost TestMain:", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_CACHE_HOME", cache); err != nil {
		fmt.Fprintln(os.Stderr, "cost TestMain:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.Remove(cache) // empty unless a test wrote there; never a tree walk
	os.Exit(code)
}

// stubSpecLookup is the aggregator tier as TestMain pins it: it knows nothing.
func stubSpecLookup(string, string) (modelspecs.Spec, bool) { return modelspecs.Spec{}, false }

// stubLivePricing is claw's tier as TestMain pins it: it prices nothing.
func stubLivePricing(string) (clawapi.ModelPricing, bool) { return clawapi.ModelPricing{}, false }

// TestTheTestBinaryRunsOnStubbedPricingTiers is the run-time witness
// internal/pricingtest's guard requires of this package: both live tiers are
// stubbed and claw's registry is switched off, whatever TestMain looks like.
func TestTheTestBinaryRunsOnStubbedPricingTiers(t *testing.T) {
	if got := os.Getenv("CLAW_DISABLE_LIVE_REGISTRY"); got != "1" {
		t.Errorf("CLAW_DISABLE_LIVE_REGISTRY = %q, want 1: claw's registry would read the host's cache and refresh it", got)
	}
	// The function itself, not its answer: with the switch on, claw's real
	// lookup answers nothing either.
	if reflect.ValueOf(livePricing).Pointer() != reflect.ValueOf(stubLivePricing).Pointer() {
		t.Error("livePricing is not stubLivePricing: claw's tier seam is not stubbed")
	}
	if reflect.ValueOf(specLookup).Pointer() != reflect.ValueOf(stubSpecLookup).Pointer() {
		t.Error("specLookup is not stubSpecLookup: the aggregator seam is not stubbed")
	}
	if err := modelspecs.Default().Refresh(context.Background()); !errors.Is(err, modelspecs.ErrSeededRegistryNeverFetches) {
		t.Errorf("the process registry refreshed (%v): TestMain did not seed it, so a lookup past the seam reaches models.dev", err)
	}
}

// withSpecs points the aggregator tier at a fixture for the duration of a test.
func withSpecs(t *testing.T, table map[string]modelspecs.Spec) {
	t.Helper()
	prev := specLookup
	specLookup = func(provider, bare string) (modelspecs.Spec, bool) {
		// Mirrors the registry's own order: the qualified key first, the
		// bare id second.
		if s, ok := table[provider+"/"+bare]; ok {
			return s, true
		}
		s, ok := table[bare]
		return s, ok
	}
	t.Cleanup(func() { specLookup = prev })
}

// The done-state of ADR-042's follow-through: a model the aggregator prices is
// priced from those rates, and a model it does not know still falls back to the
// committed table.
func TestEstimateUSD_UsesAggregatorPricing(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1") // isolate tier 2 from tier 1
	withSpecs(t, map[string]modelspecs.Spec{
		// A rate deliberately unlike the committed opus entry (5/25), so the
		// figure names which source answered.
		"claude-opus-5": {InputCostPerM: 7, OutputCostPerM: 35},
		// A model the committed table has never carried — the case the whole
		// tier exists for: published for months, priced at nothing.
		"glm-5.2": {InputCostPerM: 0.6, OutputCostPerM: 2.2},
	})

	if got := EstimateUSD("claude-opus-5", 1_000_000, 1_000_000); got != 42 {
		t.Errorf("aggregator-priced model = %v, want 42 (7+35); the static table would say 30", got)
	}
	// The qualified and region-prefixed forms resolve to the same bare id.
	if got := EstimateUSD("anthropic/eu/claude-opus-5", 1_000_000, 0); got != 7 {
		t.Errorf("region-prefixed spec = %v, want 7", got)
	}
	if got := EstimateUSD("anthropic/glm-5.2", 1_000_000, 1_000_000); got != 2.8 {
		t.Errorf("table-less model = %v, want 2.8 (0.6+2.2)", got)
	}
	// Unknown to the aggregator → the committed table, unchanged.
	if got := EstimateUSD("claude-haiku-4-5", 1_000_000, 1_000_000); got != 6 {
		t.Errorf("aggregator-unknown model = %v, want the static 6", got)
	}
	// Unknown to both stays unpriced: zero means "no cost data", and a
	// caller must not read it as a free call.
	if got := EstimateUSD("made-up-model", 1_000_000, 1_000_000); got != 0 {
		t.Errorf("unknown-everywhere model = %v, want 0", got)
	}
}

// The two rates are published and consensus-filtered independently, so a
// half-known pair is routine. Taking it would price the missing half at zero —
// which reads as a bargain and is simply wrong. A half-published pair falls
// through WHOLE to the table; a rate mixing the two sources would be traceable
// to neither.
func TestEstimateUSD_PartialAggregatorPriceFallsThroughWhole(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")

	// The committed haiku rate is 1/5, so a full fall-through prices 1M+1M
	// at 6.00 — and any half-taken pair would land somewhere else.
	cases := []struct {
		name string
		spec modelspecs.Spec
	}{
		{"input only", modelspecs.Spec{InputCostPerM: 99}},
		{"output only", modelspecs.Spec{OutputCostPerM: 99}},
		{"both zero", modelspecs.Spec{}},
		{"negative input", modelspecs.Spec{InputCostPerM: -1, OutputCostPerM: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withSpecs(t, map[string]modelspecs.Spec{"claude-haiku-4-5": c.spec})
			if got := EstimateUSD("claude-haiku-4-5", 1_000_000, 1_000_000); got != 6 {
				t.Errorf("EstimateUSD = %v, want the static 6 — a half-published pair must fall through whole", got)
			}
		})
	}
}

// Tier order: claw's live registry keeps its precedence, so no run's charged
// rate moves because iterion started reading its own aggregator.
func TestEstimateUSD_LiveRegistryStillOutranksAggregator(t *testing.T) {
	withLivePricing(t, map[string]clawapi.ModelPricing{"gpt-5": {InputUSDPerMillion: 99, OutputUSDPerMillion: 999, Source: "live"}})
	withSpecs(t, map[string]modelspecs.Spec{"gpt-5": {InputCostPerM: 1, OutputCostPerM: 2}})

	if got := EstimateUSD("gpt-5", 1_000_000, 1_000_000); got != 1098 {
		t.Errorf("EstimateUSD = %v, want 1098 (claw's 99+999); the aggregator would say 3", got)
	}
}

// The production seam, exercised end to end: the package var must read through
// the process-wide registry's bare index, not just through whatever a test
// substituted.
func TestSpecLookup_ReadsTheProcessRegistry(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	// Restore the real wiring for this test only.
	prev := specLookup
	specLookup = func(provider, bare string) (modelspecs.Spec, bool) {
		return modelspecs.Default().Lookup(provider, bare)
	}
	t.Cleanup(func() { specLookup = prev })

	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"z-ai/glm-5.2": {InputCostPerM: 0.6, OutputCostPerM: 2.2},
	})))

	// glm arrives as "anthropic/glm-5.2" (z.ai's Anthropic-compatible
	// endpoint) and is published under a different provider — the bare index
	// is what bridges the two.
	if got := EstimateUSD("anthropic/glm-5.2", 1_000_000, 1_000_000); got != 2.8 {
		t.Errorf("EstimateUSD via the process registry = %v, want 2.8", got)
	}
}

// EffectiveRate is what the pricing audit judges against, so it must report the
// aggregator tier too — otherwise the audit would flag a published price as
// "IGNORED" while the estimator was using it.
func TestEffectiveRate_ReportsAggregatorTier(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	withSpecs(t, map[string]modelspecs.Spec{"glm-5.2": {InputCostPerM: 0.6, OutputCostPerM: 2.2}})

	in, out, ok := EffectiveRate("anthropic/glm-5.2")
	if !ok || in != 0.6 || out != 2.2 {
		t.Errorf("EffectiveRate = %v/%v (ok=%v), want 0.6/2.2", in, out, ok)
	}
	if _, _, ok := StaticRate("glm-5.2"); ok {
		t.Error("fixture assumption broken: glm-5.2 gained a committed entry")
	}
}

// The qualified key must win over the bare one. This is not a nicety: the bare
// index is consensus-filtered, so it reports UNKNOWN as soon as two publishers
// quote different rates — while the provider's OWN entry holds the right
// number. Resolving by the bare index alone would drop a model to the static
// table the moment a second publisher listed it, which is the common case for
// anything popular.
//
// glm-5.2 is the fixture because it has NO committed table entry by design, so
// the bare path has nothing to land on: with a bare-only lookup this test
// reads 0, and the assertion falsifies the ordering rather than restating it.
func TestEstimateUSD_QualifiedRateBeatsTheConsensusBareOne(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	// Restore the real wiring: the ordering under test is the registry's.
	prev := specLookup
	specLookup = func(provider, bare string) (modelspecs.Spec, bool) {
		return modelspecs.Default().Lookup(provider, bare)
	}
	t.Cleanup(func() { specLookup = prev })

	// Two publishers disagreeing on the price. consensusSpec zeroes it in the
	// bare index; each provider's own entry keeps its number.
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"z-ai/glm-5.2":   {InputCostPerM: 0.6, OutputCostPerM: 2.2},
		"novita/glm-5.2": {InputCostPerM: 1.44, OutputCostPerM: 4.53},
	})))
	if _, _, ok := StaticRate("glm-5.2"); ok {
		t.Fatal("fixture assumption broken: glm-5.2 gained a committed table entry")
	}

	if got := EstimateUSD("z-ai/glm-5.2", 1_000_000, 1_000_000); got != 2.8 {
		t.Errorf("qualified spec = %v, want 2.8 (z-ai's own 0.6+2.2); a bare-only lookup reads 0 here", got)
	}
	if got := EstimateUSD("novita/glm-5.2", 1_000_000, 1_000_000); got != 5.97 {
		t.Errorf("the other publisher's spec = %v, want 5.97 (1.44+4.53)", got)
	}
	// With no provider there is genuinely no answer: the publishers disagree,
	// and iterion must not pick one of them. Unpriced, not half-priced.
	if got := EstimateUSD("glm-5.2", 1_000_000, 1_000_000); got != 0 {
		t.Errorf("bare, contested spec = %v, want 0 — unknown, never one publisher's rate", got)
	}
}
