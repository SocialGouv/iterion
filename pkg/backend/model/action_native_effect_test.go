package model_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
)

// What a native board write's failure means for its effect (ADR-151 §4):
// both stores commit the card, then write its event, and fail AFTER the
// commit with an error — so a failed store write is undecided, and the
// engine must never re-run the node that filed it. The oracle is the
// engine's REAL recovery dispatcher, driven the way the engine drives it:
// re-run while it says RetrySameNode, counting the attempts per code.

// commitThenFail files the card — the delegated Create commits — and then
// answers the error the Mongo store answers when the event after the commit
// cannot be written.
type commitThenFail struct {
	native.BoardStore
	message string
}

func (s commitThenFail) Create(in native.Issue) (*native.Issue, error) {
	if _, err := s.BoardStore.Create(in); err != nil {
		return nil, err
	}
	return nil, errors.New(s.message)
}

// listFails fails a READ: nothing was written, so the failure is decided.
type listFails struct{ native.BoardStore }

func (listFails) List(native.ListFilter) ([]*native.Issue, error) {
	return nil, errors.New("boardmongo: list issues: connection reset by peer")
}

// dispatchUntilSettled runs the node, then re-runs it for as long as the
// recovery dispatcher answers RetrySameNode — the engine's loop, bounded. It
// returns the attempts made and the final dispatch.
func dispatchUntilSettled(t *testing.T, e *model.ClawExecutor, node ir.Node) (int, runtime.RecoveryAction, runtime.ErrorCode) {
	t.Helper()
	dispatch := recovery.Dispatch(recovery.DefaultRecipes())
	prior := map[runtime.ErrorCode]int{}
	for attempt := 1; attempt <= 10; attempt++ {
		_, err := e.Execute(context.Background(), node, nil)
		if err == nil {
			t.Fatalf("attempt %d: the node succeeded — the fake store did not fail", attempt)
		}
		action, code := dispatch(context.Background(), err, func(c runtime.ErrorCode) int { return prior[c] })
		if action.Kind != runtime.RecoveryRetrySameNode {
			return attempt, action, code
		}
		prior[code]++
	}
	t.Fatal("the dispatcher retried ten times")
	return 0, runtime.RecoveryAction{}, ""
}

func countCards(t *testing.T, store native.BoardStore) int {
	t.Helper()
	issues, err := store.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(issues)
}

// The two wordings the reviewer's probe drove: a transport needle (the
// network backoff would re-run it six times) and a message no needle
// matches (the catch-all would re-run it once). Either way one plan item
// must leave ONE card.
func TestANativeWriteThatCommittedIsNeverRetried(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"boardmongo: emit event: connection reset by peer",
		"native store: append event: disk quota exceeded",
	} {
		t.Run(message, func(t *testing.T) {
			t.Parallel()
			base := newBoardStore(t)
			e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
				model.WithActionTransports(boardops.NewActionTransport(commitThenFail{BoardStore: base, message: message}, boardops.CallEnv{})))
			node := boardActionNode("board.issue.create", []string{"board.create"})
			node.Params = []ir.ActionParam{{Key: "title", Value: "Critère 1.1 — images"}}

			attempts, action, code := dispatchUntilSettled(t, e, node)
			if cards := countCards(t, base); cards != 1 {
				t.Fatalf("%d card(s) on the board after %d attempt(s) — a write that committed was re-run", cards, attempts)
			}
			if attempts != 1 {
				t.Fatalf("the node ran %d times, want once", attempts)
			}
			if code != runtime.ErrCodeAmbiguousEffect || action.Kind != runtime.RecoveryFailTerminal {
				t.Fatalf("dispatch = (%v, %q), want a terminal AMBIGUOUS_EFFECT", action.Kind, code)
			}
		})
	}
}

// The falsifier: a blanket "native board failures never retry" rule would
// pass the test above while making every transient read blip terminal. A
// failed READ wrote nothing, so the network backoff still applies.
func TestANativeReadFailureStillRetries(t *testing.T) {
	t.Parallel()
	base := newBoardStore(t)
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithActionTransports(boardops.NewActionTransport(listFails{base}, boardops.CallEnv{})))
	_, err := e.Execute(context.Background(), boardActionNode("board.issue.list", []string{"board.read"}), nil)
	if err == nil {
		t.Fatal("the read must fail")
	}
	if runtime.IsAmbiguousEffect(err) {
		t.Fatalf("a failed read is reported undecided: %v", err)
	}
	action, code := recovery.Dispatch(recovery.DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
	if action.Kind != runtime.RecoveryRetrySameNode || code != runtime.ErrCodeNetworkTransient {
		t.Fatalf("dispatch = (%v, %q), want a NETWORK_TRANSIENT retry", action.Kind, code)
	}
}

// The run's redaction must not erase the fact: a store error quoting a
// materialised secret is rewritten for the operator's sinks, and the
// rewrite must still answer as an undecided write — flattened to a string,
// it would be handed to the catch-all retry.
func TestARedactedNativeWriteFailureStaysUndecided(t *testing.T) {
	t.Parallel()
	const token = "synthetic-board-secret-2834"
	base := newBoardStore(t)
	guard := secretguard.New([]secretguard.Secret{{Name: "BOARD_PROBE", Value: token}}, secretguard.DefaultConfig())
	e := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{},
		model.WithSecretGuard(guard),
		model.WithActionTransports(boardops.NewActionTransport(commitThenFail{BoardStore: base, message: "native store: append event for " + token + ": no space left"}, boardops.CallEnv{})))
	node := boardActionNode("board.issue.create", []string{"board.create"})
	node.Params = []ir.ActionParam{{Key: "title", Value: "filed once"}}

	_, err := e.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("the create must fail")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the secret survived redaction: %v", err)
	}
	if !runtime.IsAmbiguousEffect(err) {
		t.Fatalf("redaction erased the undecided write (%T: %v) — the recovery would re-run a node whose card is filed", err, err)
	}
	if code := recovery.Classify(err); code != runtime.ErrCodeAmbiguousEffect {
		t.Fatalf("Classify = %q, want %q", code, runtime.ErrCodeAmbiguousEffect)
	}
}
