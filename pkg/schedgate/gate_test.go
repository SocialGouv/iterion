package schedgate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestApply(t *testing.T) {
	base := NewTickRecord(SurfaceHostCron, "sched-1", time.Unix(1, 0).UTC(), "")

	t.Run("no lister, no guard → proceed", func(t *testing.T) {
		out := Apply(context.Background(), GateInput{Policy: Policy{}, Record: base})
		if !out.Proceed || out.GuardRan {
			t.Fatalf("outcome = %+v, want plain proceed", out)
		}
	})

	t.Run("overlap skip names the blocking run", func(t *testing.T) {
		lister := &fakeLister{
			ids:  []string{"run-a"},
			runs: map[string]*store.Run{"run-a": {ID: "run-a", Status: store.RunStatusRunning}},
		}
		out := Apply(context.Background(), GateInput{
			Policy:     Policy{Overlap: OverlapSkip},
			Lister:     lister,
			ScheduleID: "sched-1",
			Record:     base,
		})
		if out.Proceed {
			t.Fatal("expected skip")
		}
		if out.Record.Decision != TickSkippedOverlap || out.Record.BlockingRunID != "run-a" {
			t.Fatalf("record = %+v", out.Record)
		}
		if !strings.Contains(out.Record.Reason, "overlap=skip") {
			t.Fatalf("reason = %q", out.Record.Reason)
		}
	})

	t.Run("guard ok injects stdout even when empty", func(t *testing.T) {
		out := Apply(context.Background(), GateInput{
			Policy: Policy{Guard: "true"},
			Record: base,
		})
		if !out.Proceed || !out.GuardRan {
			t.Fatalf("outcome = %+v, want proceed with GuardRan", out)
		}
		if out.GuardStdout != "" {
			t.Fatalf("stdout = %q", out.GuardStdout)
		}
	})

	t.Run("guard blocked stamps decision + exit", func(t *testing.T) {
		out := Apply(context.Background(), GateInput{
			Policy: Policy{Guard: "exit 3"},
			Record: base,
		})
		if out.Proceed {
			t.Fatal("expected block")
		}
		if out.Record.Decision != TickGuardBlocked || out.Record.GuardExit == nil || *out.Record.GuardExit != 3 {
			t.Fatalf("record = %+v", out.Record)
		}
	})

	t.Run("guard error is a decision, not an error", func(t *testing.T) {
		out := Apply(context.Background(), GateInput{
			Policy: Policy{Guard: "sleep 5", GuardTimeout: "30ms"},
			Record: base,
		})
		if out.Proceed || out.Record.Decision != TickGuardError {
			t.Fatalf("outcome = %+v", out)
		}
		if out.Record.Error == "" {
			t.Fatal("guard error must carry the cause")
		}
	})

	t.Run("keepalive: fresh run blocks the tick", func(t *testing.T) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
		lister := &fakeLister{
			ids:  []string{"alive"},
			runs: map[string]*store.Run{"alive": {ID: "alive", Status: store.RunStatusRunning, UpdatedAt: now.Add(-1 * time.Minute)}},
		}
		out := Apply(context.Background(), GateInput{
			Policy:     Policy{Overlap: OverlapKeepalive, StaleAfter: "5m"},
			Lister:     lister,
			ScheduleID: "sched-1",
			Record:     base,
			Now:        now,
		})
		if out.Proceed {
			t.Fatal("a fresh keepalive run must block the tick")
		}
		if len(out.ReapRunIDs) != 0 {
			t.Fatalf("no reap expected, got %v", out.ReapRunIDs)
		}
	})

	t.Run("keepalive: stale run relaunches and is reaped", func(t *testing.T) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
		lister := &fakeLister{
			ids:  []string{"zombie"},
			runs: map[string]*store.Run{"zombie": {ID: "zombie", Status: store.RunStatusRunning, UpdatedAt: now.Add(-10 * time.Minute)}},
		}
		out := Apply(context.Background(), GateInput{
			Policy:     Policy{Overlap: OverlapKeepalive, StaleAfter: "5m"},
			Lister:     lister,
			ScheduleID: "sched-1",
			Record:     base,
			Now:        now,
		})
		if !out.Proceed {
			t.Fatal("a stale keepalive run must not block relaunch")
		}
		if len(out.ReapRunIDs) != 1 || out.ReapRunIDs[0] != "zombie" {
			t.Fatalf("ReapRunIDs = %v, want [zombie]", out.ReapRunIDs)
		}
	})
}

// supersede lives in the SHARED vocabulary, so Validate accepts it on the
// host-cron, cloud-schedule and trigger surfaces too — all of which go through
// Apply. Without a branch here it fell through to Proceed with nothing to
// cancel, i.e. `allow` semantics on three surfaces while the policy promises
// at-most-one-live. The reap list is how a policy tells the caller to cancel.
func TestApplySupersedeReapsTheLiveRuns(t *testing.T) {
	out := Apply(context.Background(), GateInput{
		Policy:     Policy{Overlap: OverlapSupersede},
		ScheduleID: "s1",
		Lister: &fakeLister{
			ids: []string{"run-old", "run-older"},
			runs: map[string]*store.Run{
				"run-old":   {ID: "run-old", Status: store.RunStatusRunning},
				"run-older": {ID: "run-older", Status: store.RunStatusRunning},
			},
		},
	})
	if !out.Proceed {
		t.Fatal("supersede fires: the new tick is the one that matters")
	}
	if len(out.ReapRunIDs) != 2 {
		t.Fatalf("the live runs must be handed to the caller to cancel, got %v", out.ReapRunIDs)
	}
	// skip must keep blocking — the two policies are opposites, not variants.
	if blocked := Apply(context.Background(), GateInput{
		Policy: Policy{Overlap: OverlapSkip}, ScheduleID: "s1",
		Lister: &fakeLister{
			ids:  []string{"run-old"},
			runs: map[string]*store.Run{"run-old": {ID: "run-old", Status: store.RunStatusRunning}},
		},
	}); blocked.Proceed {
		t.Fatal("skip must still block while a run is live")
	}
}
