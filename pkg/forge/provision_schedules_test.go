package forge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
)

type heldProvisionProjection struct {
	RepoIntegrationStore
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *heldProvisionProjection) WithProvisionVersion(ctx context.Context, id string, version int64, retired bool, write func(context.Context) error) error {
	if !retired && s.hold.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.RepoIntegrationStore.WithProvisionVersion(ctx, id, version, retired, write)
}
func TestProvisionTailCannotReplaceSuccessorSchedules(t *testing.T) {
	ctx := context.Background()
	o, _, seal := newTestOrch(t)
	seedConn(t, o, seal)
	o.NewID = nil
	var clock atomic.Int64
	clock.Store(1700000000)
	o.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
	o.ProvisionLeaseTTL = time.Hour
	first := provisionBots(t, o, "review-pr")
	s := &heldProvisionProjection{RepoIntegrationStore: o.Integrations, entered: make(chan struct{}), release: make(chan struct{})}
	s.hold.Store(true)
	o.Integrations = s
	o.Schedules = cloudsched.NewMemoryStore()
	done := make(chan error, 1)
	go func() {
		_, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"revi-converse"}})
		done <- err
	}()
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("projection not reached")
	}
	clock.Add(7200)
	if err := o.Deprovision(ctx, "t1", first.IntegrationID); err != nil {
		close(s.release)
		<-done
		t.Fatal(err)
	}
	second, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}})
	if err != nil {
		close(s.release)
		<-done
		t.Fatal(err)
	}
	if err = o.Schedules.Create(ctx, cloudsched.ScheduledBot{ID: "successor", TenantID: "t1", RepoIntegrationID: second.IntegrationID, BotID: "review-pr"}); err != nil {
		close(s.release)
		<-done
		t.Fatal(err)
	}
	close(s.release)
	if err = <-done; !errors.Is(err, ErrIntegrationConflict) {
		t.Fatalf("stale projection was not refused: %v", err)
	}
	rows, err := o.Schedules.ListByIntegration(ctx, "t1", second.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "successor" {
		t.Fatalf("stale projection deleted successor schedules: %+v", rows)
	}
}
