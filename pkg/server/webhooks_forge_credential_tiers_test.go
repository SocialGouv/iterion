package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// seedCoveringIntegration provisions the repo-integration row that makes a
// connection PROVABLY cover a repo — what forge.Orchestrator.Provision writes
// when an operator connects a repo. Without it a connection is only "some
// connection on this host", which the webhook lanes rank BELOW the webhook's
// own forge_token binding.
func seedCoveringIntegration(t *testing.T, s *Server, connID, repo string) {
	t.Helper()
	if s.forgeIntegrations == nil {
		s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		ID:           "ri-" + connID,
		TenantID:     "t1",
		ConnectionID: connID,
		RepoFullName: repo,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// unrelatedConnWorld is the shape the connection-first resolution regressed
// on: the team holds a runtime connection on the SAME forge host that has no
// integration for this repo (another org, a repo connected long ago), while
// the webhook carries the forge_token binding an operator bound for THIS
// repo. Nothing distinguishes the two credentials by construction — only the
// integration row does.
//
// The unrelated connection's credential is BLIND to this repo: every repo
// endpoint answers 404, which is exactly what GitHub answers a credential
// that cannot see a repository, and pkg/forge/github maps that 404 to
// permission "none" — a SUCCESSFUL answer. That is what makes the tier order
// load-bearing rather than cosmetic: there is no error for a
// connection-preferring lane to fall back on, so it would silently rank every
// commenter at 0 and refuse them.
func unrelatedConnWorld(t *testing.T) (*Server, *fakeGitHubForge, webhooks.Config, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	f := newFakeGitHubForge(t)
	f.perms["maintainer-jane"] = "maintain"
	f.blindBearer = "Bearer ghp_unrelated_org"

	conn := forge.Connection{
		ID: "c-other-org", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindPAT,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		CreatedAt: time.Now().UTC(),
	}
	sealed, err := forge.SealPAT(s.sealer, conn.ID, "ghp_unrelated_org")
	if err != nil {
		t.Fatal(err)
	}
	conn.SealedPayload = sealed
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	// Deliberately NO integration row for acme/widgets: this connection
	// covers some other repo on the same host.
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()

	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = f.srv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	seedForgeToken(t, s, &cfg, "ghp_bound_for_this_repo")
	return s, f, cfg, pt
}

// A connection that does not cover the repo must not outrank the webhook's
// own forge_token binding on the /revi approve lane: the role read and the
// status write both ride the binding, and the approve lands.
func TestForgeCredentialTiers_ApproveUsesTheBindingOverAnUnrelatedConnection(t *testing.T) {
	s, f, cfg, pt := unrelatedConnWorld(t)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	statuses, comments := f.snapshot()
	if len(statuses) != 1 || statuses[0]["state"] != "success" {
		t.Fatalf("the approve must land through the bound token, got statuses=%v comments=%v body=%s", statuses, comments, w.Body.String())
	}
	for _, b := range append(f.bearersFor("permission"), f.bearersFor("status")...) {
		if b == "Bearer ghp_unrelated_org" {
			t.Errorf("a connection with no integration for this repo served a read/write the binding was bound for")
		}
	}
}

// Same shape on the /command lane: an authorized collaborator must not be
// refused because an unrelated connection answered "none" for them.
func TestForgeCredentialTiers_CommandUsesTheBindingOverAnUnrelatedConnection(t *testing.T) {
	s, f, cfg, pt := unrelatedConnWorld(t)
	f.perms["dev-dan"] = "write"
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{"billy": {{BotID: "branch-improve-loop"}}}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-billy", nil
	}

	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},` +
		`"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},` +
		`"comment":{"id":901,"body":"/billy fix it","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-901"},"sender":{"login":"dev-dan"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if launched != 1 {
		t.Fatalf("an authorized commenter must not be refused by an unrelated connection's \"none\": launched=%d code=%d body=%s", launched, w.Code, w.Body.String())
	}
	for _, b := range f.bearersFor("permission") {
		if b == "Bearer ghp_unrelated_org" {
			t.Errorf("the role read went through the unrelated connection instead of the binding")
		}
	}
}

// The tier order itself, without the HTTP lanes: a connection PROVEN to cover
// the repo outranks the binding; without that proof the binding wins; with
// neither an integration row nor a binding, the host-wide connection is still
// used — the zero-config org-wide App install, which has no row and no
// binding, must keep working.
func TestForgeCredentialTiers_ResolutionOrder(t *testing.T) {
	newWorld := func(t *testing.T, withIntegration, withBinding bool) (*Server, webhooks.Config, *fakeGitHubForge) {
		t.Helper()
		s := newWebhookTestServer(t)
		f := newFakeGitHubForge(t)
		conn := forge.Connection{
			ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindPAT,
			Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
			CreatedAt: time.Now().UTC(),
		}
		sealed, err := forge.SealPAT(s.sealer, conn.ID, "ghp_conn")
		if err != nil {
			t.Fatal(err)
		}
		conn.SealedPayload = sealed
		conns := forge.NewMemoryConnectionStore()
		if err := conns.Create(context.Background(), conn); err != nil {
			t.Fatal(err)
		}
		s.forgeConnections = conns
		s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
		if withIntegration {
			seedCoveringIntegration(t, s, "c1", "acme/widgets")
		}
		cfg, _ := ghConfig(t, s)
		cfg.ForgeBaseURL = f.srv.URL
		if withBinding {
			seedForgeToken(t, s, &cfg, "ghp_binding")
		}
		return s, cfg, f
	}
	// servedBy names the credential the resolved client actually presents.
	servedBy := func(t *testing.T, s *Server, cfg webhooks.Config, f *fakeGitHubForge) string {
		t.Helper()
		api, refusal := s.prforgeReplierAPIFor(context.Background(), cfg, webhooks.ProviderGitHub, f.srv.URL, "acme/widgets", "review-pr")
		if refusal != "" {
			t.Fatalf("no client resolved: %s", refusal)
		}
		if _, err := api.CollaboratorPermission(context.Background(), "acme/widgets", "maintainer-jane"); err != nil {
			t.Fatal(err)
		}
		bearers := f.bearersFor("permission")
		if len(bearers) == 0 {
			t.Fatal("no permission call recorded")
		}
		return bearers[len(bearers)-1]
	}

	s, cfg, f := newWorld(t, true, true)
	if got := servedBy(t, s, cfg, f); got != "Bearer ghp_conn" {
		t.Errorf("a covering connection must outrank the binding, served by %q", got)
	}
	s, cfg, f = newWorld(t, false, true)
	if got := servedBy(t, s, cfg, f); got != "Bearer ghp_binding" {
		t.Errorf("without proof of coverage the binding must serve, served by %q", got)
	}
	s, cfg, f = newWorld(t, false, false)
	if got := servedBy(t, s, cfg, f); got != "Bearer ghp_conn" {
		t.Errorf("with no binding the host-wide connection is still the last resort, served by %q", got)
	}
}
