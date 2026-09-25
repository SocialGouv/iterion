package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newRunScopeServer is a guarded run store (run-1 belongs to tenant-A) in
// front of a REAL auth service over a seeded memory identity store, so the
// choke point's authorization runs against actual memberships:
//
//	org-1
//	  tenant-A  (org-1): run-1 lives here
//	  tenant-B  (org-1)
//	u-member-a     member of tenant-A only
//	u-both         admin of tenant-A AND member of tenant-B
//	u-viewer-a     viewer of tenant-A
//	u-configed-a   config_editor of tenant-A (no rung — ADR-078)
//	u-orgadmin     admin of org-1, NO team membership anywhere
func newRunScopeServer(t *testing.T) (*Server, *tenantGuardStore) {
	t.Helper()
	srv, _ := newTestServer(t)
	orig := srv.runs
	t.Cleanup(func() { srv.runs = orig })

	realStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := realStore.CreateRun(ctx, "run-1", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// The filesystem store knows no tenants; in cloud mode the run carries
	// its team. Pin it the way the mongo store would have on CreateRun.
	if run, err := realStore.LoadRun(ctx, "run-1"); err != nil {
		t.Fatalf("load seeded run: %v", err)
	} else {
		run.TenantID = "tenant-A"
		if err := realStore.SaveRun(ctx, run); err != nil {
			t.Fatalf("pin run tenant: %v", err)
		}
	}
	guard := &tenantGuardStore{FilesystemRunStore: realStore, runTenant: "tenant-A"}
	srv.runs = newTestRunviewService(t, srv.cfg.StoreDir, runview.WithStore(guard))

	mem := identity.NewMemoryStore()
	if _, err := mem.CreateOrg(ctx, identity.Org{ID: "org-1", Name: "Org One"}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	for _, team := range []identity.Team{
		{ID: "tenant-A", Slug: "team-a", Name: "Team A", OrgID: "org-1"},
		{ID: "tenant-B", Slug: "team-b", Name: "Team B", OrgID: "org-1"},
	} {
		if _, err := mem.CreateTeam(ctx, team); err != nil {
			t.Fatalf("seed team: %v", err)
		}
	}
	seeds := []identity.Membership{
		{UserID: "u-member-a", TeamID: "tenant-A", Role: identity.RoleMember},
		{UserID: "u-member-b", TeamID: "tenant-B", Role: identity.RoleMember},
		{UserID: "u-both", TeamID: "tenant-A", Role: identity.RoleAdmin},
		{UserID: "u-both", TeamID: "tenant-B", Role: identity.RoleMember},
		{UserID: "u-viewer-a", TeamID: "tenant-A", Role: identity.RoleViewer},
		{UserID: "u-configed-a", TeamID: "tenant-A", Role: identity.RoleConfigEditor},
	}
	for _, mb := range seeds {
		if err := mem.UpsertMembership(ctx, mb); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	if err := mem.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: "u-orgadmin", OrgID: "org-1", Role: identity.OrgRoleAdmin}); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      mem,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	srv.authSvc = svc
	srv.signer = signer // the bearer path of requireAuth
	return srv, guard
}

// fullStack serves the AUTH middleware in front of the mux — the chain a
// production request walks — so a wiring that never reaches the choke
// point (or a choke point that never reaches the mux) turns red.
func fullStack(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.requireAuth(srv.mux))
	t.Cleanup(ts.Close)
	return ts
}

// scoped runs scopeRunByID for one caller and reports the written status
// plus, when served, the identity and store tenant the handler would see.
func scoped(t *testing.T, srv *Server, method, target string, id auth.Identity) (int, auth.Identity, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	// The choke point runs BEFORE the middleware stamps the tenant, so a
	// plain request context carries none — and a wiring that forgot to
	// clear it would look correct. The request therefore arrives carrying
	// the CALLER's pinned team (the state an adjacent middleware could
	// produce): the resolution must clear it, not inherit it.
	if id.TeamID != "" {
		req = req.WithContext(store.WithTenant(req.Context(), id.TeamID))
	}
	rec := httptest.NewRecorder()
	ctx, ok := srv.scopeRunByID(rec, req, id)
	if !ok {
		return rec.Code, auth.Identity{}, ""
	}
	gotID, _ := auth.FromContext(ctx)
	tenant, _ := store.TenantFromContext(ctx)
	return rec.Code, gotID, tenant
}

func caller(userID string, teamID string) auth.Identity {
	return auth.Identity{UserID: userID, Email: userID + "@x", TeamID: teamID, Role: identity.RoleMember}
}

// The choke point serves a run from the RUN's team: a caller whose
// credential is pinned to team B but who is a member of team A gets
// run-1 re-scoped to tenant-A — identity and store tenant agree on it.
func TestRunByIDIsServedFromTheRunsTeam(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	code, got, tenant := scoped(t, srv, http.MethodGet, "/api/runs/run-1", caller("u-both", "tenant-B"))
	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200: a member of the run's team reading by id", code)
	}
	if got.TeamID != "tenant-A" || got.Role != identity.RoleAdmin || got.Via != "membership" {
		t.Fatalf("identity = %+v: want the effective standing in tenant-A (admin, via membership)", got)
	}
	if tenant != "tenant-A" {
		t.Fatalf("store tenant = %q, want tenant-A", tenant)
	}
}

// No standing in the run's team: 404 — the same answer a missing run
// gives, for a caller whose only team is a different one.
func TestRunByIDInvisibleToAForeignCaller(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1", caller("u-member-b", "tenant-B"))
	if code != http.StatusNotFound {
		t.Fatalf("member of team B reading a tenant-A run: got %d, want 404", code)
	}
}

// The ladder (#1847): a viewer reads; acting is for members; a
// config_editor sees no runs at all (ADR-078); an org admin without
// membership reads as a viewer and cannot act.
func TestRunByIDLadder(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	t.Run("viewer reads", func(t *testing.T) {
		code, got, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1", caller("u-viewer-a", "tenant-A"))
		if code != http.StatusOK || got.Role != identity.RoleViewer {
			t.Fatalf("got %d (%+v): a viewer of the run's team reads it", code, got)
		}
	})
	t.Run("viewer cannot act", func(t *testing.T) {
		code, _, _ := scoped(t, srv, http.MethodPost, "/api/runs/run-1/cancel", caller("u-viewer-a", "tenant-A"))
		if code != http.StatusForbidden {
			t.Fatalf("got %d: a viewer must not cancel (#1847)", code)
		}
	})
	t.Run("member acts", func(t *testing.T) {
		code, _, _ := scoped(t, srv, http.MethodPost, "/api/runs/run-1/cancel", caller("u-member-a", "tenant-A"))
		if code != http.StatusOK {
			t.Fatalf("got %d: a member of the run's team acts on it", code)
		}
	})
	t.Run("viewer cannot drive the CDP pump", func(t *testing.T) {
		// GET, but live control of the run's browser: an action (H2).
		code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1/browser/cdp", caller("u-viewer-a", "tenant-A"))
		if code != http.StatusForbidden {
			t.Fatalf("got %d: the CDP pump is not a read", code)
		}
	})
	t.Run("member may drive the CDP pump", func(t *testing.T) {
		code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1/browser/cdp", caller("u-member-a", "tenant-A"))
		if code != http.StatusOK {
			t.Fatalf("got %d: the ladder passes a member to the pump", code)
		}
	})
	t.Run("empty wildcard answers 404 not 500", func(t *testing.T) {
		// /api/runs//run-1: the mux matches with an empty id — nothing
		// to resolve, a 404 (the review's B1; a 500 named an empty run).
		code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs//run-1", caller("u-both", "tenant-B"))
		if code != http.StatusNotFound {
			t.Fatalf("got %d: an empty wildcard is a missing run", code)
		}
	})
	t.Run("config_editor sees no runs", func(t *testing.T) {
		code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1", caller("u-configed-a", "tenant-A"))
		if code != http.StatusNotFound {
			t.Fatalf("got %d: a config_editor is not a run viewer (ADR-078)", code)
		}
	})
	t.Run("org admin without membership reads as viewer", func(t *testing.T) {
		code, got, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1", caller("u-orgadmin", "tenant-B"))
		if code != http.StatusOK || got.Role != identity.RoleViewer || got.Via != "org-admin-lineage" {
			t.Fatalf("got %d (%+v): org-admin lineage reads the run as a viewer", code, got)
		}
	})
	t.Run("org admin cannot act", func(t *testing.T) {
		code, _, _ := scoped(t, srv, http.MethodPost, "/api/runs/run-1/cancel", caller("u-orgadmin", "tenant-B"))
		if code != http.StatusForbidden {
			t.Fatalf("got %d: lineage grants read, not act", code)
		}
	})
	t.Run("super admin acts", func(t *testing.T) {
		code, got, _ := scoped(t, srv, http.MethodDelete, "/api/runs/run-1", auth.Identity{UserID: "root", IsSuperAdmin: true})
		if code != http.StatusOK || got.TeamID != "tenant-A" {
			t.Fatalf("got %d (%+v): a super-admin is re-anchored on the run's team with full power", code, got)
		}
	})
	t.Run("teamless caller with standing reads", func(t *testing.T) {
		code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1", caller("u-member-a", ""))
		if code != http.StatusOK {
			t.Fatalf("got %d: the id suffices — no active team is needed to read a run you may see", code)
		}
	})
	t.Run("synthetic principal never passes", func(t *testing.T) {
		id := caller("u-member-a", "tenant-A")
		id.Kind = auth.KindShare
		code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-1", id)
		if code != http.StatusNotFound {
			t.Fatalf("got %d: a synthetic principal is not a person in a team", code)
		}
	})
	t.Run("missing run answers 404", func(t *testing.T) {
		code, _, _ := scoped(t, srv, http.MethodGet, "/api/runs/run-missing", caller("u-both", "tenant-B"))
		if code != http.StatusNotFound {
			t.Fatalf("got %d: a missing run is 404 for everyone", code)
		}
	})
}

// A membership-store OUTAGE is a 500, never a silent degrade to viewer
// (the review's F3): the two arms of a failed read must not look alike.
func TestRunByIDStoreOutageDoesNotDegradeToViewer(t *testing.T) {
	mem := identity.NewMemoryStore()
	_ = mem.UpsertMembership(context.Background(), identity.Membership{UserID: "u-member-a", TeamID: "tenant-A", Role: identity.RoleMember})
	st := failingMembershipRead{Store: mem, err: context.DeadlineExceeded}
	_, _, err := identityInTeamOn(st, context.Background(), caller("u-member-a", "tenant-A"), "tenant-A")
	if err == nil {
		t.Fatal("a membership read that FAILED produced a verdict — it must be an error, never a refuse-or-allow")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v: the outage itself must travel", err)
	}
}

// failingMembershipRead always fails its membership read with something
// that is NOT ErrNotFound — an outage, not an answer.
type failingMembershipRead struct {
	identity.Store
	err error
}

func (d failingMembershipRead) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	return identity.Membership{}, d.err
}

// The LINEAGE arms hold the same rule: a team or org-membership read that
// fails (not "not found") is an error, not a 404.
func TestRunByIDLineageOutageIsNotARefusal(t *testing.T) {
	mem := identity.NewMemoryStore()
	ctx := context.Background()
	if _, err := mem.CreateTeam(ctx, identity.Team{ID: "tenant-A", Slug: "team-a", OrgID: "org-1"}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	noMembership := caller("u-foreign", "tenant-B")

	t.Run("team read outage", func(t *testing.T) {
		st := failingTeamRead{Store: mem, err: context.DeadlineExceeded}
		_, _, err := identityInTeamOn(st, ctx, noMembership, "tenant-A")
		if err == nil {
			t.Fatal("a team read that FAILED produced a verdict")
		}
	})
	t.Run("org membership read outage", func(t *testing.T) {
		st := failingOrgMembershipRead{Store: mem, err: context.DeadlineExceeded}
		_, _, err := identityInTeamOn(st, ctx, noMembership, "tenant-A")
		if err == nil {
			t.Fatal("an org-membership read that FAILED produced a verdict")
		}
	})
}

type failingTeamRead struct {
	identity.Store
	err error
}

func (d failingTeamRead) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	return identity.Team{}, d.err
}

type failingOrgMembershipRead struct {
	identity.Store
	err error
}

func (d failingOrgMembershipRead) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	return identity.OrgMembership{}, d.err
}

// The class is the set of REGISTERED routes whose pattern carries the run
// wildcard — every one of them goes through this file's resolution, and no
// fixed sibling of theirs ever does (the review's F1: a future
// "GET /api/runs/stats" must not be read as a run id).
func TestEveryRunWildcardRouteIsInTheClass(t *testing.T) {
	srv, _ := newTestServer(t)
	// A route whose wildcard is NOT spelled {id}: the classification is
	// wildcard-name-agnostic, so this must classify too (the review's
	// M1 - a rename must not fall out of the class, or out of this
	// witness).
	srv.mux.HandleFunc("GET /api/runs/{runID}/rva-probe", func(http.ResponseWriter, *http.Request) {})
	// Another resource's {id} route at the SAME index: not a run, and the
	// classification must leave it alone (this exact hazard panicked a
	// team-members route on a nil runs service during the round).
	srv.mux.HandleFunc("GET /api/teams/{id}/rva-probe", func(http.ResponseWriter, *http.Request) {})
	patterns := 0
	for _, route := range srv.mux.Routes() {
		inClass := routeIsRunWildcard(route.Pattern)
		if !inClass {
			if strings.HasPrefix(route.Pattern, "/api/runs") || strings.HasPrefix(route.Pattern, "/api/ws/runs") {
				// Fixed sibling: the classification must refuse it. The
				// probe carries the ROUTE'S method — a method-agnostic
				// probe would miss the literal registration and fall
				// through to a wildcard pattern (exactly the F1 hazard).
				method := route.Method
				if method == "" {
					method = http.MethodGet
				}
				req := httptest.NewRequest(method, samplePath(route.Pattern), nil)
				if _, ok, _ := srv.runIDClass(req); ok {
					t.Fatalf("%s %s: a fixed sibling was classified as a run route", route.Method, route.Pattern)
				}
			}
			continue
		}
		patterns++
		req := httptest.NewRequest(route.Method, samplePath(route.Pattern), nil)
		id, ok, _ := srv.runIDClass(req)
		if !ok || id == "" {
			t.Fatalf("%s %s: a run-wildcard route resolved to id=%q ok=%v", route.Method, route.Pattern, id, ok)
		}
	}
	if patterns == 0 {
		t.Fatal("no run-wildcard routes found — the class test is covering an empty set")
	}
}

// routeIsRunWildcard reports whether a REGISTERED pattern is shaped like a
// run-addressed route, DERIVED from the pattern (a wildcard - any spelling -
// at the id position under /api/runs or /api/ws/runs), never from the
// production table: a witness reading the table it witnesses is mute on a
// table change (the review's M1).
func routeIsRunWildcard(pattern string) bool {
	if i := strings.Index(pattern, " "); i >= 0 && !strings.HasPrefix(pattern, "/") {
		pattern = pattern[i+1:]
	}
	segs := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	for _, w := range []struct {
		fixed int
		chain []string
	}{
		{2, []string{"api", "runs"}},
		{3, []string{"api", "ws", "runs"}},
	} {
		if len(segs) > w.fixed && strings.HasPrefix(segs[w.fixed], "{") && strings.HasSuffix(segs[w.fixed], "}") {
			match := true
			for j, want := range w.chain {
				if segs[j] != want {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// samplePath turns a registered pattern into one concrete request path.
func samplePath(pattern string) string {
	if i := strings.Index(pattern, " "); i >= 0 && !strings.HasPrefix(pattern, "/") {
		pattern = pattern[i+1:]
	}
	r := strings.NewReplacer("{id}", "run-1", "{watchID}", "w-1", "{toolUseID}", "tu-1",
		"{node}", "n-1", "{version}", "v-1", "{path}", "p-1", "{kind}", "input",
		"{issueID}", "i-1", "{missionID}", "m-1", "{fileID}", "f-1", "{messageID}", "msg-1",
		"{childID}", "c-1", "{name}", "n-1", "{eventID}", "e-1", "{uploadID}", "u-1")
	return r.Replace(pattern)
}

// A caller-supplied id may carry an escaped slash: the extraction works on
// escaped SEGMENTS, so "a%2Fb" is one segment (the id), not two.
func TestExtractRunIDReadsEscapedSegmentsTheWayTheMuxDoes(t *testing.T) {
	if got := extractRunID("/api/runs/run-1/children", 2); got != "run-1" {
		t.Fatalf("extractRunID simple = %q", got)
	}
	if got := extractRunID("/api/runs/a%2Fb", 2); got != "a/b" {
		t.Fatalf("extractRunID escaped = %q, want a/b as ONE segment", got)
	}
	if got := extractRunID("/api/ws/runs/run-1/stream", 3); got != "run-1" {
		t.Fatalf("extractRunID ws = %q", got)
	}
}

// The cache is bounded: capacity is the eviction line, hits refresh, and a
// missing id is never cached.
func TestRunTenantCacheBoundsAndHits(t *testing.T) {
	c := newRunTenantCache(3)
	c.put("r1", "t1")
	c.put("r2", "t2")
	if team, ok := c.get("r1"); !ok || team != "t1" {
		t.Fatalf("get r1 = %q %v", team, ok)
	}
	c.put("r3", "t3")
	c.put("r4", "t4") // evicts r2 (least recently used: r1 was refreshed)
	if _, ok := c.get("r2"); ok {
		t.Fatal("r2 survived the eviction line")
	}
	if _, ok := c.get("r1"); !ok {
		t.Fatal("r1 was evicted despite being the most recent hit")
	}
	if c.ll.Len() > 3 {
		t.Fatalf("cache len = %d, over the bound", c.ll.Len())
	}
}

// The list scopes explicitly (#1848 / ADR-103): the handler hands the
// store a context stamped with the RESOLVED team - the caller's active
// team by default, the requested ?team_id= when the caller has standing
// in it, and a 403 naming the parameter for an invisible team. The store
// side of the filter is the mongo conformance suite's to prove.
func TestListRunsScopedByExplicitTeam(t *testing.T) {
	srv, guard := newRunScopeServer(t)

	scope := func(query string, id auth.Identity) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/runs"+query, nil)
		req = req.WithContext(auth.WithIdentity(req.Context(), id))
		rec := httptest.NewRecorder()
		srv.handleListRuns(rec, req)
		if rec.Code != http.StatusOK {
			return rec.Code, ""
		}
		return rec.Code, guard.lastListedTenant()
	}

	// No override: the caller's active team.
	code, team := scope("", auth.Identity{UserID: "u-member-b", TeamID: "tenant-B", Role: identity.RoleMember})
	if code != http.StatusOK || team != "tenant-B" {
		t.Fatalf("default scope: %d %q, want the caller's active team", code, team)
	}
	// An override to a team the caller belongs to.
	code, team = scope("?team_id=tenant-A", auth.Identity{UserID: "u-both", TeamID: "tenant-B", Role: identity.RoleMember})
	if code != http.StatusOK || team != "tenant-A" {
		t.Fatalf("?team_id=tenant-A: %d %q, want the requested team", code, team)
	}
	// An override to an invisible team: 403 naming the parameter.
	req := httptest.NewRequest(http.MethodGet, "/api/runs?team_id=tenant-A", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{UserID: "u-member-b", TeamID: "tenant-B", Role: identity.RoleMember}))
	rec := httptest.NewRecorder()
	srv.handleListRuns(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "team_id") {
		t.Fatalf("invisible override: %d %s, want a 403 naming team_id", rec.Code, rec.Body.String())
	}
}

// The FULL-STACK witness (the review's H1): a request that walks the real
// chain — requireAuth's bearer resolution, the choke point, the mux —
// reads a cross-team run it may see, and the wire carries the run's own
// team. A wiring that skips the choke point turns this red: with the
// active-team stamp alone the guarded store refuses run-1.
func TestRunByIDThroughRequireAuth(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	ts := fullStack(t, srv)

	get := func(token string) (int, string, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/run-1", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var out struct {
			Run struct {
				ID       string `json:"id"`
				TenantID string `json:"tenant_id"`
			} `json:"run"`
		}
		_ = json.Unmarshal(body, &out)
		return resp.StatusCode, out.Run.ID, out.Run.TenantID
	}

	pinned, _, err := srv.signer.IssueAccess(caller("u-both", "tenant-B"))
	if err != nil {
		t.Fatalf("mint bearer: %v", err)
	}
	code, runID, tenant := get(pinned)
	if code != http.StatusOK || runID != "run-1" || tenant != "tenant-A" {
		t.Fatalf("member of the run's team through the full chain: %d id=%q tenant=%q — the choke point is not on the bearer path", code, runID, tenant)
	}

	foreign, _, err := srv.signer.IssueAccess(caller("u-member-b", "tenant-B"))
	if err != nil {
		t.Fatalf("mint bearer: %v", err)
	}
	if code, _, _ := get(foreign); code != http.StatusNotFound {
		t.Fatalf("caller without standing through the full chain: %d, want 404", code)
	}
}

// The WebSocket ticket walks the same choke point: a ticket whose holder
// has standing upgrades; one without takes the 404 BEFORE any upgrade is
// attempted.
func TestWSRunTicketThroughRequireAuth(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	ts := fullStack(t, srv)

	ticket := func(userID, teamID string) string {
		t.Helper()
		id := caller(userID, teamID)
		tk, err := srv.wsTickets.Mint(context.Background(), id)
		if err != nil {
			t.Fatalf("mint ticket: %v", err)
		}
		return tk
	}

	code := func(tk string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/ws/runs/run-1?ticket="+tk, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := code(ticket("u-both", "tenant-B")); code == http.StatusNotFound {
		t.Fatal("a ticket with standing took 404 — the choke point refuses on the ws path")
	}
	if code := code(ticket("u-member-b", "tenant-B")); code != http.StatusNotFound {
		t.Fatalf("a ticket without standing got %d — the 404 must come before any upgrade", code)
	}
}

// The run-addressed watch stop refuses a watch that does not belong to the
// run in the path — a mismatched pair is a missing watch (the review's B2:
// the guard had no witness and its removal reddened nothing).
func TestStopAssistantWatchRefusesARunMismatch(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	srv.assistantWatches = mismatchedWatchStore{}

	req := httptest.NewRequest(http.MethodDelete, "/api/runs/run-1/assistant-watches/w-1", nil)
	req.SetPathValue("id", "run-1")
	req.SetPathValue("watchID", "w-1")
	req = req.WithContext(store.WithTenant(req.Context(), "tenant-A"))
	rec := httptest.NewRecorder()
	srv.handleStopAssistantWatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d: a watch targeting another run is not this run's watch", rec.Code)
	}
}

type mismatchedWatchStore struct {
	runwatch.Store
}

func (mismatchedWatchStore) GetWatch(context.Context, string) (runwatch.Watch, error) {
	return runwatch.Watch{ID: "w-1", TargetRunID: "other-run"}, nil
}
