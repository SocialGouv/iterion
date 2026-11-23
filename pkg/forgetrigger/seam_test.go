package forgetrigger_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// seam is the projector wired the way the server wires it: behind
// forge.Orchestrator.Subscriptions, over the real integration, webhook and
// subscription stores. The catalogue is what the orchestrator's two lookups
// read; a test edits it to stand for a manifest edit.
type seam struct {
	orch         *forge.Orchestrator
	integrations *forge.MemoryRepoIntegrationStore
	configs      *webhooks.MemoryConfigStore
	subs         *trigger.MemorySubscriptionStore
	catalogue    map[string]manifestBot
}

func newSeam(t *testing.T, cfg webhooks.Config, bots ...manifestBot) *seam {
	t.Helper()
	ctx := context.Background()
	s := &seam{
		integrations: forge.NewMemoryRepoIntegrationStore(),
		configs:      webhooks.NewMemoryConfigStore(),
		subs:         trigger.NewMemorySubscriptionStore(),
		catalogue:    map[string]manifestBot{},
	}
	in := planInput(cfg, bots...)
	for _, b := range bots {
		s.catalogue[b.id] = b
	}
	if err := s.configs.Create(ctx, in.Config); err != nil {
		t.Fatalf("create the webhook config: %v", err)
	}
	if err := s.integrations.Create(ctx, in.Integration); err != nil {
		t.Fatalf("create the integration: %v", err)
	}
	s.orch = &forge.Orchestrator{
		Integrations: s.integrations,
		Webhooks:     s.configs,
		Bots: func(_ context.Context, _, botID string) (*bundle.ForgeRequirements, error) {
			b, ok := s.catalogue[botID]
			if !ok {
				return nil, fmt.Errorf("bot %q is not in the catalogue", botID)
			}
			return b.fr, nil
		},
		Invocations: func(_ context.Context, _, botID string) ([]bundle.Invocation, error) {
			return s.catalogue[botID].invs, nil
		},
		Subscriptions: testProjector(s.subs),
		Now:           func() time.Time { return planInstant },
	}
	return s
}

func (s *seam) integration(t *testing.T) forge.RepoIntegration {
	t.Helper()
	ri, err := s.integrations.Get(context.Background(), testIntegration)
	if err != nil {
		t.Fatalf("read the integration: %v", err)
	}
	return ri
}

func TestProjector_BehindTheOrchestratorSeam(t *testing.T) {
	ctx := context.Background()
	s := newSeam(t, demoScope(), demoFeatureDev(), demoNestor("true"), demoSaboteur())

	// The first sync projects the rows and freezes the residual winner.
	report, err := s.orch.SyncSubscriptions(ctx, testTenant, testIntegration)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if want := (map[string]forge.BotTrigger{"feature-dev": {Residual: true}}); !reflect.DeepEqual(report.AdoptionPins, want) {
		t.Errorf("the first sync pins %s, want %s", describePins(report.AdoptionPins), describePins(want))
	}
	ri := s.integration(t)
	if ri.SubscriptionsAdoptedAt == nil || ri.SubscriptionsSyncError != "" || !ri.BotTriggers["feature-dev"].Residual {
		t.Fatalf("the integration is not stamped as adopted with its pin: adopted %v, sync error %q, bot_triggers %s",
			ri.SubscriptionsAdoptedAt, ri.SubscriptionsSyncError, describePins(ri.BotTriggers))
	}
	rows := storedRows(t, s.subs, testTenant)
	if len(rows) != 3 {
		t.Fatalf("the first sync stored %d rows, want 3", len(rows))
	}
	for added, want := range map[string]string{"nestor": "nestor", "saboteur": "saboteur", "bug": "feature-dev"} {
		if got := strings.Join(matchedBots(t, s.subs, added), " "); got != want {
			t.Errorf("adding %q starts [%s], want [%s]", added, got, want)
		}
	}

	// A second sync reads the pin back and changes nothing.
	again, err := s.orch.SyncSubscriptions(ctx, testTenant, testIntegration)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if again.AdoptionPins != nil {
		t.Errorf("a sync after adoption pins %s", describePins(again.AdoptionPins))
	}
	if after := storedRows(t, s.subs, testTenant); !reflect.DeepEqual(after, rows) {
		t.Errorf("a second sync moved the rows:\n before %+v\n after  %+v", rows, after)
	}

	// The report is what the routing switch reads: the lane goes to the
	// spine.
	result, err := s.orch.SetForgeRouting(ctx, testTenant, testIntegration, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if err != nil {
		t.Fatalf("switch the issue lane to the spine: %v", err)
	}
	if result.Mode != webhooks.RoutingSpine {
		t.Fatalf("the stored config routes the issue lane by %q, want the spine", result.Mode)
	}

	// On a spine-routed integration, a manifest edit that makes one bot's
	// rows overlap fails the sync with the planner's refusal, and the rows
	// stand.
	nestor := demoNestor("true")
	nestor.invs = append(nestor.invs, labelInvocation(bundle.ExecutionBoard, "Nestor", "urgent"))
	s.catalogue["nestor"] = nestor
	failed, err := s.orch.SyncSubscriptions(ctx, testTenant, testIntegration)
	var refusal *forgetrigger.ProjectionError
	if !errors.Is(err, forge.ErrSubscriptionProjection) || !errors.As(err, &refusal) {
		t.Fatalf("sync error = %v, want forge.ErrSubscriptionProjection carrying the *ProjectionError", err)
	}
	if refusal.Code != forgetrigger.ProjectionOverlappingRows || refusal.Bot != "nestor" {
		t.Errorf("refusal = code %q bot %q, want overlapping_rows on nestor", refusal.Code, refusal.Bot)
	}
	if len(failed.Overlaps) != 1 {
		t.Errorf("the report beside the refusal names overlaps %q, want one", failed.Overlaps)
	}
	if after := storedRows(t, s.subs, testTenant); !reflect.DeepEqual(after, rows) {
		t.Errorf("a refused sync moved the rows:\n before %+v\n after  %+v", rows, after)
	}
}

func TestProjector_OverlapRefusesTheSwitchToSpine(t *testing.T) {
	nestor := demoNestor("true")
	nestor.invs = append(nestor.invs, labelInvocation(bundle.ExecutionBoard, "Nestor", "urgent"))
	s := newSeam(t, demoScope(), demoFeatureDev(), nestor, demoSaboteur())

	_, err := s.orch.SetForgeRouting(context.Background(), testTenant, testIntegration, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	var refused *forge.RoutingRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("switch error = %v, want a *forge.RoutingRefusedError", err)
	}
	if refused.Code != forge.RoutingRefusedOverlappingRows {
		t.Errorf("the switch is refused with code %q, want %q", refused.Code, forge.RoutingRefusedOverlappingRows)
	}
	var refusal *forgetrigger.ProjectionError
	if !errors.As(err, &refusal) || refusal.Bot != "nestor" {
		t.Errorf("the switch's refusal does not carry the planner's, naming nestor: %v", err)
	}
	if len(refused.Report.Overlaps) != 1 || !strings.Contains(refused.Reason, refused.Report.Overlaps[0]) {
		t.Errorf("the refusal's reason %q does not name the overlap %q", refused.Reason, refused.Report.Overlaps)
	}
	if rows := storedRows(t, s.subs, testTenant); len(rows) != 0 {
		t.Errorf("a refused projection stored %q", rowIDsOf(rows))
	}
	if mode := s.integration(t).ForgeRouting.For(webhooks.LaneIssues); mode == webhooks.RoutingSpine {
		t.Errorf("the lane routes by the spine after a refused switch")
	}
}

// The switch refuses the spine on the config launch vars no manifest
// declares, by its own computation; the report's HandPinned names the same
// keys, case by case.
func TestProjector_HandPinnedAgreesWithTheSwitchItInforms(t *testing.T) {
	withVars := func(id string, vars map[string]string) manifestBot {
		b := labelBot(id, bundle.ExecutionDirect, id)
		b.fr = &bundle.ForgeRequirements{Webhook: &bundle.ForgeWebhookHints{LaunchVars: vars}}
		return b
	}
	cases := []struct {
		name   string
		config map[string]string
		want   []string
	}{
		{name: "the union the manifests declare", config: map[string]string{"shared": "from-beta", "tone": "dry"}},
		{name: "a key pinned by hand", config: map[string]string{"shared": "from-beta", "tone": "dry", "pinned": "by-hand"}, want: []string{"pinned"}},
		{name: "a declared value edited by hand", config: map[string]string{"shared": "from-beta", "tone": "wet"}, want: []string{"tone"}},
		{name: "a declared key the config lost", config: map[string]string{"shared": "from-beta"}, want: []string{"tone"}},
		{name: "the value of the bot that sorts first", config: map[string]string{"shared": "from-alpha", "tone": "dry"}, want: []string{"shared"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := botScope("alpha", "beta")
			cfg.LaunchVars = tc.config
			s := newSeam(t, cfg,
				withVars("alpha", map[string]string{"shared": "from-alpha", "tone": "dry"}),
				withVars("beta", map[string]string{"shared": "from-beta"}),
			)
			result, err := s.orch.SetForgeRouting(context.Background(), testTenant, testIntegration, webhooks.LaneIssues, webhooks.RoutingSpine, false)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("the switch is refused: %v", err)
				}
				if len(result.Report.HandPinned) != 0 {
					t.Errorf("the switch passed and the report names hand pins %q", result.Report.HandPinned)
				}
				return
			}
			var refused *forge.RoutingRefusedError
			if !errors.As(err, &refused) || refused.Code != forge.RoutingRefusedHandPinnedLaunchVars {
				t.Fatalf("switch error = %v, want a refusal with code %q", err, forge.RoutingRefusedHandPinnedLaunchVars)
			}
			if !sameList(refused.Report.HandPinned, tc.want) {
				t.Errorf("report.hand_pinned = %q, want %q", refused.Report.HandPinned, tc.want)
			}
			if named := "key(s) [" + strings.Join(refused.Report.HandPinned, " ") + "]"; !strings.Contains(refused.Reason, named) {
				t.Errorf("the switch refuses on %q; the report's hand pins read %q", refused.Reason, named)
			}
		})
	}
}
