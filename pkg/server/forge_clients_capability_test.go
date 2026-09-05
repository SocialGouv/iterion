package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// forgeAdminFor is the ONE construction every capability assertion downstream
// reads: the board sync, the push-to-forge routes, the autofix lane and the
// gate notice all do `admin.(forge.SomeCapability)` on its result. A
// capability missing from the client that construction returns is invisible
// until the assertion fails at runtime — on the App connection shape the
// studio's connect wizard creates BY DEFAULT, which is how the forge→board
// sync spent every 5-minute tick logging "provider github has no issue
// client" instead of hydrating a single card.
//
// These probes pin the capabilities per connection kind. They need no
// network: the App client is lazy (it mints on the first call), so what is
// asserted here is exactly what the server can see before any I/O.

func appConnectionFixture(t *testing.T) (*Server, forge.Connection) {
	t.Helper()
	s := newWebhookTestServer(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	conn := forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeRuntime,
		InstallationID: 42, AppSlug: "iterion-forge-x", CreatedAt: time.Now().UTC(),
	}
	return s, conn
}

// The defect this file exists for: a GitHub-App connection must yield a
// client the forge→board sync can use.
func TestForgeAdminForGitHubAppIsAnIssueClient(t *testing.T) {
	s, conn := appConnectionFixture(t)
	admin, err := s.forgeAdminFor(context.Background(), conn)
	if err != nil {
		t.Fatalf("forgeAdminFor: %v", err)
	}
	if _, ok := admin.(forge.IssueClient); !ok {
		t.Fatalf("a %s connection yields %T, which does not implement forge.IssueClient: "+
			"syncOneIntegration fails its assertion and the board is never hydrated", conn.Kind, admin)
	}
}

// The class the defect belongs to: every capability a pkg/server lane asserts
// on forgeAdminFor's result. A gap here is an operator-visible dead lane on
// App connections, so it must be a deliberate, named decision — not a
// discovery in production.
func TestForgeAdminForGitHubAppCapabilityMatrix(t *testing.T) {
	s, conn := appConnectionFixture(t)
	admin, err := s.forgeAdminFor(context.Background(), conn)
	if err != nil {
		t.Fatalf("forgeAdminFor: %v", err)
	}
	served := map[string]bool{
		"IssueClient":        assertsAs[forge.IssueClient](admin),
		"PermissionClient":   assertsAs[forge.PermissionClient](admin),
		"ReviewClient":       assertsAs[forge.ReviewClient](admin),
		"CommitStatusClient": assertsAs[forge.CommitStatusClient](admin),
		"CommitStatusLister": assertsAs[forge.CommitStatusLister](admin),
		"RepoCreator":        assertsAs[forge.RepoCreator](admin),
		"BoardClient":        assertsAs[forge.BoardClient](admin),
	}
	for name, ok := range served {
		if !ok {
			t.Errorf("a github_app connection must serve forge.%s; %T does not", name, admin)
		}
	}
	// PullClient is the KNOWN App-connection gap, asserted so it cannot
	// change in either direction unnoticed. The App client serves
	// GetPullRequest but not the list/create/merge/CI half, so the board
	// card's PR panel (GET|POST /api/v1/native/issues/{id}/pulls,
	// .../pulls/{n}/ci, .../pulls/{n}/merge) answers 501 on a github_app
	// connection while it works on a PAT one.
	//
	// It is not the same shape of fix as IssueClient: GetCIStatus reads
	// /commits/{ref}/check-runs, which needs `checks: read` — a permission
	// neither the runtime baseline nor the App manifest requests, so closing
	// the gap means a manifest change and an org re-approval per
	// installation, not a delegation. When that lands, delete this assertion.
	if assertsAs[forge.PullClient](admin) {
		t.Errorf("forge.PullClient is now served on github_app (%T) — the gap closed: "+
			"drop this assertion and the 501 note it pins", admin)
	}
}

func assertsAs[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

// The forge→board sync is the ONE lane every supported credential shape has to
// reach: without it a team's cards are never hydrated and the project pass
// reads skipped_no_card forever. So the conformance is over the whole matrix
// forgeAdminFor can build — provider × connection kind — not over the one
// shape a bug was reported on. A new provider or a new kind lands here before
// it lands in production.
func TestForgeAdminForEveryConnectionShapeIsAnIssueClient(t *testing.T) {
	s := newWebhookTestServer(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	sealed, err := forge.SealPAT(s.sealer, "c-1", "ghp_fixture")
	if err != nil {
		t.Fatal(err)
	}

	for _, provider := range []forge.Provider{forge.ProviderGitHub, forge.ProviderGitLab, forge.ProviderForgejo} {
		for _, kind := range []forge.Kind{forge.KindPAT, forge.KindOAuthApp, forge.KindGitHubApp} {
			if kind == forge.KindGitHubApp && provider != forge.ProviderGitHub {
				continue // a GitHub App is a GitHub-only credential shape
			}
			t.Run(string(provider)+"/"+string(kind), func(t *testing.T) {
				conn := forge.Connection{
					ID: "c-1", TenantID: "t1", Provider: provider, Kind: kind,
					Status: forge.StatusActive, Purpose: forge.PurposeRuntime,
					ForgeBaseURL: forge.DefaultBaseURL(provider), InstallationID: 42,
					AppSlug: "iterion-forge-x", SealedPayload: sealed,
					CreatedAt: time.Now().UTC(),
				}
				admin, err := s.forgeAdminFor(context.Background(), conn)
				if err != nil {
					t.Fatalf("forgeAdminFor: %v", err)
				}
				if _, ok := admin.(forge.IssueClient); !ok {
					t.Fatalf("%s/%s yields %T, which does not implement forge.IssueClient: "+
						"the forge→board sync cannot run on this connection shape", provider, kind, admin)
				}
			})
		}
	}
}
