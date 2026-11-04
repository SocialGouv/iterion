package runner

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
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

// r8Call scripts one call of a backend; enter runs first (the call's own
// delegate_started is already booked), mid runs after the step.
type r8Call struct {
	enter   func()
	step    bool
	mid     func()
	prose   string
	valid   bool
	missing bool // valid JSON that omits the required "reason"
	err     error
	tokens  int
	cost    float64
	session string
	total   bool
}

type r8Backend struct {
	mu    sync.Mutex
	name  string
	calls []r8Call
	n     int
	hooks *model.EventHooks
}

func (b *r8Backend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	c := b.calls[min(b.n, len(b.calls)-1)]
	b.n++
	b.mu.Unlock()
	if c.enter != nil {
		c.enter()
	}
	if c.step && b.hooks != nil {
		b.hooks.OnLLMRequest(task.NodeID, model.LLMRequestInfo{Model: task.Model, WireModel: "wire", Timestamp: time.Now()})
		b.hooks.OnLLMStepFinish(task.NodeID, model.LLMStepInfo{Number: 1, InputTokens: 1000, OutputTokens: 100})
	}
	if c.mid != nil {
		c.mid()
	}
	r := delegate.Result{Tokens: c.tokens, CostUSD: c.cost, BackendName: b.name, SessionID: c.session, CostIsSessionTotal: c.total}
	if c.err != nil {
		return r, c.err
	}
	switch {
	case c.valid:
		r.Output = map[string]any{"verdict": true, "reason": "ok"}
	case c.missing:
		r.Output = map[string]any{"verdict": true}
	default:
		r.Output = map[string]any{"text": c.prose}
		r.ParseFallback = true
	}
	return r, nil
}

// r8Shared is the run's one ordered event log, tagged per replay.
type r8Shared struct {
	mu   sync.Mutex
	rows []string
	evts []store.Event
}

// r8Gate is one replay's emitter: it forwards to the run's ONE meter, after
// an optional hold on chosen events.
type r8Gate struct {
	tag    string
	shared *r8Shared
	inner  model.EventEmitter
	hold   func(evt store.Event)
	after  func(evt store.Event)
}

func (g *r8Gate) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	if g.hold != nil {
		g.hold(evt)
	}
	g.shared.mu.Lock()
	switch evt.Type {
	case store.EventDelegateStarted, store.EventDelegateFinished, store.EventDelegateError, store.EventLLMStepFinished:
		g.shared.rows = append(g.shared.rows, g.tag+" "+string(evt.Type)+" reask="+str(evt.Data["reask"])+" backend="+str(evt.Data["backend"])+" cost="+str(evt.Data["cost_usd"])+" tokens="+str(evt.Data["tokens"]))
	}
	g.shared.evts = append(g.shared.evts, evt)
	g.shared.mu.Unlock()
	out, err := g.inner.AppendEvent(ctx, runID, evt)
	if g.after != nil {
		g.after(evt)
	}
	return out, err
}

func str(v any) string {
	if v == nil {
		return "-"
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return trimF(x)
	case int:
		return trimF(float64(x))
	case int64:
		return trimF(float64(x))
	}
	return "?"
}

func trimF(f float64) string {
	return strconv.FormatFloat(f, 'g', 8, 64)
}

func r8Env(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENROUTER_API_KEY", "ZAI_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func r8Workflow() *ir.Workflow {
	return &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{"v": {
		Name:   "v",
		Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}, {Name: "reason", Type: ir.FieldTypeString}},
	}}}
}

// r8Executor builds one executor whose events go through gate (tagged) to
// the shared meter, with bs registered and wired to the executor's hooks.
func r8Executor(node *ir.AgentNode, gate *r8Gate, reg *model.Registry, bs ...*r8Backend) *model.ClawExecutor {
	hooks := model.NewStoreEventHooks(context.Background(), gate, "run", iterlog.Nop(), nil, nil)
	breg := delegate.NewRegistry()
	for _, b := range bs {
		b.hooks = &hooks
		breg.Register(b.name, b)
	}
	if reg == nil {
		reg = model.NewRegistry()
	}
	return model.NewClawExecutor(reg, r8Workflow(),
		model.WithBackendRegistry(breg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()),
		model.WithDefaultBackend(node.Backend), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}))
}

func r8Node(backend, modelSpec string, fallbacks ...ir.Fallback) *ir.AgentNode {
	n := agentNode(backend, modelSpec, fallbacks...)
	n.OutputSchema = "v"
	return n
}

func r8Report(t *testing.T, sh *r8Shared, usage *metricsEmitter) (float64, int64) {
	t.Helper()
	for _, r := range sh.rows {
		t.Logf("  %s", r)
	}
	for k, v := range usage.RouteTotals() {
		t.Logf("  route %+v -> $%.6f %d tok", k, v.costUSD, v.tokens())
	}
	c, in, out, agg := usage.RunTotals()
	t.Logf("  RunTotals: $%.6f in=%d out=%d agg=%d", c, in, out, agg)
	return c, agg
}

// S8 — sequential: claw route answers JSON missing a field (no parse
// fallback, so the correction runs AFTER the chain on both trees), the
// claw re-ask runs one priced step. True spend: two steps.
func TestR8_S8_PostChainClawReaskWithSteps(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	b := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{
		{step: true, missing: true, tokens: 1100, cost: 1},
		{step: true, valid: true, tokens: 1100, cost: 1},
	}}
	node := r8Node(delegate.BackendClaw, "openai/gpt-5")
	e := r8Executor(node, &r8Gate{tag: "-", shared: sh, inner: usage}, nil, b)
	out, err := e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	cost, agg := r8Report(t, sh, usage)
	want := 2 * clawStepPrice(t, "openai/gpt-5", 1000, 100)
	t.Logf("calls=%d err=%v out._cost_usd=%v meter=$%.6f agg=%d; true=$%.6f", b.n, err, out["_cost_usd"], cost, agg, want)
	if math.Abs(cost-want) > 1e-9 || agg != 0 {
		t.Errorf("meter books $%.6f / agg %d, true $%.6f", cost, agg, want)
	}
}

// S9 — budget 3, claw primary: first answer prose (step), re-ask 1 prose
// again but different (step), re-ask 2 fails before a step; a claude_code
// rescue serves at $0.50. True: three steps + $0.50.
func TestR8_S9_ThreeClawReasksThenClaudeCode(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "3")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	p := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{
		{step: true, prose: "a", tokens: 1100, cost: 1},
		{step: true, prose: "b", tokens: 1100, cost: 1},
		{step: true, prose: "c", tokens: 1100, cost: 1},
		{err: errors.New("r8: refused"), tokens: 0},
	}}
	r := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := r8Node(delegate.BackendClaw, "openai/gpt-5",
		ir.Fallback{Name: "door", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5", On: []string{"any"}})
	e := r8Executor(node, &r8Gate{tag: "-", shared: sh, inner: usage}, nil, p, r)
	out, err := e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	cost, agg := r8Report(t, sh, usage)
	steps := 0
	for _, ev := range sh.evts {
		if ev.Type == store.EventLLMStepFinished {
			steps++
		}
	}
	want := float64(steps)*clawStepPrice(t, "openai/gpt-5", 1000, 100) + 0.5*float64(r.n)
	t.Logf("claw calls=%d steps=%d cc=%d err=%v out._cost_usd=%v meter=$%.6f agg=%d; true=$%.6f agg=%d", p.n, steps, r.n, err, out["_cost_usd"], cost, agg, want, 50*r.n)
	if math.Abs(cost-want) > 1e-9 || agg != int64(50*r.n) {
		t.Errorf("meter books $%.6f / agg %d, true $%.6f / %d", cost, agg, want, 50*r.n)
	}
}

// S10 — codex and pi as corrected-then-abandoned routes, claw rescue.
func TestR8_S10_CodexPiCorrectedAbandoned(t *testing.T) {
	for _, be := range []string{delegate.BackendCodex, delegate.BackendPi} {
		t.Run(be, func(t *testing.T) {
			r8Env(t)
			t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
			usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
			sh := &r8Shared{}
			p := &r8Backend{name: be, calls: []r8Call{
				{prose: "a", tokens: 100, cost: 1, session: "S"},
				{prose: "b", tokens: 100, cost: 1, session: "S"},
			}}
			r := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{valid: true, tokens: 50, cost: 0.5}}}
			node := r8Node(be, "openai/gpt-5",
				ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"any"}})
			e := r8Executor(node, &r8Gate{tag: "-", shared: sh, inner: usage}, nil, p, r)
			out, err := e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
			cost, agg := r8Report(t, sh, usage)
			want := float64(p.n) + 0.5*float64(r.n)
			t.Logf("%s calls=%d claw=%d err=%v out._cost_usd=%v meter=$%.4f agg=%d; true=$%.2f/%d", be, p.n, r.n, err, out["_cost_usd"], cost, agg, want, p.n*100+50*r.n)
			if math.Abs(cost-want) > 1e-9 || agg != int64(p.n*100+50*r.n) {
				t.Errorf("meter books $%.4f / %d, true $%.2f / %d", cost, agg, want, p.n*100+50*r.n)
			}
			for k, v := range usage.RouteTotals() {
				w := map[string]float64{be: float64(p.n), delegate.BackendClaw: 0.5}[k.backend]
				if math.Abs(v.costUSD-w) > 1e-9 {
					t.Errorf("route %+v booked $%.4f, spent $%.2f", k, v.costUSD, w)
				}
			}
		})
	}
}

// r8Pair runs replay A, then replay B once startB is closed, on two
// executors sharing the run's ONE meter (fan_out_each: same node id).
func r8Pair(t *testing.T, node *ir.AgentNode, usage *metricsEmitter, sh *r8Shared, startB, aDone chan struct{}, as, bs []*r8Backend) (errA, errB error) {
	t.Helper()
	ea := r8Executor(node, &r8Gate{tag: "A", shared: sh, inner: usage}, nil, as...)
	eb := r8Executor(node, &r8Gate{tag: "B", shared: sh, inner: usage}, nil, bs...)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = ea.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
		close(aDone)
	}()
	go func() {
		defer wg.Done()
		<-startB
		_, errB = eb.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	}()
	wg.Wait()
	return errA, errB
}

// F4 — the same chain; A's claude_code rescue answers prose and is corrected
// (re-ask valid, $1 each). Replay B starts DURING A's re-ask. On main the
// re-ask runs after A's delegate_finished; on HEAD inside the chain, before
// it. True: two steps + $2.
func TestR8_F4_FanOutSiblingStartDuringInChainCorrection(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	startB, bStarted, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	aClaw := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{step: true, err: errors.New("r8: stream died"), tokens: 1100, cost: 1}}}
	aCC := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{prose: "I approve.", tokens: 100, cost: 1},
		{enter: func() { close(startB); <-bStarted }, valid: true, tokens: 100, cost: 1},
	}}
	bClaw := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{enter: func() { close(bStarted); <-aDone }, step: true, valid: true, tokens: 1100, cost: 1}}}
	bCC := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := r8Node(delegate.BackendClaw, "openai/gpt-5",
		ir.Fallback{Name: "door", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5", On: []string{"any"}})
	errA, errB := r8Pair(t, node, usage, sh, startB, aDone, []*r8Backend{aClaw, aCC}, []*r8Backend{bClaw, bCC})
	cost, agg := r8Report(t, sh, usage)
	want := 2*clawStepPrice(t, "openai/gpt-5", 1000, 100) + 2
	t.Logf("errA=%v errB=%v aCC.calls=%d meter=$%.6f agg=%d; true=$%.6f agg=200", errA, errB, aCC.n, cost, agg, want)
	if math.Abs(cost-want) > 1e-9 || agg != 200 {
		t.Errorf("DOUBLE: meter books $%.6f / agg %d, true $%.6f / agg 200", cost, agg, want)
	}
}

// F5 — pure claw node (no fallback): A answers prose after one step; its
// re-ask fails before any step (a provider refusal), and replay B starts
// while it runs. True: two steps.
func TestR8_F5_FanOutSiblingStartDuringFailedClawReask(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	startB, bStarted, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{
		{step: true, prose: "I approve.", tokens: 1100, cost: 1},
		{enter: func() { close(startB); <-bStarted }, err: errors.New("r8: provider refused the re-ask")},
	}}
	b := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{enter: func() { close(bStarted); <-aDone }, step: true, valid: true, tokens: 1100, cost: 1}}}
	node := r8Node(delegate.BackendClaw, "openai/gpt-5")
	errA, errB := r8Pair(t, node, usage, sh, startB, aDone, []*r8Backend{a}, []*r8Backend{b})
	cost, agg := r8Report(t, sh, usage)
	want := 2 * clawStepPrice(t, "openai/gpt-5", 1000, 100)
	t.Logf("errA!=nil=%v errB=%v a.calls=%d meter=$%.6f agg=%d; true=$%.6f agg=0", errA != nil, errB, a.n, cost, agg, want)
	if math.Abs(cost-want) > 1e-9 || agg != 0 {
		t.Errorf("DOUBLE: meter books $%.6f / agg %d, true $%.6f / agg 0", cost, agg, want)
	}
}

// S11 — budget 2, claude_code SESSION totals ($1, $1.50, $2.20 on session S,
// all prose, each different), abandoned; claw rescue (no steps) $0.50.
// True: $2.20 + $0.50 / 100+60+70+50 tokens.
func TestR8_S11_TwoSessionTotalCorrectionsAbandoned(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "2")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	p := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{prose: "a", tokens: 100, cost: 1, session: "S", total: true},
		{prose: "b", tokens: 60, cost: 1.5, session: "S", total: true},
		{prose: "c", tokens: 70, cost: 2.2, session: "S", total: true},
	}}
	r := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := r8Node(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"any"}})
	e := r8Executor(node, &r8Gate{tag: "-", shared: sh, inner: usage}, nil, p, r)
	out, err := e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	cost, agg := r8Report(t, sh, usage)
	last := []float64{0, 1, 1.5, 2.2}[min(p.n, 3)]
	tok := []int{0, 100, 160, 230}[min(p.n, 3)]
	want := last + 0.5*float64(r.n)
	t.Logf("cc calls=%d claw=%d err=%v out._cost_usd=%v meter=$%.4f agg=%d; true=$%.2f/%d", p.n, r.n, err, out["_cost_usd"], cost, agg, want, tok+50*r.n)
	if math.Abs(cost-want) > 1e-9 || agg != int64(tok+50*r.n) {
		t.Errorf("meter books $%.4f / %d, true $%.2f / %d", cost, agg, want, tok+50*r.n)
	}
}
