package trigger

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// TestWatchdogMoveDoesNotSpendAOneShot: a one-shot label gate is an
// operator's single pull of a trigger. The claim watchdog moves cards
// too — repairing what a dead owner left behind — and spending the gate
// there burns an intent nobody expressed, on a card nobody touched.
// Re-arming it is manual, so waiting does not recover it.
func TestWatchdogMoveDoesNotSpendAOneShot(t *testing.T) {
	machine := Event{
		Source: SourceBoard, Kind: KindCardMoved, TenantID: "t1",
		Subject: Subject{Type: "card", ID: "c1"},
		Labels:  []string{"deploy:once"},
		Payload: map[string]any{"from_state": "in_progress", "reason": tracker.ReasonWatchdog},
	}
	human := machine
	human.Payload = map[string]any{"from_state": "in_progress"}

	for _, tc := range []struct {
		name      string
		ev        Event
		wantSpent bool
	}{
		{"a watchdog repair must not spend it", machine, false},
		{"an operator move still does", human, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			board := &recordingLabelConsumer{}
			e := &Evaluator{board: board, launcher: board}
			sub := Subscription{
				ID: "s1", ConsumeLabels: true,
				Match: Matcher{Labels: []string{"deploy:once"}},
			}
			_ = e.applyEffect(context.Background(), sub, tc.ev, effectOpts{})
			if board.consumed != tc.wantSpent {
				t.Fatalf("one-shot spent = %t, want %t (reason=%v)",
					board.consumed, tc.wantSpent, tc.ev.Payload["reason"])
			}
			if board.launched != tc.wantSpent {
				t.Fatalf("launched = %t, want %t", board.launched, tc.wantSpent)
			}
		})
	}
}

type recordingLabelConsumer struct {
	consumed bool
	launched bool
}

func (r *recordingLabelConsumer) Promote(context.Context, LaunchPlan) (string, error) { return "", nil }
func (r *recordingLabelConsumer) ConsumeMatchLabels(_ context.Context, _, _ string, _ []string) (bool, error) {
	r.consumed = true
	return true, nil
}
func (r *recordingLabelConsumer) Launch(context.Context, LaunchPlan) (string, error) {
	r.launched = true
	return "run-1", nil
}
