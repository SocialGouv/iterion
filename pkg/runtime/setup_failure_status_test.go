package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Setup runs on the same ctx the node loop does, so the same two
// interruptions reach it — and mean the same thing. Marked terminal, a
// drained run cannot be resumed at all, and a review that owed a merge gate
// never gets to post one.
func TestSetupFailureStatus(t *testing.T) {
	boom := errors.New("kubectl exec: signal: killed")

	interrupted, cancelInterrupted := context.WithCancelCause(context.Background())
	cancelInterrupted(ErrRunInterrupted)
	operator, cancelOperator := context.WithCancel(context.Background())
	cancelOperator()

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		want   store.RunStatus
		reason string
	}{
		{"a runner drain is resumable", interrupted, store.RunStatusFailedResumable, "interrupted"},
		{"an operator cancel says so", operator, store.RunStatusCancelled, "cancelled"},
		{"a real setup error still fails", context.Background(), store.RunStatusFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := setupFailureStatus(tc.ctx, "sandbox start", boom)
			if got != tc.want {
				t.Fatalf("status = %q, want %q (msg %q)", got, tc.want, msg)
			}
			if !strings.Contains(msg, "sandbox start") || !strings.Contains(msg, boom.Error()) {
				t.Errorf("the message must keep the phase and the cause; got %q", msg)
			}
		})
	}
}

// The status and the error have to agree: the runner NAKs on
// ErrRunInterrupted (redelivered to a healthy pod) and ACKs otherwise, so a
// run recorded resumable whose error lost the marker would be dropped by the
// queue and never come back.
func TestSetupErr_KeepsTheInterruptionMarker(t *testing.T) {
	e := &Engine{}
	inner := errors.New("worktree half-written")

	drained, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunInterrupted)
	if err := e.setupErr(drained, inner); !errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("a drained setup must stay recognisable to the runner, got %v", err)
	}
	if err := e.setupErr(context.Background(), inner); errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("an ordinary setup error must not claim an interruption: %v", err)
	}
}

// The composition: the engine driven with a drained ctx must leave the run
// resumable in the STORE and return an error the runner will nak.
func TestEngineRun_SetupDrainLeavesTheRunResumable(t *testing.T) {
	s := tmpStore(t)
	eng := New(devboxTestWorkflow(), s, newStubExecutor(),
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunInterrupted)

	const runID = "run-drained-in-setup"
	err := eng.Run(ctx, runID, nil)
	if err == nil {
		t.Fatal("a drained run must not report success")
	}
	if !errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("the runner naks on ErrRunInterrupted; got %v", err)
	}
	run, lerr := s.LoadRun(context.Background(), runID)
	if lerr != nil {
		t.Fatalf("load run: %v", lerr)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %q, want failed_resumable (a drain is not a workflow failure)", run.Status)
	}
}
