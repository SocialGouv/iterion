package mongo

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The scans project the record facts a usage park is read from, so a pass
// tells a wait its window still holds without a second read per row
// (#3127). Against a real replica set: the projection IS Mongo's.
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27077/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run TestRunRetryScans_ProjectTheUsagePark
func TestRunRetryScans_ProjectTheUsagePark(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-window")
	if _, err := s.UpdateRunOutcome(ctx, "run-window", store.RunStatusFailedResumable, "",
		store.RunOutcomeMeta{Code: store.FailureUsageLimitBlocked}, []store.RunStatus{store.RunStatusFailedResumable}); err != nil {
		t.Fatalf("UpdateRunOutcome: %v", err)
	}
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	skipped := now.Add(3 * 24 * time.Hour).Truncate(time.Millisecond)
	if err := s.SetRunCredStamp(ctx, "run-window", store.RunCredStamp{SkippedReopensAt: &skipped}); err != nil {
		t.Fatalf("SetRunCredStamp: %v", err)
	}
	if scheduled, _, err := s.ScheduleRunRetry(ctx, "run-window", wall.Add(time.Minute), wall, store.RetryReasonUsageWindow, "USAGE_LIMIT_BLOCKED", 5); err != nil || !scheduled {
		t.Fatalf("ScheduleRunRetry = (%v, %v)", scheduled, err)
	}

	probes, err := s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), now, time.Hour, 24, 10)
	if err != nil || len(probes) != 1 {
		t.Fatalf("ListRunsForRetryProbe = %+v (%v), want the one parked run", probes, err)
	}
	park := probes[0].Record().UsageWindowPark()
	if !park.Parked || !park.ReopensAt.Equal(skipped) {
		t.Fatalf("projected park = %+v, want parked and reopening at the skipped credential's %s", park, skipped)
	}
	due, err := s.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), wall.Add(2*time.Minute), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("ListRunsDueForRetry = %+v (%v), want the one parked run", due, err)
	}
	if rec := due[0].Record(); rec.FailureCode != store.FailureUsageLimitBlocked || rec.ContinuationState != store.ContinuationRetryArmed {
		t.Fatalf("due row record = %+v, want USAGE_LIMIT_BLOCKED armed", rec)
	}
}
