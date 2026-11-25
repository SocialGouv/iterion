package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// hookChildExecutor answers the child's `work` node with hook's verdict.
type hookChildExecutor struct {
	*stubExecutor
	hook func(ctx context.Context) error
}

func (h *hookChildExecutor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	if node.NodeID() != "work" {
		return h.stubExecutor.Execute(ctx, node, input)
	}
	return nil, h.hook(ctx)
}

// runParentOverChild runs wf with a runner that prepares and runs a real
// child engine, then calls afterChild with the child's error before
// returning it to the parent.
func runParentOverChild(t *testing.T, ctx context.Context, wf *ir.Workflow, childExec NodeExecutor, afterChild func(childErr error)) (*store.Run, error) {
	t.Helper()
	s := tmpStore(t)
	runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		inv, err := prepareResourceInvocation(ctx, s, "child-run", req.ParentRunID)
		if err != nil {
			return nil, err
		}
		cerr := New(childWorkWorkflow(), s, childExec, WithParentRunID(req.ParentRunID)).RunPrepared(inv.Context, "child-run", nil, inv)
		if afterChild != nil {
			afterChild(cerr)
		}
		return nil, cerr
	}
	parentExec := newStubExecutor()
	parentExec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "T-1"}}}, nil
	})
	err := New(wf, s, parentExec, WithSubbotRunner(runner)).Run(ctx, "parent-run", nil)
	parent, lerr := s.LoadRun(context.Background(), "parent-run")
	if lerr != nil {
		t.Fatalf("load parent: %v", lerr)
	}
	return parent, err
}

// TestSubbotChildFailureRacingParentCancel: an operator cancel of the parent
// that lands while the subbot node reads its child's verdict ends the parent
// `cancelled` in either order — the child failed first, or the cancel
// reached the child first and it answered in its own words — as a trunk
// agent node does at the same boundary. The child's words stay in the
// parent's record. A child verdict no resume can change keeps its code.
func TestSubbotChildFailureRacingParentCancel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wf        func() *ir.Workflow
		hook      func(cancel context.CancelFunc) func(context.Context) error
		cancelLow bool // cancel after the child returned
		wantCode  store.FailureCode
		wantWords string
	}{
		{
			name: "child failed first, main loop",
			wf:   parentSubbotWorkflow,
			hook: func(context.CancelFunc) func(context.Context) error {
				return func(context.Context) error { return errors.New("tests failed: 3 assertions") }
			},
			cancelLow: true, wantCode: store.FailureCancelled, wantWords: "tests failed: 3 assertions",
		},
		{
			name: "child failed first, fan-out branch",
			wf:   func() *ir.Workflow { return subbotFanOutWorkflow(true, 1) },
			hook: func(context.CancelFunc) func(context.Context) error {
				return func(context.Context) error { return errors.New("tests failed: 3 assertions") }
			},
			cancelLow: true, wantCode: store.FailureCancelled,
		},
		{
			name: "cancel reached the child first, main loop",
			wf:   parentSubbotWorkflow,
			hook: func(cancel context.CancelFunc) func(context.Context) error {
				return func(c context.Context) error { cancel(); <-c.Done(); return errors.New("signal: killed") }
			},
			wantCode: store.FailureCancelled,
		},
		{
			name: "resumable child budget verdict, main loop",
			wf:   parentSubbotWorkflow,
			hook: func(context.CancelFunc) func(context.Context) error {
				return func(context.Context) error {
					return &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "child budget spent"}
				}
			},
			cancelLow: true, wantCode: store.FailureCancelled, wantWords: "child budget spent",
		},
		{
			name: "deterministic child verdict, main loop",
			wf:   parentSubbotWorkflow,
			hook: func(context.CancelFunc) func(context.Context) error {
				return func(context.Context) error {
					return &RuntimeError{Code: ErrCodeToolFailedPermanent, Message: "lint exited 2 on unchanged inputs"}
				}
			},
			cancelLow: true, wantCode: store.FailureToolFailedPermanent, wantWords: "lint exited 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exec := &hookChildExecutor{stubExecutor: newStubExecutor(), hook: tc.hook(cancel)}
			var after func(error)
			if tc.cancelLow {
				after = func(error) { cancel() }
			}
			parent, err := runParentOverChild(t, ctx, tc.wf(), exec, after)
			if err == nil {
				t.Fatal("parent Run returned nil")
			}
			if parent.FailureCode != tc.wantCode {
				t.Fatalf("parent = %s/%q (error %q), want code %q", parent.Status, parent.FailureCode, parent.Error, tc.wantCode)
			}
			if tc.wantCode == store.FailureCancelled && (parent.Status != store.RunStatusCancelled || !errors.Is(err, ErrRunCancelled)) {
				t.Fatalf("parent = %s, err = %v, want cancelled", parent.Status, err)
			}
			if tc.wantWords != "" && !strings.Contains(parent.Error, tc.wantWords) {
				t.Fatalf("parent error %q lost the child's words %q", parent.Error, tc.wantWords)
			}
		})
	}
}

// TestSubbotParentDeadlineKeepsTimeout: the parent's deadline reaching a
// running child ends the parent failed_resumable TIMEOUT through the
// parent's own handler, with the child's words in its record.
func TestSubbotParentDeadlineKeepsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	exec := &hookChildExecutor{stubExecutor: newStubExecutor(), hook: func(c context.Context) error { <-c.Done(); return c.Err() }}
	// The child's words are whatever it returned when the deadline cut it —
	// inside its node, or earlier in its own setup — read from the runner.
	var childErr error
	parent, err := runParentOverChild(t, ctx, parentSubbotWorkflow(), exec, func(e error) { childErr = e })
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeTimeout {
		t.Fatalf("parent Run error = %v, want TIMEOUT", err)
	}
	if parent.Status != store.RunStatusFailedResumable || parent.FailureCode != store.FailureTimeout {
		t.Fatalf("parent = %s/%q, want failed_resumable/TIMEOUT", parent.Status, parent.FailureCode)
	}
	if childErr == nil || !strings.Contains(parent.Error, childErr.Error()) {
		t.Fatalf("parent error %q lost the child's words %v", parent.Error, childErr)
	}
}
