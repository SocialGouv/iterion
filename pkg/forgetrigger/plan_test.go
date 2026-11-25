package forgetrigger_test

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

const (
	testTenant      = "team-1"
	testIntegration = "int-1"
	testWebhook     = "wh-1"
	testConnection  = "conn-1"
	testRepo        = "acme/shop"

	// The binding and the origin ADR-172 §2 gives the rows of testIntegration.
	testBinding = "integration:int-1"
	testOrigin  = "forge:int-1"

	noLabelReasonTwoBots = "declares issue_labeled with no label while 2 bots share acme/shop: set its labels (bot_triggers) or enable it"
)

var planInstant = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

// stockRoles are the role bots of the deployment the plans are derived for.
var stockRoles = forgetrigger.RoleBots{Implementer: "feature-dev", Reviewer: "review-pr"}

// manifestBot is what a plan reads of one bot's manifest.
type manifestBot struct {
	id   string
	fr   *bundle.ForgeRequirements
	invs []bundle.Invocation
}

// labelInvocation is a kind=forge invocation started by a labelled issue.
func labelInvocation(mode bundle.ExecutionMode, labels ...string) bundle.Invocation {
	return bundle.Invocation{
		Kind:  bundle.InvocationKindForge,
		Mode:  mode,
		Forge: &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: labels},
	}
}

// labelBot declares one issue_labeled invocation of its own.
func labelBot(id string, mode bundle.ExecutionMode, labels ...string) manifestBot {
	return manifestBot{id: id, invs: []bundle.Invocation{labelInvocation(mode, labels...)}}
}

// blockBot claims issue_labeled through its forge: block alone, the way the
// catalogue's label bots do.
func blockBot(id string, labels ...string) manifestBot {
	return manifestBot{id: id, fr: &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventIssueLabeled}, Labels: labels}}
}

// pullRequestBot claims no issue event.
func pullRequestBot(id string) manifestBot {
	return manifestBot{id: id, invs: []bundle.Invocation{{
		Kind:  bundle.InvocationKindForge,
		Forge: &bundle.InvocationForge{Event: bundle.ForgeEventPullRequest},
	}}}
}

// botScope is a webhook config that permits exactly these bots.
func botScope(bots ...string) webhooks.Config {
	return webhooks.Config{ID: testWebhook, BotIDs: bots}
}

// planInput lists the bots in the order they are given.
func planInput(cfg webhooks.Config, bots ...manifestBot) forge.SubscriptionInput {
	in := forge.SubscriptionInput{
		Integration: forge.RepoIntegration{
			ID:           testIntegration,
			TenantID:     testTenant,
			ConnectionID: testConnection,
			RepoFullName: testRepo,
			WebhookID:    testWebhook,
		},
		Config:   cfg,
		InvByBot: map[string][]bundle.Invocation{},
		FrByBot:  map[string]*bundle.ForgeRequirements{},
	}
	for _, b := range bots {
		in.Bots = append(in.Bots, b.id)
		in.Integration.BotIDs = append(in.Integration.BotIDs, b.id)
		in.InvByBot[b.id] = b.invs
		in.FrByBot[b.id] = b.fr
	}
	return in
}

// planBothOrders plans the bots as listed and in the reverse order, requires
// the two plans to be equal, and returns that plan.
func planBothOrders(t *testing.T, cfg webhooks.Config, roles forgetrigger.RoleBots, triggers map[string]forge.BotTrigger, bots ...manifestBot) forgetrigger.Plan {
	t.Helper()
	plan := func(order []manifestBot) forgetrigger.Plan {
		in := planInput(cfg, order...)
		in.Integration.BotTriggers = triggers
		return forgetrigger.PlanIntegration(in, roles, planInstant)
	}
	reversed := slices.Clone(bots)
	slices.Reverse(reversed)
	forward, backward := plan(bots), plan(reversed)
	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("the plan depends on the order of the bots:\n listed   %+v\n reversed %+v", forward, backward)
	}
	if !slices.IsSortedFunc(forward.Rows, func(a, b trigger.Subscription) int {
		return cmp.Or(cmp.Compare(a.BotID, b.BotID), cmp.Compare(a.ID, b.ID))
	}) {
		t.Fatalf("the rows are not sorted by bot, then id: %+v", forward.Rows)
	}
	return forward
}

func rowsOf(plan forgetrigger.Plan, bot string) []trigger.Subscription {
	var out []trigger.Subscription
	for _, row := range plan.Rows {
		if row.BotID == bot {
			out = append(out, row)
		}
	}
	return out
}

func rowOf(t *testing.T, plan forgetrigger.Plan, bot string) trigger.Subscription {
	t.Helper()
	rows := rowsOf(plan, bot)
	if len(rows) != 1 {
		t.Fatalf("bot %s: want exactly one row, got %d (plan error: %v)", bot, len(rows), plan.Err)
	}
	return rows[0]
}

// labelEvent is the event a forge router emits when labels are added to an
// issue of testRepo.
func labelEvent(added ...string) trigger.Event {
	return trigger.Event{
		ID:          "forge:github:" + testBinding + ":delivery-1",
		Source:      trigger.SourceForge,
		Kind:        bundle.ForgeEventIssueLabeled,
		Action:      trigger.ForgeIssueAction,
		TenantID:    testTenant,
		Repo:        testRepo,
		Subject:     trigger.Subject{Type: "issue", ID: "42"},
		AddedLabels: added,
		OccurredAt:  planInstant,
	}
}

// startedBy stores the rows and returns the bots an added label starts, read
// the way the forge router reads them: by the integration's binding, through
// trigger.MatchForgeBound.
func startedBy(t *testing.T, rows []trigger.Subscription, added ...string) []string {
	t.Helper()
	ctx := context.Background()
	st := trigger.NewMemorySubscriptionStore()
	for _, row := range rows {
		if err := st.Create(ctx, row); err != nil {
			t.Fatalf("store row %s of bot %s: %v", row.ID, row.BotID, err)
		}
	}
	return matchedBots(t, st, added...)
}

func matchedBots(t *testing.T, st trigger.SubscriptionStore, added ...string) []string {
	t.Helper()
	match, err := trigger.MatchForgeBound(context.Background(), st, testTenant, testBinding, labelEvent(added...))
	if err != nil {
		t.Fatalf("match the binding's rows: %v", err)
	}
	if !match.Admitted {
		t.Fatalf("the label event is not admitted")
	}
	var bots []string
	for _, row := range match.Matched {
		bots = append(bots, row.BotID)
	}
	return bots
}

func boolPtr(v bool) *bool { return &v }

// sameList compares two string lists in order; nil and empty are the same.
func sameList(got, want []string) bool {
	return len(got) == len(want) && (len(got) == 0 || slices.Equal(got, want))
}

type wantRow struct {
	enabled bool
	labels  []string
	exclude []string
	reason  string
}

func TestPlan_EnableRule(t *testing.T) {
	cases := []struct {
		name     string
		cfg      webhooks.Config
		roles    forgetrigger.RoleBots
		triggers map[string]forge.BotTrigger
		bots     []manifestBot
		want     map[string]wantRow
	}{
		{
			name:  "explicit labels start each bot on its own, where the legacy route refuses both",
			cfg:   botScope("nestor", "saboteur"),
			roles: stockRoles,
			bots: []manifestBot{
				labelBot("nestor", bundle.ExecutionDirect, "nestor"),
				labelBot("saboteur", bundle.ExecutionBoard, "saboteur", "*-chaos"),
			},
			want: map[string]wantRow{
				"nestor":   {enabled: true, labels: []string{"nestor"}},
				"saboteur": {enabled: true, labels: []string{"saboteur", "*-chaos"}},
			},
		},
		{
			name:  "the legacy winner keeps the residual; it sorts before the labelled bot",
			cfg:   botScope("feature-dev", "nestor"),
			roles: stockRoles,
			bots: []manifestBot{
				blockBot("feature-dev"),
				labelBot("nestor", bundle.ExecutionDirect, "nestor"),
			},
			want: map[string]wantRow{
				"feature-dev": {enabled: true, exclude: []string{"nestor"}},
				"nestor":      {enabled: true, labels: []string{"nestor"}},
			},
		},
		{
			name:  "the legacy winner keeps the residual; it sorts after the labelled bots",
			cfg:   webhooks.Config{ID: testWebhook, BotIDs: []string{"alpha", "mid", "zeta"}, DefaultBotID: "zeta"},
			roles: stockRoles,
			bots: []manifestBot{
				labelBot("alpha", bundle.ExecutionDirect, "Bug", " alpha "),
				labelBot("mid", bundle.ExecutionBoard, "*-chaos", "bug"),
				blockBot("zeta"),
			},
			want: map[string]wantRow{
				"alpha": {enabled: true, labels: []string{"Bug", " alpha "}},
				"mid":   {enabled: true, labels: []string{"*-chaos", "bug"}},
				"zeta":  {enabled: true, exclude: []string{"*-chaos", "alpha", "bug"}},
			},
		},
		{
			name:  "the only bot that claims the event is enabled, elected or not",
			cfg:   botScope("solo", "pr-only"),
			roles: stockRoles,
			bots: []manifestBot{
				blockBot("solo"),
				pullRequestBot("pr-only"),
			},
			want: map[string]wantRow{
				"solo": {enabled: true},
			},
		},
		{
			name:  "a bot with no label that is not elected is disabled, with its reason",
			cfg:   webhooks.Config{ID: testWebhook, BotIDs: []string{"aaa", "bbb"}, DefaultBotID: "bbb"},
			roles: stockRoles,
			bots: []manifestBot{
				blockBot("aaa"),
				blockBot("bbb"),
			},
			want: map[string]wantRow{
				"aaa": {reason: noLabelReasonTwoBots},
				"bbb": {enabled: true},
			},
		},
		{
			name:  "the 403 class: the election lands on a reviewer out of scope, so neither bot is enabled",
			cfg:   botScope("aaa", "bbb"),
			roles: stockRoles,
			bots: []manifestBot{
				blockBot("aaa"),
				blockBot("bbb"),
			},
			want: map[string]wantRow{
				"aaa": {reason: noLabelReasonTwoBots},
				"bbb": {reason: noLabelReasonTwoBots},
			},
		},
		{
			name:  "the 403 class: the pinned default is a listed bot the scope does not permit",
			cfg:   webhooks.Config{ID: testWebhook, BotIDs: []string{"bbb"}, DefaultBotID: "aaa"},
			roles: stockRoles,
			bots: []manifestBot{
				blockBot("aaa"),
				blockBot("bbb"),
			},
			want: map[string]wantRow{
				"aaa": {reason: noLabelReasonTwoBots},
				"bbb": {reason: noLabelReasonTwoBots},
			},
		},
		{
			name:  "bot_triggers.enabled decides before the rule, and a disabled row's labels stay the residual's",
			cfg:   botScope("idle", "nestor"),
			roles: stockRoles,
			triggers: map[string]forge.BotTrigger{
				"nestor": {Enabled: boolPtr(false)},
				"idle":   {Enabled: boolPtr(true)},
			},
			bots: []manifestBot{
				blockBot("idle"),
				labelBot("nestor", bundle.ExecutionDirect, "nestor"),
			},
			want: map[string]wantRow{
				"idle":   {enabled: true},
				"nestor": {labels: []string{"nestor"}, reason: "disabled on the integration (bot_triggers.enabled is false): enable it there to start it again"},
			},
		},
		{
			name:  "bot_triggers.labels replace the manifest's, and an empty set claims any label",
			cfg:   botScope("feature-dev", "nestor", "saboteur"),
			roles: stockRoles,
			triggers: map[string]forge.BotTrigger{
				"nestor":   {Labels: []string{"triage"}},
				"saboteur": {Labels: []string{}},
			},
			bots: []manifestBot{
				blockBot("feature-dev"),
				labelBot("nestor", bundle.ExecutionDirect, "nestor"),
				labelBot("saboteur", bundle.ExecutionBoard, "saboteur"),
			},
			want: map[string]wantRow{
				"feature-dev": {enabled: true, exclude: []string{"triage"}},
				"nestor":      {enabled: true, labels: []string{"triage"}},
				"saboteur":    {enabled: true},
			},
		},
		{
			name:  "a residual pin keeps a bot on the residual when its manifest declares labels",
			cfg:   botScope("legacy-dev", "nestor"),
			roles: stockRoles,
			triggers: map[string]forge.BotTrigger{
				"legacy-dev": {Residual: true},
			},
			bots: []manifestBot{
				blockBot("legacy-dev", "feature"),
				labelBot("nestor", bundle.ExecutionDirect, "nestor"),
			},
			want: map[string]wantRow{
				"legacy-dev": {enabled: true, exclude: []string{"nestor"}},
				"nestor":     {enabled: true, labels: []string{"nestor"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planBothOrders(t, tc.cfg, tc.roles, tc.triggers, tc.bots...)
			if plan.Err != nil {
				t.Fatalf("plan refused: %v", plan.Err)
			}
			if len(plan.Rows) != len(tc.want) {
				t.Errorf("want %d rows, got %d", len(tc.want), len(plan.Rows))
			}
			for bot, want := range tc.want {
				row := rowOf(t, plan, bot)
				if row.Enabled != want.enabled {
					t.Errorf("bot %s: enabled = %v, want %v (reason %q)", bot, row.Enabled, want.enabled, row.DisabledReason)
				}
				if !sameList(row.Match.AddedLabels, want.labels) {
					t.Errorf("bot %s: added_labels = %q, want %q", bot, row.Match.AddedLabels, want.labels)
				}
				if !sameList(row.Match.ExcludeAddedLabels, want.exclude) {
					t.Errorf("bot %s: exclude_added_labels = %q, want %q", bot, row.Match.ExcludeAddedLabels, want.exclude)
				}
				if row.DisabledReason != want.reason {
					t.Errorf("bot %s: disabled_reason = %q, want %q", bot, row.DisabledReason, want.reason)
				}
			}
		})
	}
}

func TestPlan_WinnerResidualExcludesDeclaredLabels(t *testing.T) {
	cases := []struct {
		name          string
		cfg           webhooks.Config
		winner, named manifestBot
	}{
		{
			name:   "the winner sorts before the labelled bot",
			cfg:    botScope("feature-dev", "nestor"),
			winner: blockBot("feature-dev"),
			named:  labelBot("nestor", bundle.ExecutionDirect, "nestor"),
		},
		{
			name:   "the winner sorts after the labelled bot",
			cfg:    webhooks.Config{ID: testWebhook, BotIDs: []string{"nestor", "zz-dev"}, DefaultBotID: "zz-dev"},
			winner: blockBot("zz-dev"),
			named:  labelBot("nestor", bundle.ExecutionDirect, "nestor"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planBothOrders(t, tc.cfg, stockRoles, nil, tc.winner, tc.named)
			if plan.Err != nil {
				t.Fatalf("plan refused: %v", plan.Err)
			}
			for _, probe := range []struct {
				added []string
				want  []string
			}{
				{added: []string{"nestor"}, want: []string{tc.named.id}},
				{added: []string{"Nestor"}, want: []string{tc.named.id}},
				{added: []string{"bug"}, want: []string{tc.winner.id}},
				// Each bot claims the label that is its own: one event
				// that adds both starts both.
				{added: []string{"bug", "nestor"}, want: sorted(tc.winner.id, tc.named.id)},
			} {
				if got := startedBy(t, plan.Rows, probe.added...); !slices.Equal(got, probe.want) {
					t.Errorf("adding %q starts %q, want %q", probe.added, got, probe.want)
				}
			}
		})
	}
}

func sorted(values ...string) []string {
	slices.Sort(values)
	return values
}

// TestPlan_ForgeBlockOnlyBot_RoutedLikeLegacy plans a bot the way the server
// hands one over: its effective invocations, which for a manifest with no
// `invocations:` block are bundle.EffectiveInvocations' migration shim. Read
// as declared, the shim's direct invocation would project a direct row; the
// bot is started by the block's label route today, which is board mode. The
// shim must therefore project the synthetic board row, and a manifest that
// declares its invocation by hand keeps the direct row it asks for.
func TestPlan_ForgeBlockOnlyBot_RoutedLikeLegacy(t *testing.T) {
	fr := &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventIssueLabeled}, Labels: []string{"securite"}}
	shimmed := &bundle.Manifest{Forge: fr}
	declared := &bundle.Manifest{Forge: fr, Invocations: []bundle.Invocation{labelInvocation(bundle.ExecutionDirect, "securite")}}
	if len(bundle.EffectiveInvocations(shimmed)) == 0 {
		t.Fatalf("control: the shim of a forge:-block-only manifest is empty, so the case does not exercise it")
	}
	plan := planBothOrders(t, botScope("nestor"), stockRoles, nil,
		manifestBot{id: "nestor", fr: fr, invs: bundle.EffectiveInvocations(shimmed)})
	if plan.Err != nil {
		t.Fatalf("plan refused: %v", plan.Err)
	}
	row := rowOf(t, plan, "nestor")
	if row.Mode != bundle.ExecutionBoard {
		t.Errorf("the shim bot's row runs in mode %q, want board: the label route such a bot takes today is board", row.Mode)
	}
	if !row.Enabled {
		t.Errorf("the sole claimer of the integration derives disabled")
	}
	if !sameList(row.Match.AddedLabels, []string{"securite"}) {
		t.Errorf("the shim bot claims %q, want [securite]: the block's labels", row.Match.AddedLabels)
	}
	if row.ArgsVar != bundle.ForgeIssueDefaultArgsVar || !row.OpensMR {
		t.Errorf("the shim bot's row carries args_var %q and opens_mr %v, want the implementer contract (%q, true)",
			row.ArgsVar, row.OpensMR, bundle.ForgeIssueDefaultArgsVar)
	}
	if got := startedBy(t, plan.Rows, "securite"); !slices.Equal(got, []string{"nestor"}) {
		t.Errorf("the label securite starts %q, want [nestor]", got)
	}

	// The same manifest with a declared invocation keeps the mode it asks
	// for: the shim reading stops where the author's own list starts.
	plan = planBothOrders(t, botScope("nestor"), stockRoles, nil,
		manifestBot{id: "nestor", fr: fr, invs: bundle.EffectiveInvocations(declared)})
	if plan.Err != nil {
		t.Fatalf("plan refused: %v", plan.Err)
	}
	if row := rowOf(t, plan, "nestor"); row.Mode != bundle.ExecutionDirect {
		t.Errorf("a declared direct invocation projects mode %q, want direct", row.Mode)
	}
}

func TestPlan_WinnerIsSelectIssueLabeledBotsPick(t *testing.T) {
	cases := []struct {
		name  string
		cfg   webhooks.Config
		roles forgetrigger.RoleBots
		// claimers all claim issue_labeled with no label, so only the
		// election can enable one of them.
		claimers []string
		// The legacy election's answer for cfg, checked against
		// webhooks.LegacyIssueWinner itself.
		legacyPick      string
		legacyPermitted bool
		// residual is the claimer the plan enables on the residual.
		residual string
		// warns is a bot a warning must name.
		warns string
	}{
		{
			name: "a pinned default in scope", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"aaa", "bbb"}, DefaultBotID: "bbb"},
			claimers: []string{"aaa", "bbb"}, legacyPick: "bbb", legacyPermitted: true, residual: "bbb",
		},
		{
			name: "the implementer in scope, no default", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"aaa", "feature-dev"}},
			claimers: []string{"aaa", "feature-dev"}, legacyPick: "feature-dev", legacyPermitted: true, residual: "feature-dev",
		},
		{
			name: "a wildcard scope admits the implementer", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"*"}, WildcardBots: true},
			claimers: []string{"aaa", "feature-dev"}, legacyPick: "feature-dev", legacyPermitted: true, residual: "feature-dev",
		},
		{
			name: "the default outranks the implementer", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"aaa", "feature-dev"}, DefaultBotID: "aaa"},
			claimers: []string{"aaa", "feature-dev"}, legacyPick: "aaa", legacyPermitted: true, residual: "aaa",
		},
		{
			name: "the only bot in scope, the implementer out of it", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"aaa"}},
			claimers: []string{"aaa", "bbb"}, legacyPick: "aaa", legacyPermitted: true, residual: "aaa",
		},
		{
			name: "a pinned default the scope does not permit", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"bbb", "ccc"}, DefaultBotID: "aaa"},
			claimers: []string{"aaa", "bbb"}, legacyPick: "aaa", legacyPermitted: false,
		},
		{
			name: "the reviewer fallback, out of scope", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"aaa", "bbb"}},
			claimers: []string{"aaa", "bbb"}, legacyPick: "review-pr", legacyPermitted: false,
		},
		{
			name: "the reviewer fallback, in scope, is not derived", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"aaa", "review-pr"}},
			claimers: []string{"aaa", "review-pr"}, legacyPick: "review-pr", legacyPermitted: true, warns: "review-pr",
		},
		{
			name: "the reviewer pinned as the default is no fallback", roles: stockRoles,
			cfg:      webhooks.Config{BotIDs: []string{"aaa", "review-pr"}, DefaultBotID: "review-pr"},
			claimers: []string{"aaa", "review-pr"}, legacyPick: "review-pr", legacyPermitted: true, residual: "review-pr",
		},
		{
			name:     "no role bot and several bots in scope",
			cfg:      webhooks.Config{BotIDs: []string{"aaa", "bbb"}},
			claimers: []string{"aaa", "bbb"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pick, permitted := webhooks.LegacyIssueWinner(tc.cfg, tc.roles.Implementer, tc.roles.Reviewer)
			if pick != tc.legacyPick || permitted != tc.legacyPermitted {
				t.Fatalf("webhooks.LegacyIssueWinner = (%q, %v); this row states (%q, %v)", pick, permitted, tc.legacyPick, tc.legacyPermitted)
			}
			var bots []manifestBot
			for _, id := range tc.claimers {
				bots = append(bots, blockBot(id))
			}
			tc.cfg.ID = testWebhook
			plan := planBothOrders(t, tc.cfg, tc.roles, nil, bots...)
			if plan.Err != nil {
				t.Fatalf("plan refused: %v", plan.Err)
			}
			var enabled []string
			for _, row := range plan.Rows {
				if row.Enabled {
					enabled = append(enabled, row.BotID)
				}
			}
			var want []string
			if tc.residual != "" {
				want = []string{tc.residual}
			}
			if !slices.Equal(enabled, want) {
				t.Errorf("the plan enables %q on the residual, want %q (legacy pick %q, permitted %v)", enabled, want, pick, permitted)
			}
			named := slices.ContainsFunc(plan.Report.Warnings, func(w string) bool {
				return strings.Contains(w, "reviewer fallback")
			})
			if tc.warns == "" && named {
				t.Errorf("unexpected reviewer-fallback warning: %q", plan.Report.Warnings)
			}
			if tc.warns != "" && !slices.ContainsFunc(plan.Report.Warnings, func(w string) bool {
				return strings.Contains(w, "reviewer fallback") && strings.HasPrefix(w, "bot "+tc.warns+":")
			}) {
				t.Errorf("no warning names the reviewer fallback of %s: %q", tc.warns, plan.Report.Warnings)
			}
		})
	}
}

func TestPlan_AdoptionPinsResidualWinnerAndDisabledBots(t *testing.T) {
	ctx := context.Background()
	cfg := botScope("feature-dev", "held", "idle", "nestor")
	held := map[string]forge.BotTrigger{"held": {Enabled: boolPtr(true)}}
	asShipped := []manifestBot{
		blockBot("feature-dev"), // the legacy winner: the implementer in scope
		blockBot("held"),        // no label, and an entry the operator wrote
		blockBot("idle"),        // no label, not elected
		labelBot("nestor", bundle.ExecutionDirect, "nestor"),
	}

	// The first projection, on the real integration store: the pins are
	// stamped the way the orchestrator stamps them.
	integrations := forge.NewMemoryRepoIntegrationStore()
	first := planInput(cfg, asShipped...)
	first.Integration.BotTriggers = held
	if err := integrations.Create(ctx, first.Integration); err != nil {
		t.Fatalf("create the integration: %v", err)
	}
	firstPlan := forgetrigger.PlanIntegration(first, stockRoles, planInstant)
	if firstPlan.Err != nil {
		t.Fatalf("first plan refused: %v", firstPlan.Err)
	}
	wantPins := map[string]forge.BotTrigger{
		"feature-dev": {Residual: true},
		"idle":        {Enabled: boolPtr(false)},
	}
	if !reflect.DeepEqual(firstPlan.Report.AdoptionPins, wantPins) {
		t.Errorf("adoption pins = %s, want %s", describePins(firstPlan.Report.AdoptionPins), describePins(wantPins))
	}
	if err := integrations.StampSubscriptionSync(ctx, testIntegration, firstPlan.Report.AdoptionPins, planInstant, ""); err != nil {
		t.Fatalf("stamp the first projection: %v", err)
	}
	adopted, err := integrations.Get(ctx, testIntegration)
	if err != nil {
		t.Fatalf("read the adopted integration: %v", err)
	}
	if adopted.SubscriptionsAdoptedAt == nil {
		t.Fatalf("the first projection is not stamped as adopted")
	}

	// A later projection of the same manifests pins nothing, and derives
	// the rows the first one derived.
	again := planInput(cfg, asShipped...)
	again.Integration = adopted
	againPlan := forgetrigger.PlanIntegration(again, stockRoles, planInstant)
	if againPlan.Report.AdoptionPins != nil {
		t.Errorf("a projection after adoption pins %s, want none", describePins(againPlan.Report.AdoptionPins))
	}
	if !reflect.DeepEqual(againPlan.Rows, firstPlan.Rows) {
		t.Errorf("the rows move once the pins are stored:\n first %+v\n again %+v", firstPlan.Rows, againPlan.Rows)
	}

	// A bot that joins the integration after adoption is derived, never
	// pinned: only the first projection freezes anything.
	joined := planInput(botScope("feature-dev", "held", "idle", "late", "nestor"), append(slices.Clone(asShipped), blockBot("late"))...)
	joined.Integration.BotTriggers = adopted.BotTriggers
	joined.Integration.SubscriptionsAdoptedAt = adopted.SubscriptionsAdoptedAt
	joinedPlan := forgetrigger.PlanIntegration(joined, stockRoles, planInstant)
	if row := rowOf(t, joinedPlan, "late"); row.Enabled {
		t.Fatalf("control: the bot that joined derives an enabled row, so there is no state a pin would freeze")
	}
	if joinedPlan.Report.AdoptionPins != nil {
		t.Errorf("a projection after adoption pins %s for a bot that joined later, want none", describePins(joinedPlan.Report.AdoptionPins))
	}

	// Later, two manifests gain labels: the disabled bot's, and the
	// winner's.
	later := []manifestBot{
		blockBot("feature-dev", "feature"),
		blockBot("held"),
		blockBot("idle", "idle-go"),
		labelBot("nestor", bundle.ExecutionDirect, "nestor"),
	}
	pinned := planInput(cfg, later...)
	pinned.Integration = adopted
	pinnedPlan := forgetrigger.PlanIntegration(pinned, stockRoles, planInstant)
	if pinnedPlan.Err != nil {
		t.Fatalf("plan after the manifests gained labels refused: %v", pinnedPlan.Err)
	}
	if row := rowOf(t, pinnedPlan, "idle"); row.Enabled {
		t.Errorf("a label its manifest gained enables idle, which adoption left disabled")
	}
	for _, probe := range []struct{ added, want string }{
		{added: "idle-go", want: "feature-dev held"},
		{added: "bug", want: "feature-dev held"},
		{added: "nestor", want: "nestor"},
	} {
		if got := strings.Join(startedBy(t, pinnedPlan.Rows, probe.added), " "); got != probe.want {
			t.Errorf("after adoption, adding %q starts [%s], want [%s]", probe.added, got, probe.want)
		}
	}

	// The same manifests on an integration that holds no pin: the labels
	// decide. This is what the pins prevent.
	unpinned := planInput(cfg, later...)
	unpinned.Integration.BotTriggers = held
	unpinnedPlan := forgetrigger.PlanIntegration(unpinned, stockRoles, planInstant)
	if got := strings.Join(startedBy(t, unpinnedPlan.Rows, "idle-go"), " "); got != "idle" {
		t.Fatalf("control: without pins, adding %q starts [%s], want [idle]", "idle-go", got)
	}
	if got := strings.Join(startedBy(t, unpinnedPlan.Rows, "bug"), " "); got != "held" {
		t.Fatalf("control: without pins, adding %q starts [%s], want [held]", "bug", got)
	}
}

func describePins(pins map[string]forge.BotTrigger) string {
	bots := make([]string, 0, len(pins))
	for bot := range pins {
		bots = append(bots, bot)
	}
	slices.Sort(bots)
	var b strings.Builder
	b.WriteString("{")
	for _, bot := range bots {
		pin := pins[bot]
		enabled := "unset"
		if pin.Enabled != nil {
			enabled = map[bool]string{true: "true", false: "false"}[*pin.Enabled]
		}
		b.WriteString(" " + bot + ":(enabled=" + enabled)
		if pin.Residual {
			b.WriteString(" residual")
		}
		if pin.Labels != nil {
			b.WriteString(" labels=[" + strings.Join(pin.Labels, " ") + "]")
		}
		b.WriteString(")")
	}
	return b.String() + " }"
}

func TestPlan_OverlappingRowsOfOneBotRefused(t *testing.T) {
	twoRows := func(id string, first, second []string) manifestBot {
		return manifestBot{id: id, invs: []bundle.Invocation{
			labelInvocation(bundle.ExecutionDirect, first...),
			labelInvocation(bundle.ExecutionBoard, second...),
		}}
	}
	rowIDs := func(b manifestBot) []string {
		var ids []string
		for i := range b.invs {
			ids = append(ids, trigger.DerivedForgeSubscriptionID(testTenant, b.id, bundle.InvocationKey(b.invs, i), testIntegration))
		}
		slices.Sort(ids)
		return ids
	}
	nestor := labelBot("nestor", bundle.ExecutionDirect, "nestor")

	refused := []struct {
		name     string
		bot      manifestBot
		triggers map[string]forge.BotTrigger
	}{
		{name: "a literal both rows declare, whatever its case", bot: twoRows("duo", []string{"deploy", "Urgent"}, []string{" urgent ", "triage"})},
		{name: "a suffix wildcard on one side", bot: twoRows("duo", []string{"*-chaos"}, []string{"deploy"})},
		{name: "a star on one side", bot: twoRows("duo", []string{"deploy"}, []string{"*"})},
		{name: "a row with no label beside a labelled one", bot: twoRows("duo", []string{"deploy"}, nil)},
		{
			name:     "bot_triggers labels on a bot that derives two rows",
			bot:      twoRows("duo", []string{"deploy"}, []string{"triage"}),
			triggers: map[string]forge.BotTrigger{"duo": {Labels: []string{"ship"}}},
		},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			plan := planBothOrders(t, botScope("duo", "nestor"), stockRoles, tc.triggers, tc.bot, nestor)
			var refusal *forgetrigger.ProjectionError
			if !errors.As(plan.Err, &refusal) {
				t.Fatalf("plan error = %v, want a *ProjectionError", plan.Err)
			}
			if !errors.Is(plan.Err, forgetrigger.ErrProjection) {
				t.Errorf("the refusal does not match ErrProjection")
			}
			if refusal.Code != "overlapping_rows" {
				t.Errorf("code = %q, want overlapping_rows", refusal.Code)
			}
			if refusal.Code != forge.RoutingRefusedOverlappingRows {
				t.Errorf("code = %q, want %q, the code the routing switch refuses a lane with", refusal.Code, forge.RoutingRefusedOverlappingRows)
			}
			if refusal.Bot != "duo" {
				t.Errorf("the refusal names bot %q, want duo", refusal.Bot)
			}
			if want := rowIDs(tc.bot); !slices.Equal(refusal.Rows, want) {
				t.Errorf("the refusal names rows %q, want %q", refusal.Rows, want)
			}
			if len(plan.Rows) != 0 {
				t.Errorf("a refused plan carries %d rows to write, want none", len(plan.Rows))
			}
			if len(plan.Report.Overlaps) != 1 {
				t.Fatalf("the report names %d overlaps, want one: %q", len(plan.Report.Overlaps), plan.Report.Overlaps)
			}
			overlap := plan.Report.Overlaps[0]
			ids := rowIDs(tc.bot)
			if !strings.HasPrefix(overlap, "duo: ") || !strings.Contains(overlap, " ∩ ") ||
				!strings.Contains(overlap, ids[0]) || !strings.Contains(overlap, ids[1]) {
				t.Errorf("overlap %q does not read \"duo: <row> ∩ <row>\" over rows %q", overlap, ids)
			}
			if plan.Report.AdoptionPins != nil {
				t.Errorf("a refused plan pins %s", describePins(plan.Report.AdoptionPins))
			}
		})
	}

	accepted := []struct {
		name     string
		bot      manifestBot
		triggers map[string]forge.BotTrigger
		rows     int
	}{
		{name: "two rows of disjoint literals", bot: twoRows("duo", []string{"deploy"}, []string{"triage"}), rows: 2},
		// The same two rows declared the other way round: whichever order
		// their ids sort in, one of the two manifests declares them in the
		// other one.
		{name: "two rows of disjoint literals, declared the other way round", bot: twoRows("duo", []string{"triage"}, []string{"deploy"}), rows: 2},
		{
			name:     "bot_triggers labels on a bot that derives one row",
			bot:      labelBot("duo", bundle.ExecutionDirect, "deploy"),
			triggers: map[string]forge.BotTrigger{"duo": {Labels: []string{"*"}}},
			rows:     1,
		},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			plan := planBothOrders(t, botScope("duo", "nestor"), stockRoles, tc.triggers, tc.bot, nestor)
			if plan.Err != nil {
				t.Fatalf("plan refused: %v", plan.Err)
			}
			if got := len(rowsOf(plan, "duo")); got != tc.rows {
				t.Errorf("bot duo derives %d rows, want %d", got, tc.rows)
			}
			if len(plan.Report.Overlaps) != 0 {
				t.Errorf("the report names overlaps %q", plan.Report.Overlaps)
			}
		})
	}

	t.Run("two bots at fault: the refusal names the first, the report both", func(t *testing.T) {
		beta := twoRows("beta", []string{"x", "left"}, []string{"x", "right"})
		alpha := twoRows("alpha", []string{"*"}, []string{"y"})
		plan := planBothOrders(t, botScope("alpha", "beta"), stockRoles, nil, beta, alpha)
		var refusal *forgetrigger.ProjectionError
		if !errors.As(plan.Err, &refusal) {
			t.Fatalf("plan error = %v, want a *ProjectionError", plan.Err)
		}
		if refusal.Bot != "alpha" || !slices.Equal(refusal.Rows, rowIDs(alpha)) {
			t.Errorf("the refusal names bot %q rows %q, want alpha %q", refusal.Bot, refusal.Rows, rowIDs(alpha))
		}
		if len(plan.Report.Overlaps) != 2 || !strings.HasPrefix(plan.Report.Overlaps[0], "alpha: ") || !strings.HasPrefix(plan.Report.Overlaps[1], "beta: ") {
			t.Errorf("overlaps = %q, want alpha's then beta's", plan.Report.Overlaps)
		}
		for _, overlap := range plan.Report.Overlaps {
			if !strings.Contains(refusal.Error(), overlap) {
				t.Errorf("the refusal %q does not name overlap %q", refusal.Error(), overlap)
			}
		}
	})
}

func TestPlan_RowsCarryWhatTheirInvocationSays(t *testing.T) {
	nestor := manifestBot{
		id: "nestor",
		fr: &bundle.ForgeRequirements{Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"force": "false", "tone": "dry"}}},
		invs: []bundle.Invocation{{
			Kind:        bundle.InvocationKindForge,
			ContextVars: map[string]string{"force": "true"},
			Forge:       &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"nestor"}},
		}},
	}
	saboteur := manifestBot{
		id: "saboteur",
		invs: []bundle.Invocation{{
			Kind:    bundle.InvocationKindForge,
			Mode:    bundle.ExecutionBoard,
			ArgsVar: "mischief",
			Forge:   &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"saboteur"}},
		}},
	}
	featureDev := manifestBot{
		id: "feature-dev",
		fr: &bundle.ForgeRequirements{
			Events:  []string{bundle.ForgeEventIssueLabeled},
			Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"open_mr": "true"}},
		},
		invs: []bundle.Invocation{{
			Kind:        bundle.InvocationKindCommand,
			ArgsVar:     "task_text",
			ContextVars: map[string]string{"from_command": "true"},
			Command:     &bundle.InvocationCommand{Name: "feature"},
		}},
	}
	plan := planBothOrders(t, botScope("feature-dev", "nestor", "saboteur"), stockRoles, nil, nestor, saboteur, featureDev)
	if plan.Err != nil {
		t.Fatalf("plan refused: %v", plan.Err)
	}
	var order []string
	for _, row := range plan.Rows {
		order = append(order, row.BotID)
	}
	if !slices.Equal(order, []string{"feature-dev", "nestor", "saboteur"}) {
		t.Fatalf("the rows are those of %q, want one per bot, sorted by bot", order)
	}

	type want struct {
		key     string
		mode    bundle.ExecutionMode
		argsVar string
		opensMR bool
		vars    map[string]string
	}
	for bot, w := range map[string]want{
		// A declared invocation: its own mode (empty is direct) and args
		// var, its context vars over the manifest's launch vars.
		"nestor": {
			key: bundle.InvocationKey(nestor.invs, 0), mode: bundle.ExecutionDirect, argsVar: "feature_prompt", opensMR: true,
			vars: map[string]string{"force": "true", "tone": "dry"},
		},
		"saboteur": {
			key: bundle.InvocationKey(saboteur.invs, 0), mode: bundle.ExecutionBoard, argsVar: "mischief", opensMR: true,
		},
		// A forge: block alone: the label route — board, the first
		// command's args var and opens_mr, and none of its context vars.
		"feature-dev": {
			key: "legacy:issue_labeled", mode: bundle.ExecutionBoard, argsVar: "task_text", opensMR: false,
			vars: map[string]string{"open_mr": "true"},
		},
	} {
		row := rowOf(t, plan, bot)
		if wantID := trigger.DerivedForgeSubscriptionID(testTenant, bot, w.key, testIntegration); row.ID != wantID {
			t.Errorf("bot %s: id = %s, want %s (key %q)", bot, row.ID, wantID, w.key)
		}
		if row.ForgeBinding != testBinding || row.Origin != testOrigin {
			t.Errorf("bot %s: binding %q origin %q, want %q and %q", bot, row.ForgeBinding, row.Origin, testBinding, testOrigin)
		}
		if row.TenantID != testTenant || row.Repo != testRepo || row.Invocation != bundle.InvocationKindForge {
			t.Errorf("bot %s: tenant %q repo %q invocation %q", bot, row.TenantID, row.Repo, row.Invocation)
		}
		if row.Mode != w.mode || row.ArgsVar != w.argsVar || row.OpensMR != w.opensMR {
			t.Errorf("bot %s: mode %q args_var %q opens_mr %v, want %q %q %v", bot, row.Mode, row.ArgsVar, row.OpensMR, w.mode, w.argsVar, w.opensMR)
		}
		if len(row.Vars) != len(w.vars) || !reflect.DeepEqual(nilIfEmpty(row.Vars), nilIfEmpty(w.vars)) {
			t.Errorf("bot %s: vars = %v, want %v", bot, row.Vars, w.vars)
		}
		if row.ProjectionDigest == "" || row.ProjectionDigest != plan.Report.Digest {
			t.Errorf("bot %s: projection digest %q, the report's is %q", bot, row.ProjectionDigest, plan.Report.Digest)
		}
		if !row.CreatedAt.Equal(planInstant) || !row.UpdatedAt.Equal(planInstant) {
			t.Errorf("bot %s: stamped %v / %v, want the planning instant", bot, row.CreatedAt, row.UpdatedAt)
		}
	}
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func TestPlan_ReportNamesWhatEachBotClaimsAndWhy(t *testing.T) {
	deliver := manifestBot{id: "courier", invs: []bundle.Invocation{labelInvocation(bundle.ExecutionDeliver, "ship")}}
	deliverToo := manifestBot{id: "mailer", invs: []bundle.Invocation{labelInvocation(bundle.ExecutionDeliver, "post")}}
	plan := planBothOrders(t,
		webhooks.Config{ID: testWebhook, BotIDs: []string{"*"}, WildcardBots: true},
		stockRoles,
		map[string]forge.BotTrigger{
			"any-bot":  {Labels: []string{}},
			"reviewer": {LaunchVars: map[string]string{"k": "v"}},
		},
		blockBot("feature-dev"),
		labelBot("nestor", bundle.ExecutionDirect, "nestor", "Triage"),
		labelBot("any-bot", bundle.ExecutionDirect, "ignored"),
		blockBot("idle"),
		pullRequestBot("reviewer"),
		deliver, deliverToo,
	)
	if plan.Err != nil {
		t.Fatalf("plan refused: %v", plan.Err)
	}
	wantEnabled := map[string]map[string][]string{"issues": {
		"feature-dev": {"*", "!nestor", "!triage"},
		"nestor":      {"nestor", "Triage"},
		"any-bot":     {"*"},
	}}
	if !reflect.DeepEqual(plan.Report.Enabled, wantEnabled) {
		t.Errorf("enabled = %v, want %v", plan.Report.Enabled, wantEnabled)
	}
	wantDisabled := map[string]string{
		"idle": "declares issue_labeled with no label while 7 bots share acme/shop: set its labels (bot_triggers) or enable it",
	}
	if !reflect.DeepEqual(plan.Report.Disabled, wantDisabled) {
		t.Errorf("disabled = %v, want %v", plan.Report.Disabled, wantDisabled)
	}
	for _, fragment := range []string{
		"bot courier: invocations[0]: derives no issue_labeled route",
		"bot mailer: invocations[0]: derives no issue_labeled route",
		"bot reviewer: bot_triggers is set, and the bot's manifest claims no issue_labeled route",
		"bot reviewer: bot_triggers.launch_vars is stored and applied to no row yet",
	} {
		if !slices.ContainsFunc(plan.Report.Warnings, func(w string) bool { return strings.HasPrefix(w, fragment) }) {
			t.Errorf("no warning starts with %q: %q", fragment, plan.Report.Warnings)
		}
	}
}

func TestPlan_HandPinnedNamesTheConfigVarsNoManifestDeclares(t *testing.T) {
	withVars := func(id string, vars map[string]string) manifestBot {
		b := labelBot(id, bundle.ExecutionDirect, id)
		b.fr = &bundle.ForgeRequirements{Webhook: &bundle.ForgeWebhookHints{LaunchVars: vars}}
		return b
	}
	bots := []manifestBot{
		withVars("alpha", map[string]string{"shared": "from-alpha", "tone": "dry"}),
		withVars("beta", map[string]string{"shared": "from-beta"}),
	}
	cases := []struct {
		name   string
		config map[string]string
		want   []string
	}{
		{name: "the union a provision writes: a later bot's value over an earlier one's", config: map[string]string{"shared": "from-beta", "tone": "dry"}},
		{name: "a key no manifest declares", config: map[string]string{"shared": "from-beta", "tone": "dry", "pinned": "by-hand"}, want: []string{"pinned"}},
		{name: "a declared key whose value was edited", config: map[string]string{"shared": "from-beta", "tone": "wet"}, want: []string{"tone"}},
		{name: "a declared key the config lost", config: map[string]string{"shared": "from-beta"}, want: []string{"tone"}},
		{name: "the earlier bot's value is not the union's", config: map[string]string{"shared": "from-alpha", "tone": "dry"}, want: []string{"shared"}},
		{name: "several, sorted", config: map[string]string{"zeta": "1", "alpha": "2"}, want: []string{"alpha", "shared", "tone", "zeta"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := botScope("alpha", "beta")
			cfg.LaunchVars = tc.config
			plan := planBothOrders(t, cfg, stockRoles, nil, bots...)
			if !sameList(plan.Report.HandPinned, tc.want) {
				t.Errorf("hand_pinned = %q, want %q", plan.Report.HandPinned, tc.want)
			}
		})
	}
}

func TestPlan_RowWithNoIntegrationToBindToIsRefused(t *testing.T) {
	in := planInput(botScope("nestor"), labelBot("nestor", bundle.ExecutionDirect, "nestor"))
	in.Integration.ID = ""
	plan := forgetrigger.PlanIntegration(in, stockRoles, planInstant)
	var refusal *forgetrigger.ProjectionError
	if !errors.As(plan.Err, &refusal) {
		t.Fatalf("plan error = %v, want a *ProjectionError: a row bound to no integration would be a bus subscription", plan.Err)
	}
	if refusal.Code != forgetrigger.ProjectionUnbuildableRow || refusal.Bot != "nestor" {
		t.Errorf("refusal = code %q bot %q, want %q for nestor", refusal.Code, refusal.Bot, forgetrigger.ProjectionUnbuildableRow)
	}
	if len(plan.Rows) != 0 {
		t.Errorf("a refused plan carries %d rows", len(plan.Rows))
	}
}
