package model

// The subagent child loop inherits the parent loop's permission policy. The
// runner reads it off the ctx every tool call carries (WithParentPermission,
// set by the claw backend where it arms its own loop), so a deny or ask row
// — over a class or a spelling — bounds the calls a delegation makes, the
// same calls the parent could not make itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// subagentPolicyClient returns one scripted stream per model call and keeps
// every request body, so a test can read the conversation the child sent
// back after a gated call.
type subagentPolicyClient struct {
	mu     sync.Mutex
	calls  int
	bodies []string
}

func (m *subagentPolicyClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := json.Marshal(req.Messages)
	m.bodies = append(m.bodies, string(b))
	m.calls++
	ch := make(chan api.StreamEvent, 16)
	go func(call int) {
		defer close(ch)
		events := textEvents("done", 10, 5)
		if call == 1 {
			events = toolUseEvents("tu_1", "bash", `{"command":"echo hi"}`, 10, 5)
		}
		for _, ev := range events {
			ch <- ev
		}
	}(m.calls)
	return ch, nil
}

func (m *subagentPolicyClient) conversation(t *testing.T, call int) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if call < 1 || call > len(m.bodies) {
		t.Fatalf("model call %d not made (%d calls)", call, len(m.bodies))
	}
	return m.bodies[call-1]
}

// runChild calls the runner once with a bash tool in the registry and
// reports how many times the tool executed, alongside the client whose
// bodies hold the child's conversation.
func runChild(t *testing.T, ctx context.Context) (int, *subagentPolicyClient, error) {
	t.Helper()
	client := &subagentPolicyClient{}
	modelReg := NewRegistry()
	modelReg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })

	toolReg := tool.NewRegistry()
	bashCalls := 0
	if err := toolReg.RegisterBuiltin("bash", "test shell", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) {
			bashCalls++
			return "ran", nil
		}); err != nil {
		t.Fatalf("register bash: %v", err)
	}

	run := NewSubagentRunner(modelReg, toolReg, EventHooks{}, nil, "anthropic/claude-opus-4-6")
	_, err := run(ctx, map[string]any{"description": "probe", "prompt": "run the command"})
	if bashCalls > 1 {
		t.Fatalf("bash executed %d times", bashCalls)
	}
	return bashCalls, client, err
}

// TestSubagentChildInheritsTheParentPolicy: a parent whose rules deny the
// shell class denies the child's bash call too — the refusal the child's
// conversation carries names the parent's own rule, and the tool never ran.
// Red when the child loop is built with a nil Permission: the gate then
// stands down on exactly the delegation the parent's deny was written to
// bound.
func TestSubagentChildInheritsTheParentPolicy(t *testing.T) {
	t.Parallel()
	policy, err := permission.NewPolicy(permission.ModeDeny, nil, nil, []string{"class:shell"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := WithParentPermission(context.Background(), policy)

	calls, client, err := runChild(t, ctx)
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if calls != 0 {
		t.Fatalf("the child's bash executed %d times under the parent's deny class; want 0", calls)
	}
	conv := client.conversation(t, 2)
	if !strings.Contains(conv, "Permission denied") || !strings.Contains(conv, "class:shell") {
		t.Errorf("the child's second turn must carry the refusal naming the parent's rule; got %s", conv)
	}
}

// TestSubagentChildWithoutTheDenyStillRuns: a parent policy that names no
// shell rule lets the child run bash — the bound is the parent's rules, not
// a blanket refusal of the child loop.
func TestSubagentChildWithoutTheDenyStillRuns(t *testing.T) {
	t.Parallel()
	policy, err := permission.NewPolicy(permission.ModeAsk, []string{"bash"}, nil, nil)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := WithParentPermission(context.Background(), policy)

	calls, _, err := runChild(t, ctx)
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the child's bash executed %d times under an allow row; want 1", calls)
	}
}

// TestSubagentChildWithoutAPolicyRunsUnGated: no parent policy (the gate
// off) leaves the child ungated, exactly like the parent's own loop.
func TestSubagentChildWithoutAPolicyRunsUnGated(t *testing.T) {
	t.Parallel()
	calls, _, err := runChild(t, context.Background())
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the child's bash executed %d times with no policy; want 1", calls)
	}
}

// executeDelegation drives the full host shape through ClawBackend.Execute
// itself: the backend arms its loop with the task's policy, the `agent`
// tool's Execute is the runner a host wires (the child's bash lives in the
// process registry), and one scripted client serves both conversations in
// order — the parent dispatch, the child's bash call, the child's final
// text, the parent's final text. Returns how many times bash executed for
// real and every recorded model request, parent and child alike.
func executeDelegation(t *testing.T, denyClass bool) (int, []api.CreateMessageRequest) {
	t.Helper()
	client := newMockClient(
		toolUseEvents("tu_parent", "agent", `{"description":"probe","prompt":"run the command"}`, 10, 5),
		toolUseEvents("tu_child", "bash", `{"command":"echo hi"}`, 10, 5),
		textEvents("child done", 10, 5),
		textEvents("parent done", 10, 5),
	)
	modelReg := NewRegistry()
	modelReg.Register("test", func(string) (api.APIClient, error) { return client, nil })

	toolReg := tool.NewRegistry()
	bashCalls := 0
	if err := toolReg.RegisterBuiltin("bash", "test shell", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) {
			bashCalls++
			return "ran", nil
		}); err != nil {
		t.Fatalf("register bash: %v", err)
	}

	// The only difference between the two cells is the deny row: delegation
	// and the shell spelling stay allowed, so the child's refusal can only
	// come from the class bound.
	var deny []string
	if denyClass {
		deny = []string{"class:shell"}
	}
	policy, err := permission.NewPolicy(permission.ModeDeny, []string{"agent", "bash"}, nil, deny)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	runner := NewSubagentRunner(modelReg, toolReg, EventHooks{}, nil, "test/test-model")
	backend := NewClawBackend(modelReg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	task := delegate.Task{
		NodeID:       "delegator",
		Model:        "test/test-model",
		UserPrompt:   "delegate the command",
		HasTools:     true,
		ToolMaxSteps: 5,
		ToolDefs: []delegate.ToolDef{
			{Name: "bash", Description: "test shell", InputSchema: json.RawMessage(`{"type":"object"}`),
				Execute: func(context.Context, json.RawMessage) (string, error) { return "parent ran", nil }},
			{Name: "agent", Description: "delegate to a child", InputSchema: json.RawMessage(`{"type":"object"}`),
				// Host wiring: the runner takes the model's decoded arguments,
				// the tool surface carries them as raw JSON.
				Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
					var in map[string]any
					if err := json.Unmarshal(input, &in); err != nil {
						return "", fmt.Errorf("agent: decode input: %w", err)
					}
					return runner(ctx, in)
				}},
		},
		Permission: policy,
	}
	if _, err := backend.Execute(context.Background(), task); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return bashCalls, client.getCalls()
}

// TestExecuteArmsTheChildGateWithTheTaskPolicy: the arm lives in
// ClawBackend.Execute — the same call that gates the parent loop arms the
// ctx its tool executors receive, and the host-wired runner reads that ctx.
// A task whose policy denies the shell class stops the child's bash at the
// delegation, the refusal naming the task's own row; the read-level tests
// above build their own armed ctx, so they cannot see this line dropped.
func TestExecuteArmsTheChildGateWithTheTaskPolicy(t *testing.T) {
	t.Parallel()
	calls, reqs := executeDelegation(t, true)
	if calls != 0 {
		t.Fatalf("the child's bash executed %d times under the task's deny class; want 0", calls)
	}
	denied := false
	for _, req := range reqs {
		b, _ := json.Marshal(req.Messages)
		conv := string(b)
		if strings.Contains(conv, "run the command") &&
			strings.Contains(conv, "Permission denied") &&
			strings.Contains(conv, "class:shell") {
			denied = true
		}
	}
	if !denied {
		t.Errorf("no child conversation carries the refusal naming the task's deny row (%d conversations)", len(reqs))
	}
}

// TestExecuteWithoutTheDenyRowDelegatesUnbounded: the same Execute shape
// with only the deny row removed — the child's bash runs, so the bound was
// the task's row, not the delegation itself.
func TestExecuteWithoutTheDenyRowDelegatesUnbounded(t *testing.T) {
	t.Parallel()
	calls, reqs := executeDelegation(t, false)
	if calls != 1 {
		t.Fatalf("the child's bash executed %d times with no deny row; want 1", calls)
	}
	for _, req := range reqs {
		b, _ := json.Marshal(req.Messages)
		if strings.Contains(string(b), "Permission denied") {
			t.Fatalf("no call is refused without the deny row: %s", b)
		}
	}
}
