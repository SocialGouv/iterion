package cloudpublisher

import (
	"context"
	"maps"
	"testing"

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
// Mutant: drop CredPlan from either publish site, or key the stamp on the
// whole-bundle grant alone → this reds.
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
