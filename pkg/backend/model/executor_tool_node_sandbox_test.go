package model

import (
	"bytes"
	"context"
	"encoding/json"
	osexec "os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// unmetSandboxRun is fakeSandboxRun whose every command exits 1.
type unmetSandboxRun struct{ fakeSandboxRun }

func (unmetSandboxRun) Command(ctx context.Context, _ []string, _ sandbox.ExecOpts) *osexec.Cmd {
	return osexec.CommandContext(ctx, "false")
}

// recordingRegistry registers built-ins whose closures record that they ran
// in THIS process — which, for a sandboxed run, is the host.
func recordingRegistry(t *testing.T, names ...string) (*tool.Registry, map[string]bool) {
	t.Helper()
	ran := make(map[string]bool, len(names))
	tr := tool.NewRegistry()
	for _, name := range names {
		if err := tr.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) {
			ran[name] = true
			return `{"ran":"host"}`, nil
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	return tr, ran
}

// A tool node naming a registry tool (`command: bash`) runs that tool's
// closure in the launcher process. Under a sandbox that is the host, so a
// tool that belongs in the container — or that no side may serve — is
// refused and its closure never runs; the model's output reaching such a
// node's input must not become host execution.
//
// Reddens on the mutation that deletes the placement check in
// executeToolNodeRecipe.
func TestExecuteToolNodeRecipe_SandboxRefusesARegistryToolThatBelongsInTheContainer(t *testing.T) {
	reg, ran := recordingRegistry(t, "bash", "read_file", "web_fetch", "lsp")
	e := &ClawExecutor{logger: iterlog.Nop(), toolRegistry: reg, sandbox: fakeSandboxRun{}}

	for _, name := range []string{"bash", "read_file", "web_fetch", "lsp"} {
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "run_it"}, Command: name}
		_, err := e.executeToolNodeRecipe(context.Background(), node, map[string]any{"command": "id"})
		if err == nil {
			t.Errorf("tool node `command: %s` ran under a sandbox — its closure executes on the host", name)
			continue
		}
		if !strings.Contains(err.Error(), "outside the run's sandbox") {
			t.Errorf("tool node `command: %s`: error does not name the sandbox: %v", name, err)
		}
		if ran[name] {
			t.Errorf("tool node `command: %s`: the host closure ran", name)
		}
	}
}

// A Verified Action runs the same registry recipe as its rung 2. A refusal
// raised before the tool runs is a SETUP failure there too: the node fails
// with it, instead of the ladder reading it as a recipe run that
// `policy: best_effort` waives or a met postcondition excuses.
//
// Reddens on the mutation that folds the setup error back into runErr in
// runVerifiedRecipe.
func TestVerifiedAction_RegistryRecipeSandboxRefusalFailsTheNode(t *testing.T) {
	reg, ran := recordingRegistry(t, "bash")
	ex := vaExecutor(t, t.TempDir(), WithToolRegistry(reg))
	// Every sandboxed command exits 1: the postcondition is unmet before the
	// recipe (so rung 1 does not skip it) and after it.
	ex.SetSandbox(unmetSandboxRun{})

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "va_registry"},
		Command:       "bash",
		Postcondition: "test -f done",
		Policy:        ir.PolicyBestEffort,
	}
	_, err := ex.Execute(context.Background(), node, map[string]any{"command": "id"})
	if err == nil || !strings.Contains(err.Error(), "outside the run's sandbox") {
		t.Fatalf("Execute = %v; want the sandbox refusal as the node's failure", err)
	}
	if ran["bash"] {
		t.Error("the host closure ran")
	}
}

// Only the sandbox refusal is a setup failure. Any other failure of a
// registry recipe — here an unknown tool, which is what `command: "true"`
// resolves to (a bare word is a registry recipe) — stays a recipe run the
// ladder judges by its postcondition: under `best_effort` the node continues,
// and under `recover` the recovery rungs get their turn. The failure is still
// recorded in the log rather than dropped.
//
// Reddens on the mutation that turns every pre-run failure into a setup
// error, and on the one that drops the recipe-failure log line.
func TestVerifiedAction_OtherRegistryRecipeFailuresStayRecipeRuns(t *testing.T) {
	var logged bytes.Buffer
	ex := vaExecutor(t, t.TempDir(), WithToolRegistry(tool.NewRegistry()), WithLogger(iterlog.New(iterlog.LevelInfo, &logged)))

	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "va_unknown"},
		Command:       "true",
		Postcondition: "false",
		Policy:        ir.PolicyBestEffort,
	}
	out, err := ex.Execute(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("Execute = %v; an unknown registry recipe must stay a recipe failure the ladder can continue past", err)
	}
	if m := vaMeta(t, out); m["postcondition_met"] != false {
		t.Errorf("postcondition_met = %v, want false", m["postcondition_met"])
	}
	if !strings.Contains(logged.String(), "recipe failed:") || !strings.Contains(logged.String(), `"true"`) {
		t.Errorf("the recipe's failure was not recorded in the log:\n%s", logged.String())
	}
}

// The verdict is taken on the RESOLVED tool, not on the spelling the node
// wrote: an MCP tool reached by its bare shorthand (`command: create_issue`)
// is an MCP tool, served by the launcher, and keeps working under a sandbox.
//
// Reddens on the mutation that classifies the raw `command:` string.
func TestExecuteToolNodeRecipe_SandboxClassifiesTheResolvedTool(t *testing.T) {
	reg := tool.NewRegistry()
	ran := false
	if err := reg.RegisterMCP("github", "create_issue", "", nil, func(context.Context, json.RawMessage) (string, error) {
		ran = true
		return `{"ok":true}`, nil
	}); err != nil {
		t.Fatalf("RegisterMCP: %v", err)
	}
	e := &ClawExecutor{logger: iterlog.Nop(), toolRegistry: reg, sandbox: fakeSandboxRun{}}

	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "file_issue"}, Command: "create_issue"}
	if _, err := e.executeToolNodeRecipe(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("an MCP tool reached by its shorthand was refused under a sandbox: %v", err)
	}
	if !ran {
		t.Error("the MCP tool never ran")
	}
}

// The mirror: a launcher-placed registry tool keeps working in a sandboxed
// run, and without a sandbox every registry tool runs as before.
func TestExecuteToolNodeRecipe_LauncherToolsAndUnsandboxedRunsAreUnchanged(t *testing.T) {
	reg, ran := recordingRegistry(t, "todo_write", "bash")
	sandboxed := &ClawExecutor{logger: iterlog.Nop(), toolRegistry: reg, sandbox: fakeSandboxRun{}}
	if _, err := sandboxed.executeToolNodeRecipe(context.Background(),
		&ir.ToolNode{BaseNode: ir.BaseNode{ID: "todo"}, Command: "todo_write"}, map[string]any{}); err != nil {
		t.Errorf("a launcher-placed registry tool was refused under a sandbox: %v", err)
	}
	if !ran["todo_write"] {
		t.Error("todo_write's closure never ran")
	}

	host := &ClawExecutor{logger: iterlog.Nop(), toolRegistry: reg}
	if _, err := host.executeToolNodeRecipe(context.Background(),
		&ir.ToolNode{BaseNode: ir.BaseNode{ID: "run_it"}, Command: "bash"}, map[string]any{"command": "id"}); err != nil {
		t.Errorf("an unsandboxed run refused a registry tool node: %v", err)
	}
	if !ran["bash"] {
		t.Error("bash's closure never ran on an unsandboxed run")
	}
}
