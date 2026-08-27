package model

import "testing"

func boolPtr(b bool) *bool { return &b }

// The bare-name index was built by assigning into a map while ranging over
// one. Go randomises map iteration, so a model published by several providers
// resolved to a different provider's numbers on every process start — five
// consecutive runs of the same command produced five different prices for
// glm-5.2, one of them zero. The same index feeds the context window, where a
// silent 200K instead of 1M truncates work rather than merely mis-reporting a
// cost.
//
// These tests pin both halves of the fix: the grouping is deterministic, and
// disagreement resolves to UNKNOWN instead of to an arbitrary winner.
func TestBareIndexIsDeterministicAcrossBuilds(t *testing.T) {
	flat := map[string]fetchedSpec{
		"zai/glm-5.2":        {ContextWindow: 1_000_000, InputCostPerM: 0.50, OutputCostPerM: 2.20, Reasoning: boolPtr(true)},
		"novita/glm-5.2":     {ContextWindow: 1_000_000, InputCostPerM: 1.44, OutputCostPerM: 4.53, Reasoning: boolPtr(true)},
		"deepinfra/glm-5.2":  {ContextWindow: 1_000_000, InputCostPerM: 1.10, OutputCostPerM: 3.85, Reasoning: boolPtr(true)},
		"alibaba/glm-5.2":    {ContextWindow: 1_000_000, InputCostPerM: 0, OutputCostPerM: 0, Reasoning: boolPtr(true)},
		"anthropic/claude-x": {ContextWindow: 200_000, InputCostPerM: 3, OutputCostPerM: 15, Reasoning: boolPtr(true)},
	}

	var first fetchedSpec
	for i := 0; i < 50; i++ {
		r := &specRegistry{}
		r.indexLocked(flat)
		got := r.byModel["glm-5.2"]
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("bare index is not deterministic: build %d gave %+v, build 0 gave %+v", i, got, first)
		}
	}
}

func TestConsensusKeepsAgreementAndDropsConflict(t *testing.T) {
	flat := map[string]fetchedSpec{
		// Four providers agree on the context window and on reasoning, and
		// disagree on price. Price must vanish; the rest must survive.
		"zai/glm-5.2":       {ContextWindow: 1_000_000, InputCostPerM: 0.50, OutputCostPerM: 2.20, Reasoning: boolPtr(true), ToolCall: boolPtr(true)},
		"novita/glm-5.2":    {ContextWindow: 1_000_000, InputCostPerM: 1.44, OutputCostPerM: 4.53, Reasoning: boolPtr(true), ToolCall: boolPtr(true)},
		"deepinfra/glm-5.2": {ContextWindow: 1_000_000, InputCostPerM: 1.10, OutputCostPerM: 3.85, Reasoning: boolPtr(true), ToolCall: boolPtr(false)},
	}
	r := &specRegistry{}
	r.indexLocked(flat)
	got := r.byModel["glm-5.2"]

	if got.ContextWindow != 1_000_000 {
		t.Errorf("context window %d: unanimous values must survive", got.ContextWindow)
	}
	if got.InputCostPerM != 0 || got.OutputCostPerM != 0 {
		t.Errorf("price %v/%v: providers disagree, so the answer must be unknown rather than one of them",
			got.InputCostPerM, got.OutputCostPerM)
	}
	if got.Reasoning == nil || !*got.Reasoning {
		t.Error("reasoning: unanimous flags must survive")
	}
	if got.ToolCall != nil {
		t.Error("tool_call: providers disagree, so the flag must be unstated and fall back to the curated heuristic")
	}
}

// A single provider is not a disagreement — the common case must pass through
// untouched, or the fix would blind the index it was meant to repair.
func TestSingleProviderPassesThrough(t *testing.T) {
	flat := map[string]fetchedSpec{
		"anthropic/claude-opus-5": {ContextWindow: 1_000_000, InputCostPerM: 5, OutputCostPerM: 25, Reasoning: boolPtr(true)},
	}
	r := &specRegistry{}
	r.indexLocked(flat)

	bare := r.byModel["claude-opus-5"]
	if bare.InputCostPerM != 5 || bare.OutputCostPerM != 25 || bare.ContextWindow != 1_000_000 {
		t.Errorf("single-provider spec altered: %+v", bare)
	}
	// The provider-qualified index is the precise one and must stay exact:
	// when the provider IS known, its own numbers are the right answer.
	full := r.byFull["anthropic/claude-opus-5"]
	if full.InputCostPerM != 5 || full.OutputCostPerM != 25 {
		t.Errorf("provider-qualified spec altered: %+v", full)
	}
}

// Unanimous providers must still yield an answer — otherwise the consensus
// rule would erase every multi-provider model, including the Claude family,
// where all publishers quote the same vendor price.
func TestUnanimousProvidersStillResolve(t *testing.T) {
	flat := map[string]fetchedSpec{
		"anthropic/claude-opus-5":      {ContextWindow: 1_000_000, InputCostPerM: 5, OutputCostPerM: 25, Reasoning: boolPtr(true)},
		"azure/claude-opus-5":          {ContextWindow: 1_000_000, InputCostPerM: 5, OutputCostPerM: 25, Reasoning: boolPtr(true)},
		"github-copilot/claude-opus-5": {ContextWindow: 1_000_000, InputCostPerM: 5, OutputCostPerM: 25, Reasoning: boolPtr(true)},
	}
	r := &specRegistry{}
	r.indexLocked(flat)
	got := r.byModel["claude-opus-5"]
	if got.InputCostPerM != 5 || got.OutputCostPerM != 25 {
		t.Errorf("unanimous price lost: %+v", got)
	}
	if got.ContextWindow != 1_000_000 {
		t.Errorf("unanimous context window lost: %d", got.ContextWindow)
	}
}

// Max output joins context window and price as a consensus-filtered numeric
// field. It is tested apart from them because it was the one field parsed and
// cached but never surfaced: nothing downstream could have caught a regression
// where disagreeing providers hand a caller one publisher's completion cap.
func TestConsensusMaxOutputTokens(t *testing.T) {
	agree := map[string]fetchedSpec{
		"anthropic/claude-opus-5": {MaxOutputTokens: 64_000},
		"azure/claude-opus-5":     {MaxOutputTokens: 64_000},
	}
	r := &specRegistry{}
	r.indexLocked(agree)
	if got := r.byModel["claude-opus-5"].MaxOutputTokens; got != 64_000 {
		t.Errorf("unanimous max output = %d, want 64000", got)
	}

	disagree := map[string]fetchedSpec{
		"zai/glm-5.2":    {MaxOutputTokens: 128_000},
		"novita/glm-5.2": {MaxOutputTokens: 32_000},
	}
	r2 := &specRegistry{}
	r2.indexLocked(disagree)
	if got := r2.byModel["glm-5.2"].MaxOutputTokens; got != 0 {
		t.Errorf("conflicting max output = %d, want 0 (unknown, not one publisher's cap)", got)
	}
}
