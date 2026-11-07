package runner

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r6ProseBackend: every call answers prose (parse fallback) at a fixed
// per-call price, unless valid is set.
type r6ProseBackend struct {
	mu    sync.Mutex
	calls int
	cost  float64
	tok   int
	name  string
	valid bool
}

func (b *r6ProseBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.valid {
		return delegate.Result{Output: map[string]any{"verdict": true, "reason": "ok"}, Tokens: b.tok, CostUSD: b.cost, BackendName: b.name}, nil
	}
	return delegate.Result{Output: map[string]any{"text": "I approve this change."}, ParseFallback: true, Tokens: b.tok, CostUSD: b.cost, BackendName: b.name}, nil
}

func r6Run(t *testing.T, node *ir.AgentNode, regd map[string]delegate.Backend) (*metricsEmitter, *eventLog, error) {
	t.Helper()
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, nil)
	log := &eventLog{inner: usage}
	hooks := model.NewStoreEventHooks(context.Background(), log, "run", iterlog.Nop(), nil, nil)
	reg := delegate.NewRegistry()
	for name, b := range regd {
		reg.Register(name, b)
	}
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{"v": {
		Name: "v",
		Fields: []*ir.SchemaField{
			{Name: "verdict", Type: ir.FieldTypeBool},
			{Name: "reason", Type: ir.FieldTypeString},
		},
	}}}
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()),
		model.WithDefaultBackend(node.Backend), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}))
	out, err := exec.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	t.Logf("node output=%v err=%v", out, err)
	return usage, log, err
}

func r6Dump(t *testing.T, log *eventLog) {
	for _, e := range log.events {
		if e.Type == store.EventDelegateFinished || e.Type == store.EventDelegateError {
			t.Logf("%s data: backend=%v provider=%v cost=%v tokens=%v attempt=%v reask=%v folded=%v", e.Type, e.Data["backend"], e.Data["provider"], e.Data["cost_usd"], e.Data["tokens"], e.Data["attempt"], e.Data["reask"], e.Data["folded_spend"])
		}
	}
}

// Primary claude_code answers prose twice (first answer + one correction),
// $1 each; the chain ends on a plain `action: skip`. True spend: $2.
func TestR6_CorrectedRouteAbandonedToSkip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	primary := &r6ProseBackend{cost: 1, tok: 100, name: delegate.BackendClaudeCode}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "give_up", Action: ir.FallbackActionSkip, On: []string{"any"}})
	node.OutputSchema = "v"
	usage, log, _ := r6Run(t, node, map[string]delegate.Backend{delegate.BackendClaudeCode: primary})
	r6Dump(t, log)
	costUSD, _, _, agg := usage.RunTotals()
	want := float64(primary.calls) * primary.cost
	t.Logf("primary calls=%d true spend=$%.2f/%d tok; meter run total=$%.2f/%d tok", primary.calls, want, primary.calls*primary.tok, costUSD, agg)
	if math.Abs(costUSD-want) > 1e-9 || agg != int64(primary.calls*primary.tok) {
		t.Errorf("meter books $%.2f / %d tokens, true spend $%.2f / %d tokens", costUSD, agg, want, primary.calls*primary.tok)
	}
}

// Same primary; a claw rescue (no steps observed) serves at $0.50.
func TestR6_CorrectedRouteAbandonedToRescue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	primary := &r6ProseBackend{cost: 1, tok: 100, name: delegate.BackendClaudeCode}
	rescue := &r6ProseBackend{cost: 0.5, tok: 50, name: delegate.BackendClaw, valid: true}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5"})
	node.OutputSchema = "v"
	usage, log, _ := r6Run(t, node, map[string]delegate.Backend{delegate.BackendClaudeCode: primary, delegate.BackendClaw: rescue})
	r6Dump(t, log)
	costUSD, _, _, agg := usage.RunTotals()
	want := float64(primary.calls)*primary.cost + float64(rescue.calls)*rescue.cost
	for k, v := range usage.RouteTotals() {
		t.Logf("route %+v -> $%.2f, %d tok", k, v.costUSD, v.tokens())
	}
	t.Logf("primary calls=%d rescue calls=%d true spend=$%.2f; meter run total=$%.2f/%d tok", primary.calls, rescue.calls, want, costUSD, agg)
	if math.Abs(costUSD-want) > 1e-9 {
		t.Errorf("meter books $%.2f, true spend $%.2f", costUSD, want)
	}
	for k, v := range usage.RouteTotals() {
		if k.backend == delegate.BackendClaw && math.Abs(v.costUSD-0.5) > 1e-9 {
			t.Errorf("rescue route %+v booked $%.2f, it spent $0.50", k, v.costUSD)
		}
		if k.backend == delegate.BackendClaudeCode && math.Abs(v.costUSD-float64(primary.calls)*primary.cost) > 1e-9 {
			t.Errorf("primary route %+v booked $%.2f, it spent $%.2f", k, v.costUSD, float64(primary.calls)*primary.cost)
		}
	}
}

// r6StepProse is a claw-shaped primary: each call fires one priced step
// through the run's hooks, then answers prose.
type r6StepProse struct {
	r6ProseBackend
	hooks *model.EventHooks
}

func (b *r6StepProse) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	b.hooks.OnLLMRequest(task.NodeID, model.LLMRequestInfo{Model: task.Model, WireModel: "wire", Timestamp: time.Now()})
	b.hooks.OnLLMStepFinish(task.NodeID, model.LLMStepInfo{Number: 1, InputTokens: 1000, OutputTokens: 100})
	return b.r6ProseBackend.Execute(ctx, task)
}

// Claw primary on one key (steps observed), claude_code rescue on another:
// the rescue's own $0.50 must reach the meter.
func TestR6_StepObservedClawCorrectedThenClaudeCodeServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	node := agentNode(delegate.BackendClaw, "openai/gpt-5",
		ir.Fallback{Name: "door", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5"})
	node.OutputSchema = "v"
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, nil)
	log := &eventLog{inner: usage}
	hooks := model.NewStoreEventHooks(context.Background(), log, "run", iterlog.Nop(), nil, nil)
	primary := &r6StepProse{r6ProseBackend: r6ProseBackend{cost: 1, tok: 1100, name: delegate.BackendClaw}, hooks: &hooks}
	rescue := &r6ProseBackend{cost: 0.5, tok: 50, name: delegate.BackendClaudeCode, valid: true}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, primary)
	reg.Register(delegate.BackendClaudeCode, rescue)
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{"v": {
		Name:   "v",
		Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}, {Name: "reason", Type: ir.FieldTypeString}},
	}}}
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()),
		model.WithDefaultBackend(node.Backend), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}))
	out, err := exec.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	t.Logf("node output=%v err=%v", out, err)
	r6Dump(t, log)
	for k, v := range usage.RouteTotals() {
		t.Logf("route %+v -> $%.4f, %d tok", k, v.costUSD, v.tokens())
	}
	steps := clawStepPrice(t, "openai/gpt-5", 1000, 100) * float64(primary.calls)
	costUSD, _, _, _ := usage.RunTotals()
	t.Logf("primary calls=%d rescue calls=%d; steps=$%.4f + rescue $0.50; meter=$%.4f", primary.calls, rescue.calls, steps, costUSD)
	if rescue.calls == 1 && math.Abs(costUSD-(steps+0.5)) > 1e-9 {
		t.Errorf("meter books $%.4f, want steps $%.4f + the claude_code rescue's $0.50", costUSD, steps)
	}
}

// r6FixOnReask answers prose first, valid JSON on the correction.
type r6FixOnReask struct {
	mu    sync.Mutex
	calls int
}

func (b *r6FixOnReask) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.calls == 1 {
		return delegate.Result{Output: map[string]any{"text": "I approve."}, ParseFallback: true, Tokens: 100, CostUSD: 1, BackendName: delegate.BackendClaudeCode}, nil
	}
	return delegate.Result{Output: map[string]any{"verdict": true, "reason": "ok"}, Tokens: 100, CostUSD: 1, BackendName: delegate.BackendClaudeCode}, nil
}

// Single route, in-chain correction succeeds: true spend $2 / 200 tokens.
func TestR6_InChainCorrectionSucceedsBooksOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	b := &r6FixOnReask{}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5")
	node.OutputSchema = "v"
	usage, log, _ := r6Run(t, node, map[string]delegate.Backend{delegate.BackendClaudeCode: b})
	r6Dump(t, log)
	costUSD, _, _, agg := usage.RunTotals()
	t.Logf("calls=%d meter=$%.2f/%d tok", b.calls, costUSD, agg)
	if costUSD != 2 || agg != 200 {
		t.Errorf("meter books $%.2f / %d tokens, true spend $2 / 200", costUSD, agg)
	}
}

// Single route, the in-chain correction fails too and the node errors: the
// re-ask event books the correction, the delegate_error event the first
// answer — true spend $2 / 200 tokens, never the correction twice.
func TestR6_InChainCorrectionFailsBooksOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	b := &r6ProseBackend{cost: 1, tok: 100, name: delegate.BackendClaudeCode}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5")
	node.OutputSchema = "v"
	usage, log, err := r6Run(t, node, map[string]delegate.Backend{delegate.BackendClaudeCode: b})
	r6Dump(t, log)
	if err == nil || b.calls != 2 {
		t.Fatalf("scenario broken: want a failed node after one correction, got err=%v after %d call(s)", err, b.calls)
	}
	costUSD, _, _, agg := usage.RunTotals()
	if costUSD != 2 || agg != 200 {
		t.Errorf("meter books $%.2f / %d tokens, true spend $2 / 200", costUSD, agg)
	}
}
