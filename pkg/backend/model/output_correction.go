package model

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

const EnvOutputCorrectionBudget = "ITERION_OUTPUT_CORRECTION_BUDGET"

func DefaultOutputCorrectionBudget() int {
	raw := os.Getenv(EnvOutputCorrectionBudget)
	if raw == "" {
		return 2
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		iterlog.New(iterlog.LevelWarn, os.Stderr).Warn("OUTPUT_CORRECTION_BUDGET_INVALID: %s=%q must be a non-negative integer; using default 2", EnvOutputCorrectionBudget, raw)
		return 2
	}
	return n
}

// OutputSchemaError marks exhausted corrections or an unchanged invalid answer.
// Generic execution recovery must not repeat the business task.
type OutputSchemaError struct {
	Unchanged bool
	Cause     error
}

func (e *OutputSchemaError) Error() string {
	if e.Unchanged {
		return fmt.Sprintf("structured output invalid: correction returned an unchanged answer: %v", e.Cause)
	}
	return fmt.Sprintf("structured output invalid: correction budget exhausted: %v", e.Cause)
}
func (e *OutputSchemaError) Unwrap() error { return e.Cause }

// OutputCorrectionPolicy lets the runtime reserve a durable attempt before a
// model call and record its outcome. The executor owns validation and feedback;
// the runtime owns the invocation's budget, persistence and lifecycle events.
type OutputCorrectionPolicy struct {
	// Context bounds a correction call, leaving the business call's context intact.
	Context func(context.Context) (context.Context, context.CancelFunc, error)
	Before  func(context.Context, map[string]any, error) error
	After   func(context.Context, map[string]any, error) error
}

type outputCorrectionKey struct{}

func WithOutputCorrectionPolicy(ctx context.Context, policy OutputCorrectionPolicy) context.Context {
	return context.WithValue(ctx, outputCorrectionKey{}, policy)
}

func outputCorrectionPolicy(ctx context.Context) OutputCorrectionPolicy {
	if p, ok := ctx.Value(outputCorrectionKey{}).(OutputCorrectionPolicy); ok {
		return p
	}
	remaining := DefaultOutputCorrectionBudget()
	return OutputCorrectionPolicy{
		Before: func(ctx context.Context, _ map[string]any, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if remaining == 0 {
				return &OutputSchemaError{Cause: err}
			}
			remaining--
			return nil
		},
		After: func(context.Context, map[string]any, error) error { return nil },
	}
}

// ValidatesOutput identifies the nodes that pass through validateAndRetry.
// Tool and human answers still need the runtime's validation boundary.
func (e *ClawExecutor) ValidatesOutput(node ir.Node) bool {
	switch n := node.(type) {
	case *ir.AgentNode, *ir.JudgeNode:
		return e.schemas[ir.NodeOutputSchema(node)] != nil
	case *ir.RouterNode:
		return n.RouterMode == ir.RouterLLM
	default:
		return false
	}
}

// correctionCosts keeps cumulative session readings separate until the end of
// the loop. A sum across sessions is not itself a session total: MAX(sum, next)
// would erase earlier sessions on the third generation.
type correctionCostKey struct{ backend, session, fingerprint string }
type correctionCosts struct {
	sessions map[correctionCostKey]float64
	total    float64
	perCall  bool
}

func (c *correctionCosts) add(r delegate.Result, task *delegate.Task, backend string) float64 {
	if r.CostUSD == 0 {
		return 0
	}
	id := r.SessionID
	if id == "" && !task.ForkSession {
		id = task.SessionID
	}
	if !r.CostIsSessionTotal || (id == "" && !sharesSession(task)) {
		c.total += r.CostUSD
		c.perCall = true
		return r.CostUSD
	}
	if c.sessions == nil {
		c.sessions = make(map[correctionCostKey]float64)
	}
	key := correctionCostKey{firstNonEmpty(r.BackendName, backend), id, firstNonEmpty(r.SessionFingerprint, task.SessionFingerprint)}
	delta := reaskMarginalCost(delegate.Result{CostUSD: c.sessions[key], CostIsSessionTotal: true}, r, true)
	c.total += delta
	c.sessions[key] = max(c.sessions[key], r.CostUSD)
	return delta
}

func (c *correctionCosts) apply(r delegate.Result) delegate.Result {
	r.CostUSD = c.total
	r.CostIsSessionTotal = !c.perCall && len(c.sessions) == 1
	return r
}

func (p OutputCorrectionPolicy) begin(ctx context.Context, output map[string]any, validationErr error) (context.Context, context.CancelFunc, error) {
	cancel := func() {}
	if p.Context != nil {
		var err error
		ctx, cancel, err = p.Context(ctx)
		if err != nil {
			cancel()
			return ctx, func() {}, err
		}
	}
	if err := p.Before(ctx, output, validationErr); err != nil {
		cancel()
		return ctx, func() {}, err
	}
	return ctx, cancel, nil
}
