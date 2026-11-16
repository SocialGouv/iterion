package server

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// A keyed launch whose run was already handed on is answered with that run
// before the gate: the gate is not charged and nothing launches again. A
// key still owed passes the gate and launches under its key; a launch with
// no key never reads a submission.
func TestAKeyedLaunchHandedOnIsAnsweredBeforeTheGate(t *testing.T) {
	var gated, launched, answered int
	var launchedKey string
	var handed bool
	var answerErr error
	l := &serviceLauncher{
		launch: func(_ context.Context, spec runview.LaunchSpec) (*runview.LaunchResult, error) {
			launched++
			launchedKey = spec.SubmissionKey
			return &runview.LaunchResult{RunID: "run-new"}, nil
		},
		resolveBot: func(context.Context, string, string) (*launchBot, error) {
			return &launchBot{BotID: "b", Origin: "catalog", Path: "catalog/b/main.bot", Source: aWorkflow}, nil
		},
		gate: func(context.Context, launchSubject) (*launchAdmission, *launchDenial) {
			gated++
			return &launchAdmission{}, nil
		},
		answer: func(_ context.Context, spec *runview.LaunchSpec) (*runview.LaunchResult, error) {
			answered++
			if spec.SubmissionKey != "k-1" {
				t.Errorf("the submission read for key %q, want k-1", spec.SubmissionKey)
			}
			if answerErr != nil {
				return nil, answerErr
			}
			if handed {
				return &runview.LaunchResult{RunID: "run-k", AlreadySubmitted: true}, nil
			}
			return nil, nil
		},
	}
	ctx := context.Background()
	keyed := trigger.LaunchPlan{BotID: "b", TenantID: "t1", SubmissionKey: "k-1"}

	handed = true
	if id, err := l.Launch(ctx, keyed); err != nil || id != "run-k" || gated != 0 || launched != 0 {
		t.Fatalf("a handed-on key = %q, %v (gated %d, launched %d); want its run, no gate, no launch", id, err, gated, launched)
	}

	handed = false
	if id, err := l.Launch(ctx, keyed); err != nil || id != "run-new" || gated != 1 || launched != 1 || launchedKey != "k-1" {
		t.Fatalf("an owed key = %q, %v (gated %d, launched %d, key %q); want one gated launch under its key", id, err, gated, launched, launchedKey)
	}

	answerErr = errors.New("spent")
	if _, err := l.Launch(ctx, keyed); !errors.Is(err, answerErr) || gated != 1 {
		t.Fatalf("a submission read error = %v (gated %d); want it answered, ungated", err, gated)
	}

	before := answered
	if _, err := l.Launch(ctx, trigger.LaunchPlan{BotID: "b", TenantID: "t1"}); err != nil || answered != before {
		t.Fatalf("a launch with no key read a submission (%d reads)", answered-before)
	}
}
