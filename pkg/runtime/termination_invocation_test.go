package runtime

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestTerminationForeignSettledResultDoesNotSkipThisInvocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, bound := terminationFixture(t, 2)
		ctx, cancel := context.WithTimeout(bound, time.Second)
		defer cancel()
		time.Sleep(time.Second)
		<-ctx.Done()
		s := &terminationCallbackBudgetStore{terminationPublicStore: &terminationPublicStore{RunStore: base, seen: map[string]int{}}}
		e := New(terminationProbeWorkflow(), s, newStubExecutor(), WithTerminationWriteTimeout(2*time.Second))
		a := e.newRunState("stop", nil)
		a.ctx = ctx
		b := e.newRunState("stop", nil)
		b.ctx = ctx
		first := e.handleContextDoneWithCheckpoint(a, "step", ctx.Err())
		if s.resumableCalls != 1 {
			t.Fatalf("MISSED_FIRST_HANDLER: %v", first)
		}
		_ = e.settleLoopPhaseError(b, "step", first)
		if s.resumableCalls != 2 {
			t.Fatalf("FOREIGN_SETTLED_SKIPPED_THIS_RUN calls=%d", s.resumableCalls)
		}
	})
}
