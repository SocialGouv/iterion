package cli

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestBuildRunInputs_RefusesAnUnknownVar pins the CLI half of #1757: a
// --var that names no var of the workflow is refused, naming the key and
// the declared set — the same refusal the cloud chokepoint makes, from
// the same helper. The run would otherwise execute on defaults while the
// operator believes it was parameterised.
func TestBuildRunInputs_RefusesAnUnknownVar(t *testing.T) {
	wf := &ir.Workflow{Vars: map[string]*ir.Var{
		"engine": {Name: "engine", Type: ir.VarString},
	}}

	inputs, err := buildRunInputs(wf, "", map[string]string{"engin": "pg"}, false)
	if err == nil {
		t.Fatal("buildRunInputs accepted an unknown var, want refusal")
	}
	if !strings.Contains(err.Error(), "engin") || !strings.Contains(err.Error(), "engine") {
		t.Fatalf("error = %v, want it to name the unknown key AND the declared var", err)
	}
	if inputs != nil {
		t.Fatalf("inputs = %v on a refusal, want nil", inputs)
	}

	// A workflow with no vars at all refuses any input and says so.
	_, err = buildRunInputs(&ir.Workflow{}, "", map[string]string{"engine": "pg"}, false)
	if err == nil || !strings.Contains(err.Error(), "none") {
		t.Fatalf("error = %v, want the declared set to read as none", err)
	}

	// A declared var passes.
	if _, err := buildRunInputs(wf, "", map[string]string{"engine": "pg"}, false); err != nil {
		t.Fatalf("declared var refused: %v", err)
	}

	// The opt-out rides the unknown key instead of refusing: the
	// forwarding channel an undeclared payload key rides to a subbot
	// ({{input.extra}} in a node's `with:`, C149) is its legitimate user.
	inputs, err = buildRunInputs(wf, "", map[string]string{"engin": "pg"}, true)
	if err != nil {
		t.Fatalf("opted-out unknown var refused: %v", err)
	}
	if inputs["engin"] != "pg" {
		t.Fatalf("inputs = %v, want the unknown key riding", inputs)
	}
}
