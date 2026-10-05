package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

func routeResolveServer(t *testing.T, rec *platformcfg.PlatformCredentials) *Server {
	t.Helper()
	return New(Config{
		SkipProjectRegistration:     true,
		PlatformCredentialsSettings: platformcfg.NewMemoryStore[platformcfg.PlatformCredentials](),
		PlatformCredentialsResolver: platformcfg.NewResolver(rec0(rec), nil),
	}, iterlog.New(iterlog.LevelError, nil))
}

func rec0(rec *platformcfg.PlatformCredentials) platformcfg.Store[platformcfg.PlatformCredentials] {
	st := platformcfg.NewMemoryStore[platformcfg.PlatformCredentials]()
	if rec != nil {
		_ = st.Put(context.Background(), *rec)
	}
	return st
}

// The snapshot answers with the platform record's values and names it: a
// run doc that cannot say WHY it routes is an unauditable one.
func TestResolveRunLLMRoutePolicy_PlatformRecordWinsAndNamesItself(t *testing.T) {
	park := llmroute.RefusedPinnedPark
	s := routeResolveServer(t, &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{
		PairOrder:        []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")},
		RefusedPinnedKey: park,
	}})
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil {
		t.Fatal("snapshot = nil")
	}
	if len(got.PairOrder) != 1 || got.PairOrder[0] != "claw+anthropic_key" {
		t.Fatalf("pair_order = %v, want the record's", got.PairOrder)
	}
	if got.RefusedPinnedKey != park {
		t.Fatalf("refused_pinned_key = %q, want the record's park", got.RefusedPinnedKey)
	}
	if got.Sources["refused_pinned_key"] != llmroute.SourcePlatform {
		t.Fatalf("refused_pinned_key provenance = %q, want platform", got.Sources["refused_pinned_key"])
	}
	if got.Sources["triggers"] != llmroute.SourceDefault {
		t.Fatalf("triggers provenance = %q, want default", got.Sources["triggers"])
	}
	if len(got.Triggers) != len(llmroute.Triggers) {
		t.Fatalf("triggers = %v, want the whole vocabulary", got.Triggers)
	}
}

// The env dial answers where the record is silent, and its provenance says
// env — "was it the DB record or the deployment env?" must be answerable
// from the snapshot alone.
func TestResolveRunLLMRoutePolicy_EnvDialAnswersWithItsOwnProvenance(t *testing.T) {
	t.Setenv(llmroute.EnvRefusedPinnedKey, llmroute.RefusedPinnedPark)
	s := routeResolveServer(t, nil)
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.RefusedPinnedKey != llmroute.RefusedPinnedPark {
		t.Fatalf("refused_pinned_key = %q, want the env dial's park", got.RefusedPinnedKey)
	}
	if got.Sources["refused_pinned_key"] != llmroute.SourceEnv {
		t.Fatalf("refused_pinned_key provenance = %q, want env", got.Sources["refused_pinned_key"])
	}
}

// A launch-site layer (the webhook's own policy, slice 3's bot/run
// levels) outranks the platform record — the `higher` parameter is the
// shape slices 2–3 extend.
func TestResolveRunLLMRoutePolicy_HigherLayerOutranksThePlatform(t *testing.T) {
	park := llmroute.RefusedPinnedPark
	s := routeResolveServer(t, &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{
		RefusedPinnedKey: park,
	}})
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source: llmroute.SourceBot,
		Policy: llmroute.Policy{PairOrder: []string{llmroute.Pair(llmroute.HarnessCodex, llmroute.CredChatGPTForfait)}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got.PairOrder) != 1 || got.PairOrder[0] != "codex+chatgpt_forfait" {
		t.Fatalf("pair_order = %v, want the bot layer's", got.PairOrder)
	}
	if got.Sources["pair_order"] != llmroute.SourceBot {
		t.Fatalf("pair_order provenance = %q, want bot", got.Sources["pair_order"])
	}
	if got.RefusedPinnedKey != park {
		t.Fatalf("refused_pinned_key = %q, want the platform's park (the bot layer set nothing)", got.RefusedPinnedKey)
	}
}

// The snapshot shape never loses an empty trigger answer: the ceiling's
// "never switch" must survive the store round-trip (triggers is not
// omitempty on the snapshot — a JSON round-trip keeps the []).
func TestRunLLMRoutePolicy_EmptyTriggersSurviveJSON(t *testing.T) {
	in := store.RunLLMRoutePolicy{Triggers: []string{}, RefusedPinnedKey: llmroute.RefusedPinnedForfait}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out store.RunLLMRoutePolicy
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Triggers == nil {
		t.Fatalf("triggers = nil after a JSON round-trip of %s — omitempty erased the empty answer", b)
	}
}

// A launcher built without a resolver (tests, embedders) leaves the field
// nil: the snapshot is simply absent and the consumer applies the package
// defaults — the same tolerance retryPolicyFor states.
func TestServiceLauncher_routePolicyForToleratesNoResolver(t *testing.T) {
	l := &serviceLauncher{}
	if got, err := l.routePolicyFor(context.Background(), trigger.LaunchPlan{}); got != nil || err != nil {
		t.Fatalf("routePolicyFor = %+v, %v — want nil, nil without a resolver", got, err)
	}
	want := &store.RunLLMRoutePolicy{RefusedPinnedKey: llmroute.RefusedPinnedPark}
	l2 := &serviceLauncher{resolveRoute: func(context.Context, string, string, ...llmroute.Layer) (*store.RunLLMRoutePolicy, error) {
		return want, nil
	}}
	if got, err := l2.routePolicyFor(context.Background(), trigger.LaunchPlan{TenantID: "t1", BotID: "b1"}); err != nil || got != want {
		t.Fatalf("routePolicyFor = %+v, %v — want the resolver's snapshot", got, err)
	}
}

// The RUN level (ADR-121 §0): the launcher's own policy, folded as the
// HEAD layer — it outranks the platform record, and its explicit lock is
// the one seat that reopens strict (the marker slice 2 reserved).
func TestResolveRunLLMRoutePolicy_RunLevelOutranksAndReopens(t *testing.T) {
	yes := true
	rec := &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{Strict: &yes}}
	s := routeResolveServer(t, rec)
	no := false
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source:   llmroute.SourceRun,
		Launcher: true,
		Policy:   llmroute.Policy{Strict: &no, Locks: []string{llmroute.FieldStrict}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Strict {
		t.Fatalf("strict = true, want false — the launcher's explicit lock is the one unset path, for their own run")
	}
	if got.Sources["strict"] != llmroute.SourceRun {
		t.Fatalf("strict provenance = %q, want run", got.Sources["strict"])
	}

	// WITHOUT the lock the author's true wins over the launcher's false:
	// the launcher steers, the author's requirement stands.
	got, err = s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source:   llmroute.SourceRun,
		Launcher: true,
		Policy:   llmroute.Policy{Strict: &no},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Strict {
		t.Fatalf("strict = false, want true — no lock, no unset: the platform's true beats the run's false")
	}
	if got.Sources["strict"] != llmroute.SourcePlatform {
		t.Fatalf("strict provenance = %q, want platform", got.Sources["strict"])
	}
}

// ---- the tenant levels (delivery 2) ----

// routingPolicyStores backs the fold's tenant reads with one memory
// store per document, the way the platform record's tests wire theirs.
func routingPolicyStores() (func(string) platformcfg.Store[platformcfg.RoutingPolicyRecord], map[string]*platformcfg.MemoryStore[platformcfg.RoutingPolicyRecord]) {
	stores := map[string]*platformcfg.MemoryStore[platformcfg.RoutingPolicyRecord]{}
	return func(docID string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		st, ok := stores[docID]
		if !ok {
			st = platformcfg.NewMemoryStore[platformcfg.RoutingPolicyRecord]()
			stores[docID] = st
		}
		return st
	}, stores
}

func putRoutingPolicyRecord(t *testing.T, st platformcfg.Store[platformcfg.RoutingPolicyRecord], rec *platformcfg.RoutingPolicyRecord) {
	t.Helper()
	if rec != nil {
		if err := st.Put(context.Background(), *rec); err != nil {
			t.Fatalf("seed routing policy: %v", err)
		}
	}
}

// A team's and its org's records answer between the bot and the platform
// level, team over org, and the snapshot NAMES them: "why did this run
// route here" must be answerable per level from the run doc alone.
func TestResolveRunLLMRoutePolicy_TeamThenOrgAnswerBetweenBotAndPlatform(t *testing.T) {
	s := newOrgTestServer(t)
	seedOrg(t, s, "org1", "org1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "team1", Name: "team1", Slug: "team1", OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	putRoutingPolicyRecord(t, factory(platformcfg.TeamRoutingPolicyID("team1")), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{PairOrder: []string{"claw+zai_key"}},
	})
	putRoutingPolicyRecord(t, factory(platformcfg.OrgRoutingPolicyID("org1")), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{PairOrder: []string{"claw+anthropic_key"}},
	})

	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.PairOrder[0] != "claw+zai_key" || got.Sources["pair_order"] != llmroute.SourceTeam {
		t.Fatalf("team must answer over org: %v / %q", got.PairOrder, got.Sources["pair_order"])
	}

	// The team record gone, the org answers with its own provenance.
	del, ok := factory(platformcfg.TeamRoutingPolicyID("team1")).(platformcfg.Deleter)
	if !ok {
		t.Fatal("the memory store must implement Deleter")
	}
	if err := del.Delete(context.Background()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = s.resolveRunLLMRoutePolicy(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.PairOrder[0] != "claw+anthropic_key" || got.Sources["pair_order"] != llmroute.SourceOrg {
		t.Fatalf("org must answer once the team level is absent: %v / %q", got.PairOrder, got.Sources["pair_order"])
	}
}

// The platform's triggers are the ceiling the tenant levels may only
// narrow: a team asking for auth above a platform that allows only
// usage_window resolves to usage_window, and the provenance says
// platform_ceiling — never a silent extension.
func TestResolveRunLLMRoutePolicy_PlatformCeilingPrunesTeamTriggers(t *testing.T) {
	s := newOrgTestServer(t)
	seedOrg(t, s, "org1", "org1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "team1", Name: "team1", Slug: "team1", OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	putRoutingPolicyRecord(t, factory(platformcfg.TeamRoutingPolicyID("team1")), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{Triggers: []string{"usage_window", "auth"}},
	})
	// A platform record carrying a narrower trigger set — the route
	// resolver takes the platform voice from PlatformCredentials, so the
	// memory store needs the record BEFORE the resolver snapshot. The
	// resolver fixture (rec0) seeds it.
	s2 := routeResolveServer(t, &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{
		Triggers: []string{"usage_window"},
	}})
	s2.authSvc = s.authSvc
	factory2, _ := routingPolicyStores()
	s2.routingPolicyStoreFor = factory2
	putRoutingPolicyRecord(t, factory2(platformcfg.TeamRoutingPolicyID("team1")), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{Triggers: []string{"usage_window", "auth"}},
	})

	got, err := s2.resolveRunLLMRoutePolicy(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got.Triggers) != 1 || got.Triggers[0] != "usage_window" {
		t.Fatalf("triggers = %v, want the platform ceiling's [usage_window]", got.Triggers)
	}
	if got.Sources["triggers"] != llmroute.SourceCeiling {
		t.Fatalf("triggers provenance = %q, want platform_ceiling", got.Sources["triggers"])
	}
}

// Fail-closed is the ADR's non-negotiable: an unreadable team or org
// record REFUSES the launch — folding around it would lift cost
// governance on a blip. An unknown team refuses too (the org level
// cannot be resolved), never degrades to platform-only.
func TestResolveRunLLMRoutePolicy_UnreadableTenantLevelRefusesTheLaunch(t *testing.T) {
	s := newOrgTestServer(t)
	seedOrg(t, s, "org1", "org1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "team1", Name: "team1", Slug: "team1", OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	s.routingPolicyStoreFor = func(string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		return failingRoutePolicyStore{err: errors.New("mongo wedged")}
	}
	if _, err := s.resolveRunLLMRoutePolicy(context.Background(), "team1", ""); err == nil {
		t.Fatal("an unreadable team record must refuse the launch")
	} else if !strings.Contains(err.Error(), "team routing policy") {
		t.Fatalf("error = %v, want it to name the team level", err)
	}

	// The org read failing refuses too.
	s.routingPolicyStoreFor = func(docID string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		if strings.HasPrefix(docID, "org:") {
			return failingRoutePolicyStore{err: errors.New("mongo wedged")}
		}
		return platformcfg.NewMemoryStore[platformcfg.RoutingPolicyRecord]()
	}
	if _, err := s.resolveRunLLMRoutePolicy(context.Background(), "team1", ""); err == nil {
		t.Fatal("an unreadable org record must refuse the launch")
	}

	// An unknown team: the org level cannot resolve, refuse.
	known := s.routingPolicyStoreFor
	s.routingPolicyStoreFor = known
	if _, err := s.resolveRunLLMRoutePolicy(context.Background(), "team-unknown", ""); err == nil {
		t.Fatal("a team identity cannot resolve must refuse the launch")
	}
}

// failingRoutePolicyStore is the wedged-store probe of the fail-closed
// tests: every read fails, writes fail too (they must never be reached).
type failingRoutePolicyStore struct{ err error }

func (f failingRoutePolicyStore) Get(context.Context) (*platformcfg.RoutingPolicyRecord, error) {
	return nil, f.err
}

func (f failingRoutePolicyStore) Put(context.Context, platformcfg.RoutingPolicyRecord) error {
	return f.err
}

// The ("", "") fold — the platform admin view's call — answers the
// PLATFORM chain without touching identity or the tenant store: with
// an org record present but no team named, the org level is absent.
// Removing the teamID guard would make every admin GET 500 on
// GetTeam("").
func TestResolveRunLLMRoutePolicy_AdminViewFoldIgnoresTenantLevels(t *testing.T) {
	s := routeResolveServer(t, nil)
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	putRoutingPolicyRecord(t, factory(platformcfg.OrgRoutingPolicyID("org1")), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{PairOrder: []string{"claw+zai_key"}},
	})
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Sources["pair_order"] == llmroute.SourceOrg {
		t.Fatalf("the (\"\", \"\") fold must not consult tenant levels: %q", got.Sources["pair_order"])
	}
}

// The resolved model_classes overrides ride the SNAPSHOT (entry-wise
// fold, per-cell provenance) — the wire mirror stamps them onto the run
// message so the runner's crossings map the same table the launch
// resolved. An override a launch resolved but the run doc hid would make
// "why did this run route to X" unanswerable from the snapshot alone.
func TestResolveRunLLMRoutePolicy_ModelClassesFlowToTheSnapshot(t *testing.T) {
	s := newOrgTestServer(t)
	seedOrg(t, s, "org1", "org1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "team1", Name: "team1", Slug: "team1", OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	putRoutingPolicyRecord(t, factory(platformcfg.OrgRoutingPolicyID("org1")), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{ModelClasses: map[string]map[string]string{
			"top": {"openai": "gpt-6-astra"},
		}},
	})

	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ModelClasses["top"]["openai"] != "gpt-6-astra" {
		t.Fatalf("model_classes on the snapshot = %+v, want the org's override", got.ModelClasses)
	}
	if got.Sources["model_classes.top.openai"] != llmroute.SourceOrg {
		t.Fatalf("cell provenance = %q, want org", got.Sources["model_classes.top.openai"])
	}
	if _, block := got.Sources["model_classes"]; block {
		t.Fatalf("the block key must be omitted when cells spoke: %q", got.Sources["model_classes"])
	}
}

// The cross-harness posture rides the snapshot (the fold's
// first-setter-wins answer) — the wire stamps it from there, so a run
// doc must answer "could this run cross harnesses" without replaying
// the launch.
func TestResolveRunLLMRoutePolicy_CrossHarnessFlowsToTheSnapshot(t *testing.T) {
	s := newOrgTestServer(t)
	seedOrg(t, s, "org1", "org1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "team1", Name: "team1", Slug: "team1", OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	putRoutingPolicyRecord(t, factory(platformcfg.TeamRoutingPolicyID("team1")), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{CrossHarness: llmroute.CrossHarnessRestart},
	})

	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.CrossHarness != llmroute.CrossHarnessRestart || got.Sources["cross_harness"] != llmroute.SourceTeam {
		t.Fatalf("cross_harness = %q / %q, want the team's restart", got.CrossHarness, got.Sources["cross_harness"])
	}

	// Silence resolves to "off" (the normalized default), named default.
	if err := factory(platformcfg.TeamRoutingPolicyID("team1")).(platformcfg.Deleter).Delete(context.Background()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = s.resolveRunLLMRoutePolicy(context.Background(), "team1", "")
	if err != nil {
		t.Fatalf("resolve3: %v", err)
	}
	if got.CrossHarness != llmroute.CrossHarnessOff || got.Sources["cross_harness"] != llmroute.SourceDefault {
		t.Fatalf("silent cross_harness = %q / %q, want off / default", got.CrossHarness, got.Sources["cross_harness"])
	}
}
