package orgsweep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/log"
)

// pendingOrg seeds an org past its grace with the given teams in a memory
// identity store, the precondition PurgeOrg re-asserts.
func pendingOrg(t *testing.T, st *identity.MemoryStore, orgID string, teamIDs ...string) identity.Org {
	t.Helper()
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)
	org, err := st.CreateOrg(ctx, identity.Org{
		ID: orgID, Name: "Org " + orgID, Slug: orgID,
		Status: identity.TeamStatusPendingDeletion, PurgeAfter: &past,
	})
	if err != nil {
		t.Fatalf("CreateOrg %s: %v", orgID, err)
	}
	for _, id := range teamIDs {
		if _, err := st.CreateTeam(ctx, identity.Team{ID: id, OrgID: orgID, Name: "Team " + id, Slug: id}); err != nil {
			t.Fatalf("CreateTeam %s: %v", id, err)
		}
	}
	return org
}

// cascadeMustNotRun is the Cascade of every test below that stops before the
// Mongo sweep: reaching the identity cascade there would mean the sweep ran
// over rows whose blobs are still in the bucket.
func cascadeMustNotRun(t *testing.T) Cascade {
	return func(context.Context, string) error {
		t.Fatal("cascade must NOT run while runs of the org still own blobs")
		return nil
	}
}

// TestPurgeRuns_DeletesEveryRunOfEveryTeamWithItsBlobs is the witness of the
// run step on its own: two teams, two runs with objects in the bucket, and
// after the step the bucket holds nothing of them, the store lists none, and
// every listing ran under the team's tenant scope. No Mongo: the step is the
// part of PurgeOrg that reaches the blob store, and it is reached through the
// run store alone.
func TestPurgeRuns_DeletesEveryRunOfEveryTeamWithItsBlobs(t *testing.T) {
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-blobs", "team-1", "team-2")
	runs := newFakeRunStore()
	runs.addRun("r-1", "team-1", 3)
	runs.addRun("r-2", "team-2", 2)
	runs.addRun("r-other", "team-elsewhere", 4) // another tenant's run, with its objects
	teams, err := st.ListTeamsByOrg(context.Background(), org.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := &Purger{Store: st, Runs: runs, Cascade: cascadeMustNotRun(t)}
	n, err := p.purgeRuns(context.Background(), org, teams)
	if err != nil {
		t.Fatalf("purgeRuns: %v", err)
	}
	if n != 2 {
		t.Errorf("purgeRuns deleted %d run(s), want 2", n)
	}
	if got := runs.objects(); got != 4 {
		t.Errorf("objects left in the bucket: %d, want 4 (the other tenant's only) — the purged org's blobs survived", got)
	}
	for _, team := range []string{"team-1", "team-2"} {
		if got := runs.runsOf(team); got != 0 {
			t.Errorf("%s: %d run(s) still in the store, want 0", team, got)
		}
	}
	if got := runs.runsOf("team-elsewhere"); got != 1 {
		t.Errorf("team-elsewhere: %d run(s), want 1 — the purge reached another tenant", got)
	}
	sort.Strings(runs.listed)
	if strings.Join(runs.listed, ",") != "team-1,team-2" {
		t.Errorf("the run listing ran under tenants %v, want each team of the org exactly once", runs.listed)
	}
}

// TestPurgeOrg_WithholdsTheMongoSweepWhenARunDeleteFails: one run of two
// cannot be deleted. Every run is still attempted, the error names the one
// that failed, and NOTHING after the run step runs — the DB is nil here, so
// a DeleteMany would panic, and the cascade fails the test. The org stays
// pending for the next sweep.
func TestPurgeOrg_WithholdsTheMongoSweepWhenARunDeleteFails(t *testing.T) {
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-fail", "team-f")
	runs := newFakeRunStore()
	runs.addRun("r-ok", "team-f", 1)
	runs.addRun("r-stuck", "team-f", 2)
	stuck := errors.New("s3: 503 slow down")
	runs.failing["r-stuck"] = stuck
	p := &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t)}
	n, err := p.PurgeOrg(context.Background(), org.ID)
	if !errors.Is(err, stuck) {
		t.Fatalf("PurgeOrg err = %v, want the run's own error wrapped", err)
	}
	if !strings.Contains(err.Error(), "r-stuck") || !strings.Contains(err.Error(), "withheld") {
		t.Errorf("error %q does not name the failed run and the withheld sweep", err)
	}
	if n != 1 {
		t.Errorf("deleted %d run(s), want 1 — the other run is attempted despite the failure", n)
	}
	if got := runs.objects(); got != 2 {
		t.Errorf("objects left: %d, want 2 (the stuck run's)", got)
	}
	if _, err := st.GetOrg(context.Background(), org.ID); err != nil {
		t.Errorf("the org must stay pending for the next sweep, GetOrg: %v", err)
	}
}

// TestPurgeOrg_DefersWhenTheRunBatchIsExhausted: with a batch of one and three
// runs, two passes each delete one run and return ErrPurgeDeferred before any
// Mongo access (nil DB) or cascade; a deleted run is not listed again, so the
// passes make progress rather than redo the same run.
func TestPurgeOrg_DefersWhenTheRunBatchIsExhausted(t *testing.T) {
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-batch", "team-b")
	runs := newFakeRunStore()
	for _, id := range []string{"r-1", "r-2", "r-3"} {
		runs.addRun(id, "team-b", 1)
	}
	p := &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t), RunBatch: 1}
	for pass := 1; pass <= 2; pass++ {
		n, err := p.PurgeOrg(context.Background(), org.ID)
		if !errors.Is(err, ErrPurgeDeferred) {
			t.Fatalf("pass %d: err = %v, want ErrPurgeDeferred", pass, err)
		}
		if n != 1 {
			t.Errorf("pass %d: deleted %d run(s), want 1 (the batch)", pass, n)
		}
		if got := runs.objects(); got != 3-pass {
			t.Errorf("pass %d: objects left %d, want %d", pass, got, 3-pass)
		}
	}
	if strings.Join(runs.deleted, ",") != "r-1,r-2" {
		t.Errorf("runs deleted across the passes: %v, want r-1 then r-2 — a pass redid a run already gone", runs.deleted)
	}
	if _, err := st.GetOrg(context.Background(), org.ID); err != nil {
		t.Errorf("the org must stay pending between deferred passes, GetOrg: %v", err)
	}
}

// TestPurgeOrg_RefusesToSweepWithoutARunStore: a Purger wired without the
// run store cannot know whether the team's runs own blobs, so it refuses
// with an explicit error rather than DeleteMany-ing the run rows.
func TestPurgeOrg_RefusesToSweepWithoutARunStore(t *testing.T) {
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-nostore", "team-n")
	p := &Purger{DB: nil, Store: st, Runs: nil, Cascade: cascadeMustNotRun(t)}
	_, err := p.PurgeOrg(context.Background(), org.ID)
	if err == nil || !strings.Contains(err.Error(), "no run store wired") {
		t.Fatalf("PurgeOrg err = %v, want an explicit refusal naming the missing run store", err)
	}
}

func TestRunBatchFromEnv(t *testing.T) {
	t.Setenv(RunBatchEnv, "")
	if n, err := RunBatchFromEnv(); err != nil || n != 0 {
		t.Errorf("empty: got (%d, %v), want (0, nil)", n, err)
	}
	t.Setenv(RunBatchEnv, "250")
	if n, err := RunBatchFromEnv(); err != nil || n != 250 {
		t.Errorf("250: got (%d, %v), want (250, nil)", n, err)
	}
	for _, bad := range []string{"-1", "many", "1.5"} {
		t.Setenv(RunBatchEnv, bad)
		if n, err := RunBatchFromEnv(); err == nil {
			t.Errorf("%q: got (%d, nil), want an error — a bad dial read as 0 would silently lift the bound", bad, n)
		}
	}
}

// TestPurgeOrg_AFailedRunMakesTheDeferredPassAFailure: the batch of two
// attempts is reached after one run of the pass could not be deleted and one
// was. The verdict is the failure — the withheld-sweep error wrapping the
// run's own — not ErrPurgeDeferred, which Sweep would log as progress.
func TestPurgeOrg_AFailedRunMakesTheDeferredPassAFailure(t *testing.T) {
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-batch-fail", "team-bf")
	runs := newFakeRunStore()
	for _, id := range []string{"r-1", "r-2", "r-3"} {
		runs.addRun(id, "team-bf", 1)
	}
	stuck := errors.New("s3: 503 slow down")
	runs.failing["r-1"] = stuck
	p := &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t), RunBatch: 2}
	n, err := p.PurgeOrg(context.Background(), org.ID)
	if !errors.Is(err, stuck) {
		t.Fatalf("PurgeOrg err = %v, want the failed run's error wrapped — the batch bound hid the failure", err)
	}
	if errors.Is(err, ErrPurgeDeferred) {
		t.Errorf("PurgeOrg err = %v, must not read as a deferred (progress) pass when a run failed", err)
	}
	if !strings.Contains(err.Error(), "withheld") {
		t.Errorf("error %q does not say the sweep is withheld", err)
	}
	if n != 1 {
		t.Errorf("deleted %d run(s), want 1 (the batch, r-2)", n)
	}
	if _, err := st.GetOrg(context.Background(), org.ID); err != nil {
		t.Errorf("the org must stay pending, GetOrg: %v", err)
	}
}

// TestWithheldSweep_NamesTheFirstFailuresAndCountsTheRest: the one error a
// sweep logs for a pass stays bounded — the first withheldFailuresShown
// failures are wrapped (errors.Is finds each), the rest are a count.
func TestWithheldSweep_NamesTheFirstFailuresAndCountsTheRest(t *testing.T) {
	org := identity.Org{ID: "org-many"}
	var failures []error
	for i := 0; i < withheldFailuresShown+3; i++ {
		failures = append(failures, fmt.Errorf("run r-%d: %w", i, errors.New("s3: 503")))
	}
	err := withheldSweep(org, failures)
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf("%d run(s) could not be deleted", len(failures))) {
		t.Errorf("error %q does not count every failure", msg)
	}
	if !strings.Contains(msg, "3 more") {
		t.Errorf("error %q does not count the failures it does not show", msg)
	}
	for i := 0; i < withheldFailuresShown; i++ {
		if !errors.Is(err, failures[i]) {
			t.Errorf("failure %d is not in the error chain", i)
		}
	}
	for i := withheldFailuresShown; i < len(failures); i++ {
		if errors.Is(err, failures[i]) || strings.Contains(msg, fmt.Sprintf("run r-%d:", i)) {
			t.Errorf("failure %d is carried in full — the error is unbounded by the number of failed runs", i)
		}
	}
	few := withheldSweep(org, failures[:2])
	if strings.Contains(few.Error(), "more") || !errors.Is(few, failures[1]) {
		t.Errorf("with %d failures every one is shown, got %q", 2, few)
	}
}

// TestSweep_ReportsADeferredPassAsProgress: a sweep over an org whose run
// batch is exhausted returns no error and no purged org — the org is still
// pending — and its log says the purge resumes rather than warning of a
// failure.
func TestSweep_ReportsADeferredPassAsProgress(t *testing.T) {
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-sweep-defer", "team-sd")
	runs := newFakeRunStore()
	runs.addRun("r-1", "team-sd", 1)
	runs.addRun("r-2", "team-sd", 1)
	var logs bytes.Buffer
	logger := log.New(log.LevelInfo, &logs)
	s := &Sweeper{
		Purger: &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t), RunBatch: 1, Logger: logger},
		Logger: logger,
	}
	purged, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if purged != 0 {
		t.Errorf("Sweep purged %d org(s), want 0 — the org is deferred, not gone", purged)
	}
	if _, err := st.GetOrg(context.Background(), org.ID); err != nil {
		t.Errorf("the org must stay pending after a deferred sweep, GetOrg: %v", err)
	}
	if got := runs.objects(); got != 1 {
		t.Errorf("objects left: %d, want 1 (the run the batch left for the next sweep)", got)
	}
	out := logs.String()
	if !strings.Contains(out, "1 run(s) deleted this pass, the purge resumes at the next sweep") {
		t.Errorf("the deferred pass is not reported as progress; log:\n%s", out)
	}
	if strings.Contains(out, "purge org "+org.ID+" ("+org.Name+"): ") {
		t.Errorf("the deferred pass is reported as a failed purge; log:\n%s", out)
	}
}
