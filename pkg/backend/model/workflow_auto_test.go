package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestWorkflowAutoContinuesBackendChain(t *testing.T) {
	for _, spelling := range []string{"auto", " auto ", "${WORKFLOW_AUTO}", "${WORKFLOW_UNSET:-auto}", "{{vars.b}}", "{{vars.nested}}"} {
		for _, kind := range []string{"agent", "judge", "router"} {
			for _, target := range []string{"environment", "detected", "last-resort", "program-only"} {
				t.Run(spelling+"/"+kind+"/"+target, func(t *testing.T) {
					resetEnvForResolve(t)
					t.Setenv("WORKFLOW_AUTO", "auto")
					t.Setenv("WORKFLOW_UNSET", "")
					e := newExecutorForResolveTest(spelling)
					e.vars = map[string]any{"b": "auto", "nested": "${WORKFLOW_AUTO}"}
					e.logger = iterlog.Nop()
					want := delegate.BackendClaw
					switch target {
					case "environment":
						t.Setenv("ITERION_DEFAULT_BACKEND", "codex")
						want = "codex"
					case "detected":
						dir := t.TempDir()
						t.Setenv("CLAUDE_CONFIG_DIR", dir)
						t.Setenv("ITERION_BACKEND_PREFERENCE", "claude_code,claw")
						if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("{}"), 0o600); err != nil {
							t.Fatal(err)
						}
						fakeExecOnPath(t, "claude")
						want = "claude_code"
					case "program-only":
						e.staticProgramOnly = true
						t.Setenv("ITERION_DEFAULT_BACKEND", "codex")
						want = ""
					}
					var node ir.Node
					switch kind {
					case "agent":
						node = &ir.AgentNode{}
					case "judge":
						node = &ir.JudgeNode{}
					case "router":
						node = &ir.RouterNode{RouterMode: ir.RouterLLM}
					}
					if got := e.resolveBackendName(node); got != want {
						t.Fatalf("workflow default %q resolves to %q, want %q", spelling, got, want)
					}
					if got := e.resolveBackendName(nodeWithBackend("pi")); got != "pi" {
						t.Fatalf("explicit node backend lost to workflow default: %q", got)
					}
				})
			}
		}
	}
}

// Transport compilation cannot certify a future execution target. With no
// backend available there, the real executor still refuses before any call.
func TestUnknownCompileTargetStillFailsAtExecution(t *testing.T) {
	resetEnvForResolve(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	for _, kind := range []string{"agent", "judge", "router"} {
		t.Run(kind, func(t *testing.T) {
			fields := "  output: s\n"
			edges := "  a -> done\n"
			input := map[string]any{}
			if kind == "router" {
				fields = "  mode: llm\n"
				edges += "  a -> fail\n"
				input["_route_candidates"] = []string{"done", "fail"}
			}
			p := parsertest.Parse("target.bot", "schema s:\n  ok: bool\n\n"+kind+" a:\n"+fields+"\nworkflow w:\n  entry: a\n"+edges)
			if len(p.Diagnostics) != 0 {
				t.Fatal(p.Diagnostics)
			}
			c := ir.Compile(p.File)
			if c.HasErrors() {
				t.Fatalf("unknown target must compile for transport: %v", c.Diagnostics)
			}
			ex := newTestClawExecutor(NewRegistry(), c.Workflow, WithDefaultBackend(""), WithWorkDir(t.TempDir()), WithLogger(iterlog.Nop()), WithRetryPolicy(RetryPolicy{MaxAttempts: 1}))
			_, err := ex.Execute(context.Background(), c.Workflow.Nodes["a"], input)
			want := `invalid spec ""`
			if kind == "router" {
				want = "has no anthropic credential to serve it"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("empty target must refuse its missing model at execution, got %v", err)
			}
		})
	}
}

func TestWorkflowDefaultKeepsUnknownBackendNames(t *testing.T) {
	resetEnvForResolve(t)
	t.Setenv("ITERION_DEFAULT_BACKEND", "codex")
	for _, name := range []string{"Auto", "AUTO", "future_backend"} {
		if got := newExecutorForResolveTest(name).resolveBackendName(nodeWithBackend("")); got != name {
			t.Errorf("explicit default %q silently replaced with %q", name, got)
		}
	}
}

func TestPreflightAutoRemainsPessimistic(t *testing.T) {
	t.Setenv("WORKFLOW_AUTO", "auto")
	for _, raw := range []string{"auto", "${WORKFLOW_AUTO}", "{{vars.b}}"} {
		for _, dflt := range []string{"claw", "claude_code", "codex"} {
			if got, _ := resolveRouteBackend("", raw, dflt); got != "" {
				t.Errorf("preflight narrowed %q under %q to %q", raw, dflt, got)
			}
		}
	}
}

func TestWorkflowAutoDispatchesResolvedBackend(t *testing.T) {
	for _, raw := range []string{"auto", " auto ", "${ATTACK_AUTO}", "${ATTACK_UNSET:-auto}", "{{vars.b}}", "{{vars.nested}}"} {
		for _, kind := range []string{"agent", "judge", "router"} {
			t.Run(raw+"/"+kind, func(t *testing.T) {
				resetEnvForResolve(t)
				t.Setenv("ATTACK_AUTO", "auto")
				t.Setenv("ATTACK_UNSET", "")
				t.Setenv("ITERION_DEFAULT_BACKEND", "test_backend")
				b := &capturingBackend{results: []delegate.Result{{Output: map[string]any{"answer": "ok"}, BackendName: "test_backend"}}}
				reg := delegate.NewRegistry()
				reg.Register("test_backend", b)
				wf := &ir.Workflow{DefaultBackend: raw, Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{"s": {Name: "s", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}}}
				e := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(reg), WithWorkDir(t.TempDir()), WithLogger(iterlog.Nop()), WithRetryPolicy(RetryPolicy{MaxAttempts: 1}))
				e.vars = map[string]any{"b": "auto", "nested": "${ATTACK_AUTO}"}
				base := ir.BaseNode{ID: "a"}
				fields := ir.LLMFields{Model: "test-model"}
				schema := ir.SchemaFields{OutputSchema: "s"}
				var n ir.Node
				input := map[string]any{}
				switch kind {
				case "agent":
					n = &ir.AgentNode{BaseNode: base, LLMFields: fields, SchemaFields: schema}
				case "judge":
					n = &ir.JudgeNode{BaseNode: base, LLMFields: fields, SchemaFields: schema}
				case "router":
					n = &ir.RouterNode{BaseNode: base, LLMFields: fields, RouterMode: ir.RouterLLM}
					input["_route_candidates"] = []string{"done"}
					b.results[0].Output = map[string]any{"selected_route": "done", "reasoning": "synthetic"}
				}
				_, err := e.Execute(context.Background(), n, input)
				if err != nil || len(b.tasks) != 1 {
					t.Fatalf("real dispatch err=%v calls=%d", err, len(b.tasks))
				}
			})
		}
	}
}
