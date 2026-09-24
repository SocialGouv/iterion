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

// The backend seam, asked of a dry run, answers from the same program: the
// node's backend, else the workflow's, else "" — never the host's
// ITERION_DEFAULT_BACKEND nor its credential probe, which would give one file
// two verdicts on two machines.
func TestTheDryRunAnswersTheBackendSeamFromTheProgram(t *testing.T) {
	t.Setenv("ITERION_DEFAULT_BACKEND", "claude_code")
	named := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "named"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}
	unnamed := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "unnamed"}}
	withDefault := &Executor{wf: &ir.Workflow{DefaultBackend: "codex", Nodes: map[string]ir.Node{"named": named, "unnamed": unnamed}}}
	if got := withDefault.EffectiveBackendName(named); got != "claude_code" {
		t.Errorf("a node naming its backend: got %q, want claude_code", got)
	}
	if got := withDefault.EffectiveBackendName(unnamed); got != "codex" {
		t.Errorf("a node naming none, in a workflow whose default_backend is codex: got %q, want codex", got)
	}
	bare := &Executor{wf: &ir.Workflow{Nodes: map[string]ir.Node{"unnamed": unnamed}}}
	if got := bare.EffectiveBackendName(unnamed); got != "" {
		t.Errorf("a program naming no backend answered %q — the host's ITERION_DEFAULT_BACKEND leaked into a dry run", got)
	}
}

// A dry run resolves `{{vars.…}}` in a backend as the run does, from the vars
// the engine hands it — so `validate --exec` agrees with `run` on a node that
// inherits a templated default_backend — and follows the vars when they change.
func TestTheDryRunResolvesATemplatedBackendFromItsVars(t *testing.T) {
	auto := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "auto"}}
	x := &Executor{wf: &ir.Workflow{DefaultBackend: "{{vars.b}}", Nodes: map[string]ir.Node{"a": auto}}}
	x.SetVars(map[string]any{"b": "claw"})
	if got := x.EffectiveBackendName(auto); got != "claw" {
		t.Fatalf("an `auto` node under default_backend {{vars.b}} with b=claw answered %q, want claw", got)
	}
	x.SetVars(map[string]any{"b": "claude_code"})
	if got := x.EffectiveBackendName(auto); got != "claude_code" {
		t.Errorf("after the vars changed to b=claude_code the dry run still answered %q — it kept the vars it first saw", got)
	}
}

// Both seams a dry run answers read the same vars: a `{{vars.…}}` backend that
// resolves to claw grants claw's appends to the tool seam too. A split where
// only the backend followed the vars would read two writers as readers.
func TestBothSeamsOfADryRunReadTheSameVars(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode:   ir.BaseNode{ID: "a"},
		LLMFields:  ir.LLMFields{Backend: "{{vars.b}}"},
		Tools:      []string{"read_file"},
		AutoMemory: "on",
	}
	x := &Executor{wf: &ir.Workflow{Nodes: map[string]ir.Node{"a": node}}}
	x.SetVars(map[string]any{"b": "claw"})
	if got := x.EffectiveBackendName(node); got != "claw" {
		t.Fatalf("precondition: {{vars.b}} with b=claw answered %q, want claw", got)
	}
	if got := x.EffectiveToolNames(node, false); !slices.Contains(got, "write_file") {
		t.Errorf("the tool seam answered %v for a claw node with `auto_memory: on` — it did not read the vars the backend seam read", got)
	}
}
