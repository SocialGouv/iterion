package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

func TestModelOverrides_ForNode_Precedence(t *testing.T) {
	// Compose: run-wide backend + group model + exact-id model.
	o, err := ParseModelOverrides(
		[]string{"reviewer_*=anthropic/claude-fable-5", "reviewer_claude=anthropic/claude-opus-4-8"},
		[]string{"claw"}, // bare value → selector "*"
		nil,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		id          string
		kind        ir.NodeKind
		wantModel   string
		wantBackend string
	}{
		// exact id beats the group glob for model; backend from run-wide "*".
		{"reviewer_claude", ir.NodeJudge, "anthropic/claude-opus-4-8", "claw"},
		// group glob applies; still gets run-wide backend.
		{"reviewer_gpt", ir.NodeJudge, "anthropic/claude-fable-5", "claw"},
		// unrelated node: only the run-wide backend applies, no model override.
		{"fix_claude", ir.NodeAgent, "", "claw"},
	}
	for _, tt := range tests {
		got := o.ForNode(tt.id, tt.kind)
		if got.Model != tt.wantModel {
			t.Errorf("%s: model = %q, want %q", tt.id, got.Model, tt.wantModel)
		}
		if got.Backend != tt.wantBackend {
			t.Errorf("%s: backend = %q, want %q", tt.id, got.Backend, tt.wantBackend)
		}
	}
}

func TestModelOverrides_KindKeyword(t *testing.T) {
	var o ModelOverrides
	o.SetModel("judge", "openai/gpt-5.5")
	o.SetBackend("agent", "claude_code")

	if got := o.ForNode("reviewer_x", ir.NodeJudge); got.Model != "openai/gpt-5.5" || got.Backend != "" {
		t.Errorf("judge node: got %+v, want model=openai/gpt-5.5 backend=empty", got)
	}
	if got := o.ForNode("fix_x", ir.NodeAgent); got.Backend != "claude_code" || got.Model != "" {
		t.Errorf("agent node: got %+v, want backend=claude_code model=empty", got)
	}
	// A more specific glob beats the kind keyword.
	o.SetModel("reviewer_*", "anthropic/claude-fable-5")
	if got := o.ForNode("reviewer_x", ir.NodeJudge); got.Model != "anthropic/claude-fable-5" {
		t.Errorf("glob should beat kind keyword: got model %q", got.Model)
	}
}

func TestModelOverrides_EmptyIsNoop(t *testing.T) {
	var o ModelOverrides
	if !o.Empty() {
		t.Fatal("zero ModelOverrides should be Empty")
	}
	if got := o.ForNode("any", ir.NodeAgent); !got.Empty() {
		t.Errorf("empty overrides should resolve to empty, got %+v", got)
	}
}

func TestParseModelOverrides_Errors(t *testing.T) {
	if _, err := ParseModelOverrides([]string{"reviewer_*="}, nil, nil); err == nil {
		t.Error("expected error for empty value")
	}
	if _, err := ParseModelOverrides(nil, []string{"  "}, nil); err == nil {
		t.Error("expected error for blank arg")
	}
	// bare value is valid (selector "*").
	o, err := ParseModelOverrides([]string{"anthropic/claude-opus-4-8"}, nil, nil)
	if err != nil {
		t.Fatalf("bare value should parse: %v", err)
	}
	if got := o.ForNode("whatever", ir.NodeAgent); got.Model != "anthropic/claude-opus-4-8" {
		t.Errorf("bare value should target all nodes, got %q", got.Model)
	}
}

// Effort rides the same selector machinery as model and backend, because the
// three are one decision — a model, the backend driving it, and how hard it is
// asked to think.
func TestModelOverrides_Effort(t *testing.T) {
	o, err := ParseModelOverrides(
		nil, nil,
		[]string{"high", "reviewer_*=max"}, // bare value → selector "*"
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := o.ForNode("fix_code", ir.NodeAgent).Effort; got != "high" {
		t.Errorf("run-wide effort = %q, want high", got)
	}
	if got := o.ForNode("reviewer_claude", ir.NodeJudge).Effort; got != "max" {
		t.Errorf("glob effort = %q, want max (more specific than the run-wide rule)", got)
	}
}

// The levels are a closed set that reaches the provider verbatim, so a typo
// has to die at the flag rather than on the run's first node.
func TestParseModelOverrides_RejectsUnknownEffort(t *testing.T) {
	if _, err := ParseModelOverrides(nil, nil, []string{"turbo"}); err == nil {
		t.Fatal("expected an error for an effort level that is not in the enum")
	}
	// ultracode is a mode, not a wire value, but it IS an accepted level.
	if _, err := ParseModelOverrides(nil, nil, []string{"ultracode"}); err != nil {
		t.Errorf("ultracode must parse: %v", err)
	}
}

func TestNodeModelOverride_EmptyCountsEffort(t *testing.T) {
	if (NodeModelOverride{Effort: "high"}).Empty() {
		t.Error("an effort-only override is not empty")
	}
}

// MergeOver is what lets a resume keep the model the run was LAUNCHED with
// while still honouring a flag typed for this attempt. The failure it guards
// against is wholesale replacement: `--effort-for '*=high'` on a resume must
// not silently discard the launch's model, and inheriting must not lock the
// operator out of re-targeting.
func TestMergeOver_FlagsWinPerFieldAndInheritTheRest(t *testing.T) {
	// What the run was launched with (persisted rows).
	var launched ModelOverrides
	launched.SetModel("*", "openai/gpt-5.5")
	launched.SetBackend("*", "claw")
	launched.SetEffort("*", "low")

	// What the operator typed for THIS resume: effort only.
	var flags ModelOverrides
	flags.SetEffort("*", "high")

	got := flags.MergeOver(launched).ForNode("chat", ir.NodeAgent)
	if got.Effort != "high" {
		t.Errorf("Effort = %q, want the flag to win", got.Effort)
	}
	if got.Model != "openai/gpt-5.5" || got.Backend != "claw" {
		t.Errorf("model/backend = %q/%q, want the launch's values inherited", got.Model, got.Backend)
	}
}

func TestMergeOver_EmptySidesAreIdentity(t *testing.T) {
	var launched ModelOverrides
	launched.SetModel("*", "anthropic/claude-opus-5")

	if got := (ModelOverrides{}).MergeOver(launched).ForNode("n", ir.NodeAgent); got.Model != "anthropic/claude-opus-5" {
		t.Errorf("no flags: Model = %q, want the launch's value", got.Model)
	}
	if got := launched.MergeOver(ModelOverrides{}).ForNode("n", ir.NodeAgent); got.Model != "anthropic/claude-opus-5" {
		t.Errorf("no base: Model = %q, want the flag's value", got.Model)
	}
	if !(ModelOverrides{}).MergeOver(ModelOverrides{}).Empty() {
		t.Error("merging two empty sets must stay empty")
	}
}

// The layer decides BEFORE selector specificity (the precedence line, ADR-129
// §6): the operator's explicit resume flags win any rule the run was launched
// with, even at a less specific selector — "never silently replace an
// operator's explicit choice" reads with the operator present. Within ONE
// layer, specificity rules as before (see ForNode_Precedence).
func TestMergeOver_TheTopLayerWinsAtEverySelectorPair(t *testing.T) {
	var launched ModelOverrides
	launched.SetModel("chat", "anthropic/claude-opus-5")

	var flags ModelOverrides
	flags.SetModel("*", "openai/gpt-5.5")

	merged := flags.MergeOver(launched)
	if got := merged.ForNode("chat", ir.NodeAgent); got.Model != "openai/gpt-5.5" {
		t.Errorf("chat Model = %q, want the flag to win (the layer outranks the exact id)", got.Model)
	}
	if got := merged.ForNode("other", ir.NodeAgent); got.Model != "openai/gpt-5.5" {
		t.Errorf("other Model = %q, want the broad flag", got.Model)
	}
}

// The O2 witness (ADR-129 §6): the launcher's canonical gesture is the
// LEAST specific selector — `--backend claw` and the studio dropdown both
// record "*" — so the LAYER decides before selector specificity. Without the
// rank comparison, an exact policy id (score 100000) silently outranks the
// operator's explicit `*` row.
func TestForNode_LauncherOutranksAPolicyCellAtEverySpecificity(t *testing.T) {
	var policy ModelOverrides
	policy.SetBackend("reviewer_claude", "claw") // exact id — score 100000
	policy.SetBackend("judge", "claw")           // kind — score 10

	var launcher ModelOverrides
	launcher.SetBackend("*", "claude_code") // score 1

	got := launcher.MergeOver(policy).ForNode("reviewer_claude", ir.NodeJudge)
	if got.Backend != "claude_code" {
		t.Fatalf("reviewer_claude backend = %q, want the launcher's claude_code (the layer outranks the exact id)", got.Backend)
	}
	got = launcher.MergeOver(policy).ForNode("reviewer_x", ir.NodeJudge)
	if got.Backend != "claude_code" {
		t.Fatalf("judge node backend = %q, want the launcher's claude_code (the layer outranks the kind)", got.Backend)
	}
	// The equal-selector case still goes to the launcher.
	var policyTie ModelOverrides
	policyTie.SetBackend("*", "claw")
	got = launcher.MergeOver(policyTie).ForNode("n", ir.NodeAgent)
	if got.Backend != "claude_code" {
		t.Fatalf("equal-selector backend = %q, want the launcher's", got.Backend)
	}
	// Within ONE layer, specificity still rules.
	var singleLayer ModelOverrides
	singleLayer.SetBackend("reviewer_claude", "claw")
	singleLayer.SetBackend("*", "codex")
	if got := singleLayer.ForNode("reviewer_claude", ir.NodeJudge); got.Backend != "claw" {
		t.Fatalf("same-layer exact id = %q, want claw (specificity within a layer is untouched)", got.Backend)
	}
	// The lower layer's fields the top layer leaves blank are inherited:
	// merging is not replace-wholesale, the cells stay independent.
	var policyModel ModelOverrides
	policyModel.SetModel("judge", "openai/gpt-5.5")
	got = launcher.MergeOver(policyModel).ForNode("j1", ir.NodeJudge)
	if got.Model != "openai/gpt-5.5" || got.Backend != "claude_code" {
		t.Fatalf("got %+v, want the launcher's backend with the policy's model inherited", got)
	}
}

// ConsumesProviderHint is the exported predicate the routing policy's local
// twin drift-tests against: the three classes must stay the three classes the
// executor retry logic acts on.
func TestConsumesProviderHint_Classes(t *testing.T) {
	chains, single := ConsumesProviderHint("claude_code")
	if !chains || single {
		t.Fatalf("claude_code = (%v, %v), want (true, false)", chains, single)
	}
	chains, single = ConsumesProviderHint("claw")
	if chains || !single {
		t.Fatalf("claw = (%v, %v), want (false, true)", chains, single)
	}
	for _, b := range []string{"codex", "pi", "grok", "kimi", "opencode", "", "unknown"} {
		chains, single = ConsumesProviderHint(b)
		if chains || single {
			t.Fatalf("%q = (%v, %v), want (false, false)", b, chains, single)
		}
	}
}

// HonoursProviderHintValue is the value-aware half of the hint gate: the
// claw registry's force-Anthropic-direct switch is wired to THIS
// predicate (registry.go), so the gate and the runtime read one function
// and the classes cannot certify a value the runtime ignores.
func TestHonoursProviderHintValue_Values(t *testing.T) {
	if !HonoursProviderHintValue("claw", "anthropic") {
		t.Fatal("claw/anthropic must be honoured — it is the one switch the registry acts on")
	}
	if !HonoursProviderHintValue("claw", " Anthropic ") {
		t.Fatal("claw must honour the folded spelling: the chain parse lower-cases every step before the registry reads it")
	}
	for _, p := range []string{"zai", "openrouter", "moonshot", "openai", "", "auto", "bogus"} {
		if HonoursProviderHintValue("claw", p) {
			t.Fatalf("claw/%q must NOT count as honoured — the registry ignores it and the spend follows the default precedence", p)
		}
	}
	for _, p := range []string{"anthropic", "zai,anthropic", "zai:glm-5.2"} {
		if !HonoursProviderHintValue("claude_code", p) {
			t.Fatalf("claude_code/%q must count as honoured: the chain is walked and an unfunded step is refused loudly at dispatch", p)
		}
	}
	for _, b := range []string{"codex", "pi", "grok", "kimi", "opencode", "", "unknown"} {
		if HonoursProviderHintValue(b, "anthropic") {
			t.Fatalf("%q honours no hint (its class is (false, false)); the value predicate must agree", b)
		}
	}
}

// Every provider shape the write gate accepts must reach dispatch as a
// chain of KNOWN hints — never one raw compound string. This is the
// parser-parity witness: llmroute's validator (providerHintTokens) and
// the credential preview (chainHints) parse the cell exactly the way
// parseProviderChain does here, so a pin certified at a write surface is
// walked at dispatch as the steps it names.
func TestOverrideProviderCell_ParsesLikeTheGateCertifies(t *testing.T) {
	// The vocabulary the gate checks against, straight from its owner.
	known := map[string]bool{"": true} // "" is the folded "auto" step
	for _, h := range llmroute.ProviderHints {
		known[h] = true
	}
	for _, cell := range []string{
		"anthropic",
		"zai,anthropic",
		"zai:glm-5.2,anthropic:claude-opus-4-8",
		"zai:glm-5.2,anthropic",
		"anthropic,",
		"zai,zai,anthropic",
		" zai , anthropic ",
	} {
		e := newExecutorForResolveTest("")
		e.modelOverrides.SetProvider("n", cell)
		node := nodeWithBackendProvider("claude_code", "")
		node.ID = "n"
		got := e.resolveProviderChain(node)
		if len(got) == 0 {
			t.Fatalf("override %q resolved to an empty chain", cell)
		}
		for _, step := range got {
			if !known[step.Provider] {
				t.Fatalf("override %q step %q is not a known provider hint — the raw cell leaked into dispatch", cell, step.Provider)
			}
			if step.Provider != strings.ToLower(step.Provider) {
				t.Fatalf("override %q step %q is not folded to its route form", cell, step.Provider)
			}
		}
		// Parity with the node's own DSL field: the same value on the
		// node's provider: must resolve to the same steps.
		e2 := newExecutorForResolveTest("")
		want := e2.resolveProviderChain(nodeWithBackendProvider("claude_code", cell))
		if !equalProviderSteps(got, want) {
			t.Fatalf("override %q -> %v, but the same value on the node's DSL field -> %v — the two branches must parse alike", cell, got, want)
		}
	}
	// The env-expanded spelling stays an override expressible value for
	// the launch rows (the policy gate refuses templates; the launch's
	// own rows carry the historical RESCUE_PROVIDER spelling).
	resetEnvForResolve(t)
	t.Setenv("RESCUE_PROVIDER", "zai")
	e := newExecutorForResolveTest("")
	e.modelOverrides.SetProvider("n", "${RESCUE_PROVIDER:-anthropic}")
	node := nodeWithBackendProvider("claude_code", "")
	node.ID = "n"
	got := e.resolveProviderChain(node)
	if len(got) != 1 || got[0].Provider != "zai" {
		t.Fatalf("override env expansion = %v, want one zai step", got)
	}
	// The malformed spelling with an empty model part is refused at the
	// write surface (llmroute's gate); were it to reach dispatch anyway,
	// it parses as the bare hint — the trim the refusal names.
	e4 := newExecutorForResolveTest("")
	e4.modelOverrides.SetProvider("n", "zai:")
	n4 := nodeWithBackendProvider("claude_code", "")
	n4.ID = "n"
	trimmed := e4.resolveProviderChain(n4)
	if len(trimmed) != 1 || trimmed[0].Provider != "zai" || trimmed[0].Model != "" {
		t.Fatalf(`"zai:" parsed to %v, want one bare zai step — an empty model part trims to the bare hint`, trimmed)
	}
}

// A rule's rank is a layer, not a presence flag: MergeOver stamps top+1
// and a producer BELOW the DSL layer would sit at -1. The per-field best
// is seeded below every rank, so the only rule naming a node is never
// dropped for its layer alone.
func TestForNode_ANegativeRankRuleStillResolves(t *testing.T) {
	neg := ModelOverrides{rules: []modelOverrideRule{{
		selector: "reviewer_claude", rank: -1,
		NodeModelOverride: NodeModelOverride{Backend: "claw"},
	}}}
	if got := neg.ForNode("reviewer_claude", ir.NodeJudge); got.Backend != "claw" {
		t.Fatalf("backend = %q, want claw — a negative rank is a layer, not a drop", got.Backend)
	}
}

// A layered o stays layered: MergeOver stamps o's rules relative to their
// own top, so the inner precedence of an earlier MergeOver survives a
// later stacking — the operator's flag layered over the launch's rows
// still beats the launched pin when the stack is layered over base again.
func TestMergeOver_ALayeredTopKeepsItsInnerOrder(t *testing.T) {
	var launched ModelOverrides
	launched.SetModel("chat", "anthropic/claude-opus-5")

	var flags ModelOverrides
	flags.SetModel("*", "openai/gpt-5.5")

	layered := flags.MergeOver(launched)
	merged := layered.MergeOver(launched)
	if got := merged.ForNode("chat", ir.NodeAgent); got.Model != "openai/gpt-5.5" {
		t.Fatalf("chat Model = %q, want the operator's flag (the layered top keeps its rank above the re-stamped base)", got.Model)
	}
}
