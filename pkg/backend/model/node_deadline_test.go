package model

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// crossingCutByItsBound stands in for a sandboxed claw crossing: it runs
// until the node's own bound ends it, then reports what the far side saw —
// the runner killed with its node — and never the context error that cut it.
type crossingCutByItsBound struct {
	calls atomic.Int32
	// words is what the far side's account reads as. "signal:" is retryable
	// in place (isDelegateRetryable), so a policy with budget left loses
	// these words to the bare ctx error the backoff's cancel arm returns.
	words string
}

func (b *crossingCutByItsBound) Execute(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
	b.calls.Add(1)
	<-ctx.Done()
	return delegate.Result{}, errors.New(b.words)
}

func boundedNode(timeout string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "slow"},
		LLMFields: ir.LLMFields{Backend: "crossing", Timeout: timeout},
	}
}

func executorWithCrossing(t *testing.T, b delegate.Backend, policy RetryPolicy) *ClawExecutor {
	t.Helper()
	reg := delegate.NewRegistry()
	reg.Register("crossing", b)
	return NewClawExecutor(NewRegistry(),
		&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}},
		WithBackendRegistry(reg),
		WithRetryPolicy(policy),
	)
}

// The bound belongs to the node, so the cut is NAMED by the site that froze
// it: the cause reaches the chain for every reader that asks errors.Is for a
// cut, and the crossing's own words stay the detail instead of becoming the
// classification.
//
// Mutation that reddens this (producer): drop the `err = &ErrNodeDeadline{…}`
// defer from executeBackend (executor_build_task.go) — the error that comes
// back is the far side's bare sentence and the type assertion fails.
func TestANodeCutByItsOwnTimeoutNamesTheCutInItsChainAndNotInItsWords(t *testing.T) {
	t.Parallel()
	// MaxAttempts 1: no in-place budget, so the FAR SIDE's words are what
	// comes back — the cell the ticket names, where the chain carries the
	// cut and the text carries none of it.
	b := &crossingCutByItsBound{words: "claw backend: runner exited with error: signal: killed (stderr: )"}
	exec := executorWithCrossing(t, b, RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond})

	_, err := exec.executeBackend(context.Background(), boundedNode("20ms"), map[string]any{})
	if err == nil {
		t.Fatal("the node answered; want the failure its own bound made")
	}
	var cut *ErrNodeDeadline
	if !errors.As(err, &cut) {
		t.Fatalf("the error is %T (%v); want it to NAME the node's own bound as *ErrNodeDeadline — the engine must not have to read words for a cut it made", err, err)
	}
	if cut.NodeID != "slow" || cut.Timeout != 20*time.Millisecond {
		t.Errorf("the cut names node %q / bound %s; want \"slow\" / 20ms — the fact travels with the result, it is not reconstructed", cut.NodeID, cut.Timeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("the chain does not answer errors.Is(context.DeadlineExceeded); the engine's budget arm and a branch join both read the cut from the chain")
	}
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("the far side's account is gone from %q; the crossing's words are the detail of the failure", err.Error())
	}
	if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("the error TEXT names the cut (%q); recovery dispatch's last tier matches that substring as a network transient and re-runs the node six times (#2456)", err.Error())
	}
	if n := b.calls.Load(); n != 1 {
		t.Errorf("the backend was called %d times; want once — the bound is the author's, and the in-place budget was one attempt", n)
	}
}

// The same cut with an in-place budget left: the backoff's cancel arm
// returns the bare context error, so the OBSERVED half is the deadline
// itself. The type still carries the cut, and the classification no longer
// depends on which of the two came back (recovery.Classify reads the type).
func TestANodeCutMidBackoffStillNamesTheCutByType(t *testing.T) {
	t.Parallel()
	b := &crossingCutByItsBound{words: "claw backend: runner exited with error: signal: killed (stderr: )"}
	exec := executorWithCrossing(t, b, RetryPolicy{MaxAttempts: 3, BackoffBase: time.Hour})

	_, err := exec.executeBackend(context.Background(), boundedNode("20ms"), map[string]any{})
	var cut *ErrNodeDeadline
	if !errors.As(err, &cut) {
		t.Fatalf("the error is %T (%v); want *ErrNodeDeadline whatever the layer below handed up", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("the chain does not answer errors.Is(context.DeadlineExceeded)")
	}
}

// A node with no `timeout:` is never named as cut by one: the wrap exists
// only where the executor itself froze a deadline, so an unbounded node's
// failure keeps its own classification.
func TestANodeWithoutATimeoutIsNeverNamedAsCutByOne(t *testing.T) {
	t.Parallel()
	b := &refusingBackend{err: errors.New("claw backend: runner: boom")}
	exec := executorWithCrossing(t, b, RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond})

	_, err := exec.executeBackend(context.Background(), boundedNode(""), map[string]any{})
	var cut *ErrNodeDeadline
	if errors.As(err, &cut) {
		t.Fatalf("an unbounded node's failure was named a bound expiring: %v", err)
	}
}

// A run cancelled while the node is bounded stays the ENGINE's reading: the
// node's bound did not fire, and the cut the engine knows about at its node
// boundary must not be re-modelled as this node's own deadline.
func TestARunCancelledUnderABoundedNodeIsNotTheNodesOwnDeadline(t *testing.T) {
	t.Parallel()
	b := &crossingCutByItsBound{words: "claw backend: runner exited with error: signal: killed (stderr: )"}
	exec := executorWithCrossing(t, b, RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	_, err := exec.executeBackend(ctx, boundedNode("1h"), map[string]any{})
	if err == nil {
		t.Fatal("the node answered; want the cancellation")
	}
	var cut *ErrNodeDeadline
	if errors.As(err, &cut) {
		t.Fatalf("a cancelled run was named as the node's own timeout expiring: %v", err)
	}
}

// refusingBackend fails at once with the error it was given.
type refusingBackend struct{ err error }

func (b *refusingBackend) Execute(context.Context, delegate.Task) (delegate.Result, error) {
	return delegate.Result{}, b.err
}

// A run whose OWN deadline fires under a bounded node stays the engine's
// reading too: the node's bound did not expire, so the cut is not named as
// its own — the duration-budget arm reads the context the engine froze the
// deadline into, and a node that re-modelled the run's wall as its bound
// would take that reading away from it (the shape measured and reverted as
// #2456's patch 2).
func TestTheRunsOwnDeadlineUnderABoundedNodeIsNotTheNodesOwnDeadline(t *testing.T) {
	t.Parallel()
	b := &crossingCutByItsBound{words: "claw backend: runner exited with error: signal: killed (stderr: )"}
	exec := executorWithCrossing(t, b, RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := exec.executeBackend(ctx, boundedNode("1h"), map[string]any{})
	if err == nil {
		t.Fatal("the node answered; want the run's own deadline to end it")
	}
	var cut *ErrNodeDeadline
	if errors.As(err, &cut) {
		t.Fatalf("the RUN's deadline was named as the node's own bound expiring (%s): %v", cut.Timeout, err)
	}
}

// A BOUNDED node that fails of its own accord, well inside its bound, is not
// named as cut by it: the bound is only what expired, never a label for
// every failure a bounded node can have. Without this reading the catch-all
// retry would disappear for every node that declares a `timeout:`.
func TestABoundedNodeThatFailsInsideItsBoundIsNotNamedAsCutByIt(t *testing.T) {
	t.Parallel()
	b := &refusingBackend{err: errors.New("claw backend: runner: boom")}
	exec := executorWithCrossing(t, b, RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond})

	_, err := exec.executeBackend(context.Background(), boundedNode("1h"), map[string]any{})
	if err == nil {
		t.Fatal("the node answered; want its own failure")
	}
	var cut *ErrNodeDeadline
	if errors.As(err, &cut) {
		t.Fatalf("a failure inside the bound was named as the bound expiring: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the node's own failure is gone from %q", err.Error())
	}
}
