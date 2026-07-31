package forge

import (
	"context"
	"testing"
)

// TestProvisionKeepsTheOperatorHoldLabels: Provision rebuilds the whole webhook
// config from the manifests, so anything the operator set only on the config is
// wiped by the next `bots enable` — the reason LaunchVars and Overlap are
// persisted on the integration. Hold labels were not, and they are the
// bot-agnostic pause: the ONLY brake on the zero-touch auto-fix lane, which
// pushes code with nobody watching. Enabling one more bot silently disarmed it.
func TestProvisionKeepsTheOperatorHoldLabels(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The operator PATCHes the pause onto the provisioned webhook.
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HoldLabels = []string{"iterion:hold"}
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Later: one more bot is enabled on the same repo (the full, non
	// short-circuit provision path).
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}

	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.HoldLabels) == 0 {
		t.Errorf("HoldLabels were wiped by the re-provision (%v). The auto-fix lane "+
			"only consults them when len(cfg.HoldLabels) > 0, so the operator's pause "+
			"on an unattended code-pushing bot silently stops applying.", after.HoldLabels)
	}
}

// TestProvisionKeepsAutoFixOptIn: the zero-touch opt-in must survive the same
// path, and must never be switched ON by a provision that says nothing about
// it — enabling one more bot is not consent to unattended code pushes.
func TestProvisionKeepsAutoFixOptIn(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.AutoFixOnGateFailure {
		t.Fatal("a fresh provision must not enable the zero-touch lane")
	}

	on := true
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1", AutoFix: &on,
	}); err != nil {
		t.Fatal(err)
	}
	// A later call that says nothing about it must leave it alone, in BOTH
	// directions: silently switching automation off is as wrong as on.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"revi-converse"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	integ, err = o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !integ.AutoFixOnGateFailure {
		t.Error("enabling another bot silently switched the zero-touch lane back off")
	}
}
