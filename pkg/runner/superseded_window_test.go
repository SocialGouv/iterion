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

// windowRun is a run whose previous attempt was claimed, then interrupted by
// a drain: failed_resumable, its redelivery pending — and that redelivery,
// published after the attempt's flip.
func windowRun(t *testing.T) (store.RunStore, *queue.RunMessage) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	const runID = "run-window"
	if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusFailedResumable}); err != nil {
		t.Fatal(err)
	}
	flip, ok, err := store.AsQueuedFlipper(st).FlipToQueued(ctx, runID, store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("the previous attempt's flip: %v %v", ok, err)
	}
	if ok, err := st.UpdateRunStatusIf(ctx, runID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := st.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, fmt.Sprintf("drain: %v", runtime.ErrRunInterrupted),
		store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending}, []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
		t.Fatalf("interrupt: %v %v", ok, err)
	}
	time.Sleep(2 * time.Millisecond)
	return st, &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", PublishedAtRFC: flip.At.Add(time.Millisecond).Format(time.RFC3339Nano)}
}

// TestAdmission_aDeliveryReadInsideARefusedFlipsWindowIsReofferedNotLost:
// resume A flips the run to queued; the previous attempt's pending
// redelivery is admitted while A waits on its publication. It is
// re-offered, not acked — A is then refused and its flip reverted, and the
// same redelivery runs. On its last permitted attempt it is dropped, as a
// superseded delivery always was: there is nothing left to re-offer.
func TestAdmission_aDeliveryReadInsideARefusedFlipsWindowIsReofferedNotLost(t *testing.T) {
	st, redelivery := windowRun(t)
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, maxDeliverOverride: 8}
	time.Sleep(2 * time.Millisecond)
	flipA, ok, err := store.AsQueuedFlipper(st).FlipToQueued(ctx, redelivery.RunID, store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("A's flip: %v %v", ok, err)
	}
	during := r.lastDeliveryDisposition(r.resolveDeliveryPreconditions(redelivery), 3)
	if during.proceed || !isNakAction(during.action) {
		t.Fatalf("the pending redelivery admitted inside A's window: proceed=%v op=%q action=%v — want it re-offered: an ack is forever, and A may be refused", during.proceed, during.op, during.action)
	}
	if last := r.lastDeliveryDisposition(r.resolveDeliveryPreconditions(redelivery), 8); last.proceed || last.action != actionAck {
		t.Fatalf("the same delivery on its last permitted attempt: op=%q action=%v — want it dropped: there is nothing left to re-offer", last.op, last.action)
	}
	if ok, err := store.AsQueuedFlipper(st).RevertQueuedFlip(ctx, redelivery.RunID, flipA, "queue resume: refused"); err != nil || !ok {
		t.Fatalf("A's revert: %v %v", ok, err)
	}
	if after := r.resolveDeliveryPreconditions(redelivery); !after.proceed {
		t.Fatalf("after A's revert the redelivery is %q — want it to run, as the run's current attempt", after.op)
	}
	// Claimed past it — the newer attempt's own delivery took the run — it
	// is dropped at once.
	time.Sleep(2 * time.Millisecond)
	if _, ok, err := store.AsQueuedFlipper(st).FlipToQueued(ctx, redelivery.RunID, store.RunStatusFailedResumable, time.Now()); err != nil || !ok {
		t.Fatalf("B's flip: %v %v", ok, err)
	}
	if ok, err := st.UpdateRunStatusIf(ctx, redelivery.RunID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
		t.Fatalf("B's claim: %v %v", ok, err)
	}
	if claimed := r.lastDeliveryDisposition(r.resolveDeliveryPreconditions(redelivery), 3); claimed.proceed || claimed.action != actionAck {
		t.Fatalf("a delivery whose run a newer attempt claimed: op=%q action=%v — want it dropped", claimed.op, claimed.action)
	}
}

// TestSupersededAfterEngine_reoffersWhileTheNewerAttemptIsUnclaimed: a resume
// the engine refused as superseded is re-offered while the newer attempt is
// unclaimed, or once its flip was reverted; dropped once it was claimed.
func TestSupersededAfterEngine_reoffersWhileTheNewerAttemptIsUnclaimed(t *testing.T) {
	engineErr := fmt.Errorf("%w: run queued again", runtime.ErrResumeSuperseded)
	st, msg := windowRun(t)
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, maxDeliverOverride: 8}
	time.Sleep(2 * time.Millisecond)
	flipB, ok, err := store.AsQueuedFlipper(st).FlipToQueued(ctx, msg.RunID, store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("B's flip: %v %v", ok, err)
	}
	if out := r.supersededAfterEngine(msg, engineErr); out.proceed || !isNakAction(out.action) {
		t.Fatalf("superseded while the newer attempt is unclaimed: op=%q action=%v, want a re-offer", out.op, out.action)
	}
	if out := r.lastDeliveryDisposition(r.supersededAfterEngine(msg, engineErr), 8); out.action != actionAck {
		t.Fatalf("on the last permitted attempt: op=%q action=%v, want the drop", out.op, out.action)
	}
	if ok, err := store.AsQueuedFlipper(st).RevertQueuedFlip(ctx, msg.RunID, flipB, "refused"); err != nil || !ok {
		t.Fatalf("B's revert: %v %v", ok, err)
	}
	if out := r.supersededAfterEngine(msg, engineErr); !isNakAction(out.action) {
		t.Fatalf("superseded by a flip since reverted: op=%q action=%v, want a re-offer — this attempt is the run's current one", out.op, out.action)
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok, err := store.AsQueuedFlipper(st).FlipToQueued(ctx, msg.RunID, store.RunStatusFailedResumable, time.Now()); err != nil || !ok {
		t.Fatalf("C's flip: %v %v", ok, err)
	}
	if ok, err := st.UpdateRunStatusIf(ctx, msg.RunID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
		t.Fatalf("C's claim: %v %v", ok, err)
	}
	if out := r.supersededAfterEngine(msg, engineErr); out.action != actionAck || !errors.Is(engineErr, runtime.ErrResumeSuperseded) {
		t.Fatalf("superseded by a claimed attempt: op=%q action=%v, want the drop", out.op, out.action)
	}
}
