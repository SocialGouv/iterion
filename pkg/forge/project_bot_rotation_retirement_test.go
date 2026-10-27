package forge

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"testing"
)

type retireBeforeRotationSecret struct {
	secrets.GenericSecretStore
	before func()
	once   bool
}

func (s *retireBeforeRotationSecret) Update(ctx context.Context, gs secrets.GenericSecret) error {
	if !s.once {
		s.once = true
		s.before()
	}
	return s.GenericSecretStore.Update(ctx, gs)
}
func TestRotationRetirementRevokesUnrecordedToken(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	o.Secrets = &retireBeforeRotationSecret{GenericSecretStore: o.Secrets, before: func() {
		if e := o.Deprovision(ctx, ri.TenantID, ri.ID); e != nil {
			t.Fatalf("deprovision failed: %v", e)
		}
	}}
	n, e := o.RotateDueProjectBots(ctx, 0)
	t.Logf("rotation result n=%d err=%v", n, e)
	tomb, e := o.Integrations.GetProvision(ctx, ri.ID)
	if e != nil {
		t.Fatal(e)
	}
	_, se := o.Secrets.Get(ctx, ri.ManagedSecretID)
	t.Logf("retired=%v cleanupComplete=%v durable tokenID=%s secret=%v live=%d", tomb.Retired, tomb.RetireCleanupComplete, tomb.ProjectBot.TokenID, se, len(f.live()))
	if e = o.Deprovision(ctx, ri.TenantID, ri.ID); e != nil {
		t.Fatal(e)
	}
	n, e = o.RotateDueProjectBots(ctx, 0)
	t.Logf("retry deprovision success; next sweep n=%d err=%v live=%d", n, e, len(f.live()))
	if live := f.live(); len(live) != 0 {
		t.Fatalf("ORPHAN live token %s remains after successful retirement cleanup and retry; recorded token=%s, no retained secret", live[0].id, tomb.ProjectBot.TokenID)
	}
}
func TestRotationReactivationRevokesOldEpochToken(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	o.Secrets = &retireBeforeRotationSecret{GenericSecretStore: o.Secrets, before: func() {
		if e := o.Deprovision(ctx, ri.TenantID, ri.ID); e != nil {
			t.Fatal(e)
		}
		_, e := o.Provision(ctx, ProvisionRequest{TenantID: ri.TenantID, ConnectionID: conn.ID, RepoFullName: ri.RepoFullName, BotIDs: []string{"review-pr"}, ActorID: "u1"})
		if e != nil {
			t.Fatal(e)
		}
	}}
	n, e := o.RotateDueProjectBots(ctx, 0)
	t.Logf("old rotation n=%d err=%v", n, e)
	current, e := o.Integrations.Get(ctx, ri.ID)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("active token=%s old secret=%s new secret=%s live=%d", current.ProjectBot.TokenID, ri.ManagedSecretID, current.ManagedSecretID, len(f.live()))
	if e = o.Deprovision(ctx, ri.TenantID, ri.ID); e != nil {
		t.Fatal(e)
	}
	if live := f.live(); len(live) != 0 {
		t.Fatalf("ORPHAN token %s survives cleanup of both epochs; old recorded=%s replacement recorded=%s", live[0].id, ri.ProjectBot.TokenID, current.ProjectBot.TokenID)
	}
}
