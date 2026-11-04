package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func r9Steps(sh *r8Shared) int {
	n := 0
	for _, ev := range sh.evts {
		if ev.Type == store.EventLLMStepFinished {
			n++
		}
	}
	return n
}

func r9Chain() *ir.AgentNode {
	return r8Node(delegate.BackendClaw, "openai/gpt-5",
		ir.Fallback{Name: "door", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5", On: []string{"any"}})
}

// S12 — sequential: the same node twice on one meter (loop iterations).
// it1: claw step, prose, claw re-ask refused before a step, claude_code
// rescue $0.50 (on main the re-ask runs after the chain and the node fails).
// it2: claw call dies with an unstepped $1 spend, claude_code rescue $0.50.
func TestR9_S12_LoopIterationAfterInChainCorrection(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	claw := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{
		{step: true, prose: "I approve.", tokens: 1100, cost: 1},
		{err: errors.New("r9: provider refused the re-ask")},
		{err: errors.New("r9: stream died"), tokens: 1100, cost: 1},
	}}
	cc := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := r9Chain()
	e := r8Executor(node, &r8Gate{tag: "-", shared: sh, inner: usage}, nil, claw, cc)
	for it := 1; it <= 2; it++ {
		_, err := e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
		t.Logf("iteration %d err=%v", it, err)
	}
	cost, agg := r8Report(t, sh, usage)
	steps := r9Steps(sh)
	want := float64(steps)*clawStepPrice(t, "openai/gpt-5", 1000, 100) + 1 + 0.5*float64(cc.n)
	wantAgg := int64(1100 + 50*cc.n)
	t.Logf("claw calls=%d steps=%d cc=%d meter=$%.6f agg=%d; true=$%.6f agg=%d", claw.n, steps, cc.n, cost, agg, want, wantAgg)
	if math.Abs(cost-want) > 1e-9 || agg != wantAgg {
		t.Errorf("meter books $%.6f / agg %d, true $%.6f / agg %d", cost, agg, want, wantAgg)
	}
}

// S13 — sequential: it1's claw re-ask PANICS after its delegate_started
// (no re-ask outcome event: the set-aside verdict trails). it2 on the same
// meter: claw first answer unstepped ($1, prose), claw re-ask with one
// step, valid. True: two steps + $1.
func TestR9_S13_TrailingSetAsideAfterPanicThenNextIteration(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	claw := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{
		{step: true, prose: "I approve.", tokens: 1100, cost: 1},
		{enter: func() { panic("r9: backend panicked in the re-ask") }},
		{prose: "I approve again.", tokens: 1100, cost: 1},
		{step: true, valid: true, tokens: 1100, cost: 1},
	}}
	node := r8Node(delegate.BackendClaw, "openai/gpt-5")
	e := r8Executor(node, &r8Gate{tag: "-", shared: sh, inner: usage}, nil, claw)
	func() {
		defer func() { t.Logf("iteration 1 recovered: %v", recover()) }()
		_, err := e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
		t.Logf("iteration 1 err=%v", err)
	}()
	usage.mu.Lock()
	t.Logf("after it1: outerSteps=%s stepsSeen=%v", fmt.Sprint(usage.outerSteps), usage.stepsSeen)
	usage.mu.Unlock()
	_, err := e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
	t.Logf("iteration 2 err=%v", err)
	cost, agg := r8Report(t, sh, usage)
	steps := r9Steps(sh)
	want := float64(steps)*clawStepPrice(t, "openai/gpt-5", 1000, 100) + 1
	t.Logf("claw calls=%d steps=%d meter=$%.6f agg=%d; true=$%.6f agg=1100", claw.n, steps, cost, agg, want)
	if math.Abs(cost-want) > 1e-9 || agg != 1100 {
		t.Errorf("meter books $%.6f / agg %d, true $%.6f / agg 1100", cost, agg, want)
	}
}

// F7 — the same race on the path main shares: pure claw node, the first
// answer is JSON missing a field (no parse fallback: the correction runs
// AFTER the chain on every tree). A: one step, its claw re-ask refused
// before a step. B starts while A's re-ask is in flight; B's claw call dies
// mid-stream with a partial spend no step event observed ($1 / 1100 tok —
// generation.go folds a served-then-failed call's usage into the result and
// fires no step). True: one step + B's $1.
func TestR9_F7_PostChainReaskSiblingUnsteppedFailure(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	startB, bStarted, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{
		{step: true, missing: true, tokens: 1100, cost: 1},
		{enter: func() { close(startB); <-bStarted }, err: errors.New("r9: provider refused the re-ask")},
	}}
	b := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{enter: func() { close(bStarted); <-aDone }, err: errors.New("r9: stream died"), tokens: 1100, cost: 1}}}
	node := r8Node(delegate.BackendClaw, "openai/gpt-5")
	errA, errB := r8Pair(t, node, usage, sh, startB, aDone, []*r8Backend{a}, []*r8Backend{b})
	cost, agg := r8Report(t, sh, usage)
	steps := r9Steps(sh)
	want := float64(steps)*clawStepPrice(t, "openai/gpt-5", 1000, 100) + 1
	t.Logf("errA!=nil=%v errB!=nil=%v steps=%d meter=$%.6f agg=%d; true=$%.6f agg=1100", errA != nil, errB != nil, steps, cost, agg, want)
	if math.Abs(cost-want) > 1e-9 || agg != 1100 {
		dir := "OVER"
		if cost < want {
			dir = "UNDER"
		}
		t.Errorf("%s: meter books $%.6f / agg %d, true $%.6f / agg 1100", dir, cost, agg, want)
	}
}

// F7b — F7 with a stamped credential plan: what the pool report would book
// on the donor's fingerprint (fine table) and whether a miss voids it.
func TestR9_F7b_PostChainReaskSiblingFineTable(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	plan := map[string]string{"impl" + model.PlanKeySep + "openai": "fp-donor"}
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), plan)
	sh := &r8Shared{}
	startB, bStarted, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{
		{step: true, missing: true, tokens: 1100, cost: 1},
		{enter: func() { close(startB); <-bStarted }, err: errors.New("r9: provider refused the re-ask")},
	}}
	b := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{enter: func() { close(bStarted); <-aDone }, err: errors.New("r9: stream died"), tokens: 1100, cost: 1}}}
	node := r8Node(delegate.BackendClaw, "openai/gpt-5")
	r8Pair(t, node, usage, sh, startB, aDone, []*r8Backend{a}, []*r8Backend{b})
	r8Report(t, sh, usage)
	rows, missed := usage.fineSnapshot()
	want := float64(r9Steps(sh))*clawStepPrice(t, "openai/gpt-5", 1000, 100) + 1
	t.Logf("fine table: %+v; missed: %+v; donor spent $%.6f", rows, missed, want)
	if got := rows["fp-donor"].CostUSD; math.Abs(got-want) > 1e-9 && missed.observations == 0 {
		t.Errorf("UNDER on the donor row, table NOT voided: books $%.6f, the donor's credential spent $%.6f", got, want)
	}
}

// F8 — the mirror of F7 (over-count) on main's own path: chain claude_code ->
// claw. A: claude_code answers JSON missing a field, its post-chain re-ask
// (claude_code, $1) runs; no claw step in A's attempt (set-aside verdict:
// false). B starts during A's re-ask, its claude_code fails, its claw rescue
// steps once and serves; B's event lands after A's re-ask event. True: A's
// $2 + B's one step.
func TestR9_F8_PostChainReaskRestoresFalseOverSiblingSteps(t *testing.T) {
	r8Env(t)
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "1")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil)
	sh := &r8Shared{}
	startB, bStepped, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	aCC := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{missing: true, tokens: 100, cost: 1},
		{enter: func() { close(startB); <-bStepped }, valid: true, tokens: 100, cost: 1},
	}}
	aClaw := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{valid: true}}}
	bCC := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{{err: errors.New("r9: usage window closed")}}}
	bClaw := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{step: true, mid: func() { close(bStepped); <-aDone }, valid: true, tokens: 1100, cost: 1}}}
	node := r8Node(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"any"}})
	errA, errB := r8Pair(t, node, usage, sh, startB, aDone, []*r8Backend{aCC, aClaw}, []*r8Backend{bCC, bClaw})
	cost, agg := r8Report(t, sh, usage)
	steps := r9Steps(sh)
	want := float64(aCC.n) + float64(steps)*clawStepPrice(t, "openai/gpt-5", 1000, 100)
	t.Logf("errA=%v errB=%v aCC=%d aClaw=%d steps=%d meter=$%.6f agg=%d; true=$%.6f agg=%d", errA, errB, aCC.n, aClaw.n, steps, cost, agg, want, 100*aCC.n)
	if math.Abs(cost-want) > 1e-9 || agg != int64(100*aCC.n) {
		dir := "OVER"
		if cost < want {
			dir = "UNDER"
		}
		t.Errorf("%s: meter books $%.6f / agg %d, true $%.6f / agg %d", dir, cost, agg, want, 100*aCC.n)
	}
}
