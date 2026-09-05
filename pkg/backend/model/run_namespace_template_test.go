package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestRunNamespaceReachesPromptsAndToolCommands covers #738's class: the
// `run.*` namespace has three consumers — the expression evaluator, prompt
// bodies, and tool commands / scripts / postconditions. A member the
// runtime resolves but these do not renders as a literal `{{run.x}}` in a
// shell command, which is worse than an error because it is silent.
func TestRunNamespaceReachesPromptsAndToolCommands(t *testing.T) {
	td := &TemplateData{
		RunID: "run-abc",
		Run: map[string]any{
			"id":              "run-abc",
			"elapsed_seconds": 42.5,
			"max_cost_usd":    12.0,
		},
	}

	// Prompt bodies.
	if got, ok := lookupRunTemplateRef(td, "elapsed_seconds"); !ok || got != "42.5" {
		t.Errorf("prompt {{run.elapsed_seconds}} = %q ok=%v, want 42.5", got, ok)
	}
	if got, ok := lookupRunTemplateRef(td, "id"); !ok || got != "run-abc" {
		t.Errorf("prompt {{run.id}} = %q ok=%v, want run-abc", got, ok)
	}
	if _, ok := lookupRunTemplateRef(td, "no_such_member"); ok {
		t.Error("prompt {{run.no_such_member}} resolved; want unresolved")
	}

	// Tool commands.
	refs := []*ir.Ref{{Kind: ir.RefRun, Path: []string{"max_cost_usd"}, Raw: "{{run.max_cost_usd}}"}}
	cmd := resolveRunRefs("test $(echo {{run.max_cost_usd}}) -gt 0", "run-abc", td, refs, shellEscapeValue)
	if strings.Contains(cmd, "{{run.max_cost_usd}}") || !strings.Contains(cmd, "12") {
		t.Errorf("tool command run.max_cost_usd not substituted: %q", cmd)
	}
}
