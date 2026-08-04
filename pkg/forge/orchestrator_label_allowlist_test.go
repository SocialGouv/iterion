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
	// every FURTHER provision: the config adoption re-fires only while the
	// integration's own allowlist is empty, so persisting it here is what ends
	// the dependency on the config still carrying the operator's choice.
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

// TestProvisionWidensALabelAllowlistThatLivesOnlyOnTheConfig: the legacy state
// this branch exists to handle — the narrowing sits on the webhook config while
// the integration stores none. An operator widening it back with an explicit
// empty list matches the (empty) integration, so a mutation check that only
// compared the two would find no change, skip the write, and return 200 with
// the lane still narrowed: the API would report a widening that never reached
// the surface enforcing it.
func TestProvisionWidensALabelAllowlistThatLivesOnlyOnTheConfig(t *testing.T) {
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
	cfg.LabelAllowlist = []string{"implement"}
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Same bots, same events → the idempotent short-circuit, with an explicit
	// widening in the request.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1", LabelAllowlist: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.LabelAllowlist) != 0 {
		t.Errorf("the explicit widening never reached the webhook config: %v — the lane "+
			"stays narrowed while the API reports it open", after.LabelAllowlist)
	}
}

// TestProvisionKeepsAConfigOnlyOverlapWhileAdoptingAnAllowlist: adopting the
// label allowlist makes the idempotent path WRITE where it used to no-op, so
// every other operator field the write stamps must resolve from the config too.
// Overlap is the sharp one: it is fail-open (no policy = launch every delivery),
// and a repo that set it the documented way (a webhook PATCH) stores nothing on
// the integration — so a write driven by the allowlist would stamp "" over it.
func TestProvisionKeepsAConfigOnlyOverlapWhileAdoptingAnAllowlist(t *testing.T) {
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
	cfg.Overlap = "supersede"
	cfg.LabelAllowlist = []string{"implement"}
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// A studio bot-set PATCH: same bots, no operator field in the request.
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
	if after.Overlap != "supersede" {
		t.Errorf("overlap dropped to %q — an empty policy launches every delivery, "+
			"so a re-review webhook now runs a bot per push in a burst", after.Overlap)
	}
	if !slices.Equal(after.LabelAllowlist, []string{"implement"}) {
		t.Errorf("allowlist lost while keeping overlap: %v", after.LabelAllowlist)
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
