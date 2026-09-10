package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The third arm, and the one neither the mint-chain table nor the list-repos
// composition reaches. `rest` and `scopedREST` are pinned, but a handler calls
// AppClient.GetPullRequest, which does its own `c, err := a.scopedREST(ctx)`
// and returns the error — an equally natural place to write
// fmt.Errorf("get pull: %v", err) and flatten the sentinel. The list-repos arm
// has its own end-to-end test; without this one, a re-wrap there would land
// green and put the 502 inversion back on the PR-read route alone.
//
// Nothing is injected: the gate client is the real one, built from a stored
// App whose key is not parseable PEM. No forge is reachable and none is
// needed — the mint fails before a socket, and the route must answer 500.
func TestForgePullRequest_UnreadableStoredKeyAnswers500NotBadGateway(t *testing.T) {
	s := newForgeTestServer(t)
	s.forgeOAuthApps = forge.NewMemoryOAuthAppStore()
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	storeApp(t, s, "app-1", "team1", "SocialGouv", "111",
		"-----BEGIN RSA PRIVATE KEY-----\nnot base64\n-----END RSA PRIVATE KEY-----",
		time.Unix(1700000000, 0).UTC())
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn1", TenantID: "team1", Provider: forge.ProviderGitHub,
		Kind: forge.KindGitHubApp, Status: forge.StatusActive,
		InstallationID: 42, OAuthAppID: "app-1",
	}); err != nil {
		t.Fatal(err)
	}
	registerPublishToken(t, s, "tok1", ForgePublishGrant{
		TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
	})

	w := httptest.NewRecorder()
	s.handleForgePullRequest(w, prStateReq("tok1", "https://github.com/o/r/pull/42"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s, want 500 — GitHub was never asked; the key iterion stored is the one that cannot be read",
			w.Code, w.Body.String())
	}
}
