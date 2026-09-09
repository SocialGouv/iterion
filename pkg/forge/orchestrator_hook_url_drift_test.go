package forge

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// TestReprovisionRepairsAHookLeftOnTheOldHost: the deployment's public URL is
// not a property of the request, so an already-provisioned repo can end up
// with a hook pointing at an address the forge still calls and iterion no
// longer answers on. Re-running provisioning is the gesture an operator
// reaches for to repair that, and it has to actually do it: the idempotent
// no-op compared bots and events only, so it returned 200 having changed
// nothing while every delivery kept going to the old host.
func TestReprovisionRepairsAHookLeftOnTheOldHost(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}
	before := hookURLFor(t, fa, "group/api")
	if !strings.HasPrefix(before, "https://iterion.example.com/") {
		t.Fatalf("setup: hook = %q", before)
	}

	// The deployment moves. Nothing about the repo or its bots changes.
	o.PublicURL = "https://iterion.cloud"

	// EXACTLY the same request an operator would re-send.
	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}

	got := hookURLFor(t, fa, "group/api")
	if !strings.HasPrefix(got, "https://iterion.cloud/api/webhooks/") {
		t.Fatalf("the hook was left on the old host (%q). Re-provisioning reported success "+
			"and changed nothing, so the repo's deliveries keep going somewhere iterion "+
			"no longer serves — and no error says so anywhere.", got)
	}
}

// TestReprovisionRewritesInPlace: the repair must not strand the old hook or
// leave the two ends disagreeing. It DOES mint a fresh iwh_ — that is the
// documented design (a mutating provision never needs the prior plaintext,
// and iterion holds both ends) — so what matters is not that the secret is
// stable but that the token handed to the forge is the one the stored config
// will accept. A rotation that reached only one end would authenticate
// nothing, and the symptom would be deliveries rejected at the door.
func TestReprovisionRewritesInPlace(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
	res, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	firstHook := fa.hooks["group/api"].ID
	createsBefore := fa.creates

	o.PublicURL = "https://iterion.cloud"
	res2, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	if fa.hooks["group/api"].ID != firstHook {
		t.Errorf("hook id changed (%s → %s): the old hook is stranded on the forge and the "+
			"new one carries a different secret", firstHook, fa.hooks["group/api"].ID)
	}
	if fa.creates != createsBefore {
		t.Errorf("CreateHook was called again (%d → %d); the repair must UPDATE the hook it owns",
			createsBefore, fa.creates)
	}
	if res2.WebhookID != res.WebhookID {
		t.Errorf("webhook id changed (%s → %s)", res.WebhookID, res2.WebhookID)
	}
	// The lockstep: whatever token went to the forge must be the one the
	// stored config accepts. Both ends move together or neither does.
	cfg, err := o.Webhooks.Get(ctx, res2.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !webhooks.VerifyToken(fa.lastSecret, cfg.TokenHash) {
		t.Error("the token registered on the forge does not verify against the stored config: " +
			"a rotation reached one end only, and every delivery would be refused at the door")
	}
}

// TestReprovisionStillNoOpsWhenNothingMoved: the comparison must not turn
// every re-provision into a forge write. A fleet-wide reconcile is only safe
// to re-run if it is silent where there is nothing to repair.
func TestReprovisionStillNoOpsWhenNothingMoved(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}
	updatesBefore, createsBefore := fa.updates, fa.creates

	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}
	if fa.updates != updatesBefore || fa.creates != createsBefore {
		t.Errorf("an unchanged re-provision touched the forge (updates %d→%d, creates %d→%d)",
			updatesBefore, fa.updates, createsBefore, fa.creates)
	}
}

// TestReprovisionRepairsAnIntegrationWithNoStoredHookURL: an integration
// provisioned before hook_url was persisted carries an empty one, so it reads
// as drifted on its first re-provision after this ships. The commit claimed
// that repairs itself into the address it already had; asserting it by
// construction is not asserting it.
func TestReprovisionRepairsAnIntegrationWithNoStoredHookURL(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
	res, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	urlBefore := hookURLFor(t, fa, "group/api")
	hookBefore := fa.hooks["group/api"].ID

	// Age the record back to before hook_url was stored.
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	integ.HookURL = ""
	if err := o.Integrations.Update(ctx, integ); err != nil {
		t.Fatal(err)
	}

	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatalf("the upgrade path must not error: %v", err)
	}

	if got := hookURLFor(t, fa, "group/api"); got != urlBefore {
		t.Fatalf("hook URL = %q, want the address it already had (%q) — the one-shot repair "+
			"must be a no-change, not a move", got, urlBefore)
	}
	if fa.hooks["group/api"].ID != hookBefore {
		t.Errorf("hook id changed (%s → %s) on the upgrade path", hookBefore, fa.hooks["group/api"].ID)
	}
	after, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HookURL != urlBefore {
		t.Fatalf("hook_url was not backfilled (%q); the record would read as drifted on every "+
			"provision forever", after.HookURL)
	}
}

// TestReprovisionFollowsAConnectionPin: the desired address is the connection's
// when it pins one, so pinning a base AFTER a repo was provisioned repairs that
// repo too. Without it the pin would only apply to repos provisioned later —
// the half-applied shape this whole mechanism exists to avoid.
func TestReprovisionFollowsAConnectionPin(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	conn := seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	}
	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}

	conn.WebhookBaseURL = "https://iterion.allowlisted.example"
	if err := o.Connections.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}

	got := hookURLFor(t, fa, "group/api")
	if !strings.HasPrefix(got, "https://iterion.allowlisted.example/api/webhooks/") {
		t.Fatalf("hook = %q, want the connection's newly pinned base — a pin set after "+
			"provisioning must reach the repos already provisioned", got)
	}
}
