package server

import (
	"container/list"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run-by-id choke point (ADR-103): a route that addresses a run by id is
// served from the RUN's team, whatever team the credential is pinned to. The
// caller's standing IN that team authorizes — read for a viewer, actions for
// a member; a run the caller cannot see answers 404 exactly like a missing
// one. Run identifiers and their existence are not secrets (ADR-103);
// authorization protects contents and actions.
//
// Classification is by the ROUTE THE MUX MATCHES (the pattern), never by raw
// path prefixes: a future static sibling of /api/runs/{id} (say a new
// "GET /api/runs/stats") contains no {id} wildcard and stays outside the
// class by construction. The class test walks the mux and pins every route
// of the class to this choke point.

const (
	// runTenantLRUSize bounds the per-process run-id → team cache. A run's
	// team never changes, so entries carry no TTL; the bound keeps a
	// pathological id-flood from growing the map. Eviction is LRU.
	runTenantLRUSize = 8192
)

// runScopePrefix is one canonical run pattern shape: the fixed segment
// chain that precedes the id wildcard and the wildcard's index.
type runScopePrefix = struct {
	fixedSegments int
	segments      []string
}

// runScopePrefixes are the fixed segment chains that precede the id
// wildcard in the four canonical run patterns ("api/runs" = 2 fixed
// segments before the id, "api/ws/runs" = 3). The wildcard's NAME is
// irrelevant: any registered wildcard at that position puts the route in
// the class, so a future `{runID}` spelling cannot silently fall outside
// it (the review's M1 — the hole F1 closed, reopened by a rename). The
// CHAIN, though, must match exactly — a wildcard at the same index under
// /api/teams is another resource, not a run.
var runScopePrefixes = []runScopePrefix{
	{2, []string{"api", "runs"}},
	{3, []string{"api", "ws", "runs"}},
}

// runScopeLadderRoutes names the GET routes that are ACTIONS despite their
// method: the CDP pump hands the caller the run's browser (navigate, run
// JS, read the page), the shell hands its terminal. A viewer reads the
// run; these take a member. Keyed by the fixed segment chain after the id.
var runScopeLadderRoutes = map[string]bool{
	"browser/cdp": true,
	"shell":       true,
}

// routeSubPath returns the request path segments AFTER the id wildcard
// position, unescaped, for a route classified under the given prefix.
func routeSubPath(path string, fixedSegments int) []string {
	escaped := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(escaped) <= fixedSegments+1 {
		return nil
	}
	out := make([]string, 0, len(escaped)-fixedSegments-1)
	for _, seg := range escaped[fixedSegments+1:] {
		seg, err := url.PathUnescape(seg)
		if err != nil {
			return nil
		}
		out = append(out, seg)
	}
	return out
}

// extractRunID reads the {id} path value the way ServeMux would resolve it:
// the escaped path is split on "/" FIRST, then each segment is unescaped, so
// a caller-supplied id holding an escaped slash ("a%2Fb") stays one segment
// and agrees with r.PathValue("id") at the handler.
func extractRunID(path string, fixedSegments int) string {
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	escaped := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(escaped) <= fixedSegments {
		return ""
	}
	id, err := url.PathUnescape(escaped[fixedSegments])
	if err != nil {
		return ""
	}
	return id
}

// patternWildcardAt reports whether a registered pattern is shaped like a
// run-addressed route: the pattern's path carries the prefix chain
// ("api/runs" or "api/ws/runs") AND a wildcard — any {name} spelling — at
// the id position. The chain must match EXACTLY: /api/teams/{id} has a
// wildcard at the same index and must stay OUT of the class.
func patternWildcardAt(pattern string, w runScopePrefix) bool {
	p := pattern
	if i := strings.Index(p, " "); i >= 0 && !strings.HasPrefix(p, "/") {
		p = p[i+1:]
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(segs) <= w.fixedSegments {
		return false
	}
	for j, want := range w.segments {
		if segs[j] != want {
			return false
		}
	}
	seg := segs[w.fixedSegments]
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}

// runIDClass reports the run id a request addresses by route pattern, and
// whether the request is in the class at all. The pattern comes from the mux
// itself (the same lookup the Sentry transaction naming uses), so the class
// is the set of REGISTERED routes with an id wildcard under /api/runs and
// /api/ws/runs — fixed siblings are structurally excluded.
func (s *Server) runIDClass(r *http.Request) (string, bool, int) {
	if s == nil || s.mux == nil {
		return "", false, 0
	}
	_, pattern := s.mux.Handler(r)
	if pattern == "" {
		return "", false, 0
	}
	for _, w := range runScopePrefixes {
		if !patternWildcardAt(pattern, w) {
			continue
		}
		return extractRunID(r.URL.EscapedPath(), w.fixedSegments), true, w.fixedSegments
	}
	return "", false, 0
}

// runTenantCache is the bounded per-process map of run id to the team that
// owns it. A run's team is immutable for the run's life (a deleted run keeps
// its tombstone), so entries need no TTL; the LRU bound caps memory under an
// id-flood. Lookups only cache POSITIVE resolutions — a not-found run answers
// 404 from the store every time.
type runTenantCache struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List // front = most recent
	elems map[string]*list.Element
}

type runTenantEntry struct {
	runID string
	team  string
}

func newRunTenantCache(cap int) *runTenantCache {
	return &runTenantCache{cap: cap, ll: list.New(), elems: map[string]*list.Element{}}
}

func (c *runTenantCache) get(runID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.elems[runID]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(runTenantEntry).team, true
	}
	return "", false
}

func (c *runTenantCache) put(runID, team string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.elems[runID]; ok {
		c.ll.MoveToFront(el)
		el.Value = runTenantEntry{runID, team}
		return
	}
	el := c.ll.PushFront(runTenantEntry{runID, team})
	c.elems[runID] = el
	for c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.elems, oldest.Value.(runTenantEntry).runID)
	}
}

// runTenant resolves the team that owns a run, team-blind: the caller's own
// tenant stamp must not filter the lookup (store.TeamBlind clears it AND
// lifts the fail-closed guard — a detached context alone would keep the
// stamp and the query would only see the caller's team). Team-blindness is
// the point of the route: authorization happens through the caller's
// standing in the RESOLVED team, one line below.
func (s *Server) runTenant(ctx context.Context, runID string) (string, error) {
	if team, ok := s.runTeams.get(runID); ok {
		return team, nil
	}
	run, err := s.runs.LoadRunCtx(store.TeamBlind(ctx), runID)
	if err != nil {
		return "", err
	}
	s.runTeams.put(runID, run.TenantID)
	return run.TenantID, nil
}

// identityInTeam is the ONE computation of a caller's standing in a team
// (ADR-103): it both authorizes and produces the effective identity handlers
// see — TeamID = the run's team, Role = the caller's role THERE, org lineage
// resolved for that team. Precedence:
//
//   - synthetic principals never pass (they speak for a share or a webhook,
//     not for a person);
//   - a super-admin keeps full power, re-anchored on the run's team;
//   - a direct membership gives its role (a config_editor membership is not
//     a viewer — ADR-078 — and sees no runs);
//   - an org admin/owner of the team's parent org reads as a viewer, marked
//     Via: this GRANTS what the pinned-token path refused before (an org
//     admin's PAT pinned to another team was unusable there), it does not
//     widen a session beyond canViewTeam;
//   - everyone else: not authorized.
//
// A membership-store FAILURE is an error, never a silent degrade to viewer.
func (s *Server) identityInTeam(ctx context.Context, id auth.Identity, teamID string) (auth.Identity, bool, error) {
	return identityInTeamOn(s.authStore(), ctx, id, teamID)
}

// identityInTeamOn is identityInTeam against an explicit identity store —
// the same computation, testable against stubbed membership failures.
func identityInTeamOn(st identity.Store, ctx context.Context, id auth.Identity, teamID string) (auth.Identity, bool, error) {
	if id.IsSynthetic() {
		return auth.Identity{}, false, nil
	}
	if id.IsSuperAdmin {
		return auth.Identity{
			UserID: id.UserID, Email: id.Email, IsSuperAdmin: true,
			OrgID: id.OrgID, OrgRole: id.OrgRole,
			TeamID: teamID, Role: identity.RoleAdmin,
			Kind: id.Kind, JTI: id.JTI, Via: "super-admin",
		}, true, nil
	}
	mb, err := st.GetMembership(ctx, id.UserID, teamID)
	switch {
	case err == nil:
		if !mb.Role.AtLeast(identity.RoleViewer) {
			// A config_editor (rank 0) or empty role is not a rung:
			// ADR-078 keeps the capability out of the run console.
			return auth.Identity{}, false, nil
		}
		return auth.Identity{
			UserID: id.UserID, Email: id.Email,
			OrgID: id.OrgID, OrgRole: id.OrgRole,
			TeamID: teamID, Role: mb.Role,
			Kind: id.Kind, JTI: id.JTI, Via: "membership",
		}, true, nil
	case errors.Is(err, identity.ErrNotFound):
		// No membership. Org-admin lineage reads as a viewer (what
		// canViewTeam would grant); anything else is out. ONE pass over
		// the store: team → org, then the caller's org membership — no
		// delegation to orgAdminOfTeam, which would read the same rows
		// twice (the review's F3). A read that FAILS — as opposed to
		// answering "absent" — propagates: an outage is never a refusal
		// dressed as one (the review's M2).
		t, terr := st.GetTeam(ctx, teamID)
		if terr != nil && !errors.Is(terr, identity.ErrNotFound) {
			return auth.Identity{}, false, terr
		}
		if terr != nil || t.OrgID == "" {
			return auth.Identity{}, false, nil
		}
		om, oerr := st.GetOrgMembership(ctx, id.UserID, t.OrgID)
		if oerr != nil && !errors.Is(oerr, identity.ErrNotFound) {
			return auth.Identity{}, false, oerr
		}
		if oerr != nil || !om.Role.AtLeast(identity.OrgRoleAdmin) {
			return auth.Identity{}, false, nil
		}
		return auth.Identity{
			UserID: id.UserID, Email: id.Email,
			OrgID: t.OrgID, OrgRole: om.Role,
			TeamID: teamID, Role: identity.RoleViewer,
			Kind: id.Kind, JTI: id.JTI, Via: "org-admin-lineage",
		}, true, nil
	default:
		// A transient store failure must NOT degrade into a verdict.
		return auth.Identity{}, false, err
	}
}

// runScopeLadder is the route ladder of ADR-103, enforced HERE at the choke
// point rather than per handler: reads (GET/HEAD, the WebSocket upgrades
// included) take a viewer; every action a route performs on the run
// (cancel, resume, rewind, answer, upload, delete) takes a member. A viewer
// of the team is exactly that — read-only (#1847); a config_editor sees no
// runs at all (ADR-078). Super-admins carry RoleAdmin through
// identityInTeam and pass both rungs. GET routes that hand the caller a
// LIVE CONTROL of the run — the CDP pump (navigate, run JS, read the page)
// and the shell terminal — are actions despite their method and take a
// member too (the review's H2).
func (s *Server) runScopeLadder(r *http.Request, fixedSegments int, role identity.Role) bool {
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead
	if isRead {
		// A GET that hands the caller live control of the run is an
		// action: the CDP pump (navigate, run JS, read the page) and the
		// shell terminal take a member, not a viewer.
		joined := strings.Join(routeSubPath(r.URL.EscapedPath(), fixedSegments), "/")
		for route := range runScopeLadderRoutes {
			if joined == route || strings.HasPrefix(joined, route+"/") {
				isRead = false
				break
			}
		}
	}
	if isRead {
		return role.AtLeast(identity.RoleViewer)
	}
	return role.AtLeast(identity.RoleMember)
}

// scopeRunByID is the choke point the auth middleware calls INSTEAD OF
// stampAuthedContext on every authenticated request. Routes that address a
// run by id are resolved to the RUN's team and stamped with the caller's
// standing THERE; every other route stamps exactly as before (the original
// identity, the original team). ok=false means the response is already
// written: 404 for a run that does not exist or that the caller cannot see
// (one answer, no existence oracle), 403 for a caller without the ladder's
// rung, 500 when the store fails (fail visibly, never a degrade).
func (s *Server) scopeRunByID(w http.ResponseWriter, r *http.Request, id auth.Identity) (context.Context, bool) {
	runID, ok, fixedSegments := s.runIDClass(r)
	if !ok {
		return s.stampAuthedContext(w, r, id)
	}
	if runID == "" {
		// A matched wildcard with nothing in it (a double slash the mux
		// would have redirected): nothing to resolve — 404, not a store
		// error with an empty id.
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found")
		return nil, false
	}
	team, err := s.runTenant(r.Context(), runID)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
			s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
			return nil, false
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "resolve run tenant: %v", err)
		return nil, false
	}
	resolved, ok, err := s.identityInTeam(r.Context(), id, team)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "resolve standing in team %s: %v", team, err)
		return nil, false
	}
	if !ok {
		// No standing in the run's team: the run is invisible — the
		// same 404 a missing run answers (no existence oracle).
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %s", runID)
		return nil, false
	}
	if !s.runScopeLadder(r, fixedSegments, resolved.Role) {
		// Visible, but the ladder refuses this action for the caller's
		// role (a viewer acting): a plain 403.
		s.httpErrorFor(w, r, http.StatusForbidden, "your role in team %s does not allow this action", team)
		return nil, false
	}
	return s.stampAuthedContext(w, r, resolved)
}
