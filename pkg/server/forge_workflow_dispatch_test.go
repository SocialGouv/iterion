package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/redis/go-redis/v9"
)

// The workflow-dispatch witnesses (#3130). Each one drives the REAL path —
// the shipped bot manifests resolved from bots/, the real grant registry, the
// real handler, the real GitHub App mint and the real GitHub client — against
// a fake GitHub as strict as the API it stands for: a token minted without
// actions:write is refused the dispatch (403), an unknown workflow 404s, a
// workflow without a workflow_dispatch trigger and an unknown ref 422, and a
// mint asking for a permission the installation did not grant 422s.

// fakeDispatchForge is that GitHub. It records every mint (the permissions and
// repositories it asked for), every dispatch it accepted, and every request it
// received at all — the oracle for "refused BEFORE any forge call".
type fakeDispatchForge struct {
	srv *httptest.Server

	mu sync.Mutex
	// granted is the installation's approved permission set.
	granted map[string]string
	// repos are the repositories in the installation, short name → default
	// branch.
	repos map[string]string
	// branches are the refs that exist besides each default branch.
	branches map[string]bool
	// workflows maps a workflow file to whether it declares
	// workflow_dispatch; a file absent here does not exist.
	workflows map[string]bool
	// dispatchFault makes the dispatch POST fail after the write: "drop"
	// closes the connection unanswered, "502" answers a server error.
	dispatchFault string

	tokens     map[string]mintedTokenRecord
	mints      []mintedTokenRecord
	dispatches []dispatchRecord
	requests   []string
}

type mintedTokenRecord struct {
	Permissions  map[string]string
	Repositories []string
}

type dispatchRecord struct {
	Repo, Workflow, Ref string
}

func newFakeDispatchForge(t *testing.T) *fakeDispatchForge {
	t.Helper()
	f := &fakeDispatchForge{
		granted:   map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read", "repository_hooks": "write", "actions": "write"},
		repos:     map[string]string{"widgets": "main"},
		branches:  map[string]bool{},
		workflows: map[string]bool{"renovate.yml": true, "ci.yml": false, "lint.yml": true},
		tokens:    map[string]mintedTokenRecord{},
	}
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	// token resolves the bearer to what it was minted with; ok=false is a
	// bearer this installation never minted.
	token := func(r *http.Request) (mintedTokenRecord, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rec, ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		return rec, ok
	}
	reaches := func(rec mintedTokenRecord, repo string) bool {
		return len(rec.Repositories) == 0 || slices.Contains(rec.Repositories, repo)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var req mintedTokenRecord
		var body struct {
			Permissions  map[string]string `json:"permissions"`
			Repositories []string          `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		req.Permissions, req.Repositories = body.Permissions, body.Repositories
		f.mu.Lock()
		f.requests = append(f.requests, "mint")
		f.mints = append(f.mints, req)
		granted := f.granted
		repos := f.repos
		f.mu.Unlock()
		for name, level := range req.Permissions {
			got, ok := granted[name]
			if !ok || (level == "write" && got != "write" && got != "admin") {
				reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "The permissions requested are not granted to this installation."})
				return
			}
		}
		for _, repo := range req.Repositories {
			if _, ok := repos[repo]; !ok {
				reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."})
				return
			}
		}
		f.mu.Lock()
		tok := fmt.Sprintf("ghs_%d", len(f.mints))
		f.tokens[tok] = req
		f.mu.Unlock()
		reply(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": "2099-01-01T00:00:00Z", "permissions": req.Permissions})
	})
	// The installation's live grant, App-JWT authenticated: what the health
	// view, the refresh route and the pre-mint re-read see.
	mux.HandleFunc("GET /api/v3/app/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, "installation")
		granted := maps.Clone(f.granted)
		f.mu.Unlock()
		if r.PathValue("id") != "42" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		reply(w, http.StatusOK, map[string]any{
			"id": 42, "account": map[string]any{"login": "acme"},
			"html_url": "https://github.example/organizations/acme/settings/installations/42", "permissions": granted,
		})
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, "repo")
		branch, known := f.repos[r.PathValue("repo")]
		f.mu.Unlock()
		rec, ok := token(r)
		if !ok {
			reply(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		if r.PathValue("owner") != "acme" || !known || rec.Permissions["metadata"] == "" || !reaches(rec, r.PathValue("repo")) {
			reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		reply(w, http.StatusOK, map[string]any{"full_name": "acme/" + r.PathValue("repo"), "default_branch": branch})
	})
	mux.HandleFunc("POST /api/v3/repos/{owner}/{repo}/actions/workflows/{file}/dispatches", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, "dispatch")
		branch, known := f.repos[r.PathValue("repo")]
		dispatchable, exists := f.workflows[r.PathValue("file")]
		extra := f.branches
		fault := f.dispatchFault
		f.mu.Unlock()
		rec, ok := token(r)
		if !ok {
			reply(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		if r.PathValue("owner") != "acme" || !known || !reaches(rec, r.PathValue("repo")) {
			reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		if rec.Permissions["actions"] != "write" {
			reply(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by integration"})
			return
		}
		if !exists {
			reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			reply(w, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
			return
		}
		ref, _ := body["ref"].(string)
		if ref == "" {
			reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "Invalid request.\n\n\"ref\" wasn't supplied."})
			return
		}
		// A branch short or fully qualified, as GitHub accepts it.
		if short := strings.TrimPrefix(ref, "refs/heads/"); short != branch && !extra[short] {
			reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "No ref found for: " + ref})
			return
		}
		if !dispatchable {
			reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "Workflow does not have 'workflow_dispatch' trigger"})
			return
		}
		f.mu.Lock()
		f.dispatches = append(f.dispatches, dispatchRecord{Repo: r.PathValue("owner") + "/" + r.PathValue("repo"), Workflow: r.PathValue("file"), Ref: ref})
		f.mu.Unlock()
		switch fault {
		case "drop":
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		case "502":
			reply(w, http.StatusBadGateway, map[string]any{"message": "Server Error"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDispatchForge) snapshot() (mints []mintedTokenRecord, dispatches []dispatchRecord, requests []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mintedTokenRecord(nil), f.mints...), append([]dispatchRecord(nil), f.dispatches...), append([]string(nil), f.requests...)
}

// dispatchWorld is a cloud-shaped server: a GitHub App connection (installation
// 42 on the fake forge) of team1, its repo integration on acme/widgets, the
// shipped catalogue as the bot source, a run store, and a public URL — built
// through New so the route and the auth exemption are the production ones.
type dispatchWorld struct {
	s    *Server
	f    *fakeDispatchForge
	runs store.RunStore
	conn forge.Connection
	// manifests caches each bot's resolved manifest: resolving walks the
	// whole catalogue.
	manifests map[string]*bundle.Manifest
}

func newDispatchWorld(t *testing.T) *dispatchWorld {
	t.Helper()
	f := newFakeDispatchForge(t)
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conns := connection.NewMemoryForgeStore()
	s := New(Config{ForgeConnections: conns, Store: rs, PublicURL: "https://iterion.example"}, iterlog.New(iterlog.LevelError, nil))
	t.Cleanup(s.hub.Stop)
	// The shipped catalogue: the capability is read from the bots' REAL
	// manifests, never from a fixture that could be gentler than they are.
	s.cfg.Bots.Paths = []string{filepath.Join("..", "..", "bots")}
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	conn := forge.Connection{
		ID: "conn1", TenantID: "team1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, InstallationAccount: "acme", AccountLogin: "iterion-forge-x[bot]", AppSlug: "iterion-forge-x",
		GrantedPermissions: maps.Clone(f.granted), CreatedAt: time.Now().UTC(),
	}
	if err := conns.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		ID: "integ1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "acme/widgets",
	}); err != nil {
		t.Fatal(err)
	}
	return &dispatchWorld{s: s, f: f, runs: rs, conn: conn, manifests: map[string]*bundle.Manifest{}}
}

// manifestOf is botID's manifest as a tick of team1's schedule reads it: the
// launch resolution's own (resolveBotSource → launchBot.Manifest).
func (w *dispatchWorld) manifestOf(t *testing.T, botID string) *bundle.Manifest {
	t.Helper()
	if m, ok := w.manifests[botID]; ok {
		return m
	}
	lb, err := w.s.resolveBotSource(context.Background(), "team1", botID)
	if err != nil {
		t.Fatalf("resolve %s: %v", botID, err)
	}
	defer lb.Cleanup()
	w.manifests[botID] = lb.Manifest
	return lb.Manifest
}

// tick is the schedule mint as launchScheduledBot calls it for botID on
// integ1, with the schedule row's vars.
func (w *dispatchWorld) tick(t *testing.T, botID, scheduleID string, vars map[string]string) (map[string]string, mintedGrant, error) {
	t.Helper()
	return w.s.scheduledDispatchGrant(context.Background(), cloudsched.ScheduledBot{
		ID: scheduleID, TenantID: "team1", BotID: botID, RepoIntegrationID: "integ1",
	}, w.manifestOf(t, botID), vars)
}

// scheduleGrant mints a schedule run's grant through the production mint for
// botID, bound to renovate.yml, and records the run holding it, exactly as a
// launch would: the run's inputs carry the minted vars. It returns the token
// and the dispatch endpoint's path.
func (w *dispatchWorld) scheduleGrant(t *testing.T, botID, runID string, trust store.RunTrust) (string, string) {
	t.Helper()
	return w.scheduleGrantFor(t, botID, runID, "renovate.yml", trust)
}

// scheduleGrantFor is scheduleGrant for a schedule row naming workflow.
func (w *dispatchWorld) scheduleGrantFor(t *testing.T, botID, runID, workflow string, trust store.RunTrust) (string, string) {
	t.Helper()
	vars, minted, err := w.tick(t, botID, "sch-"+runID, map[string]string{"workflow": workflow})
	if err != nil {
		t.Fatalf("scheduledDispatchGrant: %v", err)
	}
	if minted == "" || vars[forgePublishVarToken] != string(minted) {
		t.Fatalf("no grant minted for %s (vars %v) — the fixture cannot witness a dispatch it could not make", botID, vars)
	}
	w.holdRun(t, runID, vars, trust)
	u, err := url.Parse(vars[forgePublishVarDispatch])
	if err != nil || u.Path != forgeDispatchWorkflowPath {
		t.Fatalf("minted dispatch endpoint %q, want …%s", vars[forgePublishVarDispatch], forgeDispatchWorkflowPath)
	}
	return string(minted), u.Path
}

// holdRun records a run whose inputs are vars, as the launch stores them.
func (w *dispatchWorld) holdRun(t *testing.T, runID string, vars map[string]string, trust store.RunTrust) {
	t.Helper()
	inputs := map[string]any{}
	for k, v := range vars {
		inputs[k] = v
	}
	run, err := w.runs.CreateRun(context.Background(), runID, "workflow_kick", inputs)
	if err != nil {
		t.Fatal(err)
	}
	if trust != store.RunTrustDefault {
		run.Trust = trust
		if err := w.runs.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
}

// dispatch POSTs through the server's FULL handler — routing, the auth
// middleware's exemption, the handler — as the run's tool node does.
func (w *dispatchWorld) dispatch(t *testing.T, token, runID, repo, workflow string) (int, dispatchWorkflowResponse, string) {
	t.Helper()
	body, _ := json.Marshal(dispatchWorkflowRequest{RunID: runID, Repo: repo, Workflow: workflow})
	r := httptest.NewRequest(http.MethodPost, forgeDispatchWorkflowPath, strings.NewReader(string(body)))
	r.Header.Set("X-Iterion-Run", token)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.s.Handler().ServeHTTP(rec, r)
	var out dispatchWorkflowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// assertForgeUntouched is the "refused BEFORE any forge call" oracle.
func (w *dispatchWorld) assertForgeUntouched(t *testing.T) {
	t.Helper()
	if _, _, requests := w.f.snapshot(); len(requests) != 0 {
		t.Fatalf("the forge was called %v — the refusal must come before any forge call", requests)
	}
}

func (w *dispatchWorld) claims(t *testing.T, token string) []grantDispatch {
	t.Helper()
	g, ok := w.s.forgePublishTokens.lookup(token)
	if !ok {
		t.Fatal("the grant is gone")
	}
	return g.Dispatches
}

// The whole lane, end to end: a repo-bound schedule of the shipped
// workflow-kick bot mints a grant carrying the dispatch capability, the run
// calls the endpoint the grant names, and the server dispatches the workflow
// on the default branch with a token minted for that call ONLY — exactly
// {actions: write, metadata: read}, on exactly the grant's repository.
//
// Mutation witnessed: WorkflowDispatchInstallationPermissions widened (a
// contents:write added) → red on the exact-permission assertion.
func TestWorkflowDispatch_AScheduleOfTheShippedBotDispatchesWithAPerCallNarrowToken(t *testing.T) {
	w := newDispatchWorld(t)
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-ok", store.RunTrustDefault)
	g, _ := w.s.forgePublishTokens.lookup(token)
	if !slices.Equal(g.Capabilities, []string{publishCapDispatchWorkflow}) || g.Repo != "acme/widgets" || g.ConnectionID != "conn1" || g.TeamID != "team1" || g.DispatchWorkflow != "renovate.yml" {
		t.Fatalf("schedule grant = %+v, want the integration's (team1, conn1, acme/widgets) carrying only %s, bound to renovate.yml", g, publishCapDispatchWorkflow)
	}

	code, out, raw := w.dispatch(t, token, "run-ok", "acme/widgets", "renovate.yml")
	if code != http.StatusOK || !out.Dispatched || out.Ref != "main" || out.Code != "" {
		t.Fatalf("dispatch = %d %s, want 200 dispatched on main", code, raw)
	}
	if want := w.f.srv.URL + "/acme/widgets/actions/workflows/renovate.yml"; out.WorkflowURL != want {
		t.Errorf("workflow_url = %q, want %q", out.WorkflowURL, want)
	}
	mints, dispatches, _ := w.f.snapshot()
	if len(dispatches) != 1 || dispatches[0] != (dispatchRecord{Repo: "acme/widgets", Workflow: "renovate.yml", Ref: "refs/heads/main"}) {
		t.Fatalf("forge dispatches = %+v, want exactly renovate.yml on acme/widgets@refs/heads/main (the branch, never a same-named tag)", dispatches)
	}
	if len(mints) != 1 {
		t.Fatalf("mints = %+v, want exactly one, for this call", mints)
	}
	if want := map[string]string{"actions": "write", "metadata": "read"}; !maps.Equal(mints[0].Permissions, want) {
		t.Errorf("the dispatch token asked for %v, want exactly %v — no wider", mints[0].Permissions, want)
	}
	if !slices.Equal(mints[0].Repositories, []string{"widgets"}) {
		t.Errorf("the dispatch token was narrowed to %v, want exactly the grant's repository [widgets]", mints[0].Repositories)
	}
}

// A bot whose manifest does not declare the actions scope never holds the
// capability: the shipped gitops-warden declares approvals/merge/reviewers
// and nothing else. Its schedule mints no grant at all, and its PR grant —
// minted by the PR launch path from the same manifest — is refused the
// dispatch, typed, before any forge call.
//
// Mutation witnessed: declaresWorkflowDispatch answering true for every
// manifest → the warden's schedule mints a dispatch grant → red.
func TestWorkflowDispatch_ABotWithoutTheActionsScopeHoldsNoCapability(t *testing.T) {
	t.Run("its schedule mints no grant", func(t *testing.T) {
		w := newDispatchWorld(t)
		in := map[string]string{"workflow": "renovate.yml"}
		vars, minted, err := w.tick(t, "gitops-warden", "sch-warden", in)
		if err != nil || minted != "" || !maps.Equal(vars, in) {
			t.Fatalf("schedule of a bot without the scope = (%v, %q, %v), want its vars untouched and no grant", vars, minted, err)
		}
	})
	t.Run("its PR grant is refused the dispatch", func(t *testing.T) {
		w := newDispatchWorld(t)
		prURL := w.f.srv.URL + "/acme/widgets/pull/7"
		vars, minted, err := w.s.injectForgePublishVars(context.Background(), "team1", "conn1", "gitops-warden",
			map[string]string{"pr_url": prURL}, nil, store.RunTrustDefault)
		if err != nil || minted == "" {
			t.Fatalf("PR grant for gitops-warden = (%q, %v), want one minted — the fixture must hold a grant to be refused", minted, err)
		}
		g, _ := w.s.forgePublishTokens.lookup(string(minted))
		if !slices.Equal(g.Capabilities, []string{publishCapApprove, publishCapMerge, publishCapReviewers}) {
			t.Fatalf("gitops-warden grant capabilities = %v, want its three merge-gate gestures and nothing else", g.Capabilities)
		}
		w.holdRun(t, "run-warden", vars, store.RunTrustDefault)
		code, out, raw := w.dispatch(t, string(minted), "run-warden", "acme/widgets", "renovate.yml")
		if code != http.StatusForbidden || out.Code != dispatchRefusedNoCapability || out.Dispatched {
			t.Fatalf("dispatch = %d %s, want 403 %s", code, raw, dispatchRefusedNoCapability)
		}
		w.assertForgeUntouched(t)
	})
}

// ADR-162: a run whose workspace holds code the tenant did not write never
// dispatches, even holding a grant that carries the capability.
//
// Mutation witnessed: runOwnsGrant's trust check muted → the fork run
// dispatches → red.
func TestWorkflowDispatch_AnUntrustedRunNeverDispatches(t *testing.T) {
	w := newDispatchWorld(t)
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-fork", store.RunTrustFork)
	code, out, raw := w.dispatch(t, token, "run-fork", "acme/widgets", "renovate.yml")
	if code != http.StatusForbidden || out.Code != grantUntrustedRunReason || out.Dispatched {
		t.Fatalf("dispatch by a fork-trust run = %d %s, want 403 %s", code, raw, grantUntrustedRunReason)
	}
	w.assertForgeUntouched(t)
	if c := w.claims(t, token); len(c) != 0 {
		t.Errorf("a refused run left claims %v on the grant", c)
	}
}

// The repository is the grant's, never retargeted: a request naming another
// one is refused, not dispatched on the grant's.
//
// Mutation witnessed: the repository comparison muted → the request reaches
// the forge → red.
func TestWorkflowDispatch_ARepositoryOutsideTheGrantIsRefused(t *testing.T) {
	w := newDispatchWorld(t)
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-repo", store.RunTrustDefault)
	code, out, raw := w.dispatch(t, token, "run-repo", "acme/other", "renovate.yml")
	if code != http.StatusForbidden || out.Code != dispatchRefusedRepo || out.Dispatched {
		t.Fatalf("dispatch on another repository = %d %s, want 403 %s", code, raw, dispatchRefusedRepo)
	}
	w.assertForgeUntouched(t)
}

// The workflow file lands in a forge API path: anything but a plain
// *.yml/*.yaml basename is refused at the endpoint.
//
// Mutation witnessed: the handler's ValidWorkflowFile check muted → each
// name reaches the mint (the client's own guard then refuses it with
// another code) → red on both the code and the untouched forge.
func TestWorkflowDispatch_AWorkflowThatIsNotAPlainYAMLFileNameIsRefused(t *testing.T) {
	for _, name := range []string{"../renovate.yml", "dir/renovate.yml", "renovate.sh", ".yml", "renovate", "renovate.yml/", "renovate%2F.yml", "renovate.yml?ref=x", "re novate.yml", ".github.yml"} {
		t.Run(name, func(t *testing.T) {
			w := newDispatchWorld(t)
			token, _ := w.scheduleGrant(t, "workflow-kick", "run-file", store.RunTrustDefault)
			code, out, raw := w.dispatch(t, token, "run-file", "acme/widgets", name)
			if code != http.StatusBadRequest || out.Code != dispatchRefusedBadFile {
				t.Fatalf("dispatch of %q = %d %s, want 400 %s", name, code, raw, dispatchRefusedBadFile)
			}
			w.assertForgeUntouched(t)
		})
	}
}

// A connection the server cannot dispatch through is refused typed — never a
// silent no-op, never a call: GitLab and Forgejo have no dispatch here, and a
// GitHub connection that is not an App cannot mint the narrow token.
//
// Mutation witnessed: workflowDispatchSupported answering nil → the GitLab
// grant reaches the App mint on the GitLab host → red on the code and the
// untouched host.
func TestWorkflowDispatch_AConnectionThatCannotDispatchIsRefusedTyped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider forge.Provider
		kind     forge.Kind
	}{
		{"gitlab", forge.ProviderGitLab, forge.KindPAT},
		{"forgejo", forge.ProviderForgejo, forge.KindPAT},
		{"github-pat", forge.ProviderGitHub, forge.KindPAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newDispatchWorld(t)
			conn := w.conn
			conn.ID, conn.Provider, conn.Kind = "conn-"+tc.name, tc.provider, tc.kind
			if err := w.s.forgeConnections.Create(context.Background(), conn); err != nil {
				t.Fatal(err)
			}
			if err := w.s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
				ID: "integ-" + tc.name, TenantID: "team1", ConnectionID: conn.ID, RepoFullName: "acme/widgets",
			}); err != nil {
				t.Fatal(err)
			}
			vars, minted, err := w.s.scheduledDispatchGrant(context.Background(), cloudsched.ScheduledBot{
				ID: "sch-" + tc.name, TenantID: "team1", BotID: "workflow-kick", RepoIntegrationID: "integ-" + tc.name,
			}, w.manifestOf(t, "workflow-kick"), map[string]string{"workflow": "renovate.yml"})
			if err != nil || minted == "" {
				t.Fatalf("grant on the %s connection = (%q, %v), want one — the refusal belongs to the dispatch, typed", tc.name, minted, err)
			}
			w.holdRun(t, "run-"+tc.name, vars, store.RunTrustDefault)
			code, out, raw := w.dispatch(t, string(minted), "run-"+tc.name, "acme/widgets", "renovate.yml")
			if code != http.StatusUnprocessableEntity || out.Code != dispatchRefusedUnsupported || out.Dispatched {
				t.Fatalf("dispatch through a %s connection = %d %s, want 422 %s", tc.name, code, raw, dispatchRefusedUnsupported)
			}
			w.assertForgeUntouched(t)
			if c := w.claims(t, string(minted)); len(c) != 0 {
				t.Errorf("an unsupported connection left claims %v on the grant", c)
			}
		})
	}
}

// One dispatch per GRANT: a replay is refused typed and never reaches the
// forge, concurrent replays included — and so is every other run presenting
// the same grant under its own run id: a sub-bot child handed the token
// through `with:`, a fork that inherited it. One grant cannot fan the
// dispatch out, whichever run comes first.
//
// Mutations witnessed: claimWorkflowDispatch claiming unconditionally → the
// replay dispatches a second time → red; the claim keyed on the presenting
// run (the shape before) → each child run dispatches again → red.
func TestWorkflowDispatch_OneDispatchPerGrant(t *testing.T) {
	t.Run("a replay is refused", func(t *testing.T) {
		w := newDispatchWorld(t)
		token, _ := w.scheduleGrant(t, "workflow-kick", "run-once", store.RunTrustDefault)
		if code, _, raw := w.dispatch(t, token, "run-once", "acme/widgets", "renovate.yml"); code != http.StatusOK {
			t.Fatalf("first dispatch = %d %s, want 200", code, raw)
		}
		code, out, raw := w.dispatch(t, token, "run-once", "acme/widgets", "renovate.yml")
		if code != http.StatusConflict || out.Code != dispatchRefusedRepeat {
			t.Fatalf("replay = %d %s, want 409 %s", code, raw, dispatchRefusedRepeat)
		}
		if _, dispatches, _ := w.f.snapshot(); len(dispatches) != 1 {
			t.Fatalf("forge dispatches = %+v, want exactly one", dispatches)
		}
	})
	for _, childFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("other runs holding the grant (child first: %v)", childFirst), func(t *testing.T) {
			w := newDispatchWorld(t)
			token, _ := w.scheduleGrant(t, "workflow-kick", "run-parent", store.RunTrustDefault)
			// What a sub-bot child given the parent's grant through `with:`,
			// or a fork inheriting the parent's inputs, records: the same
			// token under a run id of its own.
			children := []string{"run-child-1", "run-child-2", "run-child-3"}
			for _, id := range children {
				w.holdRun(t, id, map[string]string{forgePublishVarToken: token, "workflow": "renovate.yml"}, store.RunTrustDefault)
			}
			order := append([]string{"run-parent"}, children...)
			if childFirst {
				order = append(append([]string(nil), children...), "run-parent")
			}
			oks := 0
			for _, id := range order {
				code, out, raw := w.dispatch(t, token, id, "acme/widgets", "renovate.yml")
				switch {
				case code == http.StatusOK:
					oks++
				case code != http.StatusConflict || out.Code != dispatchRefusedRepeat:
					t.Fatalf("dispatch by %s = %d %s, want 200 for the first presenter and 409 %s after", id, code, raw, dispatchRefusedRepeat)
				}
			}
			if _, dispatches, _ := w.f.snapshot(); oks != 1 || len(dispatches) != 1 {
				t.Fatalf("%d runs presenting one grant got %d dispatches through (%d forge dispatches), want exactly one", len(order), oks, len(dispatches))
			}
		})
	}
	t.Run("concurrent replays dispatch once", func(t *testing.T) {
		w := newDispatchWorld(t)
		token, _ := w.scheduleGrant(t, "workflow-kick", "run-race", store.RunTrustDefault)
		const n = 8
		var wg sync.WaitGroup
		codes := make([]int, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i], _, _ = w.dispatch(t, token, "run-race", "acme/widgets", "renovate.yml")
			}()
		}
		wg.Wait()
		sort.Ints(codes)
		ok, conflicts := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflicts++
			}
		}
		if ok != 1 || conflicts != n-1 {
			t.Fatalf("concurrent replays answered %v, want one 200 and %d 409", codes, n-1)
		}
		if _, dispatches, _ := w.f.snapshot(); len(dispatches) != 1 {
			t.Fatalf("forge dispatches = %+v, want exactly one", dispatches)
		}
	})
	// The cloud twin: on Valkey the claim's update re-runs fn when another
	// replica wrote the grant between its WATCH and its EXEC. A pass that
	// finds the other replica's claim must answer "not claimed", whatever an
	// earlier pass of the same call decided. The interleaving is forced, not
	// raced for: a goroutine race on miniredis does not reach it.
	//
	// Mutation witnessed: the `claimed` reset at the top of the claim's fn
	// removed → the second pass keeps the first pass's true → the run
	// dispatches over the other replica's claim → red.
	t.Run("valkey: a claim another replica commits mid-transaction wins", func(t *testing.T) {
		w := newDispatchWorld(t)
		mr, rdb := newTestRedis(t)
		rivalClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rivalClient.Close() })
		st := &interleavingGrantStore{
			valkeyForgePublishTokenStore: newValkeyForgePublishTokenStore(rdb, nil),
			rival:                        newValkeyForgePublishTokenStore(rivalClient, nil),
			rivalRun:                     "run-vrace-replica-b",
		}
		w.s.forgePublishTokens = st
		token, _ := w.scheduleGrant(t, "workflow-kick", "run-vrace", store.RunTrustDefault)
		code, out, raw := w.dispatch(t, token, "run-vrace", "acme/widgets", "renovate.yml")
		if st.rivalErr != nil {
			t.Fatalf("the rival replica's claim: %v", st.rivalErr)
		}
		if st.passes() < 2 {
			t.Fatalf("the claim's fn ran %d time(s) — the interleaving did not force the re-run the witness is about", st.passes())
		}
		if code != http.StatusConflict || out.Code != dispatchRefusedRepeat {
			t.Fatalf("dispatch over another replica's mid-transaction claim = %d %s, want 409 %s", code, raw, dispatchRefusedRepeat)
		}
		if _, dispatches, _ := w.f.snapshot(); len(dispatches) != 0 {
			t.Fatalf("forge dispatches = %+v, want none — the other replica holds the claim", dispatches)
		}
		if c := w.claims(t, token); len(c) != 1 || c[0].RunID != st.rivalRun {
			t.Fatalf("claims on the grant = %+v, want the other replica's one", c)
		}
	})
}

// interleavingGrantStore is the Valkey grant store with a second replica (a
// client of its own) committing a claim on the same grant INSIDE the first
// claim's transaction — after its WATCH and read, before its EXEC — once.
type interleavingGrantStore struct {
	*valkeyForgePublishTokenStore
	rival    *valkeyForgePublishTokenStore
	rivalRun string
	rivalErr error

	once sync.Once
	mu   sync.Mutex
	runs int
}

func (s *interleavingGrantStore) passes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs
}

func (s *interleavingGrantStore) update(token string, fn func(*ForgePublishGrant)) (bool, error) {
	return s.valkeyForgePublishTokenStore.update(token, func(g *ForgePublishGrant) {
		s.mu.Lock()
		s.runs++
		s.mu.Unlock()
		fn(g)
		s.once.Do(func() {
			_, s.rivalErr = s.rival.update(token, func(g *ForgePublishGrant) {
				g.Dispatches = append(g.Dispatches, grantDispatch{RunID: s.rivalRun, Workflow: "renovate.yml", At: time.Now().UTC()})
			})
		})
	})
}

// The run presenting the grant must hold it: a run id whose record carries
// another token (or none) is refused — the trust and the claim are that
// run's, so it has to be the one the grant was minted for.
//
// Mutation witnessed: the sameGrantToken comparison muted → a run that holds
// no grant dispatches → red.
func TestWorkflowDispatch_ARunThatDoesNotHoldTheGrantIsRefused(t *testing.T) {
	w := newDispatchWorld(t)
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-holder", store.RunTrustDefault)
	w.holdRun(t, "run-stranger", map[string]string{"workflow": "renovate.yml"}, store.RunTrustDefault)
	for _, runID := range []string{"run-stranger", "run-missing"} {
		code, out, raw := w.dispatch(t, token, runID, "acme/widgets", "renovate.yml")
		if code != http.StatusForbidden || out.Code != dispatchRefusedRun {
			t.Fatalf("dispatch as %s = %d %s, want 403 %s", runID, code, raw, dispatchRefusedRun)
		}
	}
	w.assertForgeUntouched(t)
}

// A connection KNOWN to lack Actions: Read and write — stored, and live when
// re-read — fails before the mint, naming the permission to grant: the
// forgeSecurityTokenMinter precedent. The one forge call is the live read.
//
// Mutation witnessed: the pre-mint grant check removed → the mint is asked
// (and 422s) → red on "only the live grant read".
func TestWorkflowDispatch_AKnownNarrowGrantFailsBeforeTheMintNamingThePermission(t *testing.T) {
	w := newDispatchWorld(t)
	narrow := maps.Clone(w.conn.GrantedPermissions)
	delete(narrow, "actions")
	conn := w.conn
	conn.GrantedPermissions = narrow
	if err := w.s.forgeConnections.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	w.f.mu.Lock()
	w.f.granted = narrow
	w.f.mu.Unlock()
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-narrow", store.RunTrustDefault)
	code, out, raw := w.dispatch(t, token, "run-narrow", "acme/widgets", "renovate.yml")
	if code != http.StatusForbidden || out.Code != dispatchRefusedPermission || !strings.Contains(out.Reason, "Actions: Read and write") {
		t.Fatalf("dispatch on an installation without Actions = %d %s, want 403 %s naming 'Actions: Read and write'", code, raw, dispatchRefusedPermission)
	}
	if _, _, requests := w.f.snapshot(); !slices.Equal(requests, []string{"installation"}) {
		t.Fatalf("the forge was called %v, want only the live grant read — no mint, no dispatch", requests)
	}
}

// An owner approving Actions on the forge AFTER the install is honoured at the
// next dispatch: the stored grant lacking it is re-read live and synced, not
// answered "add the permission" for a step already taken (#3130's own
// sequence). The connection record carries the live grant afterwards.
//
// Mutation witnessed: installationGrants answering from the stored grant
// alone (no live re-read) → 403 workflow_dispatch_permission_missing → red.
func TestWorkflowDispatch_AnApprovalAfterTheInstallIsReadLive(t *testing.T) {
	w := newDispatchWorld(t)
	stale := maps.Clone(w.conn.GrantedPermissions)
	delete(stale, "actions")
	conn := w.conn
	conn.GrantedPermissions = stale
	if err := w.s.forgeConnections.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-approved", store.RunTrustDefault)
	code, out, raw := w.dispatch(t, token, "run-approved", "acme/widgets", "renovate.yml")
	if code != http.StatusOK || !out.Dispatched {
		t.Fatalf("dispatch after the owner approved Actions = %d %s, want 200 dispatched", code, raw)
	}
	got, err := w.s.forgeConnections.Get(context.Background(), "team1", "conn1")
	if err != nil {
		t.Fatal(err)
	}
	if !grantsActionsWrite(got.GrantedPermissions) {
		t.Fatalf("the connection's stored grant after the live read = %v, want it synced to carry actions:write", got.GrantedPermissions)
	}
}

// The security-read minter shares the re-read: a stored grant lacking
// Dependabot alerts, approved since on the forge, mints.
//
// Mutation witnessed: forgeSecurityTokenMinter's check reading
// conn.GrantedPermissions alone → ErrPermissionsNotGranted → red.
func TestForgeSecurityTokenMinter_AnApprovalAfterTheInstallIsReadLive(t *testing.T) {
	w := newDispatchWorld(t)
	w.f.mu.Lock()
	w.f.granted["vulnerability_alerts"] = "read"
	w.f.mu.Unlock()
	tok, _, err := w.s.forgeSecurityTokenMinter(context.Background(), w.conn)
	if err != nil || tok == "" {
		t.Fatalf("security-read mint after the owner approved Dependabot alerts = (%q, %v), want a token", tok, err)
	}
	if _, _, requests := w.f.snapshot(); !slices.Equal(requests, []string{"installation", "mint"}) {
		t.Fatalf("the forge was called %v, want the live grant read then the mint", requests)
	}
}

// What the forge refuses comes back typed, with its own reason: an unknown
// workflow (404), a workflow declaring no workflow_dispatch trigger (422).
func TestWorkflowDispatch_TheForgesRefusalIsReportedTyped(t *testing.T) {
	for _, tc := range []struct{ workflow, want string }{
		{"nope.yml", "not found"},
		{"ci.yml", "workflow_dispatch"},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			w := newDispatchWorld(t)
			token, _ := w.scheduleGrantFor(t, "workflow-kick", "run-refused", tc.workflow, store.RunTrustDefault)
			code, out, raw := w.dispatch(t, token, "run-refused", "acme/widgets", tc.workflow)
			if code != http.StatusUnprocessableEntity || out.Code != dispatchRefusedByForge || out.Dispatched || !strings.Contains(out.Reason, tc.want) {
				t.Fatalf("dispatch of %s = %d %s, want 422 %s naming %q", tc.workflow, code, raw, dispatchRefusedByForge, tc.want)
			}
			if _, dispatches, _ := w.f.snapshot(); len(dispatches) != 0 {
				t.Fatalf("forge dispatches = %+v, want none", dispatches)
			}
		})
	}
}

// The operator's disarm reaches the schedule lane: forge_publish_mutations
// pinned to anything but "true" on the integration (or the schedule row)
// mints no dispatch grant.
func TestWorkflowDispatch_ForgePublishMutationsDisarmsTheScheduleGrant(t *testing.T) {
	t.Run("on the integration", func(t *testing.T) {
		w := newDispatchWorld(t)
		ri, err := w.s.forgeIntegrations.Get(context.Background(), "integ1")
		if err != nil {
			t.Fatal(err)
		}
		ri.LaunchVars = map[string]string{publishMutationKillSwitchVar: "false"}
		if err := w.s.forgeIntegrations.Update(context.Background(), ri); err != nil {
			t.Fatal(err)
		}
		_, minted, err := w.tick(t, "workflow-kick", "sch-off", map[string]string{"workflow": "renovate.yml"})
		if err != nil || minted != "" {
			t.Fatalf("disarmed integration minted (%q, %v), want nothing", minted, err)
		}
	})
	t.Run("on the schedule row", func(t *testing.T) {
		w := newDispatchWorld(t)
		_, minted, err := w.tick(t, "workflow-kick", "sch-off", map[string]string{"workflow": "renovate.yml", publishMutationKillSwitchVar: "off"})
		if err != nil || minted != "" {
			t.Fatalf("disarmed schedule row minted (%q, %v), want nothing", minted, err)
		}
	})

	// The pin an operator PATCHes onto the webhook config — the half the
	// orchestrator treats as authoritative — is the pull-request lane's
	// reading (repoLaunchPolicy); the schedule lane reads the same one.
	//
	// Mutation witnessed: scheduledDispatchGrant reading integ.LaunchVars
	// instead of repoLaunchPolicy → the schedule mints its dispatch grant
	// while the pull-request lane is disarmed → red.
	t.Run("on the webhook config", func(t *testing.T) {
		w := newDispatchWorld(t)
		cfgs := webhooks.NewMemoryConfigStore()
		w.s.webhookConfigs = cfgs
		if err := cfgs.Create(context.Background(), webhooks.Config{
			ID: "wh1", TenantID: "team1", LaunchVars: map[string]string{publishMutationKillSwitchVar: "false"},
		}); err != nil {
			t.Fatal(err)
		}
		ri, err := w.s.forgeIntegrations.Get(context.Background(), "integ1")
		if err != nil {
			t.Fatal(err)
		}
		ri.WebhookID = "wh1"
		if err := w.s.forgeIntegrations.Update(context.Background(), ri); err != nil {
			t.Fatal(err)
		}
		if got := w.s.repoLaunchPolicy(context.Background(), ri, "workflow-kick")[publishMutationKillSwitchVar]; got != "false" {
			t.Fatalf("the fixture's pull-request lane reads %s=%q, want \"false\" — it must be disarmed to witness the schedule lane", publishMutationKillSwitchVar, got)
		}
		_, minted, err := w.tick(t, "workflow-kick", "sch-off", map[string]string{"workflow": "renovate.yml"})
		if err != nil || minted != "" {
			t.Fatalf("a schedule on an integration disarmed through its webhook config minted (%q, %v), want nothing", minted, err)
		}
	})
}

// The run's OWN forge token is unchanged by the dispatch lane: an
// installation that grants actions:write still mints the run token (and the
// management token) without it — the per-bot opt-in is the only way to the
// Actions permission (operator decision, 10/11).
//
// Mutation witnessed: actions added to RuntimePermissionsFor's extras → red.
func TestWorkflowDispatch_TheRunTokenNeverCarriesActions(t *testing.T) {
	base := map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read", "repository_hooks": "write", "workflows": "write", "checks": "read", "statuses": "read"}
	with := maps.Clone(base)
	with["actions"] = "write"
	if got, want := forgegithub.RuntimePermissionsFor(with), forgegithub.RuntimePermissionsFor(base); !maps.Equal(got, want) {
		t.Fatalf("RuntimePermissionsFor with actions granted = %v, want it identical to without (%v)", got, want)
	}
	if got, want := forgegithub.ManagementPermissionsFor(with), forgegithub.ManagementPermissionsFor(base); !maps.Equal(got, want) {
		t.Fatalf("ManagementPermissionsFor with actions granted = %v, want it identical to without (%v)", got, want)
	}

	// The real provisioning mint, against the installation that grants it.
	w := newDispatchWorld(t)
	if _, err := w.s.forgeAppMinter(context.Background(), w.conn); err != nil {
		t.Fatalf("forgeAppMinter: %v", err)
	}
	mints, _, _ := w.f.snapshot()
	if len(mints) != 1 {
		t.Fatalf("mints = %+v, want the managed token's one", mints)
	}
	if _, ok := mints[0].Permissions["actions"]; ok {
		t.Fatalf("the run's managed forge token asked for %v — it must never carry actions", mints[0].Permissions)
	}
}

// kickProbeManifest is a bundle manifest declaring the actions scope; the
// probe's main.bot is the spine probe.
const kickProbeManifest = "name: kick\nversion: 0.1.0\nschema_version: 1\nforge:\n  token_scopes:\n    actions: write\n"

// launchScheduledBot hands a dispatch bot its grant: the spec the run
// service is asked to launch carries the dispatch endpoint and the bearer,
// and the bearer is a live grant on the schedule's integration carrying the
// dispatch capability. A bot that does not declare the scope launches with
// neither, as every scheduled launch did before.
//
// Mutation witnessed: the scheduledDispatchGrant call removed from
// launchScheduledBot → the dispatch bot launches with no grant → red.
func TestLaunchScheduledBot_ADispatchBotLaunchesHoldingItsGrant(t *testing.T) {
	pub := &spinePublisher{}
	s := newForgeTestServer(t)
	seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(spineProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(botsDir, "kick"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"main.bot": spineProbeBot, "manifest.yaml": kickProbeManifest} {
		if err := os.WriteFile(filepath.Join(botsDir, "kick", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
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

	spec := fire("kick")
	if got := spec.Vars[forgePublishVarDispatch]; got != "https://iterion.example"+forgeDispatchWorkflowPath {
		t.Fatalf("launched %s = %q, want the dispatch endpoint (vars %v)", forgePublishVarDispatch, got, store.RedactLaunchStringVars(spec.Vars))
	}
	g, ok := s.forgePublishTokens.lookup(spec.Vars[forgePublishVarToken])
	if !ok || !slices.Equal(g.Capabilities, []string{publishCapDispatchWorkflow}) || g.Repo != "acme/widgets" || g.ConnectionID != "c1" {
		t.Fatalf("launched bearer resolves to (%+v, %v), want a live grant on acme/widgets via c1 carrying %s", g, ok, publishCapDispatchWorkflow)
	}

	spec = fire("probe")
	for _, name := range forgePublishVars() {
		if v, ok := spec.Vars[name]; ok {
			t.Fatalf("a bot without the actions scope launched with %s=%q — a schedule mints a grant for the dispatch capability only", name, v)
		}
	}
}

// failingLoadStore is a run store that cannot answer a read.
type failingLoadStore struct{ store.RunStore }

func (failingLoadStore) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, fmt.Errorf("mongo: connection reset")
}

// A run store that cannot answer says nothing about the run: the dispatch
// answers 503 workflow_dispatch_unavailable rather than refusing the run as a
// stranger to its grant, and nothing reaches the forge.
//
// Mutations witnessed: the store-failure branch removed → the read failure is
// answered 403 workflow_dispatch_run_not_grant_holder → red;
// dispatchUnavailable respelled → red on error_code.
func TestWorkflowDispatch_AStoreThatCannotAnswerIsNotARefusalOfTheRun(t *testing.T) {
	w := newDispatchWorld(t)
	token, _ := w.scheduleGrant(t, "workflow-kick", "run-blind", store.RunTrustDefault)
	w.s.cfg.Store = failingLoadStore{w.runs}
	code, out, raw := w.postDispatch(t, token, `{"run_id":"run-blind","repo":"acme/widgets","workflow":"renovate.yml"}`)
	// The wire spelling, not the constant.
	if code != http.StatusServiceUnavailable || out["error_code"] != "workflow_dispatch_unavailable" {
		t.Fatalf("dispatch over a store that cannot answer = %d %s, want 503 error_code workflow_dispatch_unavailable and no verdict about the run", code, raw)
	}
	w.assertForgeUntouched(t)
	if c := w.claims(t, token); len(c) != 0 {
		t.Errorf("a blind store left claims %v on the grant", c)
	}
}
