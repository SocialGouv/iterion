package cloudpublisher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A resume's pool acquisition supersedes the lease of the run's previous
// attempt — which the queue's pending redelivery of that attempt still runs
// on (an interim report keeps it open for exactly that). These tests drive
// SubmitResume and the broker's own report, the runner's seam.

var leaseWant = []credpool.Credential{{Source: credpool.SourceOAuth, Ref: "claude_code"}}

var leasePledge = credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code")

// leaseFixture is a run whose previous attempt holds the donor's lease after
// an interim report of $1 (a drain), parked failed_resumable with its
// redelivery pending, and a publisher whose publication publish answers.
func leaseFixture(t *testing.T, publish func(context.Context, *queue.RunMessage) error) (*poolFixture, store.RunStore, string) {
	t.Helper()
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1})
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-1"
	if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: poolTeam, OwnerID: "requester", Status: store.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pub.credPool.Acquire(ctx, credpool.Request{RunID: runID, OrgID: poolOrg, TenantID: poolTeam, UserID: "requester", Wants: leaseWant}); err != nil {
		t.Fatalf("the previous attempt's acquire: %v", err)
	}
	if err := f.pub.credPool.Report(ctx, runID, credpool.Outcome{CostUSD: 1, Interim: true}); err != nil {
		t.Fatalf("the previous attempt's interim report: %v", err)
	}
	if ok, err := st.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, "drain", store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending}, []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
		t.Fatalf("park: %v %v", ok, err)
	}
	f.pub.store = st
	f.pub.identity = &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg}}}
	f.pub.publishRetryDelays = []time.Duration{}
	f.pub.publishRun = publish
	return f, st, runID
}

func leaseResume(t *testing.T, f *poolFixture, runID string) error {
	t.Helper()
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	cs := rollbackTestSource()
	return f.pub.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: cs.Files["main.bot"]}, &ir.Workflow{Name: "w"}, cs)
}

func setPledgeEnabled(t *testing.T, f *poolFixture, enabled bool) {
	t.Helper()
	ctx := context.Background()
	p, err := f.pledges.Get(ctx, leasePledge)
	if err != nil {
		t.Fatal(err)
	}
	p.Enabled = enabled
	if err := f.pledges.Upsert(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// assertThePendingRedeliveryKeepsItsLease: the redelivery of the previous
// attempt still holds the donor (a second run is refused at
// MaxConcurrentRuns 1), and its final report is charged.
func assertThePendingRedeliveryKeepsItsLease(t *testing.T, f *poolFixture, runID string) {
	t.Helper()
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	if g, err := f.pub.credPool.Acquire(ctx, credpool.Request{RunID: "run-2", OrgID: poolOrg, TenantID: poolTeam, UserID: "requester", Wants: leaseWant}); err == nil {
		t.Errorf("a second run was granted the donor (MaxConcurrentRuns 1) while run-1's pending redelivery still runs on it: %+v", g)
	}
	if err := f.pub.credPool.Report(ctx, runID, credpool.Outcome{CostUSD: 2}); err != nil {
		t.Fatalf("the redelivery's report: %v", err)
	}
	day, _, err := f.ledger.Usage(ctx, leasePledge, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if day.CostUSD != 3 {
		t.Errorf("the donor was charged $%v for $3 of spend: the redelivery's $2 found no open lease", day.CostUSD)
	}
}

// TestSubmitResume_aRefusedResumeGivesTheLeaseBackToThePendingRedelivery: the
// resume acquires the donor (superseding the previous attempt's lease), then
// its publication fails. Its rollback releases its own grant AND gives the
// previous attempt its lease back.
func TestSubmitResume_aRefusedResumeGivesTheLeaseBackToThePendingRedelivery(t *testing.T) {
	f, _, runID := leaseFixture(t, func(context.Context, *queue.RunMessage) error { return errors.New("nats unavailable") })
	if err := leaseResume(t, f, runID); err == nil {
		t.Fatal("the resume succeeded: want its publish failure")
	}
	assertThePendingRedeliveryKeepsItsLease(t, f, runID)
}

// TestSubmitResume_aRefusedResumeWithoutADonorLeavesThePendingRedeliveryItsLease:
// the same, when the resume's acquisition finds no donor — it superseded the
// previous attempt's lease all the same before looking.
func TestSubmitResume_aRefusedResumeWithoutADonorLeavesThePendingRedeliveryItsLease(t *testing.T) {
	f, _, runID := leaseFixture(t, func(context.Context, *queue.RunMessage) error { return errors.New("nats unavailable") })
	setPledgeEnabled(t, f, false)
	if err := leaseResume(t, f, runID); err == nil {
		t.Fatal("the resume succeeded: want its publish failure")
	}
	setPledgeEnabled(t, f, true)
	assertThePendingRedeliveryKeepsItsLease(t, f, runID)
}

// TestSubmitResume_aPublishedResumeWithoutADonorSupersedesThePreviousLease: a
// resume published without a pool grant ends the previous attempt — its
// delivery is superseded — so that attempt's lease is closed, and the donor's
// slot is free for another run.
func TestSubmitResume_aPublishedResumeWithoutADonorSupersedesThePreviousLease(t *testing.T) {
	f, _, runID := leaseFixture(t, func(context.Context, *queue.RunMessage) error { return nil })
	setPledgeEnabled(t, f, false)
	if err := leaseResume(t, f, runID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	if l, err := f.leases.GetOpenByRun(ctx, runID); !errors.Is(err, credpool.ErrNotFound) {
		t.Fatalf("after the resume's publication, the previous attempt's lease is still open (%+v, %v): its donor's slot is held until the lease TTL", l, err)
	}
	setPledgeEnabled(t, f, true)
	if _, err := f.pub.credPool.Acquire(ctx, credpool.Request{RunID: "run-2", OrgID: poolOrg, TenantID: poolTeam, UserID: "requester", Wants: leaseWant}); err != nil {
		t.Fatalf("the donor's slot is still held after the run moved on: %v", err)
	}
}

// TestSubmitResume_aResumeOutrunByANewerOneDoesNotReopenTheLease: resume A
// acquires the donor and is slow to publish; an operator cancels the run and
// resumes it (B), which acquires and publishes. A then fails: its flip is not
// reverted — B owns the run — so the lease A superseded stays superseded, and
// B's is the run's only open lease.
func TestSubmitResume_aResumeOutrunByANewerOneDoesNotReopenTheLease(t *testing.T) {
	aInPublish, releaseA := make(chan struct{}), make(chan struct{})
	f, _, runID := leaseFixture(t, func(_ context.Context, m *queue.RunMessage) error {
		if m.Resume != nil && m.Resume.PriorStatus == store.RunStatusFailedResumable {
			close(aInPublish)
			<-releaseA
			return errors.New("nats: publish ack timeout (A)")
		}
		return nil
	})
	f.pub.cancelRun = func(string, store.LeaseIdentity) error { return nil }
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	before, err := f.leases.GetOpenByRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	aErr := make(chan error, 1)
	go func() { aErr <- leaseResume(t, f, runID) }()
	<-aInPublish
	if err := f.pub.CancelRun(ctx, runID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // a marker is to the millisecond (store.QueuedFlipAt)
	if err := leaseResume(t, f, runID); err != nil {
		t.Fatalf("B's resume: %v", err)
	}
	close(releaseA)
	if err := <-aErr; err == nil {
		t.Fatal("A succeeded: want its publish failure")
	}
	open, err := f.leases.ListOpenByRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID == before.ID {
		t.Fatalf("open leases of the run after A's failure: %+v — want B's alone; the previous attempt's (%s) was reopened under B", open, before.ID)
	}
}
