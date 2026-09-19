package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// TestScheduleOutcomeStampsBothOutcomesOnTheSchedule is the property test
// for #1426: on a run-terminal event, the schedule's LastRun* fields are
// updated to reflect what the run did — not just that it was dispatched.
// The pre-fix behaviour was `fired at T, that is all anyone can learn about
// it`; the fix carries the outcome back.
//
// Two rows: a successful run clears the previous error; a failed run
// stamps its Error + FailureCode. Mutation: bypass MarkRunOutcome (comment
// it out) → LastRunStatus stays empty → red on both rows.
func TestScheduleOutcomeStampsBothOutcomesOnTheSchedule(t *testing.T) {
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, nil))

	sched := cloudsched.NewMemoryStore()
	srv.cfg.ScheduledBots = sched

	// Seed a schedule.
	scheduleID := "sched-1"
	tenantID := "tenant-A"
	if err := sched.Create(context.Background(), cloudsched.ScheduledBot{
		ID: scheduleID, TenantID: tenantID, BotID: "feed-watch",
		Cron:       "* * * * *",
		NextFireAt: time.Now().Add(time.Minute),
		CreatedAt:  time.Now(), UpdatedAt: time.Now(),
		// A stale error from a previous tick — the successful outcome
		// below must CLEAR it.
		LastRunError:     "old failure",
		LastRunErrorCode: "old_code",
	}); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	// Two runs — one success, one failure — both back-referenced to the
	// schedule via Source.ScheduleID.
	runsCases := []struct {
		name            string
		runID           string
		status          store.RunStatus
		errMsg          string
		failureCode     store.FailureCode
		wantErrMsg      string
		wantErrCode     string
		wantStatus      string
		wantOutcomeKind string
	}{
		{
			name:            "success clears the previous failure",
			runID:           "run-ok",
			status:          store.RunStatusFinished,
			errMsg:          "",
			failureCode:     "",
			wantErrMsg:      "",
			wantErrCode:     "",
			wantStatus:      string(store.RunStatusFinished),
			wantOutcomeKind: trigger.KindRunFinished,
		},
		{
			name:            "failure stamps error + code",
			runID:           "run-boom",
			status:          store.RunStatusFailed,
			errMsg:          "sandbox: mode strict requested but no container runtime is available",
			failureCode:     store.FailureSandboxSetupTimeout, // stand-in for the #1425 typed refusal
			wantErrMsg:      "sandbox: mode strict requested but no container runtime is available",
			wantErrCode:     string(store.FailureSandboxSetupTimeout),
			wantStatus:      string(store.RunStatusFailed),
			wantOutcomeKind: trigger.KindRunFailed,
		},
	}
	for _, tc := range runsCases {
		t.Run(tc.name, func(t *testing.T) {
			// Seed the run through the shared RunStore then patch in the
			// Source + terminal fields via LoadRun/SaveRun — the same
			// pattern the storetest/conformance helper uses for schedule
			// back-refs.
			rs := srv.runs.RunStore()
			rctx := store.WithTenant(context.Background(), tenantID)
			if _, err := rs.CreateRun(rctx, tc.runID, "feed-watch", nil); err != nil {
				t.Fatalf("create run %s: %v", tc.runID, err)
			}
			r, err := rs.LoadRun(rctx, tc.runID)
			if err != nil {
				t.Fatalf("load run %s: %v", tc.runID, err)
			}
			r.TenantID = tenantID
			r.Status = tc.status
			r.Error = tc.errMsg
			r.FailureCode = tc.failureCode
			r.Source = &store.RunSource{
				Kind:         store.RunSourceKindSchedule,
				ScheduleID:   scheduleID,
				ScheduleName: "feed-watch",
			}
			if err := rs.SaveRun(rctx, r); err != nil {
				t.Fatalf("save run %s: %v", tc.runID, err)
			}
			// Fire the event on the subscriber directly (bypass the bus).
			ev := trigger.Event{
				Source:  trigger.SourceRun,
				Kind:    tc.wantOutcomeKind,
				Subject: trigger.Subject{Type: "run", ID: tc.runID, State: tc.wantStatus},
			}
			if err := srv.handleScheduleOutcomeEvent(context.Background(), ev); err != nil {
				t.Fatalf("handle: %v", err)
			}
			// Read back.
			got, err := sched.Get(context.Background(), scheduleID)
			if err != nil {
				t.Fatalf("get schedule: %v", err)
			}
			if got.LastRunID != tc.runID {
				t.Errorf("LastRunID=%q; want %q", got.LastRunID, tc.runID)
			}
			if got.LastRunStatus != tc.wantStatus {
				t.Errorf("LastRunStatus=%q; want %q", got.LastRunStatus, tc.wantStatus)
			}
			if got.LastRunError != tc.wantErrMsg {
				t.Errorf("LastRunError=%q; want %q", got.LastRunError, tc.wantErrMsg)
			}
			if got.LastRunErrorCode != tc.wantErrCode {
				t.Errorf("LastRunErrorCode=%q; want %q", got.LastRunErrorCode, tc.wantErrCode)
			}
			if got.LastRunAt == nil {
				t.Errorf("LastRunAt is nil; want a timestamp")
			}
		})
	}
}

// TestScheduleOutcomeIgnoresNonScheduledRuns pins that a run WITHOUT a
// Source.ScheduleID (a manual launch, a board card, a webhook) does not
// mutate any schedule record. Mutation: drop the `run.Source.ScheduleID
// == ""` guard → any schedule with a colliding LastRunID would be
// overwritten → red.
func TestScheduleOutcomeIgnoresNonScheduledRuns(t *testing.T) {
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, nil))

	sched := cloudsched.NewMemoryStore()
	srv.cfg.ScheduledBots = sched
	if err := sched.Create(context.Background(), cloudsched.ScheduledBot{
		ID: "sched-A", TenantID: "T", BotID: "b", Cron: "* * * * *",
		NextFireAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rs := srv.runs.RunStore()
	rctx := store.WithTenant(context.Background(), "T")
	if _, err := rs.CreateRun(rctx, "manual-run", "b", nil); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	r, err := rs.LoadRun(rctx, "manual-run")
	if err != nil {
		t.Fatalf("load manual run: %v", err)
	}
	r.TenantID = "T"
	r.Status = store.RunStatusFinished
	// A run without Source at all — the guard must skip it.
	r.Source = nil
	if err := rs.SaveRun(rctx, r); err != nil {
		t.Fatalf("save: %v", err)
	}

	ev := trigger.Event{
		Source: trigger.SourceRun, Kind: trigger.KindRunFinished,
		Subject: trigger.Subject{Type: "run", ID: "manual-run", State: string(store.RunStatusFinished)},
	}
	if err := srv.handleScheduleOutcomeEvent(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}
	got, err := sched.Get(context.Background(), "sched-A")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastRunID != "" {
		t.Errorf("LastRunID=%q; want empty for a non-scheduled run", got.LastRunID)
	}
}

// TestScheduleOutcomeHandlesMissingRun pins the graceful path: a run
// that has been purged (older than the retention window) fires no error
// — the schedule's LastFireAt already proves the dispatch, and a missed
// back-write is not worth surfacing.
func TestScheduleOutcomeHandlesMissingRun(t *testing.T) {
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, nil))
	srv.cfg.ScheduledBots = cloudsched.NewMemoryStore()
	ev := trigger.Event{
		Source: trigger.SourceRun, Kind: trigger.KindRunFailed,
		Subject: trigger.Subject{Type: "run", ID: "purged-run"},
	}
	if err := srv.handleScheduleOutcomeEvent(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}
}
