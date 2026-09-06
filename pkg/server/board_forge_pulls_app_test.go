package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// The board card's PR/CI panel reads forge.PullClient through
// pullClientForConn. On a github_app connection — the shape the connect
// wizard creates by default — every one of its routes answered 501 while
// the same card worked on a PAT connection. These probes wire the REAL App
// path end to end against a fake GitHub that applies GitHub's own rules: a
// mint asking for a permission the installation never approved is refused
// (422), and a call whose bearer lacks the gating permission is refused
// (403 "Resource not accessible by integration").

// pullPanelForge is that fake: the installation-token mint and probe, plus
// the pulls, check-runs and combined-status endpoints the panel reads.
type pullPanelForge struct {
	mu sync.Mutex
	// granted is what the installation's owner approved: the mint is refused
	// for any permission (or level) outside it.
	granted map[string]string
	// perms maps an issued token to the permission set it was minted with.
	perms map[string]map[string]string
	// refusedMints counts mints GitHub 422'd for want of a grant.
	refusedMints int
	srv          *httptest.Server
}

// grantCovers is GitHub's rule for a mint: a requested permission must be
// granted, at the requested level or above (write covers read).
func (f *pullPanelForge) grantCovers(name, level string) bool {
	got, ok := f.granted[name]
	if !ok {
		return false
	}
	return level != "write" || got == "write" || got == "admin"
}

func newPullPanelForge(t *testing.T, granted map[string]string) *pullPanelForge {
	t.Helper()
	f := &pullPanelForge{granted: granted, perms: map[string]map[string]string{}}
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	// bearerHas reports whether the caller's minted token carries name.
	bearerHas := func(r *http.Request, name string) bool {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		defer f.mu.Unlock()
		_, ok := f.perms[tok][name]
		return ok
	}
	notAccessible := func(w http.ResponseWriter) {
		reply(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by integration"})
	}
	pr := map[string]any{
		"number": 7, "title": "feat: widgets", "body": "Closes #12", "state": "open",
		"html_url": "https://forge.example/acme/widgets/pull/7",
		"head":     map[string]any{"ref": "feat/widgets", "sha": "deadbeef1234", "repo": map[string]any{"full_name": "acme/widgets"}},
		"base":     map[string]any{"ref": "main"},
		"user":     map[string]any{"login": "alice"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		for name, level := range body.Permissions {
			if !f.grantCovers(name, level) {
				f.refusedMints++
				reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "The permissions requested are not granted to this installation."})
				return
			}
		}
		tok := "ghs_" + strings.Join(sortedKeys(body.Permissions), "_")
		f.perms[tok] = body.Permissions
		reply(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("GET /api/v3/app/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply(w, http.StatusOK, map[string]any{
			"account":     map[string]any{"login": "acme"},
			"html_url":    "https://forge.example/organizations/acme/settings/installations/42",
			"permissions": f.granted,
		})
	})
	mux.HandleFunc("GET /api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]any{"repositories": []map[string]any{{"full_name": "acme/widgets"}}})
	})
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		if !bearerHas(r, "pull_requests") {
			notAccessible(w)
			return
		}
		reply(w, http.StatusOK, []map[string]any{pr})
	})
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		if !bearerHas(r, "pull_requests") {
			notAccessible(w)
			return
		}
		reply(w, http.StatusOK, pr)
	})
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if !bearerHas(r, "checks") {
			notAccessible(w)
			return
		}
		reply(w, http.StatusOK, map[string]any{"total_count": 1, "check_runs": []map[string]any{
			{"name": "build", "status": "completed", "conclusion": "success", "html_url": "https://forge.example/acme/widgets/runs/1", "started_at": "2026-01-01T00:00:00Z"},
		}})
	})
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/commits/{sha}/status", func(w http.ResponseWriter, r *http.Request) {
		if !bearerHas(r, "statuses") {
			notAccessible(w)
			return
		}
		reply(w, http.StatusOK, map[string]any{"state": "success", "sha": r.PathValue("sha"), "statuses": []map[string]any{}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *pullPanelForge) refused() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusedMints
}

// pullPanelFixture wires a server with a github_app connection pointed at the
// fake and a forge-linked card on the team's board, and returns the card id.
func pullPanelFixture(t *testing.T, f *pullPanelForge) (*Server, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	s.forgeConnections = forge.NewMemoryConnectionStore()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AppSlug: "iterion-forge-x", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	card, err := board.Create(native.Issue{
		Title:    "widgets",
		External: &native.ExternalRef{Provider: "github", ConnectionID: "c-app", Repo: "acme/widgets", Number: 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, card.ID
}

func pullPanelReq(method, path, cardID, number string) *http.Request {
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1"})
	r := httptest.NewRequest(method, path, nil).WithContext(ctx)
	r.SetPathValue("id", cardID)
	if number != "" {
		r.SetPathValue("number", number)
	}
	return r
}

// The panel WORKS on a github_app connection whose installation approved
// every grant the manifest requests: the linked PR is listed and its CI
// state is read — no 501 anywhere on the path.
func TestCardPullPanelServesOnAGitHubAppConnection(t *testing.T) {
	f := newPullPanelForge(t, map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"repository_hooks": "write", "statuses": "write", "checks": "read",
	})
	s, cardID := pullPanelFixture(t, f)

	w := httptest.NewRecorder()
	s.handleListIssuePulls(w, pullPanelReq("GET", "/api/v1/native/issues/"+cardID+"/pulls", cardID, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list pulls on a github_app connection: code=%d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Pulls []forge.PullRef `json:"pulls"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Pulls) != 1 || list.Pulls[0].Number != 7 {
		t.Fatalf("pulls = %+v, want the PR that closes the card's issue #12", list.Pulls)
	}

	w = httptest.NewRecorder()
	s.handleIssuePullCI(w, pullPanelReq("GET", "/api/v1/native/issues/"+cardID+"/pulls/7/ci", cardID, "7"))
	if w.Code != http.StatusOK {
		t.Fatalf("pull CI on a github_app connection: code=%d body=%s", w.Code, w.Body.String())
	}
	var ci struct {
		Status forge.CIStatus `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ci); err != nil {
		t.Fatal(err)
	}
	if ci.Status.State != forge.CISuccess || len(ci.Status.Runs) != 1 {
		t.Fatalf("ci status = %+v, want the green check-run the forge served", ci.Status)
	}
	if f.refused() != 0 {
		t.Errorf("%d mint(s) refused on a fully-granted installation: a profile asks for more than the manifest requests", f.refused())
	}
}

// An installation approved BEFORE checks:read was requested — every
// installation that predates the grant — must not answer an opaque 502/403:
// the panel names the permission and the step that closes the gap, and the
// connection health view reports the same gap before anyone opens a card.
func TestCardPullPanelNamesTheMissingChecksGrant(t *testing.T) {
	f := newPullPanelForge(t, map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"repository_hooks": "write", "statuses": "write", // no checks: the pre-existing installation shape
	})
	s, cardID := pullPanelFixture(t, f)

	// The PR half still works: it needs no grant the installation lacks.
	w := httptest.NewRecorder()
	s.handleListIssuePulls(w, pullPanelReq("GET", "/api/v1/native/issues/"+cardID+"/pulls", cardID, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list pulls must not depend on checks:read: code=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleIssuePullCI(w, pullPanelReq("GET", "/api/v1/native/issues/"+cardID+"/pulls/7/ci", cardID, "7"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a withheld grant is a configuration gap on the connection, want 422, got %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"checks:read", "approve", "settings/installations/42"} {
		if !strings.Contains(body.Error, want) {
			t.Errorf("error = %q, want it to carry %q (the permission, the step, the page)", body.Error, want)
		}
	}
	if strings.Contains(body.Error, "does not implement") {
		t.Errorf("error = %q reads as a capability miss; the client serves the capability, the installation lacks a grant", body.Error)
	}
	if f.refused() == 0 {
		t.Error("the CI profile must have asked GitHub for checks:read and been refused — the typed error comes from that refusal")
	}

	// The health probe reports the same gap, so an operator sees it on the
	// connection before a card ever shows a dead panel.
	req := forgeReq(superAdminCtx(), "GET", "/api/teams/t1/forge/connections/c-app/health", "", "t1")
	req.SetPathValue("conn_id", "c-app")
	w = httptest.NewRecorder()
	s.handleForgeConnectionHealth(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("health: code=%d body=%s", w.Code, w.Body.String())
	}
	var h forgeConnectionHealth
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if len(h.MissingCIPermissions) != 1 || h.MissingCIPermissions[0] != "checks" {
		t.Errorf("missing_ci_permissions = %v, want [checks] — the grant the card CI panel needs and the installation lacks", h.MissingCIPermissions)
	}
	if h.ManageInstallURL == "" {
		t.Error("the health view must hand over the installation page: it is where the grant is approved")
	}
}
