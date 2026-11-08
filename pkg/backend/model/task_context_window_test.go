package model

import (
	"context"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// taskContextWindow answers from every table the dispatch seam itself
// trusts. These cases pin the precedence and the honest zero.
func TestTaskContextWindow(t *testing.T) {
	t.Run("the operator table answers for a bare gateway id", func(t *testing.T) {
		gatewayEnv(t, `{"deepseek-v4.1-flash": {"context_window": 262144}}`, "")
		seedSpecs(t, nil)
		if got := taskContextWindow(os.Getenv, "deepseek-v4.1-flash"); got != 262144 {
			t.Fatalf("taskContextWindow = %d, want 262144", got)
		}
	})

	t.Run("the host-forwarded record answers as the operator table does", func(t *testing.T) {
		gatewayEnv(t, "", "")
		seedSpecs(t, nil)
		t.Setenv(compatgw.ResolvedEnvName, `{"model":"deepseek-v4.1-flash","source":"operator","context_window":262144}`)
		if got := taskContextWindow(os.Getenv, "deepseek-v4.1-flash"); got != 262144 {
			t.Fatalf("taskContextWindow = %d, want 262144 from the forwarded record", got)
		}
	})

	t.Run("a prefixed spec resolves by its wire id", func(t *testing.T) {
		// The dispatch seam keys the forwarded record and the operator table
		// by the WIRE id; a prefixed spec must hit the same tables its bare
		// id would, or every gateway-routed task would stamp nothing.
		gatewayEnv(t, `{"deepseek-v4.1-flash": {"context_window": 262144}}`, "")
		seedSpecs(t, nil)
		if got := taskContextWindow(os.Getenv, "openai_compatible/deepseek-v4.1-flash"); got != 262144 {
			t.Fatalf("taskContextWindow(prefixed) = %d, want 262144 from the operator table", got)
		}
		gatewayEnv(t, "", "")
		t.Setenv(compatgw.ResolvedEnvName, `{"model":"deepseek-v4.1-flash","source":"operator","context_window":262144}`)
		if got := taskContextWindow(os.Getenv, "openai/deepseek-v4.1-flash"); got != 262144 {
			t.Fatalf("taskContextWindow(prefixed) = %d, want 262144 from the forwarded record", got)
		}
	})

	t.Run("the bare consensus index answers what no other table carried", func(t *testing.T) {
		gatewayEnv(t, "", "")
		seedSpecs(t, map[string]modelspecs.Spec{
			"llama-3.3-70b": {ContextWindow: 131072},
		})
		if got := taskContextWindow(os.Getenv, "llama-3.3-70b"); got != 131072 {
			t.Fatalf("taskContextWindow = %d, want 131072 from the bare index", got)
		}
	})

	t.Run("an id no table knows is zero, never a guess", func(t *testing.T) {
		gatewayEnv(t, "", "")
		seedSpecs(t, nil)
		if got := taskContextWindow(os.Getenv, "totally-unknown-model-9"); got != 0 {
			t.Fatalf("taskContextWindow = %d, want 0", got)
		}
		if got := taskContextWindow(os.Getenv, ""); got != 0 {
			t.Fatalf("taskContextWindow(empty) = %d, want 0", got)
		}
		if got := taskContextWindow(os.Getenv, "   "); got != 0 {
			t.Fatalf("taskContextWindow(blank) = %d, want 0", got)
		}
	})
}

// ctxWindowSource exercises buildTask from SOURCE — the wiring between the
// resolved model and the delegate task is exactly what a unit test of the
// lookup alone would miss.
const ctxWindowSource = `prompt sys:
  """s"""

prompt usr:
  """u"""

judge reviewer:
  model: "deepseek-v4.1-flash"
  backend: "claw"
  system: sys
  user: usr

judge unknowner:
  model: "totally-unknown-model-9"
  backend: "claw"
  system: sys
  user: usr

workflow w:
  entry: reviewer
  reviewer -> unknowner
  unknowner -> done
`

func TestBuildTaskStampsTheResolvedContextWindow(t *testing.T) {
	gatewayEnv(t, `{"deepseek-v4.1-flash": {"context_window": 262144}}`, "")
	seedSpecs(t, nil)
	wf := ir.Compile(parsertest.Parse("b.bot", ctxWindowSource).File).Workflow
	cases := map[string]int{
		"reviewer":  262144,
		"unknowner": 0,
	}
	for id, want := range cases {
		task := buildWindowJudgeTask(t, wf, id)
		if task.ContextWindow != want {
			t.Fatalf("node %q: Task.ContextWindow = %d, want %d", id, task.ContextWindow, want)
		}
	}
}

// buildWindowJudgeTask compiles one judge node end to end and builds its
// delegate task, mirroring the fixture in declared_empty_tools_test.go.
func buildWindowJudgeTask(t *testing.T, wf *ir.Workflow, id string) delegate.Task {
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
