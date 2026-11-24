package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The compiler says "claw serves this node no tool" from the IR alone
// (ir.ClawToolSurfaceEmpty for an agent or judge, ir.ClawRecoveryAgentSurfaceEmpty
// for a tool node's rung-4 agent — what C351 and C352 warn on). The request
// claw sends is where that is true or false, so each declaration below is
// compiled from `.bot` text, dispatched through the executor's own entry
// point on the claw backend, and the tools of the FIRST model request are
// counted. A declaration the two answer differently is a warning that lies,
// or a tool-less dispatch that compiles clean.

// structuredOutputCarrier is the synthetic tool the structured path declares
// to receive the node's answer (GenerateObjectDirect's default SchemaName).
// It is the engine's carrier, never a tool the model can act with, so it is
// not counted as surface.
const structuredOutputCarrier = "structured_output"

// clawSurfaceWorkflow compiles one `.bot` source, failing the test on a
// parse or compile error.
func clawSurfaceWorkflow(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parsertest.Parse("b.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("fixture does not parse: %s", d.Error())
	}
	res := ir.Compile(pr.File)
	for _, d := range res.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	return res.Workflow
}

// clawSurfaceExecutor builds a claw executor whose every model request lands
// on mock. Nothing it reads or writes is the process's: the memory spaces are
// in-memory stores and the state roots are the test's own directories, so the
// dispatches run beside any other test. The ROUTE is left to the workflow —
// a fixture names claw on its node or as its `default_backend:`, which is the
// reading the compiler takes too.
func clawSurfaceExecutor(t *testing.T, wf *ir.Workflow, mock *mockAPIClient) *ClawExecutor {
	t.Helper()
	// Every name the runtime may put on a node's list resolves, so a dispatch
	// fails for what it serves and never for the registry.
	tr := tool.NewRegistry()
	for _, name := range []string{"bash", "read_file", "write_file", "glob", "todo_write", askUserToolName, delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName} {
		if err := tr.RegisterBuiltin(name, name, json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	e := NewClawExecutor(reg, wf,
		WithLogger(iterlog.Nop()),
		WithWorkDir(t.TempDir()),
		WithStoreDir(t.TempDir()),
		WithToolRegistry(tr),
		WithAutoMemoryStore(&recordingMemoryStore{}))
	e.backendRegistry.Register(delegate.BackendClaw, NewClawBackend(reg, e.hooks, e.retry, WithMemoryStore(&recordingMemoryStore{})))
	return e
}

// servedTools names the tools of the first model request a dispatch sent,
// the structured-output carrier aside. A dispatch that sent no request is
// read as serving none only when claw refused it by type — anything else is
// a broken scenario, not an answer.
func servedTools(t *testing.T, mock *mockAPIClient, dispatchErr error) []string {
	t.Helper()
	calls := mock.getCalls()
	if len(calls) == 0 {
		var refused *delegate.ErrCapabilityUnsupported
		if !errors.As(dispatchErr, &refused) {
			t.Fatalf("the dispatch sent no model request and was not refused by type: %v", dispatchErr)
		}
		return nil
	}
	var names []string
	for _, tl := range calls[0].Tools {
		if tl.Name != structuredOutputCarrier {
			names = append(names, tl.Name)
		}
	}
	return names
}

// clawSurfaceBot is one claw agent whose body and workflow lines the case
// supplies; withOutput gives it an output schema.
func clawSurfaceBot(node, workflow string, withOutput bool) string {
	output := ""
	if withOutput {
		output = "  output: out\n"
	}
	return "schema out:\n  summary: string\n\nprompt sys:\n  \"\"\"s\"\"\"\n\nprompt usr:\n  \"\"\"u\"\"\"\n\n" +
		"agent a:\n  model: \"anthropic/claude-sonnet-4-6\"\n  backend: \"claw\"\n  system: sys\n  user: usr\n" + output + node +
		"\nworkflow w:\n  entry: a\n" + workflow + "  a -> done\n"
}

// TestClawToolSurfaceEmptyAgreesWithTheExecutor holds ir.ClawToolSurfaceEmpty
// to the request Execute sends for the node, every declaration taken WITHOUT
// and WITH an output schema: the schema is what moves a node onto the
// structured path, whose request carries the answer's carrier and nothing
// the generation options hold — the memory store's tools included.
func TestClawToolSurfaceEmptyAgreesWithTheExecutor(t *testing.T) {
	t.Parallel()
	const memory = "  memory:\n    enabled: true\n    scope: \"s\"\n"
	for _, tc := range []struct {
		name, node, workflow string
		// empty is the surface a node with no output schema is served;
		// emptyWithOutput the same node declaring one.
		empty, emptyWithOutput bool
	}{
		{"no declaration", "", "", true, true},
		{"tools: [] declared", "  tools: []\n", "", true, true},
		{"capabilities alone", "  capabilities: [board.read]\n", "", true, true},
		{"auto_memory alone", "  auto_memory: on\n", "", true, true},
		{"memory block installing nothing", memory + "    read: false\n    write: false\n", "", true, true},
		{"memory write alone on the global space", memory + "    read: false\n    visibility: \"global\"\n", "", true, true},
		{"codemode off on the node, on at the workflow", "  codemode: off\n", "  codemode: on\n", true, true},

		{"a tools: entry", "  tools: [read_file]\n", "", false, false},
		{"interaction: human", "  interaction: human\n", "", false, false},
		{"interaction: async", "  interaction: async\n", "", false, false},
		{"codemode on the node", "  codemode: on\n", "", false, false},
		{"codemode on the workflow", "", "  codemode: on\n", false, false},
		{"memory block beside codemode", memory + "  codemode: on\n", "", false, false},

		// The store's tools reach the model on the text path alone.
		{"memory block", memory, "", false, true},
		{"memory read tools alone", memory + "    write: false\n", "", false, true},
		{"memory write tool alone", memory + "    read: false\n", "", false, true},
	} {
		for _, withOutput := range []bool{false, true} {
			want, name := tc.empty, tc.name+", no output schema"
			if withOutput {
				want, name = tc.emptyWithOutput, tc.name+", output schema"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				wf := clawSurfaceWorkflow(t, clawSurfaceBot(tc.node, tc.workflow, withOutput))
				node, ok := wf.Nodes["a"].(*ir.AgentNode)
				if !ok {
					t.Fatalf("node a is %T, want an agent", wf.Nodes["a"])
				}
				mock := newMockClient(textEvents("done", 10, 5), textEvents("done", 10, 5), textEvents("done", 10, 5))
				_, err := clawSurfaceExecutor(t, wf, mock).Execute(context.Background(), node, map[string]any{})
				served := servedTools(t, mock, err)

				if got := len(served) == 0; got != want {
					t.Fatalf("claw served %v, want an empty surface: %v — the scenario does not show what its row claims (err: %v)", served, want, err)
				}
				if got := ir.ClawToolSurfaceEmpty(node, wf); got != want {
					t.Errorf("ir.ClawToolSurfaceEmpty = %v while claw served %v: the compiler's reading and the wire disagree on this declaration", got, served)
				}
			})
		}
	}
}

// recoverySurfaceBot is one Verified Action tool node whose rung-4 agent the
// workflow's `default_backend:` routes to claw; recovery and workflow are the
// lines the case adds to the recovery block and to the workflow.
func recoverySurfaceBot(recovery, workflow string) string {
	return "tool deploy:\n  command: \"./deploy.sh\"\n  goal: \"the service is deployed\"\n  postcondition: \"./check.sh\"\n" +
		"  policy: recover\n  recovery:\n    max_agent_attempts: 1\n    model: \"anthropic/claude-sonnet-4-6\"\n" + recovery +
		"\nworkflow w:\n  entry: deploy\n  default_backend: \"claw\"\n" + workflow + "  deploy -> done\n"
}

// TestClawRecoveryAgentSurfaceEmptyAgreesWithTheExecutor holds
// ir.ClawRecoveryAgentSurfaceEmpty to the request rung 4 sends: the tool node
// is handed to agentRecovery, the rung's own entry point, which builds the
// synthetic agent and dispatches it like any node.
func TestClawRecoveryAgentSurfaceEmptyAgreesWithTheExecutor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, recovery, workflow string
		empty                    bool
	}{
		{"no agent_tools", "", "", true},
		{"agent_tools: [] declared", "    agent_tools: []\n", "", true},
		{"codemode off at the workflow", "", "  codemode: off\n", true},
		{"workflow capabilities", "", "  capabilities: [board.read]\n", true},

		{"agent_tools declared", "    agent_tools: [bash]\n", "", false},
		{"codemode on at the workflow", "", "  codemode: on\n", false},
		{"agent_tools beside codemode", "    agent_tools: [bash]\n", "  codemode: on\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wf := clawSurfaceWorkflow(t, recoverySurfaceBot(tc.recovery, tc.workflow))
			node, ok := wf.Nodes["deploy"].(*ir.ToolNode)
			if !ok {
				t.Fatalf("node deploy is %T, want a tool node", wf.Nodes["deploy"])
			}
			if !ir.ToolRecoveryAgentRung(node) {
				t.Fatal("the fixture does not reach rung 4, so no agent is dispatched and the row shows nothing")
			}
			mock := newMockClient(textEvents("done", 10, 5), textEvents("done", 10, 5), textEvents("done", 10, 5))
			err := clawSurfaceExecutor(t, wf, mock).agentRecovery(context.Background(), node, map[string]any{}, "recipe")
			served := servedTools(t, mock, err)

			if got := len(served) == 0; got != tc.empty {
				t.Fatalf("the recovery agent was served %v, want an empty surface: %v — the scenario does not show what its row claims (err: %v)", served, tc.empty, err)
			}
			if got := ir.ClawRecoveryAgentSurfaceEmpty(node, wf); got != tc.empty {
				t.Errorf("ir.ClawRecoveryAgentSurfaceEmpty = %v while the rung-4 agent was served %v: the compiler's reading and the wire disagree on this declaration", got, served)
			}
		})
	}
}
