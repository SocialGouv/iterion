package dispatcher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The counterpart of the deterministic give-up arm: the failures a
// re-execution CAN cure keep the bounded backoff ladder. The next sample of
// an agent's output may conform, and a transient backend fault is exactly
// what the backoff exists for — so narrowing the arm past its class would
// turn recoverable work into blocked cards.
//
// (The give-up half — the optimistic guard and the `failed_state: none`
// opt-out — is owned by TestFinishRun_DeterministicGiveUpKeepsTheGuard…,
// which drives the off-actor worker and reads the board.)
func TestFinishRun_RecoverableFailuresKeepTheRetryLadder(t *testing.T) {
	ft := newFakeTracker()
	c := newTestDispatcher(t, &StubRunner{}, ft, time.Hour)

	for _, tc := range []struct {
		id  string
		err error
	}{
		{"fake:2", &runtime.RuntimeError{Code: store.FailureExecutionFailed, Message: "upstream 502"}},
		{"fake:3", &runtime.RuntimeError{Code: store.FailureSchemaValidation, NodeID: "plan", Message: "output does not match schema"}},
		{"fake:4", fmt.Errorf("boom")},
	} {
		c.state.running[tc.id] = &runningEntry{IssueID: tc.id, Identifier: tc.id, RunID: "r-" + tc.id, WorkflowState: "ready"}
		c.finishRun(context.Background(), tc.id, tc.err)
		if _, ok := c.state.retries[tc.id]; !ok {
			t.Errorf("%v no longer schedules a retry — the recoverable classes lost their backoff ladder", tc.err)
		}
	}
	c.workersWG.Wait()
}
