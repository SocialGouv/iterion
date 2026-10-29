package model

// A delegation spends inside a tool call, after the node's own usage has been
// counted. The node's result is the only place that spend exists for an
// in-process backend — pkg/runtime's SharedBudget books a node from the
// delegate.Result it returns — so a child's tokens and cost are folded into
// it. Without the fold a claw node could spend without limit through
// subagents while every ceiling the operator declared saw nothing (#1940).

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// The three scripted calls of the delegation below, and what each bills.
const (
	parentFirstIn, parentFirstOut = 100, 30
	childIn, childOut             = 200, 60
	parentLastIn, parentLastOut   = 10, 5

	// Two priced models, deliberately different rates: the child is billed
	// at the price of the model IT ran on (gpt-5), never at the node's
	// (gpt-5-mini), and the two are far enough apart that the figures cannot
	// be confused.
	spendParentModel = "openai/gpt-5-mini"
	spendChildModel  = "openai/gpt-5"
)

// agentToolDefFor takes the `agent` tool through its REAL registration, so the
// test drives the same closure a run does (input validation, JSON shaping and
// all) rather than a hand-rolled stand-in.
func agentToolDefFor(t *testing.T, runner tool.SubagentRunner) delegate.ToolDef {
	t.Helper()
	reg := tool.NewRegistry()
	if err := tool.RegisterClawSubagents(reg, runner); err != nil {
		t.Fatalf("RegisterClawSubagents: %v", err)
	}
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	return delegate.ToolDef{
		Name:        td.QualifiedName,
		Description: td.Description,
		InputSchema: td.InputSchema,
		Execute:     td.Execute,
	}
}

// delegatingNode drives the real ClawBackend through one delegation: the node
// asks for a subagent, the child answers, the node answers. One mock client
// serves both conversations, so the scripts are consumed in call order.
func delegatingNode(t *testing.T) (delegate.Result, error) {
	t.Helper()
	agentInput := `{"description":"summarise","prompt":"Summarise Go interfaces","subagent_type":"general-purpose","model":"` + spendChildModel + `"}`
	mock := newMockClient(
		toolUseEvents("tu_1", "agent", agentInput, parentFirstIn, parentFirstOut),
		textEvents("the child's own answer", childIn, childOut),
		textEvents("the node's answer", parentLastIn, parentLastOut),
	)
	reg := NewRegistry()
	reg.Register("openai", func(string) (api.APIClient, error) { return mock, nil })

	runner := NewSubagentRunner(reg, tool.NewRegistry(), EventHooks{}, nil, "")
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	return backend.Execute(context.Background(), delegate.Task{
		NodeID:       "delegator",
		Model:        spendParentModel,
		UserPrompt:   "Delegate the summary.",
		HasTools:     true,
		ToolDefs:     []delegate.ToolDef{agentToolDefFor(t, runner)},
		ToolMaxSteps: 5,
	})
}

// TestADelegationIsBilledToTheNodeThatSpawnedIt: the node's result carries
// the parent's turns AND the child's. Red when the fold is dropped: the
// result then reports 145 tokens for a node that burned 405, and a declared
// max_tokens would never see the difference.
func TestADelegationIsBilledToTheNodeThatSpawnedIt(t *testing.T) {
	t.Parallel()
	res, err := delegatingNode(t)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	const parentOnly = parentFirstIn + parentFirstOut + parentLastIn + parentLastOut
	const withChild = parentOnly + childIn + childOut
	if res.Tokens != withChild {
		t.Fatalf("the node reports %d tokens; want %d (its own %d plus the child's %d)",
			res.Tokens, withChild, parentOnly, childIn+childOut)
	}
	// The output keys are the OTHER reader of the same figure — a `.bot`
	// reads `_tokens`, the studio renders it — and a total that disagreed
	// with Result.Tokens would make one of the two lie.
	tokens, ok := res.Output["_tokens"].(int)
	if !ok {
		t.Fatalf("_tokens is %T (%v), want an int", res.Output["_tokens"], res.Output["_tokens"])
	}
	if tokens != res.Tokens {
		t.Errorf("_tokens = %d but Result.Tokens = %d; the two readers disagree", tokens, res.Tokens)
	}
}

// TestAChildIsPricedAtItsOwnModel: the child ran on gpt-5 under a gpt-5-mini
// node, and its cost is the price of what it ran on. Red when the sum is
// re-priced at the node's model — a haiku child of an opus node would then be
// overcharged, and this one undercharged by a factor of five.
func TestAChildIsPricedAtItsOwnModel(t *testing.T) {
	t.Parallel()
	res, err := delegatingNode(t)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parentCost := estimateFor(t, spendParentModel, parentFirstIn+parentLastIn, parentFirstOut+parentLastOut)
	childCost := estimateFor(t, spendChildModel, childIn, childOut)
	atParentRate := estimateFor(t, spendParentModel, childIn, childOut)
	if math.Abs(childCost-atParentRate) < 1e-9 {
		t.Fatal("precondition: the two models must have different rates for this witness to discriminate")
	}
	want := parentCost + childCost
	if math.Abs(res.CostUSD-want) > 1e-9 {
		t.Fatalf("the node reports $%.8f; want $%.8f (its own $%.8f plus the child's $%.8f at ITS model, not $%.8f at the node's)",
			res.CostUSD, want, parentCost, childCost, atParentRate)
	}
	usd, ok := res.Output["_cost_usd"].(float64)
	if !ok {
		t.Fatalf("_cost_usd is %T (%v), want a float64", res.Output["_cost_usd"], res.Output["_cost_usd"])
	}
	if math.Abs(usd-res.CostUSD) > 1e-12 {
		t.Errorf("_cost_usd = %v but Result.CostUSD = %v; the two readers disagree", usd, res.CostUSD)
	}
}

// TestTheNodesOwnModelStaysTheReportedOne: `_model` describes the node, which
// ran on its own model; a child's model travels in the child's result text.
func TestTheNodesOwnModelStaysTheReportedOne(t *testing.T) {
	t.Parallel()
	res, err := delegatingNode(t)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, _ := res.Output["_model"].(string); got != spendParentModel {
		t.Errorf("_model = %q, want the node's own %q", got, spendParentModel)
	}
}

// TestANodeThatDelegatesNothingReportsWhatItAlwaysDid: the fold is additive —
// a node with no delegation keeps the exact account it had before, including
// the absence of a cost key when nothing is priced.
func TestANodeThatDelegatesNothingReportsWhatItAlwaysDid(t *testing.T) {
	t.Parallel()
	mock := newMockClient(textEvents("no delegation here", 11, 7))
	reg := NewRegistry()
	reg.Register("openai", func(string) (api.APIClient, error) { return mock, nil })
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	res, err := backend.Execute(context.Background(), delegate.Task{
		NodeID: "plain", Model: spendParentModel, UserPrompt: "Answer.",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Tokens != 18 {
		t.Errorf("Result.Tokens = %d, want 18", res.Tokens)
	}
	if want := estimateFor(t, spendParentModel, 11, 7); math.Abs(res.CostUSD-want) > 1e-12 {
		t.Errorf("Result.CostUSD = %v, want %v", res.CostUSD, want)
	}
}

// TestAFailedChildIsStillBilled: a child that burned a step and then died
// spent real money. GenerateTextDirect hands back the partial usage with its
// error and the runner debits BEFORE it returns, so the node's result keeps
// it — the same contract claw already honours for its own abandoned
// generations (TestClawBackendKeepsWhatAnAbandonedGenerationBurned).
//
// The child runs on its own provider here, so its conversation dies on its
// second call while the node's own script is untouched: the node survives a
// failed delegation (the tool error reaches the model) and still owes what
// the child burned.
func TestAFailedChildIsStillBilled(t *testing.T) {
	t.Parallel()
	const failingChildModel = "anthropic/claude-haiku-4-5"
	agentInput := `{"description":"summarise","prompt":"Summarise","subagent_type":"general-purpose","model":"` + failingChildModel + `"}`
	parentMock := newMockClient(
		toolUseEvents("tu_1", "agent", agentInput, parentFirstIn, parentFirstOut),
		textEvents("the node's answer", parentLastIn, parentLastOut),
	)
	// One script for two calls: the child bills a step, then finds nothing.
	childMock := newMockClient(toolUseEvents("tu_child", "noop", `{}`, childIn, childOut))

	reg := NewRegistry()
	reg.Register("openai", func(string) (api.APIClient, error) { return parentMock, nil })
	reg.Register("anthropic", func(string) (api.APIClient, error) { return childMock, nil })

	childTools := tool.NewRegistry()
	if err := childTools.RegisterBuiltin("noop", "test tool", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
		t.Fatalf("register noop: %v", err)
	}
	runner := NewSubagentRunner(reg, childTools, EventHooks{}, nil, "")
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	res, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "delegator",
		Model:        spendParentModel,
		UserPrompt:   "Delegate.",
		HasTools:     true,
		ToolDefs:     []delegate.ToolDef{agentToolDefFor(t, runner)},
		ToolMaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("Execute: %v — the node survives a failed delegation, the tool error reaches the model", err)
	}
	const parentOnly = parentFirstIn + parentFirstOut + parentLastIn + parentLastOut
	if res.Tokens != parentOnly+childIn+childOut {
		t.Fatalf("the node reports %d tokens; want %d — the failed child burned %d and is owed all the same",
			res.Tokens, parentOnly+childIn+childOut, childIn+childOut)
	}
	if want := estimateFor(t, failingChildModel, childIn, childOut); res.CostUSD <= want {
		t.Errorf("Result.CostUSD = %v; the failed child alone cost %v", res.CostUSD, want)
	}
}

// estimateFor is the price of a call, read from the same table the backend
// reads — a hard-coded figure here would pin today's rate card instead of the
// rule under test.
func estimateFor(t *testing.T, modelSpec string, in, out int) float64 {
	t.Helper()
	usd := clawCostEstimate(modelSpec, in, out)
	if usd <= 0 {
		t.Fatalf("precondition: %q must be priced for this witness to read a cost", modelSpec)
	}
	return usd
}
