package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The wrapper changes only timing and one selected I/O result. Format refusals
// always come from the real FS store, after public delete/prune/create calls.
type checkpointFailurePublicStore struct {
	store.RunStore
	t             *testing.T
	bound         context.Context
	format        store.ValueFormat
	stage, mode   string
	seen          map[string]int
	successor     *store.Run
	actualRefusal error
}

func (s *checkpointFailurePublicStore) Unwrap() store.RunStore { return s.RunStore }
func (s *checkpointFailurePublicStore) before(c context.Context, stage string) error {
	s.seen[stage]++
	checkTerminationContext(s.t, c, s.format)
	if s.seen[stage] != 1 {
		s.t.Fatalf("REPEATED_TERMINAL_CALLBACK %s %v", stage, s.seen)
	}
	target := s.stage
	if target == "fallback-event" {
		target = "event"
	}
	if stage == "resumable" && (s.stage == "fallback" || s.stage == "fallback-event") {
		return errors.New("first ordinary checkpoint failure")
	}
	if stage == target {
		switch s.mode {
		case "swap":
			s.successor = terminationCallbackReplacement(s.t, s.RunStore, s.bound, 3-s.format)
		case "ordinary":
			return errors.New("selected ordinary storage failure")
		}
	}
	return nil
}
func (s *checkpointFailurePublicStore) observe(err error) {
	if store.IsValueFormatRefusal(err) {
		s.actualRefusal = err
	}
}
func (s *checkpointFailurePublicStore) FailRunResumable(c context.Context, id string, cp *store.Checkpoint, msg string, code store.FailureCode) error {
	if err := s.before(c, "resumable"); err != nil {
		return err
	}
	err := s.RunStore.FailRunResumable(c, id, cp, msg, code)
	s.observe(err)
	return err
}
func (s *checkpointFailurePublicStore) UpdateRunStatusCoded(c context.Context, id string, status store.RunStatus, msg string, code store.FailureCode) error {
	if err := s.before(c, "fallback"); err != nil {
		return err
	}
	err := s.RunStore.UpdateRunStatusCoded(c, id, status, msg, code)
	s.observe(err)
	return err
}
func (s *checkpointFailurePublicStore) AppendEvent(c context.Context, id string, ev store.Event) (*store.Event, error) {
	if ev.Type == store.EventRunFailed {
		if err := s.before(c, "event"); err != nil {
			return nil, err
		}
	}
	out, err := s.RunStore.AppendEvent(c, id, ev)
	if ev.Type == store.EventRunFailed {
		s.observe(err)
	}
	return out, err
}
func TestCheckpointFailurePublicMatrix(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		for _, kind := range []string{"plain", "business", "ambiguous"} {
			for _, stage := range []string{"resumable", "fallback", "event", "fallback-event"} {
				for _, mode := range []string{"healthy", "ordinary", "swap"} {
					t.Run(fmt.Sprintf("format%d/%s/%s/%s", format, kind, stage, mode), func(t *testing.T) {
						base, bound := terminationFixture(t, format)
						ctx, cancel := context.WithCancel(bound)
						defer cancel()
						s := &checkpointFailurePublicStore{RunStore: base, t: t, bound: bound, format: format, stage: stage, mode: mode, seen: map[string]int{}}
						// Only a verdict no resume can change stays the subbot
						// node's once the parent's context has ended
						// (runSubbotNode); a resumable one is the parent's
						// cancellation (TestSubbotChildFailureRacingParentCancel).
						// The door is exercised with deterministic verdicts.
						code := ErrCodeToolFailedPermanent
						var original error = &RuntimeError{Code: code, Message: "plain terminal failure"}
						switch kind {
						case "business":
							code = ErrCodeLoopExhausted
							original = &RuntimeError{Code: ErrCodeLoopExhausted, Message: "established loop verdict"}
						case "ambiguous":
							code = ErrCodeAmbiguousEffect
							original = &RuntimeError{Code: code, Message: "effect outcome unknown"}
						}
						x := newStubExecutor()
						wf := terminationProbeWorkflow()
						wf.Nodes["step"] = &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "step"}, Source: "fixture-child.bot"}
						callbackCalls := 0
						err := New(wf, s, x, WithSubbotRunner(func(context.Context, SubbotRequest) (map[string]any, error) {
							callbackCalls++
							cancel()
							return nil, original
						})).Run(ctx, "stop", nil)
						if callbackCalls != 1 {
							t.Fatalf("MISSED_SUBBOT_CALLBACK calls=%d err=%v", callbackCalls, err)
						}
						got, readErr := base.LoadRun(t.Context(), "stop")
						if readErr != nil {
							t.Fatal(readErr)
						}
						if s.seen["resumable"] != 1 {
							t.Fatalf("MISSED_CHECKPOINT_DOOR %v err=%v", s.seen, err)
						}
						if mode == "swap" {
							if !errors.Is(s.actualRefusal, store.ErrValueFormatChanged) || !errors.Is(err, store.ErrValueFormatChanged) || !reflect.DeepEqual(got, s.successor) {
								t.Fatalf("CHECKPOINT_REFUSAL_LOST err=%v actual=%v unchanged=%v callbacks=%v", err, s.actualRefusal, reflect.DeepEqual(got, s.successor), s.seen)
							}
							if stage == "resumable" && (s.seen["fallback"] != 0 || s.seen["event"] != 0) || stage == "fallback" && s.seen["event"] != 0 {
								t.Fatalf("CHECKPOINT_CALLBACK_AFTER_REFUSAL %v", s.seen)
							}
							if kind != "plain" && !errors.Is(err, original) {
								t.Fatalf("CHECKPOINT_VERDICT_LOST %v", err)
							}
						} else {
							want := store.RunStatusFailedResumable
							if stage == "fallback" || stage == "fallback-event" || mode == "ordinary" && stage == "resumable" {
								want = store.RunStatusFailed
							}
							if mode == "ordinary" && stage == "fallback" {
								want = store.RunStatusRunning
							}
							if got.Status != want || store.IsValueFormatRefusal(err) {
								t.Fatalf("CHECKPOINT_POLICY_CHANGED status=%s want=%s err=%v calls=%v", got.Status, want, err, s.seen)
							}
							if want != store.RunStatusRunning && got.FailureCode != code {
								t.Fatalf("CHECKPOINT_CODE_CHANGED got=%s want=%s err=%v", got.FailureCode, code, err)
							}
							if want == store.RunStatusFailedResumable && got.Checkpoint == nil {
								t.Fatal("CHECKPOINT_LOST")
							}
						}
						t.Logf("format=%d kind=%s stage=%s mode=%s status=%s code=%s callbacks=%v err=%v", format, kind, stage, mode, got.Status, got.FailureCode, s.seen, err)
					})
				}
			}
		}
	}
}
