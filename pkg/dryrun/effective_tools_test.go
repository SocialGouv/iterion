package dryrun

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The engine's parallel-branch guard asks its executor which tools a node will
// actually HOLD, because the runtime folds its own opt-ins over the author's
// `tools:` list at build time. An executor that answers short leaves the guard
// reading the declaration — and a dry run is an executor.
//
// Measured before this seam was wired, on #1652's own headline example: a claw
// fan-out whose branches declare `tools: [read_file]` with `auto_memory: on`
// was called `clean` by `iterion validate --exec --strict` and refused at the
// router by `iterion run`. Two products of one tree disagreeing about one file.
//
// The answer is the production executor's, not a copy: a second implementation
// of the append rules is the thing #1652 exists to remove.
func TestTheDryRunAnswersTheToolSurfaceSeamSoItAgreesWithARun(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode:   ir.BaseNode{ID: "reviewer"},
		LLMFields:  ir.LLMFields{Backend: "claw"},
		Tools:      []string{"read_file"},
		AutoMemory: "on",
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"reviewer": node}}
	x := &Executor{wf: wf}

	got := x.EffectiveToolNames(node, false)
	if !slices.Contains(got, "write_file") {
		t.Fatalf("a dry run reports %v for a node that `auto_memory: on` hands a file writer — the guard then reads the declaration and calls a fan-out clean that a run refuses", got)
	}
	// …and it must not invent a widening either: the same node without the
	// opt-in holds no writer, so `validate --exec` cannot refuse a fan-out a
	// run would admit.
	plain := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "plain"},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Tools:     []string{"read_file"},
	}
	if got := x.EffectiveToolNames(plain, false); slices.Contains(got, "write_file") {
		t.Fatalf("a dry run reports a file writer on a node that was granted none: %v", got)
	}
}
