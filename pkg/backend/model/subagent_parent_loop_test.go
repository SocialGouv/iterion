package model

// A child conversation is bounded by the NODE that spawned it, not by the
// process registry a host handed the runner: its tools, its model and its
// step budget all come off the context the claw loop fills (ParentLoop, set
// where claw_backend arms its own loop). Wired in production (#1940), the
// registry spans every tool every node of the run can call, so reading it
// under a node would widen the author's declared surface at the delegation.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// parentLoopClient records what each child call was offered — the model and
// the tool names — and answers with plain text so the loop ends in one step.
type parentLoopClient struct {
	mu     sync.Mutex
	models []string
	tools  [][]string
	calls  int
	// loopForever answers every call with the same tool_use, so a test can
	// read the step budget off the number of calls the loop made.
	loopForever bool
}

func (c *parentLoopClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	c.models = append(c.models, req.Model)
	c.tools = append(c.tools, names)
	c.calls++
	loop := c.loopForever
	c.mu.Unlock()

	ch := make(chan api.StreamEvent, 16)
	go func() {
		defer close(ch)
		events := textEvents("child done", 10, 5)
		if loop {
			events = toolUseEvents("tu_1", "read_file", `{"path":"x"}`, 10, 5)
		}
		for _, ev := range events {
			ch <- ev
		}
	}()
	return ch, nil
}

func (c *parentLoopClient) offered(t *testing.T, call int) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if call < 1 || call > len(c.tools) {
		t.Fatalf("child call %d not made (%d calls)", call, len(c.tools))
	}
	return c.tools[call-1]
}

func (c *parentLoopClient) model(t *testing.T, call int) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if call < 1 || call > len(c.models) {
		t.Fatalf("child call %d not made (%d calls)", call, len(c.models))
	}
	return c.models[call-1]
}

func (c *parentLoopClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// parentLoopRunner wires a runner over a registry holding bash AND read_file,
// with the client that reports what the child was offered.
func parentLoopRunner(t *testing.T, hostDefaultModel string) (func(context.Context, map[string]any) (string, error), *parentLoopClient) {
	t.Helper()
	client := &parentLoopClient{}
	modelReg := NewRegistry()
	modelReg.Register("test", func(string) (api.APIClient, error) { return client, nil })

	toolReg := tool.NewRegistry()
	for _, name := range []string{"bash", "read_file"} {
		if err := toolReg.RegisterBuiltin(name, "test "+name, json.RawMessage(`{"type":"object"}`),
			func(context.Context, json.RawMessage) (string, error) { return "ran", nil }); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	return NewSubagentRunner(modelReg, toolReg, EventHooks{}, nil, hostDefaultModel), client
}

// parentTool is one entry of a node's effective tool set.
func parentTool(name string) GenerationTool {
	return GenerationTool{
		Name:        name,
		Description: "the node's own " + name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "ran", nil },
	}
}

// TestAChildCannotExceedTheParentNodesToolSurface is the escalation witness:
// the node declared read_file, the registry also holds bash, and the child
// must be offered read_file alone. Red when the child's tools are built from
// the registry under a parent node — the shape that turns `tools:
// [read_file]` into "read_file for me, everything for my children".
func TestAChildCannotExceedTheParentNodesToolSurface(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model: "test/parent-model",
		Tools: []GenerationTool{parentTool("read_file")},
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	offered := client.offered(t, 1)
	if len(offered) != 1 || offered[0] != "read_file" {
		t.Fatalf("the child was offered %v; the node declared [read_file] alone", offered)
	}
}

// TestAToolLessParentNodeHandsItsChildNothing: `tools: []` is the author
// saying this node has no tools, and a delegation is not a way around it. Red
// when the fallback keys on an empty slice instead of on the presence of a
// parent loop — a tool-less node would then hand its child the registry.
func TestAToolLessParentNodeHandsItsChildNothing(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{Model: "test/parent-model"})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if offered := client.offered(t, 1); len(offered) != 0 {
		t.Fatalf("the child of a tool-less node was offered %v; want nothing", offered)
	}
}

// TestAStandAloneHostStillSelectsFromItsRegistry: a host with no claw loop
// above it (e2e/claw_tool_coverage_live_test.go builds one) keeps the
// registry-selected set — the bound is the node's when there is a node, never
// a blanket refusal of the child's tools.
func TestAStandAloneHostStillSelectsFromItsRegistry(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "test/host-model")
	if _, err := run(context.Background(), map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	offered := strings.Join(client.offered(t, 1), ",")
	for _, want := range []string{"bash", "read_file"} {
		if !strings.Contains(offered, want) {
			t.Errorf("a stand-alone host's child was offered %q, missing %q", offered, want)
		}
	}
}

// TestAChildRunsOnTheParentNodesModel: production wires the runner once per
// RUN, so it names no default — the model is a property of the NODE and is
// resolved per call. Red when the parent's model is not read off the ctx: the
// call then fails for want of a model, which is what a wired-but-modelless
// runner did before.
func TestAChildRunsOnTheParentNodesModel(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model: "test/parent-model",
		Tools: []GenerationTool{parentTool("read_file")},
	})
	out, err := run(ctx, map[string]any{"description": "probe", "prompt": "go"})
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if got := client.model(t, 1); got != "parent-model" {
		t.Errorf("the child ran on model %q, want the parent node's", got)
	}
	if !strings.Contains(out, `"model": "test/parent-model"`) {
		t.Errorf("the child's result does not report the model it ran on: %s", out)
	}
}

// TestTheHostsOwnDefaultOutranksTheParentNodesModel: a host that names a
// cheap default meant it (the live coverage test spawns children on haiku
// under an opus parent) — the parent's model is the LAST resort, not the
// first.
func TestTheHostsOwnDefaultOutranksTheParentNodesModel(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "test/host-default")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model: "test/parent-model",
		Tools: []GenerationTool{parentTool("read_file")},
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if got := client.model(t, 1); got != "host-default" {
		t.Errorf("the child ran on model %q, want the host's own default", got)
	}
}

// TestTheModelsOwnArgumentStillWins: the agent tool takes a model, and an
// explicit one outranks both defaults.
func TestTheModelsOwnArgumentStillWins(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "test/host-default")
	ctx := WithParentLoop(context.Background(), ParentLoop{Model: "test/parent-model"})
	_, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "model": "test/asked-for"})
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if got := client.model(t, 1); got != "asked-for" {
		t.Errorf("the child ran on model %q, want the one the model asked for", got)
	}
}

// TestAModellessDelegationSaysWhatIsMissing: no argument, no host default and
// no parent node — the error names all three rather than inventing a model.
func TestAModellessDelegationSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	run, _ := parentLoopRunner(t, "")
	_, err := run(context.Background(), map[string]any{"description": "probe", "prompt": "go"})
	if err == nil {
		t.Fatal("a delegation with no model anywhere succeeded")
	}
	for _, want := range []string{"host default", "parent node model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q", err, want)
		}
	}
}

// TestAChildInheritsTheParentNodesStepBudget: the node's tool_max_steps
// bounds its children too — a child that looped ten times under a node
// allowed two would spend five times the node's budget inside one tool call.
// Red when the child keeps the runner's own default.
func TestAChildInheritsTheParentNodesStepBudget(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "")
	client.loopForever = true
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model:    "test/parent-model",
		Tools:    []GenerationTool{parentTool("read_file")},
		MaxSteps: 2,
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if got := client.callCount(); got != 2 {
		t.Errorf("the child made %d model calls under a node allowing 2 steps; want 2", got)
	}
}

// TestAChildNeverRecursesIntoItself: `agent` is withheld whatever set the
// child draws from — a node declaring it gets children, not grandchildren.
func TestAChildNeverRecursesIntoItself(t *testing.T) {
	t.Parallel()
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model: "test/parent-model",
		Tools: []GenerationTool{parentTool("read_file"), parentTool("agent")},
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	for _, name := range client.offered(t, 1) {
		if name == "agent" {
			t.Fatalf("the child was offered `agent`: %v", client.offered(t, 1))
		}
	}
}
