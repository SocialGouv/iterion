package forge

import (
	"context"
	"testing"
)

func TestRotationDisconnectRevokesUnrecordedToken(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	o.Secrets = &retireBeforeRotationSecret{GenericSecretStore: o.Secrets, before: func() {
		if e := o.DeprovisionConnection(ctx, ri.TenantID, conn.ID); e != nil {
			t.Fatalf("disconnect: %v", e)
		}
	}}
	n, e := o.RotateDueProjectBots(ctx, 0)
	t.Logf("rotation n=%d err=%v", n, e)
	_, ce := o.Connections.Get(ctx, conn.ID)
	tomb, te := o.Integrations.GetProvision(ctx, ri.ID)
	if te != nil {
		t.Fatal(te)
	}
	t.Logf("connection=%v retired=%v cleanup=%v durableToken=%s live=%d", ce, tomb.Retired, tomb.RetireCleanupComplete, tomb.ProjectBot.TokenID, len(f.live()))
	if live := f.live(); len(live) != 0 {
		t.Fatalf("ORPHAN %s: compensation cannot re-read deleted connection although rotation already held provider/base URL", live[0].id)
	}
}
