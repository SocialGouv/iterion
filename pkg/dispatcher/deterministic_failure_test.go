package dispatcher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The dispatcher's retry is the LOCAL counterpart of the cloud
// redelivery, and it read the same failures the same wrong way: a
// `compute` node whose expression cannot evaluate got the full backoff
// ladder — a workspace and a model budget per attempt — before the card
// finally moved to blocked. Nothing between two attempts could differ: a
// compute node runs no LLM and no shell, and a resume re-executes it
// against the same checkpoint.
//
// The verdict is the SAME terminal move the ceiling would have produced,
// taken on the first attempt instead of the Nth.
func TestFinishRun_DeterministicFailureGivesUpWithoutRetrying(t *testing.T) {
	ft := newFakeTracker()
	c := newTestDispatcher(t, &StubRunner{}, ft, time.Hour)
	c.state.running["fake:1"] = &runningEntry{IssueID: "fake:1", Identifier: "fake#1", RunID: "r1", WorkflowState: "ready"}
	ft.claims["fake:1"] = c.hostMarker

	c.finishRun(context.Background(), "fake:1", &runtime.RuntimeError{
		Code:    store.FailureExpressionFailed,
		NodeID:  "delivery_reserve",
		Message: `compute "delivery_reserve": expr: max() takes 2 arguments, got 3`,
	})

	if _, ok := c.state.retries["fake:1"]; ok {
		t.Fatal("a deterministic failure scheduled a retry — every attempt re-runs the same expression against the same checkpoint")
	}

	// A failure a re-execution CAN cure keeps the ladder: the next sample
	// of an agent's output may conform, and a transient backend fault is
	// exactly what the backoff exists for.
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
}
