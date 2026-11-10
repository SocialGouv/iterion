package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The dispatch grant's bindings (#3130, review round 1): the workflow the
// schedule row names, the bundle the tick launches, the grant itself as the
// unit of "once", the run's liveness, the request's exact shape, the typed
// answer of every refusal, and the key namespace that keeps an older build
// from honouring a grant it cannot read the binding of. Each witness drives
// the production mint and the production handler through the full mux.

// postDispatch POSTs a raw body to the dispatch endpoint through the full
// handler and decodes whatever JSON it answers.
func (w *dispatchWorld) postDispatch(t *testing.T, token, body string) (int, map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, forgeDispatchWorkflowPath, strings.NewReader(body))
	if token != "" {
		r.Header.Set("X-Iterion-Run", token)
	}
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.s.Handler().ServeHTTP(rec, r)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// The grant dispatches the workflow the schedule row names and no other: a
// run steered to another workflow_dispatch file of the repository (a deploy,
// a release) is refused typed, before any forge call.
//
// Mutation witnessed: the handler's binding comparison removed → lint.yml,
// which the fake forge would dispatch, goes through → red.
func TestWorkflowDispatch_TheGrantDispatchesOnlyTheScheduledWorkflow(t *testing.T) {
	w := newDispatchWorld(t)
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-bound", store.RunTrustDefault)
	code, out, raw := w.dispatch(t, token, "run-bound", "acme/widgets", "lint.yml")
	if code != http.StatusForbidden || out.Code != dispatchRefusedWorkflow || out.Dispatched {
		t.Fatalf("dispatch of a workflow the grant is not bound to = %d %s, want 403 %s", code, raw, dispatchRefusedWorkflow)
	}
	w.assertForgeUntouched(t)
	if c := w.claims(t, token); len(c) != 0 {
		t.Errorf("a refused workflow left claims %v on the grant", c)
	}
}

// A grant carrying the capability with no workflow bound dispatches nothing:
// the binding is the grant's, not a default.
//
// Mutation witnessed: the empty-binding half of the comparison removed (an
// unbound grant accepted for any file) → renovate.yml dispatches → red.
func TestWorkflowDispatch_AGrantBoundToNoWorkflowDispatchesNothing(t *testing.T) {
	w := newDispatchWorld(t)
	const token = "tok-unbound-workflow"
	if err := w.s.forgePublishTokens.Register(token, ForgePublishGrant{
		TeamID: "team1", ConnectionID: "conn1", Repo: "acme/widgets", Bot: "workflow-kick",
		Capabilities: []string{publishCapDispatchWorkflow}, NoPullRequest: true,
	}); err != nil {
		t.Fatal(err)
	}
	w.holdRun(t, "run-unbound", map[string]string{forgePublishVarToken: token}, store.RunTrustDefault)
	code, out, raw := w.dispatch(t, token, "run-unbound", "acme/widgets", "renovate.yml")
	if code != http.StatusForbidden || out.Code != dispatchRefusedWorkflow {
		t.Fatalf("dispatch with a grant bound to no workflow = %d %s, want 403 %s", code, raw, dispatchRefusedWorkflow)
	}
	w.assertForgeUntouched(t)
}

// A schedule row naming no workflow mints nothing: there is nothing the grant
// could be bound to, and the bot ends NO_WORKFLOW on its own.
//
// Mutation witnessed: the empty-workflow branch of scheduledDispatchGrant
// removed → a grant bound to "" is minted → red.
func TestScheduledDispatchGrant_AScheduleNamingNoWorkflowMintsNothing(t *testing.T) {
	w := newDispatchWorld(t)
	for _, vars := range []map[string]string{nil, {"workflow": "  "}} {
		out, minted, err := w.tick(t, "workflow-kick", "sch-noflow", vars)
		if err != nil || minted != "" {
			t.Fatalf("schedule naming no workflow (vars %v) = (%q, %v), want no grant and no error", vars, minted, err)
		}
		if _, ok := out[forgePublishVarDispatch]; ok {
			t.Fatalf("schedule naming no workflow handed the run %s", forgePublishVarDispatch)
		}
	}
}

// A run that is over still holds its (token, run_id) pair for as long as the
// grant lives — the post-run grace, a fork's share. It dispatches nothing.
//
// Mutation witnessed: the runIsOver check removed → the finished run
// dispatches → red.
func TestWorkflowDispatch_ARunThatIsOverDispatchesNothing(t *testing.T) {
	w := newDispatchWorld(t)
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-over", store.RunTrustDefault)
	run, err := w.runs.LoadRun(context.Background(), "run-over")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFinished
	if err := w.runs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	code, out, raw := w.dispatch(t, token, "run-over", "acme/widgets", "renovate.yml")
	if code != http.StatusForbidden || out.Code != dispatchRefusedRunOver || out.Dispatched {
		t.Fatalf("dispatch by a finished run = %d %s, want 403 %s", code, raw, dispatchRefusedRunOver)
	}
	w.assertForgeUntouched(t)
	if c := w.claims(t, token); len(c) != 0 {
		t.Errorf("a finished run left claims %v on the grant", c)
	}
}

// Decision 3: the default branch, no inputs. A body naming a ref — one the
// fake forge HOLDS, so a forwarded ref would visibly dispatch on it — or
// inputs is refused typed, never silently dropped, and nothing reaches the
// forge.
//
// Mutation witnessed: DisallowUnknownFields removed → the body decodes, the
// ref is dropped and main is dispatched → red on the code and the untouched
// forge.
func TestWorkflowDispatch_ABodyNamingARefOrInputsIsRefused(t *testing.T) {
	for name, extra := range map[string]string{
		"ref":    `"ref":"feat/pushed-by-run"`,
		"inputs": `"inputs":{"target":"production"}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := newDispatchWorld(t)
			w.f.mu.Lock()
			w.f.branches["feat/pushed-by-run"] = true
			w.f.mu.Unlock()
			token, _ := w.scheduleGrant(t, "workflow-kick", "run-ref", store.RunTrustDefault)
			body := `{"run_id":"run-ref","repo":"acme/widgets","workflow":"renovate.yml",` + extra + `}`
			code, out, raw := w.postDispatch(t, token, body)
			if code != http.StatusBadRequest || out["code"] != dispatchRefusedBadRequest {
				t.Fatalf("dispatch naming %s = %d %s, want 400 %s", name, code, raw, dispatchRefusedBadRequest)
			}
			w.assertForgeUntouched(t)
		})
	}
}

// The pull-request lane never mints the dispatch: a member-reachable launch
// (a board card, an API launch with pr_url) of a bot declaring `actions` gets
// a grant with no dispatch capability, no dispatch endpoint — and one the
// caller planted in the vars is removed, since the bearer is scoped toward
// every endpoint var — and its dispatch is refused.
//
// Mutations witnessed: pullRequestGrantCapabilitiesFor appending the dispatch
// for a manifest declaring it → the PR grant dispatches → red;
// registerRunGrant naming the dispatch endpoint whatever the capabilities →
// red on the vars; the delete of a planted endpoint removed → the planted URL
// survives → red.
func TestWorkflowDispatch_APullRequestLaunchHoldsNoDispatch(t *testing.T) {
	w := newDispatchWorld(t)
	vars, minted, err := w.s.injectForgePublishVars(context.Background(), "team1", "conn1", "workflow-kick",
		map[string]string{
			"pr_url":                w.f.srv.URL + "/acme/widgets/pull/7",
			"workflow":              "lint.yml",
			forgePublishVarDispatch: "https://attacker.example/collect",
		}, nil, store.RunTrustDefault)
	if err != nil || minted == "" {
		t.Fatalf("PR grant for workflow-kick = (%q, %v), want one minted — the fixture must hold a grant to be refused", minted, err)
	}
	g, _ := w.s.forgePublishTokens.lookup(string(minted))
	if len(g.Capabilities) != 0 || g.DispatchWorkflow != "" {
		t.Fatalf("a pull-request grant of workflow-kick = %+v, want no capability and no workflow bound", g)
	}
	if v, ok := vars[forgePublishVarDispatch]; ok {
		t.Fatalf("a pull-request launch carries %s=%q — only a schedule's dispatch grant names it", forgePublishVarDispatch, v)
	}
	w.holdRun(t, "run-pr-kick", vars, store.RunTrustDefault)
	code, out, raw := w.dispatch(t, string(minted), "run-pr-kick", "acme/widgets", "lint.yml")
	if code != http.StatusForbidden || out.Code != dispatchRefusedNoCapability {
		t.Fatalf("dispatch with a pull-request grant = %d %s, want 403 %s", code, raw, dispatchRefusedNoCapability)
	}
	w.assertForgeUntouched(t)
}

// shadowRow stores team1's own bundle under slug — main.bot only, no
// manifest, the shape BotSource.Validate accepts and that wins resolution.
func shadowRow(t *testing.T, s *Server, tenant, slug string) {
	t.Helper()
	if s.botSources == nil {
		s.botSources = botsource.NewMemoryStore()
	}
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), tenant), botsource.BotSource{
		TenantID: tenant, Slug: slug,
		Files: map[string]string{botsource.MainBotFile: spineProbeBot},
	}); err != nil {
		t.Fatal(err)
	}
}

// The pull-request lane reads the merge-gate gestures from the bundle the
// launch runs: a team row shadowing the catalogue's gitops-warden without a
// manifest declares nothing, so its grant carries no gesture.
//
// Mutation witnessed: pullRequestGrantCapabilitiesFor reading botManifestFor
// (the team manifest, else the platform/catalogue one) → the shadow inherits
// approve/merge/reviewers → red.
func TestPullRequestGrant_TheGesturesAreTheRunningBundles(t *testing.T) {
	w := newDispatchWorld(t)
	prVars := func() map[string]string { return map[string]string{"pr_url": w.f.srv.URL + "/acme/widgets/pull/7"} }
	before := w.s.pullRequestGrantCapabilitiesFor(context.Background(), "team1", "gitops-warden")
	if !slices.Equal(before, []string{publishCapApprove, publishCapMerge, publishCapReviewers}) {
		t.Fatalf("the catalogue's gitops-warden mints %v, want its three gestures — the fixture must have something to lose", before)
	}
	shadowRow(t, w.s, "team1", "gitops-warden")
	_, minted, err := w.s.injectForgePublishVars(context.Background(), "team1", "conn1", "gitops-warden", prVars(), nil, store.RunTrustDefault)
	if err != nil || minted == "" {
		t.Fatalf("PR grant = (%q, %v), want one minted", minted, err)
	}
	if g, _ := w.s.forgePublishTokens.lookup(string(minted)); len(g.Capabilities) != 0 {
		t.Fatalf("a team bundle declaring nothing was minted %v — capabilities of a catalogue manifest it shadows", g.Capabilities)
	}
}

// failingTeamRows is a bot-source store whose team tier cannot answer.
type failingTeamRows struct {
	*botsource.MemoryStore
	tenant string
}

func (f failingTeamRows) GetBySlug(ctx context.Context, tenantID, slug string) (botsource.BotSource, error) {
	if tenantID == f.tenant {
		return botsource.BotSource{}, errors.New("mongo: connection reset")
	}
	return f.MemoryStore.GetBySlug(ctx, tenantID, slug)
}

// A team tier that cannot answer says nothing about which bundle runs: the
// grant carries no gesture rather than the catalogue manifest's.
//
// Mutation witnessed: resolveBotTier treating a team-tier error as "no row"
// (falling through to the next tier) → the grant carries the catalogue
// warden's gestures → red.
func TestPullRequestGrant_AStoreThatCannotAnswerMintsNoGesture(t *testing.T) {
	w := newDispatchWorld(t)
	w.s.botSources = failingTeamRows{MemoryStore: botsource.NewMemoryStore(), tenant: "team1"}
	if caps := w.s.pullRequestGrantCapabilitiesFor(context.Background(), "team1", "gitops-warden"); len(caps) != 0 {
		t.Fatalf("capabilities over a team tier that cannot answer = %v, want none", caps)
	}
}

// scheduledKickServer is a cloud-shaped server whose catalogue holds the
// given bundles, launching through a recording publisher, with team t1's
// GitHub connection c1 and its integration i1 on acme/widgets.
func scheduledKickServer(t *testing.T, bundles map[string]map[string]string) (*Server, func(botID string) runview.LaunchSpec) {
	t.Helper()
	pub := &spinePublisher{}
	s := newForgeTestServer(t)
	seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	for slug, files := range bundles {
		if err := os.MkdirAll(filepath.Join(botsDir, slug), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(botsDir, slug, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.cfg.Bots.Paths = []string{botsDir}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.cfg.PublicURL = "https://iterion.example"
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	sealed, err := connection.SealForgePAT(s.sealer, "c1", "ghp-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub, SealedPayload: sealed}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{ID: "i1", TenantID: "t1", ConnectionID: "c1", RepoFullName: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	fire := func(botID string) runview.LaunchSpec {
		t.Helper()
		if err := s.launchScheduledBot(spineCtx(t), cloudsched.ScheduledBot{
			ID: "sb-" + botID, TenantID: "t1", BotID: botID, Cron: "* * * * *", RepoIntegrationID: "i1",
			Vars: map[string]string{"workflow": "renovate.yml"}, NextFireAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("launchScheduledBot(%s) = %v, want nil", botID, err)
		}
		return pub.lastSpec()
	}
	return s, fire
}

// The tick reads the capability from the bundle it launches: a team row that
// shadows a catalogue slug declaring `actions` — without a manifest of its
// own — runs the team's code and is minted nothing.
//
// Mutation witnessed: launchScheduledBot handing scheduledDispatchGrant
// botManifestFor's answer instead of lb.Manifest → the shadow's run holds a
// dispatch grant → red.
func TestLaunchScheduledBot_TheGrantFollowsTheBundleTheTickLaunches(t *testing.T) {
	s, fire := scheduledKickServer(t, map[string]map[string]string{
		"kick": {"main.bot": spineProbeBot, "manifest.yaml": kickProbeManifest},
	})
	if spec := fire("kick"); spec.Vars[forgePublishVarToken] == "" {
		t.Fatalf("the catalogue's kick launched without a grant (vars %v) — the fixture must have something to lose", store.RedactLaunchStringVars(spec.Vars))
	}
	shadowRow(t, s, "t1", "kick")
	spec := fire("kick")
	if spec.BotSourceTier != store.BotSourceTierTeam {
		t.Fatalf("the tick launched tier %q, want the team row", spec.BotSourceTier)
	}
	for _, name := range forgePublishVars() {
		if v, ok := spec.Vars[name]; ok {
			t.Fatalf("a team bundle declaring no actions scope launched with %s=%q — its capability came from a manifest it shadows", name, v)
		}
	}
}

// kickWideManifest declares the dispatch AND every merge-gate gesture.
const kickWideManifest = "name: kick-wide\nversion: 0.1.0\nschema_version: 1\nforge:\n  token_scopes:\n    actions: write\n    approvals: write\n    merge: write\n    reviewers: write\n"

// The schedule grant carries the dispatch ONLY, however much else the
// manifest declares: least privilege, independent of the pull-request
// binding that would refuse the verdict verbs anyway.
//
// Mutation witnessed: scheduledDispatchGrant minting the manifest's gestures
// beside the dispatch → red.
func TestLaunchScheduledBot_TheScheduleGrantCarriesTheDispatchOnly(t *testing.T) {
	s, fire := scheduledKickServer(t, map[string]map[string]string{
		"kick-wide": {"main.bot": spineProbeBot, "manifest.yaml": kickWideManifest},
	})
	spec := fire("kick-wide")
	g, ok := s.forgePublishTokens.lookup(spec.Vars[forgePublishVarToken])
	if !ok || !slices.Equal(g.Capabilities, []string{publishCapDispatchWorkflow}) || g.DispatchWorkflow != "renovate.yml" {
		t.Fatalf("schedule grant of a manifest declaring every scope = (%+v, %v), want exactly [%s] bound to renovate.yml", g, ok, publishCapDispatchWorkflow)
	}
}

// Each guard of the schedule mint, on its own scenario.
func TestScheduledDispatchGrant_EachGuardOnItsOwn(t *testing.T) {
	// Mutation witnessed: the tenant check removed → a grant on team2's
	// repository is minted for team1's schedule → red.
	t.Run("an integration of another tenant", func(t *testing.T) {
		w := newDispatchWorld(t)
		// team2's own connection and integration: everything resolves, so
		// only the tenant check stands between team1's schedule and a grant
		// on team2's repository.
		other := w.conn
		other.ID, other.TenantID = "conn-team2", "team2"
		if err := w.s.forgeConnections.Create(context.Background(), other); err != nil {
			t.Fatal(err)
		}
		if err := w.s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
			ID: "integ-team2", TenantID: "team2", ConnectionID: "conn-team2", RepoFullName: "acme/widgets",
		}); err != nil {
			t.Fatal(err)
		}
		_, minted, err := w.s.scheduledDispatchGrant(context.Background(), cloudsched.ScheduledBot{
			ID: "sch-cross", TenantID: "team1", BotID: "workflow-kick", RepoIntegrationID: "integ-team2",
		}, w.manifestOf(t, "workflow-kick"), map[string]string{"workflow": "renovate.yml"})
		if err == nil || !strings.Contains(err.Error(), "belongs to another tenant") || minted != "" {
			t.Fatalf("schedule on another tenant's integration = (%q, %v), want refused naming the tenant", minted, err)
		}
	})
	// Mutation witnessed: the no-integration branch removed →
	// forgeIntegrations.Get("") fails the whole tick → red.
	t.Run("a schedule pinning no integration launches without a grant", func(t *testing.T) {
		w := newDispatchWorld(t)
		in := map[string]string{"workflow": "renovate.yml"}
		out, minted, err := w.s.scheduledDispatchGrant(context.Background(), cloudsched.ScheduledBot{
			ID: "sch-loose", TenantID: "team1", BotID: "workflow-kick",
		}, w.manifestOf(t, "workflow-kick"), in)
		if err != nil || minted != "" || len(out) != len(in) {
			t.Fatalf("schedule pinning no integration = (%v, %q, %v), want its vars untouched, no grant and no error (the run ends NO_DISPATCH_GRANT)", out, minted, err)
		}
	})
	// Mutation witnessed: the empty-base refusal removed → a grant naming a
	// host-less endpoint is minted → red.
	t.Run("no public base URL", func(t *testing.T) {
		w := newDispatchWorld(t)
		w.s.cfg.PublicURL = ""
		_, minted, err := w.tick(t, "workflow-kick", "sch-nobase", map[string]string{"workflow": "renovate.yml"})
		if err == nil || !strings.Contains(err.Error(), "no public base URL") || minted != "" {
			t.Fatalf("schedule with no public base URL = (%q, %v), want refused naming the base URL", minted, err)
		}
	})
}

// The per-call minter guards itself: a connection it cannot narrow a token
// from is refused before any mint, whoever calls it.
//
// Mutation witnessed: the minter's own workflowDispatchSupported check
// removed → the App mint is asked for a PAT connection's installation → red
// on the error and on the untouched forge.
func TestForgeWorkflowDispatchTokenMinter_RefusesAConnectionItCannotNarrow(t *testing.T) {
	w := newDispatchWorld(t)
	conn := w.conn
	conn.Kind = forge.KindPAT
	tok, err := w.s.forgeWorkflowDispatchTokenMinter(context.Background(), conn, "acme/widgets")
	if !errors.Is(err, errWorkflowDispatchUnsupported) || tok != "" {
		t.Fatalf("mint for a PAT connection = (%q, %v), want %v", tok, err, errWorkflowDispatchUnsupported)
	}
	w.assertForgeUntouched(t)
}

// Every dispatch the endpoint attempted on the forge leaves an audit row on
// the grant's team, naming the run, the workflow and the outcome.
//
// Mutation witnessed: the auditSystem call removed → no row → red.
func TestWorkflowDispatch_EveryAttemptIsAudited(t *testing.T) {
	w := newDispatchWorld(t)
	st := audit.NewMemoryStore()
	w.s.auditStore = st
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-audit", store.RunTrustDefault)
	if code, _, raw := w.dispatch(t, token, "run-audit", "acme/widgets", "renovate.yml"); code != http.StatusOK {
		t.Fatalf("dispatch = %d %s, want 200", code, raw)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := st.ListByTenant(context.Background(), "team1", audit.Page{Action: auditActionWorkflowDispatch})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.TargetID == "run-audit" && e.Meta["workflow"] == "renovate.yml" && e.Meta["dispatched"] == true {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s row for run-audit's dispatch (rows %+v)", auditActionWorkflowDispatch, events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every answer that is not a dispatch carries its typed code — the pre-grant
// refusals included — and none says "retry": the claim is spent before the
// forge is asked, so the same grant can only be refused again.
//
// Mutations witnessed (one per row): the missing-header, unknown-token and
// deleted-connection answers written untyped (httpError) → red on the code;
// writeDispatchAnswer's retryable set back to true → red on the 502 row.
func TestWorkflowDispatch_EveryAnswerIsTyped(t *testing.T) {
	t.Run("no bearer", func(t *testing.T) {
		w := newDispatchWorld(t)
		code, out, raw := w.postDispatch(t, "", `{"run_id":"r","repo":"acme/widgets","workflow":"renovate.yml"}`)
		if code != http.StatusUnauthorized || out["code"] != dispatchRefusedNoToken {
			t.Fatalf("no bearer = %d %s, want 401 %s", code, raw, dispatchRefusedNoToken)
		}
	})
	t.Run("an unknown bearer", func(t *testing.T) {
		w := newDispatchWorld(t)
		code, out, raw := w.postDispatch(t, "tok-nobody", `{"run_id":"r","repo":"acme/widgets","workflow":"renovate.yml"}`)
		if code != http.StatusUnauthorized || out["code"] != dispatchRefusedUnknownGrant {
			t.Fatalf("unknown bearer = %d %s, want 401 %s", code, raw, dispatchRefusedUnknownGrant)
		}
	})
	t.Run("a deleted connection", func(t *testing.T) {
		w := newDispatchWorld(t)
		token, _ := w.scheduleGrant(t, "workflow-kick", "run-noconn", store.RunTrustDefault)
		if err := w.s.forgeConnections.Delete(context.Background(), "team1", "conn1"); err != nil {
			t.Fatal(err)
		}
		code, out, raw := w.dispatch(t, token, "run-noconn", "acme/widgets", "renovate.yml")
		if code != http.StatusNotFound || out.Code != dispatchRefusedNoConnection {
			t.Fatalf("dispatch over a deleted connection = %d %s, want 404 %s", code, raw, dispatchRefusedNoConnection)
		}
	})
	t.Run("a credential that cannot be minted is not retryable", func(t *testing.T) {
		w := newDispatchWorld(t)
		token, _ := w.scheduleGrant(t, "workflow-kick", "run-nomint", store.RunTrustDefault)
		w.f.srv.Close() // the forge stops answering: the per-call mint fails
		code, out, raw := w.postDispatch(t, token, `{"run_id":"run-nomint","repo":"acme/widgets","workflow":"renovate.yml"}`)
		if code != http.StatusBadGateway || out["error_code"] != dispatchRefusedCredential || out["retryable"] != false {
			t.Fatalf("dispatch whose mint fails = %d %s, want 502 error_code %s and retryable false", code, raw, dispatchRefusedCredential)
		}
		code, out, raw = w.postDispatch(t, token, `{"run_id":"run-nomint","repo":"acme/widgets","workflow":"renovate.yml"}`)
		if code != http.StatusConflict || out["code"] != dispatchRefusedRepeat {
			t.Fatalf("the retry the answer must not invite = %d %s, want 409 %s", code, raw, dispatchRefusedRepeat)
		}
	})
}

// A grant bound to no pull request lives under a key namespace an older
// build does not read, so a replica of the previous release finds no grant
// at all and answers "unknown token" — it cannot honour the grant as the
// pull-request grant its zero value would read as. A pull-request grant stays
// where every build reads it.
//
// The older build's keys are spelled here as that release spells them, not
// through this build's constants, which could move with the fix.
//
// Mutation witnessed: forgePublishRegisterKey writing every grant under the
// digest key → the older build's key holds the unbound grant → red.
func TestValkeyGrantStore_AnUnboundGrantIsInvisibleToAnOlderBuild(t *testing.T) {
	mr, rdb := newTestRedis(t)
	st := newValkeyForgePublishTokenStore(rdb, nil)
	olderBuildKeys := func(token string) []string {
		sum := sha256.Sum256([]byte(token))
		return []string{"iterion:forgepub:tokd:" + hex.EncodeToString(sum[:]), "iterion:forgepub:tok:" + token}
	}
	if err := st.Register("tok-schedule", ForgePublishGrant{
		TeamID: "team1", ConnectionID: "conn1", Repo: "acme/widgets",
		Capabilities: []string{publishCapDispatchWorkflow}, DispatchWorkflow: "renovate.yml", NoPullRequest: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range olderBuildKeys("tok-schedule") {
		if mr.Exists(key) {
			t.Fatalf("the unbound grant lives under %s, which an older build reads — and honours as a pull-request grant", key)
		}
	}
	if g, ok := st.lookup("tok-schedule"); !ok || !g.NoPullRequest || g.DispatchWorkflow != "renovate.yml" {
		t.Fatalf("this build's lookup of the unbound grant = (%+v, %v), want it found, unbound, bound to renovate.yml", g, ok)
	}
	if err := st.Register("tok-pr", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists(olderBuildKeys("tok-pr")[0]) {
		t.Fatal("a pull-request grant left the key every build reads — a rolling deploy would refuse it on the older replicas")
	}
}

// The namespace IS the binding: a record under it whose body lacks the field
// — written by hand here, as nothing in this build writes one — still reads
// unbound, on lookup and inside an update.
//
// Mutation witnessed: bindStoredGrant made a no-op → the record reads as a
// pull-request grant → red.
func TestValkeyGrantStore_TheUnboundNamespaceBindsWhateverTheBodySays(t *testing.T) {
	mr, rdb := newTestRedis(t)
	st := newValkeyForgePublishTokenStore(rdb, nil)
	body, _ := json.Marshal(map[string]any{"team_id": "team1", "connection_id": "conn1", "repo": "acme/widgets"})
	if err := mr.Set(forgePublishUnboundTokenKey("tok-bare"), string(body)); err != nil {
		t.Fatal(err)
	}
	if g, ok := st.lookup("tok-bare"); !ok || !g.NoPullRequest {
		t.Fatalf("lookup of a bare record under the unbound namespace = (%+v, %v), want it unbound", g, ok)
	}
	var seen bool
	if found, err := st.update("tok-bare", func(g *ForgePublishGrant) { seen = g.NoPullRequest }); err != nil || !found || !seen {
		t.Fatalf("update of the same record = (found %v, err %v, unbound %v), want found and unbound", found, err, seen)
	}
}
