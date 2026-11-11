// The Revi bots this repo SHIPS (bots/review-pr, bots/revi-converse), read
// through the launch's own readers — the publisher's walk
// (model.EffectiveProviders), the ONE call-site enumeration (model.CallSites)
// and the supervisors' runtime specs (supervise.SpecsFromWorkflow) — with
// every `${…}` dial the bots read blanked, so the reading is the catalogue's
// own defaults whatever the host exports.
//
// The default launch (no routing policy) is pinned byte for byte to a golden
// generated on origin/main before the routing-cells change: whatever the
// cells learn, a run no cell reaches reads exactly what it read before.

package cloudpublisher

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// updateReviWalkGolden regenerates testdata/revi_walk_default.golden.json.
// Only ever run on a tree whose readers are the reference: the golden is the
// statement "a default launch reads what origin/main read".
var updateReviWalkGolden = flag.Bool("update-revi-walk-golden", false, "rewrite the Revi default-walk golden")

// reviBots are the two shipped Revi bundles.
var reviBots = []string{"review-pr", "revi-converse"}

// catalogBotDir is a shipped bot's directory, from this package.
func catalogBotDir(t *testing.T, bot string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "bots", bot))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// freezeCatalogBotDials blanks every `${…}` name any `.bot` file of the
// bundle references, plus the supervisor environment defaults, so each span
// reads its own default — the catalogue's reading, not the host's.
func freezeCatalogBotDials(t *testing.T, bot string) {
	t.Helper()
	var texts []string
	err := filepath.WalkDir(catalogBotDir(t, bot), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".bot") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		texts = append(texts, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(texts, "\n")
	for _, n := range envDialNames(all) {
		t.Setenv(n, "")
	}
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
}

// compileShippedBot compiles one shipped bot, imports included — the
// workflow its launch publishes.
func compileShippedBot(t *testing.T, bot string) *ir.Workflow {
	t.Helper()
	path := filepath.Join(catalogBotDir(t, bot), "main.bot")
	u := unittest.Whole(unit.LoadDir(path))
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse %s: %s", path, d.Error())
		}
	}
	if u.Merged == nil {
		t.Fatalf("parse %s: no AST", path)
	}
	cr := ir.Compile(u.Merged)
	if cr.HasErrors() {
		t.Fatalf("compile %s: %s", path, cr.Diagnostics[0].Error())
	}
	return cr.Workflow
}

// reviWalkSnapshot is what the readers say about one bot under one view.
type reviWalkSnapshot struct {
	Walk          model.ProviderResolution `json:"walk"`
	OnlyEnvFunded bool                     `json:"only_env_funded"`
	CallSites     []reviCallSite           `json:"call_sites"`
	Supervisors   []reviSupervisorSpec     `json:"supervisors"`
}

type reviCallSite struct {
	ID      string          `json:"id"`
	Kind    model.SiteKind  `json:"kind"`
	Backend string          `json:"backend"`
	Legs    []credroute.Leg `json:"legs"`
	Model   string          `json:"model"`
	Role    credroute.Role  `json:"role"`
	Host    bool            `json:"host"`
	Rescue  bool            `json:"rescue"`
}

type reviSupervisorSpec struct {
	Name           string `json:"name"`
	Model          string `json:"model"`
	ProviderHint   string `json:"provider_hint"`
	GatewayWatched bool   `json:"gateway_watched"`
}

// reviReadersUnder reads wf through the launch's readers under view.
func reviReadersUnder(t *testing.T, wf *ir.Workflow, view model.RouteView) reviWalkSnapshot {
	t.Helper()
	walk := model.EffectiveProviders(view, walkProviderSet)
	snap := reviWalkSnapshot{Walk: walk, OnlyEnvFunded: walk.OnlyEnvFunded()}
	for _, s := range model.CallSites(view, nil) {
		snap.CallSites = append(snap.CallSites, reviCallSite{
			ID: s.ID, Kind: s.Kind, Backend: s.Backend, Legs: s.Legs, Model: s.Model,
			Role: s.Role, Host: s.Host, Rescue: s.Rescue,
		})
	}
	for _, s := range reviSupervisorSpecs(wf, view) {
		snap.Supervisors = append(snap.Supervisors, reviSupervisorSpec{
			Name: s.Name, Model: s.Model, ProviderHint: s.ProviderHint, GatewayWatched: s.GatewayWatched,
		})
	}
	return snap
}

// reviSupervisorSpecs is the supervisors' runtime reading of wf under view.
func reviSupervisorSpecs(wf *ir.Workflow, view model.RouteView) []supervise.Spec {
	return supervise.SpecsFromWorkflow(wf, view.DecidedVars(), view.Overrides, iterlog.Nop())
}

// TestReviShippedBots_DefaultLaunchReadsWhatOriginMainRead pins the default
// launch — no routing policy, no override row — of both shipped Revi bots,
// every reader, to the golden generated on origin/main before the routing
// cells learned their route-scoped and supervisor selectors.
func TestReviShippedBots_DefaultLaunchReadsWhatOriginMainRead(t *testing.T) {
	got := map[string]reviWalkSnapshot{}
	for _, bot := range reviBots {
		freezeCatalogBotDials(t, bot)
		wf := compileShippedBot(t, bot)
		view := model.RouteViewForLaunch(wf, model.ModelOverrides{}, nil, map[string]string(nil), nil)
		got[bot] = reviReadersUnder(t, wf, view)
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	golden := filepath.Join("testdata", "revi_walk_default.golden.json")
	if *updateReviWalkGolden {
		if err := os.WriteFile(golden, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(want) != string(raw) {
		t.Fatalf("the default launch of the shipped Revi bots no longer reads what origin/main read — a cell-less run must not move.\n--- want (%s)\n%s\n--- got\n%s", golden, want, raw)
	}
	// The golden is a statement about real routes, not an empty file.
	if w := got["review-pr"].Walk; !slices.Contains(w.Providers, "anthropic") || !slices.Contains(w.Providers, "zai") {
		t.Fatalf("review-pr's default walk names %v — the golden fixture is not reading the shipped bot", w.Providers)
	}
}

// reviGatewayCellsJSON is the routing block a product writes on its forge
// integration (PATCH /api/teams/{id}/forge/repo-bots/{integration_id},
// `routing`) — or on its team's routing policy — so that every LLM site of
// the shipped Revi pair, the pacers and the GLM rescues included, runs on
// claw against the team's own gateway model. Each node cell fixes the three
// fields the walk reads (backend, provider, model): the provider cell
// collapses the slot's own `provider:` dial, so no `${ITERION_VIBE_*}` text
// is read on any route. Every cell names its bot, so a co-enabled bot's
// same-named node stays its own.
const reviGatewayCellsJSON = `{
  "overrides": [
    {"selector": "review-pr/converge", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/converge_gpt", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/reviewer_claude", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/reviewer_claude_glance", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/reviewer_gpt", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/reviewer_gpt_glance", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/converge#glm", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/reviewer_claude#glm", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/reviewer_claude_glance#glm", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/supervisor:pacer", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "review-pr/supervisor:pacer_gpt", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "revi-converse/converse_agent", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"},
    {"selector": "revi-converse/converse_agent#glm", "backend": "claw", "provider": "openai_compatible", "model": "openai_compatible/scaleway/deepseek-v4.1-flash"}
  ]
}`

// reviGatewayModelID is the one model every cell pins.
const reviGatewayModelID = "openai_compatible/scaleway/deepseek-v4.1-flash"

// reviPrimarySites are each bot's primary LLM sites — the node routes the
// strict team_gateway refusal reads (DecidedPrimary).
var reviPrimarySites = map[string][]string{
	"review-pr":     {"converge", "converge_gpt", "reviewer_claude", "reviewer_claude_glance", "reviewer_gpt", "reviewer_gpt_glance"},
	"revi-converse": {"converse_agent"},
}

// reviRescueAndSupervisorSites are each bot's other LLM sites: the GLM
// rescues and the pacers.
var reviRescueAndSupervisorSites = map[string][]string{
	"review-pr":     {"converge#glm", "reviewer_claude#glm", "reviewer_claude_glance#glm", "supervisor:pacer", "supervisor:pacer_gpt"},
	"revi-converse": {"converse_agent#glm"},
}

// reviGatewayPolicy is the block above as a launch resolves it at level
// `source`: validated by the write surfaces' own validator, folded by the
// production fold, snapshotted the way resolveRunLLMRoutePolicy stores it.
func reviGatewayPolicy(t *testing.T, source string) *store.RunLLMRoutePolicy {
	t.Helper()
	var block llmroute.Policy
	if err := json.Unmarshal([]byte(reviGatewayCellsJSON), &block); err != nil {
		t.Fatalf("the block is not a routing policy: %v", err)
	}
	if err := llmroute.Validate(block); err != nil {
		t.Fatalf("llmroute.Validate refused the activation block: %v", err)
	}
	resolved, sources := llmroute.Resolve(llmroute.Layer{Source: source, Policy: block})
	out := &store.RunLLMRoutePolicy{PairOrder: resolved.PairOrder, Triggers: resolved.Triggers, Sources: sources}
	for _, o := range resolved.Overrides {
		out.Overrides = append(out.Overrides, store.RunLLMRouteOverride{Selector: o.Selector, Backend: o.Backend, Model: o.Model, Provider: o.Provider, Effort: o.Effort})
	}
	return out
}

// reviGatewayView is the publisher's attributed reading of bot under pol:
// the launch's own fold of the cells for that bot, read the way the
// credential walk reads it once a team key holds the gateway slot.
func reviGatewayView(t *testing.T, bot string, wf *ir.Workflow, pol *store.RunLLMRoutePolicy) model.RouteView {
	t.Helper()
	view := model.RouteViewForLaunch(wf, buildModelOverrides(runview.EffectiveModelOverrides(nil, pol, llmroute.BotIdentity{ID: bot})), nil, map[string]string(nil), nil)
	view.GatewayKeyFunded = true
	return view
}

// TestReviShippedBots_EveryCellDecidesItsRouteOnTheTeamGateway is the
// operator's activation (2026-11-10) read on the bots this repo ships, at
// the integration level and at the team level: under the block, the walk
// names the gateway alone, decides every primary site from the cells (the
// slots' `${ITERION_VIBE_*}` dials are read nowhere), defers and widens
// nowhere, and every reader downstream — the call sites the admission keys,
// the supervisors' runtime specs — sends the gateway model on claw.
func TestReviShippedBots_EveryCellDecidesItsRouteOnTheTeamGateway(t *testing.T) {
	for _, level := range []string{llmroute.SourceWebhook, llmroute.SourceTeam} {
		pol := reviGatewayPolicy(t, level)
		for _, bot := range reviBots {
			t.Run(level+"/"+bot, func(t *testing.T) {
				freezeCatalogBotDials(t, bot)
				wf := compileShippedBot(t, bot)
				view := reviGatewayView(t, bot, wf, pol)
				walk := model.EffectiveProviders(view, walkProviderSet)
				if !slices.Equal(walk.Providers, []string{"openai_compatible"}) {
					t.Errorf("walk names %v, want [openai_compatible] alone (pins: %v)", walk.Providers, walk.Pins)
				}
				if len(walk.Unknown) != 0 || !walk.NarrowSafe || len(walk.UnattributedSites) != 0 {
					t.Errorf("walk widened: unknown=%v narrow_safe=%v unattributed=%v", walk.Unknown, walk.NarrowSafe, walk.UnattributedSites)
				}
				if walk.EnvDeferred || len(walk.DeferredSites) != 0 {
					t.Errorf("walk defers to the environment: env_deferred=%v deferred_sites=%v — a cell fixing backend, provider and model decides the route", walk.EnvDeferred, walk.DeferredSites)
				}
				if got := walk.DecidedPrimary["openai_compatible"]; !slices.Equal(got, reviPrimarySites[bot]) {
					t.Errorf("decided primaries = %v, want every primary site %v", got, reviPrimarySites[bot])
				}
				pinned := walk.Pins["openai_compatible"]
				for _, site := range append(slices.Clone(reviPrimarySites[bot]), reviRescueAndSupervisorSites[bot]...) {
					if !slices.Contains(pinned, site) {
						t.Errorf("site %q is not pinned to the gateway (pins: %v)", site, walk.Pins)
					}
				}
				for _, s := range model.CallSites(view, nil) {
					if s.Model != reviGatewayModelID || s.Backend != "claw" {
						t.Errorf("call site %q sends %q on %q, want %q on claw", s.ID, s.Model, s.Backend, reviGatewayModelID)
					}
					for _, leg := range s.Legs {
						if leg.Provider != "" && leg.Provider != "openai_compatible" {
							t.Errorf("call site %q walks leg %+v outside the gateway", s.ID, leg)
						}
					}
				}
				for _, spec := range reviSupervisorSpecs(wf, view) {
					if spec.Model != reviGatewayModelID {
						t.Errorf("supervisor %q evaluates on %q, want %q", spec.Name, spec.Model, reviGatewayModelID)
					}
				}
			})
		}
	}
}

// TestReviShippedBots_TheTeamGatewayKeySealsAndStampsTheBoundary drives the
// public entry (SubmitLaunch) with each shipped Revi bot under the block, on
// a publisher whose platform tier holds the two vendor keys the cells move
// away from and whose team holds its own gateway key: the walk attributes
// the routes to that key, the bundle seals it and no vendor key, the run is
// stamped team_gateway, and the wire carries the bot's own cells.
func TestReviShippedBots_TheTeamGatewayKeySealsAndStampsTheBoundary(t *testing.T) {
	pol := reviGatewayPolicy(t, llmroute.SourceWebhook)
	for _, bot := range reviBots {
		t.Run(bot, func(t *testing.T) {
			freezeCatalogBotDials(t, bot)
			wf := compileShippedBot(t, bot)
			f := newReviAcceptanceFixture(t)
			var keyID string
			seedGatewayKey(t, f.pub.apiKeys, f.pub.sealer, "team1", func(k *secrets.ApiKey) { keyID = k.ID })
			dir := catalogBotDir(t, bot)
			src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
			if err != nil {
				t.Fatal(err)
			}
			spec := runview.LaunchSpec{
				FilePath:       filepath.Join(dir, "main.bot"),
				Source:         string(src),
				BundleDir:      dir,
				BotID:          bot,
				LLMRoutePolicy: pol,
				ModelOverrides: runview.EffectiveModelOverrides(nil, pol, llmroute.BotIdentity{ID: bot}),
			}
			ctx := store.WithIdentity(context.Background(), "team1", "webhook:cfg-1")
			runID := "run-revi-cells-" + bot
			if _, err := f.pub.SubmitLaunch(ctx, runID, spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
				t.Fatalf("SubmitLaunch = %v (log:\n%s)", err, f.logs.String())
			}
			if len(f.published) != 1 {
				t.Fatalf("published %d messages, want 1", len(f.published))
			}
			msg := f.published[0]
			run, err := f.runs.LoadRun(ctx, runID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if !run.ContentBoundary.TeamGateway() || run.ContentBoundary.KeyID != keyID {
				t.Errorf("content boundary = %+v, want team_gateway naming key %s (log:\n%s)", run.ContentBoundary, keyID, f.logs.String())
			}
			b := openGatewayBundleOn(t, f.pub, runID, "", credResolution{secretsRef: msg.SecretsRef, dek: msg.BundleDEK})
			if b.APIKeys[secrets.ProviderOpenAICompatible] != gwTeamKey {
				t.Errorf("bundle gateway slot = %q, want the team's key sealed", b.APIKeys[secrets.ProviderOpenAICompatible])
			}
			if !b.AmbientLLMRefused {
				t.Errorf("a team_gateway run's bundle does not refuse the pod's environment")
			}
			// The stamp is what the run SPENDS (the bundle keeps the shared
			// tier's default credentials beside it, by design): the team's
			// gateway key, and none of the platform's vendor keys.
			if !slices.Equal(run.CredFingerprints, []string{gwFingerprint}) {
				t.Errorf("run cred_fingerprints = %v, want the team gateway key's alone (log:\n%s)", run.CredFingerprints, f.logs.String())
			}
			for _, row := range msg.ModelOverrides {
				if strings.Contains(row.Selector, "/") {
					t.Errorf("wire row %+v still carries a bot dimension — the fold resolves it", row)
				}
			}
			if want := len(reviPrimarySites[bot]) + len(reviRescueAndSupervisorSites[bot]); len(msg.ModelOverrides) != want {
				t.Errorf("wire carries %d rows, want this bot's %d cells: %+v", len(msg.ModelOverrides), want, msg.ModelOverrides)
			}
		})
	}
}

// The activation block at the integration level, and the team's record
// writing a team-wide `*/converge` default on a vendor model: the
// integration's cell for review-pr is the more specific one (the bot's
// name outranks `*/`), so converge stays on the gateway — at the walk that
// seals the run and at the call site dispatch reads.
func TestReviShippedBots_AnIntegrationPinBeatsATeamWideStarDefault(t *testing.T) {
	var block llmroute.Policy
	if err := json.Unmarshal([]byte(reviGatewayCellsJSON), &block); err != nil {
		t.Fatal(err)
	}
	team := llmroute.Policy{Overrides: []llmroute.Override{{Selector: "*/converge", Backend: "claw", Model: "anthropic/claude-sonnet-5"}}}
	for _, p := range []llmroute.Policy{block, team} {
		if err := llmroute.Validate(p); err != nil {
			t.Fatalf("validate: %v", err)
		}
	}
	resolved, sources := llmroute.Resolve(
		llmroute.Layer{Source: llmroute.SourceWebhook, Policy: block},
		llmroute.Layer{Source: llmroute.SourceTeam, Policy: team},
	)
	pol := &store.RunLLMRoutePolicy{Sources: sources}
	for _, o := range resolved.Overrides {
		pol.Overrides = append(pol.Overrides, store.RunLLMRouteOverride{Selector: o.Selector, Backend: o.Backend, Model: o.Model, Provider: o.Provider, Effort: o.Effort})
	}
	freezeCatalogBotDials(t, "review-pr")
	wf := compileShippedBot(t, "review-pr")
	view := reviGatewayView(t, "review-pr", wf, pol)
	walk := model.EffectiveProviders(view, walkProviderSet)
	if !slices.Equal(walk.Providers, []string{llmroute.ProviderOpenAICompatible}) {
		t.Errorf("walk providers = %v, want the gateway alone", walk.Providers)
	}
	found := false
	for _, s := range model.CallSites(view, nil) {
		if s.ID != "converge" {
			continue
		}
		found = true
		if s.Model != reviGatewayModelID || s.Backend != "claw" {
			t.Errorf("converge sends %q on %q, want %q on claw — the team's */converge default beat the integration's review-pr/converge pin", s.Model, s.Backend, reviGatewayModelID)
		}
	}
	if !found {
		t.Fatal("no converge call site — the fixture no longer names the node this witness pins")
	}
}

// TestReviShippedBots_TheActivationBlockPassesTheLaunchScreens drives the
// block through runview.Service.Launch — the fold and every launch screen
// (composed pair, gateway crossing, route crossing, permission) before the
// publisher — and through the integration write screen: the documented
// activation on the bots this repo ships is admitted and draws no warning.
func TestReviShippedBots_TheActivationBlockPassesTheLaunchScreens(t *testing.T) {
	pol := reviGatewayPolicy(t, llmroute.SourceWebhook)
	for _, bot := range reviBots {
		t.Run(bot, func(t *testing.T) {
			freezeCatalogBotDials(t, bot)
			f := newReviAcceptanceFixture(t)
			seedGatewayKey(t, f.pub.apiKeys, f.pub.sealer, "team1", nil)
			dir := catalogBotDir(t, bot)
			path := filepath.Join(dir, "main.bot")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			warnings, err := runview.ScreenPolicyOverrides(path, string(src), dir, "", llmroute.BotIdentity{ID: bot}, pol)
			if err != nil || len(warnings) != 0 {
				t.Fatalf("write screen = (%q, %v), want the activation block screened clean", warnings, err)
			}
			svc, err := runview.NewService(t.TempDir(), runview.WithLogger(iterlog.Nop()), runview.WithLaunchPublisher(f.pub))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			ctx := store.WithIdentity(context.Background(), "team1", "webhook:cfg-1")
			if _, err := svc.Launch(ctx, runview.LaunchSpec{
				FilePath: path, Source: string(src), BundleDir: dir, BotID: bot, LLMRoutePolicy: pol,
			}); err != nil {
				t.Fatalf("Launch = %v (log:\n%s)", err, f.logs.String())
			}
			if len(f.published) != 1 {
				t.Fatalf("published %d messages, want 1", len(f.published))
			}
		})
	}
}
