package forge

import (
	"context"
	"slices"
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

// operatorGateDisabled must mirror the gating bots' own truthy test exactly:
// the release tracks "will the bot post a status?". Three truthiness tables
// exist along this var's path (this predicate, the runtime bool coercion,
// the bot's publish check) — any explicit pin the bot won't read as truthy
// leaves it silent, and a silent bot with forced re-review is the
// deadlock-at-full-cost shape. Absent key = derivation untouched.
func TestOperatorGateDisabled_MirrorsBotTruthiness(t *testing.T) {
	if operatorGateDisabled(nil) || operatorGateDisabled(map[string]string{}) {
		t.Error("absent key must not disable")
	}
	for _, v := range []string{"true", "1", "yes", "on", " TRUE ", "On"} {
		if operatorGateDisabled(map[string]string{"gate_enabled": v}) {
			t.Errorf("%q affirmatively enables the gate — must not disable", v)
		}
	}
	// Everything else leaves the bot silent (coerced false, or passed raw
	// and failing the bot's truthy test) — the sync must be released.
	for _, v := range []string{"false", "0", "no", "off", "", "banana"} {
		if !operatorGateDisabled(map[string]string{"gate_enabled": v}) {
			t.Errorf("%q leaves the bot silent — the forced sync must be released", v)
		}
	}
}

// An operator pin of gate_enabled=false turns the review bot advisory-only —
// no commit status is ever posted — so the statuses-scope derivation must not
// force re-review-on-sync: the forced sync exists solely to keep a REQUIRED
// check alive across pushes. First-review-only + on-demand re-review is the
// budget posture the pin buys.
func TestProvision_GateDisabledPinKeepsReviewOnSyncOff(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
		LaunchVars: map[string]string{"gate_enabled": "false"},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if cfg.ReviewOnSync {
		t.Error("gate_enabled=false pinned → no status to keep alive → re-review on sync must stay off")
	}

	// Removing the pin restores the derivation: the gate is back, so the
	// status must follow the head again.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
		LaunchVars: map[string]string{},
	}); err != nil {
		t.Fatalf("re-provision without pin: %v", err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config 2: %v", err)
	}
	if !after.ReviewOnSync {
		t.Error("pin removed → the gating derivation must force re-review on sync back on")
	}
}

// The production shape of the same decision: the repo is ALREADY provisioned
// with the forced sync, and the operator disables the gate afterwards. The
// update reaches the short-circuit path, so the backfill is what must release
// the sync — an explicit pin is a decision in both directions.
func TestProvision_GateDisabledPinReleasesExistingReviewOnSync(t *testing.T) {
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
	if cfg, err := o.Webhooks.Get(ctx, res.WebhookID); err != nil || !cfg.ReviewOnSync {
		t.Fatalf("precondition: gating bot forces sync on (err=%v)", err)
	}

	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
		LaunchVars: map[string]string{"gate_enabled": "false"},
	}); err != nil {
		t.Fatalf("re-provision with gate pin off: %v", err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if after.ReviewOnSync {
		t.Error("operator disabled the gate — the forced re-review on sync must be released with it")
	}
	if after.OperatorLaunchVars["gate_enabled"] != "false" {
		t.Errorf("the pin itself must land on the config, got %v", after.OperatorLaunchVars)
	}
}

// Rf2f99f: an operator's EXPLICIT review_on_sync choice (pinned by the
// webhook API) is never silently replaced by the gating derivation — in
// either direction. Advisory-reviews-on-every-push-without-a-gate
// (sync=true pinned + gate_enabled=false) survives a re-provision, and a
// pinned sync=false on a gating repo is not silently re-forced on.
func TestProvision_PinnedReviewOnSyncIsNeverRewritten(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
		LaunchVars: map[string]string{"gate_enabled": "false"},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The operator explicitly turns per-push advisory reviews ON, gate off —
	// the shape the webhook PATCH produces (value + pin).
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	cfg.ReviewOnSync = true
	cfg.ReviewOnSyncPinned = true
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Short-circuit re-provision (settings save) must not release it…
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot"}, ActorID: "u1",
		LaunchVars: map[string]string{"gate_enabled": "false"},
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ReviewOnSync || !after.ReviewOnSyncPinned {
		t.Fatalf("pinned sync=true silently released: %+v", after)
	}

	// …and a full-path re-provision (bot-set change) must carry it over too.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot", "dep-guard"}, ActorID: "u1",
		LaunchVars: map[string]string{"gate_enabled": "false"},
	}); err != nil {
		t.Fatalf("full re-provision: %v", err)
	}
	after2, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !after2.ReviewOnSync || !after2.ReviewOnSyncPinned {
		t.Fatalf("pinned sync=true lost on the full provision path: %+v", after2)
	}

	// The mirror direction: a pinned sync=false on a GATING repo (no gate
	// pin) must not be silently re-forced on by the derivation.
	cfg2, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	cfg2.ReviewOnSync = false
	cfg2.ReviewOnSyncPinned = true
	if err := o.Webhooks.Update(ctx, cfg2); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/gated",
		BotIDs: []string{"gate-bot", "dep-guard"}, ActorID: "u1",
		LaunchVars: map[string]string{},
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	after3, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if after3.ReviewOnSync {
		t.Fatal("pinned sync=false silently re-forced on — the historic 'must not have it re-enabled under them' hole")
	}
}

// Provision rebuilds the webhook config as a whole literal, so anything an
// operator set only on the config is wiped by the next enable. That drift
// already bit review_on_sync and launch_vars; overlap is the third field of
// the same shape, and it lands on exactly the webhooks worth setting it on.
func TestProvision_OperatorOverlapSurvivesReprovision(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard"}, ActorID: "u1",
		Overlap:    "supersede",
		LaunchVars: map[string]string{"gate_context": "iterion/review"},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	assertPinned := func(t *testing.T, when string) {
		t.Helper()
		cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
		if err != nil {
			t.Fatalf("%s: get config: %v", when, err)
		}
		if cfg.Overlap != "supersede" {
			t.Errorf("%s: overlap = %q, want the operator's pin", when, cfg.Overlap)
		}
		if cfg.OperatorLaunchVars["gate_context"] != "iterion/review" {
			t.Errorf("%s: gate_context lost: %v", when, cfg.OperatorLaunchVars)
		}
	}
	assertPinned(t, "after the first provision")

	// A MUTATING re-provision (one more bot) rebuilds the whole config.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "review-pr"}, ActorID: "u1",
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	assertPinned(t, "after enabling one more bot")

	// And a NO-OP re-provision must not drop them either.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "review-pr"}, ActorID: "u1",
	}); err != nil {
		t.Fatalf("no-op re-provision: %v", err)
	}
	assertPinned(t, "after a no-op re-provision")
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

// A re-provision rebuilds the whole webhook Config, so a field it neither
// stamps nor carries is reset the next time any bot is toggled on the repo —
// and the PATCH endpoint that sets these has no ProvisionedBy guard, so they
// are settable precisely on the managed configs it rebuilds. The armed review
// identity is one of them: losing it disarms the 🔁 button in silence.
func TestProvision_ReprovisionKeepsOperatorWebhookSettings(t *testing.T) {
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
		t.Fatal(err)
	}
	// What an operator sets the documented way (PATCH /webhooks/{id}).
	cfg.ReviewRequestLogins = []string{"iterion-bot"}
	cfg.AuthorizedRepliers = []string{"alice"}
	cfg.MonthlyCallLimit = 500
	cfg.AutoImplementOnOpen = true
	cfg.KeyOverrides = map[string]string{"anthropic": "key-1"}
	cfg.RetryMaxAttempts = 3
	cfg.RateLimit = webhooks.Rate{Rate: 10, Burst: 200}
	cfg.RateLimitPinned = true // the PATCH route stamps this on every rate_limit set
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Any later bot change re-provisions the repo.
	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "gate-bot"}, ActorID: "u1",
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.ReviewRequestLogins, []string{"iterion-bot"}) {
		t.Errorf("review_request_logins = %v, want [iterion-bot] — the 🔁 lane silently disarmed", after.ReviewRequestLogins)
	}
	if !slices.Equal(after.AuthorizedRepliers, []string{"alice"}) {
		t.Errorf("authorized_repliers = %v, want [alice]", after.AuthorizedRepliers)
	}
	if after.MonthlyCallLimit != 500 || after.RetryMaxAttempts != 3 || !after.AutoImplementOnOpen {
		t.Errorf("monthly=%d retries=%d auto_implement=%v", after.MonthlyCallLimit, after.RetryMaxAttempts, after.AutoImplementOnOpen)
	}
	if after.KeyOverrides["anthropic"] != "key-1" {
		t.Errorf("key_overrides = %v, want the operator's BYOK pin", after.KeyOverrides)
	}
	if after.RateLimit != (webhooks.Rate{Rate: 10, Burst: 200}) {
		t.Errorf("rate_limit = %+v, want the operator's raise — a reset silently 429s deliveries", after.RateLimit)
	}
	// The bot set is what a provision DOES recompute — it must not be frozen.
	if !after.AllowsBot("gate-bot") {
		t.Error("the newly enabled bot must be permitted — the carry must not freeze the bot scope")
	}
}

// R51dbee: MinReplierRole is merged stricter-of, never overwritten — the
// provision stamps a manifest-derived floor, but an operator's RAISE is a
// security control every replier gate reads, and a bot toggle must not
// silently lower it.
func TestCarryOperatorWebhookSettings_MinReplierRoleStricterOf(t *testing.T) {
	// Operator raised the floor above the derived value: the raise survives.
	cfg := webhooks.Config{MinReplierRole: "developer"}
	carryOperatorWebhookSettings(&cfg, webhooks.Config{MinReplierRole: "maintainer"})
	if cfg.MinReplierRole != "maintainer" {
		t.Fatalf("operator raise lost on re-provision: %q", cfg.MinReplierRole)
	}
	// Manifest floor higher than anything the operator set ("" reads as
	// developer): the floor stands.
	cfg = webhooks.Config{MinReplierRole: "maintainer"}
	carryOperatorWebhookSettings(&cfg, webhooks.Config{})
	if cfg.MinReplierRole != "maintainer" {
		t.Fatalf("manifest floor must stand over an unset previous: %q", cfg.MinReplierRole)
	}
	// Equal ranks ("" == developer): the derivation's value stands unchanged.
	cfg = webhooks.Config{MinReplierRole: "developer"}
	carryOperatorWebhookSettings(&cfg, webhooks.Config{})
	if cfg.MinReplierRole != "developer" {
		t.Fatalf("equal ranks must keep the derived value: %q", cfg.MinReplierRole)
	}
	// R948c68: a never-set prev ("") must NOT discard a manifest's deliberate
	// sub-developer floor — the gates read "" as developer (rank 3), but the
	// derivation ranks "" as zero, so "reporter" here was legitimately won.
	cfg = webhooks.Config{MinReplierRole: "reporter"}
	carryOperatorWebhookSettings(&cfg, webhooks.Config{})
	if cfg.MinReplierRole != "reporter" {
		t.Fatalf("unset prev must defer to a sub-developer derived floor: %q", cfg.MinReplierRole)
	}
}

// R8a3f4e: Enabled is the operator's per-repo kill switch — a re-provision
// (any bot toggle) must not silently re-arm the inbound lanes it paused.
func TestCarryOperatorWebhookSettings_EnabledKillSwitchSurvives(t *testing.T) {
	cfg := webhooks.Config{Enabled: true} // what the provision literal stamps
	carryOperatorWebhookSettings(&cfg, webhooks.Config{Enabled: false})
	if cfg.Enabled {
		t.Fatal("a re-provision must not re-arm a webhook the operator disabled")
	}
	cfg = webhooks.Config{Enabled: true}
	carryOperatorWebhookSettings(&cfg, webhooks.Config{Enabled: true})
	if !cfg.Enabled {
		t.Fatal("an enabled webhook must stay enabled through re-provision")
	}
}

// The other half of the RateLimit carry: an UNPINNED value is the
// provisioner's own former default, and a re-provision must move it to the
// CURRENT default — otherwise a default bump (1/10 → 2/60, sized for the
// review-comment fan-out) never reaches an already-provisioned repo and the
// rollout gesture that subscribes the new event leaves the old burst in
// place.
func TestProvision_ReprovisionMigratesUnpinnedRateLimitDefault(t *testing.T) {
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
		t.Fatal(err)
	}
	// What every pre-bump provision left behind: the historical default,
	// never touched by an operator.
	cfg.RateLimit = webhooks.Rate{Rate: 1, Burst: 10}
	cfg.RateLimitPinned = false
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "gate-bot"}, ActorID: "u1",
	}); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	after, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RateLimit != (webhooks.Rate{Rate: 2, Burst: 60}) {
		t.Errorf("rate_limit = %+v, want the current provision default — an unpinned historical default must not survive a re-provision", after.RateLimit)
	}
	if after.RateLimitPinned {
		t.Error("a migrated default must stay unpinned (still provisioner-owned)")
	}
}
