package forge

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"testing"
)

type policyPatchDuringReservation struct {
	RepoIntegrationStore
	configs   webhooks.ConfigStore
	webhookID string
	fired     bool
}

func (s *policyPatchDuringReservation) Update(ctx context.Context, ri RepoIntegration) error {
	if ri.ProvisionPending && !s.fired {
		s.fired = true
		cfg, err := s.configs.Get(ctx, s.webhookID)
		if err != nil {
			return err
		}
		cfg.LabelAllowlist = []string{"approved-only"}
		if err := s.configs.Update(ctx, cfg); err != nil {
			return err
		}
	}
	return s.RepoIntegrationStore.Update(ctx, ri)
}
func TestProvisionDoesNotBorrowVersionForStalePolicy(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	first := provisionBots(t, o, "review-pr")
	patch := &policyPatchDuringReservation{RepoIntegrationStore: o.Integrations, configs: o.Webhooks, webhookID: first.WebhookID}
	o.Integrations = patch
	_, err := o.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"revi-converse"}})
	if !patch.fired {
		t.Fatal("fixture did not patch the config")
	}
	if !errors.Is(err, webhooks.ErrConflict) {
		t.Fatalf("stale settings must conflict, got %v", err)
	}
	cfg, _ := o.Webhooks.Get(context.Background(), first.WebhookID)
	if !sameSet(cfg.LabelAllowlist, []string{"approved-only"}) {
		t.Fatal("provision erased concurrent operator restriction")
	}
}

type migrationPolicyAdmin struct {
	*fakeAdmin
	patch func()
}

func (a *migrationPolicyAdmin) CreateHook(ctx context.Context, repo string, spec HookSpec) (HookHandle, error) {
	a.patch()
	return a.fakeAdmin.CreateHook(ctx, repo, spec)
}

func TestProvisionMigrationFreezesSourcePolicy(t *testing.T) {
	ctx := context.Background()
	o, fa, seal := newTestOrch(t)
	seedConn(t, o, seal)
	first := provisionBots(t, o, "review-pr")
	ri, err := o.Integrations.Get(ctx, first.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	ri.WebhookSecretSealed = nil
	if err = o.Integrations.Update(ctx, ri); err != nil {
		t.Fatal(err)
	}
	patched := false
	a := &migrationPolicyAdmin{fakeAdmin: fa, patch: func() {
		cfg, err := o.Webhooks.Get(ctx, first.WebhookID)
		if err != nil {
			t.Fatal(err)
		}
		cfg.LabelAllowlist = []string{"approved-only"}
		if err = o.Webhooks.Update(ctx, cfg); !errors.Is(err, webhooks.ErrConflict) {
			t.Fatalf("migration source accepted policy PATCH that activation would discard: %v", err)
		}
		patched = true
	}}
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return a, nil }
	second := provisionBots(t, o, "revi-converse")
	if !patched || first.WebhookID == second.WebhookID {
		t.Fatal("migration fixture ineffective")
	}
	active, err := o.Webhooks.Get(ctx, second.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	active.LabelAllowlist = []string{"approved-only"}
	if err = o.Webhooks.Update(ctx, active); err != nil {
		t.Fatalf("new active policy cannot be edited: %v", err)
	}
	if err = o.Deprovision(ctx, "t1", second.IntegrationID); err != nil {
		t.Fatalf("migration blocked cleanup: %v", err)
	}
}
