package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// TestMain pins the process-wide spec registry to a nonexistent cache with
// auto-fetch off, so this package's tests never spawn a background network
// goroutine and never resolve against the host's real
// ~/.iterion/model-specs-cache.json. Without it, curated-equality assertions
// (TestCapabilitiesForModel) flake on a dev machine that has run a real bot —
// clean CI has no cache, so the failure only ever appeared locally.
func TestMain(m *testing.M) {
	restore := modelspecs.SetDefault(modelspecs.New(modelspecs.Options{
		CachePath:   filepath.Join(os.TempDir(), "iterion-modelspecs-test-absent.json"),
		NoAutoFetch: true,
	}))
	code := m.Run()
	restore()
	os.Exit(code)
}

// seedSpecs installs a registry answering exactly the given specs for the
// duration of a test. Keyed the way the aggregator's flat table is
// ("provider/model", lowercased), so the bare index is derived by the same code
// the real fetch path uses.
func seedSpecs(t *testing.T, flat map[string]modelspecs.Spec) {
	t.Helper()
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(flat)))
}

func boolp(b bool) *bool { return &b }

// The aggregator supplies; the curated table decides. These cases pin which
// half wins for each field — the rule that lets a brand-new model resolve
// correctly before any aggregator carries it.
func TestMergeSpec_AggregatorOverridesCurated(t *testing.T) {
	curated := curatedCapabilities("anthropic", "claude-sonnet-4-6")
	got := mergeSpec(modelspecs.Spec{
		ContextWindow:   1_000_000,
		MaxOutputTokens: 64_000,
		InputCostPerM:   3,
		OutputCostPerM:  15,
		Temperature:     boolp(false),
	}, curated)

	if got.ContextWindow != 1_000_000 || got.MaxOutputTokens != 64_000 {
		t.Errorf("limits = %d/%d, want 1000000/64000", got.ContextWindow, got.MaxOutputTokens)
	}
	if got.InputCostPerM != 3 || got.OutputCostPerM != 15 {
		t.Errorf("price = %v/%v, want 3/15", got.InputCostPerM, got.OutputCostPerM)
	}
	if got.Temperature {
		t.Error("an explicit aggregator false must override the curated true")
	}
	// A nil flag is "unstated", not false — the curated heuristic stands.
	if got.ToolCall != curated.ToolCall || got.Reasoning != curated.Reasoning {
		t.Errorf("unstated flags = tool:%v reason:%v, want curated tool:%v reason:%v",
			got.ToolCall, got.Reasoning, curated.ToolCall, curated.Reasoning)
	}
}

// A zeroed numeric field means the publishers disagreed (consensusSpec) or the
// source omitted it. Either way the curated value must survive — this is the
// half that keeps a randomly-drawn 200K from replacing a curated 1M.
func TestMergeSpec_ZeroLeavesCuratedStanding(t *testing.T) {
	curated := curatedCapabilities("anthropic", "glm-5.2")
	if curated.ContextWindow != 1_000_000 {
		t.Fatalf("fixture assumption broken: curated glm-5.2 window = %d", curated.ContextWindow)
	}
	got := mergeSpec(modelspecs.Spec{}, curated)
	if got.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want the curated 1000000", got.ContextWindow)
	}
	if got.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens = %d, want 0 (unknown, and no curated cap exists)", got.MaxOutputTokens)
	}
}

// The end-to-end read path: a model the aggregator carries resolves from it,
// and one it omits keeps the curated numbers. glm is the live instance —
// aggregators still omit glm-5.2, whose curated window is 1M while its 5.1/4.6
// siblings are 200K.
func TestCapabilitiesForModel_CuratedWinsWhenAggregatorOmits(t *testing.T) {
	seedSpecs(t, map[string]modelspecs.Spec{
		"anthropic/claude-sonnet-4-6": {ContextWindow: 900_000, MaxOutputTokens: 64_000, InputCostPerM: 3, OutputCostPerM: 15},
	})

	got := capabilitiesForModel("anthropic", "claude-sonnet-4-6")
	if got.ContextWindow != 900_000 || got.MaxOutputTokens != 64_000 {
		t.Errorf("aggregator-known model = %d/%d, want 900000/64000", got.ContextWindow, got.MaxOutputTokens)
	}

	for _, c := range []struct {
		model string
		want  int
	}{
		{"glm-5.2", 1_000_000},
		{"glm-5.1", 200_000},
		{"glm-4.6", 200_000},
	} {
		if got := capabilitiesForModel("anthropic", c.model).ContextWindow; got != c.want {
			t.Errorf("%s ContextWindow = %d, want %d (curated fallback)", c.model, got, c.want)
		}
	}
}

// ITERION_MODEL_SPECS=off must resolve to the curated table alone.
func TestCapabilitiesForModel_DisabledRegistryIsPureCurated(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.New(modelspecs.Options{Disabled: true})))

	got := capabilitiesForModel("anthropic", "claude-sonnet-4-6")
	if want := curatedCapabilities("anthropic", "claude-sonnet-4-6"); got != want {
		t.Errorf("disabled resolution = %+v, want curated %+v", got, want)
	}
}

// An aggregator ENTRY is not an aggregator ANSWER. The bare index is
// consensus-filtered, so a name several publishers quote differently resolves
// to an entry with every field zeroed — and the resolved capabilities are then
// wholly curated. Reporting SourceAggregator for that credits the wrong source
// on `iterion models`, on GET /api/model-capabilities and in the studio's model
// caption, where an "aggregator" answer is additionally cached for the whole
// session as if it were settled.
func TestResolveCapabilities_EmptyConsensusIsCuratedNotAggregator(t *testing.T) {
	// Neither publisher lists the id under "anthropic", and they disagree on
	// every field, so consensusSpec empties the bare entry.
	seedSpecs(t, map[string]modelspecs.Spec{
		"zai/glm-5.2":     {ContextWindow: 200_000, InputCostPerM: 0.6, OutputCostPerM: 2.2},
		"alibaba/glm-5.2": {ContextWindow: 128_000, InputCostPerM: 1.44, OutputCostPerM: 4},
	})

	rc := ResolveCapabilities("anthropic", "glm-5.2")
	if rc.Source != SourceCurated {
		t.Errorf("Source = %q, want %q: every field came from the curated table", rc.Source, SourceCurated)
	}
	if rc.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want 1000000 (curated glm-5.2 must survive)", rc.ContextWindow)
	}
	if rc.InputCostPerM != 0 || rc.OutputCostPerM != 0 {
		t.Errorf("price = %v/%v, want 0/0: the publishers disagreed", rc.InputCostPerM, rc.OutputCostPerM)
	}

	// The bare endpoint path answers the same way: nothing to supply is not a
	// hit, so a client is never told the aggregator answered with a row of
	// zeroes.
	if bare := ResolveBareModel("glm-5.2"); bare.Found {
		t.Errorf("ResolveBareModel(%q).Found = true, want false on an emptied consensus entry", "glm-5.2")
	}

	// A partial consensus still counts: one surviving field IS a contribution.
	seedSpecs(t, map[string]modelspecs.Spec{
		"zai/glm-5.2":     {ContextWindow: 200_000, MaxOutputTokens: 64_000, InputCostPerM: 0.6},
		"alibaba/glm-5.2": {ContextWindow: 128_000, MaxOutputTokens: 64_000, InputCostPerM: 1.44},
	})
	rc = ResolveCapabilities("anthropic", "glm-5.2")
	if rc.Source != SourceAggregator {
		t.Errorf("Source = %q, want %q: max output survived consensus", rc.Source, SourceAggregator)
	}
	if rc.MaxOutputTokens != 64_000 {
		t.Errorf("MaxOutputTokens = %d, want 64000", rc.MaxOutputTokens)
	}
}
