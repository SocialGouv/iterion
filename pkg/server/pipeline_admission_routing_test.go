package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The pipeline admission lane — the board's Ready sweep and the operator's
// "launch now" drag share this body — resolves the run's routing policy and
// its retry contract like every other launch lane. Before, a ticket run
// carried neither: the tenant's override cells, its provider allowlist and
// the bot's own retry directive were all absent from the run doc, on the
// one lane a studio operator uses by hand.
//
// Mutants: the resolver call removed → the policy assertions red; its error
// folded around → the refusal assertion red.

// newTicketLaneFixture is the admission lane's harness: the team-fork
// server (catalog `probe`, a cloud-shaped team `t1`) plus a native board
// holding one Ready ticket for that bot.
func newTicketLaneFixture(t *testing.T) (*Server, *tierPublisher, native.BoardStore, *native.Issue) {
	t.Helper()
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	iss, err := board.Create(native.Issue{Title: "ticket", State: native.StateReady, Bot: "probe"})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	return s, pub, board, iss
}

// TestPipelineAdmission_ResolvesRoutingPolicy: a team routing cell reaches
// the run through the ticket lane.
func TestPipelineAdmission_ResolvesRoutingPolicy(t *testing.T) {
	s, pub, board, iss := newTicketLaneFixture(t)
	seedTeamRoutingCell(t, s, "t1")

	if _, err := s.launchTicketNow(boundedCtx(t), "t1", s.runs, board, iss); err != nil {
		t.Fatalf("launchTicketNow = %v, want nil", err)
	}
	spec := pub.only(t)
	assertCarriesTheTeamCell(t, "pipeline admission", spec.LLMRoutePolicy, spec.RetryPolicy)
}

// TestPipelineAdmission_RefusesWhenTheTenantPolicyIsUnreadable: a read
// failure on a tenant routing record refuses the ticket launch, BEFORE the
// board claim — the ticket keeps its column, nothing is published, and the
// operator reads which level could not be read. A fold-around would run the
// ticket outside the tenant's provider allowlist on a store blip.
func TestPipelineAdmission_RefusesWhenTheTenantPolicyIsUnreadable(t *testing.T) {
	s, pub, board, iss := newTicketLaneFixture(t)
	s.routingPolicyStoreFor = func(string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		return failingRoutePolicyStore{err: errors.New("mongo wedged")}
	}

	_, err := s.launchTicketNow(boundedCtx(t), "t1", s.runs, board, iss)
	if err == nil {
		t.Fatal("launchTicketNow = nil, want a refusal — an unreadable tenant routing record must never launch under a policy nobody could read")
	}
	if !strings.Contains(err.Error(), "team routing policy") {
		t.Errorf("refusal = %v, want it to name the level that could not be read", err)
	}
	// The ticket travels ON the refusal, like every other exit of this body:
	// an operator reading a 500-card board needs the id, and the admission
	// loop logs one line per tick.
	if !strings.Contains(err.Error(), iss.ID) {
		t.Errorf("refusal = %v, want it to name ticket %s", err, iss.ID)
	}
	// And the CLASS travels as a sentinel, so the launch-now route can pick
	// a status instead of defaulting to 409 Conflict on a store outage.
	if !errors.Is(err, errTicketRoutingUnresolved) {
		t.Errorf("refusal %v does not carry errTicketRoutingUnresolved — the handler cannot tell this outage from a conflict on the card", err)
	}
	// The refusal carries no started-run fact, which is what lets the
	// operator's "launch now" hand its metered slot back: a monthly slot
	// spent on a run that never existed is the fact travelling wrong.
	if runview.RunMayHaveStarted(err) {
		t.Errorf("refusal %v reads as a run that may have started — the launch-now route would keep the metered slot for a ticket nothing launched", err)
	}
	if n := launchCount(pub); n != 0 {
		t.Fatalf("the refused ticket published %d launches, want 0 — the run must not exist", n)
	}
	cur, getErr := board.Get(iss.ID)
	if getErr != nil {
		t.Fatalf("board.Get: %v", getErr)
	}
	if cur.State != native.StateReady {
		t.Errorf("ticket state = %q, want %q — the refusal precedes the claim, so the card keeps its column and the next tick retries it", cur.State, native.StateReady)
	}
}

// TestPipelineBoardLaunchAnswers503OnARoutingOutage: the operator's
// "launch now" over HTTP. A tenant routing record this server cannot read is
// an OUTAGE — the card is fine, nothing conflicts — so the route answers 503
// and names the ticket, instead of the 409 Conflict its default would send
// an operator to go and edit a card nothing is wrong with.
//
// Mutant: the errTicketRoutingUnresolved arm removed from
// handlePipelineBoardTaskLaunch → the status assertion reds.
func TestPipelineBoardLaunchAnswers503OnARoutingOutage(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	s.routingPolicyStoreFor = func(string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		return failingRoutePolicyStore{err: errors.New("mongo wedged")}
	}
	card, err := board.Create(native.Issue{Title: "launch me", State: native.StateReady, Bot: "probe"})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", strings.NewReader(`{}`))
	req.SetPathValue("id", card.ID)
	req = req.WithContext(auth.WithIdentity(req.Context(),
		auth.Identity{UserID: "u1", TeamID: "t1", OrgID: "t1", Role: identity.RoleMember}))
	rec := newServerErrorRecorder(s)
	s.handlePipelineBoardTaskLaunch(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("launch-now = %d %s, want 503 — a wedged tenant routing store is this server's outage, not a conflict on the card", rec.Code, rec.Diagnostic(t))
	}
	if !strings.Contains(rec.Diagnostic(t), card.ID) {
		t.Errorf("response %s names no ticket", rec.Diagnostic(t))
	}
	if n := launchCount(pub); n != 0 {
		t.Fatalf("the refused ticket published %d launches, want 0", n)
	}
}

// TestPipelineAdmission_CarriesTheIntegrationsRoutingCell: the ticket lane
// launches the SAME cloud board the coordinator does, so an operator's
// "launch now" on a webhook-materialised card owes the same answer — the
// integration's routing block, its `providers:` lock included. A lane that
// resolved only the tenant levels would apply the integration's BYOK key
// override (on the lane that does) while dropping its lock on where that
// credential may be spent.
//
// Mutant: the layers not passed (the cardLaunchPolicyLayers call) → the
// provenance assertion reds; the read answered from the card instead of the
// record → the narrowing assertion reds. The reserved keys never reach the
// bot as vars.
func TestPipelineAdmission_CarriesTheIntegrationsRoutingCell(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	seedTeamRoutingCell(t, s, "t1")
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	cfg := webhooks.Config{
		ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub,
		Routing:          &llmroute.Policy{Providers: []string{"openai_compatible", "zai"}},
		RetryMaxAttempts: 2,
	}
	cs := seedIntegrationConfig(t, s, cfg)
	s.ensureBoardCard(boundedCtx(t), cfg, webhooks.CommandRoute{BotID: "probe", Mode: "board"},
		map[string]string{"pr_url": "https://forge.example/o/r/pull/7"},
		webhookEventMeta{Kind: "note", SubjectID: "note:1"}, native.StateReady, "https://forge.example/o/r.git", "main")
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("board.List = %v, %v; want one card", cards, err)
	}
	// The admin narrows the integration while the card waits: this lane reads
	// the record, so the hand launch spends the narrowed vocabulary.
	narrowed := cfg
	narrowed.Routing = &llmroute.Policy{Providers: []string{"zai"}}
	if err := cs.Update(boundedCtx(t), narrowed); err != nil {
		t.Fatalf("narrow integration wh1: %v", err)
	}

	if _, err := s.launchTicketNow(boundedCtx(t), "t1", s.runs, board, cards[0]); err != nil {
		t.Fatalf("launchTicketNow = %v, want nil", err)
	}
	spec := pub.only(t)
	if spec.LLMRoutePolicy == nil {
		t.Fatal("the ticket lane published no routing policy")
	}
	if got := spec.LLMRoutePolicy.Providers; len(got) != 1 || got[0] != "zai" {
		t.Errorf("providers = %v, want the integration's LIVE [zai]: [openai_compatible] would be the team level answering alone, [openai_compatible zai] the list the card froze at creation", got)
	}
	if got := spec.LLMRoutePolicy.Sources[llmroute.FieldProviders]; got != llmroute.SourceWebhook {
		t.Errorf("providers provenance = %q, want %q — the integration level must answer on this lane too", got, llmroute.SourceWebhook)
	}
	if spec.RetryPolicy == nil || spec.RetryPolicy.MaxAttempts != 2 {
		t.Errorf("retry = %+v, want the integration's max_attempts=2", spec.RetryPolicy)
	}
	// The reserved keys are launch context on this lane as well: a bot that
	// received them as vars would read a JSON blob as its own input.
	for _, reserved := range []string{boardRepoURLKey, boardRepoRefKey, boardProjectPathKey, boardKeyOverridesKey, boardSecretOverridesKey, boardWebhookIDKey} {
		if _, leaked := spec.Vars[reserved]; leaked {
			t.Errorf("the reserved %s key reached the bot as a var: %v", reserved, spec.Vars)
		}
	}
	if _, carried := spec.Vars["pr_url"]; !carried {
		t.Errorf("the lift dropped the bot's own var: %v", spec.Vars)
	}
	// A malformed override blob REFUSES, naming the ticket and the key —
	// silently dropping it would launch without the integration's BYOK key.
	bad, err := board.Create(native.Issue{Title: "bad", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{boardKeyOverridesKey: "{not json"}})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	_, err = s.launchTicketNow(boundedCtx(t), "t1", s.runs, board, bad)
	if err == nil {
		t.Fatal("launchTicketNow accepted a card carrying a malformed override blob")
	}
	if !strings.Contains(err.Error(), boardKeyOverridesKey) || !strings.Contains(err.Error(), bad.ID) {
		t.Errorf("refusal = %v, want it to name the ticket and the offending bot-arg", err)
	}
}

// TestPipelineAdmission_RefusesACardWhoseIntegrationItCannotRead is the
// ticket lane's half of the same doctrine: an operator's "launch now" on a
// card naming an integration this team cannot read is REFUSED, under the
// error class handlePipelineBoardTaskLaunch maps to 503 (a read it may be
// able to retry), and no run is published. Another team's integration is as
// unreadable as a deleted one — the id rides in a member-writable bot-arg.
//
// Mutant: the error arm turned into `routeLayers = nil` → a run published
// with no integration level, the forbidden alternative.
func TestPipelineAdmission_RefusesACardWhoseIntegrationItCannotRead(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	seedIntegrationConfig(t, s, webhooks.Config{ID: "wh1", TenantID: "t2"})
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	card, err := board.Create(native.Issue{Title: "x", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{boardWebhookIDKey: "wh1"}})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	_, err = s.launchTicketNow(boundedCtx(t), "t1", s.runs, board, card)
	if !errors.Is(err, errTicketRoutingUnresolved) {
		t.Fatalf("launchTicketNow = %v, want errTicketRoutingUnresolved — a routing contract this lane cannot resolve refuses the launch", err)
	}
	if !strings.Contains(err.Error(), "belongs to another team") || !strings.Contains(err.Error(), card.ID) {
		t.Errorf("refusal = %v, want it to name the ticket and why the integration is unreadable", err)
	}
	if n := launchCount(pub); n != 0 {
		t.Fatalf("the refused ticket published %d launches, want 0", n)
	}
}

// TestPipelineBoardCard_CarriesNoRoutingPolicyValue drives the PUBLIC door:
// POST /api/v1/pipeline-board/tasks as a plain MEMBER — the rung
// boardWriteAuth takes — with a routing policy typed into bot_args, then the
// operator's "launch now" on that card. The write path strips only the
// server-minted class (store.DropServerMintedVars), so what keeps a member's
// own vocabulary out of the run's policy is that NO lane reads a policy value
// off a card: the card names an integration or nothing at all. The team's
// level answers, the typed text rides as an inert var, and the `webhook`
// provenance on the run doc — the operator-facing "which level locked this",
// read into ProviderOutsideError.Level — stays a true statement.
//
// Mutant: a lane that folds a card-borne policy as a webhook layer → the run
// carries [zai anthropic] under provenance "webhook", a member's list
// recorded as the integration's.
func TestPipelineBoardCard_CarriesNoRoutingPolicyValue(t *testing.T) {
	env := newPipelineTierEnv(t)
	seedTeamRoutingCell(t, env.srv, "t1")
	env.seedRow(t, "t1", "probe", tierForkBot)
	const typed = `{"routing":{"providers":["zai","anthropic"],` +
		`"overrides":[{"selector":"judge","backend":"claw","model":"anthropic/claude-opus-5-5","provider":"anthropic"}]}}`

	card := env.createCard(t, `{"bot":"probe","title":"ungoverned","bot_args":`+
		`{"__iterion_launch_policy":`+quoteJSON(typed)+`,"launch_policy":`+quoteJSON(typed)+`}}`)
	if card.BotArgs["launch_policy"] != typed {
		t.Fatalf("the create path stripped the member's bot_args (%v) — this test only means something while the door is open", card.BotArgs)
	}
	env.launchCard(t, card.ID)
	spec := env.pub.only(t)
	if spec.LLMRoutePolicy == nil {
		t.Fatal("the launch published no routing policy")
	}
	if got := spec.LLMRoutePolicy.Providers; len(got) != 1 || got[0] != "openai_compatible" {
		t.Errorf("providers = %v, want the TEAM's [openai_compatible]: a member's typed list must not become the run's vocabulary", got)
	}
	if got := spec.LLMRoutePolicy.Sources[llmroute.FieldProviders]; got != llmroute.SourceTeam {
		t.Errorf("providers provenance = %q, want %q — a level that did not decide must not be named on the run doc", got, llmroute.SourceTeam)
	}
	for _, o := range spec.LLMRoutePolicy.Overrides {
		if o.Provider == "anthropic" {
			t.Errorf("the member's typed override reached the run: %+v", spec.LLMRoutePolicy.Overrides)
		}
	}
}

// TestPipelineBoardLaunchAnswers503WhenTheCardNamesAnotherTeamsIntegration:
// the launch-side tenant check is what makes the `webhook` level true — the
// id a card carries is launch context the write routes strip from a client
// (native.DropReservedLaunchArgs), and this door is the belt behind that:
// a card carrying an id its team cannot read — written before the filter
// existed, or by any non-route writer — refuses with the outage status (the
// card is not at fault) and publishes nothing. Never the other team's
// routing block, never an absent integration level.
//
// Mutant: the cfg.TenantID check dropped → 202 and a run carrying the other
// team's providers.
func TestPipelineBoardLaunchAnswers503WhenTheCardNamesAnotherTeamsIntegration(t *testing.T) {
	env := newPipelineTierEnv(t)
	env.seedRow(t, "t1", "probe", tierForkBot)
	seedIntegrationConfig(t, env.srv, webhooks.Config{
		ID: "wh-t2", TenantID: "t2", Provider: webhooks.ProviderGitHub,
		Routing: &llmroute.Policy{Providers: []string{"anthropic"}},
	})

	// Stamped the way the server stamps (store-side), not through the write
	// route: the route strips the id from a client; the launch must still
	// refuse a card that carries an unreadable one.
	card, err := env.board.Create(native.Issue{
		Title: "borrowed", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{boardWebhookIDKey: "wh-t2"},
	})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	req := env.req(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", "")
	req.SetPathValue("id", card.ID)
	rec := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskLaunch(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("launch-now = %d %s, want 503 — a card naming an integration this team cannot read must not launch", rec.Code, rec.Body.String())
	}
	if n := launchCount(env.pub); n != 0 {
		t.Fatalf("the refused card published %d launches, want 0", n)
	}
}

// quoteJSON renders s as a JSON string so a fixture can embed a blob in a
// request body without hand-escaping it.
func quoteJSON(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
