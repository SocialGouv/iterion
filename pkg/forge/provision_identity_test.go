package forge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

type heldIdentityAdmin struct {
	*fakeAdmin
	hold    atomic.Bool
	entered chan HookSpec
	release chan struct{}
}

func (a *heldIdentityAdmin) wait(spec HookSpec) {
	if a.hold.CompareAndSwap(true, false) {
		a.entered <- spec
		<-a.release
	}
}
func (a *heldIdentityAdmin) CreateHook(ctx context.Context, repo string, spec HookSpec) (HookHandle, error) {
	a.wait(spec)
	return a.fakeAdmin.CreateHook(ctx, repo, spec)
}
func (a *heldIdentityAdmin) UpdateHook(ctx context.Context, repo, id string, spec HookSpec) (HookHandle, error) {
	a.wait(spec)
	return a.fakeAdmin.UpdateHook(ctx, repo, id, spec)
}

// The first request applies LAST, after its lease expires and its successor
// finishes. The oracle reads the actual final forge secret, not either caller's
// intended token. Exercise both a fresh reservation and an existing live hook.
func TestProvisionExpiredLeaseKeepsActualForgeSecretVerifiable(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{true: "fresh", false: "existing"}[fresh], func(t *testing.T) {
			o, fa, sealer := newTestOrch(t)
			seedConn(t, o, sealer)
			o.NewID = nil
			var clock atomic.Int64
			clock.Store(1700000000)
			o.Now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
			o.ProvisionLeaseTTL = time.Hour
			req := ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}}
			if !fresh {
				if _, err := o.Provision(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			}
			a := &heldIdentityAdmin{fakeAdmin: fa, entered: make(chan HookSpec, 1), release: make(chan struct{})}
			a.hold.Store(true)
			o.AdminFor = func(context.Context, Connection) (Admin, error) { return a, nil }
			req.BotIDs = []string{"dep-guard"}
			done := make(chan error, 1)
			go func() { _, err := o.Provision(context.Background(), req); done <- err }()
			var first HookSpec
			select {
			case first = <-a.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first request never reached hook")
			}
			if _, err := o.Provision(context.Background(), req); !errors.Is(err, ErrProvisionBusy) {
				close(a.release)
				<-done
				t.Fatalf("unexpired lease admitted overlapping provision: %v", err)
			}
			clock.Add(7200)
			secondReq := req
			secondReq.BotIDs = []string{"revi-converse"}
			second, err := o.Provision(context.Background(), secondReq)
			close(a.release)
			firstErr := <-done
			if err != nil {
				t.Fatalf("successor: %v", err)
			}
			if !errors.Is(firstErr, ErrIntegrationConflict) {
				t.Fatalf("stale activation must conflict: %v", firstErr)
			}
			cfg, err := o.Webhooks.Get(context.Background(), second.WebhookID)
			if err != nil {
				t.Fatal(err)
			}
			if !webhooks.VerifyToken(fa.lastSecret, cfg.TokenHash) {
				t.Fatal("actual final forge secret fails verification after the late write")
			}
			if fa.lastSecret != first.Secret {
				t.Fatal("hostile fixture did not apply the first write last")
			}
			ri, err := o.Integrations.Get(context.Background(), second.IntegrationID)
			if err != nil || ri.ProvisionPending || !sameSet(ri.BotIDs, second.BotIDs) {
				t.Fatalf("stale finalization clobbered successor: %+v %v", ri, err)
			}
		})
	}
}

func TestProvisionRetirementUsesNewEpoch(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}}
	first, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	old, err := o.Integrations.Get(ctx, first.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Deprovision(ctx, "t1", first.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Integrations.Get(ctx, first.IntegrationID); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("retired row operational: %v", err)
	}
	second, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.WebhookID == second.WebhookID {
		t.Fatal("retirement reused the old webhook epoch")
	}
	if err := o.Integrations.Update(ctx, old); !errors.Is(err, ErrIntegrationConflict) {
		t.Fatalf("old version resurrected: %v", err)
	}
	cfg, _ := o.Webhooks.Get(ctx, second.WebhookID)
	if !webhooks.VerifyToken(fa.lastSecret, cfg.TokenHash) {
		t.Fatal("reenabled hook secret does not verify")
	}
}

func TestProvisionLegacyMigratesWithoutChangingOldSecret(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}}
	first, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	oldSecret := fa.lastSecret
	ri, _ := o.Integrations.Get(ctx, first.IntegrationID)
	ri.WebhookSecretSealed = nil
	if err := o.Integrations.Update(ctx, ri); err != nil {
		t.Fatal(err)
	}
	cfg, _ := o.Webhooks.Get(ctx, first.WebhookID)
	cfg.ProvisionIntegrationID = ""
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	req.BotIDs = []string{"revi-converse"}
	second, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.WebhookID == first.WebhookID {
		t.Fatal("legacy migration overwrote the active hook identity")
	}
	oldCfg, _ := o.Webhooks.Get(ctx, first.WebhookID)
	if !webhooks.VerifyToken(oldSecret, oldCfg.TokenHash) || oldCfg.ProvisionIntegrationID != first.IntegrationID {
		t.Fatal("legacy verification tombstone lost")
	}
	active, _ := o.Integrations.Get(ctx, first.IntegrationID)
	if active.WebhookID != second.WebhookID || active.ProvisionPending {
		t.Fatal("candidate not atomically activated")
	}
}
