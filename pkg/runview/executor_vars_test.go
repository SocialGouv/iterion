package runview

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The executor seeding takes the same contract resolveVars runs the run by:
// a launch value naming no declared var never enters the run's vars — the
// launch surfaces refuse unknown keys (UnknownInputNames), and the explicit
// --allow-unknown-inputs opt-out carries them as run INPUTS for subbot
// {{input.*}} forwarding, not as vars. Seeding them anyway let dispatch
// resolve a {{vars.zz}} the screen — reading declared vars only — refused to
// take an opinion on.
func TestBuildExecutor_UndeclaredLaunchVarDoesNotReachTheExecutorVars(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	undeclared := &ir.AgentNode{}
	undeclared.ID = "u"
	undeclared.Backend = "{{vars.zz}}"
	declared := &ir.AgentNode{}
	declared.ID = "d"
	declared.Backend = "{{vars.b}}"

	spec := ExecutorSpec{
		Ctx:   context.Background(),
		Store: st,
		RunID: "run-vars-seeding",
		Workflow: &ir.Workflow{
			Name:  "canary",
			Nodes: map[string]ir.Node{"u": undeclared, "d": declared},
			Vars:  map[string]*ir.Var{"b": {Name: "b", Type: ir.VarString, HasDefault: true, Default: "claw"}},
		},
		Vars: map[string]string{"zz": "claw", "b": "kimi"},
	}
	executor, err := BuildExecutor(spec)
	if err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	// The undeclared key must not resolve: the field reaches the registry as
	// written, the documented fate of a var nothing declared.
	if got := executor.EffectiveBackendName(undeclared); got != "{{vars.zz}}" {
		t.Errorf("EffectiveBackendName(undeclared) = %q, want the field as written — the launch value leaked into the executor's vars", got)
	}
	// The declared override resolves, or the filter drops too much.
	if got := executor.EffectiveBackendName(declared); got != "kimi" {
		t.Errorf("EffectiveBackendName(declared) = %q, want kimi — the declared launch value must reach dispatch", got)
	}
}
