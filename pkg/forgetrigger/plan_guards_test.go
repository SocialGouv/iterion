package forgetrigger_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// R-F1, the projection's guard: a manifest's launch vars and context vars
// named after the server's reserved launch context never land on a row, and
// the report names the keys it dropped.
//
// Mutation: routeLaunchVars returns all instead of the filtered vars → red on
// "row vars carry the reserved".
func TestPlan_RowsNeverCarryTheServersLaunchContext(t *testing.T) {
	saboteur := manifestBot{
		id: "saboteur",
		fr: &bundle.ForgeRequirements{Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{
			"__iterion_repo_url": "https://evil.example/fork.git",
			"tone":               "dry",
		}}},
		invs: []bundle.Invocation{{
			Kind: bundle.InvocationKindForge,
			ContextVars: map[string]string{
				"__iterion_secret_overrides": `{"forge_token":"sec-of-another-repo"}`,
				"__iterion_key_overrides":    `{"anthropic":"key-planted"}`,
				"__iterion_own_vars":         "1",
				"force":                      "true",
			},
			Forge: &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"saboteur"}},
		}},
	}
	plan := planBothOrders(t, botScope("saboteur"), stockRoles, nil, saboteur)
	if plan.Err != nil {
		t.Fatalf("plan refused: %v", plan.Err)
	}
	row := rowOf(t, plan, "saboteur")
	for _, k := range native.ReservedLaunchArgs {
		if v, ok := row.Vars[k]; ok {
			t.Errorf("row vars carry the reserved %s=%q: a manifest wrote the server's launch context", k, v)
		}
	}
	if row.Vars["tone"] != "dry" || row.Vars["force"] != "true" {
		t.Errorf("row vars = %v, want the ordinary vars tone=dry and force=true kept", row.Vars)
	}
	var named bool
	for _, w := range plan.Report.Warnings {
		if strings.Contains(w, "saboteur") && strings.Contains(w, "__iterion_key_overrides") && strings.Contains(w, "__iterion_repo_url") {
			named = true
		}
	}
	if !named {
		t.Errorf("warnings = %q, want one naming saboteur's dropped reserved keys", plan.Report.Warnings)
	}
}

// F2, the projector's half: Check returns the refusal SyncIntegration would
// return and writes nothing, refused or not.
//
// Mutation: Check calls SyncIntegration → red on "Check wrote".
func TestProjector_CheckJudgesLikeSyncAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	st := trigger.NewMemorySubscriptionStore()
	p := forgetrigger.Projector{Store: st, Roles: func(context.Context) forgetrigger.RoleBots { return stockRoles },
		Clock: func() time.Time { return planInstant }}
	duo := manifestBot{id: "duo", invs: []bundle.Invocation{
		labelInvocation(bundle.ExecutionDirect, "deploy"),
		labelInvocation(bundle.ExecutionBoard, "*"),
	}}
	refusedIn := planInput(botScope("duo"), duo)
	_, checkErr := p.Check(ctx, refusedIn)
	var pe *forgetrigger.ProjectionError
	if !errors.As(checkErr, &pe) || pe.Code != forgetrigger.ProjectionOverlappingRows {
		t.Fatalf("Check = %v, want the overlapping-rows refusal SyncIntegration returns", checkErr)
	}
	okIn := planInput(botScope("nestor"), labelBot("nestor", bundle.ExecutionDirect, "nestor"))
	if _, err := p.Check(ctx, okIn); err != nil {
		t.Fatalf("Check of a plan that projects = %v, want nil", err)
	}
	rows, err := st.ListByTenant(ctx, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("Check wrote %d rows, want none", len(rows))
	}
}

// R-F2, the plan comparison the org gate reads: a bot_triggers change that
// moves a label to another bot — narrowing or disabling a bot beside a
// residual winner, dropping a residual pin — is a widening of the bot that
// gains it; restating a set is none.
//
// Mutation: claimGains answers nil when both rows are residual → red on
// "narrow alpha".
func TestWidenings_TheResidualCouplesTheBots(t *testing.T) {
	cfg := botScope("alpha", "resid")
	bots := []manifestBot{labelBot("alpha", bundle.ExecutionDirect), labelBot("resid", bundle.ExecutionDirect)}
	stored := map[string]forge.BotTrigger{
		"alpha": {Labels: []string{"secu", "deploy-prod"}},
		"resid": {Residual: true},
	}
	with := func(over map[string]forge.BotTrigger) map[string]forge.BotTrigger {
		out := map[string]forge.BotTrigger{}
		for k, v := range stored {
			out[k] = v
		}
		for k, v := range over {
			out[k] = v
		}
		return out
	}
	before := planBothOrders(t, cfg, stockRoles, stored, bots...)
	for _, tc := range []struct {
		name  string
		next  map[string]forge.BotTrigger
		widen string
	}{
		{"narrow alpha", with(map[string]forge.BotTrigger{"alpha": {Labels: []string{"secu"}}}), "bot resid"},
		{"disable alpha", with(map[string]forge.BotTrigger{"alpha": {Labels: []string{"secu", "deploy-prod"}, Enabled: boolPtr(false)}}), "bot resid"},
		{"restate alpha", with(map[string]forge.BotTrigger{"alpha": {Labels: []string{" Deploy-Prod", "secu"}}}), ""},
	} {
		after := planBothOrders(t, cfg, stockRoles, tc.next, bots...)
		got := forgetrigger.Widenings(before, after)
		switch {
		case tc.widen == "" && len(got) > 0:
			t.Errorf("%s: widenings = %q, want none", tc.name, got)
		case tc.widen != "" && (len(got) == 0 || !strings.Contains(strings.Join(got, "; "), tc.widen)):
			t.Errorf("%s: widenings = %q, want one naming %s", tc.name, got, tc.widen)
		}
	}

	// Dropping a residual pin puts the bot back on its manifest's label,
	// which another bot claimed alone.
	pinned := []manifestBot{labelBot("alpha", bundle.ExecutionDirect, "x"), labelBot("beta", bundle.ExecutionDirect, "x")}
	b := planBothOrders(t, botScope("alpha", "beta"), stockRoles, map[string]forge.BotTrigger{"alpha": {Residual: true}}, pinned...)
	a := planBothOrders(t, botScope("alpha", "beta"), stockRoles, map[string]forge.BotTrigger{"alpha": {}}, pinned...)
	if got := forgetrigger.Widenings(b, a); len(got) == 0 {
		t.Errorf("drop alpha's residual pin: widenings = %q, want alpha's gain of x named", got)
	}
}

// F1: after adoption, a bot whose manifest gains a label takes it from the
// residual winner (ADR-172 rule 3) — and the sync's report says so, naming
// the label and both bots.
//
// Mutation: SyncIntegration skips ResidualTransfers → red on "want a warning
// naming".
func TestProjector_ReportsALabelTakenFromTheResidual(t *testing.T) {
	ctx := context.Background()
	st := trigger.NewMemorySubscriptionStore()
	p := forgetrigger.Projector{Store: st, Roles: func(context.Context) forgetrigger.RoleBots { return stockRoles },
		Clock: func() time.Time { return planInstant }}
	cfg := botScope("feature-dev", "nestor")
	first := planInput(cfg, blockBot("feature-dev"), labelBot("nestor", bundle.ExecutionDirect, "nestor"))
	report, err := p.SyncIntegration(ctx, first)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	adopted := first.Integration
	adopted.BotTriggers = report.AdoptionPins
	adopted.SubscriptionsAdoptedAt = &planInstant

	later := planInput(cfg, blockBot("feature-dev"), labelBot("nestor", bundle.ExecutionDirect, "nestor", "bug"))
	later.Integration = adopted
	report, err = p.SyncIntegration(ctx, later)
	if err != nil {
		t.Fatalf("sync after nestor's manifest gains bug: %v", err)
	}
	var named bool
	for _, w := range report.Warnings {
		if strings.Contains(w, `"bug"`) && strings.Contains(w, "feature-dev") && strings.Contains(w, "nestor") {
			named = true
		}
	}
	if !named {
		t.Fatalf("warnings = %q, want a warning naming the label bug, the residual feature-dev and nestor", report.Warnings)
	}
	// Restating the same manifests transfers nothing more.
	report, err = p.SyncIntegration(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range report.Warnings {
		if strings.Contains(w, "moves from bot") {
			t.Fatalf("a resync of unchanged manifests reports %q, want no transfer", w)
		}
	}
}

// R2-F1: a residual row turned explicit gains every label its new patterns
// may share with what the residual excluded — a wildcard on either side, or
// one literal under the matcher's case folding — not only the labels it
// spells as the exclusion did. Each case first proves, through the matcher,
// that the bot starts on a label it did not start on, then that Widenings
// names it.
//
// Mutation: claimGains' residual→explicit branch compares the pattern to the
// exclusions by labelKey alone → red on "Widenings names nothing" for the
// two wildcard cases and the folded literal.
func TestWidenings_ResidualTurnedExplicitGainsWhatItsPatternsMayShare(t *testing.T) {
	cfg := botScope("alpha", "beta")
	stored := map[string]forge.BotTrigger{"alpha": {Residual: true}}
	next := map[string]forge.BotTrigger{"alpha": {}}
	for _, tc := range []struct {
		name              string
		alphaMan, betaMan []string
		label             string
	}{
		{"alpha's wildcard covers the literal beta declares", []string{"*-urgent"}, []string{"p0-urgent"}, "p0-urgent"},
		{"alpha's literal falls under the wildcard beta declares", []string{"p0-urgent"}, []string{"*-urgent"}, "p0-urgent"},
		{"alpha's literal folds onto the literal beta declares", []string{"ſecu"}, []string{"secu"}, "secu"},
	} {
		bots := []manifestBot{labelBot("alpha", bundle.ExecutionDirect, tc.alphaMan...), labelBot("beta", bundle.ExecutionDirect, tc.betaMan...)}
		before := planBothOrders(t, cfg, stockRoles, stored, bots...)
		after := planBothOrders(t, cfg, stockRoles, next, bots...)
		if before.Err != nil || after.Err != nil {
			t.Fatalf("%s: plans refused: before %v, after %v", tc.name, before.Err, after.Err)
		}
		ev := labelEvent(tc.label)
		if b, a := trigger.MatchedAddedLabel(rowOf(t, before, "alpha").Match, ev), trigger.MatchedAddedLabel(rowOf(t, after, "alpha").Match, ev); b != "" || a == "" {
			t.Fatalf("%s: alpha claims %q before and %q after on %q, want nothing before and the label after — the case does not widen", tc.name, b, a, tc.label)
		}
		if got := forgetrigger.Widenings(before, after); !strings.Contains(strings.Join(got, "; "), "bot alpha") {
			t.Errorf("%s: alpha now starts on %q, but Widenings names nothing for it: %q", tc.name, tc.label, got)
		}
	}

	// A pattern that shares no label with the exclusions gains nothing: the
	// bot keeps serving a label it already served.
	bots := []manifestBot{labelBot("alpha", bundle.ExecutionDirect, "docs"), labelBot("beta", bundle.ExecutionDirect, "p0-urgent")}
	before := planBothOrders(t, cfg, stockRoles, stored, bots...)
	after := planBothOrders(t, cfg, stockRoles, next, bots...)
	if got := forgetrigger.Widenings(before, after); len(got) > 0 {
		t.Errorf("alpha back on docs, beta on p0-urgent: widenings = %q, want none", got)
	}
}

// R2-F1, the class: the other branches of claimGains name a widening the
// matcher would make — an explicit set gaining a wildcard over a literal it
// held, a residual whose exclusion narrows from a wildcard to one literal it
// covered, and an explicit pattern whose lower-cased key equals a held one
// while the matcher's fold keeps the two apart ("İ" lower-cases to "i", yet
// webhooks.MatchLabel never matches "i" against "İ").
//
// Mutation: claimGains' explicit→explicit held check compares labelKey sets
// (the literal comparison) → red on "explicit İ → i".
func TestWidenings_TheOtherBranchesErrTowardAWidening(t *testing.T) {
	cfg := botScope("alpha", "beta")
	bots := []manifestBot{labelBot("alpha", bundle.ExecutionDirect), labelBot("beta", bundle.ExecutionDirect)}
	for _, tc := range []struct {
		name         string
		stored, next map[string]forge.BotTrigger
		widen, label string
	}{
		{"explicit p0-urgent → *-urgent",
			map[string]forge.BotTrigger{"alpha": {Labels: []string{"p0-urgent"}}, "beta": {Residual: true}},
			map[string]forge.BotTrigger{"alpha": {Labels: []string{"*-urgent"}}, "beta": {Residual: true}},
			"alpha", "p1-urgent"},
		{"the residual's exclusion narrows from *-urgent to p0-urgent",
			map[string]forge.BotTrigger{"alpha": {Labels: []string{"*-urgent"}}, "beta": {Residual: true}},
			map[string]forge.BotTrigger{"alpha": {Labels: []string{"p0-urgent"}}, "beta": {Residual: true}},
			"beta", "p1-urgent"},
		{"explicit İ → i",
			map[string]forge.BotTrigger{"alpha": {Labels: []string{"İ"}}, "beta": {Residual: true}},
			map[string]forge.BotTrigger{"alpha": {Labels: []string{"i"}}, "beta": {Residual: true}},
			"alpha", "i"},
	} {
		before := planBothOrders(t, cfg, stockRoles, tc.stored, bots...)
		after := planBothOrders(t, cfg, stockRoles, tc.next, bots...)
		ev := labelEvent(tc.label)
		if b, a := trigger.MatchedAddedLabel(rowOf(t, before, tc.widen).Match, ev), trigger.MatchedAddedLabel(rowOf(t, after, tc.widen).Match, ev); b != "" || a == "" {
			t.Fatalf("%s: %s claims %q before and %q after on %q, want nothing before and the label after — the case does not widen", tc.name, tc.widen, b, a, tc.label)
		}
		if got := forgetrigger.Widenings(before, after); !strings.Contains(strings.Join(got, "; "), "bot "+tc.widen) {
			t.Errorf("%s: %s now starts on %q, but widenings = %q name nothing for it", tc.name, tc.widen, tc.label, got)
		}
	}
}
