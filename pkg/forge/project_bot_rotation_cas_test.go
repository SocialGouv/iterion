package forge

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"testing"
)

type rotationSecretConflict struct {
	secrets.GenericSecretStore
	integrations RepoIntegrationStore
	id           string
	fail         bool
}

func (s *rotationSecretConflict) Update(ctx context.Context, gs secrets.GenericSecret) error {
	if s.fail {
		s.fail = false
		ri, e := s.integrations.Get(ctx, s.id)
		if e != nil {
			return e
		}
		ri.SyncIssuesEnabled = true
		if e = s.integrations.Update(ctx, ri); e != nil {
			return e
		}
		return errors.New("injected one-shot secret store failure")
	}
	return s.GenericSecretStore.Update(ctx, gs)
}
func TestRotationCASPreservesIssuedCredentialAndOperatorChange(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	o.Secrets = &rotationSecretConflict{GenericSecretStore: o.Secrets, integrations: o.Integrations, id: ri.ID, fail: true}
	n, e := o.RotateDueProjectBots(ctx, 0)
	t.Logf("first sweep n=%d err=%v", n, e)
	got, _ := o.Integrations.Get(ctx, ri.ID)
	live := f.live()
	if len(live) != 1 {
		t.Fatalf("rotation lost the issued credential: live=%d", len(live))
	}
	t.Logf("durable tokenID=%s version=%d sync=%v; live forge tokens=%d liveID=%s oldRevoked=%v", got.ProjectBot.TokenID, got.Version, got.SyncIssuesEnabled, len(live), live[0].id, f.tokens[ri.ProjectBot.TokenID].revoked)
	for i := 0; i < 2; i++ {
		n, e = o.RotateDueProjectBots(ctx, 0)
		t.Logf("retry %d n=%d err=%v", i+1, n, e)
	}
	got, _ = o.Integrations.Get(ctx, ri.ID)
	if !got.SyncIssuesEnabled {
		t.Fatal("rotation overwrote concurrent operator setting")
	}
	if e != nil {
		t.Fatalf("rotation did not recover: %v", e)
	}
	if got.ProjectBot.TokenID == ri.ProjectBot.TokenID && len(f.live()) == 1 {
		t.Fatalf("LOST LIVE TOKEN: retry keeps revoked token ID %s; live forge token %s is neither recorded nor reachable from stored plaintext", got.ProjectBot.TokenID, f.live()[0].id)
	}
}
