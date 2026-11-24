package runner

import (
	"context"
	"errors"
	"fmt"
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

type r10Kind int

const (
	r10Prose r10Kind = iota
	r10Missing
	r10Valid
	r10Err  // failure that spent
	r10Err0 // failure that spent nothing
)

func (k r10Kind) String() string {
	return [...]string{"prose", "missing", "valid", "err$", "err0"}[k]
}

type r10Call struct {
	step bool
	kind r10Kind
}

func (c r10Call) String() string {
	if c.step {
		return "S" + c.kind.String()
	}
	return c.kind.String()
}

// r10Backend scripts calls and records the TRUE spend of each consumed call:
// a stepped claw call costs its step (priced on the step event), an
// unstepped one its result figure, a claude_code call its result figure.
type r10Backend struct {
	mu        sync.Mutex
	name      string
	calls     []r10Call
	n         int
	hooks     *model.EventHooks
	router    bool
	stepPrice float64
	trueUSD   float64
	trueAgg   int64
	panicAt   int // 1-based call index that panics after its step (0 = never)
	hook      func(idx int, phase string)
}

func (b *r10Backend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	c := b.calls[min(b.n, len(b.calls)-1)]
	b.n++
	idx := b.n
	b.mu.Unlock()
	claw := b.name == delegate.BackendClaw
	tokens, cost := 50, 0.5
	if claw {
		tokens, cost = 1100, 1.0
	}
	if c.kind == r10Err0 {
		tokens, cost = 0, 0
	}
	if b.hook != nil {
		b.hook(idx, "pre")
	}
	if claw && c.step && b.hooks != nil {
		b.hooks.OnLLMRequest(task.NodeID, model.LLMRequestInfo{Model: task.Model, WireModel: "wire", Timestamp: time.Now()})
		b.hooks.OnLLMStepFinish(task.NodeID, model.LLMStepInfo{Number: 1, InputTokens: 1000, OutputTokens: 100})
	}
	if b.hook != nil {
		b.hook(idx, "post")
	}
	b.mu.Lock()
	if claw && c.step {
		b.trueUSD += b.stepPrice
	} else {
		b.trueUSD += cost
		b.trueAgg += int64(tokens)
	}
	b.mu.Unlock()
	if b.panicAt == idx {
		panic("r10: backend panicked")
	}
	r := delegate.Result{Tokens: tokens, CostUSD: cost, BackendName: b.name}
	switch c.kind {
	case r10Err, r10Err0:
		return r, errors.New("r10: call failed")
	case r10Valid:
		if b.router {
			r.Output = map[string]any{"selected_route": "a", "reasoning": "ok"}
		} else {
			r.Output = map[string]any{"verdict": true, "reason": "ok"}
		}
	case r10Missing:
		if b.router {
			r.Output = map[string]any{"selected_route": "a"}
		} else {
			r.Output = map[string]any{"verdict": true}
		}
	default:
		r.Output = map[string]any{"text": "I think so."}
		r.ParseFallback = true
	}
	return r, nil
}

type r10Gate struct {
	mu    sync.Mutex
	evts  []store.Event
	inner model.EventEmitter
}

func (g *r10Gate) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	g.mu.Lock()
	g.evts = append(g.evts, evt)
	g.mu.Unlock()
	return g.inner.AppendEvent(ctx, runID, evt)
}

func r10Env(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENROUTER_API_KEY", "ZAI_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func r10Executor(defaultBackend string, gate *r10Gate, bs ...*r10Backend) *model.ClawExecutor {
	hooks := model.NewStoreEventHooks(context.Background(), gate, "run", iterlog.Nop(), nil, nil)
	breg := delegate.NewRegistry()
	for _, b := range bs {
		b.hooks = &hooks
		breg.Register(b.name, b)
	}
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{"v": {
		Name:   "v",
		Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}, {Name: "reason", Type: ir.FieldTypeString}},
	}}}
	return model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(breg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()),
		model.WithDefaultBackend(defaultBackend), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}))
}

func r10Router() *ir.RouterNode {
	n := &ir.RouterNode{RouterMode: ir.RouterLLM}
	n.ID = "route"
	n.Backend = delegate.BackendClaw
	n.Model = "openai/gpt-5"
	n.UserPrompt = "ask"
	return n
}

var r10RouterInput = map[string]any{"_route_candidates": []string{"a", "b"}}

// reaskStarts lists the in_chain marker of each re-ask delegate_started, and
// whether any outcome event carries the marker.
func r10Markers(g *r10Gate) (starts []string, outcomeMarked bool) {
	for _, e := range g.evts {
		if r, _ := e.Data["reask"].(string); r == "" {
			continue
		}
		_, has := e.Data["reask_in_chain"]
		switch e.Type {
		case store.EventDelegateStarted:
			starts = append(starts, fmt.Sprintf("%s:%v", e.Data["reask"], e.Data["reask_in_chain"]))
		case store.EventDelegateFinished, store.EventDelegateError:
			if has {
				outcomeMarked = true
			}
		}
	}
	return starts, outcomeMarked
}

// Every correction site, agent and router, inside and after the chain: only
// a re-ask run inside the chain marks its delegate_started
// `reask_in_chain`, no outcome event carries the mark, and the meter books
// the true spend.
func TestR10_MarkerPerSite(t *testing.T) {
	r10Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	price := clawStepPrice(t, "openai/gpt-5", 1000, 100)
	cases := []struct {
		name    string
		router  bool
		first   r10Call
		inChain bool
	}{
		{"agent prose (in-chain)", false, r10Call{step: true, kind: r10Prose}, true},
		{"agent missing (post-chain)", false, r10Call{step: true, kind: r10Missing}, false},
		{"router prose (in-chain)", true, r10Call{step: true, kind: r10Prose}, true},
		{"router missing (post-chain)", true, r10Call{step: true, kind: r10Missing}, false},
	}
	for _, c := range cases {
		usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, nil)
		g := &r10Gate{inner: usage}
		b := &r10Backend{name: delegate.BackendClaw, router: c.router, stepPrice: price, calls: []r10Call{c.first, {step: true, kind: r10Valid}}}
		e := r10Executor(delegate.BackendClaw, g, b)
		var err error
		if c.router {
			_, err = e.Execute(model.WithRunID(context.Background(), "run"), r10Router(), r10RouterInput)
		} else {
			n := agentNode(delegate.BackendClaw, "openai/gpt-5")
			n.OutputSchema = "v"
			_, err = e.Execute(model.WithRunID(context.Background(), "run"), n, map[string]any{})
		}
		starts, outcomeMarked := r10Markers(g)
		cost, _, _, agg := usage.RunTotals()
		t.Logf("%-28s err=%v reask starts=%v outcome-marked=%v meter=$%.6f agg=%d true=$%.6f agg=%d", c.name, err, starts, outcomeMarked, cost, agg, b.trueUSD, b.trueAgg)
		want := "restart:<nil>"
		if c.inChain {
			want = "restart:true"
		}
		if len(starts) != 1 || starts[0] != want {
			t.Errorf("%s: re-ask delegate_started marks = %v, want [%s]", c.name, starts, want)
		}
		if outcomeMarked {
			t.Errorf("%s: an outcome event carries reask_in_chain", c.name)
		}
		if math.Abs(cost-b.trueUSD) > 1e-9 || agg != b.trueAgg {
			t.Errorf("%s: meter books $%.6f / agg %d, true $%.6f / agg %d", c.name, cost, agg, b.trueUSD, b.trueAgg)
		}
	}
}
