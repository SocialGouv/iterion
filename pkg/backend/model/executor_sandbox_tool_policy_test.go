package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/permissions"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// allowingClassifier answers Allow for every call and never reads the input.
type allowingClassifier struct{}

func (allowingClassifier) Classify(context.Context, string, map[string]any) (permissions.Decision, error) {
	return permissions.DecisionAllow, nil
}

// A tool_policy allowlist is the operator's explicit deny. A sandboxed runner
// executes its container tools in-container, where the launcher's call-time
// guard never runs, so the verdict is taken at resolution and a denied
// container tool is not advertised at all. A launcher-side tool keeps the
// call-time guard, as on an unsandboxed node.
//
// Reddens on the mutation that advertises a denied container tool anyway.
func TestResolveToolsForNode_SandboxedNodeWithholdsContainerToolsThePolicyDenies(t *testing.T) {
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: placementRegistry(t, "read_file", "bash", "todo_write"),
		toolPolicy:   tool.NewPolicy("read_file"),
		sandbox:      fakeSandboxRun{},
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}}

	defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"read_file", "bash", "todo_write"})
	if err != nil {
		t.Fatalf("resolveToolsForNode: %v", err)
	}
	names := toolDefNames(defs)
	if slices.Contains(names, "bash") {
		t.Errorf("bash, which tool_policy denies, was advertised to a sandboxed node — the runner would execute it past the guard: %v", names)
	}
	if !slices.Contains(names, "read_file") {
		t.Errorf("read_file, which tool_policy allows, was withheld: %v", names)
	}
	i := slices.Index(names, "todo_write")
	if i < 0 {
		t.Fatalf("todo_write executes on the launcher, under its guard, yet was withheld: %v", names)
	}
	if _, err := defs[i].Execute(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, tool.ErrToolDenied) {
		t.Errorf("the launcher guard did not refuse todo_write's call: %v", err)
	}
}

// A tool neither side can serve (`lsp`) refuses a sandboxed node in Execute —
// unless tool_policy already denies it, in which case the node could never
// have called it: the tool is withheld instead, and the node runs as it would
// unsandboxed with that one call denied.
//
// Reddens on the mutation that restricts the pre-check to sandbox-placed
// tools.
func TestResolveToolsForNode_SandboxedNodeWithholdsAnUnplaceableToolThePolicyDenies(t *testing.T) {
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: placementRegistry(t, "read_file", "lsp"),
		toolPolicy:   tool.NewPolicy("read_file"),
		sandbox:      fakeSandboxRun{},
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}}

	defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"read_file", "lsp"})
	if err != nil {
		t.Fatalf("resolveToolsForNode: %v", err)
	}
	if names := toolDefNames(defs); slices.Contains(names, "lsp") {
		t.Errorf("lsp, which tool_policy denies, was advertised — the backend then refuses the whole sandboxed node for it: %v", names)
	}
}

// countingDenyingClassifier denies every call and counts how often it is asked.
type countingDenyingClassifier struct{ calls int }

func (c *countingDenyingClassifier) Classify(context.Context, string, map[string]any) (permissions.Decision, error) {
	c.calls++
	return permissions.DecisionDeny, nil
}

// The ahead-of-call verdict is the DETERMINISTIC one: a model classifier is
// never consulted at build time — it would cost one model call per tool per
// build, with no call input to judge, and its verdict would withhold tools
// the allowlist grants.
//
// Reddens on the mutation that drops `Deterministic` from the pre-check.
func TestResolveToolsForNode_SandboxPreCheckNeverConsultsTheClassifier(t *testing.T) {
	classifier := &countingDenyingClassifier{}
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: placementRegistry(t, "read_file", "bash"),
		toolPolicy:   &tool.ClassifierChecker{Classifier: classifier, Base: tool.NewPolicy("*")},
		sandbox:      fakeSandboxRun{},
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}}

	defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"read_file", "bash"})
	if err != nil {
		t.Fatalf("resolveToolsForNode: %v", err)
	}
	if classifier.calls != 0 {
		t.Errorf("the pre-check consulted the model classifier %d time(s) at build time", classifier.calls)
	}
	names := toolDefNames(defs)
	for _, want := range []string{"read_file", "bash"} {
		if !slices.Contains(names, want) {
			t.Errorf("%q, which the allowlist grants, was withheld: %v", want, names)
		}
	}
}

// The ahead-of-call verdict resolves the policy's alias patterns exactly as
// the call-time guard does: with the alias engine floor on, an allowlist
// naming `Read` grants `read_file` and nothing else, on both paths.
//
// Reddens on the mutation that drops ResolvePattern from the pre-check.
func TestResolveToolsForNode_SandboxPreCheckResolvesAliasesLikeTheGuard(t *testing.T) {
	ctx := tool.WithBuiltinAliases(context.Background(), true)
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}}
	newExecutor := func(sb bool) *ClawExecutor {
		e := &ClawExecutor{
			logger:       iterlog.Nop(),
			toolRegistry: placementRegistry(t, "read_file", "bash"),
			toolPolicy:   tool.NewPolicy("Read"),
		}
		if sb {
			e.sandbox = fakeSandboxRun{}
		}
		return e
	}

	hostDefs, _, err := newExecutor(false).resolveToolsForNode(ctx, node, []string{"read_file", "bash"})
	if err != nil {
		t.Fatalf("resolveToolsForNode (unsandboxed): %v", err)
	}
	guardAllows := map[string]bool{}
	for _, td := range hostDefs {
		_, callErr := td.Execute(ctx, json.RawMessage(`{}`))
		guardAllows[td.Name] = callErr == nil
	}

	sandboxedDefs, _, err := newExecutor(true).resolveToolsForNode(ctx, node, []string{"read_file", "bash"})
	if err != nil {
		t.Fatalf("resolveToolsForNode (sandboxed): %v", err)
	}
	advertised := toolDefNames(sandboxedDefs)
	for _, name := range []string{"read_file", "bash"} {
		if got := slices.Contains(advertised, name); got != guardAllows[name] {
			t.Errorf("%s: the call-time guard allows=%v but the sandboxed node advertised=%v", name, guardAllows[name], got)
		}
	}
	if !guardAllows["read_file"] || guardAllows["bash"] {
		t.Fatalf("the fixture lost its subject: guard verdicts %v, want read_file allowed and bash denied", guardAllows)
	}
}

// Without a sandbox nothing moves: the denied tool stays advertised and its
// call is refused by the guard.
func TestResolveToolsForNode_UnsandboxedNodeKeepsTheCallTimeGuard(t *testing.T) {
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: placementRegistry(t, "read_file", "bash"),
		toolPolicy:   tool.NewPolicy("read_file"),
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}}

	defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"read_file", "bash"})
	if err != nil {
		t.Fatalf("resolveToolsForNode: %v", err)
	}
	names := toolDefNames(defs)
	i := slices.Index(names, "bash")
	if i < 0 {
		t.Fatalf("bash was withheld from an unsandboxed node, whose guard refuses the call instead: %v", names)
	}
	if _, err := defs[i].Execute(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, tool.ErrToolDenied) {
		t.Errorf("the guard did not refuse bash's call: %v", err)
	}
}

// A model-consulting classifier reads each call's input, which a sandboxed
// runner never hands back for its container tools. The gap is named in the
// log instead of left silent.
//
// Reddens on the mutation that drops the warning.
func TestResolveToolsForNode_SandboxedNodeNamesTheClassifierGap(t *testing.T) {
	var buf bytes.Buffer
	e := &ClawExecutor{
		logger:       iterlog.New(iterlog.LevelInfo, &buf),
		toolRegistry: placementRegistry(t, "read_file", "bash", "lsp"),
		toolPolicy:   &tool.ClassifierChecker{Classifier: allowingClassifier{}, Base: tool.NewPolicy("*")},
		sandbox:      fakeSandboxRun{},
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}}

	if _, _, err := e.resolveToolsForNode(context.Background(), node, []string{"read_file", "bash", "lsp"}); err != nil {
		t.Fatalf("resolveToolsForNode: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "LLM tool classifier does not see") {
		t.Fatalf("no warning names the classifier gap on a sandboxed node:\n%s", logged)
	}
	for _, name := range []string{"read_file", "bash"} {
		if !strings.Contains(logged, name) {
			t.Errorf("the warning does not name %q:\n%s", name, logged)
		}
	}
	// `lsp` never runs in the container — the backend refuses the node for
	// it — so it is no call the classifier misses and the warning omits it.
	if strings.Contains(logged, "lsp") {
		t.Errorf("the warning names lsp, which no sandboxed runner executes:\n%s", logged)
	}
}
