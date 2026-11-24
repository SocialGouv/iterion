package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The scheduled lane's shape, as the walk sees it (#3241): a cloud schedule's
// tick launches as "scheduler:<bot>" and stamps the repository it serves on
// the walk (pkg/server scheduledProjectPath). The team holds two gateway keys
// serving the bot, each restricted to that repository.
const (
	scheduledLaneBot   = "feed-watch"
	scheduledLaneOwner = "scheduler:feed-watch"
	scheduledLaneRepo  = "SocialGouv/iterion-veille"
)

func scheduledLaneWalk(t *testing.T, p *Publisher, runID, repo string, wf *ir.Workflow, pins map[string]string) (secrets.RunBundle, error) {
	t.Helper()
	ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), "team1"), repo)
	creds, err := p.resolveAndSealCredentials(ctx, runID, "", "team1", scheduledLaneOwner, scheduledLaneBot, "", wf, pins, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
	if err != nil {
		return secrets.RunBundle{}, err
	}
	return openGatewayBundle(t, p, runID, creds), nil
}

func scheduledLaneGatewayKeys(t *testing.T, p *Publisher, designRepos []string) (tech, design secrets.ApiKey) {
	t.Helper()
	tech = seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-studio-tech", func(k *secrets.ApiKey) {
		k.Bots = []string{scheduledLaneBot}
		k.Repos = []string{"socialgouv/iterion-veille"}
		k.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	}, "")
	design = seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-studio-design", func(k *secrets.ApiKey) {
		k.Bots = []string{scheduledLaneBot}
		k.Repos = designRepos
		k.CreatedAt = time.Now().UTC().Add(-1 * time.Hour)
	}, "")
	return tech, design
}

// TestScheduledLane_TheStampFundsARepoRestrictedKeyAndThePinPicksIt: without
// the repository stamp, keys restricted to the repository fund nothing;
// with it, the walk spends the oldest of them; a schedule's pin picks the
// other one — the per-schedule split of two gateway keys of one team.
func TestScheduledLane_TheStampFundsARepoRestrictedKeyAndThePinPicksIt(t *testing.T) {
	p, _ := gatewayKeyPublisher(t)
	tech, design := scheduledLaneGatewayKeys(t, p, []string{"socialgouv/iterion-veille"})
	for _, row := range []struct {
		name string
		repo string
		pins map[string]string
		want string
	}{
		{"no stamp, no pin", "", nil, ""},
		{"stamp, no pin", scheduledLaneRepo, nil, "sk-studio-tech"},
		{"stamp, pin design", scheduledLaneRepo, gatewayPin(design.ID), "sk-studio-design"},
		{"stamp, pin tech", scheduledLaneRepo, gatewayPin(tech.ID), "sk-studio-tech"},
	} {
		b, err := scheduledLaneWalk(t, p, "run-"+strings.ReplaceAll(row.name, " ", "-"), row.repo, gatewayOnlyWorkflow(), row.pins)
		if err != nil {
			t.Fatalf("%s: walk = %v", row.name, err)
		}
		if got := b.APIKeys[secrets.ProviderOpenAICompatible]; got != row.want {
			t.Errorf("%s: gateway slot = %q, want %q", row.name, got, row.want)
		}
	}
}

// TestScheduledLane_AGatewayPinOutsideItsRepositoryAudienceRefusesByName: a
// gateway pin is exclusive (ADR-131 §6): a pinned key whose repos audience
// leaves the stamped repository out refuses the launch by name — the team's
// other gateway key is never sealed in its place.
func TestScheduledLane_AGatewayPinOutsideItsRepositoryAudienceRefusesByName(t *testing.T) {
	p, _ := gatewayKeyPublisher(t)
	_, design := scheduledLaneGatewayKeys(t, p, []string{"other/repo"})
	b, err := scheduledLaneWalk(t, p, "run-withheld", scheduledLaneRepo, gatewayOnlyWorkflow(), gatewayPin(design.ID))
	var refusal *GatewayPinRefusedError
	if !errors.As(err, &refusal) || refusal.Cause != GatewayPinWithheld || refusal.KeyID != design.ID {
		t.Fatalf("walk = %v, want the withheld pin's *GatewayPinRefusedError naming %s", err, design.ID)
	}
	if got := b.APIKeys[secrets.ProviderOpenAICompatible]; got != "" {
		t.Errorf("gateway slot = %q after the refusal, want none", got)
	}
}

// TestScheduledLane_TheWalkReplacesAVendorPinItsAudienceWithholds: the walk
// withholds a pinned VENDOR key whose bots audience leaves the bot out and
// spends the team's next key, with a warn line — its warn-only contract.
// This is why the schedule's tick judges its own pins' audiences before the
// launch (pkg/server judgeScheduleKeyPins) and refuses by name instead.
func TestScheduledLane_TheWalkReplacesAVendorPinItsAudienceWithholds(t *testing.T) {
	p, logs := gatewayKeyPublisher(t)
	seedPinKey(t, p, secrets.ProviderAnthropic, "sk-ant-OTHER", func(k *secrets.ApiKey) {
		k.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	}, "")
	pinned := seedPinKey(t, p, secrets.ProviderAnthropic, "sk-ant-PINNED", func(k *secrets.ApiKey) {
		k.Bots = []string{"some-other-bot"}
	}, "")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"digest": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "digest"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}},
	}}
	b, err := scheduledLaneWalk(t, p, "run-vendor", scheduledLaneRepo, wf, map[string]string{"anthropic": pinned.ID})
	if err != nil {
		t.Fatalf("walk = %v", err)
	}
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-OTHER" {
		t.Fatalf("anthropic slot = %q, want the team's other key — the walk's warn-only contract for a withheld vendor pin", got)
	}
	if !strings.Contains(logs.String(), pinned.ID) {
		t.Errorf("no log line names the withheld pin %s: %s", pinned.ID, logs.String())
	}
}
