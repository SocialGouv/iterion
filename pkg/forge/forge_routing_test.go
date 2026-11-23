package forge

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// ---- SetForgeRouting ----

func refusalOf(t *testing.T, err error) *RoutingRefusedError {
	t.Helper()
	var refused *RoutingRefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrRoutingRefused) {
		t.Fatalf("err = %v (%T), want a *RoutingRefusedError in the ErrRoutingRefused class", err, err)
	}
	return refused
}

// assertNotSwitched holds a refused switch to having written NOTHING: neither
// document's routing, no spine instant, and no version advance — the fence it
// ran in rolled back.
func assertNotSwitched(t *testing.T, s *spineOrch, res ProvisionResult, before RepoIntegration) {
	t.Helper()
	ri, cfg := s.stored(t, res)
	if !ri.ForgeRouting.IsZero() || ri.SpineSince != nil {
		t.Errorf("a refused switch wrote the integration: routing %+v, spine_since %v", ri.ForgeRouting, ri.SpineSince)
	}
	if !cfg.ForgeRouting.IsZero() || cfg.SpineSince != nil {
		t.Errorf("a refused switch wrote the webhook config: routing %+v, spine_since %v", cfg.ForgeRouting, cfg.SpineSince)
	}
	if ri.Version != before.Version {
		t.Errorf("a refused switch moved the integration's version %d → %d", before.Version, ri.Version)
	}
}

// routingSwitchStory is the life of one switch, run on both store twins: a
// refused flip writes nothing, a flip writes both documents in one fence, a
// second lane keeps the first, a rollback keeps the spine instant, and a
// second flip does not move it.
func routingSwitchStory(t *testing.T, s *spineOrch) {
	ctx := context.Background()
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	before, _ := s.stored(t, res)
	if before.SubscriptionsSyncedAt == nil {
		t.Fatal("precondition: the provision did not stamp its subscription sync")
	}

	// Refused: the rows overlap. The projection's own stamp goes with it.
	s.hour()
	s.projector.setRefuse(func(SubscriptionInput) (SubscriptionReport, error) {
		return SubscriptionReport{Overlaps: []string{"nestor: a ∩ b"}}, errRowsOverlap
	})
	_, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if got := refusalOf(t, err).Code; got != RoutingRefusedOverlappingRows {
		t.Fatalf("code = %q, want %q", got, RoutingRefusedOverlappingRows)
	}
	ri, cfg := s.stored(t, res)
	if !ri.ForgeRouting.IsZero() || ri.SpineSince != nil || !cfg.ForgeRouting.IsZero() || cfg.SpineSince != nil || ri.Version != before.Version ||
		ri.SubscriptionsSyncError != "" || !sameInstantPtr(ri.SubscriptionsSyncedAt, *before.SubscriptionsSyncedAt) {
		t.Fatalf("a refused switch left something behind: integration %+v / %v, version %d → %d, sync error %q at %v; config %+v / %v",
			ri.ForgeRouting, ri.SpineSince, before.Version, ri.Version, ri.SubscriptionsSyncError, ri.SubscriptionsSyncedAt, cfg.ForgeRouting, cfg.SpineSince)
	}
	s.projector.setRefuse(nil)

	// The flip.
	first := s.hour()
	s.probe.take()
	out, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if err != nil {
		t.Fatal(err)
	}
	fences := s.probe.take()
	if len(fences) != 1 || fences[0].err != nil {
		t.Fatalf("the flip opened fences %+v, want exactly one, successful", fences)
	}
	ri, cfg = s.stored(t, res)
	spine := webhooks.ForgeRouting{Issues: webhooks.RoutingSpine}
	if out.Mode != webhooks.RoutingSpine || ri.ForgeRouting != spine || cfg.ForgeRouting != spine ||
		!sameInstantPtr(ri.SpineSince, first) || !sameInstantPtr(cfg.SpineSince, first) || ri.Version != fences[0].version+1 ||
		!sameInstantPtr(ri.SubscriptionsSyncedAt, first) {
		t.Fatalf("after the flip: result %q, integration %+v since %v (version %d, fence opened at %d, synced %v), config %+v since %v; want spine since %v on both",
			out.Mode, ri.ForgeRouting, ri.SpineSince, ri.Version, fences[0].version, ri.SubscriptionsSyncedAt, cfg.ForgeRouting, cfg.SpineSince, first)
	}

	// A second lane keeps the first.
	s.hour()
	if _, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneComments, webhooks.RoutingShadow, false); err != nil {
		t.Fatal(err)
	}
	both := webhooks.ForgeRouting{Issues: webhooks.RoutingSpine, Comments: webhooks.RoutingShadow}
	if ri, cfg = s.stored(t, res); ri.ForgeRouting != both || cfg.ForgeRouting != both {
		t.Fatalf("after a second lane: integration %+v, config %+v; want %+v on both", ri.ForgeRouting, cfg.ForgeRouting, both)
	}

	// The rollback keeps the spine instant; a second flip does not move it.
	s.hour()
	out, err = s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingLegacy, false)
	if err != nil {
		t.Fatal(err)
	}
	ri, cfg = s.stored(t, res)
	if out.Mode != webhooks.RoutingLegacy || ri.ForgeRouting.Issues != webhooks.RoutingLegacy || cfg.ForgeRouting.Issues != webhooks.RoutingLegacy ||
		!sameInstantPtr(ri.SpineSince, first) || !sameInstantPtr(cfg.SpineSince, first) {
		t.Fatalf("after the rollback: result %q, integration %+v since %v, config %+v since %v; want legacy, since %v kept",
			out.Mode, ri.ForgeRouting, ri.SpineSince, cfg.ForgeRouting, cfg.SpineSince, first)
	}
	s.hour()
	if _, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false); err != nil {
		t.Fatal(err)
	}
	if ri, cfg = s.stored(t, res); ri.ForgeRouting != both || cfg.ForgeRouting != both || !sameInstantPtr(ri.SpineSince, first) || !sameInstantPtr(cfg.SpineSince, first) {
		t.Fatalf("after a second flip: integration %+v since %v, config %+v since %v; want %+v since %v", ri.ForgeRouting, ri.SpineSince, cfg.ForgeRouting, cfg.SpineSince, both, first)
	}
}

func TestSetForgeRouting_Story(t *testing.T) { routingSwitchStory(t, newSpineOrch(t)) }

// On Mongo the fence is a transaction the config write joins, and the
// integration's half is a pipeline update: neither is proven by the memory
// twin.
func TestSetForgeRouting_Story_Mongo(t *testing.T) { routingSwitchStory(t, newMongoSpineOrch(t)) }

// A lane with no enabled row would filter every delivery: the switch refuses
// it. force lifts no refusal of the switch, so every refusal below is asked
// both ways.
func TestSetForgeRouting_RefusesSpineWithoutEnabledRows(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			s := newSpineOrch(t)
			off := false
			res := s.provision(t, ProvisionRequest{
				BotIDs:      []string{"nestor"},
				BotTriggers: map[string]BotTrigger{"nestor": {Enabled: &off}},
			})
			before, _ := s.stored(t, res)
			_, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, force)
			refused := refusalOf(t, err)
			if refused.Code != RoutingRefusedNoEnabledRow {
				t.Fatalf("code = %q, want %q (%v)", refused.Code, RoutingRefusedNoEnabledRow, err)
			}
			if !strings.Contains(refused.Reason, "nestor") {
				t.Errorf("the refusal does not say which bot is off and why: %q", refused.Reason)
			}
			assertNotSwitched(t, s, res, before)

			// Another lane's rows do not count for this one.
			_, err = s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LanePullRequests, webhooks.RoutingSpine, force)
			if got := refusalOf(t, err).Code; got != RoutingRefusedNoEnabledRow {
				t.Fatalf("pull_requests lane: code = %q, want %q", got, RoutingRefusedNoEnabledRow)
			}
		})
	}
}

// A server that projects nothing cannot route by a projection.
func TestSetForgeRouting_RefusesSpineWithoutAProjector(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	before, _ := s.stored(t, res)
	s.Subscriptions = nil
	s.probe.take()
	_, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, true)
	if got := refusalOf(t, err).Code; got != RoutingRefusedNoProjector {
		t.Fatalf("code = %q, want %q", got, RoutingRefusedNoProjector)
	}
	assertNotSwitched(t, s, res, before)

	// Going BACK needs no projector: a rollback must not depend on the thing
	// that may be the reason for it.
	out, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingLegacy, false)
	if err != nil || out.Mode != webhooks.RoutingLegacy {
		t.Fatalf("legacy without a projector: mode %q, err %v", out.Mode, err)
	}
}

// One bot must claim an event through one row. The projector names the rows
// that may both match; the switch refuses with that list — whether the
// projector also failed (the real one does) or only reported it.
func TestSetForgeRouting_RefusesOverlappingRows(t *testing.T) {
	overlap := "nestor: forge:issue_labeled|a:|l:aaaa ∩ forge:issue_labeled|a:|l:bbbb"
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "the projector fails the projection", err: errRowsOverlap},
		{name: "the projector only reports the overlap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpineOrch(t)
			res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
			before, _ := s.stored(t, res)
			s.projector.setRefuse(func(SubscriptionInput) (SubscriptionReport, error) {
				return SubscriptionReport{
					Enabled:  map[string]map[string][]string{webhooks.LaneIssues: {"nestor": {"nestor"}}},
					Overlaps: []string{overlap},
				}, tc.err
			})
			_, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, true)
			refused := refusalOf(t, err)
			if refused.Code != RoutingRefusedOverlappingRows {
				t.Fatalf("code = %q, want %q (%v)", refused.Code, RoutingRefusedOverlappingRows, err)
			}
			if !strings.Contains(refused.Reason, overlap) {
				t.Errorf("the refusal does not name the rows: %q", refused.Reason)
			}
			if !slices.Equal(refused.Report.Overlaps, []string{overlap}) {
				t.Errorf("the refusal does not carry the report: %+v", refused.Report)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("the projector's own error is not reachable through the refusal: %v", err)
			}
			// Nothing is written, the refused projection's stamp included.
			ri, cfg := s.stored(t, res)
			if !ri.ForgeRouting.IsZero() || !cfg.ForgeRouting.IsZero() || ri.Version != before.Version || ri.SubscriptionsSyncError != "" {
				t.Errorf("a refused switch wrote: integration routing %+v, version %d → %d, sync error %q; config routing %+v",
					ri.ForgeRouting, before.Version, ri.Version, ri.SubscriptionsSyncError, cfg.ForgeRouting)
			}
		})
	}
}

// Any other refusal of the projection refuses the switch too, under its own
// code: the lane would route by rows nobody could write. The second row is the
// refusal standing alone — a report that names enabled rows, so no other check
// of the switch would have stopped it.
func TestSetForgeRouting_RefusesSpineWhenTheProjectionIsRefused(t *testing.T) {
	storeDown := errors.New("trigger store: connection reset")
	for _, tc := range []struct {
		name   string
		report SubscriptionReport
	}{
		{name: "an empty report"},
		{name: "a report that names enabled rows", report: SubscriptionReport{
			Enabled: map[string]map[string][]string{webhooks.LaneIssues: {"nestor": {"nestor"}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpineOrch(t)
			res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
			before, _ := s.stored(t, res)
			s.projector.setRefuse(func(SubscriptionInput) (SubscriptionReport, error) { return tc.report, storeDown })
			_, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
			refused := refusalOf(t, err)
			if refused.Code != RoutingRefusedProjection || !errors.Is(err, storeDown) || !errors.Is(err, ErrSubscriptionProjection) {
				t.Fatalf("code = %q, err = %v; want %q wrapping the projector's error", refused.Code, err, RoutingRefusedProjection)
			}
			assertNotSwitched(t, s, res, before)
			if ri, _ := s.stored(t, res); ri.SubscriptionsSyncError != "" {
				t.Fatalf("the refused switch kept the refusal's stamp (%q): its fence did not roll back", ri.SubscriptionsSyncError)
			}
		})
	}
}

// The webhook config's launch_vars is the union of the bots' manifest vars —
// and whatever a team admin PATCHed onto it by hand. A spine launch layers
// each bot's own vars and never reads that map, so a hand pin in it would
// stop applying without a word. The switch refuses until the pin lives on the
// integration's launch_vars, the operator layer a spine launch does apply.
func TestSetForgeRouting_RefusesHandPinnedLaunchVars(t *testing.T) {
	ctx := context.Background()
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "feature-dev"}})
	_, cfg := s.stored(t, res)
	if !maps.Equal(cfg.LaunchVars, map[string]string{"open_mr": "true"}) {
		t.Fatalf("precondition: the config's launch vars are the manifest union, got %v", cfg.LaunchVars)
	}
	// The webhook PATCH: one key no manifest declares, one manifest key
	// overridden.
	cfg.LaunchVars = map[string]string{"open_mr": "false", "gate_context": "revi/review"}
	if err := s.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := s.stored(t, res)

	for _, force := range []bool{false, true} {
		_, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, force)
		refused := refusalOf(t, err)
		if refused.Code != RoutingRefusedHandPinnedLaunchVars {
			t.Fatalf("force=%v: code = %q, want %q (%v)", force, refused.Code, RoutingRefusedHandPinnedLaunchVars, err)
		}
		for _, want := range []string{"gate_context", "open_mr", "integration's launch_vars"} {
			if !strings.Contains(refused.Reason, want) {
				t.Errorf("force=%v: the refusal does not name %q: %q", force, want, refused.Reason)
			}
		}
	}
	ri, cfg := s.stored(t, res)
	if !ri.ForgeRouting.IsZero() || !cfg.ForgeRouting.IsZero() || ri.Version != before.Version {
		t.Fatalf("a refused switch wrote: integration %+v (version %d → %d), config %+v", ri.ForgeRouting, before.Version, ri.Version, cfg.ForgeRouting)
	}
	if cfg.LaunchVars["gate_context"] != "revi/review" {
		t.Fatalf("the refusal dropped the hand pin it refused over: %v", cfg.LaunchVars)
	}

	// The remedy the refusal names: pin the keys on the integration. That
	// provision rewrites the config's copy from the manifests, and the switch
	// then passes with the pin held by the operator layer.
	s.projector.takeSyncs()
	s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "feature-dev"}, LaunchVars: map[string]string{"open_mr": "false", "gate_context": "revi/review"}})
	// That provision's own projection reads the config as it has just been
	// rewritten, not the one read before the write: a projector told the
	// stale map would report hand pins that are gone.
	syncs, _ := s.projector.takeSyncs()
	if len(syncs) != 1 {
		t.Fatalf("the remedying provision projected %d time(s), want once", len(syncs))
	}
	if got := syncs[0].Config.LaunchVars; !maps.Equal(got, map[string]string{"open_mr": "true"}) {
		t.Fatalf("the projection of the remedying provision read launch vars %v, want the manifest union the provision wrote", got)
	}
	out, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if err != nil || out.Mode != webhooks.RoutingSpine {
		t.Fatalf("after moving the pins to the integration: mode %q, err %v", out.Mode, err)
	}
	ri, cfg = s.stored(t, res)
	if cfg.OperatorLaunchVars["gate_context"] != "revi/review" || ri.LaunchVars["open_mr"] != "false" {
		t.Fatalf("the moved pins are not on the operator layer: integration %v, config operator vars %v", ri.LaunchVars, cfg.OperatorLaunchVars)
	}
}

// The first switch to spine stamps SpineSince on both documents; nothing
// moves it afterwards. A rollback keeps it — legacy keeps reading the claims
// the spine made — and the mode it answers is the one the stored config
// holds.
func TestSetForgeRouting_StampsSpineSinceOnce(t *testing.T) {
	ctx := context.Background()
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})

	first := s.hour()
	s.probe.take()
	s.projector.takeSyncs()
	out, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != webhooks.RoutingSpine || out.Lane != webhooks.LaneIssues || out.IntegrationID != res.IntegrationID || out.WebhookID != res.WebhookID {
		t.Fatalf("result = %+v", out)
	}
	if !sameInstantPtr(out.SpineSince, first) || !slices.Equal(out.Report.Enabled[webhooks.LaneIssues]["nestor"], []string{"nestor"}) {
		t.Fatalf("result: spine_since %v (want %v), report %+v", out.SpineSince, first, out.Report)
	}
	// One fence: the projection, its stamp and the switch commit together.
	fences := s.probe.take()
	if len(fences) != 1 || fences[0].retired || fences[0].err != nil {
		t.Fatalf("the switch opened fences %+v, want exactly one, successful", fences)
	}
	if syncs, inFence := s.projector.takeSyncs(); len(syncs) != 1 || !inFence[0] {
		t.Fatalf("%d projection(s), in a fence: %v; want one, inside the fence", len(syncs), inFence)
	}
	ri, cfg := s.stored(t, res)
	if ri.Version != fences[0].version+1 {
		t.Fatalf("version %d after a fence opened at %d", ri.Version, fences[0].version)
	}
	if ri.ForgeRouting.Issues != webhooks.RoutingSpine || cfg.ForgeRouting.Issues != webhooks.RoutingSpine {
		t.Fatalf("routing: integration %+v, config %+v", ri.ForgeRouting, cfg.ForgeRouting)
	}
	if !sameInstantPtr(ri.SpineSince, first) || !sameInstantPtr(cfg.SpineSince, first) {
		t.Fatalf("spine_since: integration %v, config %v; want both at the first flip %v", ri.SpineSince, cfg.SpineSince, first)
	}
	if !sameInstantPtr(ri.SubscriptionsSyncedAt, first) {
		t.Fatalf("the switch's projection was not stamped: synced %v, want %v", ri.SubscriptionsSyncedAt, first)
	}

	// The rollback.
	s.hour()
	out, err = s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingLegacy, false)
	if err != nil {
		t.Fatal(err)
	}
	ri, cfg = s.stored(t, res)
	if out.Mode != webhooks.RoutingLegacy || ri.ForgeRouting.Issues != webhooks.RoutingLegacy || cfg.ForgeRouting.For(webhooks.LaneIssues) != webhooks.RoutingLegacy {
		t.Fatalf("rollback: result %q, integration %+v, config %+v; want legacy everywhere", out.Mode, ri.ForgeRouting, cfg.ForgeRouting)
	}
	if !sameInstantPtr(ri.SpineSince, first) || !sameInstantPtr(cfg.SpineSince, first) || !sameInstantPtr(out.SpineSince, first) {
		t.Fatalf("rollback: spine_since integration %v, config %v, result %v; want all kept at %v", ri.SpineSince, cfg.SpineSince, out.SpineSince, first)
	}

	// A second switch to spine, later: the instant is the first one still.
	s.hour()
	if _, err = s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false); err != nil {
		t.Fatal(err)
	}
	ri, cfg = s.stored(t, res)
	if !sameInstantPtr(ri.SpineSince, first) || !sameInstantPtr(cfg.SpineSince, first) {
		t.Fatalf("second flip: spine_since integration %v, config %v; want both kept at %v", ri.SpineSince, cfg.SpineSince, first)
	}
}

// A rollback is never held back by the projection: when the rows are the
// problem, going back to legacy is the way out. The refusal is recorded and
// warned, and the lane is switched.
func TestSetForgeRouting_RollbackToLegacyIsNotHeldByARefusedProjection(t *testing.T) {
	ctx := context.Background()
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	flipped := s.now()
	s.flipToSpine(t, res)
	s.hour()
	s.projector.setRefuse(func(SubscriptionInput) (SubscriptionReport, error) {
		return SubscriptionReport{Overlaps: []string{"nestor: a ∩ b"}}, errRowsOverlap
	})
	out, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingLegacy, false)
	if err != nil {
		t.Fatalf("a rollback to legacy was refused over the projection: %v", err)
	}
	ri, cfg := s.stored(t, res)
	if out.Mode != webhooks.RoutingLegacy || cfg.ForgeRouting.Issues != webhooks.RoutingLegacy || ri.ForgeRouting.Issues != webhooks.RoutingLegacy {
		t.Fatalf("mode: result %q, config %+v, integration %+v; want legacy", out.Mode, cfg.ForgeRouting, ri.ForgeRouting)
	}
	if !sameInstantPtr(cfg.SpineSince, flipped) || !sameInstantPtr(ri.SpineSince, flipped) {
		t.Fatalf("spine_since: config %v, integration %v; want kept at %v", cfg.SpineSince, ri.SpineSince, flipped)
	}
	if !strings.Contains(ri.SubscriptionsSyncError, errRowsOverlap.Error()) {
		t.Fatalf("the refused projection is not recorded: %q", ri.SubscriptionsSyncError)
	}
	if s.warnings.containing(errRowsOverlap.Error()) != 1 {
		t.Fatal("the refused projection was not warned")
	}
}

// The config is written from the INTEGRATION's routing, which owns the switch:
// a lane set earlier stays, and the answer is what the config holds after the
// write, not what was asked.
func TestSetForgeRouting_WritesTheWholeSwitchAndAnswersFromTheStoredConfig(t *testing.T) {
	ctx := context.Background()
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	if _, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneComments, webhooks.RoutingShadow, false); err != nil {
		t.Fatal(err)
	}
	// The config's copy drifts from the integration's — a writer that held an
	// older document. The next switch converges it: it writes the
	// integration's routing with the lane set, never the config's own copy.
	_, drifted := s.stored(t, res)
	drifted.ForgeRouting = webhooks.ForgeRouting{PullRequests: webhooks.RoutingLegacy}
	if err := s.Webhooks.Update(ctx, drifted); err != nil {
		t.Fatal(err)
	}
	out := s.flipToSpine(t, res)
	want := webhooks.ForgeRouting{Issues: webhooks.RoutingSpine, Comments: webhooks.RoutingShadow}
	ri, cfg := s.stored(t, res)
	if ri.ForgeRouting != want || cfg.ForgeRouting != want || out.Routing != want {
		t.Fatalf("after two switches: integration %+v, config %+v, result %+v; want %+v on all three", ri.ForgeRouting, cfg.ForgeRouting, out.Routing, want)
	}

	// A config store that takes the write and holds something else: the
	// answer is the stored mode, so a switch that did not take is never
	// reported as taken.
	s.Webhooks = &lyingConfigs{ConfigStore: s.Webhooks, tamper: func(c *webhooks.Config) { c.ForgeRouting.Issues = webhooks.RoutingLegacy }}
	out, err := s.SetForgeRouting(ctx, "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != webhooks.RoutingLegacy {
		t.Fatalf("the switch answered %q while the stored config holds legacy: it reports what was asked, not what is in force", out.Mode)
	}
}

// A failed config write undoes the integration's half: the two documents never
// disagree about a switch the caller was told failed.
func TestSetForgeRouting_ConfigWriteFailureLeavesTheIntegrationUnswitched(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	before, _ := s.stored(t, res)
	down := errors.New("config store down")
	s.Webhooks = &failingConfigUpdates{ConfigStore: s.Webhooks, err: down}
	_, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if !errors.Is(err, down) {
		t.Fatalf("err = %v, want the config store's failure", err)
	}
	ri, _ := s.stored(t, res)
	if !ri.ForgeRouting.IsZero() || ri.SpineSince != nil || ri.Version != before.Version {
		t.Fatalf("the integration kept the switch its config never took: routing %+v, spine_since %v, version %d → %d", ri.ForgeRouting, ri.SpineSince, before.Version, ri.Version)
	}
}

// failingStamps fails the sync stamp the way a store outage would.
type failingStamps struct {
	RepoIntegrationStore
	err error
}

func (f *failingStamps) StampSubscriptionSync(context.Context, string, map[string]BotTrigger, time.Time, string) error {
	return f.err
}

// A projection that succeeded and a stamp the store could not write is a
// FAILURE of the switch, not a refusal of it: the caller retries, it has
// nothing to change. Typing it as a refusal would answer "your rows are
// wrong" for an outage.
func TestSetForgeRouting_AStoreFailureIsNotARefusal(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	before, _ := s.stored(t, res)
	down := errors.New("integration store down")
	s.Integrations = &failingStamps{RepoIntegrationStore: s.Integrations, err: down}
	_, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if !errors.Is(err, down) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if errors.Is(err, ErrRoutingRefused) {
		t.Fatalf("a store failure was typed as a refusal of the switch: %v", err)
	}
	assertNotSwitched(t, s, res, before)
}

type failingConfigUpdates struct {
	webhooks.ConfigStore
	err error
}

func (f *failingConfigUpdates) Update(context.Context, webhooks.Config) error { return f.err }

func TestSetForgeRouting_UnknownLaneOrModeIsTyped(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	before, _ := s.stored(t, res)
	for _, tc := range []struct{ lane, mode, field, value string }{
		{lane: "branches", mode: webhooks.RoutingSpine, field: "lane", value: "branches"},
		{lane: "", mode: webhooks.RoutingLegacy, field: "lane", value: ""},
		{lane: webhooks.LaneIssues, mode: "canary", field: "mode", value: "canary"},
		// The empty mode is "never chosen": it is not writable, a lane goes
		// back by naming legacy.
		{lane: webhooks.LaneIssues, mode: "", field: "mode", value: ""},
	} {
		_, err := s.SetForgeRouting(context.Background(), "t1", res.IntegrationID, tc.lane, tc.mode, true)
		var invalid *RoutingInvalidError
		if !errors.As(err, &invalid) || !errors.Is(err, ErrRoutingInvalid) {
			t.Fatalf("lane %q mode %q: err = %v (%T), want a *RoutingInvalidError", tc.lane, tc.mode, err, err)
		}
		if invalid.Field != tc.field || invalid.Value != tc.value || len(invalid.Allowed) == 0 {
			t.Errorf("lane %q mode %q: refusal = %+v, want field %q value %q and the allowed values", tc.lane, tc.mode, invalid, tc.field, tc.value)
		}
	}
	assertNotSwitched(t, s, res, before)
}

func TestSetForgeRouting_AnotherTenantsIntegrationIsNotFound(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	_, err := s.SetForgeRouting(context.Background(), "t2", res.IntegrationID, webhooks.LaneIssues, webhooks.RoutingSpine, false)
	if !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("err = %v, want ErrIntegrationNotFound", err)
	}
	if _, err := s.SyncSubscriptions(context.Background(), "t2", res.IntegrationID); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("sync: err = %v, want ErrIntegrationNotFound", err)
	}
}

// ---- SyncSubscriptions ----

// The on-demand sync reads the bots through the lookups Provision uses, and
// runs the projection and its stamp in one fence.
func TestSyncSubscriptions_ProjectsAndStampsInOneFence(t *testing.T) {
	ctx := context.Background()
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor", "feature-dev"}})
	provisioned, _ := s.projector.takeSyncs()
	if len(provisioned) != 1 {
		t.Fatalf("precondition: the provision projected %d time(s), want once", len(provisioned))
	}
	before, _ := s.stored(t, res)
	s.probe.take()

	// The catalogue moves between the provision and the sync: nestor's
	// manifest names another label. The sync must read it as it is NOW.
	s.Invocations = func(ctx context.Context, team, bot string) ([]bundle.Invocation, error) {
		invs, err := spineInvocations(ctx, team, bot)
		if bot == "nestor" && err == nil {
			invs = slices.Clone(invs)
			forge := *invs[0].Forge
			forge.Labels = []string{"butler"}
			invs[0].Forge = &forge
		}
		return invs, err
	}
	at := s.hour()
	report, err := s.SyncSubscriptions(ctx, "t1", res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Enabled[webhooks.LaneIssues]["nestor"], []string{"butler"}) {
		t.Fatalf("the report does not follow the catalogue: %+v", report.Enabled)
	}
	if got := s.projector.rowLabels(res.IntegrationID, "nestor"); !slices.Equal(got, []string{"butler"}) {
		t.Fatalf("nestor's row claims %v after the sync", got)
	}
	fences := s.probe.take()
	if len(fences) != 1 || fences[0].retired || fences[0].err != nil {
		t.Fatalf("the sync opened fences %+v, want exactly one, successful", fences)
	}
	syncs, inFence := s.projector.takeSyncs()
	if len(syncs) != 1 || !inFence[0] {
		t.Fatalf("%d projection(s), in a fence: %v; want one, inside the fence", len(syncs), inFence)
	}
	// The same input shape a provision hands the projector.
	if got, want := syncs[0], provisioned[0]; got.Integration.ID != want.Integration.ID || got.Config.ID != want.Config.ID ||
		!slices.Equal(got.Bots, want.Bots) || got.FrByBot["feature-dev"] == nil || len(got.InvByBot["nestor"]) != len(want.InvByBot["nestor"]) {
		t.Fatalf("the sync's projection input differs from the provision's: %+v vs %+v", got, want)
	}
	ri, _ := s.stored(t, res)
	if ri.Version != before.Version+1 || !sameInstantPtr(ri.SubscriptionsSyncedAt, at) || ri.SubscriptionsSyncError != "" {
		t.Fatalf("after the sync: version %d → %d, synced %v (want %v), error %q", before.Version, ri.Version, ri.SubscriptionsSyncedAt, at, ri.SubscriptionsSyncError)
	}
}

// A refused on-demand sync is always returned to its caller, with the report
// that names what refused it. What is stored follows the routing: recorded on
// an integration no lane of which routes by the rows, rolled back on one that
// does.
func TestSyncSubscriptions_ARefusedProjectionIsReturned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spine bool
	}{
		{name: "legacy: recorded on the integration"},
		{name: "spine: the fence rolls back", spine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpineOrch(t)
			res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
			if tc.spine {
				s.flipToSpine(t, res)
			}
			before, _ := s.stored(t, res)
			s.hour()
			s.projector.setRefuse(func(SubscriptionInput) (SubscriptionReport, error) {
				return SubscriptionReport{Overlaps: []string{"nestor: a ∩ b"}}, errRowsOverlap
			})
			report, err := s.SyncSubscriptions(context.Background(), "t1", res.IntegrationID)
			if !errors.Is(err, ErrSubscriptionProjection) || !errors.Is(err, errRowsOverlap) {
				t.Fatalf("err = %v, want ErrSubscriptionProjection wrapping the projector's refusal", err)
			}
			if !slices.Equal(report.Overlaps, []string{"nestor: a ∩ b"}) {
				t.Fatalf("the report that names the refusal was dropped: %+v", report)
			}
			ri, _ := s.stored(t, res)
			if tc.spine {
				if ri.SubscriptionsSyncError != "" || ri.Version != before.Version {
					t.Fatalf("spine: the failed fence kept something: error %q, version %d → %d", ri.SubscriptionsSyncError, before.Version, ri.Version)
				}
				return
			}
			if !strings.Contains(ri.SubscriptionsSyncError, errRowsOverlap.Error()) || ri.Version != before.Version+1 {
				t.Fatalf("legacy: the refusal is not recorded: error %q, version %d → %d", ri.SubscriptionsSyncError, before.Version, ri.Version)
			}
		})
	}
}

func TestSyncSubscriptions_WithoutAProjectorIsAnError(t *testing.T) {
	s := newSpineOrch(t)
	res := s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}})
	s.Subscriptions = nil
	if _, err := s.SyncSubscriptions(context.Background(), "t1", res.IntegrationID); !errors.Is(err, ErrSubscriptionProjection) {
		t.Fatalf("err = %v, want ErrSubscriptionProjection: a sync that projects nothing must not answer success", err)
	}
}

// ---- every config writer of the orchestrator keeps the switch ----

// routingWatch reads every Config the orchestrator hands the store — an Update
// or a Create — and notes each one that does not carry the routing switch the
// watched repo holds. The config store itself keeps the switch against a
// writer that says nothing; this asks more of the orchestrator: that what it
// writes says it. Fed from the provisioning goroutine only, but locked like
// any observer.
type routingWatch struct {
	webhooks.ConfigStore
	want  webhooks.ForgeRouting
	since time.Time

	mu      sync.Mutex
	writes  []string
	dropped []string
}

func (w *routingWatch) note(op string, c webhooks.Config) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, op+" "+c.ID)
	if c.ForgeRouting != w.want || !sameInstantPtr(c.SpineSince, w.since) {
		w.dropped = append(w.dropped, fmt.Sprintf("%s %s wrote routing %+v, spine_since %v", op, c.ID, c.ForgeRouting, c.SpineSince))
	}
}

func (w *routingWatch) Update(ctx context.Context, c webhooks.Config) error {
	w.note("update", c)
	return w.ConfigStore.Update(ctx, c)
}

func (w *routingWatch) Create(ctx context.Context, c webhooks.Config) error {
	w.note("create", c)
	return w.ConfigStore.Create(ctx, c)
}

func (w *routingWatch) seen() (writes, dropped []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.writes), slices.Clone(w.dropped)
}

// TestOrchestratorConfigWrites_KeepTheRoutingSwitch drives every site of this
// package that writes a webhook config, on a repo whose issue lane routes by
// the spine, and holds each to leaving ForgeRouting and SpineSince as they
// were — in the store, and in the document it wrote. A flip that the next
// `bots enable`, a failed enable's undo, or a credential sweep can revert is a
// repo silently back on the single-winner election.
//
// The sites, from a grep of Webhooks.Update / Webhooks.Create in the package:
// the short-circuit's operator-settings write, the rebuild's write, the undo
// of a failed rebuild (restoreConfig), the bot-rules backfill, Deprovision's
// authority binding of a legacy config, the freeze of the config a migration
// leaves (reserveHookIdentity) with the Create of the one it moves to, and the
// pin reconcile after a migration (reconcileStoredPins). Each row names the
// evidence that its site ran.
func TestOrchestratorConfigWrites_KeepTheRoutingSwitch(t *testing.T) {
	ctx := context.Background()
	type scene struct {
		s       *spineOrch
		res     ProvisionResult
		flipped time.Time
		watch   *routingWatch
	}
	// spineRouted provisions bots, flips the issue lane, and starts watching.
	spineRouted := func(t *testing.T, s *spineOrch, res ProvisionResult) *scene {
		t.Helper()
		sc := &scene{s: s, res: res, flipped: s.hour()}
		s.flipToSpine(t, res)
		s.hour()
		sc.watch = &routingWatch{ConfigStore: s.Webhooks, want: webhooks.ForgeRouting{Issues: webhooks.RoutingSpine}, since: sc.flipped}
		s.Webhooks = sc.watch
		return sc
	}
	fresh := func(t *testing.T) *scene {
		t.Helper()
		s := newSpineOrch(t)
		return spineRouted(t, s, s.provision(t, ProvisionRequest{BotIDs: []string{"nestor"}}))
	}
	editConfig := func(t *testing.T, sc *scene, edit func(*webhooks.Config)) {
		t.Helper()
		cfg, err := sc.watch.Get(ctx, sc.res.WebhookID)
		if err != nil {
			t.Fatal(err)
		}
		edit(&cfg)
		// Through the store under the watch: this edit is the row's setup,
		// not a write of the orchestrator.
		if err := sc.watch.ConfigStore.Update(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	reprovision := func(sc *scene, bots ...string) (ProvisionResult, error) {
		return sc.s.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: bots, ActorID: "u1"})
	}

	for _, tc := range []struct {
		name string
		// arrange builds the spine-routed repo; act runs the site and returns
		// the ids of the configs that must still hold the switch.
		arrange func(t *testing.T) *scene
		act     func(t *testing.T, sc *scene) []string
		// wrote is the config write the site makes, as the watch names it
		// ("update <id>" / "create <id>"); %s is the provisioned webhook id.
		wrote string
	}{
		{
			name:    "the short-circuit's operator-settings write",
			arrange: fresh,
			act: func(t *testing.T, sc *scene) []string {
				res, err := sc.s.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"nestor"}, ActorID: "u1", HoldLabels: []string{"hold"}})
				if err != nil || res.Created {
					t.Fatalf("short-circuit: created %v, err %v", res.Created, err)
				}
				if _, cfg := sc.s.stored(t, sc.res); !slices.Equal(cfg.HoldLabels, []string{"hold"}) {
					t.Fatalf("the site did not run: hold labels %v", cfg.HoldLabels)
				}
				return []string{sc.res.WebhookID}
			},
			wrote: "update %s",
		},
		{
			name:    "the rebuild's write",
			arrange: fresh,
			act: func(t *testing.T, sc *scene) []string {
				if _, err := reprovision(sc, "nestor", "saboteur"); err != nil {
					t.Fatal(err)
				}
				if _, cfg := sc.s.stored(t, sc.res); !slices.Equal(cfg.BotIDs, []string{"nestor", "saboteur"}) {
					t.Fatalf("the site did not run: bots %v", cfg.BotIDs)
				}
				return []string{sc.res.WebhookID}
			},
			wrote: "update %s",
		},
		{
			name:    "the undo of a failed rebuild",
			arrange: fresh,
			act: func(t *testing.T, sc *scene) []string {
				sc.s.AdminFor = func(context.Context, Connection) (Admin, error) {
					return nil, errors.New("gitlab: installation token: HTTP 503")
				}
				if _, err := reprovision(sc, "nestor", "saboteur"); err == nil {
					t.Fatal("the rebuild did not fail, so nothing was undone")
				}
				if _, cfg := sc.s.stored(t, sc.res); !slices.Equal(cfg.BotIDs, []string{"nestor"}) {
					t.Fatalf("the undo did not run: the config holds bots %v", cfg.BotIDs)
				}
				writes, _ := sc.watch.seen()
				if n := len(writes); n != 2 {
					t.Fatalf("config writes %v, want the rebuild's and its undo", writes)
				}
				return []string{sc.res.WebhookID}
			},
			wrote: "update %s",
		},
		{
			name: "the bot-rules backfill",
			arrange: func(t *testing.T) *scene {
				sc := fresh(t)
				editConfig(t, sc, func(c *webhooks.Config) { c.BotRules = nil })
				return sc
			},
			act: func(t *testing.T, sc *scene) []string {
				res, err := reprovision(sc, "nestor")
				if err != nil || res.Created {
					t.Fatalf("short-circuit: created %v, err %v", res.Created, err)
				}
				if _, cfg := sc.s.stored(t, sc.res); len(cfg.BotRules) != 1 {
					t.Fatalf("the backfill did not run: bot rules %+v", cfg.BotRules)
				}
				return []string{sc.res.WebhookID}
			},
			wrote: "update %s",
		},
		{
			name: "Deprovision binding a legacy config to its integration",
			arrange: func(t *testing.T) *scene {
				sc := fresh(t)
				editConfig(t, sc, func(c *webhooks.Config) { c.ProvisionIntegrationID = "" })
				return sc
			},
			act: func(t *testing.T, sc *scene) []string {
				if err := sc.s.Deprovision(ctx, "t1", sc.res.IntegrationID); err != nil {
					t.Fatal(err)
				}
				if _, cfg := sc.s.stored(t, sc.res); cfg.ProvisionIntegrationID != sc.res.IntegrationID || cfg.Enabled {
					t.Fatalf("the site did not run: authority %q, enabled %v", cfg.ProvisionIntegrationID, cfg.Enabled)
				}
				return []string{sc.res.WebhookID}
			},
			wrote: "update %s",
		},
		{
			name: "a migration: the freeze of the config it leaves, the Create of the one it moves to",
			arrange: func(t *testing.T) *scene {
				sc := fresh(t)
				ri, _ := sc.s.stored(t, sc.res)
				ri.WebhookSecretSealed = nil
				if err := sc.s.Integrations.Update(ctx, ri); err != nil {
					t.Fatal(err)
				}
				return sc
			},
			act: func(t *testing.T, sc *scene) []string {
				moved, err := reprovision(sc, "nestor", "saboteur")
				if err != nil {
					t.Fatal(err)
				}
				if moved.WebhookID == sc.res.WebhookID {
					t.Fatal("the provision did not migrate to a new webhook config")
				}
				if _, left := sc.s.stored(t, sc.res); !left.ProvisionFrozen {
					t.Fatal("the site did not run: the config the migration leaves is not frozen")
				}
				writes, _ := sc.watch.seen()
				if !slices.Contains(writes, "create "+moved.WebhookID) {
					t.Fatalf("config writes %v: the new config was not created", writes)
				}
				return []string{sc.res.WebhookID, moved.WebhookID}
			},
			wrote: "update %s",
		},
		{
			name: "the pin reconcile after a migration",
			arrange: func(t *testing.T) *scene {
				o, _, first, _ := migrationWithOperatorPins(t)
				return spineRouted(t, wireSpine(o), first)
			},
			act: func(t *testing.T, sc *scene) []string {
				// The pinned secret dies between the carry's read and the
				// new row's write: the reconcile clears it from the new row.
				tctx := store.WithTenant(ctx, "t1")
				sc.s.Webhooks = &beforeCreateStore{ConfigStore: sc.s.Webhooks, before: func() {
					if err := sc.s.Secrets.Delete(tctx, "sec-op"); err != nil {
						t.Error(err)
					}
				}}
				moved, err := reprovision(sc, "revi-converse")
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := sc.s.Webhooks.Get(ctx, moved.WebhookID)
				if err != nil {
					t.Fatal(err)
				}
				if _, stands := cfg.SecretOverrides["other_token"]; stands || moved.WebhookID == sc.res.WebhookID {
					t.Fatalf("the reconcile did not run: pins %v, migrated %v", cfg.SecretOverrides, moved.WebhookID != sc.res.WebhookID)
				}
				writes, _ := sc.watch.seen()
				if !slices.Contains(writes, "update "+moved.WebhookID) {
					t.Fatalf("config writes %v: the reconcile's Update of the new config is missing", writes)
				}
				return []string{sc.res.WebhookID, moved.WebhookID}
			},
			wrote: "update %s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := tc.arrange(t)
			ids := tc.act(t, sc)
			writes, dropped := sc.watch.seen()
			if want := fmt.Sprintf(tc.wrote, sc.res.WebhookID); !slices.Contains(writes, want) {
				t.Fatalf("config writes %v lack %q: the row did not reach its site", writes, want)
			}
			for _, d := range dropped {
				t.Errorf("a config write does not carry the switch: %s", d)
			}
			for _, id := range ids {
				cfg, err := sc.s.Webhooks.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.ForgeRouting.Issues != webhooks.RoutingSpine || !sameInstantPtr(cfg.SpineSince, sc.flipped) {
					t.Errorf("config %s after the write: routing %+v, spine_since %v; want spine since %v", id, cfg.ForgeRouting, cfg.SpineSince, sc.flipped)
				}
			}
		})
	}
}
