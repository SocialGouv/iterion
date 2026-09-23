package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// emptyToolsSource is the shape bots/evolve's gpt reviewer has: a claw judge
// that declares NO tools because its model sits near a forfait context window
// and one file read overflows it. It is exercised from SOURCE — parser,
// compiler, extractBackendFields, buildTask — because every layer between the
// two had its own way of erasing the declaration.
const emptyToolsSource = `prompt sys:
  """s"""

prompt usr:
  """u"""

judge review_gpt:
  model: "openai/gpt-5.5"
  backend: "claw"
  system: sys
  user: usr
  tools: []

judge review_claude:
  model: "anthropic/claude-opus-5"
  backend: "claw"
  system: sys
  user: usr

workflow w:
  entry: review_gpt
  review_gpt -> review_claude
  review_claude -> done
`

func buildJudgeTask(t *testing.T, wf *ir.Workflow, id string) delegate.Task {
	t.Helper()
	n, ok := wf.Nodes[id].(*ir.JudgeNode)
	if !ok {
		t.Fatalf("node %q is not a judge", id)
	}
	f, err := extractBackendFields(n)
	if err != nil {
		t.Fatalf("extractBackendFields: %v", err)
	}
	e := NewClawExecutor(NewRegistry(), wf, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()))
	task, err := e.buildTask(context.Background(), n, f, map[string]any{}, delegate.BackendClaw, &nodeBuildSession{})
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	return task
}

// The whole chain, asserted on the field the backends read: a node that
// declared `tools: []` reaches the backend with the declaration intact and
// literally zero tools, while its tool-less neighbour reaches it undeclared.
//
// Reddens on the mutation that drops ToolsDeclared from the task — which is
// what every CLI backend reads to decide whether the list bounds it.
func TestADeclaredEmptyToolListReachesTheBackendAsZeroToolsAndStaysDeclared(t *testing.T) {
	pr := parser.Parse("b.bot", emptyToolsSource)
	wf := ir.Compile(pr.File).Workflow

	declared := buildJudgeTask(t, wf, "review_gpt")
	if !declared.ToolsDeclared {
		t.Error("the declaration was lost between the source and the task")
	}
	if len(declared.AllowedTools) != 0 {
		t.Errorf("a node that declared no tools carries %v", declared.AllowedTools)
	}
	if declared.HasTools {
		t.Error("a node with no tools must not run a tool loop")
	}
	if len(declared.ToolDefs) != 0 {
		t.Errorf("a node with no tools resolved %d tool definitions", len(declared.ToolDefs))
	}

	undeclared := buildJudgeTask(t, wf, "review_claude")
	if undeclared.ToolsDeclared {
		t.Error("a node with no tools: line reached the backend as a declaration")
	}
}
