package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// patchSecurityRead drives the real handler for one connection.
func patchSecurityRead(t *testing.T, s *Server, connID string, enable bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"security_read_enabled":false}`
	if enable {
		body = `{"security_read_enabled":true}`
	}
	req := forgeReq(superAdminCtx(), "PATCH", "/api/teams/t1/forge/connections/"+connID, body, "t1")
	req.SetPathValue("conn_id", connID)
	w := httptest.NewRecorder()
	s.handlePatchForgeConnection(w, req)
	return w
}

func seedAppConn(t *testing.T, s *Server, id, org, host string, enabled bool) {
	t.Helper()
	conn := forge.Connection{
		ID: id, TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, AccountLogin: "iterion-forge-x[bot]",
		InstallationAccount: org, InstallationID: 42, ForgeBaseURL: host,
		SecurityReadEnabled: enabled,
	}
	if err := s.forgeConnections.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}

func securityReadMapFor(t *testing.T, s *Server) (map[string]string, bool) {
	t.Helper()
	list, err := s.genericSecrets.ListByTeam(context.Background(), "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range list {
		if sec.Name != forge.SecurityReadSecretName || sec.ScopeUserID != "" {
			continue
		}
		plain, err := secrets.OpenGenericSecret(s.sealer, sec.ID, sec.SealedSecret)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(plain, &m); err != nil {
			t.Fatalf("secret is not a JSON map: %v", err)
		}
		return m, true
	}
	return nil, false
}

// TestSecurityReadPatch_EnableThenDisable covers the endpoint end to end: the
// mint lands under the ORG key (not the App's bot handle), the flag is
// persisted, and disabling withdraws the entry.
func TestSecurityReadPatch_EnableThenDisable(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	minted := 0
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, error) {
		minted++
		return "ghs_minted", nil
	}

	if w := patchSecurityRead(t, s, "c1", true); w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	if minted != 1 {
		t.Fatalf("mint calls = %d, want 1 (the endpoint mints immediately so a missing grant answers now)", minted)
	}
	m, ok := securityReadMapFor(t, s)
	if !ok || m["socialgouv"] != "ghs_minted" {
		t.Fatalf("map = %v (present=%v), want the ORG key", m, ok)
	}
	conn, _ := s.forgeConnections.Get(context.Background(), "c1")
	if !conn.SecurityReadEnabled {
		t.Fatal("the flag must be persisted")
	}
	// No credential in the response body.
	if body := patchSecurityRead(t, s, "c1", true).Body.String(); strings.Contains(body, "ghs_minted") {
		t.Fatalf("the response leaked the token: %s", body)
	}

	if w := patchSecurityRead(t, s, "c1", false); w.Code != http.StatusOK {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := securityReadMapFor(t, s); ok {
		t.Fatal("disabling must withdraw the entry (map emptied → secret gone)")
	}
	conn, _ = s.forgeConnections.Get(context.Background(), "c1")
	if conn.SecurityReadEnabled {
		t.Fatal("the flag must be cleared")
	}
}

// TestSecurityReadPatch_RefusesAShadowingPersonalSecret pins the 409: a
// personal secret of the same name outranks the team map at resolution, so
// enabling would mint into something that member's runs never read.
func TestSecurityReadPatch_RefusesAShadowingPersonalSecret(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, error) {
		t.Fatal("the mint must not run when the name is shadowed")
		return "", nil
	}
	id := secrets.NewGenericSecretID()
	sealed, _ := secrets.SealGenericSecret(s.sealer, id, []byte(`{"socialgouv":"ghp_member"}`))
	if err := s.genericSecrets.Create(context.Background(), secrets.GenericSecret{
		ID: id, TenantID: "t1", ScopeTeamID: "t1", ScopeUserID: "u-member",
		Name: forge.SecurityReadSecretName, SealedSecret: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	w := patchSecurityRead(t, s, "c1", true)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "shadow") {
		t.Fatalf("the refusal must explain the shadowing: %s", w.Body.String())
	}
}

// TestSecurityReadPatch_RefusesACrossHostOrgCollision pins the other 409: the
// map is keyed by org alone, so a private instance's token must not be filed
// where the public one is read.
func TestSecurityReadPatch_RefusesACrossHostOrgCollision(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "acme", "", true)                          // github.com, already enabled
	seedAppConn(t, s, "c2", "acme", "https://ghe.corp.example", false) // same org, other host
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, error) {
		t.Fatal("the mint must not run on a colliding org")
		return "", nil
	}
	w := patchSecurityRead(t, s, "c2", true)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "keyed by org") {
		t.Fatalf("the refusal must name the cause: %s", w.Body.String())
	}
}

// TestSecurityReadPatch_RollsBackWhenThePersistFails pins the compensation: a
// token in the map with the flag still false is an orphan no lifecycle owns —
// it dies within the hour and the bot then fails on a 401 with no trail.
func TestSecurityReadPatch_RollsBackWhenThePersistFails(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "c1", "SocialGouv", "", false)
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, error) {
		return "ghs_minted", nil
	}
	s.forgeConnections = failingUpdateConnStore{s.forgeConnections}

	w := patchSecurityRead(t, s, "c1", true)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", w.Code)
	}
	if m, ok := securityReadMapFor(t, s); ok {
		t.Fatalf("the minted token must be rolled back, found %v", m)
	}
}

// failingUpdateConnStore lets every read through and fails every write — the
// "persisted the mint, could not persist the flag" window.
type failingUpdateConnStore struct{ forge.ConnectionStore }

func (f failingUpdateConnStore) Update(context.Context, forge.Connection) error {
	return errUpdateRefused
}

var errUpdateRefused = &updateRefusedError{}

type updateRefusedError struct{}

func (*updateRefusedError) Error() string { return "store refused the update" }
