package alert_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// captureSink records delivered alerts.
type captureSink struct {
	mu   sync.Mutex
	seen []alert.Alert
}

func (c *captureSink) Notify(_ context.Context, a alert.Alert) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, a)
}

func (c *captureSink) alerts() []alert.Alert {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]alert.Alert(nil), c.seen...)
}

func opsWorld(t *testing.T) (*alert.OpsDispatcher, *store.FilesystemRunStore, *captureSink) {
	t.Helper()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	d := &alert.OpsDispatcher{
		Runs: rs,
		// usernotify's claim store IS the EpisodeClaims contract — using it
		// here pins the structural compatibility the wiring relies on.
		Claims:  usernotify.NewMemSentStore(),
		Sinks:   []alert.Sink{sink},
		BaseURL: "https://iterion.example.com",
	}
	return d, rs, sink
}

func seedOpsRun(t *testing.T, rs *store.FilesystemRunStore, status store.RunStatus, mut func(*store.Run)) *store.Run {
	t.Helper()
	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(context.Background(), id, "feed-watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = status
	run.Name = "cyber digest"
	if mut != nil {
		mut(run)
	}
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

// TestOpsDispatcher_ParkedRunAlertsOnceWithETA is the 2026-08-31 scenario:
// a digest parks failed_resumable on a usage window with a retry armed. The
// operator webhook must say so ONCE — with the reason and the reset ETA —
// however many times the episode is offered (bus + sweep + replicas).
func TestOpsDispatcher_ParkedRunAlertsOnceWithETA(t *testing.T) {
	d, rs, sink := opsWorld(t)
	reset := time.Now().Add(3 * time.Hour).UTC()
	run := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) {
		r.Error = "usage cap: weekly window at 70%"
		r.RetryState = &store.RunRetryState{RetryAfter: &reset, Reason: "usage_window", Code: "USAGE_LIMIT_BLOCKED", Attempts: 1}
	})

	ev := trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil)
	if ev.Kind != trigger.KindRunFailed {
		t.Fatalf("outcome kind = %s, want run.failed", ev.Kind)
	}
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// Same episode again (sweep replay / second replica): deduped.
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("re-handle: %v", err)
	}

	got := sink.alerts()
	if len(got) != 1 {
		t.Fatalf("delivered %d alerts, want exactly 1", len(got))
	}
	a := got[0]
	if a.Kind != alert.KindRunParked {
		t.Fatalf("kind = %s, want run_parked", a.Kind)
	}
	for _, want := range []string{"usage_window", reset.Format(time.RFC3339), "usage cap"} {
		if !strings.Contains(a.Reason+a.WebhookText(), want) {
			t.Fatalf("alert text misses %q: %s", want, a.WebhookText())
		}
	}
	if !strings.Contains(a.Link, run.ID) {
		t.Fatalf("no deep link to the run: %q", a.Link)
	}
}

// TestOpsDispatcher_SweepRepaysTheDroppedEvent: the lossy bus dropped the
// outcome; the sweep window re-offers it and the alert still goes out.
func TestOpsDispatcher_SweepRepaysTheDroppedEvent(t *testing.T) {
	d, rs, sink := opsWorld(t)
	run := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) {
		r.Error = "provider window closed"
	})

	list := func(_ context.Context, _, _ time.Time, _ int) ([]usernotify.RunRef, error) {
		return []usernotify.RunRef{{ID: run.ID, Status: string(run.Status), UpdatedAt: run.UpdatedAt}}, nil
	}
	d.SweepOnce(context.Background(), list)
	if len(sink.alerts()) != 1 {
		t.Fatalf("sweep delivered %d alerts, want 1", len(sink.alerts()))
	}
	// A second pass over the same window is a cheap no-op.
	d.SweepOnce(context.Background(), list)
	if len(sink.alerts()) != 1 {
		t.Fatal("sweep re-delivered a claimed episode")
	}
}

// TestOpsDispatcher_ClassificationBoundaries: hard failures alert as
// run_failed; anything non-failed alerts nothing.
func TestOpsDispatcher_ClassificationBoundaries(t *testing.T) {
	d, rs, sink := opsWorld(t)

	hard := seedOpsRun(t, rs, store.RunStatusFailed, func(r *store.Run) { r.Error = "FailNode reached" })
	_ = d.Handle(context.Background(), trigger.BuildRunOutcome(context.Background(), rs, hard.ID, nil))
	if got := sink.alerts(); len(got) != 1 || got[0].Kind != alert.KindRunFailed {
		t.Fatalf("hard failure: %+v", got)
	}

	fin := seedOpsRun(t, rs, store.RunStatusFinished, nil)
	_ = d.Handle(context.Background(), trigger.BuildRunOutcome(context.Background(), rs, fin.ID, nil))
	if len(sink.alerts()) != 1 {
		t.Fatal("a finished run produced an operator alert")
	}
}
