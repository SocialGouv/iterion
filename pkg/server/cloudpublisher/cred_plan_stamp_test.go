package cloudpublisher

import (
	"context"
	"maps"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fine plan's PUBLICATION witnesses (#2288): a pool-served run whose
// routing walk the publisher could read stamps the node-and-provider →
// credential-fingerprint plan on its RunMessage — the producer the
// runner's fine spend table keys on; a run no lease serves publishes none.

// planDoorWorkflow is the door shape with a READABLE walk: the node pins
// its own provider (anthropic — the tenant's key) and the policy ladder
// lands the donor's zai rung (an authored `fallbacks:` chain would make
// the node ineligible — the ladder takes nothing of the author's). Every
// route names its provider, so the plan is total — the premise every
// unwritable shape (a bare model anywhere) fails and honestly publishes
// nothing.
func planDoorWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{Backend: "claude_code", Provider: "anthropic", Model: "anthropic/claude-opus-5-5"},
		Tools:     []string{},
	}}}
}

// TestCredPlanStamp_aDoorServedRunPublishesThePlan: the door granted, the
// walk is narrow-safe — the message carries the plan, and its donor row is
// the fingerprint the DOOR's key record stamps (the lease's own string).
// Mutant: drop CredPlan from the LAUNCH site, or key the stamp on the
// whole-bundle grant alone → this reds. The resume site has its own
// witness (TestCredPlanStamp_aResumeStampsThePlanOfItsOwnDonor): this test
// never resumes.
func TestCredPlanStamp_aDoorServedRunPublishesThePlan(t *testing.T) {
	f := newDoorPublishFixture(t, true)
	if _, err := f.pub.SubmitLaunch(store.WithIdentity(context.Background(), poolTeam, "requester"), "run-plan", runview.LaunchSpec{
		FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
		LLMRoutePolicy: f.policy(),
		BotID:          "docs-refresh",
	}, planDoorWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}

	msg := lastMessage(t, f)
	if msg.PoolGrantless {
		t.Fatal("the door-served run published grantless — the plan's premise broke")
	}
	if msg.CredPlan == nil {
		t.Fatal("no plan on a pool-served run — the runner's fine table would never fill and the door's booking stays coarse")
	}
	// The donor row must be the LEASE's own stamp, and the run's own row
	// the identity the runner recomputes at claim — the join in one space,
	// at the seam the broker books on.
	open, err := f.leases.ListOpenByRun(context.Background(), "run-plan")
	if err != nil || len(open) != 1 {
		t.Fatalf("open leases = (%+v, %v), want the door's alone", open, err)
	}
	want := map[string]string{
		"implement|anthropic": secrets.APIKeyFingerprint(secrets.ProviderAnthropic, "sk-ant-tenant", ""),
		"implement|zai":       open[0].Fingerprint,
	}
	if want["implement|zai"] == "" {
		t.Fatal("the door lease carries no fingerprint — the plan has nothing to meet")
	}
	if !maps.Equal(msg.CredPlan, want) {
		t.Fatalf("CredPlan = %v, want %v — the plan must name the identities the runner holds and the lease stamps", msg.CredPlan, want)
	}
}

// TestCredPlanStamp_aResumeStampsThePlanOfItsOwnDonor: a resume re-seals
// the bundle, and a resumed attempt may run on a DIFFERENT donor — so its
// plan is re-derived, never carried. The launch is served by donor B; the
// landscape flips (B withdraws, A stays); the resume grants A, and its
// message must carry a plan whose door row is A's lease fingerprint: a
// resume publishing no plan would book A the whole attempt, and one
// carrying the launch's plan would name B's credential, which no
// observation of this attempt spends — A's fine slice would book $0.
// Mutant: drop CredPlan from SubmitResume → the nil check reds. The door
// row is compared to A's lease, not merely checked present: a plan
// replayed from the launch (the doc-replay pattern the resume follows for
// its pins and its fallback chain) would still name B.
func TestCredPlanStamp_aResumeStampsThePlanOfItsOwnDonor(t *testing.T) {
	f := newDoorPublishFixture(t, true)
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	if _, err := f.pub.SubmitLaunch(ctx, "run-plan-resume", runview.LaunchSpec{
		FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
		LLMRoutePolicy: f.policy(),
		BotID:          "docs-refresh",
	}, planDoorWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	launched := lastMessage(t, f)
	launchOpen, err := f.leases.ListOpenByRun(context.Background(), "run-plan-resume")
	if err != nil || len(launchOpen) != 1 || launchOpen[0].DonorID != "donor-b" {
		t.Fatalf("open leases after the launch = (%+v, %v), want donor B's alone (the id sort picks it on a zero-usage tie)", launchOpen, err)
	}
	if got := launched.CredPlan["implement|zai"]; got == "" || got != launchOpen[0].Fingerprint {
		t.Fatalf("the launch's door row = %q, want donor B's lease fingerprint %q — the premise (a planned launch) broke", got, launchOpen[0].Fingerprint)
	}

	run, err := f.rs.LoadRun(ctx, "run-plan-resume")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.Status = store.RunStatusPausedOperator
	if err := f.rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	// The landscape flips: B withdraws — the resume must switch to A.
	if err := f.pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor-b", credpool.SourceAPIKey, "zai"), PoolID: "pool-1", UserID: "donor-b",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "zai"},
		Enabled:    false, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5}, FallbackUse: true,
	}); err != nil {
		t.Fatalf("disable B: %v", err)
	}
	if err := f.pub.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-plan-resume", FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
	}, planDoorWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if len(f.published) != 2 {
		t.Fatalf("published %d messages, want the launch's and the resume's", len(f.published))
	}

	resumed := lastMessage(t, f)
	if resumed.CredPlan == nil {
		t.Fatal("no plan on the resumed attempt — its report sends no fine table and donor A books the whole attempt")
	}
	open, err := f.leases.ListOpenByRun(context.Background(), "run-plan-resume")
	if err != nil || len(open) != 1 || open[0].DonorID != "donor" {
		t.Fatalf("open leases after the resume = (%+v, %v), want donor A's alone", open, err)
	}
	if open[0].Fingerprint == "" || open[0].Fingerprint == launchOpen[0].Fingerprint {
		t.Fatalf("donor A's lease fingerprint = %q (B's = %q) — the fixture must give the two donors distinct identities", open[0].Fingerprint, launchOpen[0].Fingerprint)
	}
	want := map[string]string{
		"implement|anthropic": secrets.APIKeyFingerprint(secrets.ProviderAnthropic, "sk-ant-tenant", ""),
		"implement|zai":       open[0].Fingerprint,
	}
	if !maps.Equal(resumed.CredPlan, want) {
		t.Fatalf("the resume's CredPlan = %v, want %v — the resumed attempt's plan names ITS donor's credential, the one its lease stamps", resumed.CredPlan, want)
	}
}

// TestCredPlanStamp_aLeaselessRunPublishesNone: the plan is the fine
// booking's producer, and a run no lease serves has no consumer for it —
// publishing one would widen the wire for the vast majority of runs and
// invite a reader that books nobody by it.
func TestCredPlanStamp_aLeaselessRunPublishesNone(t *testing.T) {
	f := newDoorPublishFixture(t, true)
	// No routing policy: no ladder, no door, no pool lease — the tenant's
	// own key funds the run.
	if _, err := f.pub.SubmitLaunch(store.WithIdentity(context.Background(), poolTeam, "requester"), "run-nolease", runview.LaunchSpec{
		FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
		BotID: "docs-refresh",
	}, planDoorWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	msg := lastMessage(t, f)
	if !msg.PoolGrantless {
		t.Fatal("a lease-less run published PoolGrantless=false — the fixture broke")
	}
	if msg.CredPlan != nil {
		t.Fatalf("CredPlan = %v, want none — a run no lease serves publishes no plan", msg.CredPlan)
	}
}

// envDialReviewWorkflow is review-pr's routing shape on the default runner:
// every routing field an env dial with a vendor default (narrow-safe, env
// deferred), two reviewers on two vendors. The dials are unset in the test
// process, so the publisher reads their defaults.
func envDialReviewWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"reviewer_claude": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "reviewer_claude"}, LLMFields: ir.LLMFields{Backend: "${PLAN_ENV_UNSET_BACKEND_CLAUDE:-claude_code}", Model: "${PLAN_ENV_UNSET_MODEL_CLAUDE:-claude-opus-5}", Provider: "${PLAN_ENV_UNSET_PROVIDER_CLAUDE:-anthropic}"}, Tools: []string{}},
		"reviewer_gpt":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "reviewer_gpt"}, LLMFields: ir.LLMFields{Backend: "${PLAN_ENV_UNSET_BACKEND_GPT:-claw}", Model: "${PLAN_ENV_UNSET_MODEL_GPT:-openai/gpt-6-sol}"}, Tools: []string{}},
	}}
}

// An env-dial run an attached pool serves side by side — the claude forfait
// and an openai key, two leases of one attempt — is planned, so the runner's
// fine table splits the attempt by credential and the donors are booked the
// attempt's cost once between them. Without a plan the broker books the whole
// attempt on EVERY lease (the coarse doctrine): two donors charged 1.00 each
// for an attempt that cost 1.00. The fine table is built here as the runner
// builds it from the plan (fineFingerprintSpend: each node's spend on its
// plan row's fingerprint). Mutant: CredentialPlan nil on an EnvDeferred walk
// → the plan assert, and the total books 2.00.
func TestCredPlanStamp_anEnvDialRunServedSideBySideBooksTheAttemptOnce(t *testing.T) {
	f := newAttachedFixture(t)
	ctx := context.Background()
	k := secrets.ApiKey{ID: "key-oa", TenantID: poolTeam, ScopeTeamID: poolTeam, ScopeUserID: "lender-oa", Provider: secrets.ProviderOpenAI, Name: "lent openai",
		Fingerprint: secrets.APIKeyFingerprint(secrets.ProviderOpenAI, "sk-openai-lent", "")}
	sealed, err := secrets.SealAPIKeyFor(f.sealer, k, []byte("sk-openai-lent"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	k.SealedSecret = sealed
	if err := f.apiKeys.Create(ctx, k); err != nil {
		t.Fatalf("seed lent openai key: %v", err)
	}
	if err := f.pledges.Upsert(ctx, credpool.Pledge{ID: credpool.PledgeID("lender-oa", credpool.SourceAPIKey, string(secrets.ProviderOpenAI)), PoolID: "pool-att", UserID: "lender-oa",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAI), KeyID: "key-oa"},
		Enabled:    true, Health: credpool.HealthOK, Priority: 3}); err != nil {
		t.Fatalf("pledge: %v", err)
	}
	wf := envDialReviewWorkflow()
	if walk := model.EffectiveProviders(model.RouteView{Workflow: envDialReviewWorkflow()}, walkProviderSet); !walk.NarrowSafe || !walk.EnvDeferred {
		t.Fatalf("test premise: want a narrow-safe env-deferred walk, got %+v", walk)
	}
	_, creds := f.resolveAttached(t, "run-env-plan", wf, frozenAttachment("pool-att"))
	if len(creds.grants) != 2 {
		t.Fatalf("fixture: grants = %d, want the forfait and the openai key side by side", len(creds.grants))
	}
	byNode := map[string]float64{"reviewer_claude": 0.40, "reviewer_gpt": 0.60}
	spend := map[string]float64{}
	for key, fp := range creds.plan {
		node, _, _ := strings.Cut(key, model.PlanKeySep)
		spend[fp] += byNode[node]
	}
	for _, g := range creds.grants {
		if _, planned := spend[g.Fingerprint]; !planned {
			t.Fatalf("plan = %v names no row for the lent credential %s (%s): the runner builds no fine table without one", creds.plan, g.Fingerprint, g.Credential)
		}
	}
	var fine []credpool.FingerprintSpend
	for fp, cost := range spend {
		fine = append(fine, credpool.FingerprintSpend{Fingerprint: fp, CostUSD: cost})
	}
	var publishedAt time.Time
	for _, g := range creds.grants {
		if g.AcquiredAt.After(publishedAt) {
			publishedAt = g.AcquiredAt
		}
	}
	if err := f.pub.credPool.ReportAttempt(ctx, "run-env-plan", publishedAt.Add(5*time.Millisecond), credpool.Outcome{CostUSD: 1.0, ByFingerprint: fine}); err != nil {
		t.Fatalf("report: %v", err)
	}
	leases, err := f.leases.ListByRun(ctx, "run-env-plan")
	if err != nil {
		t.Fatalf("leases: %v", err)
	}
	total := 0.0
	for _, l := range leases {
		total += l.CostUSD
	}
	if math.Abs(total-1.0) > 1e-9 {
		t.Fatalf("the donors of a 1.00 attempt were booked %.2f between them — every lease charged the whole attempt", total)
	}
}
