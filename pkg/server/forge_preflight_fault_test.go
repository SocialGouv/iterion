package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// writeForgeUpstreamError is the single place all three 502-defaulting arms
// cross, so it is where a pre-flight failure stops being reported as the
// forge's. Marking alone changes nothing — forgeUpstreamStatus has no case
// for it, by design — which is exactly why this junction is asserted.
func TestWriteForgeUpstreamError_PreflightIsIterionsOwn(t *testing.T) {
	w := httptest.NewRecorder()
	err := fmt.Errorf("sign app jwt: %w", forge.ErrLocalPreflight)
	if !writeForgeUpstreamError(w, err, "security-read token mint: %v", err) {
		t.Fatal("a pre-flight failure was handed back to the caller, whose default arm answers 502")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 — the forge never saw this request", w.Code)
	}
}

// The classifier keeps its own contract: it answers what the FORGE answered,
// and the forge answered nothing. Deciding fault is the writer's job, and
// pinning that here keeps a later "simplification" from folding the 500 into
// the table, where it would outrank a genuine upstream code.
func TestForgeUpstreamStatus_PreflightIsNotAForgeAnswer(t *testing.T) {
	if code, _ := forgeUpstreamStatus(fmt.Errorf("marshal: %w", forge.ErrLocalPreflight)); code != 0 {
		t.Fatalf("forgeUpstreamStatus = %d, want 0 (not an answer from the forge)", code)
	}
}

// The composition, which is what the operator actually meets: the
// security-read arm of PATCH /forge/connections/{id}. A mint that failed
// before reaching GitHub must answer 500, not 502.
func TestSecurityReadPatch_PreflightMintAnswers500NotBadGateway(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("github: app private key is not valid PEM: %w", forge.ErrLocalPreflight)
	}
	w := patchSecurityRead(t, s, "c1", true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s, want 500 — a key iterion cannot read is not GitHub being down", w.Code, w.Body.String())
	}
}

// And the arm still tells the truth the other way round: a forge that really
// did fail keeps its 502.
func TestSecurityReadPatch_GenuineForgeFailureKeepsBadGateway(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("github said no")
	}
	w := patchSecurityRead(t, s, "c1", true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s, want 502 — an unclassified failure on this arm is still the forge's", w.Code, w.Body.String())
	}
}

// The security-read composition above drives the arm through the
// s.forgeSecurityMint seam — the arm's own injection point, but not the
// chain the other two arms take. This one takes the long way, with nothing
// injected: a real github_app connection whose STORED App key is not
// parseable PEM, through forgeAdminFor → AppClient.rest →
// MintInstallationToken → the marker → the junction. No forge is reachable
// and none is needed; that is the point. The reverse direction (a forge
// that really did fail keeps its 502) is pinned at the junction, which this
// route shares.
func TestListForgeRepos_UnreadableStoredKeyAnswers500NotBadGateway(t *testing.T) {
	s := newForgeTestServer(t)
	s.forgeOAuthApps = forge.NewMemoryOAuthAppStore()
	storeApp(t, s, "app-1", "t1", "SocialGouv", "111",
		"-----BEGIN RSA PRIVATE KEY-----\nnot base64\n-----END RSA PRIVATE KEY-----",
		time.Unix(1700000000, 0).UTC())
	conn := forge.Connection{
		ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, InstallationID: 42, OAuthAppID: "app-1",
	}
	if err := s.forgeConnections.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	req := forgeReq(superAdminCtx(), "GET", "/api/teams/t1/forge/connections/c1/repos", "", "t1")
	req.SetPathValue("conn_id", "c1")
	w := httptest.NewRecorder()
	s.handleListForgeRepos(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s, want 500 — GitHub was never asked; the key iterion stored is the one that cannot be read",
			w.Code, w.Body.String())
	}
}
