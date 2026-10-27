package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// scopedHeader drives scopeRunByID — the entry BOTH credential paths take —
// with an X-Iterion-Team header, so a route outside the by-id class reaches
// stampAuthedContext, where the explicit scope lives. Reports the written
// status and, when served, the identity and store tenant a handler would
// see; a refusal carries its body instead.
func scopedHeader(t *testing.T, srv *Server, method, target string, id auth.Identity, header string) (int, auth.Identity, string, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	// Mirror the production state: the request arrives carrying the
	// caller's pinned team, the resolution must replace it, not inherit.
	if id.TeamID != "" {
		req = req.WithContext(store.WithTenant(req.Context(), id.TeamID))
	}
	if header != "" {
		req.Header.Set(tenantScopeHeader, header)
	}
	rec := httptest.NewRecorder()
	ctx, ok := srv.scopeRunByID(rec, req, id)
	if !ok {
		return rec.Code, auth.Identity{}, "", rec.Body.String()
	}
	gotID, _ := auth.FromContext(ctx)
	tenant, _ := store.TenantFromContext(ctx)
	return rec.Code, gotID, tenant, ""
}

// A member of both teams, active in A, scoping to B: the request acts on B
// with the standing identityInTeam computes THERE — a member, not the admin
// the credential presented for A. The header grants nothing a team switch
// would not (#2662).
func TestScopeHeaderActsOnTheChosenTeamWithItsOwnStanding(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	id := caller("u-both", "tenant-A")
	id.Role = identity.RoleAdmin // what the A-pinned credential presents
	code, got, tenant, _ := scopedHeader(t, srv, http.MethodGet, "/api/v1/scope-probe", id, "tenant-B")
	if code != http.StatusOK {
		t.Fatalf("admin of A who is a member of B scoping to B: got %d, want 200", code)
	}
	if got.TeamID != "tenant-B" || got.Role != identity.RoleMember || got.Via != "membership" {
		t.Fatalf("identity = %+v: want the standing recomputed in tenant-B (member, via membership) — not the admin role tenant-A presented", got)
	}
	if tenant != "tenant-B" {
		t.Fatalf("store tenant = %q, want tenant-B", tenant)
	}
}

// No standing in the named team: a 403 that NAMES the header and the team —
// the same contract resolveTenantScope gives its ?team_id= form (never an
// accepted-and-dropped scope).
func TestScopeHeaderWithoutStandingIsA403NamingTheHeader(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	code, _, _, body := scopedHeader(t, srv, http.MethodGet, "/api/v1/scope-probe", caller("u-member-a", "tenant-A"), "tenant-B")
	if code != http.StatusForbidden {
		t.Fatalf("member of A only scoping to B: got %d, want 403", code)
	}
	if !strings.Contains(body, tenantScopeHeader) || !strings.Contains(body, "tenant-B") {
		t.Fatalf("body = %q: the refusal must name the header and the refused team", body)
	}
}

// A route that scopes by the team in its PATH ignores the header: the path
// is the more local scope, and the handler's own path-team authorization
// must keep ruling.
func TestScopeHeaderNeverOverridesATeamPathRoute(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	code, got, tenant, _ := scopedHeader(t, srv, http.MethodGet, "/api/teams/tenant-B/schedules", caller("u-both", "tenant-A"), "tenant-B")
	if code != http.StatusOK {
		t.Fatalf("path-scoped route with a header: got %d, want 200 (the header is inert, the stamp unchanged)", code)
	}
	if got.TeamID != "tenant-A" || tenant != "tenant-A" {
		t.Fatalf("identity team = %q store tenant = %q: on a path-scoped route the stamp must stay the caller's active team", got.TeamID, tenant)
	}
}

// A run addressed by id is the RUN's team (ADR-103): the header plays no
// part there. A caller active in B naming B reads a tenant-A run from
// tenant-A or not at all.
func TestRunByIDClassIgnoresTheScopeHeader(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	code, got, tenant, _ := scopedHeader(t, srv, http.MethodGet, "/api/runs/run-1", caller("u-both", "tenant-B"), "tenant-B")
	if code != http.StatusOK {
		t.Fatalf("member of the run's team with a matching header: got %d, want 200", code)
	}
	if tenant != "tenant-A" || got.TeamID != "tenant-A" {
		t.Fatalf("identity team = %q store tenant = %q: a by-id run is served from its own team; the header is inert in that class", got.TeamID, tenant)
	}
}

// A super-admin scoping across teams is re-anchored with full power and the
// super-admin provenance — the same shape the by-id class produces.
func TestScopeHeaderReAnchorsASuperAdmin(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	id := caller("root", "tenant-A")
	id.IsSuperAdmin = true
	code, got, tenant, _ := scopedHeader(t, srv, http.MethodGet, "/api/v1/scope-probe", id, "tenant-B")
	if code != http.StatusOK || got.TeamID != "tenant-B" || got.Role != identity.RoleAdmin || got.Via != "super-admin" || tenant != "tenant-B" {
		t.Fatalf("super-admin scoping to B: %d %+v tenant=%q — want re-anchored admin via super-admin", code, got, tenant)
	}
}

// A team-less credential is not rescued by the header: the team-less
// refusal keeps its fail-closed contract — the header chooses a scope, it
// does not mint a team.
func TestScopeHeaderDoesNotRescueATeamLessCredential(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	code, _, _, body := scopedHeader(t, srv, http.MethodGet, "/api/v1/scope-probe", caller("u-both", ""), "tenant-B")
	if code != http.StatusForbidden {
		t.Fatalf("team-less credential with a header: got %d, want 403 (the team-less refusal stands)", code)
	}
	if !strings.Contains(body, "no active team") {
		t.Fatalf("body = %q: want the team-less refusal", body)
	}
}

// The full chain a production request walks: a bearer pinned to A with the
// header B reaches a registered route stamped for B — identity and store
// tenant agree on the chosen team (the property the launch surface needs:
// the run files in the tab's team).
func TestScopeHeaderThroughRequireAuth(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	srv.mux.Handle("GET /api/v1/scope-probe", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		tenant, _ := store.TenantFromContext(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]any{"team": id.TeamID, "role": string(id.Role), "tenant": tenant})
	}))
	ts := fullStack(t, srv)
	pinned, _, err := srv.signer.IssueAccess(caller("u-both", "tenant-A"))
	if err != nil {
		t.Fatalf("mint bearer: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/scope-probe", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+pinned)
	req.Header.Set(tenantScopeHeader, "tenant-B")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Team   string `json:"team"`
		Role   string `json:"role"`
		Tenant string `json:"tenant"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || out.Team != "tenant-B" || out.Tenant != "tenant-B" || out.Role != string(identity.RoleMember) {
		t.Fatalf("full chain: %d %+v — the header must re-scope the stamp the handler reads, at member standing", resp.StatusCode, out)
	}
}
