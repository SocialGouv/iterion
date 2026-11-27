package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The retry sweeper's re-admission of a usage-window park (ADR-167 T4a, Q2
// option a): the bot's current program first; the program the park froze
// only when the current one is refused because its source changed — a second
// resume in the same pass, on the same admission.

// sourceChangeResumer refuses a resume of the bot's current program for its
// changed source and accepts the frozen one, recording every resume.
type sourceChangeResumer struct {
	mu      sync.Mutex
	calls   []runview.ResumeSpec
	current error // the current program's answer
	frozen  error // the frozen program's answer
}

func (f *sourceChangeResumer) Resume(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec)
	err := f.current
	if spec.Frozen != nil {
		err = f.frozen
	}
	if err != nil {
		return nil, err
	}
	return &runview.LaunchResult{RunID: spec.RunID}, nil
}

func (f *sourceChangeResumer) recorded() []runview.ResumeSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runview.ResumeSpec(nil), f.calls...)
}

// usageFrozenDue is frozenDue for a usage-window park: the record names the
// usage-window source and the row waits on the usage window.
func usageFrozenDue(t *testing.T, st *frozenRetryStore, id string) (*store.Run, mongostore.RetryDueRef) {
	t.Helper()
	run, ref := frozenDue(t, st, id, "")
	run.FailureCode = store.FailureUsageLimitBlocked
	run.FrozenProgram.Source = store.FrozenProgramSourceUsageWindow
	ref.RetryState.Reason, ref.RetryState.Code = store.RetryReasonUsageWindow, "USAGE_LIMIT_BLOCKED"
	return run, ref
}

func sweepWith(s *Server, resumer runResumer, ref mongostore.RetryDueRef) {
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, s.retryNow())
}

// F30. A usage park's bound record: when the bot's program is unchanged the
// run resumes it — the current path, one resume, no frozen program — and when
// it changed, the current program's refusal is followed in the same pass by
// the frozen program's resume, unforced, on the admission the first was
// judged under, and that one is the pass's result. Red when a usage record is
// always re-admitted frozen (the unchanged case carries it), when it is never
// re-admitted frozen (the changed case is re-armed), or when the second
// resume is judged under another admission.
func TestSweepFrozen_AUsageParkResumesItsCurrentProgramUnlessItChanged(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		s, st := newFrozenSweeper(t)
		_, ref := usageFrozenDue(t, st, "run-usage")
		resumer := &sourceChangeResumer{}
		sweepWith(s, resumer, ref)
		calls := resumer.recorded()
		if len(calls) != 1 || calls[0].Frozen != nil || calls[0].FilePath == "" {
			t.Fatalf("resumes %+v, want one on the current program (a file, no frozen program)", calls)
		}
		if len(st.cleared) != 1 || retryResult(t, s, retryResultFrozenReadmitted) != 0 {
			t.Fatalf("cleared %v, frozen_readmitted %v, want the current program resumed and nothing frozen counted", st.cleared, retryResult(t, s, retryResultFrozenReadmitted))
		}
	})
	t.Run("changed", func(t *testing.T) {
		s, st := newFrozenSweeper(t)
		run, ref := usageFrozenDue(t, st, "run-usage")
		resumer := &sourceChangeResumer{current: runtime.ValidateResumeWorkflowHash("run-usage", "h1", "h2", false)}
		sweepWith(s, resumer, ref)
		calls := resumer.recorded()
		if len(calls) != 2 {
			t.Fatalf("Resume called %d times, want the current program then the frozen one", len(calls))
		}
		if calls[0].Frozen != nil || calls[0].FilePath == "" {
			t.Fatalf("first resume %+v, want the current program", calls[0])
		}
		second := calls[1]
		if second.Frozen == nil || second.Frozen.Envelope == nil || second.Frozen.Program.Key != run.FrozenProgram.Key {
			t.Fatalf("second resume carries %+v, want the usage park's frozen program", second.Frozen)
		}
		if second.FilePath != "" || second.Source != "" || second.Force || !second.Automatic {
			t.Fatalf("second resume file %q source %q force %v automatic %v, want the frozen program alone, unforced, automatic", second.FilePath, second.Source, second.Force, second.Automatic)
		}
		if second.Admitted == nil || second.Admitted != calls[0].Admitted {
			t.Fatalf("second resume admitted %p, first %p: want the one admission of the pass", second.Admitted, calls[0].Admitted)
		}
		if len(st.cleared) != 1 || retryResult(t, s, retryResultFrozenReadmitted) != 1 {
			t.Fatalf("cleared %v, frozen_readmitted %v, want the frozen program resumed and counted", st.cleared, retryResult(t, s, retryResultFrozenReadmitted))
		}
		if _, paid := st.armBudget["run-usage"]; paid {
			t.Fatal("a resumed run was re-armed against its budget")
		}
	})
}

// A refusal of the current program that is not a source change keeps its
// own handling: the frozen program is no answer to a publish blip. Red when
// any refusal of the current program falls back to the frozen one.
func TestSweepFrozen_AUsageParkFallsBackOnlyOnASourceChange(t *testing.T) {
	s, st := newFrozenSweeper(t)
	_, ref := usageFrozenDue(t, st, "run-usage")
	resumer := &sourceChangeResumer{current: errors.New("nats: publish timeout")}
	sweepWith(s, resumer, ref)
	if calls := resumer.recorded(); len(calls) != 1 || calls[0].Frozen != nil {
		t.Fatalf("resumes %+v, want the current program's alone", calls)
	}
	if _, ok := st.armBudget["run-usage"]; !ok {
		t.Fatal("the publish refusal was not re-armed")
	}
}

// The bot changed but the frozen program cannot be had: an envelope that is
// gone is counted and the current program's refusal stands (re-armed, as
// before any freeze); an envelope the store could not read re-arms on that
// error, nothing resumed. Red when a missing envelope is taken for a store
// error, or the reverse.
func TestSweepFrozen_AUsageParkWithoutItsEnvelopeKeepsTheRefusal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spoil       func(st *frozenRetryStore, r *store.Run)
		unavailable float64
	}{
		{"envelope gone", func(st *frozenRetryStore, r *store.Run) { delete(st.blobs, r.FrozenProgram.Key) }, 1},
		{"store failure", func(st *frozenRetryStore, _ *store.Run) { st.getErr = errors.New("s3: connection reset") }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := newFrozenSweeper(t)
			run, ref := usageFrozenDue(t, st, "run-usage")
			tc.spoil(st, run)
			resumer := &sourceChangeResumer{current: runtime.ValidateResumeWorkflowHash("run-usage", "h1", "h2", false)}
			sweepWith(s, resumer, ref)
			if calls := resumer.recorded(); len(calls) != 1 || calls[0].Frozen != nil {
				t.Fatalf("resumes %+v, want the current program's alone", calls)
			}
			if _, ok := st.armBudget["run-usage"]; !ok {
				t.Fatal("the run was not re-armed")
			}
			if len(st.cleared) != 0 {
				t.Fatalf("cleared %v, want nothing resumed", st.cleared)
			}
			if n := retryResult(t, s, retryResultFrozenUnavailable); n != tc.unavailable {
				t.Fatalf("frozen_unavailable %v, want %v", n, tc.unavailable)
			}
		})
	}
}
