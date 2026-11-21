package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPreEngineRefusalHonorsOriginalFormat(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreateRunWithValueFormat(context.Background(), RunCreateSpec{ID: "r", WorkflowName: "w", Format: ExactValueFormat, Status: RunStatusQueued})
	if e != nil {
		t.Fatal(e)
	}
	stale, e := WithRunValueFormat(context.Background(), RunValueFormat{RunID: r.ID, Format: LegacyValueFormat})
	if e != nil {
		t.Fatal(e)
	}
	result, e := s.RecordPreEngineRefusal(stale, r.ID, PreEngineRefusalRequest{Phase: "executor", Code: FailureExecutionFailed, Message: "stale invocation", PublishedAt: time.Now().Add(time.Second), Disposition: PreEngineRetryPending})
	after, load := s.LoadRun(context.Background(), r.ID)
	if load != nil {
		t.Fatal(load)
	}
	if result.Changed || !errors.Is(e, ErrValueFormatChanged) || after.PreEngineRefusal != nil {
		t.Fatalf("stale format mutated exact run: result=%+v err=%v refusal=%+v", result, e, after.PreEngineRefusal)
	}
}
