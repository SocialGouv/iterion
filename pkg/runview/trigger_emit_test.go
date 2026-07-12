package runview

import (
	"context"
	"errors"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

type capturePublisher struct{ events []trigger.Event }

func (c *capturePublisher) Publish(_ context.Context, ev trigger.Event) error {
	c.events = append(c.events, ev)
	return nil
}

func TestEmitRunCompletionKinds(t *testing.T) {
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// No publisher → no-op (must not panic).
	svc.emitRunCompletion("run-x", nil)

	cap := &capturePublisher{}
	svc.SetEventPublisher(cap)

	cases := []struct {
		name    string
		runID   string
		bodyErr error
		want    string
	}{
		{"finished", "run-1", nil, trigger.KindRunFinished},
		{"failed", "run-2", errors.New("boom"), trigger.KindRunFailed},
		{"cancelled", "run-3", runtime.ErrRunCancelled, trigger.KindRunCancelled},
		// A pause must NOT be mislabeled run.failed. With no persisted run the
		// enrich switch can't run, so this asserts the load-resilient
		// bodyErr-branch fix specifically.
		{"paused_human", "run-4", runtime.ErrRunPaused, trigger.KindRunPaused},
		{"paused_operator", "run-5", runtime.ErrRunPausedOperator, trigger.KindRunPaused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(cap.events)
			svc.emitRunCompletion(tc.runID, tc.bodyErr)
			if len(cap.events) != before+1 {
				t.Fatalf("expected one event emitted, got %d", len(cap.events)-before)
			}
			ev := cap.events[len(cap.events)-1]
			if ev.Source != trigger.SourceRun {
				t.Fatalf("source = %q, want run", ev.Source)
			}
			if ev.Kind != tc.want {
				t.Fatalf("kind = %q, want %q", ev.Kind, tc.want)
			}
			if ev.Subject.Type != "run" || ev.Subject.ID != tc.runID {
				t.Fatalf("subject = %+v, want run/%s", ev.Subject, tc.runID)
			}
		})
	}
}
