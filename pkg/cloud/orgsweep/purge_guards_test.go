package orgsweep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mongoMustNotBeReached is deferred by the tests whose Purger has a nil DB:
// a collection sweep panics on it, and that panic means the purge went past
// the guard under test into the Mongo sweep. It becomes the test's failure.
func mongoMustNotBeReached(t *testing.T, what string) {
	if r := recover(); r != nil {
		t.Fatalf("%s: the purge went on to the Mongo sweep (%v)", what, r)
	}
}

// restoreOrg is what auth.Service.RestoreOrg writes: the org back to active,
// its purge date cleared.
func restoreOrg(t *testing.T, st *identity.MemoryStore, orgID string) {
	t.Helper()
	ctx := context.Background()
	o, err := st.GetOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("restore: GetOrg: %v", err)
	}
	o.Status = identity.TeamStatusActive
	o.PurgeAfter = nil
	if err := st.UpdateOrg(ctx, o); err != nil {
		t.Fatalf("restore: UpdateOrg: %v", err)
	}
}

// TestPurgeOrg_ARestoreDuringTheRunsStopsAtTheNextRun: the operator restores
// the org while its first run is being deleted. The next run is not deleted —
// eligibility is re-read before each run delete — and the purge reports
// ErrNoLongerEligible with the one run it took; the org stays restored.
func TestPurgeOrg_ARestoreDuringTheRunsStopsAtTheNextRun(t *testing.T) {
	defer mongoMustNotBeReached(t, "restore mid-runs")
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-restore-runs", "team-rr")
	runs := newFakeRunStore()
	for _, id := range []string{"r-1", "r-2", "r-3"} {
		runs.addRun(id, "team-rr", 1)
	}
	runs.onDelete = func(id string) {
		if id == "r-1" {
			restoreOrg(t, st, org.ID)
		}
	}
	p := &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t)}
	n, err := p.PurgeOrg(context.Background(), org.ID)
	if !errors.Is(err, ErrNoLongerEligible) {
		t.Fatalf("PurgeOrg err = %v, want ErrNoLongerEligible", err)
	}
	if got := strings.Join(runs.attempts, ","); got != "r-1" {
		t.Errorf("runs a delete was attempted on: %s, want r-1 alone — the purge went on deleting the runs of a restored org", got)
	}
	if n != 1 {
		t.Errorf("PurgeOrg reported %d deleted, want 1", n)
	}
	got, gerr := st.GetOrg(context.Background(), org.ID)
	if gerr != nil || got.EffectiveStatus() != identity.TeamStatusActive {
		t.Errorf("the restored org must stay active: %+v, %v", got, gerr)
	}
}

// TestSweep_ARestoreAfterTheLastRunStopsBeforeTheMongoSweep: the restore lands
// while the org's LAST run is being deleted, so the per-run re-read has
// nothing left to stop. The re-read before the first collection sweep is what
// stops it (the nil DB would otherwise be reached), and the sweep reports the
// stop as a warning naming the runs already gone, not as a purged org.
func TestSweep_ARestoreAfterTheLastRunStopsBeforeTheMongoSweep(t *testing.T) {
	defer mongoMustNotBeReached(t, "restore after the last run")
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-restore-last", "team-rl")
	runs := newFakeRunStore()
	runs.addRun("r-only", "team-rl", 2)
	runs.onDelete = func(string) { restoreOrg(t, st, org.ID) }
	var logs bytes.Buffer
	logger := log.New(log.LevelInfo, &logs)
	s := &Sweeper{
		Purger: &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t), Logger: logger},
		Logger: logger,
	}
	purged, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if purged != 0 {
		t.Errorf("Sweep counted %d purged org(s), want 0 — the org was restored", purged)
	}
	out := logs.String()
	if !strings.Contains(out, "left purge eligibility mid-purge") || !strings.Contains(out, "1 run(s) already deleted") {
		t.Errorf("the stopped purge is not reported with the runs it already deleted; log:\n%s", out)
	}
	if strings.Contains(out, "purged org "+org.ID) {
		t.Errorf("a restored org is reported purged; log:\n%s", out)
	}
}

// TestPurgeOrg_ANonTerminalRunIsNotDeleted: a run still running at the
// deadline is refused by the lifecycle guard every delete authority crosses
// (store.RunDeletable) — no DeleteRun is called on it, while the engine may
// still write its objects — and it withholds the sweep like a failed delete.
func TestPurgeOrg_ANonTerminalRunIsNotDeleted(t *testing.T) {
	defer mongoMustNotBeReached(t, "a running run")
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-live", "team-live")
	runs := newFakeRunStore()
	runs.addRunWithStatus("r-live", "team-live", 2, store.RunStatusRunning)
	runs.addRun("r-done", "team-live", 1)
	p := &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t)}
	n, err := p.PurgeOrg(context.Background(), org.ID)
	if !errors.Is(err, store.ErrRunNotDeletable) {
		t.Fatalf("PurgeOrg err = %v, want the run's ErrRunNotDeletable wrapped", err)
	}
	if !strings.Contains(err.Error(), "r-live") || !strings.Contains(err.Error(), "withheld") {
		t.Errorf("error %q does not name the live run and the withheld sweep", err)
	}
	if got := strings.Join(runs.attempts, ","); got != "r-done" {
		t.Errorf("runs a delete was attempted on: %s, want r-done alone — a running run was deleted under the engine", got)
	}
	if n != 1 {
		t.Errorf("deleted %d run(s), want 1 (the finished one)", n)
	}
}

// TestPurgeOrg_TheRunBatchCountsFailedAttempts: under a bucket outage every
// delete fails. A batch of one bounds the pass to ONE attempt, not to one
// success — counting successes alone would try every run of the org.
func TestPurgeOrg_TheRunBatchCountsFailedAttempts(t *testing.T) {
	defer mongoMustNotBeReached(t, "batch under an outage")
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-outage", "team-out")
	runs := newFakeRunStore()
	outage := errors.New("s3: 503 slow down")
	for _, id := range []string{"r-1", "r-2", "r-3", "r-4"} {
		runs.addRun(id, "team-out", 1)
		runs.failing[id] = outage
	}
	p := &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t), RunBatch: 1}
	_, err := p.PurgeOrg(context.Background(), org.ID)
	if !errors.Is(err, outage) || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("PurgeOrg err = %v, want the withheld sweep wrapping the outage", err)
	}
	if len(runs.attempts) != 1 {
		t.Errorf("DeleteRun attempts with a batch of 1 under an outage: %d (%v), want 1", len(runs.attempts), runs.attempts)
	}
}

// TestPurgeRuns_ForceSweepGoesPastTheRunsItCannotDelete: the operator's escape
// hatch. For a forced org a failing run and a running one no longer withhold
// the sweep: both are named in a warning, every other run is deleted in the
// same pass whatever the batch, and the step returns no error. Another org is
// not forced by it.
func TestPurgeRuns_ForceSweepGoesPastTheRunsItCannotDelete(t *testing.T) {
	st := identity.NewMemoryStore()
	org := pendingOrg(t, st, "org-forced", "team-fo")
	runs := newFakeRunStore()
	runs.addRun("r-1-stuck", "team-fo", 1)
	runs.failing["r-1-stuck"] = errors.New("s3: AccessDenied")
	runs.addRunWithStatus("r-2-live", "team-fo", 1, store.RunStatusPausedOperator)
	runs.addRun("r-3", "team-fo", 1)
	runs.addRun("r-4", "team-fo", 1)
	teams, err := st.ListTeamsByOrg(context.Background(), org.ID)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	p := &Purger{Store: st, Runs: runs, RunBatch: 1, Logger: log.New(log.LevelInfo, &logs),
		ForceSweep: map[string]bool{org.ID: true}}
	n, err := p.purgeRuns(context.Background(), org, teams)
	if err != nil {
		t.Fatalf("purgeRuns of a forced org: %v, want nil — the force did not let the sweep go on", err)
	}
	if n != 2 {
		t.Errorf("deleted %d run(s), want 2 (r-3, r-4 in one pass despite the batch of 1)", n)
	}
	out := logs.String()
	for _, want := range []string{ForceSweepEnv, "r-1-stuck", "r-2-live"} {
		if !strings.Contains(out, want) {
			t.Errorf("the forced sweep's warning does not name %q; log:\n%s", want, out)
		}
	}

	other := pendingOrg(t, st, "org-not-forced", "team-nf")
	runs.addRun("r-nf", "team-nf", 1)
	runs.failing["r-nf"] = errors.New("s3: AccessDenied")
	oteams, err := st.ListTeamsByOrg(context.Background(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.purgeRuns(context.Background(), other, oteams); err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Errorf("an org absent from the force list: err = %v, want the withheld sweep", err)
	}
}

// TestSweeperRun_HandsTheTermOverWhenTheBucketIsUnreachable: the elected
// sweeper reports each sweep's verdict to the lease health. A purge whose run
// deletes fail on a transport error (this replica cannot reach the bucket) is
// an unreachable pass, and with K=1 Run returns at once — releasing the lease
// to a peer — instead of sleeping until the next night with the net down.
func TestSweeperRun_HandsTheTermOverWhenTheBucketIsUnreachable(t *testing.T) {
	st := identity.NewMemoryStore()
	pendingOrg(t, st, "org-egress", "team-eg")
	runs := newFakeRunStore()
	runs.addRun("r-1", "team-eg", 1)
	runs.failing["r-1"] = fmt.Errorf("s3: dial: %w", syscall.ECONNREFUSED)
	s := &Sweeper{Purger: &Purger{DB: nil, Store: st, Runs: runs, Cascade: cascadeMustNotRun(t)}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, &lease.Health{Passes: 1}) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Run kept the term through an unreachable sweep — the lease is never handed to a peer")
	}
	if ctx.Err() != nil {
		t.Fatal("Run returned only on the context, not on the health's handover")
	}
}

func TestForceSweepFromEnv(t *testing.T) {
	t.Setenv(ForceSweepEnv, "")
	if got := ForceSweepFromEnv(); len(got) != 0 {
		t.Errorf("empty: %v, want nothing forced", got)
	}
	t.Setenv(ForceSweepEnv, " org-a, ,org-b ,")
	got := ForceSweepFromEnv()
	if len(got) != 2 || !got["org-a"] || !got["org-b"] {
		t.Errorf("list: %v, want org-a and org-b", got)
	}
}
