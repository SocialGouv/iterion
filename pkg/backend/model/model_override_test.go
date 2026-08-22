package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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

// A more specific flag must still beat a broader inherited rule, and a
// broader flag must NOT beat a more specific inherited one — merging changes
// insertion order, never the specificity ranking ForNode resolves on.
func TestMergeOver_PreservesSelectorSpecificity(t *testing.T) {
	var launched ModelOverrides
	launched.SetModel("chat", "anthropic/claude-opus-5")

	var flags ModelOverrides
	flags.SetModel("*", "openai/gpt-5.5")

	merged := flags.MergeOver(launched)
	if got := merged.ForNode("chat", ir.NodeAgent); got.Model != "anthropic/claude-opus-5" {
		t.Errorf("chat Model = %q, want the more specific inherited rule to hold", got.Model)
	}
	if got := merged.ForNode("other", ir.NodeAgent); got.Model != "openai/gpt-5.5" {
		t.Errorf("other Model = %q, want the broad flag", got.Model)
	}
}
