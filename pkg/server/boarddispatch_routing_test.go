package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The board dispatcher is a launch lane like every other one: it resolves
// the run's routing policy (ADR-121/ADR-129) and its retry contract at the
// launch, and the resolver's refusal refuses the dispatch. A lane that
// resolved neither published a run whose tenant routing cells, provider
// allowlist and retry directive were all absent — the cloud card was the
// one surface where an operator's `providers:` lock did not hold.
//
// Mutants: the resolver call removed → the policy assertions red; its
// error folded around (`_ = routeErr`) → the refusal assertions red.

// seedBoardLaunchTenant gives a board-lane fixture the identity row every
// cloud deployment has. The dispatcher launches as the card's TEAM, so the
// routing fold resolves that team's level and its org's — and REFUSES a team
// no identity store can answer for (route_resolve.go: an org level that
// cannot be resolved is not an absent level). A fixture with no identity
// store is harsher than production, where the cloud startup fails without
// one; every other launch-lane fixture already seeds it.
func seedBoardLaunchTenant(t *testing.T, s *Server, teamID string) {
	t.Helper()
	s.authSvc = newOrgTestServer(t).authSvc
	seedGate(t, s, gateSpec{id: teamID})
}

// seedTeamRoutingCell wires an in-memory routing-policy store on s and puts
// one TEAM-level record for teamID: a provider allowlist and one override
// cell, which is what a launch lane has to carry onto the run.
func seedTeamRoutingCell(t *testing.T, s *Server, teamID string) {
	t.Helper()
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	putRoutingPolicyRecord(t, factory(platformcfg.TeamRoutingPolicyID(teamID)), &platformcfg.RoutingPolicyRecord{
		Policy: &llmroute.Policy{
			Providers: []string{"openai_compatible"},
			Overrides: []llmroute.Override{{
				Selector: "judge",
				Backend:  llmroute.HarnessClaw,
				Model:    "openai_compatible/gw-1/m",
			}},
		},
	})
}

// launchCount reads the recorder under its own mutex: the publisher's slice
// is written from whatever goroutine the lane under test launches on, so an
// observer of it takes the lock, always.
func launchCount(p *tierPublisher) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.specs)
}

// assertCarriesTheTeamCell is the verdict both lanes owe: the published
// spec carries the team level's allowlist and override cell, with the
// provenance that says which level answered, plus a resolved retry policy.
func assertCarriesTheTeamCell(t *testing.T, surface string, pol *store.RunLLMRoutePolicy, retry *store.RunRetryPolicy) {
	t.Helper()
	if pol == nil {
		t.Fatalf("%s published no routing policy — the tenant's cells, its provider allowlist and the audit line of the level that set them never reach the run", surface)
	}
	if len(pol.Providers) != 1 || pol.Providers[0] != "openai_compatible" {
		t.Errorf("%s: providers = %v, want the team's [openai_compatible] allowlist", surface, pol.Providers)
	}
	if got := pol.Sources[llmroute.FieldProviders]; got != llmroute.SourceTeam {
		t.Errorf("%s: providers provenance = %q, want %q — the run doc must say which level locked the vocabulary", surface, got, llmroute.SourceTeam)
	}
	if len(pol.Overrides) != 1 || pol.Overrides[0].Selector != "judge" ||
		pol.Overrides[0].Backend != llmroute.HarnessClaw || pol.Overrides[0].Model != "openai_compatible/gw-1/m" {
		t.Errorf("%s: overrides = %+v, want the team's judge cell (claw + the gateway model)", surface, pol.Overrides)
	}
	if retry == nil {
		t.Fatalf("%s published no retry policy — a bot declaring `retry: usage_window: off` is auto-retried anyway on this lane", surface)
	}
	if len(retry.Sources) == 0 {
		t.Errorf("%s: retry provenance = %v, want the resolved chain's levels", surface, retry.Sources)
	}
}

// TestBoardDispatch_ResolvesRoutingPolicy: a team routing cell reaches the
// run through the cloud board dispatcher.
func TestBoardDispatch_ResolvesRoutingPolicy(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedTeamRoutingCell(t, s, "t1")

	if err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
		ID: "native:1", Bot: "probe", State: native.StateReady,
	}); err != nil {
		t.Fatalf("processBoardCard = %v, want nil", err)
	}
	spec := pub.only(t)
	assertCarriesTheTeamCell(t, "board dispatch", spec.LLMRoutePolicy, spec.RetryPolicy)
}

// TestBoardDispatch_RefusesWhenTheTenantPolicyIsUnreadable: a read failure
// on a tenant routing record REFUSES the dispatch. Folding around it would
// lift the tenant's cost governance — and its provider allowlist — on a
// store blip, which is the one thing the fold's doctrine forbids. Nothing
// ran, so the refusal is the launchRefusal class: the card keeps its column
// under machine provenance and the next tick retries it.
func TestBoardDispatch_RefusesWhenTheTenantPolicyIsUnreadable(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	s.routingPolicyStoreFor = func(string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		return failingRoutePolicyStore{err: errors.New("mongo wedged")}
	}

	err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
		ID: "native:1", Bot: "probe", State: native.StateReady,
	})
	if !errors.Is(err, errCardLaunchRefused) {
		t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — an unreadable tenant routing record must refuse the dispatch, never launch under a policy nobody could read", err)
	}
	if !strings.Contains(err.Error(), "team routing policy") {
		t.Errorf("refusal = %v, want it to name the level that could not be read", err)
	}
	if n := launchCount(pub); n != 0 {
		t.Fatalf("the refused card published %d launches, want 0 — the run must not exist", n)
	}
}

// TestAdmitBoardCard_StaysPolicyBlind: the pre-claim admission is the
// launch PRECONDITIONS and nothing else — it must not resolve the policy.
// A card whose tenant record is unreadable is still admitted (and then
// refused by the launch), because admission failures are retried on the
// tick while a policy read is the launch's own fail-closed decision.
func TestAdmitBoardCard_StaysPolicyBlind(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	s.routingPolicyStoreFor = func(string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		return failingRoutePolicyStore{err: errors.New("mongo wedged")}
	}
	if err := s.admitBoardCard(boundedCtx(t), "t1", native.Issue{
		ID: "native:1", Bot: "probe", State: native.StateReady,
	}); err != nil {
		t.Fatalf("admitBoardCard = %v, want nil — the pre-claim admission owes the preconditions, not the policy", err)
	}
}

// seedIntegrationConfig wires an in-memory webhook config store on s and stores
// cfg in it: the card lanes read the integration level from the STORE, by the
// id the card names, so a fixture that only hands them a card proves nothing.
// Returns the store so a test can narrow the record mid-flight the way an
// admin does.
func seedIntegrationConfig(t *testing.T, s *Server, cfg webhooks.Config) *webhooks.MemoryConfigStore {
	t.Helper()
	cs, ok := s.webhookConfigs.(*webhooks.MemoryConfigStore)
	if !ok {
		cs = webhooks.NewMemoryConfigStore()
		s.webhookConfigs = cs
	}
	if err := cs.Create(boundedCtx(t), cfg); err != nil {
		t.Fatalf("seed integration %s: %v", cfg.ID, err)
	}
	return cs
}

// TestBoardDispatch_CarriesTheIntegrationsRoutingCell: the per-INTEGRATION
// level — #2006's own subject — reaching a BOARD-mode run, and reaching it
// LIVE. A lane that resolved only the tenant levels applied the integration's
// BYOK key override while dropping the integration's lock on where that
// credential may be spent; a lane that read a policy the CARD carries would
// answer with a vocabulary any board-write client can type, and would freeze
// it against the admin's later narrowing — the card names the integration,
// the launch reads the record.
//
// Driven through the real ensureBoardCard → processBoardCard pair on ONE
// config, with the admin narrowing the integration in between, so a stamp the
// lift does not read, a layer the lane does not pass, or a value frozen at
// carding time fails here rather than in production. Three distinguishable
// lists make each failure its own: the team's [openai_compatible], the
// integration's [openai_compatible zai] at carding, its [zai] at launch.
//
// Mutants: the stamp dropped (ensureBoardCard's boardWebhookIDKey arm), the
// layers not passed (processBoardCard's cardLaunchPolicyLayers call), or the
// read answered from the card → the providers/provenance assertions red.
func TestBoardDispatch_CarriesTheIntegrationsRoutingCell(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedTeamRoutingCell(t, s, "t1")
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	cfg := webhooks.Config{
		ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub,
		KeyOverrides: map[string]string{"anthropic_api_key": "byok-anthropic"},
		Routing: &llmroute.Policy{
			Providers: []string{"openai_compatible", "zai"},
			Overrides: []llmroute.Override{{Selector: "judge", Backend: llmroute.HarnessClaw, Model: "zai/glm-4.6"}},
		},
		RetryMaxAttempts: 2,
	}
	cs := seedIntegrationConfig(t, s, cfg)
	s.ensureBoardCard(boundedCtx(t), cfg, webhooks.CommandRoute{BotID: "probe", Mode: "board"},
		map[string]string{"pr_url": "https://forge.example/o/r/pull/7"},
		webhookEventMeta{Kind: "note", SubjectID: "note:1", SubjectURL: "https://forge.example/o/r/pull/7"},
		"", "https://forge.example/o/r.git", "main")
	cards, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("board.List: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("ensureBoardCard created %d cards, want 1", len(cards))
	}
	if cards[0].BotArgs[boardWebhookIDKey] != "wh1" {
		t.Fatalf("the card does not name its integration (%s = %q): %v", boardWebhookIDKey, cards[0].BotArgs[boardWebhookIDKey], cards[0].BotArgs)
	}
	for _, carried := range cards[0].BotArgs {
		if strings.Contains(carried, "providers") {
			t.Errorf("a bot-arg carries the policy's VALUES (%q) — the card names the integration, it does not copy its vocabulary", carried)
		}
	}
	// The admin narrows the integration to stop the openai_compatible spend
	// while the card waits in its column.
	narrowed := cfg
	narrowed.Routing = &llmroute.Policy{
		Providers: []string{"zai"},
		Overrides: []llmroute.Override{{Selector: "judge", Backend: llmroute.HarnessClaw, Model: "zai/glm-4.6"}},
	}
	if err := cs.Update(boundedCtx(t), narrowed); err != nil {
		t.Fatalf("narrow integration wh1: %v", err)
	}

	if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err != nil {
		t.Fatalf("processBoardCard = %v, want nil", err)
	}
	spec := pub.only(t)
	if spec.KeyOverrides["anthropic_api_key"] != "byok-anthropic" {
		t.Fatalf("the launch dropped the integration's BYOK override (%v) — this test only means something while the credential DOES travel", spec.KeyOverrides)
	}
	if spec.LLMRoutePolicy == nil {
		t.Fatal("board dispatch published no routing policy")
	}
	if got := spec.LLMRoutePolicy.Providers; len(got) != 1 || got[0] != "zai" {
		t.Errorf("providers = %v, want the integration's LIVE [zai]: [openai_compatible] would be the team level answering alone, [openai_compatible zai] the list the card froze at creation", got)
	}
	if got := spec.LLMRoutePolicy.Sources[llmroute.FieldProviders]; got != llmroute.SourceWebhook {
		t.Errorf("providers provenance = %q, want %q — the integration level must answer on this lane too", got, llmroute.SourceWebhook)
	}
	if len(spec.LLMRoutePolicy.Overrides) != 1 || spec.LLMRoutePolicy.Overrides[0].Selector != "judge" ||
		spec.LLMRoutePolicy.Overrides[0].Model != "zai/glm-4.6" {
		t.Errorf("overrides = %+v, want the integration's judge cell over the team's", spec.LLMRoutePolicy.Overrides)
	}
	if spec.RetryPolicy == nil || spec.RetryPolicy.MaxAttempts != 2 {
		t.Errorf("retry = %+v, want the integration's max_attempts=2 — the direct lane passes this layer too", spec.RetryPolicy)
	}
	// The reserved key is launch context, never a bot var.
	if _, leaked := spec.Vars[boardWebhookIDKey]; leaked {
		t.Errorf("the reserved %s key leaked into the bot's vars: %v", boardWebhookIDKey, spec.Vars)
	}
}

// TestBoardDispatch_RefusesACardWhoseIntegrationItCannotRead: the card names
// the integration whose routing block locks where its BYOK credential may be
// spent, so an id this launch cannot resolve is a REFUSAL — never an absent
// layer. Folding around it would launch the run with the integration's key
// and none of its lock, which is the hole the carry exists to close
// (ADR-129 §5). The id is member-writable like every bot-arg, so the tenant
// check is the guard: another team's integration is as unreadable as a
// deleted one.
//
// Mutant: the error arm turned into `routeLayers = nil` → every case green
// and a run published, which is exactly the forbidden alternative.
func TestBoardDispatch_RefusesACardWhoseIntegrationItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, s *Server)
		cardW string
		want  string
	}{
		{
			name: "deleted since the card was created",
			seed: func(t *testing.T, s *Server) {
				seedIntegrationConfig(t, s, webhooks.Config{ID: "other", TenantID: "t1"})
			},
			cardW: "wh1",
			want:  "read the launch policy of integration wh1",
		},
		{
			name:  "another team's integration",
			seed:  func(t *testing.T, s *Server) { seedIntegrationConfig(t, s, webhooks.Config{ID: "wh1", TenantID: "t2"}) },
			cardW: "wh1",
			want:  "belongs to another team",
		},
		{
			name:  "no config store wired",
			seed:  func(*testing.T, *Server) {},
			cardW: "wh1",
			want:  "no webhook config store wired",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &tierPublisher{}
			s, _ := newTeamForkServer(t, pub)
			tc.seed(t, s)
			err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
				ID: "native:1", Bot: "probe", State: native.StateReady,
				BotArgs: map[string]string{boardWebhookIDKey: tc.cardW},
			})
			if !errors.Is(err, errCardLaunchRefused) {
				t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — a card whose integration this launch cannot read must not launch under no integration level at all", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %v, want it to name %q", err, tc.want)
			}
			if n := launchCount(pub); n != 0 {
				t.Fatalf("the refused card published %d launches, want 0 — the run must not exist", n)
			}
		})
	}
}

// TestLaunchLanes_PinNoLaunchVarAsOperatorDecided pins a DELIBERATE decision
// of both unpolicied lanes: neither declares TrustedVarNames, so a routing
// field templated on a var a card supplies stays undecided at the gate and a
// non-empty `providers:` allowlist REFUSES the launch (ADR-129 §4 rule 2).
//
// The reason is measured right here: a card's BotArgs are not the operator's
// own `--var`. ensureBoardCard writes PAYLOAD-derived values into them — the
// PR url below, the comment body under a route's args var, a previous run's
// hand-off — and both lanes turn iss.BotArgs straight into spec.Vars. Pinning
// those names would let a forge comment decide which credential a run
// acquires, which is the hole rule 2 exists to close. The operator's own
// launch values reach a run through POST /api/runs (runs_launch.go), which
// pins them because the request IS the operator's gesture.
func TestLaunchLanes_PinNoLaunchVarAsOperatorDecided(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	// The payload-derived card the webhook lane writes. The integration it
	// names carries no routing block, so both lanes resolve the levels below
	// it and this test keeps its one subject: whose vars a card's are.
	integration := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub}
	seedIntegrationConfig(t, s, integration)
	s.ensureBoardCard(boundedCtx(t), integration,
		webhooks.CommandRoute{BotID: "probe", Mode: "board", ArgsVar: "scope_notes"},
		map[string]string{"scope_notes": "please review", "pr_url": "https://forge.example/o/r/pull/7"},
		webhookEventMeta{Kind: "note", SubjectID: "note:1"}, "", "", "")
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("board.List = %v, %v; want one card", cards, err)
	}

	if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err != nil {
		t.Fatalf("processBoardCard = %v, want nil", err)
	}
	board1 := pub.only(t)
	for _, name := range []string{"scope_notes", "pr_url"} {
		if _, carried := board1.Vars[name]; !carried {
			t.Fatalf("the board lane dropped the payload-derived var %q (%v) — the premise of this test is that it carries them", name, board1.Vars)
		}
	}
	if board1.TrustedVarNames != nil {
		t.Errorf("board dispatch pinned %v as operator-decided — a card's bot_args carry payload-derived values (scope_notes is the comment body), so pinning them lets the payload narrow the routing gate", board1.TrustedVarNames)
	}

	// The ticket lane reads the SAME cloud board store (cloudBoardResolve →
	// CloudBoardFor), so the card above is launchable through it too.
	pub2 := &tierPublisher{}
	s2, _ := newTeamForkServer(t, pub2)
	s2.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	seedIntegrationConfig(t, s2, integration)
	if _, err := s2.launchTicketNow(boundedCtx(t), "t1", s2.runs, board, cards[0]); err != nil {
		t.Fatalf("launchTicketNow = %v, want nil", err)
	}
	ticket := pub2.only(t)
	if _, carried := ticket.Vars["pr_url"]; !carried {
		t.Fatalf("the ticket lane dropped the payload-derived var (%v)", ticket.Vars)
	}
	if ticket.TrustedVarNames != nil {
		t.Errorf("pipeline admission pinned %v as operator-decided — the same card, and the same payload-derived values", ticket.TrustedVarNames)
	}
}

// TestLaunchLanes_ResolveNoIntegrationLevelForACardThatNamesNone is the
// other half of the reference's contract, and the one the teamless lane's
// own comment rests on: a card that names no integration contributes NO
// layer, so the levels below it answer alone. On the local studio lane
// (teamID "") that means the platform level and the env dials, which is why
// a tenant store cannot stall a local board — and a teamless card that DID
// name an integration is refused, never resolved, so no card-borne level can
// outrank them by accident.
//
// Mutant: cardLaunchPolicyLayers returning a zero webhook layer for an empty
// id → the provenance assertion reds (a per-integration choice nobody made,
// claimed at the integration level).
func TestLaunchLanes_ResolveNoIntegrationLevelForACardThatNamesNone(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	bare, err := board.Create(native.Issue{Title: "typed by hand", State: native.StateReady, Bot: "probe"})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	if _, err := s.launchTicketNow(boundedCtx(t), "", s.runs, board, bare); err != nil {
		t.Fatalf("launchTicketNow (teamless) = %v, want nil — a local board must launch with no tenant level to read", err)
	}
	spec := pub.only(t)
	if spec.LLMRoutePolicy == nil {
		t.Fatal("the teamless lane published no routing policy")
	}
	for _, field := range []string{llmroute.FieldProviders, llmroute.FieldOverrides} {
		if got := spec.LLMRoutePolicy.Sources[field]; got == llmroute.SourceWebhook {
			t.Errorf("%s provenance = %q on a card naming no integration — the platform level and the env dials answer alone there", field, got)
		}
	}
	if len(spec.LLMRoutePolicy.Providers) != 0 {
		t.Errorf("providers = %v, want none: no level in this fixture sets an allowlist", spec.LLMRoutePolicy.Providers)
	}

	named, err := board.Create(native.Issue{Title: "names one", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{boardWebhookIDKey: "wh1"}})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	if _, err := s.launchTicketNow(boundedCtx(t), "", s.runs, board, named); !errors.Is(err, errTicketRoutingUnresolved) {
		t.Fatalf("launchTicketNow (teamless, named) = %v, want errTicketRoutingUnresolved — a local card naming an integration resolves nothing and must refuse", err)
	}
}

// TestEnsureBoardCard_WarnsWhenTheConfigHasNoID: the stamp's one failure
// shape. A config with no id cannot be named, so the card's launch would
// resolve no integration level at all — the operator reads why instead of
// losing the integration's lock in silence.
func TestEnsureBoardCard_WarnsWhenTheConfigHasNoID(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	log := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, log)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	s.ensureBoardCard(boundedCtx(t), webhooks.Config{TenantID: "t1", Provider: webhooks.ProviderGitHub,
		Routing: &llmroute.Policy{Providers: []string{"zai"}}},
		webhooks.CommandRoute{BotID: "probe", Mode: "board"},
		map[string]string{"pr_url": "https://forge.example/o/r/pull/7"},
		webhookEventMeta{Kind: "note", SubjectID: "note:1"}, native.StateReady, "", "")
	if got := log.String(); !strings.Contains(got, "names no integration") {
		t.Errorf("warn log = %q, want the card's missing integration named", got)
	}
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("board.List = %v, %v; want one card", cards, err)
	}
	if _, named := cards[0].BotArgs[boardWebhookIDKey]; named {
		t.Errorf("the card names an integration it cannot name: %v", cards[0].BotArgs)
	}
}

// TestLaunchLanes_CarryNoIntegrationVarLayer is the other half of the shape
// both card lanes hand their readers, and the one ADR-129 §7's second
// lane-shape residual rests on: a card's BotArgs are its WHOLE var set, and
// the rule that fills them is by NAME over the already-layered map.
// ensureBoardCard copies the route's args var, the PR carry list and the
// bot's declared PR-scoped hand-offs — so an integration var whose name is
// one of those DOES ride the card (as a card value: member-writable, pinned
// operator-decided by neither lane), and every other one reaches no carded
// run, however the integration's `launch_vars` / `operator_launch_vars` set
// it. Both halves are measured here, because the line between them is what
// the residual and its follow-up are about.
//
// The consequence of the second half, measured on the publisher side by
// TestRoutingParity_TheCardLaneShapeIsItsOwn: a routing field dialled by an
// uncarried var reads from its DECLARED DEFAULT here, so under a non-empty
// `providers:` allowlist a card can be refused where the same integration's
// direct lane launches. Pinned so the residual cannot go quiet — the day
// these lanes hand the run the integration's var layers, this witness reds
// and §7's residual goes with it.
//
// Mutant: ensureBoardCard carrying every var it is handed instead of its
// list → the operator's var rides the card and the assertions red, which is
// the forbidden alternative (the card as the var carrier, frozen at carding
// time against the admin's later edit). The other direction — the carry list
// filtered against the integration's own layers, which is the absolute
// reading "no integration var rides a card" — reds the carried-name half.
// And applyWebhookVarLayers' `mergeVarsInto` turned to `fillVarGaps`, the
// forbidden alternative for the LAYERING (a repo's `gate_context` losing to
// whatever a payload stamped) → the carried-name half reds on both lanes,
// because the base below carries the payload's own branch names.
func TestLaunchLanes_CarryNoIntegrationVarLayer(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	// An integration pinning two vars at each of its two levels: one name the
	// carry list holds and one it does not, per level. `target_branch` is the
	// carried name on the operator layer — the level the forge orchestrator
	// provisions (`gate_context`, which the list does NOT hold, rides it too)
	// — and `revi_model` / `revi_backend` are the shape a routing field dials.
	integration := webhooks.Config{
		ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub,
		LaunchVars:         map[string]string{"revi_backend": "claw", "base_ref": "integration-base"},
		OperatorLaunchVars: map[string]string{"revi_model": "openai_compatible/gw-1/m", "target_branch": "integration-target"},
	}
	seedIntegrationConfig(t, s, integration)
	// The vars a webhook handler hands ensureBoardCard are already LAYERED
	// (applyWebhookVarLayers): the premise is what the card keeps of them,
	// not what the handler was given.
	//
	// The base carries the payload's OWN base_ref / target_branch, because
	// on the lane this witness simulates the handler always stamps them
	// first (buildCommandVars, buildPRForgeCommandVars) — so the carried
	// NAMES reach ensureBoardCard through a COLLISION, and asserting the
	// integration's value on them pins the direction of the layering
	// (mergeVarsInto, "layer OVER") and not merely the carry list. With a
	// base that leaves those names free, the same assertions pass whichever
	// way the two webhook levels layer.
	handlerVars := applyWebhookVarLayers(map[string]string{
		"pr_url":        "https://forge.example/o/r/pull/7",
		"base_ref":      "payload-base",
		"target_branch": "payload-target",
	}, integration)
	for _, name := range []string{"revi_backend", "revi_model", "base_ref", "target_branch"} {
		if handlerVars[name] == "" {
			t.Fatalf("precondition: the direct lane's vars carry %q (%v) — without that this test compares nothing", name, handlerVars)
		}
	}
	// The two names the carry list holds, with the integration level each
	// comes from — the half that DOES ride, asserted on its value so a card
	// carrying the name with someone else's value would not pass for it.
	carriedFromIntegration := map[string]string{"base_ref": "integration-base", "target_branch": "integration-target"}
	// The direct lane's own reading of the collision, before any card: each
	// level layers OVER the handler's base, so the integration's value is
	// what every lane starts from. A lane that layered UNDER would keep the
	// payload's branch names here, and every assertion below would then be
	// comparing the card against a different premise.
	for name, want := range carriedFromIntegration {
		if got := handlerVars[name]; got != want {
			t.Fatalf("precondition: the direct lane reads %q = %q, want %q — the webhook levels layer OVER the handler's base (applyWebhookVarLayers)", name, got, want)
		}
	}
	s.ensureBoardCard(boundedCtx(t), integration, webhooks.CommandRoute{BotID: "probe", Mode: "board"},
		handlerVars, webhookEventMeta{Kind: "note", SubjectID: "note:1"}, "", "", "")
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("board.List = %v, %v; want one card", cards, err)
	}
	if cards[0].BotArgs["pr_url"] == "" {
		t.Fatalf("the card dropped the carried PR var (%v) — the premise is a SELECTIVE carry, not an empty one", cards[0].BotArgs)
	}
	for name, want := range carriedFromIntegration {
		if got := cards[0].BotArgs[name]; got != want {
			t.Errorf("the card keeps %q = %q, want %q — the carry is by NAME over the layered map, so an integration var the list holds rides it (§7's rule, and the half its follow-up has to answer for)", name, got, want)
		}
	}
	for _, name := range []string{"revi_backend", "revi_model"} {
		if _, carried := cards[0].BotArgs[name]; carried {
			t.Errorf("the card carries the integration's %q var (%v) — ADR-129 §7's residual says a name outside the carry list does not ride; if the carry became deliberate, that residual is closed and this witness is the one to rewrite", name, cards[0].BotArgs)
		}
	}

	if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err != nil {
		t.Fatalf("processBoardCard = %v, want nil", err)
	}
	// Both lanes read the same cloud board store, so the same card measures
	// both shapes.
	pub2 := &tierPublisher{}
	s2, _ := newTeamForkServer(t, pub2)
	s2.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	seedIntegrationConfig(t, s2, integration)
	if _, err := s2.launchTicketNow(boundedCtx(t), "t1", s2.runs, board, cards[0]); err != nil {
		t.Fatalf("launchTicketNow = %v, want nil", err)
	}
	for lane, spec := range map[string]runview.LaunchSpec{"board dispatch": pub.only(t), "pipeline admission": pub2.only(t)} {
		if spec.Vars["pr_url"] == "" {
			t.Fatalf("%s: the launch dropped the carried PR var (%v)", lane, spec.Vars)
		}
		for name, want := range carriedFromIntegration {
			if got := spec.Vars[name]; got != want {
				t.Errorf("%s: the launch reads %q = %q, want %q — a carried name reaches the run with the integration's value, from the card", lane, name, got, want)
			}
		}
		for _, name := range []string{"revi_backend", "revi_model"} {
			if _, reached := spec.Vars[name]; reached {
				t.Errorf("%s: the integration's %q var reached the run (%v) — a routing field it dials would then be readable here, which §7 says it is not", lane, name, spec.Vars)
			}
		}
	}
}

// TestBoardDispatch_ACardMayNameASiblingIntegration measures the bounded
// residual ADR-129's Residuals paragraph names: the id is member-writable, so
// a rewrite can SWAP it for a sibling's rather than drop it. The card's two
// halves are independent bot-args, so integration A's BYOK blob rides under
// integration B's lock — B's `providers:` replaces the team's own list (§2:
// first-setter-wins, the webhook level above the team level) — and the run
// document records the LEVEL, never which integration set it.
//
// Both integrations belong to the card's team, so this is the residual, not
// the refusal: another team's id, or one deleted since, is refused
// (TestBoardDispatch_RefusesACardWhoseIntegrationItCannotRead). It stays a
// residual because the same member rung already sends an arbitrary
// `llm_routing` to POST /api/runs as the more specific `run` level, and
// because a team that must forbid it locks `providers` at its own level,
// which vetoes both.
//
// Mutant: liftBoardLaunchContext ignoring boardWebhookIDKey → no webhook
// level is resolved, the provenance assertion reds, and the swap stops being
// expressible at all.
func TestBoardDispatch_ACardMayNameASiblingIntegration(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedTeamRoutingCell(t, s, "t1") // the team's own lock: [openai_compatible]
	seedIntegrationConfig(t, s, webhooks.Config{
		ID: "wh-a", TenantID: "t1", Provider: webhooks.ProviderGitHub,
		KeyOverrides: map[string]string{"anthropic_api_key": "byok-anthropic-of-A"},
		Routing:      &llmroute.Policy{Providers: []string{"openai_compatible"}},
	})
	seedIntegrationConfig(t, s, webhooks.Config{
		ID: "wh-b", TenantID: "t1", Provider: webhooks.ProviderGitHub,
		Routing: &llmroute.Policy{Providers: []string{"anthropic", "zai"}},
	})
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	// What a board-task write can leave on a card: A's override blob, B's id.
	card, err := board.Create(native.Issue{Title: "mixed", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{
			boardKeyOverridesKey: `{"anthropic_api_key":"byok-anthropic-of-A"}`,
			boardWebhookIDKey:    "wh-b",
		}})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	if err := s.processBoardCard(boundedCtx(t), "t1", *card); err != nil {
		t.Fatalf("processBoardCard = %v, want nil — a sibling integration of the same team is readable here", err)
	}
	spec := pub.only(t)
	if spec.KeyOverrides["anthropic_api_key"] != "byok-anthropic-of-A" {
		t.Fatalf("the launch dropped the carried BYOK override (%v) — this witness only means something while the two halves DO come apart", spec.KeyOverrides)
	}
	if spec.LLMRoutePolicy == nil {
		t.Fatal("board dispatch published no routing policy")
	}
	if got := spec.LLMRoutePolicy.Providers; len(got) != 2 || got[0] != "anthropic" || got[1] != "zai" {
		t.Errorf("providers = %v, want the named integration's [anthropic zai]: [openai_compatible] would mean the id decided nothing", got)
	}
	if got := spec.LLMRoutePolicy.Sources[llmroute.FieldProviders]; got != llmroute.SourceWebhook {
		t.Errorf("providers provenance = %q, want %q — and the level is all the run document records: which integration set it is answerable only from the card's bot_args", got, llmroute.SourceWebhook)
	}
}
