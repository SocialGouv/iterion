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
	if _, err := ParseModelOverrides([]string{"reviewer_*="}, nil); err == nil {
		t.Error("expected error for empty value")
	}
	if _, err := ParseModelOverrides(nil, []string{"  "}); err == nil {
		t.Error("expected error for blank arg")
	}
	// bare value is valid (selector "*").
	o, err := ParseModelOverrides([]string{"anthropic/claude-opus-4-8"}, nil)
	if err != nil {
		t.Fatalf("bare value should parse: %v", err)
	}
	if got := o.ForNode("whatever", ir.NodeAgent); got.Model != "anthropic/claude-opus-4-8" {
		t.Errorf("bare value should target all nodes, got %q", got.Model)
	}
}
