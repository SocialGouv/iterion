package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// queuedResume is a run the publisher flipped from `from` to queued for a
// resume, and the message it published for that attempt.
func queuedResume(t *testing.T, from store.RunStatus, prior store.RunStatus) (store.RunStore, *queue.RunMessage) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "run-refused-resume"
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	run := &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: from,
		Checkpoint: &store.Checkpoint{NodeID: "gate", InteractionID: "run-refused-resume_gate"}}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpdateRunStatusIf(ctx, id, store.RunStatusQueued, "", []store.RunStatus{from}); err != nil || !ok {
		t.Fatalf("the publisher's queued flip: ok=%v err=%v", ok, err)
	}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1",
		PublishedAtRFC: time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
		Resume:         &queue.ResumeSpec{PriorStatus: prior}}
	return st, msg
}

func refusal() error {
	return &runtime.RuntimeError{Code: runtime.ErrCodeResumeInvalid, Message: "run executed in its parent run's copy-based sandbox"}
}

// TestReleaseRefusedResume_putsTheRunBackWhereItCameFrom: a resume the
// engine refused before its claim leaves the run where it was — a paused
// run keeps its pending question — with the refusal in its error, instead
// of queued until the orphan sweeper.
func TestReleaseRefusedResume_putsTheRunBackWhereItCameFrom(t *testing.T) {
	for _, from := range []store.RunStatus{store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator, store.RunStatusFailedResumable, store.RunStatusCancelled} {
		t.Run(string(from), func(t *testing.T) {
			st, msg := queuedResume(t, from, from)
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			if got := r.releaseRefusedResume(msg, refusal(), iterlog.Nop()); got != from {
				t.Fatalf("released to %q, want %q", got, from)
			}
			run, err := st.LoadRun(context.Background(), msg.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != from || !strings.Contains(run.Error, "copy-based sandbox") {
				t.Fatalf("run is %s with error %q, want %s carrying the refusal", run.Status, run.Error, from)
			}
			if from == store.RunStatusPausedWaitingHuman && (run.Checkpoint == nil || run.Checkpoint.InteractionID == "") {
				t.Fatal("the paused run lost its pending question")
			}
			want := runtime.ErrCodeResumeInvalid
			if from == store.RunStatusCancelled {
				want = store.FailureCancelled // nobody cancelled anything anew
			}
			if run.Status.CarriesFailureCode() && run.FailureCode != want {
				t.Fatalf("failure code %q, want %q", run.FailureCode, want)
			}
		})
	}
}

// TestReleaseRefusedResume_anOlderPublisherSaysNothing: a message from a
// publisher that predates PriorStatus puts the run in failed_resumable.
func TestReleaseRefusedResume_anOlderPublisherSaysNothing(t *testing.T) {
	st, msg := queuedResume(t, store.RunStatusPausedOperator, "")
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	if got := r.releaseRefusedResume(msg, refusal(), iterlog.Nop()); got != store.RunStatusFailedResumable {
		t.Fatalf("released to %q, want failed_resumable", got)
	}
}

// TestReleaseRefusedResume_leavesWhatItDoesNotOwn: the doc decides — a run
// the engine claimed, a newer resume's attempt and a launch are left alone.
func TestReleaseRefusedResume_leavesWhatItDoesNotOwn(t *testing.T) {
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	t.Run("claimed", func(t *testing.T) {
		st, msg := queuedResume(t, store.RunStatusFailedResumable, store.RunStatusFailedResumable)
		if ok, err := st.UpdateRunStatusIf(ctx, msg.RunID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
		if got := r.releaseRefusedResume(msg, refusal(), iterlog.Nop()); got != "" {
			t.Fatalf("a claimed run was released to %q", got)
		}
		if run, _ := st.LoadRun(ctx, msg.RunID); run.Status != store.RunStatusRunning {
			t.Fatalf("a claimed run is %s", run.Status)
		}
	})
	t.Run("newer attempt", func(t *testing.T) {
		st, msg := queuedResume(t, store.RunStatusFailedResumable, store.RunStatusFailedResumable)
		msg.PublishedAtRFC = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
		r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
		if got := r.releaseRefusedResume(msg, refusal(), iterlog.Nop()); got != "" {
			t.Fatalf("an older delivery released a newer attempt to %q", got)
		}
		if run, _ := st.LoadRun(ctx, msg.RunID); run.Status != store.RunStatusQueued {
			t.Fatalf("the newer attempt is %s, want queued", run.Status)
		}
	})
	t.Run("launch", func(t *testing.T) {
		st, msg := queuedResume(t, store.RunStatusFailedResumable, store.RunStatusFailedResumable)
		msg.Resume = nil
		r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
		if got := r.releaseRefusedResume(msg, refusal(), iterlog.Nop()); got != "" {
			t.Fatalf("a launch was released to %q", got)
		}
	})
}

// TestRunnerVerdicts_leaveAResumeToTheRelease: the runner's own refusals of
// a bundle — an IR it cannot load, an engine floor it is below — leave a
// resume queued for the release, which puts it back where it came from
// with their code; a launch keeps their verdict.
func TestRunnerVerdicts_leaveAResumeToTheRelease(t *testing.T) {
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	for _, tc := range []struct {
		name  string
		err   error
		code  store.FailureCode
		write func(r *Runner, msg *queue.RunMessage, err error)
	}{
		{"ir unloadable", fmt.Errorf("runner: %w: decode IR: bad", ErrIRUnloadable), store.FailureIRUnloadable,
			func(r *Runner, msg *queue.RunMessage, err error) { r.failUnloadableIR(ctx, msg, err) }},
		{"bot requires a newer engine", fmt.Errorf("%w: bot %q: wants 9.0.0", ErrBotRequiresNewerEngine, "b"), store.FailureBotRequiresNewerEngine,
			func(r *Runner, msg *queue.RunMessage, err error) {
				r.failBotRequiresNewerEngine(ctx, msg, &bundle.Manifest{}, "v3.0.0", err)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, msg := queuedResume(t, store.RunStatusPausedWaitingHuman, store.RunStatusPausedWaitingHuman)
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			tc.write(r, msg, tc.err)
			if run, _ := st.LoadRun(ctx, msg.RunID); run.Status != store.RunStatusQueued {
				t.Fatalf("the verdict writer moved a resume to %s before the release", run.Status)
			}
			if n := runFailedEvents(t, st, msg.RunID); n != 0 {
				t.Fatalf("the verdict writer announced %d failure(s) for a resume left to the release", n)
			}
			if got := r.releaseRefusedResume(msg, tc.err, iterlog.Nop()); got != store.RunStatusPausedWaitingHuman {
				t.Fatalf("released to %q, want paused_waiting_human", got)
			}
			if run, _ := st.LoadRun(ctx, msg.RunID); run.Checkpoint == nil || run.Checkpoint.InteractionID == "" {
				t.Fatal("the paused run lost its pending question")
			}

			st, msg = queuedResume(t, store.RunStatusFailedResumable, store.RunStatusFailedResumable)
			r = &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			tc.write(r, msg, tc.err)
			if got := r.releaseRefusedResume(msg, tc.err, iterlog.Nop()); got != store.RunStatusFailedResumable {
				t.Fatalf("released to %q, want failed_resumable", got)
			}
			if run, _ := st.LoadRun(ctx, msg.RunID); run.FailureCode != tc.code {
				t.Fatalf("failure code %q, want %q", run.FailureCode, tc.code)
			}

			st, msg = queuedResume(t, store.RunStatusFailedResumable, "")
			msg.Resume = nil
			r = &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			tc.write(r, msg, tc.err)
			if run, _ := st.LoadRun(ctx, msg.RunID); run.Status == store.RunStatusQueued || run.FailureCode != tc.code {
				t.Fatalf("a launch lost the runner's verdict: %s %q", run.Status, run.FailureCode)
			}
			if n := runFailedEvents(t, st, msg.RunID); n != 1 {
				t.Fatalf("a launch's verdict announced %d failure(s), want 1", n)
			}

			// A resume whose run is no longer queued — an orphan adopted and
			// promoted, a redelivery after a nak — takes the verdict: the
			// release has nothing to move.
			st, msg = queuedResume(t, store.RunStatusFailedResumable, store.RunStatusFailedResumable)
			if _, err := st.UpdateRunOutcome(ctx, msg.RunID, store.RunStatusFailedResumable, "orphaned",
				store.RunOutcomeMeta{Code: store.FailureProcessOrphaned, Continuation: store.ContinuationRedeliveryPending},
				[]store.RunStatus{store.RunStatusQueued}); err != nil {
				t.Fatal(err)
			}
			r = &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			tc.write(r, msg, tc.err)
			if got := r.releaseRefusedResume(msg, tc.err, iterlog.Nop()); got != "" {
				t.Fatalf("released a run the queue no longer holds to %q", got)
			}
			if run, _ := st.LoadRun(ctx, msg.RunID); run.FailureCode != tc.code || run.ContinuationState != store.ContinuationFinal {
				t.Fatalf("a resume of an adopted orphan lost the runner's verdict: %s %q %q", run.Status, run.FailureCode, run.ContinuationState)
			}
			if n := runFailedEvents(t, st, msg.RunID); n != 1 {
				t.Fatalf("an adopted orphan's verdict announced %d failure(s), want 1", n)
			}
		})
	}
}

// TestParksOnDLQ_onlyWhatTheQueueWouldRedeliver: the final delivery parks a
// failure the queue would have redelivered, never a verdict the runner acks.
func TestParksOnDLQ_onlyWhatTheQueueWouldRedeliver(t *testing.T) {
	if !parksOnDLQ(errors.New("the provider went away"), "run-1") {
		t.Error("a plain failure on the final delivery is not parked")
	}
	for _, err := range []error{
		refusal(),
		fmt.Errorf("runner: %w: decode IR: bad", ErrIRUnloadable),
		fmt.Errorf("%w: bot %q: wants 9.0.0", ErrBotRequiresNewerEngine, "b"),
		runtime.ErrRunInterrupted,
		nil,
	} {
		if parksOnDLQ(err, "run-1") {
			t.Errorf("%v is parked on the DLQ, want it left to its own verdict", err)
		}
	}
}

func runFailedEvents(t *testing.T, st store.RunStore, runID string) int {
	t.Helper()
	evs, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Type == store.EventRunFailed {
			n++
		}
	}
	return n
}

// TestRecordsRetrySkipped: a released resume is put on the timeline, the
// runner's own verdicts included; a launch's verdict announces itself.
func TestRecordsRetrySkipped(t *testing.T) {
	for _, tc := range []struct {
		final    string
		released store.RunStatus
		want     bool
	}{
		{"deterministic_failure", "", true},
		{"deterministic_failure", store.RunStatusPausedWaitingHuman, true},
		{"ir_unloadable", store.RunStatusPausedWaitingHuman, true},
		{"bot_requires_newer_engine", store.RunStatusFailedResumable, true},
		{"ir_unloadable", "", false},
		{"ok", "", false},
	} {
		if got := recordsRetrySkipped(tc.final, tc.released); got != tc.want {
			t.Errorf("recordsRetrySkipped(%q, %q) = %v, want %v", tc.final, tc.released, got, tc.want)
		}
	}
}

// TestRecordRetrySkipped_aReleasedRunnerVerdictSaysWhatCuresIt: --force does
// not cure a runner that cannot load the IR or runs below the bot's engine:
// the hint says what does.
func TestRecordRetrySkipped_aReleasedRunnerVerdictSaysWhatCuresIt(t *testing.T) {
	for _, tc := range []struct {
		code store.FailureCode
		cure string
	}{
		{store.FailureIRUnloadable, "align the runner"},
		{store.FailureBotRequiresNewerEngine, "bump the runner image"},
		{"RESUME_INVALID", "--force"},
	} {
		st, msg := queuedResume(t, store.RunStatusPausedWaitingHuman, store.RunStatusPausedWaitingHuman)
		r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
		r.recordRetrySkipped(msg, tc.code, "refused", nil, store.RunStatusPausedWaitingHuman)
		evs, err := st.LoadEvents(context.Background(), msg.RunID)
		if err != nil {
			t.Fatal(err)
		}
		var hint string
		for _, ev := range evs {
			if ev.Type == store.EventRunRetrySkipped {
				hint, _ = ev.Data["hint"].(string)
			}
		}
		if !strings.Contains(hint, tc.cure) || !strings.Contains(hint, string(store.RunStatusPausedWaitingHuman)) {
			t.Errorf("%s: hint %q, want it to say %q and the status the run is back to", tc.code, hint, tc.cure)
		}
	}
}
