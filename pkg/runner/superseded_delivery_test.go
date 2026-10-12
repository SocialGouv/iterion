package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestDispositionForStatus_aDeliveryPublishedBeforeTheRunWasQueuedAgainIsStale:
// only a publication queues a run, so a delivery published before the run's
// QueuedAt belongs to an attempt that is over — whatever the run's status
// now, and whether it is a launch or a resume. It is dropped before any work.
// A delivery published after the marker is judged by the run's status, as
// before; a doc without the marker, or a publication time that cannot be
// read, has no identity to tell.
func TestDispositionForStatus_aDeliveryPublishedBeforeTheRunWasQueuedAgainIsStale(t *testing.T) {
	published := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	statuses := []store.RunStatus{store.RunStatusQueued, store.RunStatusRunning, store.RunStatusFailedResumable,
		store.RunStatusPausedOperator, store.RunStatusPausedWaitingHuman, store.RunStatusFinished, store.RunStatusCancelled}
	for _, status := range statuses {
		for _, resume := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resume=%v", status, resume), func(t *testing.T) {
				msg := func(publishedRFC string) *queue.RunMessage {
					m := &queue.RunMessage{RunID: "run-stale", PublishedAtRFC: publishedRFC}
					if resume {
						m.Resume = &queue.ResumeSpec{}
					}
					return m
				}
				later := published.Add(time.Minute)
				out := dispositionForStatus(msg(published.Format(time.RFC3339Nano)), &store.Run{ID: "run-stale", Status: status, QueuedAt: &later})
				assertStaleDisposition(t, status, out)
				stale := func(o preconditionOutcome) bool {
					return o.op == "ack-stale-attempt" || o.op == "nak-stale-attempt-unclaimed"
				}
				earlier := published.Add(-time.Minute)
				if out := dispositionForStatus(msg(published.Format(time.RFC3339Nano)), &store.Run{ID: "run-stale", Status: status, QueuedAt: &earlier}); stale(out) {
					t.Fatalf("the delivery of the current attempt taken as stale: %+v", out)
				}
				if out := dispositionForStatus(msg(published.Format(time.RFC3339Nano)), &store.Run{ID: "run-stale", Status: status}); stale(out) {
					t.Fatalf("a doc without a queued_at taken as stale: %+v", out)
				}
				if out := dispositionForStatus(msg("yesterday"), &store.Run{ID: "run-stale", Status: status, QueuedAt: &later}); stale(out) {
					t.Fatalf("a publication time that cannot be read taken as stale: %+v", out)
				}
			})
		}
	}
}

// TestSupersededUnderLock_readsTheRunAsItIsNow: once the lock is held, the
// run is read again and the identity rule applied to what it says now — a
// newer attempt queued after the admission read drops the delivery.
func TestSupersededUnderLock_readsTheRunAsItIsNow(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	published := time.Now().UTC()
	for i, tc := range []struct {
		name     string
		queuedAt time.Time
		stale    bool
	}{{"queued before the publication", published.Add(-time.Minute), false}, {"queued again after it", published.Add(time.Minute), true}} {
		t.Run(tc.name, func(t *testing.T) {
			queuedAt := tc.queuedAt
			runID := fmt.Sprintf("run-lock-%d", i)
			if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusQueued, QueuedAt: &queuedAt}); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			msg := &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", PublishedAtRFC: published.Format(time.RFC3339Nano), Resume: &queue.ResumeSpec{}}
			out, stale := r.supersededUnderLock(msg, iterlog.Nop())
			if stale != tc.stale {
				t.Fatalf("under the lock: stale=%v %+v, want stale=%v", stale, out, tc.stale)
			}
			if stale {
				assertStaleDisposition(t, store.RunStatusQueued, out)
			}
		})
	}
}

// assertStaleDisposition: a delivery its run (status) was queued past is
// dropped — re-offered instead while the run is still queued, its newer
// attempt unclaimed, and dropped on its last permitted attempt.
func assertStaleDisposition(t *testing.T, status store.RunStatus, out preconditionOutcome) {
	t.Helper()
	if status != store.RunStatusQueued {
		if out.proceed || out.op != "ack-stale-attempt" || out.action != actionAck {
			t.Fatalf("a delivery published before the run was queued again (now %s): %+v, want ack-stale-attempt", status, out)
		}
		return
	}
	if out.proceed || out.op != "nak-stale-attempt-unclaimed" || out.action != actionNakDelayed || out.delay != supersededQueuedNakDelay {
		t.Fatalf("a delivery published before the run was queued again, that attempt unclaimed: %+v, want it re-offered", out)
	}
	if last := out.onLastDelivery; last == nil || last.op != "ack-stale-attempt" || last.action != actionAck {
		t.Fatalf("on its last permitted attempt: %+v, want ack-stale-attempt", last)
	}
}

// TestClassifyExecResult_aSupersededResumeIsLeftToTheNewerAttempt: a resume
// the engine found superseded is acked — never redelivered into a DLQ park
// that would land on the newer attempt — and is neither banked nor
// announced as an outcome.
func TestClassifyExecResult_aSupersededResumeIsLeftToTheNewerAttempt(t *testing.T) {
	err := fmt.Errorf("%w: run r1 was queued at T2, this delivery was published at T1", runtime.ErrResumeSuperseded)
	out := classifyExecResult(err, "r1")
	if out.action != actionAck || out.finalStatus != "superseded" {
		t.Fatalf("classifyExecResult(superseded) = %+v, want an ack, superseded", out)
	}
	if bankableStatus(out.finalStatus) {
		t.Fatal("a superseded delivery banks its workspace onto the newer attempt's run")
	}
	if outcomeSideEffectsFire(err, out.action) {
		t.Fatal("a superseded delivery announces an outcome for the newer attempt's run")
	}
	if !errors.Is(err, runtime.ErrResumeSuperseded) {
		t.Fatal("precondition: the error is the engine's")
	}
}
