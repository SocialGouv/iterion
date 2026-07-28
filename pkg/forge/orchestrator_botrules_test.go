package forge

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

func ruleFor(t *testing.T, rules []webhooks.BotRule, botID string) webhooks.BotRule {
	t.Helper()
	for _, r := range rules {
		if r.BotID == botID {
			return r
		}
	}
	t.Fatalf("no rule for %q in %+v", botID, rules)
	return webhooks.BotRule{}
}

// Two bots on one repo webhook each keep their OWN author filter and their own
// launch vars — the property the flattened Config fields cannot express.
func TestProvision_BotRulesPerBotRouting(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if len(cfg.BotRules) != 2 {
		t.Fatalf("want one rule per bot, got %+v", cfg.BotRules)
	}

	guard := ruleFor(t, cfg.BotRules, "dep-guard")
	if !sameSet(guard.AuthorAllowlist, []string{"dependabot[bot]", "*renovate[bot]"}) {
		t.Errorf("guard keeps its own allowlist even though the shared one re-opened: %v", guard.AuthorAllowlist)
	}
	// The comment event must NEVER reach a rule: it routes through CommandMap,
	// so leaking it here would auto-launch the bot on every PR comment.
	if !sameSet(guard.Events, []string{bundle.ForgeEventPullRequest}) {
		t.Errorf("guard events = %v, want pull_request only", guard.Events)
	}

	reviewer := ruleFor(t, cfg.BotRules, "review-pr")
	if len(reviewer.AuthorAllowlist) != 0 {
		t.Errorf("reviewer declares no allowlist → stays open, got %v", reviewer.AuthorAllowlist)
	}
	if !sameSet(reviewer.AuthorDenylist, []string{"dependabot[bot]", "*renovate[bot]"}) {
		t.Errorf("the guard's exclusive claim must land on the reviewer's denylist, got %v", reviewer.AuthorDenylist)
	}
	if guard.LaunchVars["pr_review_mode"] != "" {
		t.Errorf("the reviewer's vars leaked into the guard: %v", guard.LaunchVars)
	}
	if reviewer.LaunchVars["pr_review_mode"] != "summary" {
		t.Errorf("the reviewer lost its own vars: %v", reviewer.LaunchVars)
	}

	// And the routing this produces is the whole point.
	if got := cfg.RulesForEvent(bundle.ForgeEventPullRequest, "socialgouv-renovate[bot]"); len(got) != 1 || got[0].BotID != "dep-guard" {
		t.Errorf("a self-hosted renovate App PR must route to the guard alone, got %+v", got)
	}
	if got := cfg.RulesForEvent(bundle.ForgeEventPullRequest, "alice"); len(got) != 1 || got[0].BotID != "review-pr" {
		t.Errorf("a human PR must route to the reviewer alone, got %+v", got)
	}
}

// A bot with a forge: block and no invocations must keep launching, and a
// bot claiming its authors alone must not deny itself.
func TestProvision_BotRulesSingleBotNoSelfDeny(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	guard := ruleFor(t, cfg.BotRules, "dep-guard")
	if len(guard.AuthorDenylist) != 0 {
		t.Errorf("a bot must never deny itself, got %v", guard.AuthorDenylist)
	}
	if got := cfg.RulesForEvent(bundle.ForgeEventPullRequest, "renovate[bot]"); len(got) != 1 {
		t.Errorf("the sole bot must still claim its own PRs, got %+v", got)
	}
}

// A bot that declares `statuses` can be a REQUIRED check, and a required
// check lives on ONE head SHA: if the bot does not re-run when the author
// pushes a fix, the status is absent from the new head — indistinguishable
// from "never reviewed" — and the PR is blocked with no way out but an admin
// bypass. That is SocialGouv/iterion#300. Provisioning must therefore turn
// re-review-on-sync on from the declared capability.
func TestProvision_StatusesScopeEnablesReviewOnSync(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	// dep-guard declares no `statuses` scope → no gate → no forced re-review.
	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if cfg.ReviewOnSync {
		t.Error("no bot posts a commit status → re-review on sync must stay off (it costs a run per push)")
	}

	// gate-bot does → every push must re-evaluate the gate on the new head.
	res2, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision gated: %v", err)
	}
	cfg2, err := o.Webhooks.Get(ctx, res2.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config 2: %v", err)
	}
	if !cfg2.ReviewOnSync {
		t.Error("a bot declaring statuses:write gates merges — its status must follow the head SHA")
	}
}

// The same migration guard for re-review: a repo already provisioned with a
// gating bot reaches the SHORT-CIRCUIT, never the full path. A derivation that
// only runs on a fresh provision leaves every deployed repo with the bug it
// was written to close — the PR blocked on a status stuck to an old head.
func TestProvision_NoOpReprovisionBackfillsReviewOnSync(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Simulate the deployed state: provisioned before the derivation existed.
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	cfg.ReviewOnSync = false
	cfg.BotRules = nil
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}

	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config 2: %v", err)
	}
	if !after.ReviewOnSync {
		t.Fatal("a gating bot on an already-provisioned repo must gain re-review on push")
	}
}

// A repo whose bots post no status keeps re-review off: it costs a run per
// push, and nothing there needs a status to follow the head.
func TestProvision_NoGatingBotLeavesReviewOnSyncAlone(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard"}, ActorID: "u1",
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if cfg.ReviewOnSync {
		t.Error("no bot posts a status here — re-review on push must stay off")
	}
}

// The migration guard: re-enabling an unchanged bot set short-circuits, so
// without an explicit backfill an already-provisioned repo would stay on the
// legacy single-bot path forever — tests green, production unchanged.
func TestProvision_NoOpReprovisionBackfillsBotRules(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Simulate a config written before BotRules existed.
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	priorHash := cfg.TokenHash
	cfg.BotRules = nil
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}
	hooksBefore := len(fa.hooks)

	res2, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if res2.Created {
		t.Errorf("an unchanged bot set must stay a no-op provision")
	}
	after, err := o.Webhooks.Get(ctx, res2.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config 2: %v", err)
	}
	if len(after.BotRules) != 2 {
		t.Fatalf("the short-circuit must still backfill the routing table, got %+v", after.BotRules)
	}
	// A backfill is a config write only: no fresh token, no forge hook call.
	if after.TokenHash != priorHash {
		t.Errorf("backfill must not mint a new token")
	}
	if len(fa.hooks) != hooksBefore {
		t.Errorf("backfill must not touch the forge hook")
	}
}
