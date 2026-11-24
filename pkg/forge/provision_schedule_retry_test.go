package forge

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"testing"
)

type projectionVersionChange struct {
	RepoIntegrationStore
	once bool
}

func (s *projectionVersionChange) WithProvisionVersion(ctx context.Context, id string, v int64, retired bool, write func(context.Context) error) error {
	if !retired && !s.once {
		s.once = true
		ri, e := s.GetProvision(ctx, id)
		if e != nil {
			return e
		}
		ri.AutoFixOnGateFailure = true
		if e = s.Update(ctx, ri); e != nil {
			return e
		}
	}
	return s.RepoIntegrationStore.WithProvisionVersion(ctx, id, v, retired, write)
}
func TestProvisionProjectionConflictRetryRepairsSchedules(t *testing.T) {
	ctx := context.Background()
	o, _, seal := newTestOrch(t)
	seedConn(t, o, seal)
	o.Schedules = cloudsched.NewMemoryStore()
	o.Integrations = &projectionVersionChange{RepoIntegrationStore: o.Integrations}
	o.Invocations = func(context.Context, string, string) ([]bundle.Invocation, error) {
		return []bundle.Invocation{{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"}}}, nil
	}
	req := ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"}
	_, err := o.Provision(ctx, req)
	if !errors.Is(err, ErrIntegrationConflict) {
		t.Fatalf("expected fence conflict: %v", err)
	}
	t.Logf("first provision: %v", err)
	res, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := o.Schedules.ListByIntegration(ctx, "t1", res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("same-request retry success, schedules=%d", len(rows))
	if len(rows) != 1 {
		t.Fatalf("retry did not repair schedules=%d", len(rows))
	}
}
