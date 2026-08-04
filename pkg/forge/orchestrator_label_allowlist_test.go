package forge

import (
	"context"
	"slices"
	"testing"
)

// TestProvisionKeepsTheOperatorLabelAllowlist: narrowing the issue-labeled lane
// (only `implement` dispatches the implementer, not every triage label) is a
// webhook-config PATCH, and Provision rebuilds that config from the manifests.
// Enabling one more bot therefore wiped the narrowing — silently, and fail-OPEN:
// the repo went back to "any label on any issue starts a feature-dev campaign".
func TestProvisionKeepsTheOperatorLabelAllowlist(t *testing.T) {
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
	// The operator narrows the lane the documented way: a PATCH on the webhook.
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LabelAllowlist = []string{"implement"}
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Later: one more bot is enabled on the same repo — the full (non
	// short-circuit) provision path, which rewrites the config as a whole.
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
	if !slices.Equal(after.LabelAllowlist, []string{"implement"}) {
		t.Errorf("LabelAllowlist was wiped by the re-provision (%v). An empty allowlist "+
			"matches EVERY label, so the repo silently returns to dispatching the "+
			"implementer on any label an operator adds to any issue.", after.LabelAllowlist)
	}
	// It must also land on the integration, which is what makes it durable for
	// every FURTHER provision (the config adoption above only fires once).
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(integ.LabelAllowlist, []string{"implement"}) {
		t.Errorf("the adopted allowlist was not persisted on the integration: %v", integ.LabelAllowlist)
	}
}

// TestProvisionKeepsLabelAllowlistOnTheIdempotentPath: re-enabling the SAME
// bots takes the short-circuit, which writes the operator fields back onto the
// config. It must carry the allowlist too — otherwise the cheap no-op path is
// exactly what re-widens the lane.
func TestProvisionKeepsLabelAllowlistOnTheIdempotentPath(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1", LabelAllowlist: []string{"implement"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.LabelAllowlist, []string{"implement"}) {
		t.Errorf("the idempotent re-provision widened the issue lane back to any label: %v", after.LabelAllowlist)
	}
}

// TestProvisionSetsAndClearsLabelAllowlist: the operator owns the field in both
// directions. Nil says nothing (keep), a non-nil list narrows, and a non-nil
// EMPTY list is the explicit widening — without that distinction there is no
// way back to "any label" once a repo has been narrowed.
func TestProvisionSetsAndClearsLabelAllowlist(t *testing.T) {
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
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LabelAllowlist) != 0 {
		t.Fatalf("a fresh provision must not invent an allowlist: %v", cfg.LabelAllowlist)
	}

	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1", LabelAllowlist: []string{"implement", "ship-it"},
	}); err != nil {
		t.Fatal(err)
	}
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(integ.LabelAllowlist, []string{"implement", "ship-it"}) {
		t.Fatalf("allowlist not applied: %v", integ.LabelAllowlist)
	}

	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1", LabelAllowlist: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	integ, err = o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(integ.LabelAllowlist) != 0 {
		t.Fatalf("an explicit empty allowlist must widen the lane back: %v", integ.LabelAllowlist)
	}
	cfg, err = o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LabelAllowlist) != 0 {
		t.Fatalf("the widening did not reach the webhook config: %v", cfg.LabelAllowlist)
	}
}
