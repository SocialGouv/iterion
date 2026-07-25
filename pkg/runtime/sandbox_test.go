package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// WorkflowSandboxActive must reflect the RESOLVED sandbox decision, not the
// static wf.Sandbox declaration: ITERION_SANDBOX_OVERRIDE=none neutralizes a
// bot's inline sandbox block (the run executes in the runner pod), and
// callers like the cloud runner's file-secret materialization key off it.
func TestWorkflowSandboxActive(t *testing.T) {
	inline := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "x:y"}}
	cases := []struct {
		name     string
		wf       *ir.Workflow
		override string
		def      string
		want     bool
	}{
		{"inline block, no override", inline, "", "", true},
		{"inline block neutralized by override none", inline, "none", "", false},
		{"inline block wins over override auto", inline, "auto", "", true},
		// The engine itself is neutral: policy (sandbox-by-default) lives
		// at product entry points via ResolveGlobalSandboxDefault.
		{"no block, no override, no default", &ir.Workflow{}, "", "", false},
		{"no block, global default auto", &ir.Workflow{}, "", "auto", true},
		{"no block, global default none", &ir.Workflow{}, "", "none", false},
		{"nil workflow, override none", nil, "none", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WorkflowSandboxActive(c.wf, c.override, c.def); got != c.want {
				t.Errorf("WorkflowSandboxActive(override=%q, default=%q) = %v, want %v", c.override, c.def, got, c.want)
			}
		})
	}
}
