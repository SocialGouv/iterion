package cli

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/store"
)

const resumeLaunchVarsSource = `dsl: 2

vars:
  cfg: json
  labels: string[]

schema out:
  ok: bool

tool noop:
  command: "true"
  output: out

workflow resumed:
  entry: noop
  noop -> done
`

// A resumed run's stored inputs are its launch TEXT, and the resumed
// executor is seeded with them read at each var's declared type — the
// launch seed's reading — with the names that are no var of the workflow
// left out, as a launch leaves them out. A raw seed stores the text as text
// (a `json` var's dotted field finds nothing) and merges every stored input
// into the vars, undeclared ones included.
//
// Mutation seen red (restored): the resume seed put back to
// exec.SetVars(r.Inputs) — the dotted field resolved to nothing and the
// undeclared input resolved.
func TestAResumedExecutorReadsTheStoredInputsAtTheirDeclaredType(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	cr := ir.Compile(parsertest.Parse("resumed.bot", resumeLaunchVarsSource).File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %v", d)
		}
	}
	storeDir := t.TempDir()
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	run := &store.Run{ID: "run-resumed", Inputs: map[string]any{
		"cfg":    `{"backend":"claw"}`,
		"labels": "a11y,rgaa",
		"zz":     "undeclared",
	}}
	ne, err := buildResumeExecutor(ResumeOptions{RunID: run.ID}, cr.Workflow, st, storeDir, nil, run, nil)
	if err != nil {
		t.Fatalf("buildResumeExecutor: %v", err)
	}
	e, ok := ne.(*model.ClawExecutor)
	if !ok {
		t.Fatalf("buildResumeExecutor returned %T, want *model.ClawExecutor", ne)
	}
	if got := e.ResolveRoutingField("{{vars.cfg.backend}}"); got != "claw" {
		t.Errorf("{{vars.cfg.backend}} resolved to %q, want %q — the stored json text was seeded as text", got, "claw")
	}
	if got := e.ResolveRoutingField("{{vars.zz}}"); got == "undeclared" {
		t.Error("a stored input naming no var of the workflow entered the resumed executor's vars")
	}
}
