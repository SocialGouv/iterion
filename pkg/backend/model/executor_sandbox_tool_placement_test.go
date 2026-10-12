package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runops"
)

// placementRegistry registers inert built-ins under the given names. Nothing
// here executes: the tests are about which names reach a sandboxed node,
// never about what the tools do.
func placementRegistry(t *testing.T, names ...string) *tool.Registry {
	t.Helper()
	tr := tool.NewRegistry()
	for _, name := range names {
		if err := tr.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) {
			return "", nil
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	return tr
}

func toolDefNames(defs []delegate.ToolDef) []string {
	names := make([]string, 0, len(defs))
	for _, td := range defs {
		names = append(names, td.Name)
	}
	return names
}

// A tool with no in-container form (a language server the launcher spawns, a
// worker keyed on a model-supplied directory, …) would execute on the host if
// a sandboxed node were allowed to start with it. The claw backend refuses it
// before the runner starts, through the public Execute path.
//
// Reddens on the mutation that deletes the refuseToolsWithNoSandboxPlacement
// call from ClawBackend.Execute.
func TestClawExecute_RefusesAToolWithNoSandboxPlacement(t *testing.T) {
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	task := delegate.Task{
		NodeID:  "orchestrator",
		Model:   "openai/gpt-5.6-sol",
		Sandbox: fakeSandboxRun{},
		ToolDefs: []delegate.ToolDef{
			{Name: "read_file"},
			{Name: "lsp"},
		},
	}

	_, err := b.Execute(context.Background(), task)
	if err == nil {
		t.Fatal("a sandboxed claw task carrying `lsp` was executed — the tool would have run on the host")
	}
	for _, want := range []string{
		`node "orchestrator"`,
		`tool "lsp"`,
		"sandboxed runner cannot execute in-container",
		"run the workflow unsandboxed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// The control that keeps the placement refusal inside the sandbox branch: an
// unsandboxed node never meets the table. Its tools execute in the launcher
// process itself — where the host-coupled halves of lsp, worker_* and friends
// live anyway — so `lsp` and `repl` must reach the runner-less path untouched
// and fail later on whatever the launcher genuinely lacks (here: the empty
// registry resolves no model client), never on placement.
//
// Reddens on the mutation that hoists refuseToolsWithNoSandboxPlacement out
// of the `task.Sandbox != nil` branch.
func TestClawExecute_UnsandboxedNodeIsNeverPlacementRefused(t *testing.T) {
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	task := delegate.Task{
		NodeID:   "orchestrator",
		Model:    "openai/gpt-5.6-sol",
		ToolDefs: []delegate.ToolDef{{Name: "read_file"}, {Name: "lsp"}, {Name: "repl"}},
	}

	_, err := b.Execute(context.Background(), task)
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "sandboxed runner cannot execute in-container") {
		t.Errorf("an unsandboxed node was refused on sandbox placement: %v", err)
	}
}

// A tool no side can serve that the permission policy denies outright could
// never have been called by the node: it is dropped rather than refused, the
// way a tool_policy-denied tool is withheld at resolution.
//
// Reddens on the mutation that drops the filter from ClawBackend.Execute.
func TestClawExecute_DropsAnUnplaceableToolThePolicyDenies(t *testing.T) {
	pol, err := permission.NewPolicy(permission.ModeDeny, []string{"read_file"}, nil, []string{"lsp"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	task := delegate.Task{
		NodeID:     "reader",
		Model:      "openai/gpt-5.6-sol",
		Sandbox:    fakeSandboxRun{},
		Permission: pol,
		ToolDefs:   []delegate.ToolDef{{Name: "read_file"}, {Name: "lsp"}},
	}
	if _, err := b.Execute(context.Background(), task); err != nil && strings.Contains(err.Error(), `tool "lsp"`) {
		t.Errorf("the node was refused for lsp, which its permission policy denies outright: %v", err)
	}
}

// The refusal lives in Execute, not in buildTask: a build failure of the
// primary aborts the node before its `fallbacks:` are walked, while an
// Execute failure is one route's failure. buildTask must therefore hand the
// tool through untouched.
//
// Reddens on the mutation that reinstates a PlacementRefused refusal in
// buildTask.
func TestBuildTask_LeavesThePlacementRefusalToExecute(t *testing.T) {
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: placementRegistry(t, "read_file", "lsp", "todo_write"),
		sandbox:      fakeSandboxRun{},
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "orchestrator"}}
	f := backendFields{id: "orchestrator", model: "anthropic/claude-opus-5", tools: []string{"read_file", "lsp"}}

	task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("buildTask refused a sandboxed node at build time, so its fallbacks never get a turn: %v", err)
	}
	if names := toolDefNames(task.ToolDefs); !slices.Contains(names, "lsp") {
		t.Errorf("buildTask dropped `lsp`; the refusal belongs to Execute: %v", names)
	}
}

// A sandboxed node keeps everything a container CAN serve. The tools that
// reach a filesystem or start a process are exactly the ones the runner
// registers locally, so the sandbox must not cost the node its own work.
func TestBuildTaskKeepsSandboxExecutableToolsOnASandboxedNode(t *testing.T) {
	declared := []string{"bash", "read_file", "write_file", "diagnostic_shell", "repl", "notebook_edit", "agent"}
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: placementRegistry(t, append(append([]string{}, declared...), "todo_write")...),
		sandbox:      fakeSandboxRun{},
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}}
	f := backendFields{id: "worker", model: "anthropic/claude-opus-5", tools: declared}

	task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("buildTask refused a sandboxed node whose tools all run in-container: %v", err)
	}
	names := toolDefNames(task.ToolDefs)
	for _, want := range declared {
		if !slices.Contains(names, want) {
			t.Errorf("%q was dropped from a sandboxed node; the runner executes it in-container: %v", want, names)
		}
	}
	// Kept is not enough: the backend must also accept the task, or the node
	// is refused at Execute for a tool the container can serve.
	if err := refuseToolsWithNoSandboxPlacement(task); err != nil {
		t.Errorf("the backend refuses a sandboxed task whose tools all run in-container: %v", err)
	}
}

// fakeAsyncBinder wires an async question channel that accepts everything.
type fakeAsyncBinder struct{}

func (fakeAsyncBinder) BindAsyncAsk(context.Context, string, string) AsyncAskHook {
	return fakeAsyncHook{}
}

type fakeAsyncHook struct{}

func (fakeAsyncHook) Post(context.Context, delegate.AsyncQuestion) (string, error) { return "q1", nil }
func (fakeAsyncHook) Pending(context.Context) ([]delegate.PendingAsync, error)     { return nil, nil }
func (fakeAsyncHook) CollectAnswers(context.Context) (string, error)               { return "", nil }

// A sandboxed claw task wired to post async questions is refused BY TYPE —
// the launcher does not bind the async pair to the question channel on that
// path — instead of every async call failing mid-run.
//
// Reddens on the mutation that deletes the async arm of ClawBackend.Execute.
func TestClawExecute_RefusesAsyncInteractionInASandboxByType(t *testing.T) {
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	task := delegate.Task{
		NodeID:            "asker",
		Model:             "openai/gpt-5.6-sol",
		Sandbox:           fakeSandboxRun{},
		PostAsyncQuestion: func(delegate.AsyncQuestion) (string, error) { return "q1", nil },
	}
	_, err := b.Execute(context.Background(), task)
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("Execute = %v; want a typed ErrCapabilityUnsupported for async on a sandboxed claw task", err)
	}
}

// The refusal is one ROUTE's failure: with claw as the node's primary, an
// async-capable fallback serves the node — the promise the docs make. A
// refusal raised while building the primary would end the node before the
// chain is walked.
//
// Reddens on the mutation that moves the refusal back into buildTask.
func TestSandboxedClawAsyncPrimaryFallsThroughToAnAsyncCapableRoute(t *testing.T) {
	fallback := &asyncCapableRecorder{}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}))
	reg.Register("claude_code", fallback)
	exec := newFallbackExecutor(reg, EventHooks{})
	exec.sandbox = fakeSandboxRun{}
	exec.asyncAsk = fakeAsyncBinder{}
	exec.toolRegistry = placementRegistry(t, "ask_user", delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName, "todo_write")

	node := fallbackAgentNodeModel("asker", delegate.BackendClaw, "", "openai/gpt-5.6-sol")
	node.Interaction = ir.InteractionAsync
	node.Fallbacks = []ir.Fallback{{Name: "cc", Backend: "claude_code"}}
	if _, err := exec.Execute(context.Background(), node, nil); err != nil {
		t.Fatalf("Execute = %v; want the async-capable fallback to serve the node", err)
	}
	if len(fallback.calls) != 1 {
		t.Errorf("fallback calls = %d, want 1", len(fallback.calls))
	}
}

// Every tool a claw node receives WITHOUT its author typing the name — the
// interaction pair, the board and runs tools a capability unlocks, ultracode's
// orchestration grant, the task list — must be servable under a sandbox. Were
// one refused, every sandboxed node carrying that grant would be refused with
// it, for a tool nobody asked for.
//
// Reddens on the mutation that classifies `agent` (or any implicit grant)
// PlacementRefused.
func TestImplicitClawGrantsAreServableUnderASandbox(t *testing.T) {
	e := &ClawExecutor{logger: iterlog.Nop(), sandbox: fakeSandboxRun{}}
	f := backendFields{id: "n", tools: []string{"read_file"}, interaction: ir.InteractionAsync}
	caps := []string{
		boardops.CapBoardRead, boardops.CapBoardCreate, boardops.CapBoardMove, boardops.CapBoardAssign,
		boardops.CapBoardLabel, boardops.CapBoardClose, boardops.CapBoardComment, runops.CapRunsRead,
	}

	got := e.assembleEffectiveTools(f, delegate.BackendClaw, caps, true)
	for _, want := range []string{"agent", "todo_write", "ask_user", delegate.AskUserAsyncToolName} {
		if !slices.Contains(got, want) {
			t.Fatalf("assembleEffectiveTools no longer grants %q, so this test lost its subject: %v", want, got)
		}
	}
	for _, name := range got {
		if name == "read_file" {
			continue
		}
		if placement, reason := tool.SandboxPlacementOf(name); placement == tool.PlacementRefused {
			t.Errorf("implicit grant %q is refused under a sandbox (%s): every sandboxed node carrying it would fail", name, reason)
		}
	}
}

// Ultracode's implicit `agent` grant holds under a sandbox as it does
// without one: the runner registers `agent` in the container, so there is
// nothing to withhold — and nothing to warn about, including from the
// admission query that probes every node with ultracode on.
func TestAssembleEffectiveTools_UltracodeGrantsAgentUnderASandboxToo(t *testing.T) {
	f := backendFields{id: "n", tools: []string{"bash"}}
	for _, e := range []*ClawExecutor{
		{logger: iterlog.Nop()},
		{logger: iterlog.Nop(), sandbox: fakeSandboxRun{}},
	} {
		if got := e.assembleEffectiveTools(f, delegate.BackendClaw, nil, true); !slices.Contains(got, "agent") {
			t.Errorf("ultracode did not grant `agent` (sandboxed=%v): %v", e.sandbox != nil, got)
		}
	}
}
