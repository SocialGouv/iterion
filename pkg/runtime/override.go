package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Live-steering overrides (bump_loop / raise_budget).
//
// The engine's run state is owned by the single execution-loop goroutine
// (see the concurrency contract on runState), so overrides are applied
// COOPERATIVELY: callers push an OverrideMsg onto the channel wired via
// WithOverrideChannel and the engine drains it at the top of each
// execLoop iteration — the same safe boundary as the operator-pause
// signal. The engine applies the change, persists it on the run record
// (so resume re-applies it), emits EventRunSteered, then acks.
//
// A run inside a long node execution therefore acks at the NEXT loop
// boundary — the same latency profile as pause. Callers bound their
// wait with OverrideMsg.Await.

// OverrideKind discriminates the steering commands.
type overrideKind int

const (
	overrideBumpLoop overrideKind = iota + 1
	overrideRaiseBudget
)

// UnknownLoopError reports a bump_loop against a loop name the workflow
// does not declare; Available lists the declared names for the caller's
// error payload (truthful-contract 400).
type UnknownLoopError struct {
	Loop      string
	Available []string
}

func (e *UnknownLoopError) Error() string {
	return fmt.Sprintf("runtime: unknown loop %q (available: %v)", e.Loop, e.Available)
}

// ErrNoBudgetDeclared reports a raise_budget on a run whose workflow
// declares no budget: block. The engine deliberately does NOT
// synthesize a budget mid-run — an unbudgeted run is unbounded, and
// silently upgrading it to a bounded one would change enforcement
// semantics under the operator's feet (truthful-contract 409).
var ErrNoBudgetDeclared = errors.New("runtime: run has no budget block; cannot raise caps")

// ErrInvalidOverride reports a structurally invalid command (e.g. a
// non-positive bump delta) — truthful-contract 400.
var ErrInvalidOverride = errors.New("runtime: invalid override")

// OverrideResult is the engine's synchronous answer to one override.
type OverrideResult struct {
	// Applied describes what changed (empty on noop/error).
	Applied map[string]any
	// Effective is the post-apply state relevant to the command.
	Effective map[string]any
	// Noop is true when the command was valid but changed nothing.
	Noop       bool
	NoopReason string
	// Err carries *UnknownLoopError / ErrNoBudgetDeclared /
	// ErrInvalidOverride, or a persistence failure (state applied
	// in-memory but not durable — the message says so explicitly).
	Err error
}

// OverrideMsg is one steering command in flight. Construct via the
// factories; the zero value is invalid.
type OverrideMsg struct {
	kind     overrideKind
	loopName string
	delta    int
	budget   ir.BudgetOverrides
	// IssuedBy names the principal for the persisted event ("" local).
	issuedBy string
	ack      chan OverrideResult
}

// NewBumpLoopOverride grants delta extra iterations to the named loop
// for the remainder of the run (entry-resets keep their semantics: the
// grant raises the ceiling, it does not touch counters).
func NewBumpLoopOverride(loop string, delta int, issuedBy string) *OverrideMsg {
	return &OverrideMsg{
		kind:     overrideBumpLoop,
		loopName: loop,
		delta:    delta,
		issuedBy: issuedBy,
		ack:      make(chan OverrideResult, 1),
	}
}

// NewRaiseBudgetOverride raises the run's budget caps to the supplied
// ABSOLUTE values (raise-only; lower-or-equal values are a noop).
func NewRaiseBudgetOverride(caps ir.BudgetOverrides, issuedBy string) *OverrideMsg {
	return &OverrideMsg{
		kind:     overrideRaiseBudget,
		budget:   caps,
		issuedBy: issuedBy,
		ack:      make(chan OverrideResult, 1),
	}
}

// Ack delivers the result to the waiting sender. The reply primitive of
// whoever owns the receive side of the channel — the engine's drain in
// production, a harness in tests. Buffered(1): never blocks; a second
// Ack on the same message is dropped rather than panicking.
func (m *OverrideMsg) Ack(res OverrideResult) {
	select {
	case m.ack <- res:
	default:
	}
}

// Await blocks for the engine's ack, bounded by timeout and ctx. The
// engine goroutine may have exited (run just finished) — the timeout is
// the guard against waiting on a dead channel.
func (m *OverrideMsg) Await(ctx context.Context, timeout time.Duration) (OverrideResult, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case res := <-m.ack:
		return res, nil
	case <-ctx.Done():
		return OverrideResult{}, ctx.Err()
	case <-t.C:
		return OverrideResult{}, fmt.Errorf("runtime: override not acknowledged within %s (run busy in a long node, or just terminated)", timeout)
	}
}

// drainOverrides applies every pending override at the safe boundary.
// Non-blocking: an empty channel costs one failed select.
func (e *Engine) drainOverrides(rs *runState) {
	if e.overrideCh == nil {
		return
	}
	for {
		select {
		case msg, ok := <-e.overrideCh:
			if !ok {
				return
			}
			if msg == nil {
				continue
			}
			msg.Ack(e.applyOverride(rs, msg))
		default:
			return
		}
	}
}

// applyOverride executes one steering command on the run state (single
// writer: only ever called from the execution-loop goroutine), persists
// the new steering state, and emits the run_steered event.
func (e *Engine) applyOverride(rs *runState, msg *OverrideMsg) OverrideResult {
	switch msg.kind {
	case overrideBumpLoop:
		return e.applyBumpLoop(rs, msg)
	case overrideRaiseBudget:
		return e.applyRaiseBudget(rs, msg)
	default:
		return OverrideResult{Err: fmt.Errorf("%w: unknown kind %d", ErrInvalidOverride, msg.kind)}
	}
}

func (e *Engine) applyBumpLoop(rs *runState, msg *OverrideMsg) OverrideResult {
	if msg.delta <= 0 {
		return OverrideResult{Err: fmt.Errorf("%w: bump delta must be >= 1, got %d", ErrInvalidOverride, msg.delta)}
	}
	loop, ok := e.workflow.Loops[msg.loopName]
	if !ok || loop == nil {
		names := make([]string, 0, len(e.workflow.Loops))
		for name := range e.workflow.Loops {
			names = append(names, name)
		}
		sort.Strings(names)
		return OverrideResult{Err: &UnknownLoopError{Loop: msg.loopName, Available: names}}
	}

	if rs.loopOverrides == nil {
		rs.loopOverrides = make(map[string]int)
	}
	rs.loopOverrides[msg.loopName] += msg.delta
	extra := rs.loopOverrides[msg.loopName]
	effective := e.resolveLoopMax(loop, rs)

	res := OverrideResult{
		Applied: map[string]any{
			"loop":  msg.loopName,
			"delta": msg.delta,
			"extra": extra,
		},
		Effective: map[string]any{
			"effective_max": effective,
			"current":       rs.loopCounters[msg.loopName],
		},
	}
	res.Err = e.persistSteering(rs)
	e.emitSteered(rs, "bump_loop", msg.loopName, msg.delta, msg.issuedBy, res)
	return res
}

func (e *Engine) applyRaiseBudget(rs *runState, msg *OverrideMsg) OverrideResult {
	if msg.budget.IsZero() {
		return OverrideResult{Err: fmt.Errorf("%w: raise_budget requires at least one cap", ErrInvalidOverride)}
	}
	if err := msg.budget.Validate(); err != nil {
		return OverrideResult{Err: fmt.Errorf("%w: %v", ErrInvalidOverride, err)}
	}
	if rs.budget == nil {
		return OverrideResult{Err: ErrNoBudgetDeclared}
	}
	effective, raised := rs.budget.RaiseCaps(msg.budget)
	res := OverrideResult{
		Effective: map[string]any{
			"max_cost_usd":   effective.MaxCostUSD,
			"max_tokens":     effective.MaxTokens,
			"max_iterations": effective.MaxIterations,
			"max_duration":   effective.MaxDuration,
		},
	}
	if !raised {
		res.Noop = true
		res.NoopReason = "new caps do not exceed the current ones"
		return res
	}
	res.Applied = appliedBudgetFields(msg.budget)
	res.Err = e.persistSteering(rs)
	e.emitSteered(rs, "raise_budget", "", 0, msg.issuedBy, res)
	return res
}

// persistSteering writes the run's full steering state (grants +
// ABSOLUTE effective caps) so resume re-applies it. A failure keeps the
// in-memory change (the run continues with what the operator granted)
// but is surfaced loudly in the ack: the grant will NOT survive a
// resume.
func (e *Engine) persistSteering(rs *runState) error {
	var raises *store.RunBudgetRaises
	if rs.budget != nil {
		if eff, everRaised := rs.budget.Raises(); everRaised {
			raises = &store.RunBudgetRaises{
				MaxCostUSD:    eff.MaxCostUSD,
				MaxTokens:     eff.MaxTokens,
				MaxIterations: eff.MaxIterations,
				MaxDuration:   eff.MaxDuration,
			}
		}
	}
	var grants map[string]int
	if len(rs.loopOverrides) > 0 {
		grants = make(map[string]int, len(rs.loopOverrides))
		for k, v := range rs.loopOverrides {
			grants[k] = v
		}
	}
	if grants == nil && raises == nil {
		return nil
	}
	if err := e.store.PatchRunSteering(rs.ctx, rs.runID, grants, raises); err != nil {
		return fmt.Errorf("runtime: steering applied in-memory but NOT persisted (a resume will lose it): %w", err)
	}
	return nil
}

func (e *Engine) emitSteered(rs *runState, command, target string, delta int, operator string, res OverrideResult) {
	data := map[string]any{"command": command}
	if target != "" {
		data["target"] = target
	}
	if delta > 0 {
		data["delta"] = delta
	}
	if len(res.Applied) > 0 {
		data["applied"] = res.Applied
	}
	if len(res.Effective) > 0 {
		data["effective"] = res.Effective
	}
	if operator != "" {
		data["operator"] = operator
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventRunSteered, "", data); err != nil && e.logger != nil {
		e.logger.Warn("runtime: emit run_steered: %v", err)
	}
}

// applySteeringState re-seeds the run state from the persisted steering
// fields at resume: loop grants back onto runState, budget raises back
// onto the freshly-rebuilt SharedBudget (raise-only, so a workflow
// source whose declared caps grew past the raise keeps the higher
// value). Called by both resume paths right after newRunState.
func (e *Engine) applySteeringState(rs *runState, r *store.Run) {
	if r == nil {
		return
	}
	if len(r.LoopOverrides) > 0 {
		rs.loopOverrides = make(map[string]int, len(r.LoopOverrides))
		for k, v := range r.LoopOverrides {
			rs.loopOverrides[k] = v
		}
	}
	if len(r.PermissionGrants) > 0 {
		rs.permissionGrants = append([]string(nil), r.PermissionGrants...)
	}
	if r.BudgetRaises != nil && rs.budget != nil {
		rs.budget.RaiseCaps(ir.BudgetOverrides{
			MaxCostUSD:    r.BudgetRaises.MaxCostUSD,
			MaxTokens:     r.BudgetRaises.MaxTokens,
			MaxIterations: r.BudgetRaises.MaxIterations,
			MaxDuration:   r.BudgetRaises.MaxDuration,
		})
	}
}

func appliedBudgetFields(o ir.BudgetOverrides) map[string]any {
	m := map[string]any{}
	if o.MaxCostUSD > 0 {
		m["max_cost_usd"] = o.MaxCostUSD
	}
	if o.MaxTokens > 0 {
		m["max_tokens"] = o.MaxTokens
	}
	if o.MaxIterations > 0 {
		m["max_iterations"] = o.MaxIterations
	}
	if o.MaxDuration != "" {
		m["max_duration"] = o.MaxDuration
	}
	return m
}

// recordPermissionGrant appends a newly earned permission allow rule to
// the run's accumulated set and persists it, so the next resume — which
// builds a fresh engine and a fresh runState — still carries it. No-ops
// on a duplicate so a repeated `allow always` on the same tool does not
// grow the stored slice. A persistence failure is logged, never fatal:
// the grant still holds for the current re-invocation, and the worst
// case is the operator being asked once more.
func (e *Engine) recordPermissionGrant(rs *runState, rule string) {
	if rs == nil || rule == "" {
		return
	}
	if slices.Contains(rs.permissionGrants, rule) {
		return
	}
	rs.permissionGrants = append(rs.permissionGrants, rule)
	if e.store == nil {
		return
	}
	if err := e.store.PatchRunPermissionGrants(rs.ctx, rs.runID, rs.permissionGrants); err != nil {
		e.logger.Warn("runtime: persist permission grant %q: %v", rule, err)
	}
}
