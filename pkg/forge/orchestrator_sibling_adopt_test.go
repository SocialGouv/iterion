package forge

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// R4-1/R4-2: the sibling operator fields (overlap, hold labels, label
// allowlist) have the same two-store divergence hazard launch vars had —
// sharper, because the documented webhook PATCH writes them to the CONFIG
// ONLY, so preferring the integration on a silent re-provision silently
// reverts the documented gesture at 200 OK. Doctrine: the config is the only
// out-of-band write surface and, with enforcement-first ordering, never the
// stale half — so on disagreement it wins, and the provision converges the
// integration to it.

func provisionWithSettings(t *testing.T, o *Orchestrator, connID string, overlap string, hold, allowlist []string) ProvisionResult {
	t.Helper()
	res, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID:     "t1",
		ConnectionID: connID,
		RepoFullName: "group/api",
		BotIDs:       []string{"review-pr"},
		Overlap:      overlap,
		HoldLabels:   hold,
		// An allowlist intersects the review-pr manifest's own, which these
		// tests do not declare; exercise it through the store directly.
		ActorID: "tester",
	})
	if err != nil {
		t.Fatalf("provision with settings: %v", err)
	}
	return res
}

func divergeConfig(t *testing.T, o *Orchestrator, res ProvisionResult, edit func(*webhooks.Config)) {
	t.Helper()
	cfg, err := o.Webhooks.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	edit(&cfg)
	if err := o.Webhooks.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func silentReprovision(t *testing.T, o *Orchestrator, connID string) {
	t.Helper()
	if _, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: connID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
	}); err != nil {
		t.Fatalf("silent re-provision: %v", err)
	}
}

// (a) The documented-PATCH shape: provisioned "skip" on both stores, the
// operator PATCHes "supersede" (config only), a silent re-provision must
// enforce "supersede" and converge the integration — never revert the
// gesture.
//
// Mutation that reddens this test: preferring the integration's non-empty
// overlap when the config's differs.
func TestProvisionConfigWinsAnOverlapDivergence(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithSettings(t, o, conn.ID, "skip", nil, nil)
	divergeConfig(t, o, res, func(c *webhooks.Config) { c.Overlap = "supersede" })

	silentReprovision(t, o, conn.ID)
	integ, cfg := readBothStores(t, o, res)
	if cfg.Overlap != "supersede" {
		t.Errorf("the documented PATCH was reverted: config overlap = %q", cfg.Overlap)
	}
	if integ.Overlap != "supersede" {
		t.Errorf("the integration did not converge to the enforced value: %q", integ.Overlap)
	}

	// An integration-only value with an EMPTY config keeps standing — the
	// crash/one-sided shape: a deliberate clear via the PATCH is normalized
	// to "allow" (webhooks_routes.go), so direct store manipulation is the
	// only remaining producer of this divergence.
	divergeConfig(t, o, res, func(c *webhooks.Config) { c.Overlap = "" })
	integ.Overlap = "skip"
	if err := o.Integrations.Update(context.Background(), integ); err != nil {
		t.Fatal(err)
	}
	silentReprovision(t, o, conn.ID)
	_, cfg = readBothStores(t, o, res)
	if cfg.Overlap != "skip" {
		t.Errorf("an integration-only overlap must keep standing when the config has none: got %q", cfg.Overlap)
	}
}

// (b) The lifted hold: the operator clears the pause with [] (config only —
// an explicit EMPTY the store now keeps); a silent re-provision must keep
// the hold lifted and converge the integration, never resurrect ["freeze"].
//
// Mutation that reddens this test: adopting the integration's non-empty hold
// over the config's explicit empty.
func TestProvisionConfigWinsALiftedHold(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithSettings(t, o, conn.ID, "", []string{"freeze"}, nil)
	divergeConfig(t, o, res, func(c *webhooks.Config) { c.HoldLabels = []string{} })

	silentReprovision(t, o, conn.ID)
	integ, cfg := readBothStores(t, o, res)
	if len(cfg.HoldLabels) != 0 {
		t.Errorf("the lifted hold was resurrected on the enforcement half: %v", cfg.HoldLabels)
	}
	if len(integ.HoldLabels) != 0 {
		t.Errorf("the integration did not converge to the lifted hold: %v", integ.HoldLabels)
	}
}

// (d) Both-non-empty disagreement on every sibling field: the config wins
// each, and the integration converges.
func TestProvisionConfigWinsEverySiblingDisagreement(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithSettings(t, o, conn.ID, "skip", []string{"freeze"}, nil)
	integ, cfg := readBothStores(t, o, res)
	integ.LabelAllowlist = []string{"implement"}
	if err := o.Integrations.Update(context.Background(), integ); err != nil {
		t.Fatal(err)
	}
	divergeConfig(t, o, res, func(c *webhooks.Config) {
		c.Overlap = "supersede"
		c.HoldLabels = []string{"maintenance"}
		c.LabelAllowlist = []string{"ship-it"}
	})

	silentReprovision(t, o, conn.ID)
	integ, cfg = readBothStores(t, o, res)
	if cfg.Overlap != "supersede" || integ.Overlap != "supersede" {
		t.Errorf("overlap: cfg=%q integ=%q, want supersede on both", cfg.Overlap, integ.Overlap)
	}
	if len(cfg.HoldLabels) != 1 || cfg.HoldLabels[0] != "maintenance" || len(integ.HoldLabels) != 1 || integ.HoldLabels[0] != "maintenance" {
		t.Errorf("hold: cfg=%v integ=%v, want [maintenance] on both", cfg.HoldLabels, integ.HoldLabels)
	}
	if len(cfg.LabelAllowlist) != 1 || cfg.LabelAllowlist[0] != "ship-it" || len(integ.LabelAllowlist) != 1 || integ.LabelAllowlist[0] != "ship-it" {
		t.Errorf("allowlist: cfg=%v integ=%v, want [ship-it] on both", cfg.LabelAllowlist, integ.LabelAllowlist)
	}
}

// (c) One-sided shapes keep today's fallbacks: a config-only narrowing is
// adopted (it is the documented gesture the integration has not caught up
// with), and a nil config leaves the integration's alone.
func TestProvisionSiblingOneSidedShapesKeepTheirFallbacks(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	res := provisionWithSettings(t, o, conn.ID, "", nil, nil)
	divergeConfig(t, o, res, func(c *webhooks.Config) { c.LabelAllowlist = []string{"implement"} })

	silentReprovision(t, o, conn.ID)
	integ, cfg := readBothStores(t, o, res)
	if len(cfg.LabelAllowlist) != 1 || cfg.LabelAllowlist[0] != "implement" {
		t.Errorf("the config-only narrowing was lost: %v", cfg.LabelAllowlist)
	}
	if len(integ.LabelAllowlist) != 1 || integ.LabelAllowlist[0] != "implement" {
		t.Errorf("the integration did not converge to the narrowing: %v", integ.LabelAllowlist)
	}
}
