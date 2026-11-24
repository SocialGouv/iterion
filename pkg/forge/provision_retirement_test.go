package forge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
)

type heldRetirementAdmin struct {
	*fakeAdmin
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (a *heldRetirementAdmin) DeleteHook(ctx context.Context, repo, id string) error {
	if a.hold.CompareAndSwap(true, false) {
		close(a.entered)
		<-a.release
	}
	// A provider deletes the captured hook ID, not whatever now owns the repo.
	if hook, ok := a.hooks[repo]; !ok || hook.ID != id {
		return ErrHookNotFound
	}
	return a.fakeAdmin.DeleteHook(ctx, repo, id)
}

func TestProvisionRetiredWorkerCannotDeleteSuccessorSchedule(t *testing.T) {
	ctx := context.Background()
	o, fa, seal := newTestOrch(t)
	seedConn(t, o, seal)
	o.NewID = nil
	var clock atomic.Int64
	clock.Store(1700000000)
	o.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
	o.ProvisionLeaseTTL = time.Hour
	o.Schedules = cloudsched.NewMemoryStore()
	first := provisionBots(t, o, "review-pr")
	if err := o.Schedules.Create(ctx, cloudsched.ScheduledBot{ID: "old-schedule", TenantID: "t1", RepoIntegrationID: first.IntegrationID, BotID: "review-pr"}); err != nil {
		t.Fatal(err)
	}
	a := &heldRetirementAdmin{fakeAdmin: fa, entered: make(chan struct{}), release: make(chan struct{})}
	a.hold.Store(true)
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return a, nil }
	done := make(chan error, 1)
	go func() { done <- o.Deprovision(ctx, "t1", first.IntegrationID) }()
	select {
	case <-a.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement not reached")
	}
	// Even after the lease expires, reactivation waits for credential cleanup.
	clock.Add(7200)
	req := ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}}
	if _, err := o.Provision(ctx, req); !errors.Is(err, ErrProvisionBusy) {
		close(a.release)
		<-done
		t.Fatalf("unfinished retirement admitted reactivation: %v", err)
	}
	if err := o.Deprovision(ctx, "t1", first.IntegrationID); err != nil {
		close(a.release)
		<-done
		t.Fatal(err)
	}
	second, err := o.Provision(ctx, req)
	if err != nil {
		close(a.release)
		<-done
		t.Fatal(err)
	}
	if err := o.Schedules.Create(ctx, cloudsched.ScheduledBot{ID: "new-schedule", TenantID: "t1", RepoIntegrationID: second.IntegrationID, BotID: "review-pr"}); err != nil {
		close(a.release)
		<-done
		t.Fatal(err)
	}
	close(a.release)
	if err := <-done; !errors.Is(err, ErrIntegrationConflict) {
		t.Fatalf("stale retirement must conflict: %v", err)
	}
	if _, err := o.Integrations.Get(ctx, second.IntegrationID); err != nil {
		t.Fatal("new epoch not active", err)
	}
	rows, err := o.Schedules.ListByIntegration(ctx, "t1", second.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "new-schedule" {
		t.Fatalf("retired worker deleted successor schedule: %+v", rows)
	}
}
