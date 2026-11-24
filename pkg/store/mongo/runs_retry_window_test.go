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

// The credential probe's scan lists every armed usage-window wait and every
// run a pull still marks — the probe whose resume cleared its retry, which
// only its marker keeps visible — and projects the credentials and the retry
// policy the pass groups and bounds them by (#3127). Against a real replica
// set: the $or and the projection ARE Mongo's.
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27077/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run TestRunRetryScans_ListTheCredentialWindow
func TestRunRetryScans_ListTheCredentialWindow(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)
	wall := now.Add(4 * 24 * time.Hour)
	arm := func(id, reason string) {
		t.Helper()
		seedFailedResumable(t, s, id)
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		r.RetryPolicy = &store.RunRetryPolicy{MaxWait: "36h"}
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
		if err := s.SetRunCredStamp(ctx, id, store.RunCredStamp{Fingerprints: []string{"fp-zai"}}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", id, err)
		}
		if scheduled, _, err := s.ScheduleRunRetry(ctx, id, wall.Add(time.Minute), wall, reason, "USAGE_LIMIT_BLOCKED", 5); err != nil || !scheduled {
			t.Fatalf("ScheduleRunRetry %s = (%v, %v)", id, scheduled, err)
		}
	}
	arm("cred-armed", store.RetryReasonUsageWindow)
	arm("cred-probed", store.RetryReasonUsageWindow)
	arm("cred-other-reason", "transient")
	if pulled, err := s.PullRunRetry(ctx, "cred-probed", wall.Add(time.Minute), now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v)", pulled, err)
	}
	if err := s.ClearRunRetry(ctx, "cred-probed"); err != nil {
		t.Fatalf("ClearRunRetry: %v", err)
	}
	// The resume re-stamps the run's sealed credentials (cloudpublisher's
	// SetRunCredStamp, unconditional): its re-resolution sealed the next
	// slot. The probe still belongs to the window its wall was met on.
	if err := s.SetRunCredStamp(ctx, "cred-probed", store.RunCredStamp{Fingerprints: []string{"fp-zai-slot-2"}}); err != nil {
		t.Fatalf("SetRunCredStamp re-stamp: %v", err)
	}

	listed := func(since time.Time) map[string]RetryDueRef {
		t.Helper()
		rows, err := s.ListRunsForCredentialProbe(store.WithoutTenantFilter(ctx), since, 500)
		if err != nil {
			t.Fatalf("ListRunsForCredentialProbe: %v", err)
		}
		out := map[string]RetryDueRef{}
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	got := listed(now.Add(-time.Hour))
	if _, ok := got["cred-other-reason"]; ok || len(got) != 2 {
		t.Fatalf("listed %v, want the armed wait and the probed run only", got)
	}
	armed, probed := got["cred-armed"], got["cred-probed"]
	if rec := armed.Record(); rec.UsageWindowPark().Credential != "fp-zai" || rec.RetryPolicy == nil || rec.RetryPolicy.MaxWait != "36h" {
		t.Fatalf("armed row record = %+v, want its credential fp-zai and its 36h max_wait projected", rec)
	}
	if probed.RetryState == nil || probed.RetryState.ProbeFrom == nil || probed.RetryState.RetryAfter != nil {
		t.Fatalf("probed row retry state = %+v, want the marker without an armed retry", probed.RetryState)
	}
	if key := probed.RetryState.CredentialWindow(); key != "fp-zai" {
		t.Fatalf("probed row credential window = %q after its resume re-stamped its credentials, want fp-zai — the arming's copy, which no later write moves", key)
	}
	if got := listed(now.Add(time.Hour)); len(got) != 1 || got["cred-armed"].ID == "" {
		t.Fatalf("listed %v with the lookback after the pull, want only the armed wait", got)
	}
}
