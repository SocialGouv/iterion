package runtime

import (
	"context"
	"testing"
)

// A fail node's `message:` is rendered at fail time against the RUN's
// namespaces (failOutcome → resolveMapping → resolveRef, whose `input` arm
// reads sc.runInputs), so `{{input.X}}` there is the LAUNCH PAYLOAD's key —
// never an input built from the incoming edges. The compiler's C308 used to
// claim the opposite on that surface ("empty on every path", remedy: map the
// field on an incoming edge), which is why this reading is pinned where it
// lives: the diagnostics now say the run's inputs (C149), and a change here
// has to redden before an author is told something false again.
func TestAFailMessageReadsTheRunsInputsNotTheEdges(t *testing.T) {
	const unmapped = `dsl: 2
schema s:
  ok: bool

agent lead:
  model: "m"
  output: s

fail stopped:
  code: STOPPED
  message: "inventory: {{input.inv}}"

workflow w:
  worktree: none
  entry: lead
  lead -> stopped
`
	const mapped = `dsl: 2
schema s:
  ok: bool

agent lead:
  model: "m"
  output: s

fail stopped:
  code: STOPPED
  message: "inventory: {{input.inv}}"

workflow w:
  worktree: none
  entry: lead
  lead -> stopped with { inv: "FROM-THE-EDGE" }
`
	for _, tc := range []struct {
		name   string
		src    string
		inputs map[string]any
		want   string
	}{
		{"the launch payload is what the message reads", unmapped, map[string]any{"inv": "FROM-LAUNCH-PAYLOAD"}, "inventory: FROM-LAUNCH-PAYLOAD"},
		{"a launch that omits the key renders the empty string", unmapped, nil, "inventory: "},
		{"the incoming edge's `with` changes nothing", mapped, nil, "inventory: "},
		{"and the launch payload still wins over it", mapped, map[string]any{"inv": "FROM-LAUNCH-PAYLOAD"}, "inventory: FROM-LAUNCH-PAYLOAD"},
	} {
		cr := compileBot(t, tc.src)
		if cr.HasErrors() {
			t.Fatalf("%s: fixture does not compile: %v", tc.name, cr.Diagnostics)
		}
		s := tmpStore(t)
		eng := New(cr.Workflow, s, newStubExecutor())
		runID := "fail-msg-" + tc.name
		if err := eng.Run(context.Background(), runID, tc.inputs); err == nil {
			t.Fatalf("%s: the run succeeded; the graph routes to a fail node", tc.name)
		}
		run, err := s.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("%s: load run: %v", tc.name, err)
		}
		if run.Error != tc.want {
			t.Errorf("%s: the operator reads %q, want %q", tc.name, run.Error, tc.want)
		}
	}
}
