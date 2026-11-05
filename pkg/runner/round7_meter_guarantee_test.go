package runner

import (
	"context"
	"errors"
	"math"
	"os"
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

// r7Call scripts one call of a backend.
type r7Call struct {
	step    bool // fire one priced claw step (1000 in / 100 out) through the hooks
	prose   string
	valid   bool
	err     error
	tokens  int
	cost    float64
	session string
	total   bool
}

type r7Backend struct {
	mu    sync.Mutex
	name  string
	calls []r7Call
	n     int
	hooks *model.EventHooks
	tasks []delegate.Task
}

func (b *r7Backend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tasks = append(b.tasks, task)
	c := b.calls[min(b.n, len(b.calls)-1)]
	b.n++
	if c.step && b.hooks != nil {
		b.hooks.OnLLMRequest(task.NodeID, model.LLMRequestInfo{Model: task.Model, WireModel: "wire", Timestamp: time.Now()})
		b.hooks.OnLLMStepFinish(task.NodeID, model.LLMStepInfo{Number: 1, InputTokens: 1000, OutputTokens: 100})
	}
	r := delegate.Result{Tokens: c.tokens, CostUSD: c.cost, BackendName: b.name, SessionID: c.session, CostIsSessionTotal: c.total}
	if c.err != nil {
		return r, c.err
	}
	if c.valid {
		r.Output = map[string]any{"verdict": true, "reason": "ok"}
		return r, nil
	}
	r.Output = map[string]any{"text": c.prose}
	r.ParseFallback = true
	return r, nil
}

func r7Exec(t *testing.T, node *ir.AgentNode, bs ...*r7Backend) (*metricsEmitter, *eventLog, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENROUTER_API_KEY", "ZAI_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	log := &eventLog{inner: usage}
	hooks := model.NewStoreEventHooks(context.Background(), log, "run", iterlog.Nop(), nil, nil)
	reg := delegate.NewRegistry()
	for _, b := range bs {
		b.hooks = &hooks
		reg.Register(b.name, b)
	}
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{"v": {
		Name:   "v",
		Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}, {Name: "reason", Type: ir.FieldTypeString}},
	}}}
	node.OutputSchema = "v"
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()),
		model.WithDefaultBackend(node.Backend), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}))
	out, err := exec.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	t.Logf("node output cost=%v tokens=%v err=%v", out["_cost_usd"], out["_tokens"], err)
	for _, e := range log.events {
		switch e.Type {
		case store.EventDelegateStarted, store.EventDelegateFinished, store.EventDelegateError, store.EventLLMStepFinished:
			t.Logf("  %-20s backend=%v route=%v cost=%v tokens=%v reask=%v folded=%v", e.Type, e.Data["backend"], e.Data["route_model"], e.Data["cost_usd"], e.Data["tokens"], e.Data["reask"], e.Data["folded_spend"])
		}
	}
	return usage, log, err
}

func r7Routes(t *testing.T, usage *metricsEmitter) {
	for k, v := range usage.RouteTotals() {
		t.Logf("  route %+v -> $%.4f %d tok", k, v.costUSD, v.tokens())
	}
}

var errR7Reask = errors.New("r7: provider refused the re-ask")

// S1: single claw route; first answer prose (one priced step observed), the
// re-ask fails before any step. True spend: the one step.
func TestR7_S1_ClawReaskFailsBeforeAStep(t *testing.T) {
	t.Setenv(model.EnvOutputCorrectionBudget, "1")
	b := &r7Backend{name: delegate.BackendClaw, calls: []r7Call{
		{step: true, prose: "I approve.", tokens: 1100, cost: 1},
		{err: errR7Reask},
	}}
	usage, _, err := r7Exec(t, agentNode(delegate.BackendClaw, "openai/gpt-5"), b)
	r7Routes(t, usage)
	costUSD, in, out, agg := usage.RunTotals()
	want := clawStepPrice(t, "openai/gpt-5", 1000, 100)
	t.Logf("calls=%d err!=nil=%v meter=$%.6f in=%d out=%d agg=%d; true (one step)=$%.6f", b.n, err != nil, costUSD, in, out, agg, want)
	if math.Abs(costUSD-want) > 1e-9 || agg != 0 {
		t.Errorf("DOUBLE/WRONG: meter books $%.6f (agg %d tok), the only spend is one step $%.6f already booked by llm_step_finished", costUSD, agg, want)
	}
}

// S2: claw primary (step observed), its re-ask fails before any step; a
// claude_code rescue serves at $0.50. True spend: the step + $0.50.
func TestR7_S2_ClawReaskFailsThenClaudeCodeServes(t *testing.T) {
	t.Setenv(model.EnvOutputCorrectionBudget, "1")
	p := &r7Backend{name: delegate.BackendClaw, calls: []r7Call{
		{step: true, prose: "I approve.", tokens: 1100, cost: 1},
		{err: errR7Reask},
	}}
	r := &r7Backend{name: delegate.BackendClaudeCode, calls: []r7Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := agentNode(delegate.BackendClaw, "openai/gpt-5",
		ir.Fallback{Name: "door", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5", On: []string{"any"}})
	usage, _, err := r7Exec(t, node, p, r)
	r7Routes(t, usage)
	costUSD, _, _, agg := usage.RunTotals()
	want := clawStepPrice(t, "openai/gpt-5", 1000, 100) + 0.5
	t.Logf("primary calls=%d rescue calls=%d err=%v meter=$%.6f agg=%d; true=$%.6f", p.n, r.n, err, costUSD, agg, want)
	if math.Abs(costUSD-want) > 1e-9 {
		t.Errorf("DOUBLE/WRONG: meter books $%.6f, true spend $%.6f", costUSD, want)
	}
}

// S3: claude_code primary reports SESSION TOTALS (S: $1 then $1.50 after the
// resumed re-ask), still prose; a claw rescue (no steps) serves at $0.50.
// True spend: session S $1.50 + $0.50.
func TestR7_S3_SessionTotalCorrectionAbandoned(t *testing.T) {
	t.Setenv(model.EnvOutputCorrectionBudget, "1")
	p := &r7Backend{name: delegate.BackendClaudeCode, calls: []r7Call{
		{prose: "I approve.", tokens: 100, cost: 1, session: "S", total: true},
		{prose: "Still approve, really.", tokens: 60, cost: 1.5, session: "S", total: true},
	}}
	r := &r7Backend{name: delegate.BackendClaw, calls: []r7Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"any"}})
	usage, _, err := r7Exec(t, node, p, r)
	r7Routes(t, usage)
	costUSD, _, _, agg := usage.RunTotals()
	t.Logf("primary calls=%d (reask session=%q fork=%v) rescue=%d err=%v meter=$%.4f agg=%d; true=$2.00/210", p.n, p.tasks[len(p.tasks)-1].SessionID, p.tasks[len(p.tasks)-1].ForkSession, r.n, err, costUSD, agg)
	if math.Abs(costUSD-2.0) > 1e-9 || agg != 210 {
		t.Errorf("meter books $%.4f / %d tok, true $2.00 / 210", costUSD, agg)
	}
}

// S4: two claude_code routes each corrected then abandoned, a claw rescue
// serves. True: 4 x $1 + $0.50; per route $2 / $2 / $0.50.
func TestR7_S4_TwoCorrectedRoutesAbandoned(t *testing.T) {
	t.Setenv(model.EnvOutputCorrectionBudget, "1")
	p := &r7Backend{name: delegate.BackendClaudeCode, calls: []r7Call{
		{prose: "a", tokens: 100, cost: 1}, {prose: "b", tokens: 100, cost: 1},
		{prose: "c", tokens: 100, cost: 1}, {prose: "d", tokens: 100, cost: 1},
	}}
	r := &r7Backend{name: delegate.BackendClaw, calls: []r7Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "son", Model: "anthropic/claude-sonnet-5", On: []string{"any"}},
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"any"}})
	usage, _, err := r7Exec(t, node, p, r)
	r7Routes(t, usage)
	costUSD, _, _, agg := usage.RunTotals()
	want := float64(p.n) + 0.5
	t.Logf("cc calls=%d claw=%d err=%v meter=$%.4f agg=%d; true=$%.2f/%d (the correction budget is per node invocation)", p.n, r.n, err, costUSD, agg, want, p.n*100+50)
	if math.Abs(costUSD-want) > 1e-9 || agg != int64(p.n*100+50) {
		t.Errorf("meter books $%.4f / %d, true $%.2f", costUSD, agg, want)
	}
	for k, v := range usage.RouteTotals() {
		want := map[string]float64{"anthropic/claude-opus-5": 2, "anthropic/claude-sonnet-5": float64(p.n - 2), "openai/gpt-5": 0.5}[k.model]
		if math.Abs(v.costUSD-want) > 1e-9 {
			t.Errorf("route %+v booked $%.4f, spent $%.2f", k, v.costUSD, want)
		}
	}
}

// S5: partial correction — budget 2, the first re-ask answers different
// prose, the second fails metered ($1/100 tok); a claw rescue serves.
// True: 3 x $1 + $0.50.
func TestR7_S5_PartialCorrectionThenRescue(t *testing.T) {
	t.Setenv(model.EnvOutputCorrectionBudget, "2")
	p := &r7Backend{name: delegate.BackendClaudeCode, calls: []r7Call{
		{prose: "a", tokens: 100, cost: 1}, {prose: "b", tokens: 100, cost: 1},
		{err: errR7Reask, tokens: 100, cost: 1},
	}}
	r := &r7Backend{name: delegate.BackendClaw, calls: []r7Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"any"}})
	usage, _, err := r7Exec(t, node, p, r)
	r7Routes(t, usage)
	costUSD, _, _, agg := usage.RunTotals()
	t.Logf("cc calls=%d claw=%d err=%v meter=$%.4f agg=%d; true=$3.50/350", p.n, r.n, err, costUSD, agg)
	if math.Abs(costUSD-3.5) > 1e-9 || agg != 350 {
		t.Errorf("meter books $%.4f / %d, true $3.50 / 350", costUSD, agg)
	}
}

// S6: terminal element corrected then failed, after an abandoned corrected
// one (exhausted chain). True: 4 x $1.
func TestR7_S6_ExhaustedChainBothCorrected(t *testing.T) {
	t.Setenv(model.EnvOutputCorrectionBudget, "1")
	p := &r7Backend{name: delegate.BackendClaudeCode, calls: []r7Call{
		{prose: "a", tokens: 100, cost: 1}, {prose: "b", tokens: 100, cost: 1},
		{prose: "c", tokens: 100, cost: 1}, {prose: "d", tokens: 100, cost: 1},
	}}
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "son", Model: "anthropic/claude-sonnet-5", On: []string{"any"}})
	usage, _, err := r7Exec(t, node, p)
	r7Routes(t, usage)
	costUSD, _, _, agg := usage.RunTotals()
	t.Logf("cc calls=%d err=%v meter=$%.4f agg=%d; true=$%d/%d", p.n, err, costUSD, agg, p.n, p.n*100)
	if math.Abs(costUSD-float64(p.n)) > 1e-9 || agg != int64(p.n*100) {
		t.Errorf("meter books $%.4f / %d, true $%d", costUSD, agg, p.n)
	}
}

// S7: claw primary runs one observed step and then fails (a later turn's
// provider error); the claude_code rescue answers prose and its in-chain
// correction succeeds. True spend: the claw step + 2 x $1.
func TestR7_S7_ClawStepsThenCorrectedClaudeCodeRescue(t *testing.T) {
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	p := &r7Backend{name: delegate.BackendClaw, calls: []r7Call{
		{step: true, err: errR7Reask, tokens: 1100, cost: 1},
	}}
	r := &r7Backend{name: delegate.BackendClaudeCode, calls: []r7Call{
		{prose: "I approve.", tokens: 100, cost: 1},
		{valid: true, tokens: 100, cost: 1},
	}}
	node := agentNode(delegate.BackendClaw, "openai/gpt-5",
		ir.Fallback{Name: "door", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5", On: []string{"any"}})
	usage, _, err := r7Exec(t, node, p, r)
	r7Routes(t, usage)
	costUSD, _, _, agg := usage.RunTotals()
	want := clawStepPrice(t, "openai/gpt-5", 1000, 100) + 2
	t.Logf("claw calls=%d cc calls=%d err=%v meter=$%.6f agg=%d; true=$%.6f (agg 200)", p.n, r.n, err, costUSD, agg, want)
	if math.Abs(costUSD-want) > 1e-9 || agg != 200 {
		t.Errorf("DOUBLE/WRONG: meter books $%.6f / agg %d, true $%.6f / agg 200", costUSD, agg, want)
	}
}
